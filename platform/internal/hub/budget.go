package hub

import (
	"context"
	"fmt"
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
