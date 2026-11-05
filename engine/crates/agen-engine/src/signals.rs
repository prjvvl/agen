//! `signal_agent` / `query_agent` (3.4, part): a run checks in on, or sends
//! a message to, one of its own descendants (a task it delegated to,
//! directly or transitively), without going through the delegation tools.
//! Managed mode only (like `report_progress`): without a starter, both
//! tools no-op with a friendly result.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::{json, Value};

use crate::delegate::TaskStarter;
use crate::provider::ToolSpec;
use crate::tools::{Tool, ToolContext, ToolError};

pub const SIGNAL_AGENT: &str = "signal_agent";
pub const QUERY_AGENT: &str = "query_agent";

pub struct SignalAgent {
    starter: Option<Arc<dyn TaskStarter>>,
}

impl SignalAgent {
    pub fn new(starter: Option<Arc<dyn TaskStarter>>) -> Self {
        Self { starter }
    }
}

#[async_trait]
impl Tool for SignalAgent {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: SIGNAL_AGENT.into(),
            description: "Send a message to a task you started (directly, or one it in turn started), without \
                           waiting for an answer. Only works on your own descendants."
                .into(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "task": {"type": "string", "description": "The descendant task's id"},
                    "message": {"type": "string"}
                },
                "required": ["task", "message"],
                "additionalProperties": false
            }),
        }
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let task = args.get("task").and_then(Value::as_str).unwrap_or_default();
        let message = args.get("message").and_then(Value::as_str).unwrap_or_default();
        if task.is_empty() || message.is_empty() {
            return Err(ToolError::new("task and message are required"));
        }
        let Some(starter) = &self.starter else {
            return Ok("not sent: no managed platform configured".into());
        };
        starter
            .signal_descendant(&ctx.task_id, task, message)
            .await
            .map_err(ToolError::new)?;
        Ok(format!("sent to task {task}"))
    }
}

pub struct QueryAgent {
    starter: Option<Arc<dyn TaskStarter>>,
}

impl QueryAgent {
    pub fn new(starter: Option<Arc<dyn TaskStarter>>) -> Self {
        Self { starter }
    }
}

#[async_trait]
impl Tool for QueryAgent {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: QUERY_AGENT.into(),
            description: "Check in on a task you started (directly, or one it in turn started): its state, \
                           current activity, latest reported progress and outcome, without touching it. Only \
                           works on your own descendants."
                .into(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "task": {"type": "string", "description": "The descendant task's id"}
                },
                "required": ["task"],
                "additionalProperties": false
            }),
        }
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let task = args.get("task").and_then(Value::as_str).unwrap_or_default();
        if task.is_empty() {
            return Err(ToolError::new("task is required"));
        }
        let Some(starter) = &self.starter else {
            return Ok("no managed platform configured".into());
        };
        let v = starter
            .query_descendant(&ctx.task_id, task)
            .await
            .map_err(ToolError::new)?;
        Ok(format_query(&v))
    }
}

/// Renders a `QueryTaskResponse` (JSON, as the Manager answers it) as text
/// for the model.
fn format_query(v: &Value) -> String {
    let state = v
        .get("state")
        .and_then(Value::as_str)
        .unwrap_or("TASK_STATE_UNSPECIFIED")
        .trim_start_matches("TASK_STATE_")
        .to_lowercase();
    let mut out = format!("task {}: {state}", v.get("id").and_then(Value::as_str).unwrap_or(""));
    if let Some(a) = v.get("activity").and_then(Value::as_str).filter(|s| !s.is_empty()) {
        match v.get("detail").and_then(Value::as_str).filter(|s| !s.is_empty()) {
            Some(d) => out.push_str(&format!(" | activity: {a} ({d})")),
            None => out.push_str(&format!(" | activity: {a}")),
        }
    }
    if let Some(p) = v.get("progress").filter(|p| !p.is_null()) {
        let step = p.get("step").and_then(Value::as_str).unwrap_or("");
        let done = p.get("done").and_then(Value::as_bool).unwrap_or(false);
        out.push_str(&format!(" | progress: {step} (done={done})"));
    }
    if let Some(o) = v.get("outcome").filter(|o| !o.is_null()) {
        if let Some(s) = o.get("summary").and_then(Value::as_str).filter(|s| !s.is_empty()) {
            out.push_str(&format!(" | outcome: {s}"));
        }
    }
    out
}
