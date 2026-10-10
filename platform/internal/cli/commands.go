package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/definition"
)

func (e *env) table() *tabwriter.Writer { return tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0) }

func ref(cf clientFlags, name string) *agenv1.DeploymentRef {
	return &agenv1.DeploymentRef{Namespace: cf.namespace, Name: name}
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func stateName(s fmt.Stringer, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), prefix))
}

// readBundle reads a bundle directory as bundle-relative path -> bytes.
func readBundle(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if definition.Ignored(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		files[rel] = b
		return err
	})
	if err == nil && len(files) == 0 {
		err = fmt.Errorf("%s has no files", dir)
	}
	return files, err
}

func (e *env) cmdDeploy(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("deploy", &cf)
	name := fs.String("name", "", "deployment name (default: bundle name)")
	replicas := fs.Int("replicas", -1, "desired instances after deploying")
	validate := fs.Bool("validate", false, "check the bundle and print warnings without deploying")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen deploy <bundle-dir> [--name N] [--replicas N] [--validate] [-n NS]")
	}
	files, err := readBundle(pos[0])
	if err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	if *validate {
		r, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: cf.namespace, Name: *name, BundleFiles: files, ValidateOnly: true}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		for _, w := range r.Msg.Warnings {
			fmt.Fprintln(e.stdout, "warning: "+w)
		}
		fmt.Fprintln(e.stdout, "bundle is valid")
		return nil
	}
	created, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: cf.namespace, Name: *name, BundleFiles: files}))
	var d *agenv1.Deployment
	var warnings []string
	switch {
	case err == nil:
		d, warnings = created.Msg.Deployment, created.Msg.Warnings
	case connect.CodeOf(err) == connect.CodeAlreadyExists:
		n := *name
		if n == "" {
			n = bundleName(files)
		}
		if n == "" {
			return errors.New("deployment exists but the bundle has no name in plugin.json; pass --name")
		}
		u, err := c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref(cf, n), BundleFiles: files}))
		if err != nil {
			return err
		}
		d, warnings = u.Msg.Deployment, u.Msg.Warnings
	default:
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(e.stderr, "warning: "+w)
	}
	if *replicas >= 0 {
		s, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref(cf, d.Name), Desired: int32(*replicas)}))
		if err != nil {
			return err
		}
		d = s.Msg.Deployment
	}
	if cf.json {
		return e.printJSON(d)
	}
	fmt.Fprintf(e.stdout, "deployed %s/%s definition %s desired %d\n", d.Namespace, d.Name, shortDigest(d.DefinitionDigest), d.Desired)
	return nil
}

// bundleName reads the name from .claude-plugin/plugin.json or plugin.json.
func bundleName(files map[string][]byte) string {
	for _, p := range []string{".claude-plugin/plugin.json", "plugin.json"} {
		if b, ok := files[p]; ok {
			var m struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b, &m) == nil && m.Name != "" {
				return m.Name
			}
		}
	}
	return ""
}

func (e *env) cmdScale(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("scale", &cf)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("agen scale <name> <n>")
	}
	n, err := strconv.Atoi(pos[1])
	if err != nil || n < 0 {
		return fmt.Errorf("invalid instance count %q", pos[1])
	}
	return e.scaleTo(ctx, cf, pos[0], n)
}

func (e *env) scaleTo(ctx context.Context, cf clientFlags, name string, n int) error {
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref(cf, name), Desired: int32(n)}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	fmt.Fprintf(e.stdout, "%s/%s desired %d\n", r.Msg.Deployment.Namespace, r.Msg.Deployment.Name, r.Msg.Deployment.Desired)
	return nil
}

// cmdStop pauses a deployment: all instances stop and it stays at 0 (queued
// tasks and calls wait) until `agen start`.
func (e *env) cmdStop(ctx context.Context, args []string) error { return e.pause(ctx, args, true) }

// cmdStart resumes a paused deployment.
func (e *env) cmdStart(ctx context.Context, args []string) error { return e.pause(ctx, args, false) }

