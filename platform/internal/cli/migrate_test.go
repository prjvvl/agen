package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prjvvl/agen/platform/internal/store"
)

func freshPostgresDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("AGEN_TEST_POSTGRES_URL")
	if base == "" {
		if os.Getenv("AGEN_REQUIRE_PG") == "1" {
			t.Fatal("AGEN_REQUIRE_PG=1 but AGEN_TEST_POSTGRES_URL is unset")
		}
		t.Skip("needs AGEN_TEST_POSTGRES_URL")
	}
	admin, err := store.Open(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	name := "agen_mig_" + strings.ToLower(store.NewID())
	if _, err := admin.DB().Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.DB().Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	return base[:strings.LastIndex(base, "/")+1] + name
}

// A local fleet's history (sessions, runs, traces, deployments, tokens)
// moves from SQLite to Postgres with `agen migrate`, and the fleet runs on
// Postgres afterwards with the same credentials.
func TestMigrateLocalFleetToPostgres(t *testing.T) {
	pg := freshPostgresDB(t)
	bin := hostBin(t)
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")
	up := func(extra ...string) chan int {
		done := make(chan int, 1)
		out := &safeBuf{}
		args := append([]string{"up", "--listen", "127.0.0.1:0", "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, extra...)
		go func() { done <- Main(context.Background(), args, out, &safeBuf{}) }()
		waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(out.String(), "agen is up") })
		waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })
		return done
	}
	down := func(done chan int) {
		agen(t, 0, "down")
		<-done
	}

	done := up()
	allow := `"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"allow"}]}`
	for _, a := range []struct{ name, config, script string }{
		{"writer", `{"kind":"pool","scale":{"min":0,"max":1}}`, `{"cycle":true,"responses":[{"text":"Draft ready."}]}`},
		{"boss", `{"kind":"pool","scale":{"min":0,"max":1},"delegates":[{"name":"writer"}],` + allow + `}`,
			`{"perRun":true,"responses":[{"toolCalls":[{"name":"call_agent","arguments":{"agent":"writer","message":"write"}}]},{"text":"Boss done."}]}`},
	} {
		dir := filepath.Join(t.TempDir(), a.name)
		copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), dir)
		os.WriteFile(filepath.Join(dir, "x-agen", "config.json"), []byte(a.config), 0o644)
		os.WriteFile(filepath.Join(dir, "x-agen", "fake-script.json"), []byte(a.script), 0o644)
		agen(t, 0, "deploy", dir, "--name", a.name)
	}
	task := asJSON(t, agen(t, 0, "run", "boss", "go", "--json"))
	before := agen(t, 0, "trace", task["id"].(string))
	if !strings.Contains(before, ": 2 runs,") {
		t.Fatalf("trace before: %s", before)
	}
	// Refused while the fleet runs (the source would keep changing).
	agen(t, 1, "migrate", "--to", pg)
	down(done)

	out := agen(t, 0, "migrate", "--to", pg)
	if !strings.Contains(out, "runs") || !strings.Contains(out, "migrated") {
		t.Fatalf("migrate: %s", out)
	}

	// The same fleet on Postgres: history is there, credentials still work,
	// and new work runs (hosts write run data to Postgres).
	done = up("--store", pg)
	after := agen(t, 0, "trace", task["id"].(string))
	if !strings.Contains(after, ": 2 runs,") || !strings.Contains(after, "[default/writer run ") {
		t.Fatalf("trace after migration:\n%s", after)
	}
	if !strings.Contains(agen(t, 0, "ps"), "default/boss") {
		t.Fatal("deployments not migrated")
	}
	if out := agen(t, 0, "run", "boss", "again"); strings.TrimSpace(out) != "Boss done." {
		t.Fatalf("run on postgres: %q", out)
	}
	down(done)
	pgStore, err := store.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	defer pgStore.Close()
	runs, err := pgStore.ListRuns(context.Background(), "default", "", 100)
	if err != nil || len(runs) != 4 {
		t.Fatalf("runs in postgres: %d %v", len(runs), err)
	}
}
