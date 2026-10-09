//! Model provider port. The engine talks to models only through
//! [`ModelProvider`]; adapters live in submodules.

pub mod fake;
pub mod openai;
pub mod replay;

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Role {
    User,
    Assistant,
    Tool,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    pub arguments: Value,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Message {
    pub role: Role,
    #[serde(default)]
    pub content: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ToolCall>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_call_id: Option<String>,
}

impl Message {
    pub fn user(content: impl Into<String>) -> Self {
        Self {
            role: Role::User,
            content: content.into(),
            tool_calls: vec![],
            tool_call_id: None,
        }
    }
    pub fn assistant(content: impl Into<String>, tool_calls: Vec<ToolCall>) -> Self {
        Self {
            role: Role::Assistant,
            content: content.into(),
            tool_calls,
            tool_call_id: None,
        }
    }
    pub fn tool(call_id: impl Into<String>, content: impl Into<String>) -> Self {
        Self {
            role: Role::Tool,
            content: content.into(),
            tool_calls: vec![],
            tool_call_id: Some(call_id.into()),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolSpec {
    pub name: String,
    pub description: String,
    /// JSON Schema for the arguments object.
    pub parameters: Value,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ModelRequest {
    pub model: String,
    pub system: String,
    pub messages: Vec<Message>,
    #[serde(default)]
    pub tools: Vec<ToolSpec>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_output_tokens: Option<u32>,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Serialize, Deserialize)]
pub struct Usage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    /// Reported by the provider when known.
    #[serde(default)]
    pub cost_usd: f64,
}

impl Usage {
    pub fn add(&mut self, other: Usage) {
        self.input_tokens += other.input_tokens;
        self.output_tokens += other.output_tokens;
        self.cost_usd += other.cost_usd;
    }
    pub fn total_tokens(&self) -> u64 {
        self.input_tokens + self.output_tokens
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ModelResponse {
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub tool_calls: Vec<ToolCall>,
    #[serde(default)]
    pub usage: Usage,
    /// Provider stop reason, e.g. "stop", "tool_calls", "length".
    #[serde(default)]
    pub finish_reason: String,
}

/// Marker key the provider puts in tool-call arguments that were not valid
/// JSON (e.g. cut off by the output token limit). Such calls are never executed.
pub const INVALID_ARGS_KEY: &str = "_invalid_json";

#[derive(Debug, thiserror::Error)]
pub enum ProviderError {
    #[error("provider configuration: {0}")]
    Config(String),
    #[error("provider request failed: {0}")]
    Request(String),
    #[error("provider rate limited or unavailable (retryable): {0}")]
    Retryable(String),
    #[error("scripted provider: {0}")]
    Script(String),
    /// Rate limited with a server-provided wait (seconds).
    #[error("provider rate limited (retry after {1}s): {0}")]
    RetryAfter(String, u64),
}

/// Receives streamed text as it is generated.
pub type DeltaSink<'a> = &'a (dyn Fn(&str) + Send + Sync);

#[async_trait]
pub trait ModelProvider: Send + Sync {
    fn name(&self) -> &str;
    async fn complete(&self, req: &ModelRequest, on_delta: DeltaSink<'_>) -> Result<ModelResponse, ProviderError>;
}

/// A sink that ignores deltas.
pub fn no_deltas(_: &str) {}

/// Build the provider named in a bundle's `harness.json`.
pub fn from_harness(
    bundle: &crate::bundle::Bundle,
    secrets: &crate::secrets::Secrets,
) -> Result<std::sync::Arc<dyn ModelProvider>, ProviderError> {
    let h = &bundle.harness;
    match h.provider.as_str() {
        "fake" => {
            let script = h
                .script
                .as_deref()
                .ok_or_else(|| ProviderError::Config("fake provider needs script".into()))?;
            Ok(std::sync::Arc::new(fake::FakeProvider::from_file(
                &bundle.path(script),
            )?))
        }
        "replay" => {
            let cassette = h
                .cassette
                .as_deref()
                .ok_or_else(|| ProviderError::Config("replay provider needs cassette".into()))?;
            Ok(std::sync::Arc::new(replay::ReplayProvider::from_file(
                &bundle.path(cassette),
            )?))
        }
        "openrouter" | "openai" => {
            let key_name = api_key_name(h);
            let key = secrets
                .get(&key_name)
                .ok_or_else(|| ProviderError::Config(format!("API key secret {key_name} is not set")))?;
            let default_url = if h.provider == "openrouter" {
                openai::OPENROUTER_URL
            } else {
                openai::OPENAI_URL
            };
            let url = h.base_url.as_deref().unwrap_or(default_url);
            Ok(std::sync::Arc::new(openai::OpenAiCompatible::new(
                &h.provider,
                url,
                key,
            )?))
        }
        other => Err(ProviderError::Config(format!("unknown provider {other:?}"))),
    }
}

/// Name of the secret holding the provider API key (`apiKeySecret` or the
/// provider default, e.g. `OPENROUTER_API_KEY`).
pub fn api_key_name(h: &crate::bundle::Harness) -> String {
    h.api_key_secret.clone().unwrap_or_else(|| match h.provider.as_str() {
        "openai" => "OPENAI_API_KEY".into(),
        _ => "OPENROUTER_API_KEY".into(),
    })
}
