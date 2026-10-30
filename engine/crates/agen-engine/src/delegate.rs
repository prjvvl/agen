//! Delegation to other agents over A2A (docs/architecture.md §6).
//!
//! A bundle lists the deployments it may call in `x-agen/config.json`
//! `delegates`; the engine exposes one `call_agent` tool for them. A call is
//! an A2A `message/send` to the target's Gateway, carrying the W3C
//! `traceparent` of the tool span and the lineage (`agen.depth`,
//! `agen.root_run_id`, `agen.parent_run_id`), so the callee's run joins the
//! caller's trace. The message id is derived from the run, step and call, and
//! Gateways treat a repeated message id as the same task, so a retried or
//! resumed call never runs the callee twice.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::bundle::{Delegate, Limits, Trust};
use crate::provider::ToolSpec;
use crate::store::Store;
use crate::tools::{Tool, ToolContext, ToolError};

/// Where to reach a deployment over A2A, and the credential to present.
#[derive(Debug, Clone, Default)]
pub struct Resolved {
    pub endpoints: Vec<String>,
    /// A short-lived call token for these endpoints (sent as a bearer
    /// token); Gateways that require callers to authenticate refuse calls
    /// without it.
    pub token: Option<String>,
}

/// Finds the A2A endpoints of a deployment.
#[async_trait]
pub trait Resolver: Send + Sync {
    async fn resolve(&self, namespace: &str, name: &str) -> Result<Resolved, String>;
}

/// Resolves through the Nest Manager's localhost ManagerService (managed
/// mode: `AGEN_MANAGER_URL` + `AGEN_MANAGER_TOKEN`). Hosts never call the Hub.
pub struct ManagerResolver {
    url: String,
    token: String,
    http: reqwest::Client,
}

impl ManagerResolver {
    pub fn new(url: &str, token: &str) -> Self {
        Self {
            url: url.trim_end_matches('/').to_string(),
            token: token.to_string(),
            http: reqwest::Client::new(),
        }
    }

    /// From `AGEN_MANAGER_URL` / `AGEN_MANAGER_TOKEN`, if set.
    pub fn from_env() -> Option<Self> {
        let url = std::env::var("AGEN_MANAGER_URL").ok().filter(|u| !u.is_empty())?;
        let token = std::env::var("AGEN_MANAGER_TOKEN").unwrap_or_default();
        Some(Self::new(&url, &token))
    }
}

#[async_trait]
impl Resolver for ManagerResolver {
    async fn resolve(&self, namespace: &str, name: &str) -> Result<Resolved, String> {
        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct Out {
            endpoints: Vec<String>,
            token: String,
            message: String,
        }
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/Resolve", self.url))
            .bearer_auth(&self.token)
            .json(&json!({"ref": {"namespace": namespace, "name": name}}))
            .timeout(Duration::from_secs(30))
            .send()
            .await
            .map_err(|e| format!("manager unreachable: {e}"))?;
        let ok = resp.status().is_success();
        let out: Out = resp.json().await.unwrap_or_default();
        if !ok {
            return Err(format!("resolve {namespace}/{name}: {}", out.message));
        }
        Ok(Resolved {
            endpoints: out.endpoints,
            token: Some(out.token).filter(|t| !t.is_empty()),
        })
    }
}

