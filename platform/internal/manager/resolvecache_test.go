package manager

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

func cacheManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := New(Config{HubURL: "http://127.0.0.1:1", HostBin: "agen-host", StoreURL: "sqlite:x", DataDir: dir, NestToken: "x",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Cached call tokens survive a Manager restart (Hub-down operation),
// are per caller, and an expired one is never handed out.
func TestResolveCache(t *testing.T) {
	dir := t.TempDir()
	m := cacheManager(t, dir)
	live := &agenv1.ResolveResponse{Endpoints: []string{"http://gw/a2a/default/writer"}, Token: "tok-live", TokenExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
	dead := &agenv1.ResolveResponse{Endpoints: []string{"http://gw/a2a/default/old"}, Token: "tok-dead", TokenExpiresAt: timestamppb.New(time.Now().Add(-time.Minute))}
	m.rememberResolve("default", "boss", "default", "writer", live)
	m.rememberResolve("default", "boss", "default", "old", dead)

	if r, ok := m.cachedResolve("default", "boss", "default", "writer"); !ok || r.Token != "tok-live" {
		t.Fatalf("live entry: %v %v", r, ok)
	}
	if _, ok := m.cachedResolve("default", "other", "default", "writer"); ok {
		t.Fatal("another caller got boss's token")
	}
	if _, ok := m.cachedResolve("default", "boss", "default", "old"); ok {
		t.Fatal("expired token handed out")
	}
	st, err := os.Stat(filepath.Join(dir, resolvedFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("cache file mode %v", st.Mode())
	}

	// A new Manager (restart during a Hub outage) still has the token.
	m2 := cacheManager(t, dir)
	m2.loadResolved()
	if r, ok := m2.cachedResolve("default", "boss", "default", "writer"); !ok || r.Token != "tok-live" || r.Endpoints[0] != live.Endpoints[0] {
		t.Fatalf("after restart: %v %v", r, ok)
	}
}

// With StoreStrict, a namespace without its own Store URL is never
// started on the default (unscoped) one.
func TestStrictStoreMapping(t *testing.T) {
	m := cacheManager(t, t.TempDir())
	m.cfg.StoreURLs = map[string]string{"team-a": "postgres://a"}
	m.cfg.StoreStrict = true
	err := m.start(t.Context(), &agenv1.Assignment{Namespace: "team-b", Deployment: "x", DefinitionDigest: "sha256:x"})
	if err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("unmapped namespace: %v", err)
	}
	if got := m.storeFor("team-a"); got != "postgres://a" {
		t.Fatal(got)
	}
}