func (e *env) pause(ctx context.Context, args []string, paused bool) error {
	var cf clientFlags
	name := "start"
	if paused {
		name = "stop"
	}
	pos, err := parse(e.flags(name, &cf), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen stop|start <name>")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref(cf, pos[0]), Paused: paused}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	d := r.Msg.Deployment
	if paused {
		fmt.Fprintf(e.stdout, "%s/%s stopped (paused)\n", d.Namespace, d.Name)
	} else {
		fmt.Fprintf(e.stdout, "%s/%s started, desired %d\n", d.Namespace, d.Name, d.Desired)
	}
	return nil
}

func (e *env) cmdRm(ctx context.Context, args []string) error {
	var cf clientFlags
	pos, err := parse(e.flags("rm", &cf), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen rm <name>")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.DeleteDeployment(ctx, connect.NewRequest(&agenv1.DeleteDeploymentRequest{Ref: ref(cf, pos[0])}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	fmt.Fprintf(e.stdout, "removed %s/%s\n", cf.namespace, pos[0])
	return nil
}

func (e *env) cmdPs(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("ps", &cf)
	all := fs.Bool("all", false, "list every instance on every nest, in all namespaces")
	allNs := fs.Bool("A", false, "all namespaces")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	ns := cf.namespace
	if *all || *allNs {
		ns = ""
	}
	if *all {
		r, err := c.ListInstances(ctx, connect.NewRequest(&agenv1.ListInstancesRequest{Namespace: ns}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		nests := map[string]string{}
		if nr, err := c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{})); err == nil {
			for _, n := range nr.Msg.Nests {
				nests[n.Id] = n.Name
			}
		}
		w := e.table()
		fmt.Fprintln(w, "INSTANCE\tDEPLOYMENT\tNEST\tSTATE\tTASKS\tTOOLS\tDEFINITION\tENDPOINT")
		for _, in := range r.Msg.Instances {
			nest := nests[in.NestId]
			if nest == "" {
				nest = in.NestId
			}
			fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%d\t%s\t%s\t%s\n", in.Id, in.Namespace, in.Deployment, nest,
				stateName(in.State, "INSTANCE_STATE_"), in.RunningTasks, toolSummary(in), shortDigest(in.DefinitionDigest), in.Endpoint)
		}
		return w.Flush()
	}
	r, err := c.ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{Namespace: ns}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	w := e.table()
	fmt.Fprintln(w, "DEPLOYMENT\tKIND\tREADY\tDESIRED\tMIN..MAX\tDEFINITION\tSTATUS")
	for _, d := range r.Msg.Deployments {
		status := ""
		switch {
		case d.Paused:
			status = "stopped"
		case d.BudgetExhausted:
			status = fmt.Sprintf("daily budget used ($%.2f)", d.SpentUsdToday)
		}
		fmt.Fprintf(w, "%s/%s\t%s\t%d\t%d\t%d..%d\t%s\t%s\n", d.Namespace, d.Name, stateName(d.Kind, "DEPLOYMENT_KIND_"), d.Ready, d.Desired,
			d.GetScale().GetMin(), d.GetScale().GetMax(), shortDigest(d.DefinitionDigest), status)
	}
	return w.Flush()
}

func (e *env) cmdLogs(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("logs", &cf)
	follow := fs.Bool("f", false, "follow")
	instance := fs.String("instance", "", "only this instance")
	limit := fs.Int("limit", 200, "lines")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen logs <name> [-f] [--instance ID]")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	var since *timestamppb.Timestamp
	// Lines are fetched from 1 ms before the newest one printed; seen holds
	// the lines printed at that newest millisecond so none repeat or drop.
	var lastMs int64
	seen := map[string]bool{}
	for {
		r, err := c.GetLogs(ctx, connect.NewRequest(&agenv1.GetLogsRequest{Ref: ref(cf, pos[0]), InstanceId: *instance, Limit: int32(*limit), Since: since}))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if cf.json && !*follow {
			return e.printJSON(r.Msg)
		}
		for _, l := range r.Msg.Lines {
			ms := l.Time.AsTime().UnixMilli()
			key := l.InstanceId + "\x00" + l.Level + "\x00" + l.Message
			if ms == lastMs && seen[key] {
				continue
			}
			if ms != lastMs {
				lastMs, seen = ms, map[string]bool{}
			}
			seen[key] = true
			fmt.Fprintf(e.stdout, "%s %s %-5s %s\n", l.Time.AsTime().Local().Format(time.RFC3339), l.InstanceId, l.Level, l.Message)
		}
		if lastMs > 0 {
			since = timestamppb.New(time.UnixMilli(lastMs - 1))
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (e *env) cmdRun(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("run", &cf)
	noWait := fs.Bool("no-wait", false, "print the task id and return")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the result")
	key := fs.String("idempotency-key", "", "resubmitting the same key returns the same task")
	conversation := fs.String("conversation", "", "continue the conversation of earlier tasks with this key")
	labels := labelFlag{}
	fs.Var(labels, "label", "label the task, key=value (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("agen run <name> <input> [--conversation KEY] [--label k=v] [--no-wait] [--timeout D]")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	s, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref(cf, pos[0]), Input: pos[1], IdempotencyKey: *key,
		ConversationKey: *conversation, Labels: labels}))
	if err != nil {
		return err
	}
	t := s.Msg.Task
	if *noWait {
		if cf.json {
			return e.printJSON(t)
		}
		fmt.Fprintln(e.stdout, t.Id)
		return nil
	}
	deadline := time.Now().Add(*timeout)
	for !terminal(t.State) {
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s still %s after %s", t.Id, stateName(t.State, "TASK_STATE_"), *timeout)
		}
		r, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: t.Id, WaitSeconds: 30}))
		if err != nil {
			return err
		}
		if a := r.Msg.Task.PendingApprovalId; a != "" && a != t.PendingApprovalId {
			fmt.Fprintf(e.stderr, "waiting for approval %s (agen approvals; agen approve %s with another token)\n", a, a)
		}
		t = r.Msg.Task
	}
	if cf.json {
		return e.printJSON(t)
	}
	if t.State != agenv1.TaskState_TASK_STATE_SUCCEEDED {
		return fmt.Errorf("task %s %s: %s", t.Id, stateName(t.State, "TASK_STATE_"), t.Error)
	}
	fmt.Fprintln(e.stdout, t.Output)
	return nil
}

