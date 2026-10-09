package hub

import (
	"testing"
	"time"

	"github.com/prjvvl/agen/platform/internal/store"
)

func dep(kind string, desired int, lastActivityMs int64, scale string) store.Deployment {
	return store.Deployment{Kind: kind, Desired: desired, LastActivityMs: lastActivityMs, Scale: []byte(scale)}
}

func TestDesiredFor(t *testing.T) {
	now := time.Now().UnixMilli()
	recent, old := now-1000, now-10*60*1000
	cases := []struct {
		name string
		d    store.Deployment
		load int
		want int
	}{
		{"pool scales up at once", dep("pool", 0, old, `{"min":0,"max":5,"targetQueuePerInstance":2}`), 7, 4},
		{"pool capped at max", dep("pool", 1, recent, `{"min":0,"max":3}`), 50, 3},
		{"pool holds while recently active", dep("pool", 3, recent, `{"min":0,"max":5,"idleTimeoutSeconds":60}`), 0, 3},
		{"pool scales to min after idle", dep("pool", 3, old, `{"min":1,"max":5,"idleTimeoutSeconds":60}`), 0, 1},
		{"pool without idle timeout uses the default", dep("pool", 2, now-60*1000, `{"min":0,"max":5}`), 0, 2},
		{"pool partial scale down after idle", dep("pool", 4, old, `{"min":0,"max":5,"idleTimeoutSeconds":60}`), 1, 1},
		{"task stops as soon as the queue is empty", dep("task", 2, recent, `{"min":0,"max":5}`), 0, 0},
		{"task follows load", dep("task", 0, recent, `{"min":0,"max":5}`), 3, 3},
		{"desired below min is raised", dep("pool", 0, old, `{"min":2,"max":5}`), 0, 2},
		{"singleton max one", dep("singleton", 0, old, `{"min":0,"max":1}`), 9, 1},
	}
	for _, c := range cases {
		if got := DesiredFor(c.d, c.load, now); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}
