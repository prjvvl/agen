package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/prjvvl/agen/platform/internal/store"
)

// The Store URL given to Nests and their hosts can be a least-privilege
// role: hosts run agents with it, but it cannot read the Hub's keys or
// write tokens.
func TestHostRoleIsLeastPrivilege(t *testing.T) {
	admin := os.Getenv("AGEN_TEST_POSTGRES_URL")
	if admin == "" {
		t.Skip("needs AGEN_TEST_POSTGRES_URL")
	}
	bin := hostBin(t)
	const password = "host-role-test-password-123"
	t.Setenv("AGEN_HOST_DB_PASSWORD", password)
	out := agen(t, 0, "store", "host-role", "--store", admin, "--role", "agen_host_test")
	if !strings.Contains(out, "agen_host_test") || strings.Contains(out, password) {
		t.Fatalf("output: %s", out)
	}
	agen(t, 0, "store", "host-role", "--store", admin, "--role", "agen_host_test") // idempotent
	u, _ := url.Parse(admin)
	u.User = url.UserPassword("agen_host_test", password)
	hostURL := u.String()

	db, err := sql.Open("pgx", hostURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs").Scan(&n); err != nil {
		t.Fatalf("host role cannot read runs: %v", err)
	}
	for _, q := range []string{
		"SELECT key_pem FROM hub_ca",
		"SELECT private_key FROM hub_token_key",
		"SELECT secret_hash FROM api_tokens",
		"INSERT INTO api_tokens (id, name, secret_hash, scopes, namespaces, created_ms, expires_ms, revoked) VALUES ('x','x','x','[\"admin\"]','[]',0,0,0)",
		"SELECT * FROM tasks",
	} {
		if _, err := db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("host role allowed %q: %v", q, err)
		}
	}

	// A real agent run with the host role's Store URL.
	cmd := exec.Command(bin, "run", filepath.Join(repoRoot(), "examples", "bundles", "hello"), "--input", "hi", "--json", "--store", hostURL)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("agen-host with the host role: %v %s", err, raw)
	}
	var res map[string]any
	json.Unmarshal(raw, &res)
	if res["status"] != "succeeded" {
		t.Fatalf("run: %s", raw)
	}

	// Namespace-scoped roles (row-level security): a host of namespace A
	// runs agents there, but sees nothing of namespace B and cannot write
	// into it.
	nsA, nsB := "ns-a-"+strings.ToLower(store.NewID())[:8], "ns-b-"+strings.ToLower(store.NewID())[:8]
	roleURL := func(role string) string {
		agen(t, 0, "store", "host-role", "--store", admin, "--role", role, "--namespace", map[string]string{"agen_host_a": nsA, "agen_host_b": nsB}[role])
		u, _ := url.Parse(admin)
		u.User = url.UserPassword(role, password)
		return u.String()
	}
	urlA, urlB := roleURL("agen_host_a"), roleURL("agen_host_b")
	runIn := func(storeURL, ns string) (map[string]any, error) {
		out, err := exec.Command(bin, "run", filepath.Join(repoRoot(), "examples", "bundles", "hello"), "--input", "hi", "--json",
			"--store", storeURL, "--namespace", ns).Output()
		var r map[string]any
		json.Unmarshal(out, &r)
		return r, err
	}
	resA, err := runIn(urlA, nsA)
	if err != nil || resA["status"] != "succeeded" {
		t.Fatalf("host A in its namespace: %v %v", resA, err)
	}
	runA, _ := resA["run_id"].(string)
	if runA == "" {
		runA, _ = resA["id"].(string)
	}
	count := func(dbURL, q string) int {
		db, err := sql.Open("pgx", dbURL)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRowContext(ctx, q, nsA).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM sessions WHERE namespace = $1",
		"SELECT COUNT(*) FROM runs WHERE namespace = $1",
		"SELECT COUNT(*) FROM conversations c JOIN sessions s ON s.id = c.session_id WHERE s.namespace = $1",
		"SELECT COUNT(*) FROM messages m JOIN runs r ON r.conversation_id = m.conversation_id WHERE r.namespace = $1",
		"SELECT COUNT(*) FROM spans sp JOIN runs r ON r.id = sp.run_id WHERE r.namespace = $1",
	} {
		if a := count(urlA, q); a == 0 {
			t.Fatalf("host A does not see its own rows: %s", q)
		}
		if admin := count(os.Getenv("AGEN_TEST_POSTGRES_URL"), q); admin == 0 {
			t.Fatalf("the Hub (table owner) no longer sees rows: %s", q)
		}
		if b := count(urlB, q); b != 0 {
			t.Fatalf("host B sees %d rows of namespace A: %s", b, q)
		}
	}
	// Writing into another namespace is refused.
	cross, err := exec.Command(bin, "run", filepath.Join(repoRoot(), "examples", "bundles", "hello"), "--input", "hi", "--json",
		"--store", urlA, "--namespace", nsB).CombinedOutput()
	if err == nil || !strings.Contains(string(cross), "row-level security") {
		t.Fatalf("host A ran an agent in namespace B: %v %s", err, cross)
	}
}
