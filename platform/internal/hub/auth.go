package hub

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"

	"github.com/prjvvl/agen/platform/internal/store"
)

// Scopes (docs/architecture.md §10). admin implies every scope; operator and
// approver imply viewer.
const (
	ScopeViewer   = "viewer"
	ScopeOperator = "operator"
	ScopeApprover = "approver"
	ScopeAdmin    = "admin"
)

var validScopes = map[string]bool{ScopeViewer: true, ScopeOperator: true, ScopeApprover: true, ScopeAdmin: true}

// Principal is an authenticated caller.
type Principal struct {
	// ID is stable and used as the approval "decided_by" / "requested_by".
	ID string
	// Name is the token's name ("" for the admin token).
	Name       string
	Scopes     map[string]bool
	Namespaces []string // empty = all
	// NestID is set for a Nest's own token (NestService only).
	NestID string
	// OnBehalf marks a token that acts for the submitter of a task; once
	// resolved for a call, ActingFor names the token it acted through.
	OnBehalf  bool
	ActingFor string
}

// Can reports whether the principal holds scope (directly or implied).
func (p Principal) Can(scope string) bool {
	if p.Scopes[ScopeAdmin] {
		return true
	}
	if scope == ScopeViewer && (p.Scopes[ScopeOperator] || p.Scopes[ScopeApprover]) {
		return true
	}
	return p.Scopes[scope]
}

// AllowsNamespace reports whether the principal may act in ns.
func (p Principal) AllowsNamespace(ns string) bool {
	if len(p.Namespaces) == 0 {
		return true
	}
	for _, n := range p.Namespaces {
		if n == ns {
			return true
		}
	}
	return false
}

type principalKey struct{}

// PrincipalFrom returns the authenticated caller.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// WithPrincipal attaches a principal (used by in-process callers such as MCP).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Auth authenticates bearer tokens: the configured admin token, or an API
// token stored (hashed) in the store.
type Auth struct {
	AdminToken string
	Store      *store.Store
}

// Authenticate resolves a bearer secret to a principal.
func (a *Auth) Authenticate(ctx context.Context, secret string) (Principal, error) {
	if secret == "" {
		return Principal{}, errors.New("missing bearer token")
	}
	if a.AdminToken != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(a.AdminToken)) == 1 {
		return Principal{ID: "admin", Scopes: map[string]bool{ScopeAdmin: true}}, nil
	}
	t, err := a.Store.LookupAPIToken(ctx, secret)
	if err != nil {
		return Principal{}, errors.New("invalid or revoked token")
	}
	p := Principal{ID: "token:" + t.ID, Name: t.Name, Scopes: map[string]bool{}, Namespaces: t.Namespaces, OnBehalf: t.OnBehalf}
	for _, s := range t.Scopes {
		p.Scopes[s] = true
	}
	if p.Scopes[ScopeNest] && strings.HasPrefix(t.Name, nestTokenPrefix) {
		p.NestID = strings.TrimPrefix(t.Name, nestTokenPrefix)
	}
	return p, nil
}

func bearer(h interface{ Get(string) string }) string {
	v := h.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// RequiredScope is the scope a HubService method (e.g. "SubmitTask") needs.
func RequiredScope(method string) string { return procedureScope("/" + method) }

// procedureScope maps each Hub RPC to the scope it requires.
func procedureScope(procedure string) string {
	name := procedure[strings.LastIndex(procedure, "/")+1:]
	switch name {
	case "ListDeployments", "GetDeployment", "ListInstances", "ListNests", "GetTask", "ListTasks", "Resolve",
		"ListDefinitions", "GetDefinition", "ListTriggerEvents", "ListRuns", "GetTrace", "GetLogs", "WhoAmI", "GetBundleGuide",
		"GetTranscript", "GetMetrics", "ListTemplates":
		return ScopeViewer
	case "CreateDeployment", "UpdateDeployment", "ScaleDeployment", "DeleteDeployment", "PauseDeployment", "SubmitTask", "CancelTask", "RequestWake",
		"CreateWebhookSecret":
		return ScopeOperator
	case "ListApprovals", "DecideApproval":
		return ScopeApprover
	default: // tokens, join tokens and anything new: admin until classified
		return ScopeAdmin
	}
}

// Interceptor authenticates every call and enforces the procedure's scope.
func (a *Auth) Interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			p, err := a.Authenticate(ctx, bearer(req.Header()))
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			if p.OnBehalf {
				if p, err = a.actFor(ctx, p, req.Header().Get(TaskHeader)); err != nil {
					return nil, connect.NewError(connect.CodePermissionDenied, err)
				}
			}
			if need := procedureScope(req.Spec().Procedure); !p.Can(need) {
				return nil, connect.NewError(connect.CodePermissionDenied, scopeError(req.Spec().Procedure, need, p))
			}
			return next(WithPrincipal(ctx, p), req)
		}
	}
}

