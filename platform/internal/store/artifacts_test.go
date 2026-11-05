package store

import (
	"context"
	"testing"
)

// Artifacts are written by the engine's save_artifact tool, not the Go
// store; these tests seed a row directly (as the engine would) and check
// GetArtifact/ListArtifacts read it back.
func TestArtifactsGetAndList(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("art")
		seed := func(id, name, taskID string) {
			_, err := s.db.ExecContext(ctx, "INSERT INTO artifacts (namespace, id, name, media_type, size, content, task_id, run_id, created_ms) VALUES ($1, $2, $3, 'text/plain', $4, $5, $6, 'r1', $7)",
				ns, id, name, len(name), name+" content", taskID, NowMs())
			must(t, err)
		}
		seed("sha256:a", "note-a", "t1")
		seed("sha256:b", "note-b", "t2")

		got, err := s.GetArtifact(ctx, ns, "sha256:a")
		must(t, err)
		if got.Name != "note-a" || got.Content != "note-a content" {
			t.Fatalf("get: %+v", got)
		}
		_, err = s.GetArtifact(ctx, ns, "sha256:missing")
		if err != ErrNotFound {
			t.Fatalf("missing: %v", err)
		}

		all, err := s.ListArtifacts(ctx, ns, "")
		must(t, err)
		if len(all) != 2 {
			t.Fatalf("list all: %d", len(all))
		}
		byTask, err := s.ListArtifacts(ctx, ns, "t1")
		must(t, err)
		if len(byTask) != 1 || byTask[0].ID != "sha256:a" {
			t.Fatalf("list by task: %+v", byTask)
		}
		other, err := s.ListArtifacts(ctx, uniq("none"), "")
		must(t, err)
		if len(other) != 0 {
			t.Fatalf("a different namespace sees nothing: %d", len(other))
		}
	})
}
