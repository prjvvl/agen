package bundle

import "fmt"

// routers are the routing models agen knows (see the engine's
// provider/router.rs): their plugin id and highest cost tier.
var routers = map[string]struct {
	plugin string
	top    int
}{
	"typesafe/jev-router":  {"jev-router", 2},
	"openrouter/auto":      {"auto-router", 4},
	"openrouter/auto-beta": {"auto-beta-router", 4},
}

var tiers = []string{"low", "medium", "high", "xhigh", "max"}

func tierRank(t string) int {
	for i, x := range tiers {
		if x == t {
			return i
		}
	}
	return -1
}

// checkHarness reports harness settings the schema cannot judge: router and
// OpenRouter settings on other providers or models, and tiers a router does
// not offer. The engine applies the same checks when an instance starts.
func checkHarness(h map[string]any) []string {
	if h == nil {
		return nil
	}
	var out []string
	provider, _ := h["provider"].(string)
	model, _ := h["model"].(string)
	router, hasRouter := h["router"].(map[string]any)
	_, hasOR := h["openrouter"]
	if provider != "openrouter" {
		if hasOR {
			out = append(out, `openrouter applies to provider "openrouter" only`)
		}
		if hasRouter {
			out = append(out, `router applies to provider "openrouter" only`)
		}
	}
	if fb, ok := h["fallbackModels"].([]any); ok {
		for _, m := range fb {
			if m == model {
				out = append(out, "fallbackModels repeats the primary model")
			}
		}
	}
	if !hasRouter {
		return out
	}
	kind, ok := routers[model]
	if !ok {
		return append(out, fmt.Sprintf("router: model %q is not a routing model (typesafe/jev-router, openrouter/auto, openrouter/auto-beta)", model))
	}
	for _, f := range []string{"tier", "maxTier", "budgetTier"} {
		if t, ok := router[f].(string); ok && (tierRank(t) < 0 || tierRank(t) > kind.top) {
			out = append(out, fmt.Sprintf("router/%s: tier %q is not offered by %s (use one of %v)", f, t, kind.plugin, tiers[:kind.top+1]))
		}
	}
	t, _ := router["tier"].(string)
	m, _ := router["maxTier"].(string)
	if t != "" && m != "" && tierRank(t) > tierRank(m) {
		out = append(out, fmt.Sprintf("router/tier: tier %q is above maxTier %q", t, m))
	}
	if or, ok := h["openrouter"].(map[string]any); ok {
		if plugins, ok := or["plugins"].([]any); ok {
			for _, p := range plugins {
				if pm, ok := p.(map[string]any); ok && pm["id"] == kind.plugin {
					out = append(out, fmt.Sprintf("openrouter/plugins: plugin %q is set by \"router\"; configure it there", kind.plugin))
				}
			}
		}
	}
	return out
}
