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
	return pd
}
