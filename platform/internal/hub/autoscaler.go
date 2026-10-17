package hub

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/prjvvl/agen/platform/internal/store"
)

// DefaultIdleTimeout applies to pools and singletons without
// scale.idle_timeout. Task deployments default to 0: they stop as soon as
// their queue is empty.
const DefaultIdleTimeout = 5 * time.Minute

// PressureIdle is how long a deployment must be without work before it gives
// its capacity back early, while another deployment waits for Nest capacity.
const PressureIdle = time.Minute

// Autoscale sets every deployment's desired count from its load
// (docs/architecture.md §5): scale up at once to
// clamp(ceil((queued + in-flight) / target_queue_per_instance), min, max);
// scale down only after no task or A2A activity for the idle timeout. Manual
// scaling counts as activity, so it holds for one idle window.
func (s *Scheduler) Autoscale(ctx context.Context, epoch int64) error {
	st := s.Hub.Store
	deps, err := st.ListDeployments(ctx, "")
	if err != nil {
		return err
	}
	now := store.NowMs()
	// Capacity pressure: a deployment wants more instances than its Nests
	// have room for. Idle deployments holding that room then scale down
	// without waiting out their idle timeout, so work that waits gets it.
	starved, err := s.holdingStarvedNests(ctx, deps)
	if err != nil {
		return err
	}
	for _, d := range deps {
		q, err := st.Queue(ctx, d.Namespace, d.Name)
		if err != nil {
			s.Log.Warn("autoscale: queue stats failed", "deployment", d.Namespace+"/"+d.Name, "err", err)
			continue
		}
		load := q.Queued + q.InFlight
		if hold, held := s.Hub.providerHold(ctx, d); held && now < hold.RetryMs {
			load = q.InFlight // queued work waits for the provider account
		}
		_, exhausted, err := s.Hub.spentToday(ctx, d)
		if err != nil {
			s.Log.Warn("autoscale: usage failed", "deployment", d.Namespace+"/"+d.Name, "err", err)
			continue
		}
		desiredFor := func(cur store.Deployment) int {
			if exhausted {
				return 0 // daily budget used up: nothing new runs today
			}
			if starved[[2]string{cur.Namespace, cur.Name}] && load == 0 && now-cur.LastActivityMs >= PressureIdle.Milliseconds() {
				return DesiredFor(cur, 0, math.MaxInt64) // as if idle for good
			}
			return DesiredFor(cur, load, now)
		}
		if next := desiredFor(d); next == d.Desired {
			continue
		}
		// Recompute on the row being written (the policy or desired count may
		// have changed since it was read); the write is fenced to the leader.
		var from, to int
		if _, err := st.UpdateDeploymentFenced(ctx, epoch, d.Namespace, d.Name, func(cur *store.Deployment) error {
			from, to = cur.Desired, desiredFor(*cur)
			if from == to {
				return errNoChange
			}
			cur.Desired = to
			return nil
		}); err != nil {
			if errors.Is(err, errNoChange) {
				continue
			}
			if errors.Is(err, store.ErrFenced) {
				return err // no longer leader
			}
			if ctx.Err() == nil {
				s.Log.Warn("autoscale update failed", "deployment", d.Namespace+"/"+d.Name, "err", err)
			}
			continue
		}
		if from != to {
			s.Log.Info("autoscale", "deployment", d.Namespace+"/"+d.Name, "from", from, "to", to, "queued", q.Queued, "in_flight", q.InFlight)
		}
	}
	return nil
}

var errNoChange = errors.New("no change")

// holdingStarvedNests returns the deployments holding capacity on a full Nest that
// another deployment needs: one that wants more instances than are placed
// and whose placement labels match that Nest.
func (s *Scheduler) holdingStarvedNests(ctx context.Context, deps []store.Deployment) (map[[2]string]bool, error) {
	st := s.Hub.Store
	nests, err := st.ListNests(ctx)
	if err != nil {
		return nil, err
	}
	all, err := st.AllAssignments(ctx)
	if err != nil {
		return nil, err
	}
	placed := map[[2]string]int{}
	used := map[string]int{}
	for _, a := range all {
		placed[[2]string{a.Namespace, a.Deployment}] += a.Count
		used[a.NestID] += a.Count
	}
	full := map[string]bool{}
	for _, d := range deps {
		if d.Paused || d.Desired <= placed[[2]string{d.Namespace, d.Name}] {
			continue
		}
		for _, n := range nests {
			if n.Capacity > 0 && used[n.ID] >= n.Capacity && nestMatches(n, d) {
				full[n.ID] = true
			}
		}
	}
	out := map[[2]string]bool{}
	for _, a := range all {
		if full[a.NestID] {
			out[[2]string{a.Namespace, a.Deployment}] = true
		}
	}
	return out, nil
}

// scaleUpForDemand raises desired to one more than the live instances (up to
// scale.max) when a Gateway reports calls waiting on busy instances. Counting
// live (incl. starting) instances keeps repeated reports from overshooting;
// the autoscaler scales back down after the idle timeout.
func (h *Hub) scaleUpForDemand(ctx context.Context, ns, name string) error {
	insts, err := h.Store.ListInstances(ctx, ns, name, "")
	if err != nil {
		return err
	}
	live := 0
	for _, in := range insts {
		switch in.State {
		case "starting":
			return nil // capacity is already on its way
		case "ready", "busy":
			live++
		}
	}
	_, err = h.Store.UpdateDeployment(ctx, ns, name, func(d *store.Deployment) error {
		want := min(max(d.Desired, live+1), int(PolicyOf(*d).Scale.Max))
		if d.Paused || want <= d.Desired {
			return errNoChange
		}
		d.Desired, d.LastActivityMs = want, store.NowMs()
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	return err
}

// DesiredFor computes a deployment's desired instances for a load at nowMs.
func DesiredFor(d store.Deployment, load int, nowMs int64) int {
	if d.Paused {
		return 0
	}
	sp := PolicyOf(d).Scale
	lo, hi := int(sp.Min), int(sp.Max)
	if hi < lo {
		hi = lo
	}
	target := int(sp.TargetQueuePerInstance)
	if target <= 0 {
		target = 1
	}
	want := clamp((load+target-1)/target, lo, hi)
	cur := clamp(d.Desired, lo, hi)
	if want >= cur {
		return want
	}
	idle := time.Duration(sp.IdleTimeoutSeconds) * time.Second
	if idle == 0 && d.Kind != "task" {
		idle = DefaultIdleTimeout
	}
	if nowMs-d.LastActivityMs >= idle.Milliseconds() {
		return want
	}
	return cur
}
