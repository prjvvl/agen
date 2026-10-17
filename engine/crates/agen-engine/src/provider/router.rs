//! Routing models on OpenRouter: the model id names a router that picks the
//! serving model (and reasoning effort) per request. Settings go in a plugin
//! whose id depends on the router; the serving model comes back in the
//! response's `model` field.
//!
//! - `typesafe/jev-router`: plugin `jev-router`, cost tiers low, medium, high.
//! - `openrouter/auto`: plugin `auto-router`, tiers low .. max.
//! - `openrouter/auto-beta`: plugin `auto-beta-router`, tiers low .. max.
//!
//! A tier is a band, not a ceiling: the router picks among models of that
//! band, so a lower tier usually costs less but is not guaranteed to.

use serde::Deserialize;
use serde_json::{json, Value};

/// All tier names, cheapest first.
pub const TIERS: [&str; 5] = ["low", "medium", "high", "xhigh", "max"];

/// Share of the daily budget after which runs use `budgetTier`.
pub const BUDGET_PRESSURE: f64 = 0.8;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RouterKind {
    pub plugin_id: &'static str,
    /// Highest tier the router accepts.
    pub max_tier: &'static str,
}

/// The router a model id names, if it is one.
pub fn router_for(model: &str) -> Option<RouterKind> {
    match model {
        "typesafe/jev-router" => Some(RouterKind {
            plugin_id: "jev-router",
            max_tier: "high",
        }),
        "openrouter/auto" => Some(RouterKind {
            plugin_id: "auto-router",
            max_tier: "max",
        }),
        "openrouter/auto-beta" => Some(RouterKind {
            plugin_id: "auto-beta-router",
            max_tier: "max",
        }),
        _ => None,
    }
}