/// Queues tasks on other deployments for a run (managed mode: the Manager).
#[async_trait]
pub trait TaskStarter: Send + Sync {
    /// Returns the new task's id.
    async fn start(&self, req: StartRequest<'_>) -> Result<String, String>;
    /// Waits up to `wait` for a task (typically one `start` returned) to
    /// reach a terminal state (3.3, part: `call_agent`'s unified delegation
    /// path, before it suspends instead of blocking indefinitely).
    async fn wait(&self, task_id: &str, wait: Duration) -> Result<WaitOutcome, String>;
    /// Reports progress on the current task (3.4, part: the `report_progress`
    /// built-in), not one it started.
    async fn report_progress(&self, task_id: &str, progress: Value) -> Result<(), String>;
    /// Progress, activity and outcome of one of the current task's own
    /// descendants (3.4, part: the `query_agent` built-in). The JSON shape
    /// of `QueryTaskResponse`.
    async fn query_descendant(&self, caller_task_id: &str, task_id: &str) -> Result<Value, String>;
    /// Signals one of the current task's own descendants (3.4, part: the
    /// `signal_agent` built-in).
    async fn signal_descendant(&self, caller_task_id: &str, task_id: &str, message: &str) -> Result<(), String>;
    /// Claims the current task's unconsumed run_inbox entries other than
    /// "child" rows (3.4/3.9, part: draining a suspended run's inbox into
    /// one it is resuming is `call_agent`'s own re-check of the child's
    /// status, not this). Each claimed entry is a JSON object
    /// `{"kind", "ref", "payload"}`; claiming is atomic on the Hub side, so
    /// a retried call after a transport error never redelivers one already
    /// claimed.
    async fn drain_inbox(&self, task_id: &str) -> Result<Vec<Value>, String>;
}

/// What waiting for a task found.
pub enum WaitOutcome {
    /// It reached a terminal state.
    Done {
        success: bool,
        output: String,
        error: String,
        outcome: Option<Value>,
    },
    /// Still not done when the wait elapsed.
    Pending,
}

/// One async delegation.
pub struct StartRequest<'a> {
    pub namespace: &'a str,
    pub name: &'a str,
    pub input: &'a str,
    pub run_id: &'a str,
    pub traceparent: &'a str,
    pub reply_conversation_key: &'a str,
    pub idempotency_key: &'a str,
    /// Narrows what the child may do (only for delegates with `overrides: true`).
    pub overrides: Option<&'a crate::overrides::TaskOverrides>,
    pub model_tier: &'a str,
    /// The caller's trust right now; the child inherits it as is (a child
    /// never has higher trust than its caller's current state).
    pub trust: Trust,
}

#[async_trait]
impl TaskStarter for ManagerResolver {
    async fn start(&self, req: StartRequest<'_>) -> Result<String, String> {
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/StartTask", self.url))
            .bearer_auth(&self.token)
            .json(&json!({
                "ref": {"namespace": req.namespace, "name": req.name},
                "input": req.input,
                "runId": req.run_id,
                "traceparent": req.traceparent,
                "replyConversationKey": req.reply_conversation_key,
                "idempotencyKey": req.idempotency_key,
                "overrides": req.overrides,
                "modelTier": req.model_tier,
                "trust": req.trust.as_str(),
            }))
            .timeout(Duration::from_secs(60))
            .send()
            .await
            .map_err(|e| format!("manager unreachable: {e}"))?;
        let ok = resp.status().is_success();
        let v: Value = resp.json().await.unwrap_or(Value::Null);
        if !ok {
            return Err(format!(
                "start {}/{}: {}",
                req.namespace,
                req.name,
                v.get("message").and_then(Value::as_str).unwrap_or("failed")
            ));
        }
        v.pointer("/task/id")
            .and_then(Value::as_str)
            .map(str::to_string)
            .ok_or_else(|| "manager answered without a task".into())
    }

    async fn wait(&self, task_id: &str, wait: Duration) -> Result<WaitOutcome, String> {
        let wait_secs = wait.as_secs().clamp(1, i32::MAX as u64);
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/WaitTask", self.url))
            .bearer_auth(&self.token)
            .json(&json!({"taskId": task_id, "waitSeconds": wait_secs}))
            .timeout(wait + Duration::from_secs(15))
            .send()
            .await
            .map_err(|e| format!("manager unreachable: {e}"))?;
        let ok = resp.status().is_success();
        let v: Value = resp.json().await.unwrap_or(Value::Null);
        if !ok {
            return Err(format!(
                "wait for task {task_id}: {}",
                v.get("message").and_then(Value::as_str).unwrap_or("failed")
            ));
        }
        let task = v.get("task").cloned().unwrap_or(Value::Null);
        match task.get("state").and_then(Value::as_str).unwrap_or("") {
            "TASK_STATE_SUCCEEDED" | "TASK_STATE_FAILED" | "TASK_STATE_CANCELLED" => Ok(WaitOutcome::Done {
                success: task.get("state").and_then(Value::as_str) == Some("TASK_STATE_SUCCEEDED"),
                output: task
                    .get("output")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string(),
                error: task
                    .get("error")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string(),
                outcome: task.get("outcome").cloned().filter(|v| !v.is_null()),
            }),
            _ => Ok(WaitOutcome::Pending),
        }
    }

