package hub

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Thresholds for the warnings GetActivity returns.
const (
	nearLimitShare    = 0.8              // of max turns, the run budget or the daily budget
	quietAfter        = 5 * time.Minute  // a run with no step finished for this long
	approvalWaitAfter = 30 * time.Minute // a pending approval this old
	queueWaitAfter    = 5 * time.Minute  // the oldest due task waiting this long
	burstWindow       = 10 * time.Minute
	burstTasks        = 10 // tasks in one conversation within burstWindow
	modelErrWindow    = 15 * time.Minute
	modelErrMinCalls  = 5
	modelErrShare     = 0.3
	defaultMaxTurns   = 16
)

// maxTurnsByDigest caches agent.md maxTurns per definition.
var maxTurnsByDigest sync.Map

func (h *Hub) maxTurns(ctx context.Context, digest string) int {
	if v, ok := maxTurnsByDigest.Load(digest); ok {
		return v.(int)
	}
	n := defaultMaxTurns
	if def, err := h.Store.GetDefinition(ctx, digest); err == nil {
		if b, err := bundle.Parse(def.Files); err == nil && b.MaxTurns > 0 {
			n = b.MaxTurns
		}
	}
	maxTurnsByDigest.Store(digest, n)
	return n
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// human writes a duration in ms the way people say it: "40 min", "3 h 5 min", "2 days".
func human(msecs int64) string {
	m := msecs / 60_000
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 48*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d h", m/60)
		}
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	default:
		return fmt.Sprintf("%d days", m/(24*60))
	}
}

