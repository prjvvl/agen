package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/store"
	"github.com/prjvvl/agen/platform/internal/templates"
)

// visibleNamespaces returns the namespaces a list should cover (nil = all).
func visibleNamespaces(ctx context.Context, requested string) ([]string, error) {
	nss, err := namespacesFor(ctx, requested)
	if err != nil {
		return nil, err
	}
	if len(nss) == 1 && nss[0] == "" {
		return nil, nil
	}
	return nss, nil
}

var runStatuses = map[string]bool{"running": true, "waiting_approval": true, "succeeded": true, "failed": true, "cancelled": true}

func pageToken(r store.RunRow) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(r.StartedMs, 10) + ":" + r.ID))
}

func parsePageToken(tok string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err == nil {
		if ms, id, ok := strings.Cut(string(b), ":"); ok {
			if n, err := strconv.ParseInt(ms, 10, 64); err == nil && id != "" {
				return n, id, nil
			}
		}
	}
	return 0, "", invalid("invalid page_token")
}

func (h *Hub) ListRuns(ctx context.Context, req *connect.Request[agenv1.ListRunsRequest]) (*connect.Response[agenv1.ListRunsResponse], error) {
	m := req.Msg
	nss, err := visibleNamespaces(ctx, m.Namespace)
	if err != nil {
		return nil, err
	}
	if m.Status != "" && !runStatuses[m.Status] {
		return nil, invalid("status must be one of running, waiting_approval, succeeded, failed, cancelled")
	}
	limit := min(listLimit(m.Limit, 50), 500)
	f := store.RunFilter{Namespaces: nss, Deployment: m.Deployment, Status: m.Status, TaskID: m.TaskId, RootsOnly: m.RootsOnly,
		Labels: m.Labels, Limit: limit + 1}
	if m.Since != nil {
		f.SinceMs = m.Since.AsTime().UnixMilli()
	}
	if m.PageToken != "" {
		if f.BeforeMs, f.BeforeID, err = parsePageToken(m.PageToken); err != nil {
			return nil, err
		}
	}
	list, err := h.Store.ListRunsFiltered(ctx, f)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.ListRunsResponse{}
	if len(list) > limit {
		list = list[:limit]
		out.NextPageToken = pageToken(list[limit-1])
	}
	var trees map[string]store.TreeUsage
	if m.RootsOnly {
		ids := make([]string, len(list))
		for i, r := range list {
			ids[i] = r.ID
		}
		if trees, err = h.Store.TreeUsages(ctx, ids); err != nil {
			return nil, connectErr(err)
		}
	}
	for _, r := range list {
		p := runProto(r)
		if t, ok := trees[r.ID]; ok {
			p.TreeRuns = int32(t.Runs)
			p.TreeUsage = &agenv1.Usage{InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CostUsd: t.CostUSD}
		}
		out.Runs = append(out.Runs, p)
	}
	return connect.NewResponse(out), nil
}

// storedMessage is the engine's message JSON.
type storedMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

