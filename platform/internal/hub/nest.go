package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/pki"
	"github.com/prjvvl/agen/platform/internal/store"
)

// ScopeNest is held only by tokens issued to a Nest at enrolment.
const ScopeNest = "nest"

// nestTokenPrefix names a Nest's token: "nest:<nest id>".
const nestTokenPrefix = "nest:"

// NestAPI serves NestService (docs/architecture.md §5): the pull API every
// Manager uses. The Hub never connects into a Nest.
type NestAPI struct {
	hub *Hub
	// WatchPoll is how often WatchAssignments re-reads the store. Polling
	// the store (not in-memory notification) keeps every Hub replica able
	// to serve the stream.
	WatchPoll time.Duration
	// MaxLeaseSeconds bounds lease requests.
	MaxLeaseSeconds int32
}

// Nest returns the NestService implementation over this Hub.
func (h *Hub) Nest() *NestAPI {
	return &NestAPI{hub: h, WatchPoll: 250 * time.Millisecond, MaxLeaseSeconds: 300}
}

var _ agenv1connect.NestServiceHandler = (*NestAPI)(nil)

// Handler mounts NestService. Enroll is authenticated by its join token;
// every other call needs the Nest's own bearer token.
func (n *NestAPI) Handler() (string, http.Handler) {
	path, h := agenv1connect.NewNestServiceHandler(n, connect.WithInterceptors(&nestAuth{auth: n.hub.Auth, hub: n.hub}))
	return path, withPeerCert(h)
}

// nestAuth authenticates unary and streaming NestService calls.
type nestAuth struct {
	auth *Auth
	hub  *Hub
}

func (a *nestAuth) check(ctx context.Context, procedure string, h http.Header) (context.Context, error) {
	if strings.HasSuffix(procedure, "/Enroll") {
		return ctx, nil
	}
	if a.hub.CA != nil {
		// mTLS: the client certificate issued at enrolment is the identity.
		n, err := a.hub.Store.NestByCert(ctx, peerCert(ctx))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("NestService requires the client certificate issued at enrolment"))
		}
		return WithPrincipal(ctx, Principal{ID: nestTokenPrefix + n.ID, Scopes: map[string]bool{ScopeNest: true}, NestID: n.ID}), nil
	}
	p, err := a.auth.Authenticate(ctx, bearer(h))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if p.NestID == "" && !p.Can(ScopeAdmin) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("requires a nest token"))
	}
	return WithPrincipal(ctx, p), nil
}

func (a *nestAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := a.check(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *nestAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *nestAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := a.check(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// requireNest fails unless the caller is that Nest (or an admin).
func requireNest(ctx context.Context, nestID string) error {
	p := PrincipalFrom(ctx)
	if nestID == "" {
		return invalid("nest_id is required")
	}
	if p.NestID == nestID || (p.NestID == "" && p.Can(ScopeAdmin)) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("token does not belong to nest "+nestID))
}

// callerNest is the calling Nest's id ("" for an admin).
func callerNest(ctx context.Context) string { return PrincipalFrom(ctx).NestID }

// requireNestTask fails unless the task is currently leased by the caller.
func (n *NestAPI) requireNestTask(ctx context.Context, nestID, taskID string) (store.Task, error) {
	if err := requireNest(ctx, nestID); err != nil {
		return store.Task{}, err
	}
	t, err := n.hub.Store.GetTask(ctx, taskID)
	if err != nil {
		return t, connectErr(err)
	}
	if t.LeaseNestID != nestID {
		return t, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("task %s is not leased by nest %s", taskID, nestID))
	}
	return t, nil
}

func (n *NestAPI) Enroll(ctx context.Context, req *connect.Request[agenv1.EnrollRequest]) (*connect.Response[agenv1.EnrollResponse], error) {
	m := req.Msg
	if m.Name == "" {
		return nil, invalid("name is required")
	}
	if m.Capacity < 0 {
		return nil, invalid("capacity must be >= 0")
	}
	backend := m.Backend
	if backend == "" {
		backend = "native"
	}
	ca := n.hub.CA
	if ca != nil {
		// Checked before the join token is used up.
		if err := pki.CheckCSR(m.CsrPem); err != nil {
			return nil, invalid("this Hub uses mTLS: enrol with a valid csr_pem (" + err.Error() + ")")
		}
	}
	var certPEM string
	var sign func(string) (string, error)
	if ca != nil {
		sign = func(nestID string) (string, error) {
			c, fp, err := ca.SignNestCSR(m.CsrPem, nestID)
			certPEM = c
			return fp, err
		}
	}
	nest, secret, err := n.hub.Store.EnrollNest(ctx, m.JoinToken, store.Nest{Name: m.Name, Backend: backend, Labels: m.Labels,
		Capacity: int(m.Capacity), GatewayURL: m.GatewayUrl}, sign)
	if errors.Is(err, store.ErrForbidden) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("join token is invalid, expired or already used"))
	}
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.EnrollResponse{NestId: nest.ID}
	if ca == nil {
		out.NestToken = secret
		return connect.NewResponse(out), nil
	}
	out.CertificatePem, out.CaPem = certPEM, string(ca.CertPEM)
	return connect.NewResponse(out), nil
}