func (h *Hub) GetActivity(ctx context.Context, req *connect.Request[agenv1.GetActivityRequest]) (*connect.Response[agenv1.GetActivityResponse], error) {
	nss, err := visibleNamespaces(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, err
	}
	now := store.NowMs()
	out := &agenv1.GetActivityResponse{Now: timestamppb.New(time.UnixMilli(now))}
	warn := func(kind, severity string, ns, dep, run, msg string) {
		out.Warnings = append(out.Warnings, &agenv1.ActivityWarning{Kind: kind, Severity: severity, Ref: &agenv1.DeploymentRef{Namespace: ns, Name: dep}, RunId: run, Message: msg})
	}
	runs, err := h.Store.ActiveRuns(ctx, nss)
	if err != nil {
		return nil, connectErr(err)
	}
	ids := make([]string, len(runs))
	children := map[string][]store.ActiveRun{}
	for i, r := range runs {
		ids[i] = r.ID
		if r.ParentRunID != "" {
			children[r.ParentRunID] = append(children[r.ParentRunID], r)
		}
	}
	steps, err := h.Store.OpenSteps(ctx, ids)
	if err != nil {
		return nil, connectErr(err)
	}
	approvals, err := h.Store.PendingApprovalsByRun(ctx, ids)
	if err != nil {
		return nil, connectErr(err)
	}
	deps := map[string]store.Deployment{}
	depOf := func(ns, name string) (store.Deployment, bool) {
		k := ns + "/" + name
		if d, ok := deps[k]; ok {
			return d, true
		}
		d, err := h.Store.GetDeployment(ctx, ns, name)
		if err != nil {
			return d, false
		}
		deps[k] = d
		return d, true
	}
	for _, r := range runs {
		a := &agenv1.ActiveRun{RunId: r.ID, Ref: &agenv1.DeploymentRef{Namespace: r.Namespace, Name: r.Deployment}, TaskId: r.TaskID,
			Input: clip(r.Input, 200), StartedAt: ms(r.StartedMs), Turns: r.Step, MaxTurns: int32(h.maxTurns(ctx, r.DefinitionDigest)),
			Usage: &agenv1.Usage{InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CostUsd: r.CostUSD}, TraceId: r.TraceID,
			ParentRunId: r.ParentRunID, RootRunId: r.RootRunID, ConversationKey: r.ConversationKey, Source: r.Source}
		if r.CallerDeployment != "" {
			a.Caller = &agenv1.DeploymentRef{Namespace: r.CallerNamespace, Name: r.CallerDeployment}
		}
		st := steps[r.ID]
		last := max(st.LastEndMs, r.StartedMs)
		a.LastStepAt = ms(last)
		since := last
		switch {
		case approvals[r.ID].ID != "":
			ap := approvals[r.ID]
			a.Activity, a.ApprovalId, since = "approval", ap.ID, ap.CreatedMs
			if now-ap.CreatedMs > approvalWaitAfter.Milliseconds() {
				warn("waiting_approval", "warning", r.Namespace, r.Deployment, r.ID,
					fmt.Sprintf("waiting %s for approval %s", human(now-ap.CreatedMs), ap.ID))
			}
		case len(children[r.ID]) > 0:
			a.Activity, a.Detail, since = "agent", children[r.ID][0].Deployment, children[r.ID][0].StartedMs
		case st.Name == "gen_ai.chat":
			a.Activity, since = "model", st.StartMs
		case st.Name == "agen.tool":
			a.Activity, a.Detail, since = "tool", str(st.Attributes, "gen_ai.tool.name"), st.StartMs
			if t := str(st.Attributes, "agen.delegate.target"); t != "" {
				a.Activity, a.Detail = "agent", t
			}
		default:
			a.Activity = "idle"
		}
		a.ActivitySince = ms(since)
		out.Runs = append(out.Runs, a)

		if a.MaxTurns > 0 && float64(r.Step) >= nearLimitShare*float64(a.MaxTurns) {
			warn("near_turn_limit", "warning", r.Namespace, r.Deployment, r.ID, fmt.Sprintf("turn %d of %d", r.Step, a.MaxTurns))
		}
		if d, ok := depOf(r.Namespace, r.Deployment); ok {
			b := PolicyOf(d).Budget
			if max := b.MaxTokensPerRun; max > 0 && float64(r.InputTokens+r.OutputTokens) >= nearLimitShare*float64(max) {
				warn("near_run_budget", "warning", r.Namespace, r.Deployment, r.ID, fmt.Sprintf("%d of %d tokens", r.InputTokens+r.OutputTokens, max))
			}
			if max := b.MaxUsdPerRun; max > 0 && r.CostUSD >= nearLimitShare*max {
				warn("near_run_budget", "warning", r.Namespace, r.Deployment, r.ID, fmt.Sprintf("$%.4f of $%.4f", r.CostUSD, max))
			}
		}
		if (a.Activity == "idle" || a.Activity == "model" || a.Activity == "tool") && now-last > quietAfter.Milliseconds() {
			warn("quiet_run", "warning", r.Namespace, r.Deployment, r.ID,
				fmt.Sprintf("no step finished for %s (%s)", human(now-last), a.Activity))
		}
	}

	queues, err := h.Store.Queues(ctx, nss)
	if err != nil {
		return nil, connectErr(err)
	}
	// Deployments whose every run waits for an approval: their queue cannot move.
	waitingOnly := map[[2]string]bool{}
	for _, r := range out.Runs {
		k := [2]string{r.Ref.Namespace, r.Ref.Name}
		if _, seen := waitingOnly[k]; !seen {
			waitingOnly[k] = true
		}
		waitingOnly[k] = waitingOnly[k] && r.Activity == "approval"
	}
	for _, q := range queues {
		if q.Queued > 0 && waitingOnly[[2]string{q.Namespace, q.Deployment}] {
			warn("blocked_by_approval", "warning", q.Namespace, q.Deployment, "",
				fmt.Sprintf("%d tasks queued behind a pending approval", q.Queued))
		}
		out.Queues = append(out.Queues, &agenv1.QueueStatus{Ref: &agenv1.DeploymentRef{Namespace: q.Namespace, Name: q.Deployment},
			Queued: int32(q.Queued), Scheduled: int32(q.Scheduled), OldestQueuedAt: ms(q.OldestQueuedMs)})
		if q.Queued > 0 && q.OldestQueuedMs > 0 && now-q.OldestQueuedMs > queueWaitAfter.Milliseconds() {
			warn("queue_waiting", "warning", q.Namespace, q.Deployment, "",
				fmt.Sprintf("%d tasks waiting, the oldest for %s", q.Queued, human(now-q.OldestQueuedMs)))
		}
	}
	bursts, err := h.Store.Bursts(ctx, nss, now-burstWindow.Milliseconds(), burstTasks)
	if err != nil {
		return nil, connectErr(err)
	}
	for _, b := range bursts {
		warn("burst", "critical", b.Namespace, b.Deployment, "",
			fmt.Sprintf("%d tasks in conversation %q in the last %s: a loop?", b.Tasks, b.ConversationKey, human(burstWindow.Milliseconds())))
	}

	// Model errors per deployment, recent window.
	calls, err := h.Store.ModelCalls(ctx, nss, now-modelErrWindow.Milliseconds(), 5000)
	if err != nil {
		return nil, connectErr(err)
	}
	type tally struct{ calls, failed int }
	errs := map[[2]string]*tally{}
	for _, c := range calls {
		k := [2]string{c.Namespace, c.Deployment}
		if errs[k] == nil {
			errs[k] = &tally{}
		}
		errs[k].calls++
		if c.Status == "error" {
			errs[k].failed++
		}
	}
	keys := make([][2]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		t := errs[k]
		if t.calls >= modelErrMinCalls && float64(t.failed) >= modelErrShare*float64(t.calls) {
			warn("model_errors", "critical", k[0], k[1], "", fmt.Sprintf("%d of %d model calls failed in the last %s", t.failed, t.calls, human(modelErrWindow.Milliseconds())))
		}
	}

	// Daily budgets and held provider accounts.
	all, err := h.Store.ListDeployments(ctx, "")
	if err != nil {
		return nil, connectErr(err)
	}
	p := PrincipalFrom(ctx)
	heldSeen := map[string]bool{}
	for _, d := range all {
		if !p.AllowsNamespace(d.Namespace) || (req.Msg.Namespace != "" && d.Namespace != req.Msg.Namespace) {
			continue
		}
		if spent, exhausted, err := h.spentToday(ctx, d); err == nil {
			if max := PolicyOf(d).Budget.MaxUsdPerDay; exhausted {
				warn("near_daily_budget", "critical", d.Namespace, d.Name, "", fmt.Sprintf("daily budget used up ($%.2f of $%.2f)", spent, max))
			} else if max > 0 && spent >= nearLimitShare*max {
				warn("near_daily_budget", "warning", d.Namespace, d.Name, "", fmt.Sprintf("$%.2f of the $%.2f daily budget spent", spent, max))
			}
		}
		if hold, ok := h.providerHold(ctx, d); ok && !heldSeen[hold.Account+d.Namespace+d.Name] {
			heldSeen[hold.Account+d.Namespace+d.Name] = true
			warn("provider_held", "critical", d.Namespace, d.Name, "",
				fmt.Sprintf("provider account %s held: %s; next try %s", hold.Account, hold.Reason, time.UnixMilli(hold.RetryMs).UTC().Format(time.RFC3339)))
		}
	}
	return connect.NewResponse(out), nil
}

