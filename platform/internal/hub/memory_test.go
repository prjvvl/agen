package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// SetMemoryBlock validates its arguments and stores a block (creating it
// on first write, replacing its value on later ones); ListMemoryBlocks
// reads a namespace's blocks back, newest-updated first.
// features: memory.blocks
func TestSetAndListMemoryBlocks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)

	if _, err := c.SetMemoryBlock(ctx, connect.NewRequest(&agenv1.SetMemoryBlockRequest{Value: "x"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("no id accepted: %v", err)
	}
	if _, err := c.SetMemoryBlock(ctx, connect.NewRequest(&agenv1.SetMemoryBlockRequest{Id: "notes", Value: "a long value", LimitChars: 3})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("value over limit_chars accepted: %v", err)
	}
	r, err := c.SetMemoryBlock(ctx, connect.NewRequest(&agenv1.SetMemoryBlockRequest{Id: "notes", Label: "Notes", Description: "scratch", Value: "hello"}))
	if err != nil || r.Msg.Block.Version != 1 || r.Msg.Block.Value != "hello" {
		t.Fatalf("set: %v %v", r, err)
	}
	r2, err := c.SetMemoryBlock(ctx, connect.NewRequest(&agenv1.SetMemoryBlockRequest{Id: "notes", Value: "world"}))
	if err != nil || r2.Msg.Block.Version != 2 || r2.Msg.Block.Value != "world" {
		t.Fatalf("replace: %v %v", r2, err)
	}
	if _, err := c.SetMemoryBlock(ctx, connect.NewRequest(&agenv1.SetMemoryBlockRequest{Id: "other", Value: "y"})); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListMemoryBlocks(ctx, connect.NewRequest(&agenv1.ListMemoryBlocksRequest{}))
	if err != nil || len(list.Msg.Blocks) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	ids := map[string]bool{}
	for _, b := range list.Msg.Blocks {
		ids[b.Id] = true
	}
	if !ids["notes"] || !ids["other"] {
		t.Fatalf("list: %v", list.Msg.Blocks)
	}

	// A viewer token may list but not set.
	viewer, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "v", Scopes: []string{ScopeViewer}}))
	if err != nil {
		t.Fatal(err)
	}
	vc := e.client(viewer.Msg.Secret)
	if _, err := vc.ListMemoryBlocks(ctx, connect.NewRequest(&agenv1.ListMemoryBlocksRequest{})); err != nil {
		t.Fatalf("viewer list: %v", err)
	}
	if _, err := vc.SetMemoryBlock(ctx, connect.NewRequest(&agenv1.SetMemoryBlockRequest{Id: "notes", Value: "nope"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer set: %v", err)
	}
}
