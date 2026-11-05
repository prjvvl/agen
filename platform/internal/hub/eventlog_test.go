package hub

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// ListEvents: what a token may see, filters, the cursor and waiting for the
// next event.
// features: events
func TestListEventsScopesFiltersAndWaits(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adm := e.client(admin)
	if _, err := adm.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	sub, err := adm.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Labels: map[string]string{"team": "a"}}))
	if err != nil {
		t.Fatal(err)
	}
	// An approval event, visible to approvers only.
	if err := e.store.AppendEvent(ctx, store.Event{Namespace: "default", Type: "approval.pending", Deployment: "hello", Severity: "warning"}); err != nil {
		t.Fatal(err)
	}
	all, err := adm.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	types := func(r *connect.Response[agenv1.ListEventsResponse]) []string {
		var out []string
		for _, ev := range r.Msg.Events {
			out = append(out, ev.Type)
		}
		return out
	}
	if got := strings.Join(types(all), ","); got != "deployment.created,task.queued,approval.pending" {
		t.Fatalf("admin sees %s", got)
	}
	q := all.Msg.Events[1]
	if q.TaskId != sub.Msg.Task.Id || q.Labels["team"] != "a" || q.Data.AsMap()["source"] != "api" || q.At == nil {
		t.Fatalf("task.queued: %v", q)
	}
	viewer, _ := adm.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "v", Scopes: []string{"viewer"}}))
	v := e.client(viewer.Msg.Secret)
	vr, err := v.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{}))
	if err != nil || strings.Join(types(vr), ",") != "deployment.created,task.queued" {
		t.Fatalf("viewer sees %v %v", types(vr), err)
	}
	// A token limited to another namespace sees nothing here, and may not ask.
	other, _ := adm.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "o", Scopes: []string{"viewer"}, Namespaces: []string{"team-b"}}))
	o := e.client(other.Msg.Secret)
	if r, err := o.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{})); err != nil || len(r.Msg.Events) != 0 {
		t.Fatalf("other namespace: %v %v", r, err)
	}
	if _, err := o.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{Namespace: "default"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("asking for a namespace it may not see: %v", err)
	}
	if _, err := adm.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{Severity: "loud"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad severity: %v", err)
	}
	// Filters.
	r, _ := adm.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{Types: []string{"task."}}))
	if strings.Join(types(r), ",") != "task.queued" {
		t.Fatalf("type prefix: %v", types(r))
	}
	r, _ = adm.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{Severity: "warning"}))
	if strings.Join(types(r), ",") != "approval.pending" {
		t.Fatalf("severity: %v", types(r))
	}
	// Waiting: returns as soon as a matching event arrives.
	cursor := all.Msg.NextSeq
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = adm.CancelTask(context.Background(), connect.NewRequest(&agenv1.CancelTaskRequest{Id: sub.Msg.Task.Id}))
	}()
	start := time.Now()
	w, err := adm.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{AfterSeq: cursor, Types: []string{"task.finished"}, WaitSeconds: 20}))
	if err != nil || len(w.Msg.Events) != 1 || w.Msg.Events[0].Data.AsMap()["state"] != "cancelled" || time.Since(start) > 10*time.Second {
		t.Fatalf("wait: %v %v after %s", w, err, time.Since(start))
	}
	// Nothing new: a short wait returns empty with the cursor kept.
	w2, err := adm.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{AfterSeq: w.Msg.NextSeq, WaitSeconds: 1}))
	if err != nil || len(w2.Msg.Events) != 0 || w2.Msg.NextSeq != w.Msg.NextSeq {
		t.Fatalf("empty wait: %v %v", w2, err)
	}
}

