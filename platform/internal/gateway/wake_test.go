package gateway

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/manager"
)

// slowFleet has no ready instance until readyAt; its first wake fails.
type slowFleet struct {
	readyAt time.Time
	mu      sync.Mutex
	wakes   []time.Time
}

func (f *slowFleet) Acquire(ns, dep string) *manager.Slot {
	if time.Now().Before(f.readyAt) {
		return nil
	}
	return &manager.Slot{InstanceID: "i1"}
}
func (f *slowFleet) Assignment(ns, dep string) (*agenv1.Assignment, bool)  { return nil, false }
func (f *slowFleet) Saturated(ns, dep string) bool                         { return false }
func (f *slowFleet) DefinitionDir(context.Context, string) (string, error) { return "", nil }
func (f *slowFleet) TokenKeys() map[string]ed25519.PublicKey               { return nil }
func (f *slowFleet) ReportActivity(ctx context.Context, ns, dep string, saturated bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !time.Now().Before(f.readyAt) {
		return nil // the touch after a slot is acquired, not a wake
	}
	f.wakes = append(f.wakes, time.Now())
	if len(f.wakes) == 1 {
		return errors.New("hub unreachable")
	}
	return nil
}

// A call waiting for a slow cold start keeps the deployment awake: the wake
// is repeated while it waits (so the autoscaler sees activity and does not
// scale it back to zero), and a failed first wake is retried.
func TestWaitingCallKeepsWaking(t *testing.T) {
	f := &slowFleet{readyAt: time.Now().Add(2500 * time.Millisecond)}
	g := New(f, "http://gw", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s, rerr := g.acquire(context.Background(), "default", "slow"); rerr != nil || s == nil {
		t.Fatalf("acquire: %v", rerr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// ~2.5 s of waiting: the failed first wake plus about one a second.
	if len(f.wakes) < 3 {
		t.Fatalf("%d wake requests while waiting 2.5s, want >= 3", len(f.wakes))
	}
	for i := 2; i < len(f.wakes); i++ {
		if gap := f.wakes[i].Sub(f.wakes[i-1]); gap < 900*time.Millisecond {
			t.Fatalf("wakes %v apart; at most one a second per deployment", gap)
		}
	}
}

type countingFleet struct {
	slowFleet
	mu      sync.Mutex
	reports int
}

func (f *countingFleet) ReportActivity(ctx context.Context, ns, dep string, saturated bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports++
	return nil
}

func (f *countingFleet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reports
}

// A call that runs longer than the idle timeout keeps its deployment awake:
// activity is reported while it runs, and no more once it ends.
func TestRunningCallReportsActivity(t *testing.T) {
	f := &countingFleet{}
	g := New(f, "http://gw", slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.beatEvery, g.touchEvery = 20*time.Millisecond, 0
	stop := g.heartbeat(context.Background(), "default", "long")
	time.Sleep(200 * time.Millisecond)
	stop()
	stop() // safe to call twice
	time.Sleep(50 * time.Millisecond)
	n := f.count()
	if n < 3 {
		t.Fatalf("%d activity reports in 200ms of running, want >= 3", n)
	}
	time.Sleep(100 * time.Millisecond)
	if f.count() != n {
		t.Fatalf("activity reported after the call ended: %d -> %d", n, f.count())
	}
}
