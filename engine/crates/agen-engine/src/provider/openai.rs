//! OpenAI-compatible chat completions adapter. Serves OpenRouter (default
//! base URL https://openrouter.ai/api/v1) and OpenAI directly
//! (https://api.openai.com/v1); both speak the same wire format.
//!
//! Streams with SSE, reassembles streamed tool calls, maps tool names that
//! the API would reject (e.g. `server.tool`) and reports usage/cost.

use std::collections::BTreeMap;
use std::time::Duration;

use async_trait::async_trait;
use futures_util::StreamExt;
use serde_json::{json, Value};

use super::{DeltaSink, Message, ModelProvider, ModelRequest, ModelResponse, ProviderError, Role, ToolCall, Usage};

pub const OPENROUTER_URL: &str = "https://openrouter.ai/api/v1";
pub const OPENAI_URL: &str = "https://api.openai.com/v1";

pub struct OpenAiCompatible {
    name: String,
    base_url: String,
    api_key: String,
    client: reqwest::Client,
    openrouter: bool,
}

impl OpenAiCompatible {
    pub fn new(name: &str, base_url: &str, api_key: &str) -> Result<Self, ProviderError> {
        let client = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(15))
            .read_timeout(Duration::from_secs(120))
            .user_agent(concat!("agen/", env!("CARGO_PKG_VERSION")))
            .build()
            .map_err(|e| ProviderError::Config(e.to_string()))?;
        Ok(Self {
            name: name.into(),
            base_url: base_url.trim_end_matches('/').into(),
            api_key: api_key.into(),
            client,
            openrouter: name == "openrouter",
        })
    }

    pub fn openrouter(api_key: &str) -> Result<Self, ProviderError> {
        Self::new("openrouter", OPENROUTER_URL, api_key)
    }

    pub fn openai(api_key: &str) -> Result<Self, ProviderError> {
        Self::new("openai", OPENAI_URL, api_key)
    }
}

/// Tool names must match `^[a-zA-Z0-9_-]{1,64}$` on the wire.
pub fn wire_name(name: &str) -> String {
    let mut s: String = name
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '_' || c == '-' {
                c
            } else {
                '_'
            }
        })
        .collect();
    s.truncate(64);
    s
}

pub fn request_body(req: &ModelRequest, openrouter: bool) -> (Value, BTreeMap<String, String>) {
    let mut names = BTreeMap::new(); // wire -> original
    for t in &req.tools {
        names.insert(wire_name(&t.name), t.name.clone());
    }
    let to_wire = |n: &str| wire_name(n);
    let mut messages = vec![json!({"role": "system", "content": req.system})];
    for m in &req.messages {
        messages.push(message_json(m, &to_wire));
    }
    let mut body = json!({
        "model": req.model,
        "messages": messages,
        "stream": true,
        "stream_options": {"include_usage": true},
    });
    if !req.tools.is_empty() {
        body["tools"] = Value::Array(
            req.tools
                .iter()
                .map(|t| json!({"type": "function", "function": {"name": to_wire(&t.name), "description": t.description, "parameters": t.parameters}}))
                .collect(),
        );
    }
    if let Some(t) = req.temperature {
        body["temperature"] = json!(t);
    }
    if let Some(n) = req.max_output_tokens {
        body["max_tokens"] = json!(n);
    }
    if openrouter {
        body["usage"] = json!({"include": true});
    }
    (body, names)
}

fn message_json(m: &Message, to_wire: &dyn Fn(&str) -> String) -> Value {
    match m.role {
        Role::User => json!({"role": "user", "content": m.content}),
        Role::Tool => json!({"role": "tool", "tool_call_id": m.tool_call_id, "content": m.content}),
        Role::Assistant => {
            let mut v = json!({"role": "assistant", "content": if m.content.is_empty() { Value::Null } else { json!(m.content) }});
            if !m.tool_calls.is_empty() {
                v["tool_calls"] = Value::Array(
                    m.tool_calls
                        .iter()
                        .map(|c| json!({"id": c.id, "type": "function", "function": {"name": to_wire(&c.name), "arguments": c.arguments.to_string()}}))
                        .collect(),
                );
            }
            v
        }
    }
}

#[derive(Default)]
struct PartialCall {
    id: String,
    name: String,
    args: String,
}

