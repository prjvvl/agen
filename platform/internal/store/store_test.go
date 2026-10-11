package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Every test runs against SQLite and, when AGEN_TEST_POSTGRES_URL is set,
// Postgres. AGEN_REQUIRE_PG=1 makes a missing Postgres URL a failure.
func backends(t *testing.T) map[string]*Store {
	t.Helper()
	ctx := context.Background()
	out := map[string]*Store{}
	lite, err := Open(ctx, "sqlite:"+filepath.ToSlash(filepath.Join(t.TempDir(), "p.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lite.Close() })
	out["sqlite"] = lite.WithLeaderLease("lease-" + NewID())
	if url := os.Getenv("AGEN_TEST_POSTGRES_URL"); url != "" {
		pg, err := Open(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pg.Close() })
		out["postgres"] = pg.WithLeaderLease("lease-" + NewID())
	} else if os.Getenv("AGEN_REQUIRE_PG") == "1" {
		t.Fatal("AGEN_REQUIRE_PG=1 but AGEN_TEST_POSTGRES_URL is unset")
	}
	return out
}

func each(t *testing.T, f func(t *testing.T, s *Store)) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) { f(t, s) })
	}
}

func uniq(prefix string) string { return prefix + "-" + NewID() }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrationsAreSharedWithTheEngine(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		var n int
		must(t, s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version BETWEEN 1 AND 5").Scan(&n))
		if n != 5 {
			t.Fatalf("expected migrations 1-5 recorded, got %d", n)
		}
		// Engine tables and platform tables live side by side.
		for _, table := range []string{"runs", "spans", "effects", "deployments", "tasks", "leases"} {
			if _, err := s.db.Exec("SELECT 1 FROM " + table + " WHERE 1 = 0"); err != nil {
				t.Fatalf("table %s: %v", table, err)
			}
		}
	})
	// Re-opening a migrated database is a no-op.
	dir := t.TempDir()
	url := "sqlite:" + filepath.ToSlash(filepath.Join(dir, "x.db"))
	a, err := Open(context.Background(), url)
	must(t, err)
	a.Close()
	b, err := Open(context.Background(), url)
	must(t, err)
	b.Close()
}

func TestDefinitionsAndDeployments(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		d := Definition{Digest: uniq("sha256:x"), Name: "hello", Files: map[string][]byte{"plugin.json": []byte(`{"name":"hello"}`)}}
		must(t, s.PutDefinition(ctx, d))
		must(t, s.PutDefinition(ctx, d)) // idempotent
		got, err := s.GetDefinition(ctx, d.Digest)
		must(t, err)
		if string(got.Files["plugin.json"]) != `{"name":"hello"}` {
			t.Fatalf("files: %v", got.Files)
		}
		if _, err := s.GetDefinition(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}

		ns := uniq("ns")
		dep, err := s.CreateDeployment(ctx, Deployment{Namespace: ns, Name: "hello", DefinitionDigest: d.Digest, Kind: "pool",
			Scale: json.RawMessage(`{"min":0,"max":3}`)})
		must(t, err)
		if dep.Generation != 1 || string(dep.Scale) != `{"min":0,"max":3}` || string(dep.Triggers) != "[]" {
			t.Fatalf("%+v", dep)
		}
		if _, err := s.CreateDeployment(ctx, dep); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate create: %v", err)
		}
		dep, err = s.SetDesired(ctx, ns, "hello", 2)
		must(t, err)
		if dep.Desired != 2 || dep.Generation != 2 {
			t.Fatalf("%+v", dep)
		}
		must(t, s.TouchActivity(ctx, ns, "hello"))
		list, err := s.ListDeployments(ctx, ns)
		must(t, err)
		if len(list) != 1 || list[0].LastActivityMs == 0 {
			t.Fatalf("%+v", list)
		}
		task, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "hello", Input: "x"})
		must(t, err)
		must(t, s.DeleteDeployment(ctx, ns, "hello"))
		if got, _ := s.GetTask(ctx, task.ID); got.State != "cancelled" {
			t.Fatalf("queued task not cancelled on delete: %s", got.State)
		}
		if err := s.DeleteDeployment(ctx, ns, "hello"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
}