// The event stream carries the event log with ids, resumes from
// Last-Event-ID, and filters by type.
// features: events
func TestEventStreamResumesFromTheLastEventID(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adm := e.client(admin)
	if _, err := adm.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	path, h := e.hub.EventsHandler()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	open := func(query, lastID string) (*bufio.Scanner, func()) {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/events?kinds=log"+query, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("stream: %v %v", resp, err)
		}
		return bufio.NewScanner(resp.Body), func() { resp.Body.Close() }
	}
	// next returns the id and type of the next log event.
	next := func(sc *bufio.Scanner) (string, string) {
		t.Helper()
		ch := make(chan [2]string, 1)
		go func() {
			var id, typ string
			for sc.Scan() {
				l := sc.Text()
				switch {
				case strings.HasPrefix(l, "id: "):
					id = strings.TrimPrefix(l, "id: ")
				case strings.HasPrefix(l, "event: ") && id != "":
					typ = strings.TrimPrefix(l, "event: ")
				case strings.HasPrefix(l, "data: ") && typ != "":
					ch <- [2]string{id, typ}
					return
				}
			}
			ch <- [2]string{"", ""}
		}()
		select {
		case r := <-ch:
			return r[0], r[1]
		case <-time.After(10 * time.Second):
			t.Fatal("no event on the stream")
			return "", ""
		}
	}
	// Without a cursor the stream starts at the newest event.
	sc, closeA := open("&type=task.", "")
	time.Sleep(100 * time.Millisecond)
	for range 2 {
		if _, err := adm.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"})); err != nil {
			t.Fatal(err)
		}
	}
	id1, typ := next(sc)
	if typ != "task.queued" {
		t.Fatalf("first: %s", typ)
	}
	closeA()
	// Reconnect with Last-Event-ID: the second event is not lost.
	sc, closeB := open("&type=task.", id1)
	defer closeB()
	id2, typ := next(sc)
	n1, _ := strconv.Atoi(id1)
	n2, _ := strconv.Atoi(id2)
	if typ != "task.queued" || n2 <= n1 {
		t.Fatalf("resumed: %s %s after %s", typ, id2, id1)
	}
}

// Activity warnings become activity.warning events (once a day each), a
// quiet run also task.stuck, config.json alerts sets the thresholds, and
// answers cut at the output limit are a warning.
// features: events
func TestWarningsBecomeEvents(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	files := helloFiles(t)
	files["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":1},"alerts":{"quietRun":"1m"}}`)
	d, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: files}))
	if err != nil {
		t.Fatal(err)
	}
	task, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	// A run quiet for 2 minutes: past its 1 minute alert (the default is 5).
	then := store.NowMs() - 2*60_000
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','x','default','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, definition_digest, status, input, trace_id, started_ms, task_id) VALUES ('r1','s1','c1','default','hello','" +
			d.Msg.Deployment.DefinitionDigest + "','running','x','t1',$1,'" + task.Msg.Task.Id + "')",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, then); err != nil {
			t.Fatal(err)
		}
	}
	// Three answers asked again after hitting the output limit (another run).
	if _, err := e.store.DB().ExecContext(ctx, "INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms) VALUES ('r2','s1','c1','default','hello','succeeded','x','t2',$1,$1)", store.NowMs()); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := e.store.DB().ExecContext(ctx, `INSERT INTO spans (span_id, trace_id, run_id, name, start_ms, end_ms, status, attributes, seq) VALUES ($1,'t2','r2','gen_ai.chat',$2,$2,'ok','{"agen.output.retried_with":2048}',$3)`,
			"sp"+strconv.Itoa(i), store.NowMs(), i); err != nil {
			t.Fatal(err)
		}
	}
	e.hub.recordWarnings(ctx)
	e.hub.recordWarnings(ctx)
	r, err := c.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{Types: []string{"activity.warning", "task.stuck"}}))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, ev := range r.Msg.Events {
		k := ev.Type
		if ev.Type == "activity.warning" {
			k += ":" + ev.Data.AsMap()["kind"].(string)
		}
		kinds[k]++
		if ev.Type == "task.stuck" && ev.TaskId != task.Msg.Task.Id {
			t.Fatalf("task.stuck names %q", ev.TaskId)
		}
	}
	if kinds["activity.warning:quiet_run"] != 1 || kinds["task.stuck"] != 1 || kinds["activity.warning:output_cut"] != 1 {
		t.Fatalf("warning events: %v", kinds)
	}
}