    async fn report_progress(&self, task_id: &str, progress: Value) -> Result<(), String> {
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/ReportProgress", self.url))
            .bearer_auth(&self.token)
            .json(&json!({"taskId": task_id, "progress": progress}))
            .timeout(Duration::from_secs(15))
            .send()
            .await
            .map_err(|e| format!("manager unreachable: {e}"))?;
        if !resp.status().is_success() {
            let v: Value = resp.json().await.unwrap_or(Value::Null);
            return Err(format!(
                "report progress for task {task_id}: {}",
                v.get("message").and_then(Value::as_str).unwrap_or("failed")
            ));
        }
        Ok(())
    }

    async fn query_descendant(&self, caller_task_id: &str, task_id: &str) -> Result<Value, String> {
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/QueryChildTask", self.url))
            .bearer_auth(&self.token)
            .json(&json!({"callerTaskId": caller_task_id, "taskId": task_id}))
            .timeout(Duration::from_secs(15))
            .send()
            .await
            .map_err(|e| format!("manager unreachable: {e}"))?;
        let ok = resp.status().is_success();
        let v: Value = resp.json().await.unwrap_or(Value::Null);
        if !ok {
            return Err(format!(
                "query task {task_id}: {}",
                v.get("message").and_then(Value::as_str).unwrap_or("failed")
            ));
        }
        Ok(v)
    }

    async fn signal_descendant(&self, caller_task_id: &str, task_id: &str, message: &str) -> Result<(), String> {
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/SignalChildTask", self.url))
            .bearer_auth(&self.token)
            .json(&json!({"callerTaskId": caller_task_id, "taskId": task_id, "message": message}))
            .timeout(Duration::from_secs(15))
            .send()
            .await
            .map_err(|e| format!("manager unreachable: {e}"))?;
        if !resp.status().is_success() {
            let v: Value = resp.json().await.unwrap_or(Value::Null);
            return Err(format!(
                "signal task {task_id}: {}",
                v.get("message").and_then(Value::as_str).unwrap_or("failed")
            ));
        }
        Ok(())
    }

    async fn drain_inbox(&self, task_id: &str) -> Result<Vec<Value>, String> {
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/DrainInbox", self.url))
            .bearer_auth(&self.token)
            .json(&json!({"taskId": task_id}))
            .timeout(Duration::from_secs(10))
            .send()
            .await
            .map_err(|e| format!("manager unreachable: {e}"))?;
        let ok = resp.status().is_success();
        let v: Value = resp.json().await.unwrap_or(Value::Null);
        if !ok {
            return Err(format!(
                "drain inbox for task {task_id}: {}",
                v.get("message").and_then(Value::as_str).unwrap_or("failed")
            ));
        }
        Ok(v.get("entries").and_then(Value::as_array).cloned().unwrap_or_default())
    }
}

/// Name of the tool [`CallAgent`] provides.
pub const CALL_AGENT: &str = "call_agent";
/// Name of the tool [`StartAgent`] provides.
pub const START_AGENT: &str = "start_agent";

/// The `start_agent` tool: async delegation. The task is queued on the
/// target and the tool returns at once; when it ends, its result arrives in
/// this conversation as a new message (the Hub submits it as a task with the
/// conversation's key), so long work does not hold the caller's run open.
pub struct StartAgent {
    delegates: Vec<Delegate>,
    default_namespace: String,
    starter: Arc<dyn TaskStarter>,
    limits: Limits,
    store: Arc<Store>,
}

impl StartAgent {
    pub fn new(
        delegates: Vec<Delegate>,
        default_namespace: &str,
        starter: Arc<dyn TaskStarter>,
        limits: Limits,
        store: Arc<Store>,
    ) -> Self {
        Self {
            delegates,
            default_namespace: default_namespace.to_string(),
            starter,
            limits,
            store,
        }
    }
}