func TestNestsAssignmentsAndLeaderFencing(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		n, err := s.UpsertNest(ctx, Nest{Name: "nest-a", Backend: "native", Labels: map[string]string{"gpu": "no"}, Capacity: 4})
		must(t, err)
		if n.State != "active" || n.Labels["gpu"] != "no" {
			t.Fatalf("%+v", n)
		}
		must(t, s.Heartbeat(ctx, n.ID, 5, "http://127.0.0.1:1"))
		lost, err := s.MarkLostNests(ctx, NowMs()+1)
		must(t, err)
		if !contains(lost, n.ID) {
			t.Fatalf("nest not marked lost: %v", lost)
		}
		must(t, s.Heartbeat(ctx, n.ID, 5, "http://127.0.0.1:1"))
		if got, _ := s.GetNest(ctx, n.ID); got.State != "active" || got.Capacity != 5 {
			t.Fatalf("heartbeat did not revive: %+v", got)
		}

		lease := s.LeaderLeaseName()
		e1, err := s.AcquireLease(ctx, lease, "hub-1", 60_000)
		must(t, err)
		if _, err := s.AcquireLease(ctx, lease, "hub-2", 60_000); !errors.Is(err, ErrConflict) {
			t.Fatalf("second holder must not win a live lease: %v", err)
		}
		if again, _ := s.AcquireLease(ctx, lease, "hub-1", 60_000); again != e1 {
			t.Fatalf("renewal changed epoch %d -> %d", e1, again)
		}
		ns := uniq("ns")
		as := []Assignment{{NestID: n.ID, Count: 2, DefinitionDigest: "d", Generation: 1}}
		must(t, s.SetAssignments(ctx, e1, ns, "hello", as))
		got, err := s.AssignmentsForNest(ctx, n.ID)
		must(t, err)
		if len(got) != 1 || got[0].Count != 2 || got[0].Namespace != ns {
			t.Fatalf("%+v", got)
		}
		// hub-1 loses the lease (expired); hub-2 takes over with a new epoch.
		must(t, s.ReleaseLease(ctx, lease, "hub-1", e1))
		e2, err := s.AcquireLease(ctx, lease, "hub-2", 60_000)
		must(t, err)
		if e2 != e1+1 {
			t.Fatalf("takeover epoch %d, want %d", e2, e1+1)
		}
		if err := s.SetAssignments(ctx, e1, ns, "hello", nil); !errors.Is(err, ErrFenced) {
			t.Fatalf("stale leader must be fenced: %v", err)
		}
		must(t, s.SetAssignments(ctx, e2, ns, "hello", nil))
		if got, _ := s.AssignmentsForNest(ctx, n.ID); len(got) != 0 {
			t.Fatalf("assignments not cleared: %+v", got)
		}
	})
}

func TestInstancesReportReplacesNestSet(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		nest := uniq("nest")
		ns := uniq("ns")
		must(t, s.ReportInstances(ctx, nest, []Instance{
			{ID: uniq("i"), Namespace: ns, Deployment: "d", State: "ready"},
			{ID: uniq("i"), Namespace: ns, Deployment: "d", State: "busy", RunningTasks: 1},
		}))
		list, err := s.ListInstances(ctx, ns, "d", "")
		must(t, err)
		if len(list) != 2 {
			t.Fatalf("%+v", list)
		}
		keep := list[0]
		keep.State = "draining"
		must(t, s.ReportInstances(ctx, nest, []Instance{keep}))
		list, _ = s.ListInstances(ctx, ns, "", nest)
		if len(list) != 1 || list[0].State != "draining" {
			t.Fatalf("%+v", list)
		}
		must(t, s.ReportInstances(ctx, nest, nil))
		if list, _ = s.ListInstances(ctx, ns, "", ""); len(list) != 0 {
			t.Fatalf("%+v", list)
		}
	})
}

