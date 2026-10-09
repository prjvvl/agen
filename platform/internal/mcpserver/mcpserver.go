// Package mcpserver serves the Hub API as MCP tools (docs/architecture.md
// §11): one tool per unary HubService RPC, derived from the proto
// descriptors (internal/apidesc). A tool call is executed by the very same
// Connect handler that serves REST/gRPC, with the caller's bearer token, so
// authentication, scopes and results are identical across CLI, REST and MCP.
//
// Transport: MCP "streamable HTTP" in its stateless form — JSON-RPC over
// POST /mcp answered with application/json (no server-initiated streams).
package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/prjvvl/agen/platform/internal/apidesc"
	"github.com/prjvvl/agen/platform/internal/version"
)

// ProtocolVersion is the MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

type server struct {
	api   http.Handler
	tools []apidesc.Tool
	index map[string]apidesc.Tool
}

// Handler returns the MCP endpoint over api, the handler serving HubService.
func Handler(api http.Handler) (http.Handler, error) {
	sd, err := apidesc.Service("agen.v1.HubService")
	if err != nil {
		return nil, err
	}
	s := &server{api: api, tools: apidesc.Tools(sd), index: map[string]apidesc.Tool{}}
	for _, t := range s.tools {
		s.index[t.Name] = t
	}
	return s, nil
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP endpoint is stateless: POST JSON-RPC messages", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, response(nil, nil, &rpcError{-32600, "request too large"}))
		return
	}
	// A JSON-RPC batch or a single message.
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		var batch []rpcRequest
		if err := json.Unmarshal(t, &batch); err != nil {
			writeJSON(w, http.StatusBadRequest, response(nil, nil, &rpcError{-32700, "parse error"}))
			return
		}
		var out []any
		for _, req := range batch {
			if resp := s.handle(r, req); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, response(nil, nil, &rpcError{-32700, "parse error"}))
		return
	}
	resp := s.handle(r, req)
	if resp == nil { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func response(id json.RawMessage, result any, e *rpcError) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handle answers one message; nil for notifications.
func (s *server) handle(r *http.Request, req rpcRequest) any {
	notification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		return response(req.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "agen", "version": version.Version},
			"instructions": "Agen fleet control: list, deploy, scale, stop and inspect agent deployments, submit tasks, " +
				"and decide approvals. Every tool is one Hub API call and needs the same token scopes as the API.",
		}, nil)
	case "ping":
		return response(req.ID, map[string]any{}, nil)
	case "tools/list":
		return response(req.ID, map[string]any{"tools": s.tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return response(req.ID, nil, &rpcError{-32602, "invalid params"})
		}
		t, ok := s.index[p.Name]
		if !ok {
			return response(req.ID, nil, &rpcError{-32602, fmt.Sprintf("unknown tool %q", p.Name)})
		}
		return response(req.ID, s.call(r, t, p.Arguments), nil)
	default:
		if notification || strings.HasPrefix(req.Method, "notifications/") {
			return nil
		}
		return response(req.ID, nil, &rpcError{-32601, fmt.Sprintf("method %q not found", req.Method)})
	}
}

// call runs a tool through the API handler with the caller's credentials.
// API errors come back as tool results with isError, as MCP prescribes.
func (s *server) call(r *http.Request, t apidesc.Tool, args json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	req := httptest.NewRequestWithContext(r.Context(), http.MethodPost, t.Procedure, bytes.NewReader(args))
	req.Header.Set("Content-Type", "application/json")
	if a := r.Header.Get("Authorization"); a != "" {
		req.Header.Set("Authorization", a)
	}
	rec := httptest.NewRecorder()
	s.api.ServeHTTP(rec, req)
	text := strings.TrimSpace(rec.Body.String())
	var structured map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &structured)
	out := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if rec.Code != http.StatusOK {
		out["isError"] = true
		return out
	}
	if structured != nil {
		out["structuredContent"] = structured
	}
	return out
}
