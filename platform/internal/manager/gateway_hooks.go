package manager

import (
	"context"
	"sync"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// Slot is a reserved task slot on a ready instance (used by the Gateway to
// run an A2A call directly, without the Hub).
type Slot struct {
	InstanceID string
	Host       *HostClient
	m          *Manager
	in         *instance
	once       sync.Once
}

// Release frees the slot.
func (s *Slot) Release() { s.once.Do(func() { s.m.release(s.in) }) }

// Acquire reserves a slot on the least loaded ready instance of a deployment
// in this Nest, or returns nil if none is free.
func (m *Manager) Acquire(ns, dep string) *Slot {
	in := m.pick(ns, dep)
	if in == nil {
		return nil
	}
	m.mu.Lock()
	host := in.host
	m.mu.Unlock()
	return &Slot{InstanceID: in.id, Host: host, m: m, in: in}
}

// Saturated reports whether this Nest runs ready instances of a deployment
// and every one of them is at its concurrency limit (not merely starting).
func (m *Manager) Saturated(ns, dep string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	serving := 0
	for _, in := range m.instances {
		if in.ns != ns || in.dep != dep || in.retiring {
			continue
		}
		switch in.state {
		case "starting":
			return false // capacity is on its way
		case "ready", "busy":
			serving++
			if in.running < in.maxConcurrency && !in.served {
				return false
			}
		}
	}
	return serving > 0
}

// Assignment returns this Nest's current assignment for a deployment.
func (m *Manager) Assignment(ns, dep string) (*agenv1.Assignment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.assignments[[2]string{ns, dep}]
	return a, ok
}

// ReportActivity tells the Hub a deployment has traffic; a sleeping
// deployment is woken (desired >= 1). Fails if this Nest may not run it.
func (m *Manager) ReportActivity(ctx context.Context, ns, dep string, saturated bool) error {
	_, err := m.hub().ReportActivity(ctx, connect.NewRequest(&agenv1.ReportActivityRequest{Ref: &agenv1.DeploymentRef{Namespace: ns, Name: dep},
		Saturated: saturated}))
	return err
}

// DefinitionDir returns the unpacked bundle directory of a definition.
func (m *Manager) DefinitionDir(ctx context.Context, digest string) (string, error) {
	return m.definitionDir(ctx, digest)
}
