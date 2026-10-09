//! Record real provider interactions to a cassette, then replay them offline.
//!
//! Replay is strict: each request must match the recorded request's
//! fingerprint, so a changed prompt or tool schema fails loudly instead of
//! silently returning a stale answer.

use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use super::{DeltaSink, ModelProvider, ModelRequest, ModelResponse, ProviderError};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Interaction {
    pub fingerprint: String,
    pub request: ModelRequest,
    pub response: ModelResponse,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Cassette {
    pub provider: String,
    pub interactions: Vec<Interaction>,
}

impl Cassette {
    pub fn load(path: &Path) -> Result<Self, ProviderError> {
        let text = std::fs::read_to_string(path)
            .map_err(|e| ProviderError::Config(format!("cassette {}: {e}", path.display())))?;
        serde_json::from_str(&text).map_err(|e| ProviderError::Config(format!("cassette {}: {e}", path.display())))
    }

    pub fn save(&self, path: &Path) -> std::io::Result<()> {
        std::fs::write(path, serde_json::to_vec_pretty(self).expect("cassette serializes"))
    }
}

/// Stable hash of a request (canonical JSON; object keys are sorted by serde_json's BTreeMap).
pub fn fingerprint(req: &ModelRequest) -> String {
    let v = serde_json::to_value(req).expect("request serializes");
    let canonical = serde_json::to_string(&v).expect("value serializes");
    format!("sha256:{}", hex::encode(Sha256::digest(canonical.as_bytes())))
}

/// Wraps a real provider and records every interaction.
pub struct RecordingProvider<P> {
    inner: P,
    cassette: Mutex<Cassette>,
}

impl<P: ModelProvider> RecordingProvider<P> {
    pub fn new(inner: P) -> Self {
        let provider = inner.name().to_string();
        Self {
            inner,
            cassette: Mutex::new(Cassette {
                provider,
                interactions: vec![],
            }),
        }
    }

    pub fn cassette(&self) -> Cassette {
        self.cassette.lock().unwrap().clone()
    }
}

#[async_trait]
impl<P: ModelProvider> ModelProvider for RecordingProvider<P> {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn complete(&self, req: &ModelRequest, on_delta: DeltaSink<'_>) -> Result<ModelResponse, ProviderError> {
        let resp = self.inner.complete(req, on_delta).await?;
        self.cassette.lock().unwrap().interactions.push(Interaction {
            fingerprint: fingerprint(req),
            request: req.clone(),
            response: resp.clone(),
        });
        Ok(resp)
    }
}

pub struct ReplayProvider {
    cassette: Cassette,
    next: AtomicUsize,
}

impl ReplayProvider {
    pub fn new(cassette: Cassette) -> Self {
        Self {
            cassette,
            next: AtomicUsize::new(0),
        }
    }

    pub fn from_file(path: &Path) -> Result<Self, ProviderError> {
        Ok(Self::new(Cassette::load(path)?))
    }
}

#[async_trait]
impl ModelProvider for ReplayProvider {
    fn name(&self) -> &str {
        "replay"
    }

    async fn complete(&self, req: &ModelRequest, on_delta: DeltaSink<'_>) -> Result<ModelResponse, ProviderError> {
        let i = self.next.fetch_add(1, Ordering::SeqCst);
        let Some(it) = self.cassette.interactions.get(i) else {
            return Err(ProviderError::Script(format!(
                "cassette exhausted after {} interactions",
                self.cassette.interactions.len()
            )));
        };
        let fp = fingerprint(req);
        if fp != it.fingerprint {
            return Err(ProviderError::Script(format!(
                "request {i} does not match cassette (prompt, tools or history changed); re-record the cassette"
            )));
        }
        if !it.response.text.is_empty() {
            on_delta(&it.response.text);
        }
        Ok(it.response.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::provider::fake::{FakeProvider, FakeResponse};
    use crate::provider::{no_deltas, Message};

    fn req(text: &str) -> ModelRequest {
        ModelRequest {
            model: "m".into(),
            system: "s".into(),
            messages: vec![Message::user(text)],
            tools: vec![],
            temperature: Some(0.0),
            max_output_tokens: None,
        }
    }

    #[tokio::test]
    async fn record_then_replay_roundtrip_through_file() {
        let rec = RecordingProvider::new(FakeProvider::new(vec![
            FakeResponse {
                text: "one".into(),
                ..Default::default()
            },
            FakeResponse {
                text: "two".into(),
                ..Default::default()
            },
        ]));
        rec.complete(&req("a"), &no_deltas).await.unwrap();
        rec.complete(&req("b"), &no_deltas).await.unwrap();

        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("c.json");
        rec.cassette().save(&path).unwrap();

        let rp = ReplayProvider::from_file(&path).unwrap();
        assert_eq!(rp.complete(&req("a"), &no_deltas).await.unwrap().text, "one");
        assert_eq!(rp.complete(&req("b"), &no_deltas).await.unwrap().text, "two");
        assert!(rp.complete(&req("c"), &no_deltas).await.is_err());
    }

    #[tokio::test]
    async fn replay_rejects_changed_request() {
        let rec = RecordingProvider::new(FakeProvider::new(vec![FakeResponse {
            text: "x".into(),
            ..Default::default()
        }]));
        rec.complete(&req("original"), &no_deltas).await.unwrap();
        let rp = ReplayProvider::new(rec.cassette());
        let e = rp.complete(&req("changed"), &no_deltas).await.unwrap_err();
        assert!(e.to_string().contains("re-record"), "{e}");
    }

    #[test]
    fn fingerprint_is_deterministic() {
        assert_eq!(fingerprint(&req("a")), fingerprint(&req("a")));
        assert_ne!(fingerprint(&req("a")), fingerprint(&req("b")));
    }
}