#[async_trait]
impl Tool for StartAgent {
    fn spec(&self) -> ToolSpec {
        let names: Vec<&str> = self
            .delegates
            .iter()
            .filter(|d| d.url.is_none())
            .map(|d| d.name.as_str())
            .collect();
        let with_overrides: Vec<&str> = self
            .delegates
            .iter()
            .filter(|d| d.url.is_none() && d.overrides)
            .map(|d| d.name.as_str())
            .collect();
        let overrides_note = if with_overrides.is_empty() {
            String::new()
        } else {
            format!(
                " For {}, you may also narrow the brief with tools, skills, budget and modelTier.",
                with_overrides.join(", ")
            )
        };
        ToolSpec {
            name: START_AGENT.into(),
            description: format!(
                "Start another agent on a task without waiting for it (for long work). The tool returns at once; \
                 the agent's answer arrives later in this conversation as a new message that begins with \
                 \"[Result of the task you started on ...]\". After starting, finish your current turn; do not poll.{overrides_note}"
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "agent": {"type": "string", "enum": names},
                    "message": {"type": "string", "description": "What the agent should do"},
                    "tools": {"type": "array", "items": {"type": "string"}, "description": "Only let it call these of its tools (narrows; only for delegates that accept overrides)"},
                    "skills": {"type": "array", "items": {"type": "string"}, "description": "Skills of its bundle to preload"},
                    "budget": {
                        "type": "object",
                        "description": "Run limits, at most its own",
                        "properties": {
                            "maxTokens": {"type": "integer"},
                            "maxUsd": {"type": "number"},
                            "maxTurns": {"type": "integer"},
                            "maxDuration": {"type": "string", "description": "e.g. \"20m\""}
                        },
                        "additionalProperties": false
                    },
                    "modelTier": {"type": "string", "description": "Cost tier for a routing model"}
                },
                "required": ["agent", "message"],
                "additionalProperties": false
            }),
        }
    }

    // A repeated call (retry, resume) is the same task: the idempotency key
    // is derived from the run, step and call.
    fn idempotency_key(&self, _args: &Value) -> Option<String> {
        Some("start-agent-task".into())
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let agent = args.get("agent").and_then(Value::as_str).unwrap_or_default();
        let message = args.get("message").and_then(Value::as_str).unwrap_or_default();
        let d = self
            .delegates
            .iter()
            .find(|d| d.name == agent && d.url.is_none())
            .ok_or_else(|| ToolError::new(format!("unknown agent {agent:?}")))?;
        let overrides = crate::overrides::TaskOverrides::from_call_args(&args).map_err(ToolError::new)?;
        if overrides.is_some() && !d.overrides {
            return Err(ToolError::new(format!(
                "{agent} does not accept overrides (its delegate entry needs \"overrides\": true)"
            )));
        }
        let model_tier = args.get("modelTier").and_then(Value::as_str).unwrap_or_default();
        let max = self.limits.delegation_depth();
        if max > 0 && ctx.depth + 1 > max {
            return Err(ToolError::new(format!(
                "delegation depth {} exceeds max_delegation_depth {max}",
                ctx.depth + 1
            )));
        }
        let root = if ctx.root_run_id.is_empty() {
            ctx.run_id.clone()
        } else {
            ctx.root_run_id.clone()
        };
        let call_key = format!("{}.{}.{}", ctx.run_id, ctx.step, ctx.call_id);
        if let Some((per_run, total)) = self
            .store
            .record_delegation(&call_key, &root, &ctx.run_id)
            .await
            .map_err(|e| ToolError::new(e.to_string()))?
        {
            let over = match (self.limits.fan_out(), self.limits.total_delegations()) {
                (m, _) if m > 0 && per_run > i64::from(m) => Some(format!(
                    "this run already made {} delegated calls (max_fan_out {m})",
                    per_run - 1
                )),
                (_, m) if m > 0 && total > i64::from(m) => Some(format!(
                    "the run tree already made {} delegated calls (max_total_delegations {m})",
                    total - 1
                )),
                _ => None,
            };
            if let Some(msg) = over {
                let _ = self.store.forget_delegation(&call_key).await;
                return Err(ToolError::new(msg));
            }
        }
        // The result needs a conversation to come back to.
        let reply_key = if ctx.conversation_key.is_empty() {
            self.store
                .ensure_conversation_key(&ctx.conversation_id)
                .await
                .map_err(|e| ToolError::new(e.to_string()))?
        } else {
            ctx.conversation_key.clone()
        };
        let ns = d.namespace.clone().unwrap_or_else(|| self.default_namespace.clone());
        let id = self
            .starter
            .start(StartRequest {
                namespace: &ns,
                name: &d.name,
                input: message,
                run_id: &ctx.run_id,
                traceparent: &ctx.traceparent,
                reply_conversation_key: &reply_key,
                idempotency_key: &format!("start:{call_key}"),
                overrides: overrides.as_ref(),
                model_tier,
                trust: ctx.trust,
            })
            .await
            .map_err(ToolError::new)?;
        Ok(format!(
            "Started task {id} on {agent}. Its result will arrive in this conversation as a new message when it finishes; \
             finish your current turn now."
        ))
    }
}

