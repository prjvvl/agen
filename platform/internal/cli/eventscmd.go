package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdEvents lists the event log, or follows it with -f.
func (e *env) cmdEvents(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("events", &cf)
	var types listFlag
	fs.Var(&types, "type", `only this type (repeatable; "task." matches every task event)`)
	labels := labelFlag{}
	fs.Var(labels, "label", "only events with this key=value label (repeatable)")
	severity := fs.String("severity", "", "only events at least this severe: info, warning or critical")
	after := fs.Int64("after", 0, "only events after this seq")
	limit := fs.Int("limit", 50, "events")
	follow := fs.Bool("f", false, "keep printing new events")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("agen events [name] [--type T] [--label k=v] [--severity S] [--after SEQ] [-f]")
	}
	dep := ""
	if len(pos) == 1 {
		dep = pos[0]
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	req := &agenv1.ListEventsRequest{Namespace: cf.namespace, Deployment: dep, Types: types, Labels: labels, Severity: *severity,
		AfterSeq: *after, Limit: int32(*limit)}
	if *follow && *after == 0 {
		// Follow from now: skip what is already there.
		req.Limit = 1000
		for {
			r, err := c.ListEvents(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			req.AfterSeq = r.Msg.NextSeq
			if len(r.Msg.Events) < int(req.Limit) {
				break
			}
		}
		req.Limit = int32(*limit)
	}
	if !*follow {
		r, err := c.ListEvents(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		w := e.table()
		fmt.Fprintln(w, "SEQ\tTIME\tTYPE\tDEPLOYMENT\tTASK\tDETAILS")
		for _, ev := range r.Msg.Events {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", ev.Seq, ev.At.AsTime().Local().Format(time.RFC3339), ev.Type, ev.Deployment, ev.TaskId, eventDetails(ev))
		}
		return w.Flush()
	}
	for {
		req.WaitSeconds = 30
		r, err := c.ListEvents(ctx, connect.NewRequest(req))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		req.AfterSeq = r.Msg.NextSeq
		for _, ev := range r.Msg.Events {
			if cf.json {
				b, _ := protojson.Marshal(ev)
				fmt.Fprintln(e.stdout, string(b))
				continue
			}
			fmt.Fprintf(e.stdout, "%d %s %s %s %s %s\n", ev.Seq, ev.At.AsTime().Local().Format(time.TimeOnly), ev.Type, ev.Deployment, ev.TaskId, eventDetails(ev))
		}
	}
}

// eventDetails is a short line from an event's data.
func eventDetails(ev *agenv1.Event) string {
	d := ev.GetData().AsMap()
	var parts []string
	if o, ok := d["outcome"].(map[string]any); ok {
		s := fmt.Sprint(o["status"])
		if r, ok := o["reason"]; ok {
			s += " (" + fmt.Sprint(r) + ")"
		}
		parts = append(parts, "outcome "+s)
	} else if st, ok := d["state"]; ok {
		parts = append(parts, fmt.Sprint(st))
	}
	for _, k := range []string{"tool", "account", "kind", "trigger", "percent", "message", "error", "summary"} {
		if v, ok := d[k]; ok && v != "" {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return oneLine(strings.Join(parts, " "), 100)
}