func scopeError(procedure, need string, p Principal) error {
	have := make([]string, 0, len(p.Scopes))
	for s := range p.Scopes {
		have = append(have, s)
	}
	sort.Strings(have)
	held := "none"
	if len(have) > 0 {
		held = strings.Join(have, ", ")
	}
	return fmt.Errorf("%s requires the %s scope; this token has: %s (create one with: agen token create --scope %s)",
		procedure[strings.LastIndex(procedure, "/")+1:], need, held, need)
}

// TaskHeader names the task an on-behalf token works for. The MCP endpoint
// sets it from the call's _meta "agen/taskId", which the engine fills in.
const TaskHeader = "X-Agen-Task"

// actFor resolves an on-behalf token to the principal it acts for: the
// submitter of the unfinished task it works on, limited to the scopes and
// namespaces both hold. Admin powers are never passed on.
func (a *Auth) actFor(ctx context.Context, tok Principal, taskID string) (Principal, error) {
	if taskID == "" {
		return Principal{}, errors.New("this token acts for the person whose task an agent works on, so it only works from an agent's MCP calls (they name the task in _meta agen/taskId)")
	}
	t, err := a.Store.GetTask(ctx, taskID)
	if err != nil || t.Terminal() {
		return Principal{}, fmt.Errorf("task %s is not running", taskID)
	}
	if !tok.AllowsNamespace(t.Namespace) {
		return Principal{}, fmt.Errorf("task %s is in namespace %s, which this token may not use", taskID, t.Namespace)
	}
	var user Principal
	switch id := t.SubmittedBy; {
	case id == "admin":
		user = Principal{ID: "admin", Scopes: map[string]bool{ScopeAdmin: true}}
	case strings.HasPrefix(id, "token:"):
		ut, err := a.Store.GetAPIToken(ctx, strings.TrimPrefix(id, "token:"))
		if err != nil || ut.Revoked || (ut.ExpiresMs > 0 && ut.ExpiresMs < store.NowMs()) || ut.OnBehalf {
			return Principal{}, fmt.Errorf("the token that submitted task %s is no longer valid", taskID)
		}
		user = Principal{ID: id, Name: ut.Name, Scopes: map[string]bool{}, Namespaces: ut.Namespaces}
		for _, s := range ut.Scopes {
			user.Scopes[s] = true
		}
	default:
		return Principal{}, fmt.Errorf("task %s was not submitted by a person (%s), so there is nobody to act for", taskID, id)
	}
	out := Principal{ID: user.ID, Name: user.Name, Scopes: map[string]bool{}, ActingFor: tok.ID}
	for _, s := range []string{ScopeViewer, ScopeOperator, ScopeApprover} {
		if tok.Can(s) && user.Can(s) {
			out.Scopes[s] = true
		}
	}
	switch {
	case len(tok.Namespaces) == 0:
		out.Namespaces = user.Namespaces
	case len(user.Namespaces) == 0:
		out.Namespaces = tok.Namespaces
	default:
		for _, ns := range tok.Namespaces {
			if user.AllowsNamespace(ns) {
				out.Namespaces = append(out.Namespaces, ns)
			}
		}
		if len(out.Namespaces) == 0 {
			return Principal{}, errors.New("this token and the person it acts for share no namespace")
		}
	}
	return out, nil
}

// requireNamespace fails unless the caller may act in ns.
func requireNamespace(ctx context.Context, ns string) error {
	if !PrincipalFrom(ctx).AllowsNamespace(ns) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("namespace "+ns+" is not allowed for this token"))
	}
	return nil
}