/// The `call_agent` tool.
pub struct CallAgent {
    delegates: Vec<Delegate>,
    default_namespace: String,
    resolver: Option<Arc<dyn Resolver>>,
    /// Starts a delegate as a child task and waits on it (3.2b/3.3: the
    /// unified path every platform delegate uses unless it opts into plain
    /// A2A). None in standalone/embedded mode, where every delegate must be
    /// `url` or resolved A2A.
    starter: Option<Arc<dyn TaskStarter>>,
    limits: Limits,
    store: Arc<Store>,
    http: reqwest::Client,
    timeout: Duration,
}

impl CallAgent {
    pub fn new(
        delegates: Vec<Delegate>,
        default_namespace: &str,
        resolver: Option<Arc<dyn Resolver>>,
        starter: Option<Arc<dyn TaskStarter>>,
        limits: Limits,
        store: Arc<Store>,
    ) -> Self {
        Self {
            delegates,
            default_namespace: default_namespace.to_string(),
            resolver,
            starter,
            limits,
            store,
            http: reqwest::Client::new(),
            // A Gateway lets a call wait up to 10 min for a free instance and
            // run for up to 1 h.
            timeout: Duration::from_secs(75 * 60),
        }
    }

    /// `call_agent`'s unified path (3.2b/3.3) for a platform delegate: start
    /// it as a child task and wait on it, same as `start_agent` would, but
    /// inline — up to `limits.inlineWait` before suspending the whole run
    /// instead of blocking it (the caller resumes once the child is done,
    /// re-running this exact call, which by then finds it done at once).
    async fn call_unified(
        &self,
        starter: &Arc<dyn TaskStarter>,
        d: &Delegate,
        message: &str,
        ctx: &ToolContext,
        call_key: &str,
    ) -> Result<String, ToolError> {
        let ns = d.namespace.clone().unwrap_or_else(|| self.default_namespace.clone());
        let task_id = starter
            .start(StartRequest {
                namespace: &ns,
                name: &d.name,
                input: message,
                run_id: &ctx.run_id,
                traceparent: &ctx.traceparent,
                // No reply task: the answer comes back as this call's
                // result when the caller's run resumes, not as a new
                // message in the conversation.
                reply_conversation_key: "",
                idempotency_key: &format!("call:{call_key}"),
                overrides: None,
                model_tier: "",
                trust: ctx.trust,
            })
            .await
            .map_err(ToolError::new)?;
        let outcome = tokio::select! {
            r = starter.wait(&task_id, self.limits.inline_wait()) => r.map_err(ToolError::new)?,
            _ = ctx.cancel.cancelled() => return Err(ToolError::unknown("cancelled")),
        };
        match outcome {
            WaitOutcome::Done {
                success: true,
                output,
                outcome,
                ..
            } => Ok(with_outcome(output, outcome.as_ref())),
            WaitOutcome::Done { error, .. } => Err(ToolError::new(if error.is_empty() {
                format!("agent {} (task {task_id}) did not finish successfully", d.name)
            } else {
                error
            })),
            WaitOutcome::Pending => Err(ToolError::suspend(format!("child:{task_id}"))),
        }
    }

    async fn endpoints(&self, d: &Delegate) -> Result<Resolved, ToolError> {
        if let Some(url) = &d.url {
            return Ok(Resolved {
                endpoints: vec![url.clone()],
                token: None,
            });
        }
        let ns = d.namespace.clone().unwrap_or_else(|| self.default_namespace.clone());
        let resolver = self.resolver.as_ref().ok_or_else(|| {
            ToolError::new(format!(
                "agent {:?} has no url and no resolver is configured (run it on the platform or set delegates[].url)",
                d.name
            ))
        })?;
        let eps = resolver.resolve(&ns, &d.name).await.map_err(ToolError::new)?;
        if eps.endpoints.is_empty() {
            return Err(ToolError::new(format!(
                "agent {ns}/{} is not reachable (no gateway)",
                d.name
            )));
        }
        Ok(eps)
    }

