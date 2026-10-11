package hub

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/prjvvl/agen/platform/internal/store"
)

// Scheduler is the Hub leader's control loop (docs/architecture.md §5). Every
// tick it takes or renews the leader lease and, only while leader:
// requeues tasks whose lease expired, expires approvals, marks silent Nests
// lost and places each deployment's desired instances on active Nests.
// Assignment writes are fenced by the lease epoch, so a stale leader cannot
// overwrite a newer leader's placement.
type Scheduler struct {
	Hub *Hub
	// Holder identifies this Hub replica in the leader lease.
	Holder    string
	Tick      time.Duration
	LeaseTTL  time.Duration
	LostAfter time.Duration
	// MaxTaskAttempts fails a task whose lease expired this many times.
	MaxTaskAttempts int
	// Autoscaling runs the autoscaler each tick (default on).
	Autoscaling bool
	// Steps (optional) run on the leader after placement each tick
	// (autoscaler, triggers).
	Steps []func(ctx context.Context, epoch int64) error
	Log   *slog.Logger
	// Retention deletes spans, log lines and trigger events older than this,
	// once an hour (0 keeps them).
	Retention time.Duration
	pruned    time.Time
}

// NewScheduler returns a scheduler with default timings.
func (h *Hub) NewScheduler(holder string) *Scheduler {
	return &Scheduler{Hub: h, Holder: holder, Tick: time.Second, LeaseTTL: 10 * time.Second, LostAfter: 15 * time.Second, MaxTaskAttempts: 5, Autoscaling: true, Log: slog.Default()}
}

// Run ticks until ctx is done, then releases leadership.
func (s *Scheduler) Run(ctx context.Context) {
	var epoch int64
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		e, err := s.Step(ctx)
		if err != nil && ctx.Err() == nil {
			s.Log.Warn("scheduler step failed", "err", err)
		}
		if e > 0 {
			epoch = e
		}
		select {
		case <-ctx.Done():
			if epoch > 0 {
				_ = s.Hub.Store.ReleaseLease(context.Background(), s.Hub.Store.LeaderLeaseName(), s.Holder, epoch)
			}
			return
		case <-t.C:
		}
	}
}

// Step runs one tick. It returns the leader epoch, or 0 when another replica
// is leader.
func (s *Scheduler) Step(ctx context.Context) (int64, error) {
	st := s.Hub.Store
	epoch, err := st.AcquireLease(ctx, st.LeaderLeaseName(), s.Holder, s.LeaseTTL.Milliseconds())
	if errors.Is(err, store.ErrConflict) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	_, failed, err := st.RequeueExpiredLeases(ctx, s.MaxTaskAttempts)
	if err != nil {
		return epoch, err
	}
	for _, id := range failed {
		go func() {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			s.Hub.afterTaskEnded(c, id)
		}()
	}
	if _, err := st.ExpireApprovals(ctx); err != nil {
		return epoch, err
	}
	lost, err := st.MarkLostNests(ctx, store.NowMs()-s.LostAfter.Milliseconds())
	if err != nil {
		return epoch, err
	}
	for _, id := range lost {
		s.Log.Warn("nest lost; rescheduling its instances", "nest", id)
		if err := st.DeleteInstancesOfNest(ctx, id); err != nil {
			return epoch, err
		}
	}
	// Triggers first: the tasks they queue are seen by the autoscaler in the
	// same tick.
	if err := s.FireTriggers(ctx, epoch); err != nil {
		return epoch, err
	}
	if s.Autoscaling {
		if err := s.Autoscale(ctx, epoch); err != nil {
			return epoch, err
		}
	}
	if err := s.Place(ctx, epoch); err != nil {
		return epoch, err
	}
	for _, step := range s.Steps {
		if err := step(ctx, epoch); err != nil {
			return epoch, err
		}
	}
	if s.Retention > 0 && time.Since(s.pruned) > time.Hour {
		s.pruned = time.Now()
		n, err := st.PruneObservability(ctx, store.NowMs()-s.Retention.Milliseconds())
		if err != nil {
			return epoch, err
		}
		if n > 0 {
			s.Log.Info("pruned old trace data", "rows", n, "retention", s.Retention)
		}
	}
	return epoch, nil
}

