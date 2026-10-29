package mcpserver

import (
	"encoding/json"
	"testing"
)

// negotiatedVersion echoes a client's requested protocolVersion when this
// server still supports it, so a client on an older revision whose
// initialize/tools shape has not changed is not forced onto the newest one;
// an unknown or missing version falls back to this server's latest.
func TestNegotiatedVersion(t *testing.T) {
	cases := []struct {
		params string
		want   string
	}{
		{`{"protocolVersion":"2025-06-18"}`, "2025-06-18"},
		{`{"protocolVersion":"2025-03-26"}`, "2025-03-26"},
		{`{"protocolVersion":"2025-11-25"}`, ProtocolVersion},
		{`{"protocolVersion":"2024-01-01"}`, ProtocolVersion},
		{`{}`, ProtocolVersion},
		{``, ProtocolVersion},
	}
	for _, c := range cases {
		if got := negotiatedVersion(json.RawMessage(c.params)); got != c.want {
			t.Errorf("negotiatedVersion(%q) = %q, want %q", c.params, got, c.want)
		}
	}
	if ProtocolVersion != "2025-11-25" {
		t.Fatalf("latest protocol version: %q", ProtocolVersion)
	}
}
