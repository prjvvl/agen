// Routing models pick the serving model per request, within a cost tier
// (see the engine's provider/router.rs).
export const ROUTER_TIERS: Record<string, string[]> = {
  "typesafe/jev-router": ["low", "medium", "high"],
  "openrouter/auto": ["low", "medium", "high", "xhigh", "max"],
  "openrouter/auto-beta": ["low", "medium", "high", "xhigh", "max"],
};

/** fitTier keeps a tier within what the router offers. */
export function fitTier(model: string, tier: string): string {
  const tiers = ROUTER_TIERS[model] ?? [];
  return tiers.includes(tier) ? tier : tiers[tiers.length - 1];
}

type Harness = {
  model?: string;
  router?: { tier?: string; maxTier?: string; budgetTier?: string };
  fallbackModels?: string[];
};

/**
 * withModel returns a harness for another model: router settings only stay
 * on a routing model (with tiers it offers), and a fallback that is now the
 * model itself is dropped.
 */
export function withModel<H extends Harness>(
  h: H,
  model: string,
  tier?: string,
): H {
  const out: H = { ...h, model };
  if (ROUTER_TIERS[model]) {
    const r = { ...(h.router ?? {}) };
    if (tier) r.tier = tier;
    if (!r.tier) r.tier = "medium";
    for (const k of ["tier", "maxTier", "budgetTier"] as const)
      if (r[k]) r[k] = fitTier(model, r[k]!);
    out.router = r;
  } else {
    delete out.router;
  }
  if (h.fallbackModels) {
    const fb = h.fallbackModels.filter((m) => m !== model);
    if (fb.length) out.fallbackModels = fb;
    else delete out.fallbackModels;
  }
  return out;
}
