//! `report_progress` (3.4, part): a built-in tool an agent calls to note
//! what it is doing, so a caller or a person watching sees more than
//! "running" for a long task. Shows up on the task (its latest value) and
//! as a rate-limited `task.progress` event (the history). Managed mode
//! only: without a Manager to report to, the call just no-ops with a
//! friendly result, same as `call_agent`'s A2A path degrades gracefully
//! without a resolver.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::{json, Value};

use crate::delegate::TaskStarter;
use crate::provider::ToolSpec;
use crate::tools::{Tool, ToolContext, ToolError};

pub const REPORT_PROGRESS: &str = "report_progress";

pub struct ReportProgress {
    starter: Option<Arc<dyn TaskStarter>>,
}

impl ReportProgress {
    pub fn new(starter: Option<Arc<dyn TaskStarter>>) -> Self {
        Self { starter }
    }
}

#[async_trait]
impl Tool for ReportProgress {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: REPORT_PROGRESS.into(),
            description: "Report progress on the current task: a short description of the current step, whether \
                           the work is done, how many steps there are in total (optional), a note, and any \
                           artifact ids made so far. Visible to whoever is watching the task; does not end the run."
                .into(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "step": {"type": "string", "description": "What is happening right now, e.g. \"reading page 3 of 12\""},
                    "done": {"type": "boolean", "description": "Default false"},
                    "total": {"type": "integer", "description": "Total number of steps, if known"},
                    "note": {"type": "string"},
                    "artifacts": {"type": "array", "items": {"type": "string"}, "description": "Ids of artifacts made so far"}
                },
                "required": ["step"],
                "additionalProperties": false
            }),
        }
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let step = args.get("step").and_then(Value::as_str).unwrap_or_default();
        if step.is_empty() {
            return Err(ToolError::new("step is required"));
        }
        let Some(starter) = &self.starter else {
            return Ok("progress noted (not reported anywhere: no managed platform configured)".into());
        };
        let progress = json!({
            "step": step,
            "done": args.get("done").and_then(Value::as_bool).unwrap_or(false),
            "total": args.get("total").and_then(Value::as_i64).unwrap_or(0),
            "note": args.get("note").and_then(Value::as_str).unwrap_or(""),
            "artifacts": args.get("artifacts").cloned().unwrap_or_else(|| json!([])),
        });
        starter
            .report_progress(&ctx.task_id, progress)
            .await
            .map_err(ToolError::new)?;
        Ok("progress reported".into())
    }
}
