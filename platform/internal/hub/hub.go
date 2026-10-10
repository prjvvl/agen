// Package hub is the control plane (docs/architecture.md §3–§5): the client
// API (HubService) over the store. The scheduler, autoscaler, triggers and
// the Nest API build on the same Hub value.
package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/pki"
	"github.com/prjvvl/agen/platform/internal/store"
)

func msTime(t int64) time.Time { return time.UnixMilli(t) }

// Hub serves the client API.
type Hub struct {
	Store *store.Store
	Auth  *Auth
	// Poll is how often GetTask(wait_seconds) re-checks a task.
	Poll time.Duration

	// CA is set by EnableTLS: Nests then enrol with a CSR and must present
	// their client certificate.
	CA *pki.Authority

	rejMu    sync.Mutex
	rejected map[string]time.Time // last recorded webhook rejection per trigger

	tokenMu sync.Mutex
	keys    *tokenKeyring // call-token keys, re-read every keyringTTL
}

// New creates a Hub over a store; adminToken authenticates the admin.
func New(s *store.Store, adminToken string) *Hub {
	return &Hub{Store: s, Auth: &Auth{AdminToken: adminToken, Store: s}, Poll: 100 * time.Millisecond}
}

// Handler mounts HubService (Connect, gRPC and JSON) with authentication.
func (h *Hub) Handler() (string, http.Handler) {
	return agenv1connect.NewHubServiceHandler(h, connect.WithInterceptors(h.Auth.Interceptor()))
}

var _ agenv1connect.HubServiceHandler = (*Hub)(nil)

