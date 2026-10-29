package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prjvvl/agen/platform/internal/mcpserver"
)

// The MCP endpoint negotiates its protocol version with the client's
// initialize params, instead of always answering with its latest.
// features: tools.mcp
func TestMCPEndpointNegotiatesProtocolVersion(t *testing.T) {
	e := setup(t)
	path, api := e.hub.Handler()
	mux := http.NewServeMux()
	mux.Handle(path, api)
	mcpH, err := mcpserver.Handler(mux)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mcpH)
	defer srv.Close()
	initialize := func(params any) string {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params})
		resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v: %s", err, raw)
		}
		return out["result"].(map[string]any)["protocolVersion"].(string)
	}
	if got := initialize(map[string]any{"protocolVersion": "2025-06-18"}); got != "2025-06-18" {
		t.Fatalf("an older, still-supported client: got %q", got)
	}
	if got := initialize(map[string]any{"protocolVersion": "2020-01-01"}); got != mcpserver.ProtocolVersion {
		t.Fatalf("an unknown version falls back to the latest: got %q, want %q", got, mcpserver.ProtocolVersion)
	}
	if got := initialize(map[string]any{}); got != mcpserver.ProtocolVersion {
		t.Fatalf("no version given: got %q, want %q", got, mcpserver.ProtocolVersion)
	}
}