/// Accumulates a streamed response.
#[derive(Default)]
pub struct StreamState {
    text: String,
    calls: BTreeMap<u64, PartialCall>,
    usage: Usage,
    finish_reason: String,
}

impl StreamState {
    /// Apply one SSE `data:` payload. Returns text to stream to the caller.
    pub fn apply(&mut self, chunk: &Value) -> Result<Option<String>, ProviderError> {
        if let Some(err) = chunk.get("error") {
            let msg = err
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or("unknown error")
                .to_string();
            let code = err.get("code").and_then(Value::as_u64).unwrap_or(0);
            return Err(if code == 429 || code >= 500 {
                ProviderError::Retryable(msg)
            } else {
                ProviderError::Request(msg)
            });
        }
        if let Some(u) = chunk.get("usage").filter(|u| !u.is_null()) {
            self.usage = Usage {
                input_tokens: u.get("prompt_tokens").and_then(Value::as_u64).unwrap_or(0),
                output_tokens: u.get("completion_tokens").and_then(Value::as_u64).unwrap_or(0),
                cost_usd: u.get("cost").and_then(Value::as_f64).unwrap_or(0.0),
            };
        }
        let mut out = None;
        if let Some(fr) = chunk.pointer("/choices/0/finish_reason").and_then(Value::as_str) {
            self.finish_reason = fr.to_string();
        }
        if let Some(delta) = chunk.pointer("/choices/0/delta") {
            if let Some(t) = delta.get("content").and_then(Value::as_str) {
                if !t.is_empty() {
                    self.text.push_str(t);
                    out = Some(t.to_string());
                }
            }
            if let Some(calls) = delta.get("tool_calls").and_then(Value::as_array) {
                for c in calls {
                    let idx = c.get("index").and_then(Value::as_u64).unwrap_or(0);
                    let p = self.calls.entry(idx).or_default();
                    if let Some(id) = c.get("id").and_then(Value::as_str) {
                        p.id = id.to_string();
                    }
                    if let Some(n) = c.pointer("/function/name").and_then(Value::as_str) {
                        p.name.push_str(n);
                    }
                    if let Some(a) = c.pointer("/function/arguments").and_then(Value::as_str) {
                        p.args.push_str(a);
                    }
                }
            }
        }
        Ok(out)
    }

    pub fn finish(self, names: &BTreeMap<String, String>) -> ModelResponse {
        let tool_calls = self
            .calls
            .into_values()
            .map(|p| {
                let arguments = if p.args.trim().is_empty() {
                    json!({})
                } else {
                    serde_json::from_str(&p.args).unwrap_or_else(|_| json!({ super::INVALID_ARGS_KEY: p.args }))
                };
                ToolCall {
                    id: if p.id.is_empty() {
                        format!("call_{}", crate::store::new_id().to_lowercase())
                    } else {
                        p.id
                    },
                    name: names.get(&p.name).cloned().unwrap_or(p.name),
                    arguments,
                }
            })
            .collect();
        ModelResponse {
            text: self.text,
            tool_calls,
            usage: self.usage,
            finish_reason: self.finish_reason,
        }
    }
}

#[async_trait]
impl ModelProvider for OpenAiCompatible {
    fn name(&self) -> &str {
        &self.name
    }

