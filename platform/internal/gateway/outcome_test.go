package gateway

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/testutil"
)

// A run's outcome travels over A2A: needs_input and rejected become the A2A
// states input-required and rejected, the outcome is in the task metadata,
// finish data is a data part, and a calling agent sees the status, report
// and data in its tool result.
// features: engine.outcome
func TestA2ACarriesRunOutcomes(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	asker := testutil.BundleFiles(t, "hello")
	asker["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[
		{"toolCalls":[{"name":"finish","arguments":{"status":"needs_input","summary":"Which repo?"}}]}]}`)
	halfway := testutil.BundleFiles(t, "hello")
	halfway["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[
		{"toolCalls":[{"name":"finish","arguments":{"status":"partial","summary":"Half done.","data":{"done":2}}}]}]}`)
	refuser := testutil.BundleFiles(t, "hello")
	refuser["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[
		{"toolCalls":[{"name":"finish","arguments":{"status":"rejected","summary":"Not my job."}}]}]}`)
	boss := testutil.BundleFiles(t, "hello")
	boss["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":2},
		"delegates":[{"name":"asker"},{"name":"halfway"}],
		"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"allow"}]}}`)
	boss["x-agen/fake-script.json"] = []byte(`{"perRun":true,"responses":[
		{"toolCalls":[{"name":"call_agent","arguments":{"agent":"asker","message":"fix it"}}]},
		{"expect":"[needs_input] Which repo?","toolCalls":[{"name":"call_agent","arguments":{"agent":"halfway","message":"do it"}}]},
		{"text":"Boss: done","expect":"[partial] Half done.\ndata: {\"done\":2}"}]}`)
	k.deploy(t, "asker", asker, 1)
	k.deploy(t, "halfway", halfway, 1)
	k.deploy(t, "refuser", refuser, 1)
	k.deploy(t, "boss", boss, 1)
	k.waitReady(t, 4)

	_, res := sendA2A(t, k.gwURL+"/a2a/default/asker", "m-ask")
	status, _ := res["status"].(map[string]any)
	meta, _ := res["metadata"].(map[string]any)
	outcome, _ := meta["agen.outcome"].(map[string]any)
	if status["state"] != "input-required" || outcome["status"] != "needs_input" || outcome["inputFrom"] != "caller" {
		t.Fatalf("asker: %v", res)
	}
	_, res = sendA2A(t, k.gwURL+"/a2a/default/refuser", "m-refuse")
	if status, _ := res["status"].(map[string]any); status["state"] != "rejected" {
		t.Fatalf("refuser: %v", res)
	}
	_, res = sendA2A(t, k.gwURL+"/a2a/default/halfway", "m-half")
	status, _ = res["status"].(map[string]any)
	arts, _ := res["artifacts"].([]any)
	if status["state"] != "completed" || len(arts) != 2 {
		t.Fatalf("halfway: %v", res)
	}
	data, _ := arts[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["data"].(map[string]any)
	if data["done"] != float64(2) {
		t.Fatalf("halfway data part: %v", arts)
	}

	// The fake script's expectations check the tool results boss's model saw.
	sub, err := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "boss"}, Input: "go"}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id, WaitSeconds: 30}))
	if err != nil || got.Msg.Task.Output != "Boss: done" || got.Msg.Task.Outcome.GetStatus() != "done" {
		t.Fatalf("boss task: %v %v", got, err)
	}
}
