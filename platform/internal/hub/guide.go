package hub

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/apidesc"
	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/store"
)

var exampleBundle = map[string]string{
	"plugin.json": `{
  "name": "researcher",
  "version": "0.1.0",
  "description": "Answers questions using a fetch tool"
}
`,
	"x-agen/agent.md": `---
name: researcher
description: Answers questions using a fetch tool
maxTurns: 12
---

You answer questions. Use fetch.fetch to read web pages when you need facts, and cite the URLs you used.
`,
	"x-agen/harness.json": `{
  "provider": "openrouter",
  "model": "deepseek/deepseek-v4-flash",
  "maxOutputTokens": 2000
}
`,
	"x-agen/secrets.json": `{
  "OPENROUTER_API_KEY": { "source": "platform" }
}
`,
	"x-agen/config.json": `{
  "kind": "pool",
  "scale": { "min": 0, "max": 3, "idleTimeout": "5m" },
  "budget": { "maxTokensPerRun": 50000, "maxUsdPerDay": 1 },
  "permissions": {
    "default": "deny",
    "rules": [{ "tool": "fetch.*", "action": "allow" }]
  },
  "tools": { "fetch.fetch": { "sideEffect": false } },
  "triggers": [
    {
      "type": "webhook",
      "name": "ask",
      "idempotencyKey": { "header": "X-Request-Id" },
      "conversationKey": { "field": "thread" }
    }
  ]
}
`,
	"mcp.json": `{
  "mcpServers": {
    "fetch": { "command": "uvx", "args": ["mcp-server-fetch"] }
  }
}
`,
}

var schemaFiles = map[string]string{
	"plugin.json":         "plugin",
	"mcp.json":            "mcp",
	"x-agen/agent.md":     "agent",
	"x-agen/harness.json": "harness",
	"x-agen/config.json":  "config",
	"x-agen/secrets.json": "secrets",
}

func (h *Hub) GetBundleGuide(context.Context, *connect.Request[agenv1.GetBundleGuideRequest]) (*connect.Response[agenv1.GetBundleGuideResponse], error) {
	out := &agenv1.GetBundleGuideResponse{Guide: apidesc.Primer, Schemas: map[string]string{}, Example: exampleBundle}
	for file, name := range schemaFiles {
		s, err := bundle.Schema(name)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out.Schemas[file] = s
	}
	return connect.NewResponse(out), nil
}

func (h *Hub) WhoAmI(ctx context.Context, _ *connect.Request[agenv1.WhoAmIRequest]) (*connect.Response[agenv1.WhoAmIResponse], error) {
	p := PrincipalFrom(ctx)
	out := &agenv1.WhoAmIResponse{Id: p.ID, Name: p.Name, Namespaces: p.Namespaces}
	for s := range p.Scopes {
		out.Scopes = append(out.Scopes, s)
	}
	sort.Strings(out.Scopes)
	return connect.NewResponse(out), nil
}

// checkWorkIdentity validates a task's conversation key and labels.
func checkWorkIdentity(key string, labels map[string]string) error {
	if len(key) > 256 {
		return invalid("conversation_key is longer than 256 bytes")
	}
	if err := store.CheckLabels(labels); err != nil {
		return invalid(err.Error())
	}
	return nil
}

// scopedKey prefixes a conversation key with where it came from, so a key
// sent to one entry point (API, a webhook, an A2A caller) can never name a
// conversation of another.
func scopedKey(prefix, key string) string {
	if key == "" {
		return ""
	}
	return prefix + key
}

// bundleFiles merges binary and text bundle files; a path may be given once.
func bundleFiles(bin map[string][]byte, text map[string]string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(bin)+len(text))
	for p, b := range bin {
		out[p] = b
	}
	for p, t := range text {
		if _, dup := out[p]; dup {
			return nil, invalid(fmt.Sprintf("%s is in both bundle_files and bundle_text", p))
		}
		out[p] = []byte(t)
	}
	return out, nil
}

// textFiles returns the files that are UTF-8 text.
func textFiles(files map[string][]byte) map[string]string {
	out := map[string]string{}
	for p, b := range files {
		if utf8.Valid(b) && !strings.ContainsRune(string(b), 0) {
			out[p] = string(b)
		}
	}
	return out
}

// bundleWarnings lists likely mistakes that do not make a bundle invalid.
func (h *Hub) bundleWarnings(ctx context.Context, ns string, b *bundle.Bundle) []string {
	var w []string
	if len(b.ToolServers) == 0 && len(b.Delegates) == 0 && b.Skills == 0 {
		w = append(w, "the agent has no tools: add servers to mcp.json, deployments to x-agen/config.json delegates, or skills")
	}
	switch b.Provider {
	case "fake":
		w = append(w, `x-agen/harness.json: provider "fake" is scripted and never calls a model`)
	case "replay":
		w = append(w, `x-agen/harness.json: provider "replay" replays a recorded cassette and never calls a model`)
	}
	known := func(name string) bool {
		for _, s := range b.ToolServers {
			if strings.HasPrefix(name, s+".") || strings.HasPrefix(name, s+"_") {
				return true
			}
		}
		return name == "call_agent" || name == "load_skill"
	}
	for _, r := range b.PermissionRules {
		if !strings.Contains(r, "*") && !known(r) {
			w = append(w, fmt.Sprintf("x-agen/config.json: permission rule %q names no server in mcp.json", r))
		}
	}
	for _, t := range b.ToolSettings {
		if !known(t) {
			w = append(w, fmt.Sprintf("x-agen/config.json: tools.%s names no server in mcp.json", t))
		}
	}
	for _, d := range b.Delegates {
		if _, err := h.Store.GetDeployment(ctx, ns, d); err != nil {
			w = append(w, fmt.Sprintf("x-agen/config.json: delegate %q is not deployed in namespace %s", d, ns))
		}
	}
	if _, ok := b.Files["x-agen/secrets.json"]; !ok && (b.Provider == "openrouter" || b.Provider == "openai") {
		w = append(w, fmt.Sprintf("x-agen/secrets.json is missing: provider %q needs an API key secret", b.Provider))
	}
	for p := range b.Files {
		if path.Base(p) == ".env" {
			w = append(w, p+": bundles are stored in the Hub; keep secrets out of them (use x-agen/secrets.json)")
		}
	}
	sort.Strings(w)
	return w
}