    async fn complete(&self, req: &ModelRequest, on_delta: DeltaSink<'_>) -> Result<ModelResponse, ProviderError> {
        let (body, names) = request_body(req, self.openrouter);
        let mut rb = self
            .client
            .post(format!("{}/chat/completions", self.base_url))
            .bearer_auth(&self.api_key)
            .json(&body);
        if self.openrouter {
            rb = rb
                .header("X-Title", "Agen")
                .header("HTTP-Referer", "https://github.com/prjvvl/agen");
        }
        let resp = rb
            .send()
            .await
            .map_err(|e| ProviderError::Retryable(format!("connect: {e}")))?;
        let status = resp.status();
        let retry_after = resp
            .headers()
            .get("retry-after")
            .and_then(|v| v.to_str().ok())
            .and_then(|v| v.trim().parse::<u64>().ok());
        if !status.is_success() {
            let text = resp.text().await.unwrap_or_default();
            let msg = format!("HTTP {status}: {}", text.chars().take(500).collect::<String>());
            if let (429, Some(secs)) = (status.as_u16(), retry_after) {
                return Err(ProviderError::RetryAfter(msg, secs));
            }
            return Err(if status.as_u16() == 429 || status.is_server_error() {
                ProviderError::Retryable(msg)
            } else {
                ProviderError::Request(msg)
            });
        }
        let mut state = StreamState::default();
        let mut buf = String::new();
        let mut stream = resp.bytes_stream();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(|e| ProviderError::Retryable(format!("stream: {e}")))?;
            buf.push_str(&String::from_utf8_lossy(&chunk));
            while let Some(pos) = buf.find('\n') {
                let line = buf[..pos].trim_end_matches('\r').to_string();
                buf.drain(..=pos);
                let Some(data) = line.strip_prefix("data:") else {
                    continue;
                }; // comments/keepalives
                let data = data.trim();
                if data == "[DONE]" {
                    return Ok(state.finish(&names));
                }
                if data.is_empty() {
                    continue;
                }
                let v: Value =
                    serde_json::from_str(data).map_err(|e| ProviderError::Request(format!("bad stream chunk: {e}")))?;
                if let Some(t) = state.apply(&v)? {
                    on_delta(&t);
                }
            }
        }
        Ok(state.finish(&names))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::provider::ToolSpec;

    #[test]
    fn body_maps_messages_tools_and_names() {
        let req = ModelRequest {
            model: "m".into(),
            system: "sys".into(),
            messages: vec![
                Message::user("hi"),
                Message::assistant(
                    "",
                    vec![ToolCall {
                        id: "c1".into(),
                        name: "fs.read".into(),
                        arguments: json!({"p": 1}),
                    }],
                ),
                Message::tool("c1", "data"),
            ],
            tools: vec![ToolSpec {
                name: "fs.read".into(),
                description: "d".into(),
                parameters: json!({"type": "object"}),
            }],
            temperature: Some(0.1),
            max_output_tokens: Some(20),
        };
        let (b, names) = request_body(&req, true);
        assert_eq!(b["messages"][0]["role"], "system");
        assert_eq!(b["messages"][2]["tool_calls"][0]["function"]["name"], "fs_read");
        assert_eq!(b["messages"][2]["tool_calls"][0]["function"]["arguments"], "{\"p\":1}");
        assert!(b["messages"][2]["content"].is_null());
        assert_eq!(b["messages"][3]["tool_call_id"], "c1");
        assert_eq!(b["tools"][0]["function"]["name"], "fs_read");
        assert_eq!(b["max_tokens"], 20);
        assert_eq!(b["usage"]["include"], true);
        assert_eq!(names["fs_read"], "fs.read");
        assert!(request_body(&req, false).0.get("usage").is_none());
    }

    #[test]
    fn stream_reassembles_tool_calls_and_usage() {
        let mut s = StreamState::default();
        for c in [
            json!({"choices":[{"delta":{"content":"Hel"}}]}),
            json!({"choices":[{"delta":{"content":"lo"}}]}),
            json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"fs_read","arguments":"{\"p\""}}]}}]}),
            json!({"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":2}"}}]}}]}),
            json!({"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"other","arguments":""}}]}}]}),
            json!({"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"cost":0.00012}}),
        ] {
            s.apply(&c).unwrap();
        }
        let mut names = BTreeMap::new();
        names.insert("fs_read".to_string(), "fs.read".to_string());
        let r = s.finish(&names);
        assert_eq!(r.text, "Hello");
        assert_eq!(r.tool_calls.len(), 2);
        assert_eq!(
            (r.tool_calls[0].name.as_str(), &r.tool_calls[0].arguments),
            ("fs.read", &json!({"p": 2}))
        );
        assert_eq!(r.tool_calls[1].arguments, json!({}));
        assert_eq!((r.usage.input_tokens, r.usage.output_tokens), (11, 7));
        assert!((r.usage.cost_usd - 0.00012).abs() < 1e-12);
    }

    #[test]
    fn stream_error_chunks_are_classified() {
        let mut s = StreamState::default();
        assert!(matches!(
            s.apply(&json!({"error":{"code":429,"message":"slow down"}})),
            Err(ProviderError::Retryable(_))
        ));
        assert!(matches!(
            s.apply(&json!({"error":{"code":400,"message":"bad"}})),
            Err(ProviderError::Request(_))
        ));
    }
}