/// Position of a tier (0 = low), if it is a tier name.
pub fn rank(tier: &str) -> Option<usize> {
    TIERS.iter().position(|t| *t == tier)
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct RouterSettings {
    pub tier: Option<String>,
    pub max_tier: Option<String>,
    pub budget_tier: Option<String>,
    #[serde(default)]
    pub allowed_models: Vec<String>,
    #[serde(default)]
    pub excluded_models: Vec<String>,
}

impl RouterSettings {
    /// Problems with these settings for a router (empty when fine).
    pub fn check(&self, kind: RouterKind) -> Vec<(&'static str, String)> {
        let top = rank(kind.max_tier).unwrap_or(0);
        let mut out = vec![];
        for (field, v) in [
            ("/router/tier", &self.tier),
            ("/router/maxTier", &self.max_tier),
            ("/router/budgetTier", &self.budget_tier),
        ] {
            if let Some(t) = v {
                if rank(t).is_none_or(|r| r > top) {
                    out.push((
                        field,
                        format!(
                            "tier {t:?} is not offered by {}; use one of {}",
                            kind.plugin_id,
                            TIERS[..=top].join(", ")
                        ),
                    ));
                }
            }
        }
        if let (Some(t), Some(m)) = (&self.tier, &self.max_tier) {
            if rank(t) > rank(m) {
                out.push(("/router/tier", format!("tier {t:?} is above maxTier {m:?}")));
            }
        }
        out
    }

    /// The tier for one run: the task's choice (else the default), capped by
    /// `maxTier`; near the daily budget no higher than `budgetTier` (low).
    /// None leaves the choice to the router.
    pub fn tier_for(&self, requested: Option<&str>, near_budget: bool, kind: RouterKind) -> Option<String> {
        let top = rank(kind.max_tier).unwrap_or(0);
        let cap = self.max_tier.as_deref().and_then(rank).unwrap_or(top).min(top);
        let mut tier = requested
            .filter(|t| rank(t).is_some())
            .or(self.tier.as_deref())
            .and_then(rank);
        if let Some(t) = tier {
            tier = Some(t.min(cap));
        }
        if near_budget {
            let b = self.budget_tier.as_deref().and_then(rank).unwrap_or(0).min(cap);
            tier = Some(tier.map_or(b, |t| t.min(b)));
        }
        tier.map(|t| TIERS[t].to_string())
    }

    /// The plugin entry for a request at a tier.
    pub fn plugin(&self, kind: RouterKind, tier: Option<&str>) -> Value {
        let mut p = json!({"id": kind.plugin_id});
        if let Some(t) = tier {
            p["cost_tier"] = json!(t);
        }
        if !self.allowed_models.is_empty() {
            p["allowed_models"] = json!(self.allowed_models);
        }
        if !self.excluded_models.is_empty() {
            p["excluded_models"] = json!(self.excluded_models);
        }
        p
    }
}

/// A short summary of a router's decision, from `openrouter_metadata` (sent
/// when the request asks for it): what kind of task it saw, why it picked,
/// and the effort.
pub fn decision_summary(meta: &Value, plugin_id: &str) -> Option<Value> {
    let stage = meta
        .get("pipeline")?
        .as_array()?
        .iter()
        .find(|p| p.get("name").and_then(Value::as_str) == Some(plugin_id))?;
    let data = stage.get("data")?;
    let mut out = serde_json::Map::new();
    if let Some(task) = data.pointer("/answers/task/choice").and_then(Value::as_str) {
        out.insert("task".into(), json!(task));
    }
    for k in ["reason", "version", "floor"] {
        if let Some(v) = data.get(k).and_then(Value::as_str) {
            out.insert(k.into(), json!(v));
        }
    }
    if let Some(e) = data
        .get("selected_effort")
        .or_else(|| data.pointer("/selected/effort"))
        .and_then(Value::as_str)
    {
        out.insert("effort".into(), json!(e));
    }
    if let Some(s) = stage.get("summary").and_then(Value::as_str) {
        out.insert("summary".into(), json!(s));
    }
    Some(Value::Object(out))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s(tier: Option<&str>, max: Option<&str>) -> RouterSettings {
        RouterSettings {
            tier: tier.map(Into::into),
            max_tier: max.map(Into::into),
            ..Default::default()
        }
    }

    #[test]
    fn known_routers() {
        assert_eq!(router_for("typesafe/jev-router").unwrap().plugin_id, "jev-router");
        assert_eq!(router_for("openrouter/auto").unwrap().max_tier, "max");
        assert!(router_for("deepseek/deepseek-v4-flash").is_none());
    }

    #[test]
    fn tier_choice() {
        let jev = router_for("typesafe/jev-router").unwrap();
        // default, override, cap, budget pressure
        assert_eq!(
            s(Some("medium"), None).tier_for(None, false, jev).as_deref(),
            Some("medium")
        );
        assert_eq!(
            s(Some("low"), None).tier_for(Some("high"), false, jev).as_deref(),
            Some("high")
        );
        assert_eq!(
            s(Some("low"), Some("medium"))
                .tier_for(Some("high"), false, jev)
                .as_deref(),
            Some("medium")
        );
        assert_eq!(s(Some("high"), None).tier_for(None, true, jev).as_deref(), Some("low"));
        assert_eq!(s(None, None).tier_for(None, false, jev), None);
        assert_eq!(s(None, None).tier_for(None, true, jev).as_deref(), Some("low"));
        // a router's own top caps a request beyond it
        assert_eq!(s(None, None).tier_for(Some("max"), false, jev).as_deref(), Some("high"));
        // nonsense requests fall back to the default
        assert_eq!(
            s(Some("low"), None).tier_for(Some("ultra"), false, jev).as_deref(),
            Some("low")
        );
        let budget = RouterSettings {
            budget_tier: Some("medium".into()),
            ..s(Some("high"), None)
        };
        assert_eq!(budget.tier_for(None, true, jev).as_deref(), Some("medium"));
        assert_eq!(budget.tier_for(Some("low"), true, jev).as_deref(), Some("low"));
    }

    #[test]
    fn checks_tiers_against_the_router() {
        let jev = router_for("typesafe/jev-router").unwrap();
        assert!(s(Some("low"), Some("high")).check(jev).is_empty());
        assert_eq!(s(Some("xhigh"), None).check(jev)[0].0, "/router/tier");
        assert_eq!(s(Some("high"), Some("low")).check(jev)[0].0, "/router/tier");
        let auto = router_for("openrouter/auto").unwrap();
        assert!(s(Some("max"), None).check(auto).is_empty());
    }

    #[test]
    fn plugin_entry() {
        let jev = router_for("typesafe/jev-router").unwrap();
        let st = RouterSettings {
            allowed_models: vec!["deepseek/*".into()],
            ..Default::default()
        };
        assert_eq!(
            st.plugin(jev, Some("low")),
            json!({"id": "jev-router", "cost_tier": "low", "allowed_models": ["deepseek/*"]})
        );
        assert_eq!(s(None, None).plugin(jev, None), json!({"id": "jev-router"}));
    }

    #[test]
    fn decision_summary_from_metadata() {
        let meta = json!({"pipeline": [
            {"type": "plugin", "name": "jev-router", "summary": "Selected a model and effort with the decision policy",
             "data": {"answers": {"task": {"type": "choice", "choice": "code:general_impl"}}, "reason": "initial", "version": "v1", "floor": "basic"}}
        ]});
        let d = decision_summary(&meta, "jev-router").unwrap();
        assert_eq!(d["task"], "code:general_impl");
        assert_eq!(d["reason"], "initial");
        assert!(decision_summary(&meta, "auto-router").is_none());
    }
}
