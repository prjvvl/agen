package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdRuns lists runs: every run, or with --roots one per trace with the
// usage of its whole run tree.
func (e *env) cmdRuns(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("runs", &cf)
	limit := fs.Int("limit", 20, "runs")
	status := fs.String("status", "", "running, waiting_approval, succeeded, failed or cancelled")
	roots := fs.Bool("roots", false, "only runs that started a trace, with the usage of the runs they delegated to")
	labels := labelFlag{}
	fs.Var(labels, "label", "only runs with this key=value label (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("agen runs [name] [--status S] [--roots] [--label k=v]")
	}
	dep := ""
	if len(pos) == 1 {
		dep = pos[0]
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Namespace: cf.namespace, Deployment: dep, Limit: int32(*limit),
		Status: *status, RootsOnly: *roots, Labels: labels}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	w := e.table()
	fmt.Fprintln(w, "RUN\tDEPLOYMENT\tSTATUS\tSTARTED\tDURATION\tTOKENS\tCOST\tTRACE")
	for _, run := range r.Msg.Runs {
		u := run.Usage
		if run.TreeUsage != nil {
			u = run.TreeUsage
		}
		dur := "-"
		if run.EndedAt != nil {
			dur = run.EndedAt.AsTime().Sub(run.StartedAt.AsTime()).Round(time.Millisecond).String()
		}
		fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%s\t%d\t$%.4f\t%s\n", run.Id, run.Namespace, run.Deployment, run.Status,
			run.StartedAt.AsTime().Local().Format(time.RFC3339), dur, u.GetInputTokens()+u.GetOutputTokens(), u.GetCostUsd(), run.TraceId)
	}
	return w.Flush()
}

// cmdTranscript prints what a run sent and received: by run id, or by the
// id of a task (its run).
func (e *env) cmdTranscript(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("transcript", &cf)
	history := fs.Bool("history", false, "also the earlier messages of the conversation")
	system := fs.Bool("system", false, "also the system prompt")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen transcript <run-id|task-id> [--history] [--system]")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	runID := pos[0]
	if tk, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: runID})); err == nil {
		if tk.Msg.Task.RunId == "" {
			return fmt.Errorf("task %s has no run yet", runID)
		}
		runID = tk.Msg.Task.RunId
	}
	r, err := c.GetTranscript(ctx, connect.NewRequest(&agenv1.GetTranscriptRequest{RunId: runID, IncludeHistory: *history}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	if *system && r.Msg.SystemPrompt != "" {
		fmt.Fprintf(e.stdout, "── system\n%s\n\n", r.Msg.SystemPrompt)
	}
	for _, m := range r.Msg.Messages {
		head := m.Role
		if m.RunId != runID {
			head += " (earlier run " + m.RunId + ")"
		}
		if m.ToolCallId != "" {
			head += " ← " + m.ToolCallId
		}
		fmt.Fprintf(e.stdout, "── %s\n", head)
		if m.Content != "" {
			fmt.Fprintln(e.stdout, m.Content)
		}
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Arguments.AsInterface())
			fmt.Fprintf(e.stdout, "→ %s %s (%s)\n", tc.Name, args, tc.Id)
		}
		fmt.Fprintln(e.stdout)
	}
	return nil
}

// cmdNotify manages a namespace's notification targets.
func (e *env) cmdNotify(ctx context.Context, args []string) error {
	const usage = "agen notify set <name> <url> [--event E]... | agen notify ls | agen notify rm <name>"
	if len(args) == 0 {
		return usageErr(usage)
	}
	var cf clientFlags
	fs := e.flags("notify "+args[0], &cf)
	var events listFlag
	if args[0] == "set" {
		fs.Var(&events, "event", "event to send (repeatable; default all): approval.pending, task.failed, budget.exhausted")
	}
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	switch {
	case args[0] == "set" && len(pos) == 2:
		r, err := c.SetNotificationTarget(ctx, connect.NewRequest(&agenv1.SetNotificationTargetRequest{Target: &agenv1.NotificationTarget{
			Namespace: cf.namespace, Name: pos[0], Url: pos[1], Events: events}}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		fmt.Fprintf(e.stdout, "%s\nEach POST carries X-Agen-Signature: sha256=<HMAC-SHA256 of the body with this secret> (shown only once)\n", r.Msg.Secret)
		return nil
	case args[0] == "ls" && len(pos) == 0:
		r, err := c.ListNotificationTargets(ctx, connect.NewRequest(&agenv1.ListNotificationTargetsRequest{Namespace: cf.namespace}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		w := e.table()
		fmt.Fprintln(w, "NAMESPACE\tNAME\tURL\tEVENTS")
		for _, t := range r.Msg.Targets {
			ev := strings.Join(t.Events, ",")
			if ev == "" {
				ev = "all"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Namespace, t.Name, t.Url, ev)
		}
		return w.Flush()
	case args[0] == "rm" && len(pos) == 1:
		_, err := c.DeleteNotificationTarget(ctx, connect.NewRequest(&agenv1.DeleteNotificationTargetRequest{Namespace: cf.namespace, Name: pos[0]}))
		return err
	}
	return usageErr(usage)
}
