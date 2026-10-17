package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Provider health (docs/architecture.md §8). A task that fails with a
// provider account error (invalid or expired key, no credit, key limit)
// holds its provider account: every deployment using the same provider and
// key takes no new tasks, does not scale up for queued work and is not woken
// for A2A calls, so queued work waits instead of failing one task at a time.
// After a backoff one probe task goes through; any run of the account that
// succeeds afterwards (task or A2A) ends the hold. Operators can end it at
// once (ResumeProvider), e.g. after topping up credit.

// accountErrorMarker is how the engine reports account errors in run errors.
const accountErrorMarker = "provider account error"

// accounts caches a definition's provider account by digest.
var accounts sync.Map // digest -> string

// accountOf names the provider account a deployment uses:
// "<provider>:<key secret>[@<base url>]", or "" for scripted providers.
func (h *Hub) accountOf(ctx context.Context, d store.Deployment) string {
	if v, ok := accounts.Load(d.DefinitionDigest); ok {
		return v.(string)
	}
	def, err := h.Store.GetDefinition(ctx, d.DefinitionDigest)
	if err != nil {
		return ""
	}
	var hs struct {
		Provider     string `json:"provider"`
		APIKeySecret string `json:"apiKeySecret"`
		BaseURL      string `json:"baseUrl"`
	}
	_ = json.Unmarshal(def.Files["x-agen/harness.json"], &hs)
	acct := ""
	switch hs.Provider {
	case "openrouter", "openai":
		key := hs.APIKeySecret
		if key == "" {
			key = map[string]string{"openrouter": "OPENROUTER_API_KEY", "openai": "OPENAI_API_KEY"}[hs.Provider]
		}
		acct = hs.Provider + ":" + key
		if hs.BaseURL != "" {
			acct += "@" + strings.TrimRight(hs.BaseURL, "/")
		}
	}
	accounts.Store(d.DefinitionDigest, acct)
	return acct
}

// providerHold returns the hold on a deployment's account, if any.
func (h *Hub) providerHold(ctx context.Context, d store.Deployment) (store.ProviderHold, bool) {
	acct := h.accountOf(ctx, d)
	if acct == "" {
		return store.ProviderHold{}, false
	}
	hold, err := h.Store.GetProviderHold(ctx, acct)
	return hold, err == nil
}

func heldError(hold store.ProviderHold) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("provider account %s is held after an account error (%s); next try at %s",
		hold.Account, hold.Reason, time.UnixMilli(hold.RetryMs).UTC().Format(time.RFC3339)))
}

// providerTaskEnded updates holds when a task ends: an account error holds
// the account (or, for a failed probe, backs off further); a probe that
// failed for another reason may be retried at once.
func (h *Hub) providerTaskEnded(ctx context.Context, t store.Task) {
	d, err := h.Store.GetDeployment(ctx, t.Namespace, t.Deployment)
	if err != nil {
		return
	}
	acct := h.accountOf(ctx, d)
	if acct == "" {
		return
	}
	hold, held := h.providerHold(ctx, d)
	accountFailure := t.State == "failed" && strings.Contains(t.Error, accountErrorMarker)
	switch {
	case accountFailure && (!held || hold.ProbeTask == t.ID):
		reason := t.Error
		if i := strings.Index(reason, accountErrorMarker); i >= 0 {
			reason = strings.TrimSpace(strings.TrimPrefix(reason[i+len(accountErrorMarker):], ":"))
		}
		if len(reason) > 300 {
			reason = reason[:300] + "…"
		}
		hold, created, err := h.Store.HoldProvider(ctx, acct, reason)
		if err != nil {
			h.logger().Warn("provider hold failed", "account", acct, "err", err)
			return
		}
		h.logger().Warn("provider account held", "account", acct, "reason", reason, "next_try", time.UnixMilli(hold.RetryMs).UTC())
		if created {
			h.notifyTargets(ctx, d.Namespace, d.Name, "provider.held", "hold", providerHoldProto(hold))
		}
	case held && hold.ProbeTask == t.ID && t.State != "succeeded":
		_ = h.Store.ReleaseProbe(ctx, acct, t.ID)
	}
}

// checkProviderHolds ends holds whose account works again: a run of one of
// its deployments that started after the hold succeeded.
func (h *Hub) checkProviderHolds(ctx context.Context) error {
	holds, err := h.Store.ListProviderHolds(ctx)
	if err != nil || len(holds) == 0 {
		return err
	}
	deps, err := h.Store.ListDeployments(ctx, "")
	if err != nil {
		return err
	}
	for _, hold := range holds {
		var refs [][2]string
		var some store.Deployment
		for _, d := range deps {
			if h.accountOf(ctx, d) == hold.Account {
				refs = append(refs, [2]string{d.Namespace, d.Name})
				some = d
			}
		}
		ok, err := h.Store.RunSucceededSince(ctx, refs, hold.HeldMs)
		if err != nil {
			return err
		}
		if ok || len(refs) == 0 {
			if cleared, _ := h.Store.ClearProviderHold(ctx, hold.Account); cleared && len(refs) > 0 {
				h.logger().Info("provider account works again", "account", hold.Account)
				h.notifyTargets(ctx, some.Namespace, some.Name, "provider.resumed", "hold", providerHoldProto(hold))
			}
		}
	}
	return nil
}

func providerHoldProto(h store.ProviderHold) *agenv1.ProviderHold {
	return &agenv1.ProviderHold{Account: h.Account, Reason: h.Reason, Failures: int32(h.Failures), HeldAt: ms(h.HeldMs), RetryAt: ms(h.RetryMs), ProbeTaskId: h.ProbeTask}
}

func (h *Hub) ListProviderHolds(ctx context.Context, _ *connect.Request[agenv1.ListProviderHoldsRequest]) (*connect.Response[agenv1.ListProviderHoldsResponse], error) {
	holds, err := h.Store.ListProviderHolds(ctx)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.ListProviderHoldsResponse{}
	for _, x := range holds {
		out.Holds = append(out.Holds, providerHoldProto(x))
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) ResumeProvider(ctx context.Context, req *connect.Request[agenv1.ResumeProviderRequest]) (*connect.Response[agenv1.ResumeProviderResponse], error) {
	if req.Msg.Account == "" {
		return nil, invalid("account is required (agen providers lists held accounts)")
	}
	ok, err := h.Store.ClearProviderHold(ctx, req.Msg.Account)
	if err != nil {
		return nil, connectErr(err)
	}
	if !ok {
		return nil, connectErr(fmt.Errorf("%w: no hold on %s", store.ErrNotFound, req.Msg.Account))
	}
	h.logger().Info("provider account resumed by operator", "account", req.Msg.Account, "by", PrincipalFrom(ctx).ID)
	return connect.NewResponse(&agenv1.ResumeProviderResponse{}), nil
}
