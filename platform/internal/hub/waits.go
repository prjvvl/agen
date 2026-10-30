package hub

import (
	"context"

	"github.com/prjvvl/agen/platform/internal/store"
)

// deploymentWait is why work of a deployment waits ("" = it can run now).
type deploymentWait struct{ reason string }

// waitReason says why a queued task has not started (see Task.wait_reason);
// "" for a task that is not queued or is about to be taken. cache keeps
// the per-deployment part across the tasks of one response.
func (h *Hub) waitReason(ctx context.Context, t store.Task, cache map[string]deploymentWait) string {
	if t.State != "queued" {
		return ""
	}
	if t.NotBeforeMs > store.NowMs() {
		return "not_before"
	}
	key := t.Namespace + "/" + t.Deployment
	dw, ok := cache[key]
	if !ok {
		dw = deploymentWait{reason: h.deploymentWaitReason(ctx, t.Namespace, t.Deployment)}
		cache[key] = dw
	}
	// Held back by the deployment as a whole first: nothing of it runs.
	switch dw.reason {
	case "paused", "budget", "provider_held":
		return dw.reason
	}
	busy, caller, locked, err := h.Store.QueuedBlockers(ctx, t)
	switch {
	case err != nil:
		return ""
	case busy:
		return "conversation_busy"
	case caller:
		return "caller_running"
	case locked:
		return "locks"
	}
	return dw.reason
}

// deploymentWaitReason is why a deployment's due work waits, if it does.
func (h *Hub) deploymentWaitReason(ctx context.Context, ns, name string) string {
	d, err := h.Store.GetDeployment(ctx, ns, name)
	if err != nil {
		return ""
	}
	if d.Paused {
		return "paused"
	}
	if _, exhausted, err := h.spentToday(ctx, d); err == nil && exhausted {
		return "budget"
	}
	if _, held := h.providerHold(ctx, d); held {
		return "provider_held"
	}
	instances, err := h.Store.ListInstances(ctx, ns, name, "")
	if err != nil {
		return ""
	}
	ready, starting, busy := 0, 0, 0
	for _, in := range instances {
		switch in.State {
		case "ready":
			ready++
		case "starting":
			starting++
		case "busy":
			busy++
		}
	}
	switch {
	case ready > 0:
		return ""
	case starting > 0:
		return "starting"
	case busy > 0:
		return "busy"
	case d.Desired == 0:
		return "asleep"
	}
	if placed, err := h.Store.AssignedCount(ctx, ns, name); err == nil && placed < d.Desired {
		return "capacity"
	}
	return "starting"
}
