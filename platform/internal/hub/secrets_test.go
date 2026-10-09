package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// Platform secrets go in through the API and never come back out of it;
// a Nest gets a value only for a deployment assigned to it whose bundle
// declares that name with source "platform".
func TestPlatformSecrets(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	files := helloFiles(t)
	files["x-agen/secrets.json"] = []byte(`{"API_KEY":{"source":"platform"},"OTHER":{"source":"env"},"ALIAS":{"source":"platform","key":"REAL_KEY"},"ADMIN_ONLY":{"source":"platform"}}`)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: files})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "elsewhere", BundleFiles: files})); err != nil {
		t.Fatal(err)
	}
	idB, b := e.enroll(t, "b", 2, map[string]string{"zone": "b"})
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)

	for name, v := range map[string]string{"API_KEY": "sk-platform-123", "REAL_KEY": "sk-real-456", "OTHER": "sk-other-789"} {
		if _, err := c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: name, Value: v, Deployments: []string{"hello", "elsewhere"}})); err != nil {
			t.Fatal(err)
		}
	}
	// A secret must name who may read it.
	if _, err := c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "NOBODY", Value: "sk-nobody"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("secret without deployments: %v", err)
	}
	// Declared by the bundle but granted to another deployment only: an
	// operator deploying a bundle that declares it gets nothing.
	if _, err := c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "ADMIN_ONLY", Value: "sk-admin-only", Deployments: []string{"billing"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "TINY", Value: "ab", Deployments: []string{"hello"}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("too-short secret: %v", err)
	}
	ls, err := c.ListSecrets(ctx, connect.NewRequest(&agenv1.ListSecretsRequest{}))
	if err != nil || len(ls.Msg.Secrets) != 4 {
		t.Fatalf("list: %v %v", ls, err)
	}
	// Setting secrets is an admin action.
	viewer, _ := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "ops", Scopes: []string{ScopeOperator}}))
	if _, err := e.client(viewer.Msg.Secret).SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "API_KEY", Value: "hijacked", Deployments: []string{"hello"}})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("operator set a secret: %v", err)
	}

	get := func(dep, name string) (string, error) {
		r, err := b.GetPlatformSecret(ctx, connect.NewRequest(&agenv1.GetPlatformSecretRequest{NestId: idB, Ref: ref("", dep), Name: name}))
		if err != nil {
			return "", err
		}
		return r.Msg.Value, nil
	}
	if v, err := get("hello", "API_KEY"); err != nil || v != "sk-platform-123" {
		t.Fatalf("declared secret: %q %v", v, err)
	}
	if v, err := get("hello", "REAL_KEY"); err != nil || v != "sk-real-456" {
		t.Fatalf("declared via key: %q %v", v, err)
	}
	for _, c := range []struct{ dep, name string }{
		{"hello", "OTHER"}, // declared, but source env
		{"hello", "UNDECLARED"},
		{"hello", "ADMIN_ONLY"},  // declared, but granted to billing only
		{"elsewhere", "API_KEY"}, // not assigned to this nest
	} {
		if _, err := get(c.dep, c.name); code(err) != connect.CodePermissionDenied {
			t.Fatalf("%s/%s: %v", c.dep, c.name, err)
		}
	}
	if _, err := c.DeleteSecret(ctx, connect.NewRequest(&agenv1.DeleteSecretRequest{Name: "API_KEY"})); err != nil {
		t.Fatal(err)
	}
	if _, err := get("hello", "API_KEY"); code(err) != connect.CodeNotFound {
		t.Fatalf("deleted secret: %v", err)
	}
}
