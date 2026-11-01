package hub

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Daily budgets (docs/architecture.md §7): the engine enforces per-run
// token/$ budgets; the Hub enforces budget.max_usd_per_day from the usage the
// hosts record in the Store. An exhausted deployment stops cleanly: runs in
// flight finish, nothing new starts (no leases, autoscaler holds 0, wake-ups
// refused) and queued work waits for the next UTC day.

// dayStartMs is the start of the current UTC day.
func dayStartMs() int64 {
	return time.UnixMilli(store.NowMs()).UTC().Truncate(24 * time.Hour).UnixMilli()
}

// spentToday returns today's model spend and whether the daily budget is used up.
func (h *Hub) spentToday(ctx context.Context, d store.Deployment) (float64, bool, error) {
	if PolicyOf(d).Budget.MaxUsdPerDay <= 0 {
		return 0, false, nil // no daily budget: nothing to query
	}
	_, _, cost, err := h.Store.Usage(ctx, d.Namespace, d.Name, dayStartMs())
	if err != nil {
		return 0, false, err
	}
	max := PolicyOf(d).Budget.MaxUsdPerDay
	return cost, max > 0 && cost >= max, nil
}

// BudgetPressure is the share of the daily budget after which leased tasks
// carry budget_pressure (routing models drop to their budget tier).
const BudgetPressure = 0.8

// exhaustedLabels returns the "key=value" strings of d's label_usd_per_day
// rules whose spend today has reached their limit (3.5, part: label
// budgets). Unlike the deployment-wide daily budget, which holds the
// whole deployment at zero instances once exhausted, an exhausted label
// only excludes candidates carrying that label from LeaseTasks — the
// rest of the deployment's queue keeps going. Nil, no error: no rules
// configured, nothing to check.
func (h *Hub) exhaustedLabels(ctx context.Context, d store.Deployment) (map[string]bool, error) {
	rules := PolicyOf(d).Budget.LabelUsdPerDay
	if len(rules) == 0 {
		return nil, nil
	}
	since := dayStartMs()
	out := map[string]bool{}
	for kv, max := range rules {
		if max <= 0 {
			continue
		}
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		spent, err := h.Store.LabelSpentToday(ctx, d.Namespace, d.Name, key, val, since)
		if err != nil {
			return nil, err
		}
		if spent >= max {
			out[kv] = true
		}
	}
	return out, nil
}

// labelsExhausted reports whether t carries any label matching an
// exhausted rule from exhaustedLabels.
func labelsExhausted(t store.Task, exhausted map[string]bool) bool {
	for k, v := range t.Labels {
		if exhausted[k+"="+v] {
			return true
		}
	}
	return false
}

func budgetError(d store.Deployment, spent float64) error {
	next := time.UnixMilli(dayStartMs()).UTC().Add(24 * time.Hour)
	return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("%s/%s used its daily budget ($%.4f of $%.4f); new work resumes at %s",
		d.Namespace, d.Name, spent, PolicyOf(d).Budget.MaxUsdPerDay, next.Format(time.RFC3339)))
}

// withBudget fills the budget fields of a deployment message.
func (h *Hub) withBudget(ctx context.Context, d store.Deployment, pd *agenv1.Deployment) *agenv1.Deployment {
	if spent, exhausted, err := h.spentToday(ctx, d); err == nil {
		pd.SpentUsdToday, pd.BudgetExhausted = spent, exhausted
	}
	if hold, ok := h.providerHold(ctx, d); ok {
		pd.ProviderHold = providerHoldProto(hold)
	}
	h.stampCronTimes(ctx, d, pd)
	h.stampRandomTimes(ctx, d, pd)
	return pd
}

// narrowForTreeBudget narrows combined's budget by the tree's remaining
// budget (3.5, part): a tree budget is the root task's own
// overrides.budget (maxTokens/maxUsd), not a separate field — a child's
// effective budget is the smaller of its own request and the tree's
// remainder (the root's limit minus every run of the tree's usage so
// far, TreeUsage). No root budget: no narrowing, combined is returned
// unchanged (nil stays nil).
//
// This is computed once, at the child's own submission, not re-checked
// against the tree's usage again for the rest of that child's run: a
// sibling spending heavily later is not retroactively applied to a run
// already in progress, only to the NEXT child submitted after it. A
// remainder that is already used up floors at a small positive value
// (1 token / $0.0001) rather than 0, since the engine's own existing
// budget_exceeded check treats a 0 limit as "unset, no limit" — a floor
// still lets the child's own very first turn run (0 tokens used is not
// yet over a 1-token limit), then ends it with a report on its second
// check, the same "overshoot of at most one model call" the daily
// deployment budget already documents.
// labelBudgetMetrics is GetMetrics' per-label rollup (3.5, part): today's
// spend against every configured budget.labelUsdPerDay rule of a
// deployment, regardless of GetMetrics' own window — a label budget
// resets daily, not by the call's window/buckets. Nil, same as
// exhaustedLabels, when the deployment has no such rules.
func (h *Hub) labelBudgetMetrics(ctx context.Context, ns, name string) []*agenv1.LabelBudgetMetric {
	d, err := h.Store.GetDeployment(ctx, ns, name)
	if err != nil {
		return nil
	}
	rules := PolicyOf(d).Budget.LabelUsdPerDay
	if len(rules) == 0 {
		return nil
	}
	since := dayStartMs()
	kvs := make([]string, 0, len(rules))
	for kv := range rules {
		kvs = append(kvs, kv)
	}
	sort.Strings(kvs)
	out := make([]*agenv1.LabelBudgetMetric, 0, len(kvs))
	for _, kv := range kvs {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		spent, err := h.Store.LabelSpentToday(ctx, ns, name, key, val, since)
		if err != nil {
			continue
		}
		out = append(out, &agenv1.LabelBudgetMetric{Label: kv, SpentUsd: spent, LimitUsd: rules[kv]})
	}
	return out
}

