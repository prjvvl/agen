package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/testutil"
)

// The agent card advertises the current A2A protocol revision.
// features: delegation
func TestAgentCardAdvertisesA2AVersion(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	if _, err := k.c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: testutil.BundleFiles(t, "hello")})); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(k.gwURL + "/a2a/default/hello/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode: %v: %s", err, raw)
	}
	if card["protocolVersion"] != A2AVersion {
		t.Fatalf("protocolVersion: %v, want %q", card["protocolVersion"], A2AVersion)
	}
}
