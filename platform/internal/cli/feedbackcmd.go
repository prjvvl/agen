package cli

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdFeedback records or lists feedback on tasks.
func (e *env) cmdFeedback(ctx context.Context, args []string) error {
	const usage = "agen feedback record <task> --score S [--note TEXT] [--label k=v] | agen feedback ls [name] [--label k=v] [--since D]"
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageErr(usage)
	}
	var cf clientFlags
	fs := e.flags("feedback "+args[0], &cf)
	switch args[0] {
	case "record":
		score := fs.Float64("score", -1, "0.0 (worst) to 1.0 (best)")
		note := fs.String("note", "", "why")
		labels := labelFlag{}
		fs.Var(labels, "label", "label the feedback, key=value (repeatable)")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageErr(usage)
		}
		if *score < 0 || *score > 1 {
			return usageErr("--score is required and must be between 0.0 and 1.0")
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		r, err := c.RecordFeedback(ctx, connect.NewRequest(&agenv1.RecordFeedbackRequest{TaskId: pos[0], Score: *score, Note: *note, Labels: labels}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		fmt.Fprintf(e.stdout, "recorded feedback %s on task %s: %s\n", r.Msg.Feedback.Id, pos[0], strconv.FormatFloat(r.Msg.Feedback.Score, 'f', -1, 64))
		return nil
	case "ls":
		labels := labelFlag{}
		fs.Var(labels, "label", "only feedback with this key=value label (repeatable)")
		since := fs.Duration("since", 0, "only feedback recorded within this long (e.g. 24h)")
		limit := fs.Int("limit", 50, "feedback")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) > 1 {
			return usageErr(usage)
		}
		dep := ""
		if len(pos) == 1 {
			dep = pos[0]
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		req := &agenv1.ListFeedbackRequest{Namespace: cf.namespace, Deployment: dep, Labels: labels, Limit: int32(*limit)}
		if *since > 0 {
			req.Since = timestamppb.New(time.Now().Add(-*since))
		}
		r, err := c.ListFeedback(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		w := e.table()
		fmt.Fprintln(w, "ID\tDEPLOYMENT\tTASK\tSCORE\tBY\tCREATED\tNOTE")
		for _, f := range r.Msg.Feedback {
			fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\n", f.Id, f.Namespace, f.Deployment, f.TaskId,
				strconv.FormatFloat(f.Score, 'f', 2, 64), f.By, f.CreatedAt.AsTime().Local().Format(time.RFC3339), oneLine(f.Note, 60))
		}
		return w.Flush()
	default:
		return usageErr(usage)
	}
}