// treeBudgetThresholds emits budget.threshold events (scope "tree") at
// 50/80/100% of a root task's own overrides.budget, the same limit
// narrowForTreeBudget narrows every child's effective budget against.
// Dedupe key is per root task (not per day: a tree's budget, unlike the
// deployment's, does not reset daily), so each crossing fires once for
// the tree's whole lifetime regardless of which task in it ends.
func (h *Hub) treeBudgetThresholds(ctx context.Context, t store.Task) {
	if t.RootTaskID == "" {
		return
	}
	root, err := h.Store.GetTask(ctx, t.RootTaskID)
	if err != nil {
		return
	}
	rootOv, _ := store.ParseOverrides(root.Overrides)
	if rootOv == nil || rootOv.Budget == nil || (rootOv.Budget.MaxUsd <= 0 && rootOv.Budget.MaxTokens <= 0) {
		return
	}
	inTok, outTok, usd, err := h.Store.TreeUsage(ctx, t.RootTaskID)
	if err != nil {
		return
	}
	limit, spent, unit := rootOv.Budget.MaxUsd, usd, "usd"
	if limit <= 0 {
		limit, spent, unit = float64(rootOv.Budget.MaxTokens), float64(inTok+outTok), "tokens"
	}
	for _, pct := range []int{50, 80, 100} {
		if spent < limit*float64(pct)/100 {
			break
		}
		severity := map[int]string{50: "info", 80: "warning", 100: "critical"}[pct]
		_, _ = h.Store.AppendEventOnce(ctx, store.Event{Namespace: t.Namespace, Type: "budget.threshold", Deployment: root.Deployment,
			TaskID: t.RootTaskID, Severity: severity,
			Data:      map[string]any{"percent": pct, "scope": "tree", "rootTaskId": t.RootTaskID, "unit": unit, "spent": spent, "limit": limit},
			DedupeKey: fmt.Sprintf("budget:tree:%s:%d", t.RootTaskID, pct)})
	}
}

// labelBudgetThresholds emits budget.threshold events (scope "label") at
// 50/80/100% of each configured label_usd_per_day rule, the same spend
// exhaustedLabels checks against. Dedupe key is per label and UTC day,
// same cadence as the deployment-wide budget's own thresholds.
func (h *Hub) labelBudgetThresholds(ctx context.Context, d store.Deployment, t store.Task) {
	rules := PolicyOf(d).Budget.LabelUsdPerDay
	if len(rules) == 0 {
		return
	}
	day := time.UnixMilli(dayStartMs()).UTC().Format(time.DateOnly)
	for kv, max := range rules {
		if max <= 0 {
			continue
		}
		key, val, ok := strings.Cut(kv, "=")
		if !ok || t.Labels[key] != val {
			continue
		}
		spent, err := h.Store.LabelSpentToday(ctx, d.Namespace, d.Name, key, val, dayStartMs())
		if err != nil {
			continue
		}
		for _, pct := range []int{50, 80, 100} {
			if spent < max*float64(pct)/100 {
				break
			}
			severity := map[int]string{50: "info", 80: "warning", 100: "critical"}[pct]
			_, _ = h.Store.AppendEventOnce(ctx, store.Event{Namespace: d.Namespace, Type: "budget.threshold", Deployment: d.Name, Severity: severity,
				Data:      map[string]any{"percent": pct, "scope": "label", "label": kv, "spentUsd": spent, "limitUsd": max, "day": day},
				DedupeKey: fmt.Sprintf("budget:label:%s/%s:%s:%s:%d", d.Namespace, d.Name, kv, day, pct)})
		}
	}
}

func (h *Hub) narrowForTreeBudget(ctx context.Context, rootTaskID string, combined *store.TaskOverrides) *store.TaskOverrides {
	root, err := h.Store.GetTask(ctx, rootTaskID)
	if err != nil {
		return combined
	}
	rootOv, _ := store.ParseOverrides(root.Overrides)
	if rootOv == nil || rootOv.Budget == nil || (rootOv.Budget.MaxTokens <= 0 && rootOv.Budget.MaxUsd <= 0) {
		return combined
	}
	inTok, outTok, usd, err := h.Store.TreeUsage(ctx, rootTaskID)
	if err != nil {
		return combined
	}
	tokens := inTok + outTok
	if combined == nil {
		combined = &store.TaskOverrides{}
	}
	if combined.Budget == nil {
		combined.Budget = &store.RunBudget{}
	}
	if max := rootOv.Budget.MaxTokens; max > 0 {
		remainder := max - tokens
		if remainder < 1 {
			remainder = 1
		}
		if combined.Budget.MaxTokens <= 0 || remainder < combined.Budget.MaxTokens {
			combined.Budget.MaxTokens = remainder
		}
	}
	if max := rootOv.Budget.MaxUsd; max > 0 {
		remainder := max - usd
		if remainder < 0.0001 {
			remainder = 0.0001
		}
		if combined.Budget.MaxUsd <= 0 || remainder < combined.Budget.MaxUsd {
			combined.Budget.MaxUsd = remainder
		}
	}
	return combined
}