    /// One A2A message/send. Ok(text) on a completed task.
    async fn send(&self, endpoint: &str, token: Option<&str>, body: &Value) -> Result<String, SendError> {
        let mut req = self.http.post(endpoint);
        if let Some(t) = token {
            req = req.bearer_auth(t);
        }
        let resp = req
            .json(body)
            .timeout(self.timeout)
            .send()
            .await
            .map_err(|e| SendError::Transport(e.to_string()))?;
        let status = resp.status();
        let v: Value = resp.json().await.unwrap_or(Value::Null);
        if let Some(err) = v.get("error") {
            let msg = err
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or("error")
                .to_string();
            // 401: the call token expired or this Gateway does not know the
            // Hub key yet; another endpoint or a fresh resolve may work.
            return Err(
                if status.as_u16() == 503 || status.as_u16() == 404 || status.as_u16() == 401 {
                    SendError::Transport(msg)
                } else {
                    SendError::Remote(msg)
                },
            );
        }
        let result = v
            .get("result")
            .ok_or_else(|| SendError::Transport(format!("gateway answered {status}")))?;
        let state = result.pointer("/status/state").and_then(Value::as_str).unwrap_or("");
        let text_of = |parts: Option<&Value>| -> String {
            parts
                .and_then(Value::as_array)
                .map(|ps| {
                    ps.iter()
                        .filter_map(|p| p.get("text").and_then(Value::as_str))
                        .collect::<Vec<_>>()
                        .join("\n")
                })
                .unwrap_or_default()
        };
        let outcome = result.pointer("/metadata/agen.outcome");
        match state {
            "completed" | "input-required" | "rejected" => {
                let text = if state == "completed" {
                    result
                        .get("artifacts")
                        .and_then(Value::as_array)
                        .map(|arts| {
                            arts.iter()
                                .map(|a| text_of(a.get("parts")))
                                .filter(|t| !t.is_empty())
                                .collect::<Vec<_>>()
                                .join("\n")
                        })
                        .unwrap_or_default()
                } else {
                    text_of(result.pointer("/status/message/parts"))
                };
                Ok(with_outcome(text, outcome))
            }
            other => Err(SendError::Remote(format!(
                "agent task {other}: {}",
                text_of(result.pointer("/status/message/parts"))
            ))),
        }
    }
}

/// A delegate's answer with how its run ended, when that is not simply done:
/// `[partial: max_turns] <report>`, and its structured data as JSON.
fn with_outcome(text: String, outcome: Option<&Value>) -> String {
    let Some(o) = outcome else { return text };
    let status = o.get("status").and_then(Value::as_str).unwrap_or("done");
    let mut out = match (status, o.get("reason").and_then(Value::as_str)) {
        ("done", _) => text,
        (s, Some(r)) if !r.is_empty() => format!("[{s}: {r}] {text}"),
        (s, _) => format!("[{s}] {text}"),
    };
    if let Some(d) = o.get("data").filter(|d| !d.is_null()) {
        out.push_str(&format!("\ndata: {d}"));
    }
    out
}

/// Sends tasks/cancel (by message id) to the endpoint of a call in flight
/// when dropped; `endpoint` is cleared once the call has an answer.
struct CancelCallee {
    http: reqwest::Client,
    endpoint: Option<String>,
    token: Option<String>,
    message_id: String,
}

impl Drop for CancelCallee {
    fn drop(&mut self) {
        let Some(ep) = self.endpoint.take() else { return };
        let Ok(rt) = tokio::runtime::Handle::try_current() else {
            return;
        };
        let http = self.http.clone();
        let body = json!({"jsonrpc": "2.0", "id": 2, "method": "tasks/cancel",
            "params": {"id": "", "metadata": {"messageId": self.message_id}}});
        let token = self.token.take();
        rt.spawn(async move {
            let mut req = http.post(&ep);
            if let Some(t) = token {
                req = req.bearer_auth(t);
            }
            let _ = req.json(&body).timeout(Duration::from_secs(5)).send().await;
        });
    }
}

enum SendError {
    /// Not delivered or not answered: try another endpoint.
    Transport(String),
    /// The callee answered with a failure.
    Remote(String),
}