func TestTaskQueueLeasingAndFencing(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		ns := uniq("ns")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "1", IdempotencyKey: "k1"})
		must(t, err)
		again, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "1", IdempotencyKey: "k1"})
		must(t, err)
		if again.ID != a.ID {
			t.Fatal("idempotency key must return the same task")
		}
		for i := 0; i < 9; i++ {
			_, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x", RootRunID: "root", Depth: 1})
			must(t, err)
		}
		q, err := s.Queue(ctx, ns, "d")
		must(t, err)
		if q.Queued != 10 || q.InFlight != 0 {
			t.Fatalf("%+v", q)
		}

		// Concurrent leasing never hands one task to two nests.
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for w := 0; w < 5; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				got, err := s.LeaseTasks(ctx, "nest-"+itoa(w), ns, "d", 3, 60_000)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				for _, tk := range got {
					seen[tk.ID]++
				}
				mu.Unlock()
			}(w)
		}
		wg.Wait()
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("task %s leased %d times", id, n)
			}
		}
		if len(seen) != 10 {
			t.Fatalf("leased %d of 10", len(seen))
		}

		leased, err := s.ListTasks(ctx, ns, "d", "leased", 0)
		must(t, err)
		tk := leased[0]
		must(t, s.StartTask(ctx, tk.ID, tk.LeaseID, "inst-1"))
		must(t, s.ExtendLease(ctx, tk.ID, tk.LeaseID, 60_000))
		// The lease expires; the task is re-queued and re-leased elsewhere.
		old := NowMs
		NowMs = func() int64 { return old() + 120_000 }
		n, _, err := s.RequeueExpiredLeases(ctx, 0)
		NowMs = old
		must(t, err)
		// The sweep is global (a shared database may hold other tests' tasks);
		// check this namespace.
		q, err = s.Queue(ctx, ns, "d")
		must(t, err)
		if n < 10 || q.Queued != 10 || q.InFlight != 0 {
			t.Fatalf("requeued %d; queue %+v", n, q)
		}
		re, err := s.LeaseTasks(ctx, "nest-new", ns, "d", 10, 60_000)
		must(t, err)
		var fresh Task
		for _, r := range re {
			if r.ID == tk.ID {
				fresh = r
			}
		}
		if fresh.LeaseID == "" || fresh.LeaseID == tk.LeaseID || fresh.Attempts != 2 {
			t.Fatalf("re-lease: %+v", fresh)
		}
		// The partitioned old holder cannot complete or extend it.
		if err := s.CompleteTask(ctx, tk.ID, tk.LeaseID, true, "stale", "", "r", "i"); !errors.Is(err, ErrFenced) {
			t.Fatalf("stale completion: %v", err)
		}
		if err := s.ExtendLease(ctx, tk.ID, tk.LeaseID, 1000); !errors.Is(err, ErrFenced) {
			t.Fatalf("stale extend: %v", err)
		}
		must(t, s.CompleteTask(ctx, tk.ID, fresh.LeaseID, true, "done", "", "run-1", "inst-2"))
		done, _ := s.GetTask(ctx, tk.ID)
		if done.State != "succeeded" || done.Output != "done" || done.RunID != "run-1" || !done.Terminal() {
			t.Fatalf("%+v", done)
		}
		if err := s.CompleteTask(ctx, tk.ID, fresh.LeaseID, true, "twice", "", "", ""); !errors.Is(err, ErrFenced) {
			t.Fatal("a finished task cannot be completed again")
		}
		c, err := s.CancelTask(ctx, re[1].ID)
		must(t, err)
		if c.State != "cancelled" {
			t.Fatal(c.State)
		}
	})
}