func terminal(s agenv1.TaskState) bool {
	return s == agenv1.TaskState_TASK_STATE_SUCCEEDED || s == agenv1.TaskState_TASK_STATE_FAILED || s == agenv1.TaskState_TASK_STATE_CANCELLED
}

func (e *env) cmdTasks(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("tasks", &cf)
	limit := fs.Int("limit", 20, "tasks")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	dep := ""
	if len(pos) > 0 {
		dep = pos[0]
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Namespace: cf.namespace, Deployment: dep, Limit: int32(*limit)}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	w := e.table()
	fmt.Fprintln(w, "TASK\tDEPLOYMENT\tSTATE\tSOURCE\tATTEMPTS\tCREATED")
	for _, t := range r.Msg.Tasks {
		state := stateName(t.State, "TASK_STATE_")
		if t.PendingApprovalId != "" {
			state += " (approval " + t.PendingApprovalId + ")"
		}
		fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%d\t%s\n", t.Id, t.Namespace, t.Deployment, state, t.Source, t.Attempts,
			t.CreatedAt.AsTime().Local().Format(time.RFC3339))
	}
	return w.Flush()
}

// labelFlag collects repeated --label key=value flags.
type labelFlag map[string]string

func (l labelFlag) String() string { return "" }

func (l labelFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("label %q: want key=value", v)
	}
	l[k] = val
	return nil
}

// toolSummary is "<n> tools" plus any tool server that is not connected.
func toolSummary(in *agenv1.Instance) string {
	s := fmt.Sprintf("%d", len(in.Tools))
	for _, ts := range in.ToolServers {
		if ts.State != "connected" {
			s += " (" + ts.Name + " " + ts.State + ")"
		}
	}
	return s
}

