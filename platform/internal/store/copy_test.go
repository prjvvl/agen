package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freshPostgres creates an empty database on the test server and returns its
// URL (dropped when the test ends).
func freshPostgres(t *testing.T) string {
	t.Helper()
	base := os.Getenv("AGEN_TEST_POSTGRES_URL")
	if base == "" {
		return ""
	}
	admin, err := Open(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	name := "agen_copy_" + strings.ToLower(NewID())
	if _, err := admin.DB().Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.DB().Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	i := strings.LastIndex(base, "/")
	url := base[:i+1] + name
	if q := strings.Index(base[i:], "?"); q >= 0 {
		url += base[i+q:]
	}
	return url
}

func TestCopyToMovesEverythingDurable(t *testing.T) {
	ctx := context.Background()
	src, err := Open(ctx, "sqlite:"+filepath.ToSlash(filepath.Join(t.TempDir(), "src.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	// Platform state.
	must(t, src.PutDefinition(ctx, Definition{Digest: "sha256:d1", Name: "hello", Files: map[string][]byte{"plugin.json": []byte(`{"name":"hello"}`)}}))
	_, err = src.CreateDeployment(ctx, Deployment{Namespace: "default", Name: "hello", DefinitionDigest: "sha256:d1", Kind: "pool"})
	must(t, err)
	task, err := src.SubmitTask(ctx, Task{Namespace: "default", Deployment: "hello", Input: "hi", SubmittedBy: "admin"})
	must(t, err)
	_, secret, err := src.CreateAPIToken(ctx, "ops", []string{"operator"}, nil, 0)
	must(t, err)
	_, err = src.CreateApproval(ctx, Approval{Namespace: "default", Deployment: "hello", RunID: "r1", Tool: "pay", Arguments: []byte(`{}`), RequestedBy: "admin"}, 60_000)
	must(t, err)
	// Engine run data (as agen-host writes it).
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms, singleton) VALUES ('s1','hello','default','hello','{}',1,1,1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',1)",
		"INSERT INTO messages (conversation_id, seq, run_id, body, created_ms) VALUES ('c1',0,'r1','{\"role\":\"user\",\"content\":\"hi\"}',1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, output, trace_id, started_ms, ended_ms, task_id, owner, epoch) VALUES ('r1','s1','c1','default','hello','succeeded','hi','Hello!','t1',1,2,'" + task.ID + "','i1',1)",
		"INSERT INTO spans (span_id, trace_id, parent_span_id, run_id, name, start_ms, end_ms, status, attributes, seq) VALUES ('a','t1','','r1','agen.run',1,2,'ok','{}',0)",
		"INSERT INTO logs (instance_id, namespace, deployment, time_ms, level, message) VALUES ('i1','default','hello',1,'info','run r1 started')",
	} {
		if _, err := src.DB().ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	targets := map[string]string{"sqlite": "sqlite:" + filepath.ToSlash(filepath.Join(t.TempDir(), "dst.db"))}
	if pg := freshPostgres(t); pg != "" {
		targets["postgres"] = pg
	} else if os.Getenv("AGEN_REQUIRE_PG") == "1" {
		t.Fatal("AGEN_REQUIRE_PG=1 but AGEN_TEST_POSTGRES_URL is unset")
	}
	for name, url := range targets {
		t.Run(name, func(t *testing.T) {
			dst, err := Open(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			rep, err := src.CopyTo(ctx, dst)
			if err != nil {
				t.Fatal(err)
			}
			for tbl, want := range map[string]int64{"definitions": 1, "deployments": 1, "tasks": 1, "api_tokens": 1, "approvals": 1, "sessions": 1, "messages": 1, "runs": 1, "spans": 1, "logs": 1} {
				if rep[tbl] != want {
					t.Errorf("%s: copied %d, want %d", tbl, rep[tbl], want)
				}
			}
			// Everything reads back through the normal APIs.
			d, err := dst.GetDeployment(ctx, "default", "hello")
			if err != nil || d.DefinitionDigest != "sha256:d1" {
				t.Fatalf("deployment: %+v %v", d, err)
			}
			def, err := dst.GetDefinition(ctx, "sha256:d1")
			if err != nil || string(def.Files["plugin.json"]) != `{"name":"hello"}` {
				t.Fatalf("definition: %v", err)
			}
			if tk, err := dst.GetTask(ctx, task.ID); err != nil || tk.SubmittedBy != "admin" {
				t.Fatalf("task: %+v %v", tk, err)
			}
			if _, err := dst.LookupAPIToken(ctx, secret); err != nil {
				t.Fatalf("token does not work after migration: %v", err)
			}
			spans, runs, err := dst.Trace(ctx, "t1")
			if err != nil || len(spans) != 1 || len(runs) != 1 || runs[0].Output != "Hello!" || runs[0].TaskID != task.ID {
				t.Fatalf("trace: %v %v %v", spans, runs, err)
			}
			// New rows keep working (the log id sequence moved past copied ids).
			if _, err := dst.DB().ExecContext(ctx, "INSERT INTO logs (instance_id, namespace, deployment, time_ms, level, message) VALUES ('i2','default','hello',3,'info','after')"); err != nil {
				t.Fatalf("insert after copy: %v", err)
			}
			// A second copy into the now non-empty target is refused.
			if _, err := src.CopyTo(ctx, dst); err == nil || !strings.Contains(err.Error(), "not empty") {
				t.Fatalf("copy into non-empty target: %v", err)
			}
		})
	}
}
