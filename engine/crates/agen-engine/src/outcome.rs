//! How a run ended, beyond its status: the `finish` tool, the optional result
//! schema, and the report a run gives when it stops at a limit.

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::provider::ToolSpec;
use crate::tools::{Tool, ToolContext, ToolError};

pub const FINISH: &str = "finish";

/// The tool result of an accepted `finish` call. The engine recognises a
/// finished run by it, also when the run is resumed after a crash.
pub const FINISHED: &str = "ok: finished";

/// The statuses `finish` accepts. `cancelled` is set by the engine only.
pub const FINISH_STATUSES: &[&str] = &["done", "partial", "needs_input", "rejected", "failed"];

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RunOutcome {
    /// done, partial, needs_input, rejected, failed or cancelled.
    pub status: String,
    /// Why the run stopped early (max_turns, budget, duration, stuck,
    /// cancelled, interrupted, error, no_answer); empty when it finished.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub summary: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub data: Option<Value>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub artifacts: Vec<String>,
    /// needs_input: "caller" or "human".
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub input_from: String,
}

impl RunOutcome {
    pub fn new(status: &str, reason: &str, summary: &str) -> Self {
        Self {
            status: status.into(),
            reason: reason.into(),
            summary: summary.into(),
            ..Default::default()
        }
    }

    /// Stored form ("" for none).
    pub fn to_json(o: &Option<RunOutcome>) -> String {
        o.as_ref()
            .map(|o| serde_json::to_string(o).unwrap_or_default())
            .unwrap_or_default()
    }

    pub fn from_json(s: &str) -> Option<RunOutcome> {
        if s.is_empty() {
            return None;
        }
        serde_json::from_str(s).ok()
    }
}

/// The schema a run's `finish` data must match (harness `result.schema`).
#[derive(Clone)]
pub struct ResultSchema {
    schema: Value,
    validator: std::sync::Arc<jsonschema::Validator>,
}

impl std::fmt::Debug for ResultSchema {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ResultSchema").field("schema", &self.schema).finish()
    }
}

impl ResultSchema {
    pub fn new(schema: Value) -> Result<Self, String> {
        let validator = jsonschema::validator_for(&schema).map_err(|e| format!("result.schema: {e}"))?;
        Ok(Self {
            schema,
            validator: std::sync::Arc::new(validator),
        })
    }

    pub fn schema(&self) -> &Value {
        &self.schema
    }

    fn check(&self, data: &Value) -> Result<(), String> {
        let errors: Vec<String> = self
            .validator
            .iter_errors(data)
            .take(5)
            .map(|e| {
                let at = e.instance_path().to_string();
                if at.is_empty() {
                    e.to_string()
                } else {
                    format!("{at}: {e}")
                }
            })
            .collect();
        if errors.is_empty() {
            Ok(())
        } else {
            Err(errors.join("; "))
        }
    }
}

/// The outcome a `finish` call asks for, or why it is refused.
pub fn from_finish(args: &Value, schema: Option<&ResultSchema>) -> Result<RunOutcome, String> {
    let status = args.get("status").and_then(Value::as_str).unwrap_or_default();
    if !FINISH_STATUSES.contains(&status) {
        return Err(format!("status must be one of {}", FINISH_STATUSES.join(", ")));
    }
    let summary = args.get("summary").and_then(Value::as_str).unwrap_or_default();
    let data = args.get("data").filter(|d| !d.is_null()).cloned();
    if let Some(schema) = schema {
        // A run that could not do its work may end without data.
        if status == "done" || data.is_some() {
            let d = data.clone().unwrap_or(Value::Null);
            schema
                .check(&d)
                .map_err(|e| format!("data does not match the result schema: {e}"))?;
        }
    }
    let artifacts = match args.get("artifacts") {
        None | Some(Value::Null) => vec![],
        Some(Value::Array(a)) => a
            .iter()
            .map(|v| v.as_str().map(String::from).ok_or("artifacts must be artifact ids"))
            .collect::<Result<_, _>>()?,
        Some(_) => return Err("artifacts must be a list of artifact ids".into()),
    };
    let input_from = match (status, args.get("inputFrom").and_then(Value::as_str)) {
        ("needs_input", None) => "caller",
        ("needs_input", Some(f @ ("caller" | "human"))) => f,
        ("needs_input", Some(_)) => return Err("inputFrom must be caller or human".into()),
        _ => "",
    };
    Ok(RunOutcome {
        status: status.into(),
        reason: String::new(),
        summary: summary.into(),
        data,
        artifacts,
        input_from: input_from.into(),
    })
}