func (e *env) cmdWhoami(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("whoami", &cf)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.WhoAmI(ctx, connect.NewRequest(&agenv1.WhoAmIRequest{}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	who := r.Msg.Id
	if r.Msg.Name != "" {
		who = r.Msg.Name + " (" + r.Msg.Id + ")"
	}
	ns := "all"
	if len(r.Msg.Namespaces) > 0 {
		ns = strings.Join(r.Msg.Namespaces, ", ")
	}
	fmt.Fprintf(e.stdout, "%s\nscopes: %s\nnamespaces: %s\n", who, strings.Join(r.Msg.Scopes, ", "), ns)
	return nil
}

func (e *env) cmdNests(ctx context.Context, args []string) error {
	var cf clientFlags
	if len(args) > 0 && args[0] == "revoke" {
		rest, err := parse(e.flags("nests revoke", &cf), args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return usageErr("agen nests revoke NEST_ID")
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		if _, err := c.RevokeNest(ctx, connect.NewRequest(&agenv1.RevokeNestRequest{Id: rest[0]})); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "nest %s revoked\n", rest[0])
		return nil
	}
	if _, err := parse(e.flags("nests", &cf), args); err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	w := e.table()
	fmt.Fprintln(w, "NEST\tNAME\tBACKEND\tSTATE\tUSED/CAPACITY\tLAST HEARTBEAT")
	for _, n := range r.Msg.Nests {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d/%d\t%s\n", n.Id, n.Name, n.Backend, stateName(n.State, "NEST_STATE_"), n.Used, n.Capacity,
			n.LastHeartbeat.AsTime().Local().Format(time.RFC3339))
	}
	return w.Flush()
}

func (e *env) cmdApprovals(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("approvals", &cf)
	all := fs.Bool("all", false, "include decided approvals")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	req := &agenv1.ListApprovalsRequest{Namespace: cf.namespace, State: agenv1.ApprovalState_APPROVAL_STATE_PENDING}
	if *all {
		req.State = agenv1.ApprovalState_APPROVAL_STATE_UNSPECIFIED
	}
	r, err := c.ListApprovals(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	w := e.table()
	fmt.Fprintln(w, "APPROVAL\tDEPLOYMENT\tTOOL\tSTATE\tREQUESTED BY\tTASK\tARGUMENTS\tCREATED\tEXPIRES")
	for _, a := range r.Msg.Approvals {
		args, _ := a.Arguments.MarshalJSON()
		state := stateName(a.State, "APPROVAL_STATE_")
		if a.DecidedByName != "" {
			state += " by " + a.DecidedByName
		}
		fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Id, a.Namespace, a.Deployment, a.Tool, state, a.RequestedByName, a.TaskId, args,
			a.CreatedAt.AsTime().Local().Format(time.RFC3339), a.ExpiresAt.AsTime().Local().Format(time.RFC3339))
	}
	return w.Flush()
}

func (e *env) decide(approve bool) func(context.Context, []string) error {
	return func(ctx context.Context, args []string) error {
		var cf clientFlags
		pos, err := parse(e.flags("approve", &cf), args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageErr("agen approve|deny <approval-id>")
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		r, err := c.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: pos[0], Approve: approve}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		fmt.Fprintf(e.stdout, "%s %s\n", r.Msg.Approval.Id, stateName(r.Msg.Approval.State, "APPROVAL_STATE_"))
		return nil
	}
}

func (e *env) cmdJoinToken(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("join-token", &cf)
	ttl := fs.Duration("ttl", time.Hour, "validity")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: int32(ttl.Seconds())}))
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, r.Msg.Token)
	if r.Msg.CaHash != "" {
		fmt.Fprintf(e.stdout, "CA %s (agen nest run --ca-hash %s)\n", r.Msg.CaHash, r.Msg.CaHash)
	}
	return nil
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, strings.Split(v, ",")...); return nil }

