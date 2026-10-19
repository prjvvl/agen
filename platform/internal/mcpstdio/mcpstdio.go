// Package mcpstdio serves a set of tools as an MCP server over stdio
// (newline-delimited JSON-RPC), for the tool servers built into agen.
package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
)

// Tool describes one tool for tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// Tools is what a server offers.
type Tools interface {
	Tools() []Tool
	// Call runs a tool; an error is shown to the model as the tool's error.
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// ReadOnly marks a tool that changes nothing.
var ReadOnly = map[string]any{"readOnlyHint": true}

// Object, Str and Int build small JSON Schemas.
func Object(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func Str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func Int(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func Bool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// Serve answers MCP requests on r with responses on w until r ends.
func Serve(ctx context.Context, t Tools, name, instructions string, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // a notification
		}
		result, rerr := handle(ctx, t, name, instructions, req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func handle(ctx context.Context, t Tools, name, instructions, method string, params json.RawMessage) (any, map[string]any) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": name, "version": "1"}, "instructions": instructions}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": t.Tools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, map[string]any{"code": -32602, "message": err.Error()}
		}
		text, err := t.Call(ctx, p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
	}
	return nil, map[string]any{"code": -32601, "message": "method not found: " + method}
}