func (h *Hub) GetTranscript(ctx context.Context, req *connect.Request[agenv1.GetTranscriptRequest]) (*connect.Response[agenv1.GetTranscriptResponse], error) {
	run, err := h.Store.GetRun(ctx, req.Msg.RunId)
	if err != nil {
		return nil, connectErr(err)
	}
	if err := requireNamespace(ctx, run.Namespace); err != nil {
		return nil, err
	}
	rows, err := h.Store.Transcript(ctx, run, req.Msg.IncludeHistory)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.GetTranscriptResponse{Run: runProto(run)}
	for _, r := range rows {
		var m storedMessage
		if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
			continue
		}
		tm := &agenv1.TranscriptMessage{Seq: r.Seq, RunId: r.RunID, Role: strings.ToLower(m.Role), Content: m.Content,
			ToolCallId: m.ToolCallID, CreatedAt: ms(r.CreatedMs)}
		for _, c := range m.ToolCalls {
			args := &structpb.Value{}
			if args.UnmarshalJSON(c.Arguments) != nil {
				args = structpb.NewStringValue(string(c.Arguments))
			}
			tm.ToolCalls = append(tm.ToolCalls, &agenv1.ToolCallMessage{Id: c.ID, Name: c.Name, Arguments: args})
		}
		out.Messages = append(out.Messages, tm)
	}
	if run.DefinitionDigest != "" {
		if def, err := h.Store.GetDefinition(ctx, run.DefinitionDigest); err == nil {
			out.SystemPrompt = bundle.SystemPrompt(def.Files)
		}
	}
	return connect.NewResponse(out), nil
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func (h *Hub) GetMetrics(ctx context.Context, req *connect.Request[agenv1.GetMetricsRequest]) (*connect.Response[agenv1.GetMetricsResponse], error) {
	nss, err := visibleNamespaces(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, err
	}
	window := int64(req.Msg.WindowSeconds)
	if window <= 0 {
		window = 86400
	}
	window = min(window, 30*86400)
	buckets := int64(req.Msg.Buckets)
	if buckets <= 0 {
		buckets = 24
	}
	buckets = min(buckets, 200)
	bucketMs := max(window*1000/buckets, 1)
	since := store.NowMs() - bucketMs*buckets
	stats, err := h.Store.RunStats(ctx, nss, since)
	if err != nil {
		return nil, connectErr(err)
	}
	type acc struct {
		m         *agenv1.DeploymentMetrics
		durations []int64
	}
	byDep := map[string]*acc{}
	for _, r := range stats {
		key := r.Namespace + "/" + r.Deployment
		a := byDep[key]
		if a == nil {
			a = &acc{m: &agenv1.DeploymentMetrics{Ref: &agenv1.DeploymentRef{Namespace: r.Namespace, Name: r.Deployment}, Usage: &agenv1.Usage{},
				Buckets: make([]*agenv1.MetricsBucket, buckets)}}
			for i := range a.m.Buckets {
				a.m.Buckets[i] = &agenv1.MetricsBucket{}
			}
			byDep[key] = a
		}
		m := a.m
		m.Runs++
		switch r.Status {
		case "failed":
			m.Failed++
		case "cancelled":
			m.Cancelled++
		}
		if r.EndedMs == 0 {
			m.Active++
		} else {
			a.durations = append(a.durations, r.EndedMs-r.StartedMs)
		}
		m.Usage.InputTokens += r.InputTokens
		m.Usage.OutputTokens += r.OutputTokens
		m.Usage.CostUsd += r.CostUSD
		b := m.Buckets[min(max((r.StartedMs-since)/bucketMs, 0), buckets-1)]
		b.Runs++
		if r.Status == "failed" {
			b.Failed++
		}
		b.CostUsd += r.CostUSD
	}
	out := &agenv1.GetMetricsResponse{Since: timestamppb.New(time.UnixMilli(since)), BucketSeconds: int32(bucketMs / 1000)}
	keys := make([]string, 0, len(byDep))
	for k := range byDep {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := byDep[k]
		slices.Sort(a.durations)
		a.m.P50Ms, a.m.P95Ms = percentile(a.durations, 0.5), percentile(a.durations, 0.95)
		out.Deployments = append(out.Deployments, a.m)
	}
	calls, err := h.Store.CallStats(ctx, nss, since)
	if err != nil {
		return nil, connectErr(err)
	}
	p := PrincipalFrom(ctx)
	edges := map[string]*agenv1.CallEdge{}
	var order []string
	for _, c := range calls {
		if !p.AllowsNamespace(c.FromNamespace) || !p.AllowsNamespace(c.ToNamespace) {
			continue
		}
		key := c.FromNamespace + "/" + c.FromDeployment + ">" + c.ToNamespace + "/" + c.ToDeployment
		e := edges[key]
		if e == nil {
			e = &agenv1.CallEdge{From: &agenv1.DeploymentRef{Namespace: c.FromNamespace, Name: c.FromDeployment},
				To: &agenv1.DeploymentRef{Namespace: c.ToNamespace, Name: c.ToDeployment}}
			edges[key] = e
			order = append(order, key)
		}
		e.Calls++
		if c.Status == "failed" {
			e.Failed++
		}
	}
	sort.Strings(order)
	for _, k := range order {
		out.Edges = append(out.Edges, edges[k])
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) ListTemplates(context.Context, *connect.Request[agenv1.ListTemplatesRequest]) (*connect.Response[agenv1.ListTemplatesResponse], error) {
	list, err := templates.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &agenv1.ListTemplatesResponse{}
	for _, t := range list {
		out.Templates = append(out.Templates, &agenv1.Template{Name: t.Name, Title: t.Title, Description: t.Description, Category: t.Category,
			Secrets: t.Secrets, Files: t.Files})
	}
	return connect.NewResponse(out), nil
}

// ---- notification targets ----

// NotificationEvents are the event types a notification target can receive.
var NotificationEvents = []string{"approval.pending", "task.failed", "budget.exhausted"}

var targetName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func targetProto(t store.NotificationTarget) *agenv1.NotificationTarget {
	return &agenv1.NotificationTarget{Namespace: t.Namespace, Name: t.Name, Url: t.URL, Events: t.Events, CreatedAt: ms(t.CreatedMs)}
}

func (h *Hub) SetNotificationTarget(ctx context.Context, req *connect.Request[agenv1.SetNotificationTargetRequest]) (*connect.Response[agenv1.SetNotificationTargetResponse], error) {
	t := req.Msg.GetTarget()
	if t == nil {
		return nil, invalid("target is required")
	}
	ns := nsOr(t.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	if !targetName.MatchString(t.Name) {
		return nil, invalid("name must be lowercase letters, digits and dashes (at most 63)")
	}
	if u, err := url.Parse(t.Url); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, invalid("url must be an http(s) URL")
	}
	for _, e := range t.Events {
		if !slices.Contains(NotificationEvents, e) {
			return nil, invalid(fmt.Sprintf("unknown event %q (known: %s)", e, strings.Join(NotificationEvents, ", ")))
		}
	}
	saved, secret, err := h.Store.SetNotificationTarget(ctx, store.NotificationTarget{Namespace: ns, Name: t.Name, URL: t.Url, Events: t.Events})
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.SetNotificationTargetResponse{Target: targetProto(saved), Secret: secret}), nil
}

func (h *Hub) ListNotificationTargets(ctx context.Context, req *connect.Request[agenv1.ListNotificationTargetsRequest]) (*connect.Response[agenv1.ListNotificationTargetsResponse], error) {
	if req.Msg.Namespace != "" {
		if err := requireNamespace(ctx, req.Msg.Namespace); err != nil {
			return nil, err
		}
	}
	list, err := h.Store.ListNotificationTargets(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, connectErr(err)
	}
	p := PrincipalFrom(ctx)
	out := &agenv1.ListNotificationTargetsResponse{}
	for _, t := range list {
		if p.AllowsNamespace(t.Namespace) {
			out.Targets = append(out.Targets, targetProto(t))
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) DeleteNotificationTarget(ctx context.Context, req *connect.Request[agenv1.DeleteNotificationTargetRequest]) (*connect.Response[agenv1.DeleteNotificationTargetResponse], error) {
	ns := nsOr(req.Msg.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	if err := h.Store.DeleteNotificationTarget(ctx, ns, req.Msg.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no notification target %s/%s", ns, req.Msg.Name))
		}
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.DeleteNotificationTargetResponse{}), nil
}