func TestAttemptCapReleaseAndEnrolment(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		ns := uniq("ns")
		tk, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "boom"})
		must(t, err)
		old := NowMs
		defer func() { NowMs = old }()
		// Released before starting: back in the queue, attempt refunded.
		l, err := s.LeaseTasks(ctx, "n1", ns, "d", 1, 60_000)
		must(t, err)
		must(t, s.ReleaseTask(ctx, tk.ID, l[0].LeaseID, true))
		if err := s.ReleaseTask(ctx, tk.ID, l[0].LeaseID, true); !errors.Is(err, ErrFenced) {
			t.Fatalf("double release: %v", err)
		}
		got, _ := s.GetTask(ctx, tk.ID)
		if got.State != "queued" || got.Attempts != 0 {
			t.Fatalf("after release: %+v", got)
		}
		// Each lease that expires counts; at the cap the task fails.
		for i := 1; i <= 3; i++ {
			l, err := s.LeaseTasks(ctx, "n1", ns, "d", 1, 1_000)
			must(t, err)
			if len(l) != 1 || l[0].Attempts != i {
				t.Fatalf("lease %d: %+v", i, l)
			}
			base := old()
			NowMs = func() int64 { return base + 10_000 }
			_, failed, err := s.RequeueExpiredLeases(ctx, 3)
			NowMs = old
			must(t, err)
			if (i == 3) != (len(failed) == 1 && failed[0] == tk.ID) {
				t.Fatalf("lease %d failed %v", i, failed)
			}
		}
		got, _ = s.GetTask(ctx, tk.ID)
		if got.State != "failed" || got.Error == "" {
			t.Fatalf("after cap: %+v", got)
		}

		// Enrolment is atomic and single-use; the nest token carries its id.
		jt, err := s.CreateJoinToken(ctx, 60_000)
		must(t, err)
		n, secret, err := s.EnrollNest(ctx, jt, Nest{Name: "e", Backend: "native", Capacity: 2, Labels: map[string]string{"a": "b"}}, nil)
		must(t, err)
		if n.State != "active" || n.Labels["a"] != "b" || secret == "" {
			t.Fatalf("%+v", n)
		}
		tok, err := s.LookupAPIToken(ctx, secret)
		must(t, err)
		if tok.Name != "nest:"+n.ID || len(tok.Scopes) != 1 || tok.Scopes[0] != "nest" {
			t.Fatalf("%+v", tok)
		}
		if _, _, err := s.EnrollNest(ctx, jt, Nest{Name: "again"}, nil); !errors.Is(err, ErrForbidden) {
			t.Fatalf("reused join token: %v", err)
		}

		// A nest cannot overwrite another nest's instance row.
		id := uniq("i")
		must(t, s.ReportInstances(ctx, "nest-a", []Instance{{ID: id, Namespace: ns, Deployment: "d", State: "ready"}}))
		must(t, s.ReportInstances(ctx, "nest-b", []Instance{{ID: id, Namespace: ns, Deployment: "d", State: "failed"}}))
		list, _ := s.ListInstances(ctx, ns, "d", "")
		if len(list) != 1 || list[0].NestID != "nest-a" || list[0].State != "ready" {
			t.Fatalf("%+v", list)
		}
	})
}

func TestApprovals(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		ns := uniq("ns")
		a, err := s.CreateApproval(ctx, Approval{Namespace: ns, Deployment: "d", Tool: "pay", RequestedBy: "token:alice",
			Arguments: json.RawMessage(`{"to":"bob"}`)}, 60_000)
		must(t, err)
		if a.State != "pending" {
			t.Fatal(a.State)
		}
		if _, err := s.DecideApproval(ctx, a.ID, true, "token:alice"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("self-approval must be forbidden: %v", err)
		}
		d, err := s.DecideApproval(ctx, a.ID, true, "token:bob")
		must(t, err)
		if d.State != "approved" || d.DecidedBy != "token:bob" {
			t.Fatalf("%+v", d)
		}
		if _, err := s.DecideApproval(ctx, a.ID, false, "token:carol"); !errors.Is(err, ErrConflict) {
			t.Fatal("decided approvals are final")
		}
		dupNS := uniq("ns")
		// The same ask while one is pending (a concurrent request) returns
		// that one: one pending approval per run, tool and arguments.
		dupA, errA := s.CreateApproval(ctx, Approval{Namespace: dupNS, Deployment: "d", RunID: dupNS, Tool: "pay", Arguments: []byte(`{"n":1}`), RequestedBy: "x"}, 60_000)
		dupB, errB := s.CreateApproval(ctx, Approval{Namespace: dupNS, Deployment: "d", RunID: dupNS, Tool: "pay", Arguments: []byte(`{"n":1}`), RequestedBy: "x"}, 60_000)
		if errA != nil || errB != nil || dupA.ID != dupB.ID {
			t.Fatalf("duplicate pending approval: %v %v %s %s", errA, errB, dupA.ID, dupB.ID)
		}
		// Large arguments (a tool writing a file) still raise one ask.
		bigArgs, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", 12_000)})
		bigA, errA := s.CreateApproval(ctx, Approval{Namespace: dupNS, Deployment: "d", RunID: dupNS + "-big", Tool: "write", Arguments: bigArgs, RequestedBy: "x"}, 60_000)
		bigB, errB := s.CreateApproval(ctx, Approval{Namespace: dupNS, Deployment: "d", RunID: dupNS + "-big", Tool: "write", Arguments: bigArgs, RequestedBy: "x"}, 60_000)
		if errA != nil || errB != nil || bigA.ID != bigB.ID {
			t.Fatalf("large-argument approval: %v %v %s %s", errA, errB, bigA.ID, bigB.ID)
		}
		b, err := s.CreateApproval(ctx, Approval{Namespace: ns, Deployment: "d", Tool: "pay", RequestedBy: "x"}, 10)
		must(t, err)
		old := NowMs
		NowMs = func() int64 { return old() + 1000 }
		got, err := s.GetApproval(ctx, b.ID)
		NowMs = old
		must(t, err)
		if got.State != "expired" {
			t.Fatalf("not expired: %s", got.State)
		}
		pending, _ := s.ListApprovals(ctx, ns, "pending")
		if len(pending) != 0 {
			t.Fatalf("%+v", pending)
		}
	})
}

