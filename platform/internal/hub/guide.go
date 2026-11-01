package hub

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
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
    "fetch": { "command": "uvx", "args": ["mcp-server-fetch==2026.8.18"] }
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
	out := &agenv1.WhoAmIResponse{Id: p.ID, Name: p.Name, Namespaces: p.Namespaces, Trust: p.Trust}
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
	if key == "" || strings.HasPrefix(key, prefix) {
		return key // a key read back from a task already carries its source
	}
	return prefix + key
}

// maxScheduleAhead bounds how far ahead a task may be scheduled.
const maxScheduleAhead = 90 * 24 * time.Hour

// checkTier accepts an empty tier or a routing model cost tier.
func checkTier(t string) error {
	switch t {
	case "", "low", "medium", "high", "xhigh", "max":
		return nil
	}
	return invalid(fmt.Sprintf("model_tier %q: use low, medium, high, xhigh or max", t))
}

func checkPriority(p string) error {
	switch p {
	case "", "urgent", "high", "normal", "background":
		return nil
	}
	return invalid(fmt.Sprintf("priority %q: use urgent, high, normal or background", p))
}

func checkTrust(t string) error {
	switch t {
	case "", "operator", "trusted", "untrusted":
		return nil
	}
	return invalid(fmt.Sprintf("trust %q: use operator, trusted or untrusted", t))
}

// checkOnParentEnd validates Task.on_parent_end (3.4, part): the
// parent-close policy applied to a task's own children when it ends.
func checkOnParentEnd(v string) error {
	switch v {
	case "", "cancel", "abandon":
		return nil
	}
	return invalid(fmt.Sprintf("on_parent_end %q: use cancel or abandon", v))
}

// checkDelivery validates SubmitTaskRequest.delivery (3.9, part): a
// per-task override of the deployment's own conversations.queueMode, for
// this submission only.
func checkDelivery(v string) error {
	switch v {
	case "", "followup", "collect", "steer", "interrupt":
		return nil
	}
	return invalid(fmt.Sprintf("delivery %q: use followup, collect, steer or interrupt", v))
}

// checkConversationPolicy validates a deployment's conversations policy
// (3.9, part): queue_mode and debounce (collect only).
func checkConversationPolicy(p *agenv1.ConversationPolicy) error {
	if p == nil {
		return nil
	}
	switch p.QueueMode {
	case "", "followup", "collect", "steer", "interrupt":
	default:
		return invalid(fmt.Sprintf("conversations.queueMode %q: use followup, collect, steer or interrupt", p.QueueMode))
	}
	if p.Debounce != "" {
		if _, err := time.ParseDuration(p.Debounce); err != nil {
			return invalid(fmt.Sprintf("conversations.debounce %q: %s", p.Debounce, err))
		}
	}
	return nil
}

// checkBudget validates a deployment's budget policy (3.5, part):
// label_usd_per_day's keys are "key=value", each validated the same way a
// real task label would be (store.CheckLabels), so a budget rule can
// never target a label shape the system would not actually assign.
func checkBudget(b *agenv1.Budget) error {
	if b == nil {
		return nil
	}
	for kv := range b.LabelUsdPerDay {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return invalid(fmt.Sprintf("budget.labelUsdPerDay %q: use \"key=value\"", kv))
		}
		if err := store.CheckLabels(map[string]string{key: val}); err != nil {
			return invalid(fmt.Sprintf("budget.labelUsdPerDay %q: %s", kv, err))
		}
	}
	return nil
}

// trustRank orders trust for narrowing: higher is more trusted. "" (unset)
// is trusted.
func trustRank(t string) int {
	switch t {
	case "operator":
		return 2
	case "untrusted":
		return 0
	default:
		return 1
	}
}

// narrowTrust returns the lower (less trusted) of a and b.
func narrowTrust(a, b string) string {
	if trustRank(a) <= trustRank(b) {
		return a
	}
	return b
}

// checkLocks validates a task's declared locks (3.11, part): at most 16,
// each a non-empty name of at most 128 characters, no duplicates.
func checkLocks(locks []string) error {
	if len(locks) > 16 {
		return invalid("at most 16 locks per task")
	}
	seen := map[string]bool{}
	for _, name := range locks {
		if name == "" || len(name) > 128 {
			return invalid("a lock name must be 1-128 characters")
		}
		if seen[name] {
			return invalid(fmt.Sprintf("lock %q named twice", name))
		}
		seen[name] = true
	}
	return nil
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
	if b.RecordInteractions {
		w = append(w, "x-agen/config.json: recordInteractions is on — every run saves its full prompts and tool-call payloads as a cassette (agen replay)")
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
		if d.URL != "" {
			continue
		}
		dns := d.Namespace
		if dns == "" {
			dns = ns
		}
		if _, err := h.Store.GetDeployment(ctx, dns, d.Name); err != nil {
			w = append(w, fmt.Sprintf("x-agen/config.json: delegate %q is not deployed in namespace %s", d.Name, dns))
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