// RenewCertificate issues a new client certificate to an enrolled Nest,
// authenticated by its current one, and retires the old one.
func (n *NestAPI) RenewCertificate(ctx context.Context, req *connect.Request[agenv1.RenewCertificateRequest]) (*connect.Response[agenv1.RenewCertificateResponse], error) {
	if err := requireNest(ctx, req.Msg.NestId); err != nil {
		return nil, err
	}
	ca := n.hub.CA
	if ca == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this Hub does not use certificates"))
	}
	if err := pki.CheckCSR(req.Msg.CsrPem); err != nil {
		return nil, invalid("csr_pem: " + err.Error())
	}
	certPEM, fp, err := ca.SignNestCSR(req.Msg.CsrPem, req.Msg.NestId)
	if err != nil {
		return nil, connectErr(err)
	}
	// Compare-and-swap on the presented certificate: a Nest revoked in the
	// meantime is not re-activated.
	if err := n.hub.Store.RotateNestCert(ctx, req.Msg.NestId, peerCert(ctx), fp); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("this certificate can no longer be renewed (revoked or already replaced)"))
		}
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.RenewCertificateResponse{CertificatePem: certPEM, CaPem: string(ca.CertPEM)}), nil
}

// assignmentsProto returns a Nest's assignments with the deployment facts a
// Manager needs to start instances.
func (n *NestAPI) assignmentsProto(ctx context.Context, list []store.Assignment) []*agenv1.Assignment {
	out := make([]*agenv1.Assignment, 0, len(list))
	for _, a := range list {
		pa := &agenv1.Assignment{Namespace: a.Namespace, Deployment: a.Deployment, NestId: a.NestID, Count: int32(a.Count),
			DefinitionDigest: a.DefinitionDigest, Generation: a.Generation}
		if d, err := n.hub.Store.GetDeployment(ctx, a.Namespace, a.Deployment); err == nil {
			pa.Kind = d.Kind
			pol := PolicyOf(d)
			pa.MaxConcurrency = pol.Scale.MaxConcurrency
			pa.MaxDelegationDepth = pol.Limits.MaxDelegationDepth
		}
		out = append(out, pa)
	}
	return out
}

func (n *NestAPI) WatchAssignments(ctx context.Context, req *connect.Request[agenv1.WatchAssignmentsRequest], stream *connect.ServerStream[agenv1.WatchAssignmentsResponse]) error {
	if err := requireNest(ctx, req.Msg.NestId); err != nil {
		return err
	}
	var last []store.Assignment
	var revision int64
	first := true
	for {
		if nest, err := n.hub.Store.GetNest(ctx, req.Msg.NestId); err == nil && nest.State == "revoked" {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("nest credentials were revoked"))
		}
		raw, err := n.hub.Store.AssignmentsForNest(ctx, req.Msg.NestId)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return connectErr(err)
		}
		// Assignment rows carry the deployment generation, so any change to
		// kind or concurrency also changes the raw set.
		if first || !sameStoreAssignments(last, raw) {
			cur := n.assignmentsProto(ctx, raw)
			first = false
			revision++
			last = raw
			if err := stream.Send(&agenv1.WatchAssignmentsResponse{Assignments: cur, Revision: revision}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(n.WatchPoll):
		}
	}
}