/// The built-in `finish` tool: ends the run with a status and report.
pub struct Finish {
    schema: Option<ResultSchema>,
}

impl Finish {
    pub fn new(schema: Option<ResultSchema>) -> Self {
        Self { schema }
    }
}

#[async_trait]
impl Tool for Finish {
    fn spec(&self) -> ToolSpec {
        let mut data = serde_json::json!({"description": "Structured result of the work"});
        if let Some(s) = &self.schema {
            data = s.schema().clone();
        }
        let mut required = vec!["status", "summary"];
        if self.schema.is_some() {
            required.push("data");
        }
        ToolSpec {
            name: FINISH.into(),
            description: "End this run and report how it went. status: done (the work is complete), partial (only part of it), needs_input (you need an answer to go on; say what in summary), rejected (you will not do this work), failed (you could not do it). Call it alone, after your other tool calls have returned.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "status": {"type": "string", "enum": FINISH_STATUSES},
                    "summary": {"type": "string", "description": "Your report: what was done, what is left, what you need"},
                    "data": data,
                    "artifacts": {"type": "array", "items": {"type": "string"}, "description": "Ids of artifacts that hold the result"},
                    "inputFrom": {"type": "string", "enum": ["caller", "human"], "description": "needs_input: who must answer (default caller)"}
                },
                "required": required
            }),
        }
    }
    fn side_effect(&self) -> bool {
        false
    }
    async fn call(&self, args: Value, _ctx: ToolContext) -> Result<String, ToolError> {
        from_finish(&args, self.schema.as_ref())
            .map(|_| FINISHED.to_string())
            .map_err(|e| ToolError::new(format!("{e}; call finish again with corrected arguments")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn finish_arguments_are_checked() {
        let o = from_finish(&json!({"status": "done", "summary": "ok"}), None).unwrap();
        assert_eq!(o.status, "done");
        assert_eq!(o.summary, "ok");
        assert!(from_finish(&json!({"status": "cancelled"}), None).is_err());
        assert!(from_finish(&json!({"summary": "x"}), None).is_err());
        let o = from_finish(&json!({"status": "needs_input", "summary": "which repo?"}), None).unwrap();
        assert_eq!(o.input_from, "caller");
        assert!(from_finish(&json!({"status": "needs_input", "inputFrom": "god"}), None).is_err());
        assert!(from_finish(&json!({"status": "done", "artifacts": [1]}), None).is_err());
    }

    #[test]
    fn data_must_match_the_schema() {
        let s = ResultSchema::new(json!({
            "type": "object",
            "properties": {"n": {"type": "integer"}},
            "required": ["n"]
        }))
        .unwrap();
        assert!(from_finish(&json!({"status": "done", "data": {"n": 1}}), Some(&s)).is_ok());
        let e = from_finish(&json!({"status": "done", "data": {"n": "x"}}), Some(&s)).unwrap_err();
        assert!(e.contains("/n"), "{e}");
        assert!(from_finish(&json!({"status": "done"}), Some(&s)).is_err());
        // A failed run may report without data.
        assert!(from_finish(&json!({"status": "failed", "summary": "no access"}), Some(&s)).is_ok());
        assert!(ResultSchema::new(json!({"type": 5})).is_err());
    }

    #[test]
    fn stored_form_round_trips() {
        let o = Some(RunOutcome::new("partial", "max_turns", "half"));
        assert_eq!(RunOutcome::from_json(&RunOutcome::to_json(&o)), o);
        assert_eq!(RunOutcome::to_json(&None), "");
        assert_eq!(RunOutcome::from_json(""), None);
    }
}
