package apidesc

import (
	"encoding/json"
	"testing"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
)

func TestEveryHubRPCBecomesATool(t *testing.T) {
	sd, err := Service(agenv1connect.HubServiceName)
	if err != nil {
		t.Fatal(err)
	}
	tools := Tools(sd)
	if got, want := len(tools), sd.Methods().Len(); got != want {
		t.Fatalf("tools=%d methods=%d", got, want)
	}
	byName := map[string]Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	scale, ok := byName["scale_deployment"]
	if !ok {
		t.Fatalf("missing scale_deployment in %v", byName)
	}
	if scale.Procedure != agenv1connect.HubServiceScaleDeploymentProcedure {
		t.Fatalf("procedure %q", scale.Procedure)
	}
	props := scale.InputSchema["properties"].(map[string]any)
	if _, ok := props["desired"]; !ok {
		t.Fatalf("schema lacks desired: %v", props)
	}
	ref := props["ref"].(map[string]any)["properties"].(map[string]any)
	if _, ok := ref["namespace"]; !ok {
		t.Fatalf("nested ref schema missing namespace: %v", ref)
	}
	// Comments from the proto flow into descriptions.
	if byName["list_nests"].Description == "" || byName["create_deployment"].Description == "CreateDeployment" {
		t.Fatalf("descriptions not taken from proto comments: %+v", byName["create_deployment"].Description)
	}
	if _, err := json.Marshal(tools); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaMatchesProtoJSONNames(t *testing.T) {
	sd, _ := Service(agenv1connect.HubServiceName)
	for _, tl := range Tools(sd) {
		if tl.Name == "submit_task" {
			props := tl.InputSchema["properties"].(map[string]any)
			if _, ok := props["idempotencyKey"]; !ok {
				t.Fatalf("expected protojson lowerCamel name idempotencyKey, got %v", props)
			}
		}
	}
	// Generated Go types and the embedded descriptors describe the same message.
	if (&agenv1.SubmitTaskRequest{}).ProtoReflect().Descriptor().Fields().Len() != 3 {
		t.Fatal("SubmitTaskRequest drifted from embedded descriptors")
	}
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{"ListNests": "list_nests", "GetTrace": "get_trace", "A": "a"} {
		if got := SnakeCase(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