func (n *NestAPI) ReportStatus(ctx context.Context, req *connect.Request[agenv1.ReportStatusRequest]) (*connect.Response[agenv1.ReportStatusResponse], error) {
	m := req.Msg
	if err := requireNest(ctx, m.NestId); err != nil {
		return nil, err
	}
	if err := n.hub.Store.Heartbeat(ctx, m.NestId, int(m.Capacity), m.GatewayUrl); err != nil {
		return nil, connectErr(err)
	}
	list := make([]store.Instance, 0, len(m.Instances))
	for _, in := range m.Instances {
		if in.Id == "" || in.Deployment == "" {
			return nil, invalid("every instance needs an id and a deployment")
		}
		si := store.Instance{ID: in.Id, Namespace: nsOr(in.Namespace), Deployment: in.Deployment, NestID: m.NestId,
			DefinitionDigest: in.DefinitionDigest, State: InstanceStateName(in.State), Endpoint: in.Endpoint,
			RunningTasks: int(in.RunningTasks), Message: in.Message, Tools: store.InstanceTools{Tools: in.Tools}}
		for _, ts := range in.ToolServers {
			si.Tools.Servers = append(si.Tools.Servers, store.ToolServer{Name: ts.Name, State: ts.State, ToolCount: int(ts.ToolCount)})
		}
		if in.StartedAt != nil {
			si.StartedMs = in.StartedAt.AsTime().UnixMilli()
		}
		list = append(list, si)
	}
	if err := n.hub.Store.ReportInstances(ctx, m.NestId, list); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.ReportStatusResponse{}), nil
}

func (n *NestAPI) leaseMs(sec int32) int64 {
	if sec <= 0 {
		sec = 30
	}
	if sec > n.MaxLeaseSeconds {
		sec = n.MaxLeaseSeconds
	}
	return int64(sec) * 1000
}

func (n *NestAPI) LeaseTasks(ctx context.Context, req *connect.Request[agenv1.LeaseTasksRequest]) (*connect.Response[agenv1.LeaseTasksResponse], error) {
	m := req.Msg
	if err := requireNest(ctx, m.NestId); err != nil {
		return nil, err
	}
	ns, name := nsOr(m.GetRef().GetNamespace()), m.GetRef().GetName()
	// A Nest may only take work for deployments assigned to it.
	as, err := n.hub.Store.AssignmentsForNest(ctx, m.NestId)
	if err != nil {
		return nil, connectErr(err)
	}
	assigned := false
	for _, a := range as {
		assigned = assigned || (a.Namespace == ns && a.Deployment == name)
	}
	if !assigned {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/%s is not assigned to nest %s", ns, name, m.NestId))
	}
	if d, err := n.hub.Store.GetDeployment(ctx, ns, name); err == nil {
		if _, exhausted, err := n.hub.spentToday(ctx, d); err == nil && exhausted {
			return connect.NewResponse(&agenv1.LeaseTasksResponse{}), nil // no new work today
		}
		if d.Paused {
			return connect.NewResponse(&agenv1.LeaseTasksResponse{}), nil
		}
	}
	max := int(m.Max)
	if max > 100 {
		max = 100
	}
	list, err := n.hub.Store.LeaseTasks(ctx, m.NestId, ns, name, max, n.leaseMs(m.LeaseSeconds))
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.LeaseTasksResponse{}
	for _, t := range list {
		out.Tasks = append(out.Tasks, TaskProto(t))
	}
	if len(list) > 0 {
		_ = n.hub.Store.TouchActivity(ctx, ns, name)
	}
	return connect.NewResponse(out), nil
}

func (n *NestAPI) ExtendLease(ctx context.Context, req *connect.Request[agenv1.ExtendLeaseRequest]) (*connect.Response[agenv1.ExtendLeaseResponse], error) {
	m := req.Msg
	if _, err := n.requireNestTask(ctx, m.NestId, m.TaskId); err != nil {
		return nil, err
	}
	if m.InstanceId != "" {
		if err := n.hub.Store.StartTask(ctx, m.TaskId, m.LeaseId, m.InstanceId); err != nil {
			return nil, connectErr(err)
		}
	}
	if err := n.hub.Store.ExtendLease(ctx, m.TaskId, m.LeaseId, n.leaseMs(m.LeaseSeconds)); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.ExtendLeaseResponse{}), nil
}

