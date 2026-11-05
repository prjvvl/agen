package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// GetArtifact validates its id and reads an artifact back (one the engine's
// save_artifact tool would have written); ListArtifacts filters by task.
// features: artifacts
func TestGetAndListArtifacts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)

	if _, err := c.GetArtifact(ctx, connect.NewRequest(&agenv1.GetArtifactRequest{})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("no id accepted: %v", err)
	}
	if _, err := c.GetArtifact(ctx, connect.NewRequest(&agenv1.GetArtifactRequest{Id: "sha256:none"})); code(err) != connect.CodeNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	seed := func(id, name, taskID string) {
		_, err := e.store.DB().ExecContext(ctx, "INSERT INTO artifacts (namespace, id, name, media_type, size, content, task_id, run_id, created_ms) VALUES ('default', $1, $2, 'text/plain', $3, $4, $5, 'r1', $6)",
			id, name, len(name), name+" content", taskID, store.NowMs())
		if err != nil {
			t.Fatal(err)
		}
	}
	seed("sha256:a", "note-a", "t1")
	seed("sha256:b", "note-b", "t2")

	got, err := c.GetArtifact(ctx, connect.NewRequest(&agenv1.GetArtifactRequest{Id: "sha256:a"}))
	if err != nil || got.Msg.Artifact.Name != "note-a" || got.Msg.Artifact.Content != "note-a content" {
		t.Fatalf("get: %v %v", got, err)
	}
	all, err := c.ListArtifacts(ctx, connect.NewRequest(&agenv1.ListArtifactsRequest{}))
	if err != nil || len(all.Msg.Artifacts) != 2 {
		t.Fatalf("list all: %v %v", all, err)
	}
	byTask, err := c.ListArtifacts(ctx, connect.NewRequest(&agenv1.ListArtifactsRequest{TaskId: "t1"}))
	if err != nil || len(byTask.Msg.Artifacts) != 1 || byTask.Msg.Artifacts[0].Id != "sha256:a" {
		t.Fatalf("list by task: %v %v", byTask, err)
	}

	// A viewer token may read artifacts.
	viewer, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "v", Scopes: []string{ScopeViewer}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(viewer.Msg.Secret).ListArtifacts(ctx, connect.NewRequest(&agenv1.ListArtifactsRequest{})); err != nil {
		t.Fatalf("viewer list: %v", err)
	}
}
