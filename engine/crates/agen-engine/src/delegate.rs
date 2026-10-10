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

use crate::bundle::{Delegate, Limits};
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

/// The `call_agent` tool.
pub struct CallAgent {
    delegates: Vec<Delegate>,
    default_namespace: String,
    resolver: Option<Arc<dyn Resolver>>,
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
        limits: Limits,
        store: Arc<Store>,
    ) -> Self {
        Self {
            delegates,
            default_namespace: default_namespace.to_string(),
            resolver,
            limits,
            store,
            http: reqwest::Client::new(),
            timeout: Duration::from_secs(600),
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
        match state {
            "completed" => Ok(result
                .get("artifacts")
                .and_then(Value::as_array)
                .map(|arts| {
                    arts.iter()
                        .map(|a| text_of(a.get("parts")))
                        .collect::<Vec<_>>()
                        .join("\n")
                })
                .unwrap_or_default()),
            other => Err(SendError::Remote(format!(
                "agent task {other}: {}",
                text_of(result.pointer("/status/message/parts"))
            ))),
        }
    }
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
            name: "call_agent".into(),
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
        if let Some(max) = self.limits.max_delegation_depth {
            if max > 0 && depth > max {
                return Err(ToolError::new(format!(
                    "delegation depth {depth} exceeds max_delegation_depth {max}"
                )));
            }
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
            let over = match (self.limits.max_fan_out, self.limits.max_total_delegations) {
                (Some(max), _) if max > 0 && per_run > i64::from(max) => Some(format!(
                    "this run already made {} delegated calls (max_fan_out {max})",
                    per_run - 1
                )),
                (_, Some(max)) if max > 0 && total > i64::from(max) => Some(format!(
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