func (n *NestAPI) CompleteTask(ctx context.Context, req *connect.Request[agenv1.CompleteTaskRequest]) (*connect.Response[agenv1.CompleteTaskResponse], error) {
	m := req.Msg
	if _, err := n.requireNestTask(ctx, m.NestId, m.TaskId); err != nil {
		return nil, err
	}
	if err := n.hub.Store.CompleteTask(ctx, m.TaskId, m.LeaseId, m.Success, m.Output, m.Error, m.RunId, m.InstanceId); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.CompleteTaskResponse{}), nil
}

func (n *NestAPI) ReleaseTask(ctx context.Context, req *connect.Request[agenv1.ReleaseTaskRequest]) (*connect.Response[agenv1.ReleaseTaskResponse], error) {
	m := req.Msg
	if _, err := n.requireNestTask(ctx, m.NestId, m.TaskId); err != nil {
		return nil, err
	}
	if err := n.hub.Store.ReleaseTask(ctx, m.TaskId, m.LeaseId, m.NotStarted); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.ReleaseTaskResponse{}), nil
}

// ---- approvals (raised by instances through their Manager) ----

func (n *NestAPI) CreateApproval(ctx context.Context, req *connect.Request[agenv1.CreateApprovalRequest]) (*connect.Response[agenv1.CreateApprovalResponse], error) {
	a := req.Msg.GetApproval()
	if a == nil || a.Deployment == "" || a.Tool == "" {
		return nil, invalid("approval with deployment and tool is required")
	}
	ns := nsOr(a.Namespace)
	if nest := callerNest(ctx); nest != "" && !n.assigned(ctx, nest, ns, a.Deployment) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s/%s is not assigned to this nest", ns, a.Deployment))
	}
	// The approval must belong to a real run of that deployment...
	run, err := n.hub.Store.GetRun(ctx, a.RunId)
	if err != nil || run.Namespace != ns || run.Deployment != a.Deployment {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run %q is not a run of %s/%s", a.RunId, ns, a.Deployment))
	}
	// ...owned by the instance asking: an instance cannot raise asks (and so
	// requesters) on other runs of its deployment.
	if callerNest(ctx) != "" {
		owner, err := n.hub.Store.RunOwner(ctx, a.RunId)
		if err != nil || req.Msg.InstanceId == "" || owner != req.Msg.InstanceId {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("run %q is not owned by instance %q", a.RunId, req.Msg.InstanceId))
		}
	}
	// Canonical JSON (sorted keys), so a repeated request matches its
	// pending approval; protojson output is deliberately unstable.
	args := []byte("{}")
	if a.Arguments != nil {
		if b, err := json.Marshal(a.Arguments.AsMap()); err == nil {
			args = b
		}
	}
	ttl := int64(req.Msg.TtlSeconds) * 1000
	if ttl <= 0 {
		ttl = int64(time.Hour / time.Millisecond)
	}
	// A resumed run asking the same question gets the same approval.
	if prev, err := n.hub.Store.PendingApproval(ctx, run.ID, a.Tool, args); err == nil {
		return connect.NewResponse(&agenv1.CreateApprovalResponse{Approval: ApprovalProto(prev)}), nil
	}
	// Set by the Hub, never by the caller (a caller could otherwise name a
	// person and so bar them from deciding): the principal that submitted
	// the run's task, so nobody approves work they asked for themselves;
	// else the nest.
	requestedBy := PrincipalFrom(ctx).ID
	if nest := callerNest(ctx); nest != "" {
		requestedBy = nestTokenPrefix + nest
	}
	by, err := n.submitterOf(ctx, run)
	if err != nil {
		// A requester record that cannot be verified must not fall back to
		// the Nest: that would let the real requester approve their own ask.
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if by != "" {
		requestedBy = by
	}
	id := store.NewID()
	out, err := n.hub.Store.CreateApproval(ctx, store.Approval{ID: id, Namespace: ns, Deployment: a.Deployment, RunID: a.RunId, Tool: a.Tool,
		Arguments: args, RequestedBy: requestedBy}, ttl)
	if err != nil {
		return nil, connectErr(err)
	}
	if out.ID == id {
		go n.hub.notifyApproval(context.WithoutCancel(ctx), out)
	}
	return connect.NewResponse(&agenv1.CreateApprovalResponse{Approval: ApprovalProto(out)}), nil
}

