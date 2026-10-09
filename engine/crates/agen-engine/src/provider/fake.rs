//! Deterministic scripted provider for tests. Never touches the network.
//!
//! Script file format:
//! ```json
//! { "responses": [
//!     { "toolCalls": [{ "name": "search", "arguments": {"q": "x"} }] },
//!     { "expect": "result text", "text": "Final answer" }
//! ] }
//! ```
//! Responses are returned in order. `expect` asserts the last message sent to
//! the model contains the given substring, so tests can prove tool results
//! reached the model.

use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};

use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use super::{DeltaSink, ModelProvider, ModelRequest, ModelResponse, ProviderError, Role, ToolCall, Usage};

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct ScriptedToolCall {
    pub name: String,
    #[serde(default)]
    pub arguments: Value,
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct FakeResponse {
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub tool_calls: Vec<ScriptedToolCall>,
    pub expect: Option<String>,
    pub usage: Option<Usage>,
    /// Milliseconds to wait before answering (to simulate latency).
    #[serde(default)]
    pub delay_ms: u64,
    /// Return this error instead of a response.
    pub error: Option<String>,
}

#[derive(Debug, Deserialize)]
struct Script {
    responses: Vec<FakeResponse>,
    /// Repeat the script from the start when exhausted (for load tests).
    #[serde(default)]
    cycle: bool,
    /// Pick the response by the turn within the current run (assistant
    /// messages since the run's user message) instead of a process-wide
    /// counter, so concurrent runs in one process each follow the script.
    #[serde(default, rename = "perRun")]
    per_run: bool,
}

pub struct FakeProvider {
    responses: Vec<FakeResponse>,
    cycle: bool,
    per_run: bool,
    next: AtomicUsize,
}

impl FakeProvider {
    pub fn new(responses: Vec<FakeResponse>) -> Self {
        Self {
            responses,
            cycle: false,
            per_run: false,
            next: AtomicUsize::new(0),
        }
    }

    pub fn cycling(mut self) -> Self {
        self.cycle = true;
        self
    }

    pub fn from_file(path: &Path) -> Result<Self, ProviderError> {
        let text = std::fs::read_to_string(path)
            .map_err(|e| ProviderError::Config(format!("fake script {}: {e}", path.display())))?;
        let s: Script = serde_json::from_str(&text)
            .map_err(|e| ProviderError::Config(format!("fake script {}: {e}", path.display())))?;
        if s.responses.is_empty() {
            return Err(ProviderError::Config("fake script has no responses".into()));
        }
        Ok(Self {
            responses: s.responses,
            cycle: s.cycle,
            per_run: s.per_run,
            next: AtomicUsize::new(0),
        })
    }

    /// Number of completions served so far.
    pub fn calls(&self) -> usize {
        self.next.load(Ordering::SeqCst)
    }
}

/// Rough, deterministic token estimate (≈4 chars per token).
pub fn estimate_tokens(s: &str) -> u64 {
    (s.chars().count() as u64).div_ceil(4)
}

#[async_trait]
impl ModelProvider for FakeProvider {
    fn name(&self) -> &str {
        "fake"
    }

    async fn complete(&self, req: &ModelRequest, on_delta: DeltaSink<'_>) -> Result<ModelResponse, ProviderError> {
        let n = self.next.fetch_add(1, Ordering::SeqCst);
        let n = if self.per_run {
            // Turn index within this run: assistant messages after the last
            // user message.
            req.messages
                .iter()
                .rev()
                .take_while(|m| m.role != Role::User)
                .filter(|m| m.role == Role::Assistant)
                .count()
        } else {
            n
        };
        let idx = if self.cycle { n % self.responses.len() } else { n };
        let Some(r) = self.responses.get(idx) else {
            return Err(ProviderError::Script(format!(
                "script exhausted after {} responses",
                self.responses.len()
            )));
        };
        if r.delay_ms > 0 {
            tokio::time::sleep(std::time::Duration::from_millis(r.delay_ms)).await;
        }
        if let Some(e) = &r.error {
            return Err(ProviderError::Retryable(e.clone()));
        }
        if let Some(expect) = &r.expect {
            let last = req.messages.last().map(|m| m.content.as_str()).unwrap_or("");
            if !last.contains(expect.as_str()) {
                return Err(ProviderError::Script(format!(
                    "response {idx}: expected last message to contain {expect:?}, got {last:?}"
                )));
            }
        }
        for word in r.text.split_inclusive(' ') {
            on_delta(word);
        }
        let tool_calls = r
            .tool_calls
            .iter()
            .map(|c| ToolCall {
                id: format!("call_{}", crate::store::new_id().to_lowercase()),
                name: c.name.clone(),
                arguments: c.arguments.clone(),
            })
            .collect();
        let usage = r.usage.unwrap_or_else(|| {
            let input: u64 =
                estimate_tokens(&req.system) + req.messages.iter().map(|m| estimate_tokens(&m.content)).sum::<u64>();
            Usage {
                input_tokens: input,
                output_tokens: estimate_tokens(&r.text).max(1),
                cost_usd: 0.0,
            }
        });
        Ok(ModelResponse {
            finish_reason: if r.tool_calls.is_empty() {
                "stop".into()
            } else {
                "tool_calls".into()
            },
            text: r.text.clone(),
            tool_calls,
            usage,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::provider::{no_deltas, Message};
    use std::sync::Mutex;

    fn req(last: &str) -> ModelRequest {
        ModelRequest {
            model: "fake-1".into(),
            system: "sys".into(),
            messages: vec![Message::user(last)],
            tools: vec![],
            temperature: None,
            max_output_tokens: None,
        }
    }

    #[tokio::test]
    async fn returns_scripted_responses_in_order_and_streams() {
        let p = FakeProvider::new(vec![
            FakeResponse {
                tool_calls: vec![ScriptedToolCall {
                    name: "t".into(),
                    arguments: serde_json::json!({"a": 1}),
                }],
                ..Default::default()
            },
            FakeResponse {
                text: "hello there world".into(),
                expect: Some("tool-out".into()),
                ..Default::default()
            },
        ]);
        let r1 = p.complete(&req("hi"), &no_deltas).await.unwrap();
        assert_eq!(r1.tool_calls[0].name, "t");
        assert_eq!(r1.tool_calls[0].arguments["a"], 1);
        let seen = Mutex::new(String::new());
        let sink = |d: &str| seen.lock().unwrap().push_str(d);
        let r2 = p.complete(&req("tool-out: 42"), &sink).await.unwrap();
        assert_eq!(r2.text, "hello there world");
        assert_eq!(*seen.lock().unwrap(), "hello there world");
        assert!(r2.usage.input_tokens > 0 && r2.usage.output_tokens > 0);
        assert!(matches!(
            p.complete(&req("x"), &no_deltas).await,
            Err(ProviderError::Script(_))
        ));
        assert_eq!(p.calls(), 3);
    }

    #[tokio::test]
    async fn expect_mismatch_fails() {
        let p = FakeProvider::new(vec![FakeResponse {
            expect: Some("needle".into()),
            ..Default::default()
        }]);
        let e = p.complete(&req("haystack"), &no_deltas).await.unwrap_err();
        assert!(e.to_string().contains("needle"), "{e}");
    }

    #[tokio::test]
    async fn cycles_and_loads_file() {
        let dir = tempfile::tempdir().unwrap();
        let f = dir.path().join("s.json");
        std::fs::write(&f, r#"{"responses":[{"text":"a"},{"text":"b"}],"cycle":true}"#).unwrap();
        let p = FakeProvider::from_file(&f).unwrap();
        let mut got = vec![];
        for _ in 0..5 {
            got.push(p.complete(&req("x"), &no_deltas).await.unwrap().text);
        }
        assert_eq!(got, ["a", "b", "a", "b", "a"]);
        std::fs::write(&f, r#"{"responses":[]}"#).unwrap();
        assert!(FakeProvider::from_file(&f).is_err());
    }
}
