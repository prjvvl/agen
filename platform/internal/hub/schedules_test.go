package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// What the console's Schedules view needs: when each cron trigger fires
// next and last fired, the tasks waiting for their start time (soonest
// first), trigger events across deployments, and warnings for missed
// windows and refused webhook calls.
// features: tasks.schedule, triggers
func TestSchedulesData(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "nightly",
		BundleFiles: withTriggers(t, `[{"type":"cron","name":"night","schedule":"0 3 * * *","input":"run"},{"type":"webhook","name":"hook"}]`)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RecordTriggerEvent(ctx, store.TriggerEvent{Namespace: "default", Deployment: "nightly", Trigger: "night", State: "missed",
		DueMs: store.NowMs() - 3600_000, Message: "the Hub was not running at the due time"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RecordTriggerEvent(ctx, store.TriggerEvent{Namespace: "default", Deployment: "nightly", Trigger: "hook", State: "rejected",
		DueMs: store.NowMs(), Message: "bad signature"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RecordTriggerEvent(ctx, store.TriggerEvent{Namespace: "default", Deployment: "hello", Trigger: "x", State: "fired",
		DueMs: store.NowMs() - 60_000}); err != nil {
		t.Fatal(err)
	}

	d, err := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "nightly")}))
	if err != nil {
		t.Fatal(err)
	}
	var cronT *agenv1.Trigger
	for _, tr := range d.Msg.Deployment.Triggers {
		if tr.Name == "night" {
			cronT = tr
		}
	}
	next := cronT.GetNextDueAt().AsTime()
	if !next.After(time.Now()) || next.Sub(time.Now()) > 24*time.Hour || next.UTC().Hour() != 3 {
		t.Fatalf("next due: %v", next)
	}
	if cronT.GetLastDueAt() == nil {
		t.Fatal("last due not set after a missed window")
	}

	for _, in := range []time.Duration{2 * time.Hour, time.Hour} {
		if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "later " + in.String(),
			NotBefore: timestamppb.New(time.Now().Add(in))})); err != nil {
			t.Fatal(err)
		}
	}
	c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "now"}))
	sch, err := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Scheduled: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(sch.Msg.Tasks) != 2 || sch.Msg.Tasks[0].Input != "later 1h0m0s" {
		t.Fatalf("scheduled tasks: %v", sch.Msg.Tasks)
	}

	all, err := c.ListTriggerEvents(ctx, connect.NewRequest(&agenv1.ListTriggerEventsRequest{}))
	if err != nil || len(all.Msg.Events) != 3 {
		t.Fatalf("events of every deployment: %v %v", all, err)
	}

	act, err := c.GetActivity(ctx, connect.NewRequest(&agenv1.GetActivityRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range act.Msg.Warnings {
		got[w.Kind] = w.Message
	}
	if !strings.Contains(got["trigger_missed"], `"night" missed 1 window`) || !strings.Contains(got["webhook_rejected"], "bad signature") {
		t.Fatalf("warnings: %v", got)
	}
}