// submitterOf finds the principal behind a run: its task's submitter, or,
// for a delegated (A2A) run with no Hub task, the submitter of its root run's
// task, or the user of the Hub-signed call token that started it over A2A.
func (n *NestAPI) submitterOf(ctx context.Context, run store.RunRow) (string, error) {
	for _, r := range []store.RunRow{run, n.rootRun(ctx, run)} {
		if r.TaskID != "" {
			if t, err := n.hub.Store.GetTask(ctx, r.TaskID); err == nil && t.SubmittedBy != "" {
				return t.SubmittedBy, nil
			}
		}
		// A run started over A2A by a person: the recorded call token must
		// verify with a Hub key and be for this run's deployment (hosts
		// write runs, but cannot sign). A record that does not verify is an
		// error, never "no requester".
		if r.RequestedBy != "" {
			user := n.hub.tokenUser(ctx, r.RequestedBy, r.Namespace+"/"+r.Deployment)
			if user == "" {
				return "", fmt.Errorf("run %s has a requester record that does not verify", r.ID)
			}
			return user, nil
		}
	}
	return "", nil
}

func (n *NestAPI) rootRun(ctx context.Context, run store.RunRow) store.RunRow {
	if run.RootRunID == "" || run.RootRunID == run.ID {
		return store.RunRow{}
	}
	r, _ := n.hub.Store.GetRun(ctx, run.RootRunID)
	return r
}

func (n *NestAPI) assigned(ctx context.Context, nestID, ns, deployment string) bool {
	as, _ := n.hub.Store.AssignmentsForNest(ctx, nestID)
	for _, a := range as {
		if a.Namespace == ns && a.Deployment == deployment {
			return true
		}
	}
	return false
}

// eligible reports whether a nest may run a deployment: it is assigned, or
// the nest is active and matches the deployment's placement labels (a
// Gateway waking a deployment that has no instances yet).
func (n *NestAPI) eligible(ctx context.Context, nestID, ns, name string) (bool, error) {
	if n.assigned(ctx, nestID, ns, name) {
		return true, nil
	}
	d, err := n.hub.Store.GetDeployment(ctx, ns, name)
	if err != nil {
		return false, err
	}
	nest, err := n.hub.Store.GetNest(ctx, nestID)
	if err != nil {
		return false, err
	}
	return nestMatches(nest, d), nil
}

// nestMatches reports whether an active nest satisfies a deployment's
// placement labels.
func nestMatches(n store.Nest, d store.Deployment) bool {
	if n.State != "active" {
		return false
	}
	for k, v := range PolicyOf(d).Placement.GetLabels() {
		if n.Labels[k] != v {
			return false
		}
	}
	return true
}

func (n *NestAPI) GetApproval(ctx context.Context, req *connect.Request[agenv1.GetApprovalRequest]) (*connect.Response[agenv1.GetApprovalResponse], error) {
	wait := time.Duration(req.Msg.WaitSeconds) * time.Second
	if wait > 60*time.Second {
		wait = 60 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		_, _ = n.hub.Store.ExpireApprovals(ctx)
		a, err := n.hub.Store.GetApproval(ctx, req.Msg.Id)
		if err != nil {
			return nil, connectErr(err)
		}
		if nest := callerNest(ctx); nest != "" && !n.assigned(ctx, nest, a.Namespace, a.Deployment) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("approval belongs to a deployment not assigned to this nest"))
		}
		if a.State != "pending" || time.Now().After(deadline) {
			return connect.NewResponse(&agenv1.GetApprovalResponse{Approval: ApprovalProto(a)}), nil
		}
		select {
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		case <-time.After(n.hub.Poll):
		}
	}
}

func (n *NestAPI) ReportActivity(ctx context.Context, req *connect.Request[agenv1.ReportActivityRequest]) (*connect.Response[agenv1.ReportActivityResponse], error) {
	ns, name := nsOr(req.Msg.GetRef().GetNamespace()), req.Msg.GetRef().GetName()
	if nest := callerNest(ctx); nest != "" {
		ok, err := n.eligible(ctx, nest, ns, name)
		if err != nil {
			return nil, connectErr(err)
		}
		if !ok {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("nest may not run %s/%s", ns, name))
		}
	}
	if _, err := n.hub.Wake(ctx, ns, name); err != nil {
		return nil, connectErr(err)
	}
	if req.Msg.Saturated {
		if err := n.hub.scaleUpForDemand(ctx, ns, name); err != nil {
			return nil, connectErr(err)
		}
	}
	return connect.NewResponse(&agenv1.ReportActivityResponse{}), nil
}

