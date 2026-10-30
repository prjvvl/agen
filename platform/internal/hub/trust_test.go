package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// CreateApiToken validates and stores trust; WhoAmI reports it back.
// features: access.tokens
func TestCreateApiTokenValidatesAndStoresTrust(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "bad", Scopes: []string{ScopeOperator}, Trust: "whenever"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid trust accepted: %v", err)
	}
	r, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "untrusted-bot", Scopes: []string{ScopeOperator}, Trust: "untrusted"}))
	if err != nil || r.Msg.Token.Trust != "untrusted" {
		t.Fatalf("create: %v %v", r, err)
	}
	who, err := e.client(r.Msg.Secret).WhoAmI(ctx, connect.NewRequest(&agenv1.WhoAmIRequest{}))
	if err != nil || who.Msg.Trust != "untrusted" {
		t.Fatalf("whoami: %v %v", who, err)
	}
	// Unset defaults to trusted.
	plain, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "plain-bot", Scopes: []string{ScopeOperator}}))
	if err != nil {
		t.Fatal(err)
	}
	who2, err := e.client(plain.Msg.Secret).WhoAmI(ctx, connect.NewRequest(&agenv1.WhoAmIRequest{}))
	if err != nil || who2.Msg.Trust != "trusted" {
		t.Fatalf("default trust: %v %v", who2, err)
	}
}

// SubmitTask resolves a task's trust from its token, and a caller may ask
// for a lower trust than its own but never a higher one.
// features: tasks.run
func TestSubmitTaskResolvesAndNarrowsTrust(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Trust: "whenever"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid trust accepted: %v", err)
	}
	// The admin bearer token is "operator" trust.
	asOperator, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil || asOperator.Msg.Task.Trust != "operator" {
		t.Fatalf("admin task trust: %v %v", asOperator, err)
	}
	// A caller may lower it.
	lowered, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Trust: "untrusted"}))
	if err != nil || lowered.Msg.Task.Trust != "untrusted" {
		t.Fatalf("lowered task trust: %v %v", lowered, err)
	}
	// A "trusted" token asking for "operator" does not raise it.
	tok, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "u", Scopes: []string{ScopeOperator}}))
	if err != nil {
		t.Fatal(err)
	}
	notRaised, err := e.client(tok.Msg.Secret).SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Trust: "operator"}))
	if err != nil || notRaised.Msg.Task.Trust != "trusted" {
		t.Fatalf("a token cannot raise its own trust: %v %v", notRaised, err)
	}
}

// SubmitChildTask takes the trust the caller (the engine) reports, but
// never raises it above the parent task's own stored trust.
// features: delegation.async
func TestSubmitChildTaskNeverRaisesTrustAboveTheParents(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idB, b := e.enroll(t, "nest-b", 4, nil)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "worker", BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "worker"), Desired: 1}))
	e.scheduler().Step(ctx)
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "parent", Trust: "untrusted"})); err != nil {
		t.Fatal(err)
	}
	pl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(pl.Msg.Tasks) != 1 {
		t.Fatalf("lease parent: %v %v", pl, err)
	}
	parent := pl.Msg.Tasks[0]
	if parent.Trust != "untrusted" {
		t.Fatalf("parent trust: %v", parent.Trust)
	}
	// The engine (mistakenly, or an old binary) reports "operator": capped
	// at the parent's own "untrusted".
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: parent.Id, Trust: "operator"}))
	if err != nil || ch.Msg.Task.Trust != "untrusted" {
		t.Fatalf("child trust must not exceed the parent's: %v %v", ch, err)
	}
	// An empty trust (an older caller) is capped the same way.
	ch2, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child2",
		ParentTaskId: parent.Id}))
	if err != nil || ch2.Msg.Task.Trust != "untrusted" {
		t.Fatalf("child trust (unset) must not exceed the parent's: %v %v", ch2, err)
	}
}