#[async_trait]
impl Tool for CallAgent {
    fn spec(&self) -> ToolSpec {
        let names: Vec<&str> = self.delegates.iter().map(|d| d.name.as_str()).collect();
        let listing: Vec<String> = self
            .delegates
            .iter()
            .map(|d| match &d.description {
                Some(desc) => format!("- {}: {desc}", d.name),
                None => format!("- {}", d.name),
            })
            .collect();
        ToolSpec {
            name: CALL_AGENT.into(),
            description: format!(
                "Ask another agent to do something and wait for its answer. Available agents:\n{}",
                listing.join("\n")
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "agent": {"type": "string", "enum": names},
                    "message": {"type": "string", "description": "What to ask the agent"}
                },
                "required": ["agent", "message"],
                "additionalProperties": false
            }),
        }
    }

    // Repeating a call is safe: the message id makes it the same A2A task.
    fn idempotency_key(&self, _args: &Value) -> Option<String> {
        Some("a2a-message-id".into())
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let agent = args.get("agent").and_then(Value::as_str).unwrap_or_default();
        let message = args.get("message").and_then(Value::as_str).unwrap_or_default();
        let d = self
            .delegates
            .iter()
            .find(|d| d.name == agent)
            .ok_or_else(|| ToolError::new(format!("unknown agent {agent:?}")))?;
        let depth = ctx.depth + 1;
        let max = self.limits.delegation_depth();
        if max > 0 && depth > max {
            return Err(ToolError::new(format!(
                "delegation depth {depth} exceeds max_delegation_depth {max}"
            )));
        }
        let root = if ctx.root_run_id.is_empty() {
            ctx.run_id.clone()
        } else {
            ctx.root_run_id.clone()
        };
        let message_id = format!("{}.{}.{}", ctx.run_id, ctx.step, ctx.call_id);
        // Limits count each call once (by message id), across retries and
        // resumes; a repeated message id is the same call.
        if let Some((per_run, total)) = self
            .store
            .record_delegation(&message_id, &root, &ctx.run_id)
            .await
            .map_err(|e| ToolError::new(e.to_string()))?
        {
            let over = match (self.limits.fan_out(), self.limits.total_delegations()) {
                (max, _) if max > 0 && per_run > i64::from(max) => Some(format!(
                    "this run already made {} delegated calls (max_fan_out {max})",
                    per_run - 1
                )),
                (_, max) if max > 0 && total > i64::from(max) => Some(format!(
                    "the run tree already made {} delegated calls (max_total_delegations {max})",
                    total - 1
                )),
                _ => None,
            };
            if let Some(msg) = over {
                let _ = self.store.forget_delegation(&message_id).await;
                return Err(ToolError::new(msg));
            }
        }
        if !d.sync_a2a() {
            if let Some(starter) = self.starter.clone() {
                return self.call_unified(&starter, d, message, &ctx, &message_id).await;
            }
        }
        let body = json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "message/send",
            "params": {
                "message": {
                    "kind": "message",
                    "role": "user",
                    "messageId": message_id,
                    "parts": [{"kind": "text", "text": message}]
                },
                "metadata": {
                    "traceparent": ctx.traceparent,
                    "agen.depth": depth,
                    "agen.root_run_id": root,
                    "agen.parent_run_id": ctx.run_id,
                    "agen.labels": ctx.labels
                }
            }
        });
        let mut last = String::new();
        // If this call is abandoned (the caller's run is cancelled, which
        // drops this future), the guard tells the callee to stop.
        let mut guard = CancelCallee {
            http: self.http.clone(),
            endpoint: None,
            token: None,
            message_id: message_id.clone(),
        };
        // A sleeping target is woken by its Gateway; transport failures move
        // on to the next endpoint and, after all, retry after a pause.
        for round in 0..3 {
            let resolved = self.endpoints(d).await?;
            for ep in resolved.endpoints {
                guard.endpoint = Some(ep.clone());
                guard.token = resolved.token.clone();
                let r = tokio::select! {
                    r = self.send(&ep, resolved.token.as_deref(), &body) => r,
                    _ = ctx.cancel.cancelled() => return Err(ToolError::unknown("cancelled")),
                };
                guard.endpoint = None;
                match r {
                    Ok(text) => return Ok(text),
                    Err(SendError::Remote(e)) => return Err(ToolError::new(e)),
                    Err(SendError::Transport(e)) => last = format!("{ep}: {e}"),
                }
            }
            if round < 2 {
                tokio::select! {
                    _ = tokio::time::sleep(Duration::from_secs(2)) => {}
                    _ = ctx.cancel.cancelled() => return Err(ToolError::new("cancelled")),
                }
            }
        }
        Err(ToolError::new(format!("agent {agent} unreachable: {last}")))
    }
}
