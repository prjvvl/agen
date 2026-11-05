//! Per-task overrides: settings a task carries that can only narrow what
//! its bundle allows (docs/architecture.md). The JSON shape matches the
//! platform's `store.TaskOverrides` exactly, since it crosses the Hub ->
//! Manager -> host boundary as plain JSON.

use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TaskOverrides {
    /// Only these of the bundle's tools (exact names or globs, as
    /// permission rules write them). `finish` and `load_skill` are always
    /// available. Empty: every bundle tool.
    #[serde(default)]
    pub tools: Vec<String>,
    /// Skills (of the bundle) to preload, in addition to the bundle's own
    /// `preloadSkills`.
    #[serde(default)]
    pub skills: Vec<String>,
    /// Text appended after the system prompt.
    #[serde(default)]
    pub system_addendum: String,
    /// Extra rules, deny or ask only, checked before the bundle's own.
    #[serde(default)]
    pub permissions: Vec<OverrideRule>,
    /// Run limits, at most the bundle's own.
    #[serde(default)]
    pub budget: Option<RunBudgetOverride>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct OverrideRule {
    pub tool: String,
    /// "deny" or "ask".
    pub action: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RunBudgetOverride {
    pub max_tokens: Option<u64>,
    pub max_usd: Option<f64>,
    pub max_turns: Option<u32>,
    /// e.g. "20m".
    pub max_duration: Option<String>,
}

impl TaskOverrides {
    /// Parses a task's stored/wire overrides ("" or "null" -> none).
    pub fn parse(raw: &str) -> Option<Self> {
        if raw.is_empty() || raw == "null" {
            return None;
        }
        serde_json::from_str(raw).ok()
    }

    /// True if there is nothing to apply (an all-default value, same as
    /// `None`).
    pub fn is_empty(&self) -> bool {
        self == &Self::default()
    }

    /// From the arguments of `start_agent`'s override fields (`tools`,
    /// `skills`, `budget`, with `systemAddendum`/`permissions` reserved for
    /// a future caller; absent fields stay unset). `None` when the call
    /// gave no overrides at all.
    pub fn from_call_args(args: &Value) -> Result<Option<Self>, String> {
        let mut o = TaskOverrides::default();
        let mut any = false;
        if let Some(list) = args.get("tools") {
            o.tools = string_list(list, "tools")?;
            any = any || !o.tools.is_empty();
        }
        if let Some(list) = args.get("skills") {
            o.skills = string_list(list, "skills")?;
            any = any || !o.skills.is_empty();
        }
        if let Some(b) = args.get("budget").filter(|b| !b.is_null()) {
            let budget = RunBudgetOverride {
                max_tokens: b.get("maxTokens").and_then(Value::as_u64),
                max_usd: b.get("maxUsd").and_then(Value::as_f64),
                max_turns: b.get("maxTurns").and_then(Value::as_u64).map(|n| n as u32),
                max_duration: b.get("maxDuration").and_then(Value::as_str).map(str::to_string),
            };
            if let Some(d) = &budget.max_duration {
                parse_duration_checked(d)?;
            }
            o.budget = Some(budget);
            any = true;
        }
        Ok(if any { Some(o) } else { None })
    }

    /// Combines `self` as the parent's effective overrides with `child`'s
    /// own: tool and skill allowlists intersect (empty on one side keeps
    /// the other's), permission rules concatenate (parent's first), the
    /// system addendum appends, and budgets take the smaller of each set
    /// field. Narrows only, in either direction.
    pub fn combine(parent: Option<&TaskOverrides>, child: Option<&TaskOverrides>) -> Option<TaskOverrides> {
        match (parent, child) {
            (None, c) => c.cloned(),
            (p, None) => p.cloned(),
            (Some(p), Some(c)) => Some(TaskOverrides {
                tools: intersect_or_either(&p.tools, &c.tools),
                skills: intersect_or_either(&p.skills, &c.skills),
                system_addendum: [p.system_addendum.as_str(), c.system_addendum.as_str()]
                    .iter()
                    .filter(|s| !s.is_empty())
                    .copied()
                    .collect::<Vec<_>>()
                    .join("\n"),
                permissions: p.permissions.iter().chain(c.permissions.iter()).cloned().collect(),
                budget: combine_budgets(p.budget.as_ref(), c.budget.as_ref()),
            }),
        }
    }
}

fn string_list(v: &Value, field: &str) -> Result<Vec<String>, String> {
    v.as_array()
        .ok_or_else(|| format!("{field} must be a list of strings"))?
        .iter()
        .map(|x| {
            x.as_str()
                .map(str::to_string)
                .ok_or_else(|| format!("{field} must be a list of strings"))
        })
        .collect()
}

fn parse_duration_checked(s: &str) -> Result<(), String> {
    crate::bundle::parse_duration(s)
        .map(|_| ())
        .ok_or_else(|| format!("budget.maxDuration {s:?} is not a duration (e.g. \"20m\")"))
}

fn intersect_or_either(a: &[String], b: &[String]) -> Vec<String> {
    match (a.is_empty(), b.is_empty()) {
        (true, _) => b.to_vec(),
        (_, true) => a.to_vec(),
        _ => a.iter().filter(|x| b.contains(x)).cloned().collect(),
    }
}

fn combine_budgets(a: Option<&RunBudgetOverride>, b: Option<&RunBudgetOverride>) -> Option<RunBudgetOverride> {
    match (a, b) {
        (None, x) => x.cloned(),
        (x, None) => x.cloned(),
        (Some(a), Some(b)) => Some(RunBudgetOverride {
            max_tokens: min_opt(a.max_tokens, b.max_tokens),
            max_usd: min_opt_f(a.max_usd, b.max_usd),
            max_turns: min_opt(a.max_turns, b.max_turns),
            max_duration: min_duration(a.max_duration.as_deref(), b.max_duration.as_deref()),
        }),
    }
}

fn min_opt<T: Ord + Copy>(a: Option<T>, b: Option<T>) -> Option<T> {
    match (a, b) {
        (Some(a), Some(b)) => Some(a.min(b)),
        (a, None) => a,
        (None, b) => b,
    }
}

fn min_opt_f(a: Option<f64>, b: Option<f64>) -> Option<f64> {
    match (a, b) {
        (Some(a), Some(b)) => Some(a.min(b)),
        (a, None) => a,
        (None, b) => b,
    }
}

fn min_duration(a: Option<&str>, b: Option<&str>) -> Option<String> {
    match (a, b) {
        (Some(a), Some(b)) => {
            let (da, db) = (crate::bundle::parse_duration(a), crate::bundle::parse_duration(b));
            match (da, db) {
                (Some(da), Some(db)) if da <= db => Some(a.to_string()),
                (Some(_), Some(_)) => Some(b.to_string()),
                _ => Some(a.to_string()),
            }
        }
        (Some(a), None) => Some(a.to_string()),
        (None, b) => b.map(str::to_string),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parses_the_platform_json_shape() {
        let o = TaskOverrides::parse(
            r#"{"tools":["read.*"],"skills":["coding"],"systemAddendum":"Be terse.",
                "permissions":[{"tool":"write.delete","action":"deny"}],
                "budget":{"maxTokens":5000,"maxUsd":0.1,"maxTurns":5,"maxDuration":"10m"}}"#,
        )
        .unwrap();
        assert_eq!(o.tools, vec!["read.*"]);
        assert_eq!(o.permissions[0].action, "deny");
        assert_eq!(o.budget.unwrap().max_turns, Some(5));
        assert_eq!(TaskOverrides::parse(""), None);
        assert_eq!(TaskOverrides::parse("null"), None);
    }

    #[test]
    fn combine_narrows_both_ways() {
        let parent = TaskOverrides {
            tools: vec!["read.*".into(), "write.*".into()],
            budget: Some(RunBudgetOverride {
                max_tokens: Some(10_000),
                max_duration: Some("30m".into()),
                ..Default::default()
            }),
            ..Default::default()
        };
        let child = TaskOverrides {
            tools: vec!["write.*".into(), "exec.*".into()],
            budget: Some(RunBudgetOverride {
                max_tokens: Some(2_000),
                max_duration: Some("1h".into()),
                ..Default::default()
            }),
            ..Default::default()
        };
        let c = TaskOverrides::combine(Some(&parent), Some(&child)).unwrap();
        assert_eq!(c.tools, vec!["write.*"]); // only the intersection
        let b = c.budget.unwrap();
        assert_eq!(b.max_tokens, Some(2_000)); // the smaller
        assert_eq!(b.max_duration, Some("30m".to_string())); // the smaller
        assert_eq!(TaskOverrides::combine(None, None), None);
        assert_eq!(TaskOverrides::combine(Some(&parent), None), Some(parent));
    }

    #[test]
    fn from_call_args_rejects_bad_shapes() {
        assert!(TaskOverrides::from_call_args(&json!({})).unwrap().is_none());
        let o = TaskOverrides::from_call_args(&json!({"tools": ["read.*"]}))
            .unwrap()
            .unwrap();
        assert_eq!(o.tools, vec!["read.*"]);
        assert!(TaskOverrides::from_call_args(&json!({"tools": "nope"})).is_err());
        assert!(TaskOverrides::from_call_args(&json!({"budget": {"maxDuration": "nope"}})).is_err());
    }
}