func (e *env) cmdToken(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return usageErr("agen token create --name N --scope viewer|operator|approver|admin [--namespace NS] [--ttl D] [--on-behalf]")
	}
	var cf clientFlags
	fs := e.flags("token create", &cf)
	name := fs.String("name", "", "token name")
	var scopes, namespaces listFlag
	fs.Var(&scopes, "scope", "scope (repeatable)")
	fs.Var(&namespaces, "namespace", "limit to namespace (repeatable)")
	ttl := fs.Duration("ttl", 0, "validity (0 = no expiry)")
	onBehalf := fs.Bool("on-behalf", false, "for an agent that works for people: it acts for whoever submitted its task (see the assistant template)")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	if len(scopes) == 0 {
		return errors.New("at least one --scope is required")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: *name, Scopes: scopes, Namespaces: namespaces,
		TtlSeconds: int32(ttl.Seconds()), OnBehalf: *onBehalf}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	fmt.Fprintf(e.stdout, "%s\n(token id %s; the secret is shown only once)\n", r.Msg.Secret, r.Msg.Token.Id)
	return nil
}

// cmdCall sends one A2A message to a deployment: the Hub resolves it to a
// Gateway, and the call itself goes straight to that Gateway.
func (e *env) cmdCall(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("call", &cf)
	timeout := fs.Duration("timeout", 5*time.Minute, "request timeout")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("agen call <name> <text> [--timeout D] [--json]")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: ref(cf, pos[0])}))
	if err != nil {
		return err
	}
	if len(r.Msg.Endpoints) == 0 {
		return fmt.Errorf("%s/%s has no reachable gateway (no eligible nest with a gateway)", cf.namespace, pos[0])
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
		"message": map[string]any{"kind": "message", "role": "user", "messageId": newMessageID(), "parts": []map[string]string{{"kind": "text", "text": pos[1]}}},
	}})
	cctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, r.Msg.Endpoints[0], bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Msg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Msg.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Result *struct {
			Status struct {
				State   string `json:"state"`
				Message *struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
			Artifacts []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"artifacts"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if cf.json {
		_, err := fmt.Fprintln(e.stdout, strings.TrimSpace(string(raw)))
		return err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("gateway answered %s: %s", resp.Status, raw)
	}
	if out.Error != nil {
		return fmt.Errorf("a2a error %d: %s", out.Error.Code, out.Error.Message)
	}
	if out.Result == nil {
		return fmt.Errorf("gateway answered %s without a result", resp.Status)
	}
	if out.Result.Status.State != "completed" {
		msg := ""
		if m := out.Result.Status.Message; m != nil && len(m.Parts) > 0 {
			msg = m.Parts[0].Text
		}
		return fmt.Errorf("task %s: %s", out.Result.Status.State, msg)
	}
	for _, a := range out.Result.Artifacts {
		for _, p := range a.Parts {
			fmt.Fprintln(e.stdout, p.Text)
		}
	}
	return nil
}

func newMessageID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *env) cmdResolve(ctx context.Context, args []string) error {
	var cf clientFlags
	pos, err := parse(e.flags("resolve", &cf), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen resolve <name>")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: ref(cf, pos[0])}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	fmt.Fprintf(e.stdout, "ready %d\n", r.Msg.Ready)
	for _, ep := range r.Msg.Endpoints {
		fmt.Fprintln(e.stdout, ep)
	}
	return nil
}

func (e *env) cmdTriggers(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("triggers", &cf)
	limit := fs.Int("limit", 50, "events")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen triggers <name> [--limit N]")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.ListTriggerEvents(ctx, connect.NewRequest(&agenv1.ListTriggerEventsRequest{Ref: ref(cf, pos[0]), Limit: int32(*limit)}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	w := e.table()
	fmt.Fprintln(w, "TRIGGER\tSTATE\tDUE\tTASK\tMESSAGE")
	for _, ev := range r.Msg.Events {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ev.Trigger, stateName(ev.State, "TRIGGER_EVENT_STATE_"),
			ev.DueAt.AsTime().Local().Format(time.RFC3339), ev.TaskId, ev.Message)
	}
	return w.Flush()
}

