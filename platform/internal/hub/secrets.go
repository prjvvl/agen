package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

func (h *Hub) SetSecret(ctx context.Context, req *connect.Request[agenv1.SetSecretRequest]) (*connect.Response[agenv1.SetSecretResponse], error) {
	ns := nsOr(req.Msg.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	if !secretName.MatchString(req.Msg.Name) {
		return nil, invalid("invalid secret name")
	}
	if len(req.Msg.Value) < 4 || len(req.Msg.Value) > 64<<10 {
		return nil, invalid("a secret value must be 4 bytes to 64 KiB (short values cannot be redacted safely)")
	}
	if len(req.Msg.Deployments) == 0 {
		return nil, invalid("name the deployments allowed to read the secret (agen secret set NAME --for DEPLOYMENT)")
	}
	for _, d := range req.Msg.Deployments {
		if d == "" {
			return nil, invalid("empty deployment name")
		}
	}
	if err := h.Store.SetPlatformSecret(ctx, ns, req.Msg.Name, req.Msg.Value, req.Msg.Deployments...); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.SetSecretResponse{}), nil
}

func (h *Hub) ListSecrets(ctx context.Context, req *connect.Request[agenv1.ListSecretsRequest]) (*connect.Response[agenv1.ListSecretsResponse], error) {
	ns := nsOr(req.Msg.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	list, err := h.Store.ListPlatformSecrets(ctx, ns)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.ListSecretsResponse{}
	for _, s := range list {
		out.Secrets = append(out.Secrets, &agenv1.SecretInfo{Namespace: s.Namespace, Name: s.Name, UpdatedAt: ms(s.UpdatedMs), Deployments: s.Deployments})
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) DeleteSecret(ctx context.Context, req *connect.Request[agenv1.DeleteSecretRequest]) (*connect.Response[agenv1.DeleteSecretResponse], error) {
	ns := nsOr(req.Msg.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	if err := h.Store.DeletePlatformSecret(ctx, ns, req.Msg.Name); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.DeleteSecretResponse{}), nil
}

// declaredPlatformSecret reports whether a deployment's bundle declares key
// (the lookup key: "key", else the name) with source "platform".
func declaredPlatformSecret(files map[string][]byte, key string) bool {
	var decl map[string]struct {
		Source string `json:"source"`
		Key    string `json:"key"`
	}
	if json.Unmarshal(files["x-agen/secrets.json"], &decl) != nil {
		return false
	}
	for name, d := range decl {
		lookup := d.Key
		if lookup == "" {
			lookup = name
		}
		if d.Source == "platform" && lookup == key {
			return true
		}
	}
	return false
}

// GetPlatformSecret gives a Nest a secret for one of its deployments, if
// that deployment's current definition declares it.
func (n *NestAPI) GetPlatformSecret(ctx context.Context, req *connect.Request[agenv1.GetPlatformSecretRequest]) (*connect.Response[agenv1.GetPlatformSecretResponse], error) {
	if err := requireNest(ctx, req.Msg.NestId); err != nil {
		return nil, err
	}
	ns, dep := nsOr(req.Msg.GetRef().GetNamespace()), req.Msg.GetRef().GetName()
	if !n.assigned(ctx, req.Msg.NestId, ns, dep) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s/%s is not assigned to this nest", ns, dep))
	}
	d, err := n.hub.Store.GetDeployment(ctx, ns, dep)
	if err != nil {
		return nil, connectErr(err)
	}
	def, err := n.hub.Store.GetDefinition(ctx, d.DefinitionDigest)
	if err != nil {
		return nil, connectErr(err)
	}
	if !declaredPlatformSecret(def.Files, req.Msg.Name) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s/%s does not declare platform secret %q", ns, dep, req.Msg.Name))
	}
	// The secret must also name this deployment: declaring it in a bundle
	// (which any operator can deploy) is not enough.
	allowed, err := n.hub.Store.PlatformSecretDeployments(ctx, ns, req.Msg.Name)
	if err == nil && !slices.Contains(allowed, dep) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("secret %q is not granted to %s/%s (agen secret set --for)", req.Msg.Name, ns, dep))
	}
	v, err := n.hub.Store.GetPlatformSecret(ctx, ns, req.Msg.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("secret %q is not set in namespace %s (agen secret set)", req.Msg.Name, ns))
	}
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.GetPlatformSecretResponse{Value: v}), nil
}
