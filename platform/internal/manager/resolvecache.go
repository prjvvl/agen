package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// resolvedEntry is a cached Resolve for one caller deployment and target:
// endpoints plus the call token, so delegation keeps working while the Hub
// is down (until the token expires).
type resolvedEntry struct {
	CallerNS, CallerDep, NS, Name string
	// Resp is protojson (stable on disk across versions).
	Resp json.RawMessage
	At   time.Time

	resp *agenv1.ResolveResponse
}

// resolvedFile keeps the cache across Manager restarts (it holds bearer
// tokens: 0600, in the Nest's data dir).
const resolvedFile = "resolved.json"

// callTokenRefreshAfter: cached tokens older than this are refreshed in the
// background while the Hub is up, so an outage starts with a fresh token.
const callTokenRefreshAfter = 10 * time.Minute

func resolveKey(callerNS, callerDep, ns, name string) string {
	return callerNS + "/" + callerDep + " -> " + ns + "/" + name
}

// rememberResolve caches a Resolve (caller holds no lock).
func (m *Manager) rememberResolve(callerNS, callerDep, ns, name string, resp *agenv1.ResolveResponse) {
	raw, _ := protojson.Marshal(resp)
	m.mu.Lock()
	m.resolved[resolveKey(callerNS, callerDep, ns, name)] = &resolvedEntry{CallerNS: callerNS, CallerDep: callerDep, NS: ns, Name: name,
		Resp: raw, At: time.Now(), resp: resp}
	m.mu.Unlock()
	m.saveResolved()
}

// cachedResolve returns a cached Resolve whose call token is still valid.
func (m *Manager) cachedResolve(callerNS, callerDep, ns, name string) (*agenv1.ResolveResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.resolved[resolveKey(callerNS, callerDep, ns, name)]
	if e == nil || e.resp == nil {
		return nil, false
	}
	if exp := e.resp.GetTokenExpiresAt(); e.resp.Token != "" && exp != nil && time.Now().After(exp.AsTime()) {
		return nil, false // expired: the Gateway would refuse it
	}
	return e.resp, true
}

func (m *Manager) saveResolved() {
	m.mu.Lock()
	b, err := json.Marshal(m.resolved)
	m.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(m.cfg.DataDir, 0o700)
	tmp := filepath.Join(m.cfg.DataDir, resolvedFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(m.cfg.DataDir, resolvedFile))
	}
}

func (m *Manager) loadResolved() {
	b, err := os.ReadFile(filepath.Join(m.cfg.DataDir, resolvedFile))
	if err != nil {
		return
	}
	var saved map[string]*resolvedEntry
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for _, e := range saved {
		r := &agenv1.ResolveResponse{}
		if protojson.Unmarshal(e.Resp, r) == nil {
			e.resp = r
		}
	}
	m.mu.Lock()
	for k, e := range saved {
		if e.resp != nil {
			m.resolved[k] = e
		}
	}
	m.mu.Unlock()
}

// refreshResolved renews cached call tokens older than callTokenRefreshAfter
// while the Hub is reachable.
func (m *Manager) refreshResolved(ctx context.Context) {
	m.mu.Lock()
	var due []*resolvedEntry
	for _, e := range m.resolved {
		if time.Since(e.At) > callTokenRefreshAfter {
			due = append(due, e)
		}
	}
	m.mu.Unlock()
	for _, e := range due {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := m.hub().Resolve(c, connect.NewRequest(&agenv1.ResolveRequest{Ref: &agenv1.DeploymentRef{Namespace: e.NS, Name: e.Name},
			Caller: &agenv1.DeploymentRef{Namespace: e.CallerNS, Name: e.CallerDep}}))
		cancel()
		switch connect.CodeOf(err) {
		case connect.CodeNotFound, connect.CodePermissionDenied, connect.CodeInvalidArgument:
			// The pair is no longer allowed (or the target is gone): drop it.
			m.mu.Lock()
			delete(m.resolved, resolveKey(e.CallerNS, e.CallerDep, e.NS, e.Name))
			m.mu.Unlock()
			m.saveResolved()
			continue
		}
		if err != nil {
			return // Hub unreachable: keep what we have
		}
		m.rememberResolve(e.CallerNS, e.CallerDep, e.NS, e.Name, resp.Msg)
	}
}