func nsOr(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

// ready counts ready or busy instances per deployment.
func (h *Hub) ready(ctx context.Context, ns, name string) int {
	list, _ := h.Store.ListInstances(ctx, ns, name, "")
	n := 0
	for _, i := range list {
		if i.State == "ready" || i.State == "busy" {
			n++
		}
	}
	return n
}

// ---- deployments ----

func (h *Hub) CreateDeployment(ctx context.Context, req *connect.Request[agenv1.CreateDeploymentRequest]) (*connect.Response[agenv1.CreateDeploymentResponse], error) {
	m := req.Msg
	ns := nsOr(m.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	files, err := bundleFiles(m.BundleFiles, m.BundleText)
	if err != nil {
		return nil, err
	}
	b, err := bundle.Parse(files)
	if err != nil {
		return nil, connectErr(err)
	}
	name := m.Name
	if name == "" {
		name = b.Name
	}
	p := policyFromBundle(b)
	kind := b.Kind
	if m.Kind != agenv1.DeploymentKind_DEPLOYMENT_KIND_UNSPECIFIED {
		kind = kindString(m.Kind)
	}
	if m.Scale != nil {
		p.Scale = m.Scale
	}
	if m.Budget != nil {
		p.Budget = m.Budget
	}
	if m.Limits != nil {
		p.Limits = m.Limits
	}
	if m.Placement != nil {
		p.Placement = m.Placement
	}
	if len(m.Triggers) > 0 {
		p.Triggers = m.Triggers
	}
	if err := validateScale(kind, p.Scale); err != nil {
		return nil, err
	}
	if err := validateTriggers(p.Triggers); err != nil {
		return nil, err
	}
	warnings := h.bundleWarnings(ctx, ns, b)
	if m.ValidateOnly {
		if _, err := h.Store.GetDeployment(ctx, ns, name); err == nil {
			warnings = append(warnings, fmt.Sprintf("deployment %s/%s already exists: deploying would fail; use UpdateDeployment", ns, name))
		}
		return connect.NewResponse(&agenv1.CreateDeploymentResponse{Warnings: warnings}), nil
	}
	if err := h.Store.PutDefinition(ctx, store.Definition{Digest: b.Digest, Name: b.Name, Files: b.Files}); err != nil {
		return nil, connectErr(err)
	}
	stampTriggers(p.Triggers, nil)
	d := store.Deployment{Namespace: ns, Name: name, DefinitionDigest: b.Digest, Kind: kind, Desired: int(p.Scale.Min)}
	p.apply(&d)
	d, err = h.Store.CreateDeployment(ctx, d)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.CreateDeploymentResponse{Deployment: deploymentProto(d, 0), Warnings: warnings}), nil
}

func validateScale(kind string, s *agenv1.ScalePolicy) error {
	if _, ok := kindNames[kind]; !ok {
		return invalid("kind must be singleton, pool or task")
	}
	if s.Max == 0 {
		s.Max = 1
	}
	if kind == "singleton" && s.Max > 1 {
		return invalid("a singleton runs at most one instance (scale.max must be 1)")
	}
	if s.Min < 0 || s.Min > s.Max {
		return invalid("scale.min must be between 0 and scale.max")
	}
	return nil
}

func (h *Hub) UpdateDeployment(ctx context.Context, req *connect.Request[agenv1.UpdateDeploymentRequest]) (*connect.Response[agenv1.UpdateDeploymentResponse], error) {
	m := req.Msg
	ns := nsOr(m.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	var nb *bundle.Bundle
	var warnings []string
	if len(m.BundleFiles) > 0 || len(m.BundleText) > 0 {
		files, err := bundleFiles(m.BundleFiles, m.BundleText)
		if err != nil {
			return nil, err
		}
		b, err := bundle.Parse(files)
		if err != nil {
			return nil, connectErr(err)
		}
		warnings = h.bundleWarnings(ctx, ns, b)
		if !m.ValidateOnly {
			if err := h.Store.PutDefinition(ctx, store.Definition{Digest: b.Digest, Name: b.Name, Files: b.Files}); err != nil {
				return nil, connectErr(err)
			}
		}
		nb = b
	}
	if m.ValidateOnly {
		cur, err := h.Store.GetDeployment(ctx, ns, m.GetRef().GetName())
		if err != nil {
			return nil, connectErr(err)
		}
		p := PolicyOf(cur)
		kind := cur.Kind
		if nb != nil {
			bp := policyFromBundle(nb)
			kind, p.Scale, p.Triggers = nb.Kind, bp.Scale, bp.Triggers
		}
		if m.Scale != nil {
			p.Scale = m.Scale
		}
		if len(m.Triggers) > 0 {
			p.Triggers = m.Triggers
		}
		if err := validateScale(kind, p.Scale); err != nil {
			return nil, err
		}
		if err := validateTriggers(p.Triggers); err != nil {
			return nil, err
		}
		return connect.NewResponse(&agenv1.UpdateDeploymentResponse{Warnings: warnings}), nil
	}
	d, err := h.Store.UpdateDeployment(ctx, ns, m.GetRef().GetName(), func(d *store.Deployment) error {
		p := PolicyOf(*d)
		if nb != nil {
			// A new bundle version brings its own kind and policy (the bundle
			// is the source of truth); placement is not part of bundles and is
			// kept. Explicit request fields below still override.
			bp := policyFromBundle(nb)
			d.DefinitionDigest, d.Kind = nb.Digest, nb.Kind
			p.Scale, p.Budget, p.Limits, p.Triggers = bp.Scale, bp.Budget, bp.Limits, bp.Triggers
		}
		if m.Scale != nil {
			p.Scale = m.Scale
		}
		if m.Budget != nil {
			p.Budget = m.Budget
		}
		if m.Limits != nil {
			p.Limits = m.Limits
		}
		if m.Placement != nil {
			p.Placement = m.Placement
		}
		if len(m.Triggers) > 0 {
			p.Triggers = m.Triggers
		}
		if err := validateScale(d.Kind, p.Scale); err != nil {
			return err
		}
		if err := validateTriggers(p.Triggers); err != nil {
			return err
		}
		stampTriggers(p.Triggers, PolicyOf(*d).Triggers)
		d.Desired = clamp(d.Desired, int(p.Scale.Min), int(p.Scale.Max))
		if d.Paused {
			d.Desired = 0
		}
		p.apply(d)
		return nil
	})
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.UpdateDeploymentResponse{Deployment: h.withBudget(ctx, d, deploymentProto(d, h.ready(ctx, ns, d.Name))), Warnings: warnings}), nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (h *Hub) GetDeployment(ctx context.Context, req *connect.Request[agenv1.GetDeploymentRequest]) (*connect.Response[agenv1.GetDeploymentResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	d, err := h.Store.GetDeployment(ctx, ns, req.Msg.GetRef().GetName())
	if err != nil {
		return nil, connectErr(err)
	}
	insts, err := h.Store.ListInstances(ctx, ns, d.Name, "")
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.GetDeploymentResponse{}
	ready := 0
	for _, i := range insts {
		out.Instances = append(out.Instances, instanceProto(i))
		if i.State == "ready" || i.State == "busy" {
			ready++
		}
	}
	out.Deployment = h.withBudget(ctx, d, deploymentProto(d, ready))
	return connect.NewResponse(out), nil
}

func (h *Hub) ListDeployments(ctx context.Context, req *connect.Request[agenv1.ListDeploymentsRequest]) (*connect.Response[agenv1.ListDeploymentsResponse], error) {
	if req.Msg.Namespace != "" {
		if err := requireNamespace(ctx, req.Msg.Namespace); err != nil {
			return nil, err
		}
	}
	list, err := h.Store.ListDeployments(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, connectErr(err)
	}
	p := PrincipalFrom(ctx)
	out := &agenv1.ListDeploymentsResponse{}
	for _, d := range list {
		if p.AllowsNamespace(d.Namespace) {
			out.Deployments = append(out.Deployments, h.withBudget(ctx, d, deploymentProto(d, h.ready(ctx, d.Namespace, d.Name))))
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) ScaleDeployment(ctx context.Context, req *connect.Request[agenv1.ScaleDeploymentRequest]) (*connect.Response[agenv1.ScaleDeploymentResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	want := int(req.Msg.Desired)
	d, err := h.Store.UpdateDeployment(ctx, ns, req.Msg.GetRef().GetName(), func(d *store.Deployment) error {
		if d.Paused {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("deployment is paused; resume it first"))
		}
		if want > 0 {
			if spent, exhausted, err := h.spentToday(ctx, *d); err == nil && exhausted {
				return budgetError(*d, spent)
			}
		}
		s := PolicyOf(*d).Scale
		if want < int(s.Min) || want > int(s.Max) {
			return invalid("desired must be within scale.min..scale.max; update the scale policy to go beyond it")
		}
		d.Desired = want
		// Counts as activity (same write), so the autoscaler holds it for
		// one idle window.
		d.LastActivityMs = store.NowMs()
		return nil
	})
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.ScaleDeploymentResponse{Deployment: h.withBudget(ctx, d, deploymentProto(d, h.ready(ctx, ns, d.Name)))}), nil
}

func (h *Hub) DeleteDeployment(ctx context.Context, req *connect.Request[agenv1.DeleteDeploymentRequest]) (*connect.Response[agenv1.DeleteDeploymentResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	if err := h.Store.DeleteDeployment(ctx, ns, req.Msg.GetRef().GetName()); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.DeleteDeploymentResponse{}), nil
}

// ---- fleet ----

func (h *Hub) ListInstances(ctx context.Context, req *connect.Request[agenv1.ListInstancesRequest]) (*connect.Response[agenv1.ListInstancesResponse], error) {
	if req.Msg.Namespace != "" {
		if err := requireNamespace(ctx, req.Msg.Namespace); err != nil {
			return nil, err
		}
	}
	list, err := h.Store.ListInstances(ctx, req.Msg.Namespace, req.Msg.Deployment, req.Msg.NestId)
	if err != nil {
		return nil, connectErr(err)
	}
	p := PrincipalFrom(ctx)
	out := &agenv1.ListInstancesResponse{}
	for _, i := range list {
		if p.AllowsNamespace(i.Namespace) {
			out.Instances = append(out.Instances, instanceProto(i))
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) ListNests(ctx context.Context, _ *connect.Request[agenv1.ListNestsRequest]) (*connect.Response[agenv1.ListNestsResponse], error) {
	nests, err := h.Store.ListNests(ctx)
	if err != nil {
		return nil, connectErr(err)
	}
	insts, err := h.Store.ListInstances(ctx, "", "", "")
	if err != nil {
		return nil, connectErr(err)
	}
	used := map[string]int{}
	for _, in := range insts {
		used[in.NestID]++
	}
	out := &agenv1.ListNestsResponse{}
	for _, n := range nests {
		out.Nests = append(out.Nests, nestProto(n, used[n.ID]))
	}
	return connect.NewResponse(out), nil
}

// ---- work ----

func (h *Hub) SubmitTask(ctx context.Context, req *connect.Request[agenv1.SubmitTaskRequest]) (*connect.Response[agenv1.SubmitTaskResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	name := req.Msg.GetRef().GetName()
	if _, err := h.Store.GetDeployment(ctx, ns, name); err != nil {
		return nil, connectErr(err)
	}
	if err := checkWorkIdentity(req.Msg.ConversationKey, req.Msg.Labels); err != nil {
		return nil, err
	}
	t, err := h.Store.SubmitTask(ctx, store.Task{Namespace: ns, Deployment: name, Input: req.Msg.Input, IdempotencyKey: req.Msg.IdempotencyKey, Source: "api",
		SubmittedBy: PrincipalFrom(ctx).ID, ConversationKey: scopedKey("api:", req.Msg.ConversationKey), Labels: req.Msg.Labels})
	if err != nil {
		return nil, connectErr(err)
	}
	_ = h.Store.TouchActivity(ctx, ns, name)
	return connect.NewResponse(&agenv1.SubmitTaskResponse{Task: TaskProto(t)}), nil
}

// MaxTaskWait bounds GetTask's wait_seconds.
const MaxTaskWait = 5 * time.Minute

func (h *Hub) GetTask(ctx context.Context, req *connect.Request[agenv1.GetTaskRequest]) (*connect.Response[agenv1.GetTaskResponse], error) {
	wait := min(time.Duration(req.Msg.WaitSeconds)*time.Second, MaxTaskWait)
	deadline := time.Now().Add(wait)
	for {
		t, err := h.Store.GetTask(ctx, req.Msg.Id)
		if err != nil {
			return nil, connectErr(err)
		}
		if err := requireNamespace(ctx, t.Namespace); err != nil {
			return nil, err
		}
		if t.Terminal() || time.Now().After(deadline) {
			return connect.NewResponse(&agenv1.GetTaskResponse{Task: TaskProto(t)}), nil
		}
		select {
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		case <-time.After(h.Poll):
		}
	}
}

func (h *Hub) ListTasks(ctx context.Context, req *connect.Request[agenv1.ListTasksRequest]) (*connect.Response[agenv1.ListTasksResponse], error) {
	nss, err := namespacesFor(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, err
	}
	limit := listLimit(req.Msg.Limit, 100)
	var list []store.Task
	for _, ns := range nss {
		part, err := h.Store.ListTasks(ctx, ns, req.Msg.Deployment, taskStateName(req.Msg.State), limit)
		if err != nil {
			return nil, connectErr(err)
		}
		list = append(list, part...)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedMs != list[j].CreatedMs {
			return list[i].CreatedMs > list[j].CreatedMs
		}
		return list[i].ID > list[j].ID
	})
	out := &agenv1.ListTasksResponse{}
	for i, t := range list {
		if i == limit {
			break
		}
		out.Tasks = append(out.Tasks, TaskProto(t))
	}
	return connect.NewResponse(out), nil
}

// namespacesFor returns the namespaces a list request should query: the
// requested one (if the caller may see it), else every namespace the caller
// may see ("" = all). Filtering in the query keeps LIMIT correct for
// namespace-scoped tokens.
func namespacesFor(ctx context.Context, requested string) ([]string, error) {
	p := PrincipalFrom(ctx)
	if requested != "" {
		if err := requireNamespace(ctx, requested); err != nil {
			return nil, err
		}
		return []string{requested}, nil
	}
	if len(p.Namespaces) == 0 {
		return []string{""}, nil
	}
	return p.Namespaces, nil
}

func listLimit(requested int32, def int) int {
	if requested <= 0 {
		return def
	}
	return int(requested)
}

func (h *Hub) CancelTask(ctx context.Context, req *connect.Request[agenv1.CancelTaskRequest]) (*connect.Response[agenv1.CancelTaskResponse], error) {
	t, err := h.Store.GetTask(ctx, req.Msg.Id)
	if err != nil {
		return nil, connectErr(err)
	}
	if err := requireNamespace(ctx, t.Namespace); err != nil {
		return nil, err
	}
	t, err = h.Store.CancelTask(ctx, req.Msg.Id)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.CancelTaskResponse{Task: TaskProto(t)}), nil
}

// ---- addressing ----

func (h *Hub) Resolve(ctx context.Context, req *connect.Request[agenv1.ResolveRequest]) (*connect.Response[agenv1.ResolveResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	name := req.Msg.GetRef().GetName()
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	out, err := h.resolve(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	// Calling runs work: only operators get a token for the Gateways.
	if p := PrincipalFrom(ctx); p.Can(ScopeOperator) {
		if err := h.issueCallToken(ctx, out, "user:"+p.ID, "", ns, name, userCallTokenTTL); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(out), nil
}

// resolve returns the A2A routes of a deployment: the Gateways of nests
// running it, or of eligible nests when it sleeps.
func (h *Hub) resolve(ctx context.Context, ns, name string) (*agenv1.ResolveResponse, error) {
	d, err := h.Store.GetDeployment(ctx, ns, name)
	if err != nil {
		return nil, connectErr(err)
	}
	all, err := h.Store.AllAssignments(ctx)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.ResolveResponse{Ready: int32(h.ready(ctx, ns, name))}
	route := func(n store.Nest) string { return strings.TrimRight(n.GatewayURL, "/") + "/a2a/" + ns + "/" + name }
	for _, a := range all {
		if a.Namespace != ns || a.Deployment != name {
			continue
		}
		if n, err := h.Store.GetNest(ctx, a.NestID); err == nil && n.State == "active" && n.GatewayURL != "" {
			out.Endpoints = append(out.Endpoints, route(n))
		}
	}
	if len(out.Endpoints) == 0 {
		// Asleep (no instances placed): any eligible nest's Gateway can take
		// the call and wake the deployment.
		nests, err := h.Store.ListNests(ctx)
		if err != nil {
			return nil, connectErr(err)
		}
		for _, n := range nests {
			if n.GatewayURL != "" && nestMatches(n, d) {
				out.Endpoints = append(out.Endpoints, route(n))
			}
		}
	}
	return out, nil
}

func (h *Hub) RequestWake(ctx context.Context, req *connect.Request[agenv1.RequestWakeRequest]) (*connect.Response[agenv1.RequestWakeResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	name := req.Msg.GetRef().GetName()
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	d, err := h.Wake(ctx, ns, name)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.RequestWakeResponse{Deployment: h.withBudget(ctx, d, deploymentProto(d, h.ready(ctx, ns, name)))}), nil
}

// Wake ensures at least one instance is desired and records activity. A
// paused deployment is not woken (FailedPrecondition).
func (h *Hub) Wake(ctx context.Context, ns, name string) (store.Deployment, error) {
	d, err := h.Store.GetDeployment(ctx, ns, name)
	if err != nil {
		return d, err
	}
	if d.Paused {
		return d, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/%s is paused", ns, name))
	}
	if spent, exhausted, err := h.spentToday(ctx, d); err == nil && exhausted {
		return d, budgetError(d, spent)
	}
	if max := int(PolicyOf(d).Scale.Max); d.Desired >= 1 || max < 1 {
		return d, h.Store.TouchActivity(ctx, ns, name)
	}
	return h.Store.UpdateDeployment(ctx, ns, name, func(d *store.Deployment) error {
		if d.Paused {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/%s is paused", ns, name))
		}
		if d.Desired < 1 {
			d.Desired = 1
		}
		d.LastActivityMs = store.NowMs()
		return nil
	})
}

// PauseDeployment stops a deployment and holds it at 0 instances (paused =
// true), or resumes it at scale.min.
func (h *Hub) PauseDeployment(ctx context.Context, req *connect.Request[agenv1.PauseDeploymentRequest]) (*connect.Response[agenv1.PauseDeploymentResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	d, err := h.Store.UpdateDeployment(ctx, ns, req.Msg.GetRef().GetName(), func(d *store.Deployment) error {
		d.Paused = req.Msg.Paused
		if d.Paused {
			d.Desired = 0
		} else {
			d.Desired = int(PolicyOf(*d).Scale.Min)
			d.LastActivityMs = store.NowMs()
		}
		return nil
	})
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.PauseDeploymentResponse{Deployment: h.withBudget(ctx, d, deploymentProto(d, h.ready(ctx, ns, d.Name)))}), nil
}

// ---- approvals ----

func (h *Hub) ListApprovals(ctx context.Context, req *connect.Request[agenv1.ListApprovalsRequest]) (*connect.Response[agenv1.ListApprovalsResponse], error) {
	state := ""
	for k, v := range approvalStates {
		if v == req.Msg.State {
			state = k
		}
	}
	if req.Msg.Namespace != "" {
		if err := requireNamespace(ctx, req.Msg.Namespace); err != nil {
			return nil, err
		}
	}
	list, err := h.Store.ListApprovals(ctx, req.Msg.Namespace, state)
	if err != nil {
		return nil, connectErr(err)
	}
	p := PrincipalFrom(ctx)
	out := &agenv1.ListApprovalsResponse{}
	name := h.principalNames(ctx)
	for _, a := range list {
		if p.AllowsNamespace(a.Namespace) {
			out.Approvals = append(out.Approvals, h.approvalProto(a, name))
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) DecideApproval(ctx context.Context, req *connect.Request[agenv1.DecideApprovalRequest]) (*connect.Response[agenv1.DecideApprovalResponse], error) {
	a, err := h.Store.GetApproval(ctx, req.Msg.Id)
	if err != nil {
		return nil, connectErr(err)
	}
	if err := requireNamespace(ctx, a.Namespace); err != nil {
		return nil, err
	}
	me := PrincipalFrom(ctx).ID
	if me == a.RequestedBy {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
			"approval %s was requested by work this token (%s) asked for, and nobody may approve their own request: decide it with another token that has the approver scope",
			a.ID, h.principalNames(ctx)(me)))
	}
	decided, err := h.Store.DecideApproval(ctx, req.Msg.Id, req.Msg.Approve, me)
	if errors.Is(err, store.ErrConflict) {
		cur, _ := h.Store.GetApproval(ctx, req.Msg.Id)
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("approval %s is no longer pending (it is %s)", a.ID, cur.State))
	}
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.DecideApprovalResponse{Approval: h.approvalProto(decided, h.principalNames(ctx))}), nil
}

// ---- definitions ----

// visibleDigests returns the definition digests a namespace-scoped caller may
// see (those used by deployments in its namespaces); nil means all.
// Definitions are content-addressed and shared, so they carry no namespace.
func (h *Hub) visibleDigests(ctx context.Context) (map[string]bool, error) {
	p := PrincipalFrom(ctx)
	if len(p.Namespaces) == 0 {
		return nil, nil
	}
	out := map[string]bool{}
	for _, ns := range p.Namespaces {
		deps, err := h.Store.ListDeployments(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			out[d.DefinitionDigest] = true
		}
	}
	return out, nil
}

func (h *Hub) ListDefinitions(ctx context.Context, req *connect.Request[agenv1.ListDefinitionsRequest]) (*connect.Response[agenv1.ListDefinitionsResponse], error) {
	list, err := h.Store.ListDefinitions(ctx, req.Msg.Name)
	if err != nil {
		return nil, connectErr(err)
	}
	visible, err := h.visibleDigests(ctx)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.ListDefinitionsResponse{}
	for _, d := range list {
		if visible == nil || visible[d.Digest] {
			out.Definitions = append(out.Definitions, &agenv1.Definition{Name: d.Name, Digest: d.Digest, CreatedAt: ms(d.CreatedMs)})
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) GetDefinition(ctx context.Context, req *connect.Request[agenv1.GetDefinitionRequest]) (*connect.Response[agenv1.GetDefinitionResponse], error) {
	visible, err := h.visibleDigests(ctx)
	if err != nil {
		return nil, connectErr(err)
	}
	if visible != nil && !visible[req.Msg.Digest] {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("definition not found"))
	}
	d, err := h.Store.GetDefinition(ctx, req.Msg.Digest)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.GetDefinitionResponse{Definition: &agenv1.Definition{Name: d.Name, Digest: d.Digest, Files: d.Files, CreatedAt: ms(d.CreatedMs)},
		TextFiles: textFiles(d.Files)}), nil
}

// ---- triggers ----

func (h *Hub) ListTriggerEvents(ctx context.Context, req *connect.Request[agenv1.ListTriggerEventsRequest]) (*connect.Response[agenv1.ListTriggerEventsResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	states := map[agenv1.TriggerEventState]string{agenv1.TriggerEventState_TRIGGER_EVENT_STATE_FIRED: "fired",
		agenv1.TriggerEventState_TRIGGER_EVENT_STATE_MISSED: "missed", agenv1.TriggerEventState_TRIGGER_EVENT_STATE_REJECTED: "rejected"}
	list, err := h.Store.ListTriggerEvents(ctx, ns, req.Msg.GetRef().GetName(), states[req.Msg.State], int(req.Msg.Limit))
	if err != nil {
		return nil, connectErr(err)
	}
	back := map[string]agenv1.TriggerEventState{}
	for k, v := range states {
		back[v] = k
	}
	out := &agenv1.ListTriggerEventsResponse{}
	for _, e := range list {
		out.Events = append(out.Events, &agenv1.TriggerEvent{Id: e.ID, Namespace: e.Namespace, Deployment: e.Deployment, Trigger: e.Trigger,
			State: back[e.State], TaskId: e.TaskID, DueAt: ms(e.DueMs), RecordedAt: ms(e.RecordedMs), Message: e.Message})
	}
	return connect.NewResponse(out), nil
}

// ---- observability ----

func (h *Hub) ListRuns(ctx context.Context, req *connect.Request[agenv1.ListRunsRequest]) (*connect.Response[agenv1.ListRunsResponse], error) {
	nss, err := namespacesFor(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, err
	}
	limit := listLimit(req.Msg.Limit, 50)
	var list []store.RunRow
	for _, ns := range nss {
		part, err := h.Store.ListRuns(ctx, ns, req.Msg.Deployment, limit)
		if err != nil {
			return nil, connectErr(err)
		}
		list = append(list, part...)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].StartedMs != list[j].StartedMs {
			return list[i].StartedMs > list[j].StartedMs
		}
		return list[i].ID > list[j].ID
	})
	out := &agenv1.ListRunsResponse{}
	for i, r := range list {
		if i == limit {
			break
		}
		out.Runs = append(out.Runs, runProto(r))
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) GetTrace(ctx context.Context, req *connect.Request[agenv1.GetTraceRequest]) (*connect.Response[agenv1.GetTraceResponse], error) {
	spans, runs, err := h.Store.Trace(ctx, req.Msg.TraceId)
	if err != nil {
		return nil, connectErr(err)
	}
	p := PrincipalFrom(ctx)
	out := &agenv1.GetTraceResponse{}
	allowed := map[string]bool{}
	for _, r := range runs {
		if p.AllowsNamespace(r.Namespace) {
			allowed[r.ID] = true
			out.Runs = append(out.Runs, runProto(r))
		}
	}
	for _, s := range spans {
		if allowed[s.RunID] {
			out.Spans = append(out.Spans, spanProto(s))
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) GetLogs(ctx context.Context, req *connect.Request[agenv1.GetLogsRequest]) (*connect.Response[agenv1.GetLogsResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	var since int64
	if req.Msg.Since != nil {
		since = req.Msg.Since.AsTime().UnixMilli()
	}
	lines, err := h.Store.Logs(ctx, ns, req.Msg.GetRef().GetName(), req.Msg.InstanceId, since, int(req.Msg.Limit))
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.GetLogsResponse{}
	for _, l := range lines {
		out.Lines = append(out.Lines, &agenv1.LogLine{InstanceId: l.InstanceID, Time: ms(l.TimeMs), Level: l.Level, Message: l.Message})
	}
	return connect.NewResponse(out), nil
}

// ---- access ----

func (h *Hub) CreateJoinToken(ctx context.Context, req *connect.Request[agenv1.CreateJoinTokenRequest]) (*connect.Response[agenv1.CreateJoinTokenResponse], error) {
	ttl := int64(req.Msg.TtlSeconds)
	if ttl <= 0 {
		ttl = 3600
	}
	secret, err := h.Store.CreateJoinToken(ctx, ttl*1000)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.CreateJoinTokenResponse{Token: secret}
	if h.CA != nil {
		out.CaHash = h.CA.Hash()
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) CreateApiToken(ctx context.Context, req *connect.Request[agenv1.CreateApiTokenRequest]) (*connect.Response[agenv1.CreateApiTokenResponse], error) {
	if req.Msg.Name == "" || len(req.Msg.Scopes) == 0 {
		return nil, invalid("name and at least one scope are required")
	}
	for _, s := range req.Msg.Scopes {
		if !validScopes[s] {
			return nil, invalid("unknown scope " + s)
		}
	}
	t, secret, err := h.Store.CreateAPIToken(ctx, req.Msg.Name, req.Msg.Scopes, req.Msg.Namespaces, int64(req.Msg.TtlSeconds)*1000)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.CreateApiTokenResponse{Token: tokenProto(t), Secret: secret}), nil
}

func tokenProto(t store.APIToken) *agenv1.ApiToken {
	return &agenv1.ApiToken{Id: t.ID, Name: t.Name, Scopes: t.Scopes, Namespaces: t.Namespaces, CreatedAt: ms(t.CreatedMs), ExpiresAt: ms(t.ExpiresMs), Revoked: t.Revoked}
}

func (h *Hub) ListApiTokens(ctx context.Context, _ *connect.Request[agenv1.ListApiTokensRequest]) (*connect.Response[agenv1.ListApiTokensResponse], error) {
	list, err := h.Store.ListAPITokens(ctx)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.ListApiTokensResponse{}
	for _, t := range list {
		out.Tokens = append(out.Tokens, tokenProto(t))
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) RevokeNest(ctx context.Context, req *connect.Request[agenv1.RevokeNestRequest]) (*connect.Response[agenv1.RevokeNestResponse], error) {
	if req.Msg.Id == "" {
		return nil, invalid("id is required")
	}
	if err := h.Store.RevokeNest(ctx, req.Msg.Id); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.RevokeNestResponse{}), nil
}

func (h *Hub) RevokeApiToken(ctx context.Context, req *connect.Request[agenv1.RevokeApiTokenRequest]) (*connect.Response[agenv1.RevokeApiTokenResponse], error) {
	if err := h.Store.RevokeAPIToken(ctx, req.Msg.Id); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.RevokeApiTokenResponse{}), nil
}