// modelMetrics aggregates model calls by the model that served them.
func (h *Hub) modelMetrics(ctx context.Context, nss []string, since int64) ([]*agenv1.ModelMetrics, error) {
	calls, err := h.Store.ModelCalls(ctx, nss, since, 50000)
	if err != nil {
		return nil, err
	}
	byModel := map[string]*agenv1.ModelMetrics{}
	durations := map[string][]int64{}
	for _, c := range calls {
		model := str(c.Attributes, "gen_ai.response.model")
		if model == "" {
			model = str(c.Attributes, "gen_ai.request.model")
		}
		if model == "" {
			continue
		}
		m := byModel[model]
		if m == nil {
			m = &agenv1.ModelMetrics{Model: model, Provider: str(c.Attributes, "gen_ai.provider.name"), Usage: &agenv1.Usage{}}
			byModel[model] = m
		}
		m.Calls++
		if c.Status == "error" {
			m.Failed++
		}
		if b, _ := c.Attributes["agen.fallback"].(bool); b {
			m.Fallbacks++
		}
		num := func(k string) float64 { f, _ := c.Attributes[k].(float64); return f }
		m.Usage.InputTokens += int64(num("gen_ai.usage.input_tokens"))
		m.Usage.OutputTokens += int64(num("gen_ai.usage.output_tokens"))
		m.Usage.CostUsd += num("agen.usage.cost_usd")
		durations[model] = append(durations[model], c.DurationMs)
	}
	out := make([]*agenv1.ModelMetrics, 0, len(byModel))
	for model, m := range byModel {
		d := durations[model]
		slices.Sort(d)
		m.P50Ms, m.P95Ms = percentile(d, 0.5), percentile(d, 0.95)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// entryEdges groups runs without a parent run by where their work came from.
func (h *Hub) entryEdges(ctx context.Context, nss []string, since int64) ([]*agenv1.EntryEdge, error) {
	entries, err := h.Store.Entries(ctx, nss, since)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*agenv1.EntryEdge{}
	var order []string
	for _, e := range entries {
		kind, from := e.Source, ""
		if i := strings.IndexByte(kind, ':'); i >= 0 {
			kind, from = kind[:i], kind[i+1:]
		}
		switch kind {
		case "":
			kind = "a2a" // no task: an A2A call from outside the fleet
		case "api", "cron", "webhook", "reply":
		default:
			continue // a2a child tasks are calls between agents (edges)
		}
		k := kind + ">" + e.Namespace + "/" + e.Deployment
		if kind == "reply" {
			k = from + ">" + k
		}
		edge := byKey[k]
		if edge == nil {
			edge = &agenv1.EntryEdge{Source: kind, To: &agenv1.DeploymentRef{Namespace: e.Namespace, Name: e.Deployment}}
			if kind == "reply" {
				edge.From = &agenv1.DeploymentRef{Namespace: e.Namespace, Name: from}
			}
			byKey[k] = edge
			order = append(order, k)
		}
		edge.Runs++
		if e.Status == "failed" {
			edge.Failed++
		}
	}
	sort.Strings(order)
	out := make([]*agenv1.EntryEdge, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out, nil
}