func TestTriggerEventsAndTokens(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		ns := uniq("ns")
		ok, err := s.RecordTriggerEvent(ctx, TriggerEvent{Namespace: ns, Deployment: "d", Trigger: "hourly", State: "fired", DueMs: 1000})
		must(t, err)
		dup, err := s.RecordTriggerEvent(ctx, TriggerEvent{Namespace: ns, Deployment: "d", Trigger: "hourly", State: "fired", DueMs: 1000})
		must(t, err)
		if !ok || dup {
			t.Fatalf("a firing must be recorded exactly once (%v, %v)", ok, dup)
		}
		_, _ = s.RecordTriggerEvent(ctx, TriggerEvent{Namespace: ns, Deployment: "d", Trigger: "hourly", State: "missed", DueMs: 2000})
		if last, _ := s.LastTriggerDue(ctx, ns, "d", "hourly"); last != 2000 {
			t.Fatal(last)
		}
		missed, _ := s.ListTriggerEvents(ctx, ns, "d", "missed", 0)
		if len(missed) != 1 {
			t.Fatalf("%+v", missed)
		}

		tok, secret, err := s.CreateAPIToken(ctx, uniq("ci"), []string{"operator"}, []string{ns}, 0, false)
		must(t, err)
		got, err := s.LookupAPIToken(ctx, secret)
		must(t, err)
		if got.ID != tok.ID || got.Scopes[0] != "operator" || got.Namespaces[0] != ns {
			t.Fatalf("%+v", got)
		}
		if _, err := s.LookupAPIToken(ctx, "wrong"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		must(t, s.RevokeAPIToken(ctx, tok.ID))
		if _, err := s.LookupAPIToken(ctx, secret); !errors.Is(err, ErrForbidden) {
			t.Fatal("revoked token must not work")
		}
		join, err := s.CreateJoinToken(ctx, 60_000)
		must(t, err)
		must(t, s.ConsumeJoinToken(ctx, join, "nest-1"))
		if err := s.ConsumeJoinToken(ctx, join, "nest-2"); !errors.Is(err, ErrForbidden) {
			t.Fatal("join tokens work once")
		}
	})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Migrations are split into statements on ";" (by this package and by the
// Rust engine), so a ";" in a comment would break them.
func TestMigrationCommentsHaveNoSemicolons(t *testing.T) {
	for _, dir := range []string{"migrations/sqlite", "migrations/postgres"} {
		entries, err := migrations.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, _ := migrations.ReadFile(dir + "/" + e.Name())
			for i, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "--") && strings.Contains(line, ";") {
					t.Errorf("%s/%s:%d: comment contains ';'", dir, e.Name(), i+1)
				}
			}
		}
	}
}

// Leader-only deployment writes (the autoscaler) are fenced by the leader
// lease epoch.
func TestFencedDeploymentUpdates(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		ns := uniq("ns")
		_, err := s.CreateDeployment(ctx, Deployment{Namespace: ns, Name: "d", DefinitionDigest: "sha256:x", Kind: "pool"})
		must(t, err)
		epoch, err := s.AcquireLease(ctx, s.LeaderLeaseName(), "leader-a", 60_000)
		must(t, err)
		d, err := s.UpdateDeploymentFenced(ctx, epoch, ns, "d", func(d *Deployment) error { d.Desired = 2; return nil })
		must(t, err)
		if d.Desired != 2 {
			t.Fatalf("%+v", d)
		}
		if _, err := s.UpdateDeploymentFenced(ctx, epoch+1, ns, "d", func(d *Deployment) error { d.Desired = 5; return nil }); !errors.Is(err, ErrFenced) {
			t.Fatalf("stale epoch: %v", err)
		}
		got, _ := s.GetDeployment(ctx, ns, "d")
		if got.Desired != 2 {
			t.Fatalf("fenced write applied: %+v", got)
		}
	})
}