// Place reconciles every deployment's assignments with its desired count.
func (s *Scheduler) Place(ctx context.Context, epoch int64) error {
	st := s.Hub.Store
	nests, err := st.ListNests(ctx)
	if err != nil {
		return err
	}
	deps, err := st.ListDeployments(ctx, "")
	if err != nil {
		return err
	}
	all, err := st.AllAssignments(ctx)
	if err != nil {
		return err
	}
	current := map[[2]string][]store.Assignment{}
	used := map[string]int{}
	for _, a := range all {
		k := [2]string{a.Namespace, a.Deployment}
		current[k] = append(current[k], a)
		used[a.NestID] += a.Count
	}
	for _, d := range deps {
		k := [2]string{d.Namespace, d.Name}
		cur := current[k]
		for _, a := range cur {
			used[a.NestID] -= a.Count
		}
		want := d.Desired
		if d.Kind == "singleton" && want > 1 {
			want = 1
		}
		next := Placement(d, want, nests, cur, used)
		for _, a := range next {
			used[a.NestID] += a.Count
		}
		if !sameStoreAssignments(cur, next) {
			if err := st.SetAssignments(ctx, epoch, d.Namespace, d.Name, next); err != nil {
				return err
			}
		}
	}
	return nil
}

// Placement computes a deployment's assignments: keep existing instances on
// eligible Nests (active, labels match, within capacity), remove surplus from
// the most loaded Nest first, and add missing ones to the Nest with the most
// free capacity. usedByOthers counts other deployments' instances per Nest.
// A Nest with capacity 0 has no limit. Instances that fit nowhere stay
// unplaced until capacity appears.
func Placement(d store.Deployment, want int, nests []store.Nest, cur []store.Assignment, usedByOthers map[string]int) []store.Assignment {
	labels := PolicyOf(d).Placement.GetLabels()
	eligible := map[string]store.Nest{}
	for _, n := range nests {
		if n.State != "active" {
			continue
		}
		ok := true
		for k, v := range labels {
			ok = ok && n.Labels[k] == v
		}
		if ok {
			eligible[n.ID] = n
		}
	}
	free := func(id string, have int) int {
		c := eligible[id].Capacity
		if c <= 0 {
			return 1 << 30
		}
		return c - usedByOthers[id] - have
	}
	counts := map[string]int{}
	total := 0
	for _, a := range cur {
		if _, ok := eligible[a.NestID]; !ok {
			continue
		}
		keep := a.Count
		if f := free(a.NestID, 0); keep > f {
			keep = max(f, 0)
		}
		counts[a.NestID] = keep
		total += keep
	}
	ids := make([]string, 0, len(eligible))
	for id := range eligible {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for total > want {
		best := ""
		for _, id := range ids {
			if counts[id] > 0 && (best == "" || counts[id] > counts[best]) {
				best = id
			}
		}
		counts[best]--
		total--
	}
	for total < want {
		best, bestFree := "", 0
		for _, id := range ids {
			if f := free(id, counts[id]); f > 0 && (best == "" || f > bestFree || (f == bestFree && counts[id] < counts[best])) {
				best, bestFree = id, f
			}
		}
		if best == "" {
			break
		}
		counts[best]++
		total++
	}
	var out []store.Assignment
	for _, id := range ids {
		if counts[id] > 0 {
			out = append(out, store.Assignment{Namespace: d.Namespace, Deployment: d.Name, NestID: id, Count: counts[id],
				DefinitionDigest: d.DefinitionDigest, Generation: d.Generation})
		}
	}
	return out
}

func sameStoreAssignments(a, b []store.Assignment) bool {
	if len(a) != len(b) {
		return false
	}
	byNest := map[string]store.Assignment{}
	for _, x := range a {
		byNest[x.NestID] = x
	}
	for _, y := range b {
		x, ok := byNest[y.NestID]
		if !ok || x.Count != y.Count || x.DefinitionDigest != y.DefinitionDigest || x.Generation != y.Generation {
			return false
		}
	}
	return true
}
