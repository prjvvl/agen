//! Managed mode: an instance started by a Nest Manager reaches the platform
//! only through the Manager's localhost `ManagerService`
//! (`AGEN_MANAGER_URL`, authenticated with `AGEN_MANAGER_TOKEN`).

use std::time::Duration;

use async_trait::async_trait;
use serde_json::{json, Value};

use crate::agent::{ApprovalDecision, ApprovalRequest, Approver};

/// Durable "ask" approvals: the Manager records the approval at the Hub,
/// where a human decides it (CLI, API, MCP, UI), and answers when it is
/// decided or expired. A resumed run asking the same question gets the same
/// pending approval.
pub struct ManagerApprover {
    url: String,
    token: String,
    ttl: Duration,
    http: reqwest::Client,
}

impl ManagerApprover {
    pub fn new(url: &str, token: &str, ttl: Duration) -> Self {
        Self {
            url: url.trim_end_matches('/').to_string(),
            token: token.to_string(),
            ttl,
            http: reqwest::Client::new(),
        }
    }

    /// From `AGEN_MANAGER_URL` / `AGEN_MANAGER_TOKEN`, if set.
    pub fn from_env(ttl: Duration) -> Option<Self> {
        let url = std::env::var("AGEN_MANAGER_URL").ok().filter(|u| !u.is_empty())?;
        let token = std::env::var("AGEN_MANAGER_TOKEN").unwrap_or_default();
        Some(Self::new(&url, &token, ttl))
    }

    /// Err((message, retryable)).
    async fn request(&self, req: &ApprovalRequest) -> Result<Value, (String, bool)> {
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/RequestApproval", self.url))
            .bearer_auth(&self.token)
            .json(&json!({
                "runId": req.run_id,
                "tool": req.tool,
                "argumentsJson": req.arguments.to_string(),
                "ttlSeconds": self.ttl.as_secs().clamp(1, i32::MAX as u64),
            }))
            .send()
            .await
            .map_err(|e| (e.to_string(), true))?;
        let status = resp.status();
        let v: Value = resp.json().await.map_err(|e| (e.to_string(), true))?;
        if !status.is_success() {
            // Unavailable and timeouts are retried; anything else (bad
            // token, unknown run, instance gone) will not get better.
            let retryable = status.is_server_error() || status.as_u16() == 408 || status.as_u16() == 429;
            return Err((
                format!("{status}: {}", v.get("message").and_then(Value::as_str).unwrap_or("")),
                retryable,
            ));
        }
        Ok(v)
    }
}

#[async_trait]
impl Approver for ManagerApprover {
    async fn decide(&self, req: ApprovalRequest) -> ApprovalDecision {
        // The Manager holds the call until a decision; transport errors (e.g.
        // a Manager restart) are retried. The engine bounds the whole wait
        // with its approval timeout.
        loop {
            match self.request(&req).await {
                Ok(v) => {
                    return match v.pointer("/approval/state").and_then(Value::as_str).unwrap_or("") {
                        "APPROVAL_STATE_APPROVED" => ApprovalDecision::Approved,
                        "APPROVAL_STATE_DENIED" => ApprovalDecision::Denied,
                        "APPROVAL_STATE_PENDING" => continue, // wait window ended; ask again
                        _ => ApprovalDecision::Expired,
                    };
                }
                Err((e, true)) => {
                    eprintln!("agen [warn] approval request failed, retrying: {e}");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
                Err((e, false)) => {
                    eprintln!("agen [error] approval request refused: {e}");
                    return ApprovalDecision::Denied;
                }
            }
        }
    }
}

/// Platform secrets (bundle source "platform"): the Manager fetches them from
/// the Hub for this instance's deployment, which must declare them. Values
/// are only registered with the Redactor like any other secret.
pub struct ManagerSecrets {
    url: String,
    token: String,
    http: reqwest::Client,
}

impl ManagerSecrets {
    /// From `AGEN_MANAGER_URL` / `AGEN_MANAGER_TOKEN`, if set.
    pub fn from_env() -> Option<Self> {
        let url = std::env::var("AGEN_MANAGER_URL").ok().filter(|u| !u.is_empty())?;
        Some(Self {
            url: url.trim_end_matches('/').to_string(),
            token: std::env::var("AGEN_MANAGER_TOKEN").unwrap_or_default(),
            http: reqwest::Client::new(),
        })
    }
}

#[async_trait]
impl crate::secrets::PlatformSecrets for ManagerSecrets {
    async fn resolve(&self, name: &str) -> Option<String> {
        let resp = self
            .http
            .post(format!("{}/agen.v1.ManagerService/ResolveSecret", self.url))
            .bearer_auth(&self.token)
            .json(&json!({ "name": name }))
            .timeout(Duration::from_secs(30))
            .send()
            .await
            .ok()?;
        if !resp.status().is_success() {
            return None;
        }
        let v: Value = resp.json().await.ok()?;
        v.get("value")
            .and_then(Value::as_str)
            .map(str::to_string)
            .filter(|s| !s.is_empty())
    }
}