func (e *env) cmdWebhookSecret(ctx context.Context, args []string) error {
	var cf clientFlags
	pos, err := parse(e.flags("webhook-secret", &cf), args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("agen webhook-secret <name> <trigger>")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref(cf, pos[0]), Trigger: pos[1]}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	fmt.Fprintf(e.stdout, "%s\nPOST %s with \"Authorization: Bearer <secret>\" (the secret is shown only once)\n", r.Msg.Secret, r.Msg.Path)
	return nil
}

// cmdTrace prints one trace as a span tree across agents: by trace id, or by
// the id of a task (its run's trace).
func (e *env) cmdTrace(ctx context.Context, args []string) error {
	var cf clientFlags
	pos, err := parse(e.flags("trace", &cf), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen trace <task-id|trace-id>")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	traceID := pos[0]
	if !isTraceID(traceID) {
		tk, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: pos[0]}))
		if err != nil {
			return err
		}
		runs, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Namespace: tk.Msg.Task.Namespace, TaskId: tk.Msg.Task.Id}))
		if err != nil {
			return err
		}
		for _, r := range runs.Msg.Runs {
			if r.Id == tk.Msg.Task.RunId {
				traceID = r.TraceId
			}
		}
		if !isTraceID(traceID) {
			return fmt.Errorf("task %s has no run yet", pos[0])
		}
	}
	tr, err := c.GetTrace(ctx, connect.NewRequest(&agenv1.GetTraceRequest{TraceId: traceID}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(tr.Msg)
	}
	dep := map[string]string{}
	for _, r := range tr.Msg.Runs {
		dep[r.Id] = r.Namespace + "/" + r.Deployment
	}
	children := map[string][]*agenv1.Span{}
	ids := map[string]bool{}
	for _, s := range tr.Msg.Spans {
		ids[s.SpanId] = true
	}
	var roots []*agenv1.Span
	for _, s := range tr.Msg.Spans {
		if s.ParentSpanId == "" || !ids[s.ParentSpanId] {
			roots = append(roots, s)
		} else {
			children[s.ParentSpanId] = append(children[s.ParentSpanId], s)
		}
	}
	fmt.Fprintf(e.stdout, "trace %s: %d runs, %d spans\n", traceID, len(tr.Msg.Runs), len(tr.Msg.Spans))
	var walk func(s *agenv1.Span, depth int)
	walk = func(s *agenv1.Span, depth int) {
		extra := ""
		if s.Name == "agen.run" || s.Name == "agen.run.resume" {
			extra = "  [" + dep[s.RunId] + " run " + s.RunId + "]"
		}
		if tool, ok := s.Attributes.AsMap()["gen_ai.tool.name"].(string); ok {
			extra = "  " + tool
		}
		ms := s.End.AsTime().Sub(s.Start.AsTime()).Milliseconds()
		fmt.Fprintf(e.stdout, "%s%s %s %dms%s\n", strings.Repeat("  ", depth), s.Name, s.Status, ms, extra)
		for _, ch := range children[s.SpanId] {
			walk(ch, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return nil
}

func isTraceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// cmdUI prints a sign-in link for the web UI. The token travels in the URL
// fragment, which browsers never send to a server; the UI keeps it in the
// tab's session storage and removes it from the address bar.
func (e *env) cmdUI(_ context.Context, args []string) error {
	var cf clientFlags
	if _, err := parse(e.flags("ui", &cf), args); err != nil {
		return err
	}
	if cf.hub == "" || cf.token == "" {
		lc, err := loadLocalConfig(e.home)
		if err != nil && cf.hub == "" {
			return errors.New("no local agen found: run 'agen up', or pass --hub and --token")
		}
		if cf.hub == "" {
			cf.hub = lc.Hub
		}
		if cf.token == "" && strings.TrimRight(cf.hub, "/") == strings.TrimRight(lc.Hub, "/") {
			cf.token = lc.Token
		}
	}
	if cf.token == "" {
		return errors.New("--token is required for " + cf.hub)
	}
	fmt.Fprintf(e.stdout, "%s/#token=%s\n", strings.TrimRight(cf.hub, "/"), cf.token)
	return nil
}
