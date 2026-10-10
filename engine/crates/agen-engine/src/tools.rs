//! Tool port. Tools come from MCP servers, the host application (SDK custom
//! tools) or the engine itself (e.g. `load_skill`).

use std::collections::BTreeMap;
use std::sync::Arc;

use async_trait::async_trait;
use serde_json::Value;

use crate::provider::ToolSpec;
use crate::secrets::Secrets;

#[derive(Debug, Clone, thiserror::Error)]
#[error("{message}")]
pub struct ToolError {
    pub message: String,
    /// The call may or may not have taken effect (timeout, lost connection).
    /// Side-effecting calls with this error are reported as `effect_unknown`
    /// and never recorded as completed.
    pub outcome_unknown: bool,
}

impl ToolError {
    pub fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            outcome_unknown: false,
        }
    }

    pub fn unknown(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            outcome_unknown: true,
        }
    }
}

/// What a tool sees when called.
#[derive(Clone, Default)]
pub struct ToolContext {
    pub namespace: String,
    pub deployment: String,
    pub task_id: String,
    pub run_id: String,
    pub conversation_id: String,
    /// The caller's key for the conversation ("" when none was given).
    pub conversation_key: String,
    pub labels: BTreeMap<String, String>,
    pub call_id: String,
    /// The run step (model turn) the call belongs to.
    pub step: i64,
    pub secrets: Secrets,
    pub cancel: tokio_util::sync::CancellationToken,
    /// W3C traceparent of the tool's span (for calls to other agents).
    pub traceparent: String,
    /// Root of the delegation tree ("" = this run is the root).
    pub root_run_id: String,
    /// Delegation depth of this run (0 for a run nobody delegated).
    pub depth: u32,
}

#[async_trait]
pub trait Tool: Send + Sync {
    fn spec(&self) -> ToolSpec;
    /// True if calling this tool changes the outside world. Side-effecting
    /// calls go through the effect ledger and are never silently repeated.
    fn side_effect(&self) -> bool {
        true
    }
    /// A key that makes repeating this call safe (e.g. an order client id).
    fn idempotency_key(&self, _args: &Value) -> Option<String> {
        None
    }
    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError>;
}

#[derive(Clone, Default)]
pub struct ToolRegistry {
    tools: BTreeMap<String, Arc<dyn Tool>>,
}

impl ToolRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Add a tool; tool names must be unique.
    pub fn register(&mut self, tool: Arc<dyn Tool>) -> Result<(), String> {
        let name = tool.spec().name;
        if self.tools.contains_key(&name) {
            return Err(format!("duplicate tool name {name:?}"));
        }
        self.tools.insert(name, tool);
        Ok(())
    }

    pub fn get(&self, name: &str) -> Option<Arc<dyn Tool>> {
        self.tools.get(name).cloned()
    }

    pub fn specs(&self) -> Vec<ToolSpec> {
        self.tools.values().map(|t| t.spec()).collect()
    }

    pub fn len(&self) -> usize {
        self.tools.len()
    }

    pub fn is_empty(&self) -> bool {
        self.tools.is_empty()
    }
}

/// A tool built from a closure, for SDKs and tests.
pub struct FnTool<F> {
    spec: ToolSpec,
    side_effect: bool,
    f: F,
}

impl<F, Fut> FnTool<F>
where
    F: Fn(Value, ToolContext) -> Fut + Send + Sync,
    Fut: std::future::Future<Output = Result<String, ToolError>> + Send,
{
    pub fn new(name: &str, description: &str, parameters: Value, side_effect: bool, f: F) -> Self {
        Self {
            spec: ToolSpec {
                name: name.into(),
                description: description.into(),
                parameters,
            },
            side_effect,
            f,
        }
    }
}

#[async_trait]
impl<F, Fut> Tool for FnTool<F>
where
    F: Fn(Value, ToolContext) -> Fut + Send + Sync,
    Fut: std::future::Future<Output = Result<String, ToolError>> + Send,
{
    fn spec(&self) -> ToolSpec {
        self.spec.clone()
    }
    fn side_effect(&self) -> bool {
        self.side_effect
    }
    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        (self.f)(args, ctx).await
    }
}

/// Progressive skill loading: the system prompt lists skill names and
/// descriptions; the model calls `load_skill` to read one in full.
pub struct LoadSkill {
    skills: BTreeMap<String, String>,
}

impl LoadSkill {
    pub fn new(skills: &[crate::bundle::Skill]) -> Self {
        Self {
            skills: skills.iter().map(|s| (s.name.clone(), s.body.clone())).collect(),
        }
    }
}

#[async_trait]
impl Tool for LoadSkill {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: "load_skill".into(),
            description: "Load the full instructions of one of your skills by name.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {"name": {"type": "string", "enum": self.skills.keys().collect::<Vec<_>>()}},
                "required": ["name"]
            }),
        }
    }
    fn side_effect(&self) -> bool {
        false
    }
    async fn call(&self, args: Value, _ctx: ToolContext) -> Result<String, ToolError> {
        let name = args.get("name").and_then(Value::as_str).unwrap_or_default();
        self.skills
            .get(name)
            .cloned()
            .ok_or_else(|| ToolError::new(format!("no skill named {name:?}")))
    }
}
