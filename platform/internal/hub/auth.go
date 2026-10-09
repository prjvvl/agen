package hub

import (
	"context"
	"crypto/subtle"
	"errors"
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
	ID         string
	Scopes     map[string]bool
	Namespaces []string // empty = all
	// NestID is set for a Nest's own token (NestService only).
	NestID string
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
	p := Principal{ID: "token:" + t.ID, Scopes: map[string]bool{}, Namespaces: t.Namespaces}
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

// procedureScope maps each Hub RPC to the scope it requires.
func procedureScope(procedure string) string {
	name := procedure[strings.LastIndex(procedure, "/")+1:]
	switch name {
	case "ListDeployments", "GetDeployment", "ListInstances", "ListNests", "GetTask", "ListTasks", "Resolve",
		"ListDefinitions", "GetDefinition", "ListTriggerEvents", "ListRuns", "GetTrace", "GetLogs":
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
			if need := procedureScope(req.Spec().Procedure); !p.Can(need) {
				return nil, connect.NewError(connect.CodePermissionDenied, errors.New("requires scope "+need))
			}
			return next(WithPrincipal(ctx, p), req)
		}
	}
}

// requireNamespace fails unless the caller may act in ns.
func requireNamespace(ctx context.Context, ns string) error {
	if !PrincipalFrom(ctx).AllowsNamespace(ns) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("namespace "+ns+" is not allowed for this token"))
	}
	return nil
}
