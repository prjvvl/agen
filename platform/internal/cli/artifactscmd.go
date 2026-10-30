package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdArtifacts: agen artifacts get|ls — named, content-addressed text an
// agent saved during a run with the save_artifact tool.
func (e *env) cmdArtifacts(ctx context.Context, args []string) error {
	const usage = "agen artifacts get <id> | agen artifacts ls [task]"
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageErr(usage)
	}
	var cf clientFlags
	fs := e.flags("artifacts "+args[0], &cf)
	switch args[0] {
	case "get":
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageErr(usage)
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		r, err := c.GetArtifact(ctx, connect.NewRequest(&agenv1.GetArtifactRequest{Namespace: cf.namespace, Id: pos[0]}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		fmt.Fprint(e.stdout, r.Msg.Artifact.Content)
		return nil
	case "ls":
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) > 1 {
			return usageErr(usage)
		}
		taskID := ""
		if len(pos) == 1 {
			taskID = pos[0]
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		r, err := c.ListArtifacts(ctx, connect.NewRequest(&agenv1.ListArtifactsRequest{Namespace: cf.namespace, TaskId: taskID}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		w := e.table()
		fmt.Fprintln(w, "ID\tNAMESPACE\tNAME\tMEDIA TYPE\tSIZE\tTASK\tCREATED")
		for _, a := range r.Msg.Artifacts {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", a.Id, a.Namespace, a.Name, a.MediaType, a.Size, a.TaskId, a.CreatedAt.AsTime().Local().Format(time.RFC3339))
		}
		return w.Flush()
	default:
		return usageErr(usage)
	}
}
