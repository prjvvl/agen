package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prjvvl/agen/platform/internal/store"
)

// Scenario 4: a singleton sleeps between uses; a cron trigger and an HTTP
// (A2A) call each wake it; its memory (one session and conversation in the
// Store) survives although every wake is a new host process.
func TestScenario4SingletonWokenByCronAndHTTPKeepsMemory(t *testing.T) {
	_, home := startUp(t)
	bundle := filepath.Join(t.TempDir(), "diary")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	os.WriteFile(filepath.Join(bundle, "x-agen", "config.json"), []byte(`{"kind":"singleton","scale":{"min":0,"max":1,"idleTimeout":"3s"},
		"triggers":[{"type":"cron","name":"nightly","schedule":"@every 12s","input":"cron entry"}]}`), 0o644)
	os.WriteFile(filepath.Join(bundle, "x-agen", "fake-script.json"), []byte(`{"cycle":true,"responses":[{"text":"noted"}]}`), 0o644)
	agen(t, 0, "deploy", bundle, "--name", "diary")

	st, err := store.Open(context.Background(), defaultStoreURL(home))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.DB()
	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		return n
	}
	asleep := func() bool {
		return !strings.Contains(agen(t, 0, "ps", "--all"), "default/diary") && strings.Contains(agen(t, 0, "ps"), "default/diary  singleton  0      0")
	}

	// 1. Asleep; the cron trigger wakes it and its task runs.
	if !asleep() {
		t.Fatal("singleton running before any trigger")
	}
	waitUntil(t, "cron run", 60*time.Second, func() bool {
		return count("SELECT COUNT(*) FROM runs WHERE deployment = 'diary' AND status = 'succeeded'") >= 1
	})
	// 2. It goes back to sleep after the idle timeout.
	// The cron-woken instance is listed, then gone (the process exited).
	var first string
	if err := db.QueryRow("SELECT owner FROM runs WHERE deployment = 'diary' ORDER BY started_ms LIMIT 1").Scan(&first); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "cron-woken instance listed", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "ps", "--all"), first) })
	waitUntil(t, "sleep after cron", 30*time.Second, func() bool { return !strings.Contains(agen(t, 0, "ps", "--all"), first) && asleep() })
	// 3. An HTTP call wakes it and is answered.
	if out := agen(t, 0, "call", "diary", "http entry"); strings.TrimSpace(out) != "noted" {
		t.Fatalf("call: %q", out)
	}
	agen(t, 0, "stop", "diary")

	// Memory: every run (cron and HTTP, on different host processes) is in
	// the singleton's one session and conversation, which holds both entries.
	runs := count("SELECT COUNT(*) FROM runs WHERE deployment = 'diary'")
	owners := count("SELECT COUNT(DISTINCT owner) FROM runs WHERE deployment = 'diary'")
	convs := count("SELECT COUNT(DISTINCT conversation_id) FROM runs WHERE deployment = 'diary'")
	sessions := count("SELECT COUNT(*) FROM sessions WHERE deployment = 'diary'")
	if rows, err := db.Query("SELECT id, owner, task_id, status, started_ms FROM runs WHERE deployment = 'diary' ORDER BY started_ms"); err == nil {
		for rows.Next() {
			var id, owner, task, status string
			var st int64
			rows.Scan(&id, &owner, &task, &status, &st)
			t.Logf("run %s owner=%s task=%s status=%s started=%d", id, owner, task, status, st)
		}
		rows.Close()
	}
	if runs < 2 || owners < 2 || convs != 1 || sessions != 1 {
		t.Fatalf("runs %d on %d instances, %d conversations, %d sessions", runs, owners, convs, sessions)
	}
	var cronMsgs, httpMsgs int
	cronMsgs = count("SELECT COUNT(*) FROM messages m JOIN runs r ON r.conversation_id = m.conversation_id WHERE r.deployment = 'diary' AND m.body LIKE '%cron entry%'")
	httpMsgs = count("SELECT COUNT(*) FROM messages m JOIN runs r ON r.conversation_id = m.conversation_id WHERE r.deployment = 'diary' AND m.body LIKE '%http entry%'")
	if cronMsgs == 0 || httpMsgs == 0 {
		t.Fatalf("conversation lacks entries: cron %d http %d", cronMsgs, httpMsgs)
	}
	src := count("SELECT COUNT(*) FROM tasks WHERE deployment = 'diary' AND source = 'cron:nightly' AND state = 'succeeded'")
	if src < 1 {
		t.Fatal("no succeeded cron task")
	}
}
