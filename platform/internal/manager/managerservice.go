package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/store"
)

// ManagerService is the Nest-local API agen-host instances use to reach the
// platform (docs/architecture.md §7: hosts never call the Hub). It listens on
// localhost; each instance authenticates with its own token
// (AGEN_MANAGER_TOKEN), which also identifies it.
type managerService struct {
	agenv1connect.UnimplementedManagerServiceHandler
	m *Manager
}

type instanceIDKey struct{}

// startManagerService listens on a localhost port and returns its URL.
func (m *Manager) startManagerService() error {
	addr := m.cfg.ManagerListen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	auth := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			tok := strings.TrimPrefix(req.Header().Get("Authorization"), "Bearer ")
			m.mu.Lock()
			id, ok := m.instanceTokens[tok]
			m.mu.Unlock()
			if tok == "" || !ok {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unknown instance token"))
			}
			return next(context.WithValue(ctx, instanceIDKey{}, id), req)
		}
	})
	path, h := agenv1connect.NewManagerServiceHandler(&managerService{m: m}, connect.WithInterceptors(auth))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	m.managerURL = "http://" + ln.Addr().String()
	if m.cfg.ManagerURL != "" {
		m.managerURL = strings.TrimRight(m.cfg.ManagerURL, "/")
	}
	m.managerSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = m.managerSrv.Serve(ln) }()
	return nil
}

// newInstanceToken registers a token for an instance. Caller holds m.mu.
func (m *Manager) newInstanceToken(instanceID string) string {
	tok := store.NewSecret("agen_inst_")
	m.instanceTokens[tok] = instanceID
	return tok
}

// Resolve resolves a delegation target through the Hub and caches the
// answer, so delegation keeps working from the cache while the Hub is down.
func (s *managerService) Resolve(ctx context.Context, req *connect.Request[agenv1.ResolveRequest]) (*connect.Response[agenv1.ResolveResponse], error) {
	ns, name := req.Msg.GetRef().GetNamespace(), req.Msg.GetRef().GetName()
	if ns == "" {
		ns = "default"
	}
	// The calling instance's deployment: the Hub names it in the call token.
	id, _ := ctx.Value(instanceIDKey{}).(string)
	s.m.mu.Lock()
	in := s.m.instances[id]
	s.m.mu.Unlock()
	if in == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("instance is no longer running"))
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := s.m.hub().Resolve(c, connect.NewRequest(&agenv1.ResolveRequest{Ref: &agenv1.DeploymentRef{Namespace: ns, Name: name},
		Caller: &agenv1.DeploymentRef{Namespace: in.ns, Name: in.dep}}))
	if err == nil {
		s.m.rememberResolve(in.ns, in.dep, ns, name, resp.Msg)
		return connect.NewResponse(resp.Msg), nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodePermissionDenied, connect.CodeInvalidArgument:
		return nil, err
	}
	// Hub down: the cached endpoints and call token keep delegation working
	// until the token expires (it is refreshed while the Hub is up).
	if cached, ok := s.m.cachedResolve(in.ns, in.dep, ns, name); ok {
		s.m.cfg.Log.Warn("hub unreachable; resolving from cache", "call", resolveKey(in.ns, in.dep, ns, name), "err", err)
		return connect.NewResponse(cached), nil
	}
	return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("hub unreachable and no valid cached call token for %s/%s: %w", ns, name, err))
}

// RequestApproval raises a durable "ask" approval at the Hub for the calling
// instance's run and holds the call until it is decided or expires. Hub
// outages are waited out; the engine bounds the wait with its approval
// timeout.
func (s *managerService) RequestApproval(ctx context.Context, req *connect.Request[agenv1.RequestApprovalRequest]) (*connect.Response[agenv1.RequestApprovalResponse], error) {
	id, _ := ctx.Value(instanceIDKey{}).(string)
	s.m.mu.Lock()
	in := s.m.instances[id]
	s.m.mu.Unlock()
	if in == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("instance is no longer running"))
	}
	args := &structpb.Struct{}
	if req.Msg.ArgumentsJson != "" {
		if err := args.UnmarshalJSON([]byte(req.Msg.ArgumentsJson)); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	var a *agenv1.Approval
	for a == nil {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		r, err := s.m.hub().CreateApproval(c, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{
			Namespace: in.ns, Deployment: in.dep, RunId: req.Msg.RunId, Tool: req.Msg.Tool, Arguments: args}, TtlSeconds: req.Msg.TtlSeconds,
			InstanceId: id}))
		cancel()
		switch {
		case err == nil:
			a = r.Msg.Approval
		case connect.CodeOf(err) == connect.CodeUnavailable || connect.CodeOf(err) == connect.CodeDeadlineExceeded || connect.CodeOf(err) == connect.CodeUnknown:
			if !sleepCtx(ctx, 2*time.Second) {
				return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
			}
		default:
			return nil, err
		}
	}
	s.m.cfg.Log.Info("approval requested", "approval", a.Id, "instance", id, "tool", a.Tool)
	for a.State == agenv1.ApprovalState_APPROVAL_STATE_PENDING {
		r, err := s.m.hub().GetApproval(ctx, connect.NewRequest(&agenv1.GetApprovalRequest{Id: a.Id, WaitSeconds: 30}))
		if err != nil {
			if ctx.Err() != nil {
				return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
			}
			switch connect.CodeOf(err) {
			case connect.CodeNotFound, connect.CodePermissionDenied, connect.CodeInvalidArgument, connect.CodeUnauthenticated:
				return nil, err // will not get better
			}
			if !sleepCtx(ctx, 2*time.Second) {
				return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
			}
			continue
		}
		a = r.Msg.Approval
	}
	return connect.NewResponse(&agenv1.RequestApprovalResponse{Approval: a}), nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ResolveSecret gives an instance a platform secret of its deployment,
// from the Hub. Values are kept in memory only (never on disk), so an
// instance restarted while the Hub is down can still start.
func (s *managerService) ResolveSecret(ctx context.Context, req *connect.Request[agenv1.ResolveSecretRequest]) (*connect.Response[agenv1.ResolveSecretResponse], error) {
	id, _ := ctx.Value(instanceIDKey{}).(string)
	s.m.mu.Lock()
	in := s.m.instances[id]
	s.m.mu.Unlock()
	if in == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("instance is no longer running"))
	}
	key := in.ns + "/" + in.dep + "/" + req.Msg.Name
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := s.m.hub().GetPlatformSecret(c, connect.NewRequest(&agenv1.GetPlatformSecretRequest{NestId: s.m.nestID,
		Ref: &agenv1.DeploymentRef{Namespace: in.ns, Name: in.dep}, Name: req.Msg.Name}))
	if err == nil {
		s.m.mu.Lock()
		s.m.secrets[key] = resp.Msg.Value
		s.m.mu.Unlock()
		return connect.NewResponse(&agenv1.ResolveSecretResponse{Value: resp.Msg.Value}), nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodePermissionDenied, connect.CodeInvalidArgument:
		return nil, err
	}
	s.m.mu.Lock()
	v, ok := s.m.secrets[key]
	s.m.mu.Unlock()
	if ok {
		return connect.NewResponse(&agenv1.ResolveSecretResponse{Value: v}), nil
	}
	return nil, connect.NewError(connect.CodeUnavailable, err)
}