func (n *NestAPI) GetDefinition(ctx context.Context, req *connect.Request[agenv1.GetDefinitionRequest]) (*connect.Response[agenv1.GetDefinitionResponse], error) {
	if nest := callerNest(ctx); nest != "" {
		as, err := n.hub.Store.AssignmentsForNest(ctx, nest)
		if err != nil {
			return nil, connectErr(err)
		}
		ok := false
		for _, a := range as {
			ok = ok || a.DefinitionDigest == req.Msg.Digest
		}
		if !ok {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("definition is not assigned to this nest"))
		}
	}
	d, err := n.hub.Store.GetDefinition(ctx, req.Msg.Digest)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.GetDefinitionResponse{Definition: &agenv1.Definition{Name: d.Name, Digest: d.Digest, Files: d.Files, CreatedAt: ms(d.CreatedMs)}}), nil
}

// Resolve lets a Manager resolve a delegation target for one of its
// instances. Delegation stays within the fleet; any deployment may be
// resolved.
func (n *NestAPI) Resolve(ctx context.Context, req *connect.Request[agenv1.ResolveRequest]) (*connect.Response[agenv1.ResolveResponse], error) {
	ns, name := nsOr(req.Msg.GetRef().GetNamespace()), req.Msg.GetRef().GetName()
	out, err := n.hub.resolve(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	// A Manager resolving for one of its instances gets a call token naming
	// that deployment as the caller, if the deployment really runs there.
	if c := req.Msg.GetCaller(); c.GetName() != "" {
		cns := nsOr(c.GetNamespace())
		if cns != ns {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("delegation stays within a namespace (%s -> %s)", cns, ns))
		}
		nest := callerNest(ctx)
		if nest == "" || !n.assigned(ctx, nest, cns, c.GetName()) {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s/%s is not assigned to this nest", cns, c.GetName()))
		}
		if err := n.hub.issueCallToken(ctx, out, "agent:"+cns+"/"+c.GetName(), nest, ns, name, agentCallTokenTTL); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(out), nil
}

// SubmitChildTask queues a delegated task on behalf of a task this Nest is
// running. The Hub derives the lineage from that parent task (depth = parent
// depth + 1, same root run), so a Nest cannot reset the depth to escape the
// target's max_delegation_depth. Delegation stays within the parent's
// namespace.
func (n *NestAPI) SubmitChildTask(ctx context.Context, req *connect.Request[agenv1.SubmitChildTaskRequest]) (*connect.Response[agenv1.SubmitChildTaskResponse], error) {
	m := req.Msg
	if m.ParentTaskId == "" {
		return nil, invalid("parent_task_id is required")
	}
	parent, err := n.requireNestTask(ctx, m.NestId, m.ParentTaskId)
	if err != nil {
		return nil, err
	}
	if parent.Terminal() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("parent task %s already finished", parent.ID))
	}
	ns, name := nsOr(m.GetRef().GetNamespace()), m.GetRef().GetName()
	if ns != parent.Namespace {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("delegation from namespace %s to %s is not allowed", parent.Namespace, ns))
	}
	d, err := n.hub.Store.GetDeployment(ctx, ns, name)
	if err != nil {
		return nil, connectErr(err)
	}
	parentRun := m.ParentRunId
	if parentRun != "" {
		run, err := n.hub.Store.GetRun(ctx, parentRun)
		if err != nil || run.TaskID != parent.ID {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run %q is not the parent task's run", parentRun))
		}
	}
	root := parent.RootRunID
	if root == "" {
		root = parentRun
	}
	depth := parent.Depth + 1
	if max := int(PolicyOf(d).Limits.MaxDelegationDepth); max > 0 && depth > max {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("delegation depth %d exceeds max_delegation_depth %d of %s/%s", depth, max, ns, name))
	}
	t, err := n.hub.Store.SubmitTask(ctx, store.Task{Namespace: ns, Deployment: name, Input: m.Input, Source: "a2a", IdempotencyKey: m.IdempotencyKey,
		ParentTaskID: parent.ID, ParentRunID: parentRun, RootRunID: root, Depth: depth, Traceparent: m.Traceparent,
		SubmittedBy: parent.SubmittedBy})
	if err != nil {
		return nil, connectErr(err)
	}
	_ = n.hub.Store.TouchActivity(ctx, ns, name)
	return connect.NewResponse(&agenv1.SubmitChildTaskResponse{Task: TaskProto(t)}), nil
}
