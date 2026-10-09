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
