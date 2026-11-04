//! `acquire_lock`/`release_lock` (1.5, part): built-in tools for a lock
//! named mid-run, on top of a task's own locks declared up front
//! (`SubmitTask.locks`, taken all at once before the task is leased).
//! Both reuse the same `locks` table a declared lock is granted into, so
//! the same uniqueness rule (one task per namespace+name) keeps two runs
//! from ever holding the same resource whether its lock was declared or
//! named on the fly. A grant's fencing token — advancing only when the
//! lock changes hands, never on a re-grant to the same holder — is the
//! one thing an expired lease alone can't give a tool server outside the
//! Hub's own control: it lets that server remember the highest token it
//! has seen for a resource and reject a write tagged with a stale one.
//! Managed mode only: without a Manager these no-op with a friendly
//! result, same as the other built-ins that need one.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{json, Value};

use crate::delegate::TaskStarter;
use crate::provider::ToolSpec;
use crate::tools::{Tool, ToolContext, ToolError};

pub const ACQUIRE_LOCK: &str = "acquire_lock";
pub const RELEASE_LOCK: &str = "release_lock";

pub struct AcquireLock {
    starter: Option<Arc<dyn TaskStarter>>,
}

impl AcquireLock {
    pub fn new(starter: Option<Arc<dyn TaskStarter>>) -> Self {
        Self { starter }
    }
}

#[async_trait]
impl Tool for AcquireLock {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: ACQUIRE_LOCK.into(),
            description: "Grant yourself an exclusive named lock on a resource not already covered by this task's \
                           own declared locks, for as long as ttl_seconds (then it expires on its own). Fails with \
                           a clear error if another task currently holds it; try again later or pick a different \
                           name. Release it with release_lock once you're done, or let it expire."
                .into(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "name": {"type": "string", "description": "The resource's lock name"},
                    "ttl_seconds": {"type": "integer", "description": "How long to hold it, 1-86400"}
                },
                "required": ["name", "ttl_seconds"],
                "additionalProperties": false
            }),
        }
    }

    fn side_effect(&self) -> bool {
        true
    }

    fn idempotency_key(&self, args: &Value) -> Option<String> {
        args.get("name")
            .and_then(Value::as_str)
            .map(|n| format!("acquire_lock:{n}"))
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let name = args.get("name").and_then(Value::as_str).unwrap_or_default();
        if name.is_empty() {
            return Err(ToolError::new("name is required"));
        }
        let ttl = args.get("ttl_seconds").and_then(Value::as_i64).unwrap_or(0);
        if !(1..=86400).contains(&ttl) {
            return Err(ToolError::new("ttl_seconds must be 1-86400"));
        }
        let Some(starter) = &self.starter else {
            return Ok(format!(
                "lock {name:?} granted (not tracked anywhere: no managed platform configured)"
            ));
        };
        let fence = starter
            .acquire_lock(&ctx.task_id, name, Duration::from_secs(ttl as u64))
            .await
            .map_err(ToolError::new)?;
        ctx.lock_fences.lock().unwrap().insert(name.to_string(), fence);
        Ok(format!("lock {name:?} granted (fence {fence})"))
    }
}

pub struct ReleaseLock {
    starter: Option<Arc<dyn TaskStarter>>,
}

impl ReleaseLock {
    pub fn new(starter: Option<Arc<dyn TaskStarter>>) -> Self {
        Self { starter }
    }
}

#[async_trait]
impl Tool for ReleaseLock {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: RELEASE_LOCK.into(),
            description: "Release a lock you hold before it would otherwise expire, whether it was declared up \
                           front or acquired with acquire_lock. Releasing one you don't currently hold is harmless."
                .into(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "name": {"type": "string", "description": "The resource's lock name"}
                },
                "required": ["name"],
                "additionalProperties": false
            }),
        }
    }

    fn side_effect(&self) -> bool {
        true
    }

    fn idempotency_key(&self, args: &Value) -> Option<String> {
        args.get("name")
            .and_then(Value::as_str)
            .map(|n| format!("release_lock:{n}"))
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let name = args.get("name").and_then(Value::as_str).unwrap_or_default();
        if name.is_empty() {
            return Err(ToolError::new("name is required"));
        }
        let Some(starter) = &self.starter else {
            return Ok(format!(
                "lock {name:?} released (not tracked anywhere: no managed platform configured)"
            ));
        };
        starter.release_lock(&ctx.task_id, name).await.map_err(ToolError::new)?;
        ctx.lock_fences.lock().unwrap().remove(name);
        Ok(format!("lock {name:?} released"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn without_a_starter_both_tools_no_op_with_a_friendly_result() {
        let acquire = AcquireLock::new(None);
        let r = acquire
            .call(json!({"name": "x", "ttl_seconds": 60}), ToolContext::default())
            .await
            .unwrap();
        assert!(r.contains("no managed platform"), "{r}");
        let release = ReleaseLock::new(None);
        let r = release
            .call(json!({"name": "x"}), ToolContext::default())
            .await
            .unwrap();
        assert!(r.contains("no managed platform"), "{r}");
    }

    #[test]
    fn ttl_bounds_are_validated_before_ever_reaching_a_starter() {
        // A missing starter already no-ops successfully, so this checks
        // validation runs first by using one that would panic if called.
        struct Unreachable;
        #[async_trait]
        impl TaskStarter for Unreachable {
            async fn start(&self, _req: crate::delegate::StartRequest<'_>) -> Result<String, String> {
                unreachable!()
            }
            async fn wait(&self, _task_id: &str, _wait: Duration) -> Result<crate::delegate::WaitOutcome, String> {
                unreachable!()
            }
            async fn report_progress(&self, _task_id: &str, _progress: Value) -> Result<(), String> {
                unreachable!()
            }
            async fn query_descendant(&self, _caller_task_id: &str, _task_id: &str) -> Result<Value, String> {
                unreachable!()
            }
            async fn signal_descendant(
                &self,
                _caller_task_id: &str,
                _task_id: &str,
                _message: &str,
            ) -> Result<(), String> {
                unreachable!()
            }
            async fn drain_inbox(&self, _task_id: &str) -> Result<Vec<Value>, String> {
                unreachable!()
            }
            async fn unfinished_children(&self, _task_id: &str) -> Result<i64, String> {
                unreachable!()
            }
            async fn is_held(&self, _task_id: &str) -> Result<bool, String> {
                unreachable!()
            }
            async fn acquire_lock(&self, _task_id: &str, _name: &str, _ttl: Duration) -> Result<i64, String> {
                unreachable!()
            }
            async fn release_lock(&self, _task_id: &str, _name: &str) -> Result<(), String> {
                unreachable!()
            }
            async fn tree_budget_exceeded(&self, _task_id: &str) -> Result<bool, String> {
                unreachable!()
            }
        }
        let rt = tokio::runtime::Runtime::new().unwrap();
        rt.block_on(async {
            let tool = AcquireLock::new(Some(Arc::new(Unreachable)));
            let err = tool
                .call(json!({"name": "x", "ttl_seconds": 0}), ToolContext::default())
                .await
                .unwrap_err();
            assert!(err.message.contains("ttl_seconds"), "{err}");
            let err = tool
                .call(json!({"name": "", "ttl_seconds": 60}), ToolContext::default())
                .await
                .unwrap_err();
            assert!(err.message.contains("name"), "{err}");
        });
    }
}
