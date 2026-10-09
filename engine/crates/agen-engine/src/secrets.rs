//! Secret resolution by name and redaction.
//!
//! Bundles only name secrets (`x-agen/secrets.json`). Values are resolved at
//! runtime and registered with the [`Redactor`], which every persist, export
//! and model call passes through, so values never reach the Store, traces,
//! logs or the model.

use std::collections::BTreeMap;
use std::sync::{Arc, RwLock};

use async_trait::async_trait;

use crate::bundle::SecretRef;

#[derive(Debug, thiserror::Error)]
pub enum SecretError {
    #[error("secret {0} is not set (source {1})")]
    Missing(String, String),
    #[error("secret {0}: source {1} is not available here")]
    Unsupported(String, String),
    #[error("secret {0} is shorter than {MIN_SECRET_LEN} characters and cannot be redacted safely")]
    TooShort(String),
}

/// Resolves secrets whose source is `platform` (the Manager in managed mode,
/// or the host application in embedded mode).
#[async_trait]
pub trait PlatformSecrets: Send + Sync {
    async fn resolve(&self, name: &str) -> Option<String>;
}

/// Resolved secret values, available to tools by name.
#[derive(Clone, Default)]
pub struct Secrets {
    values: Arc<BTreeMap<String, String>>,
}

impl Secrets {
    pub fn get(&self, name: &str) -> Option<&str> {
        self.values.get(name).map(String::as_str)
    }

    pub fn names(&self) -> impl Iterator<Item = &str> {
        self.values.keys().map(String::as_str)
    }

    pub fn from_map(values: BTreeMap<String, String>) -> Self {
        Self {
            values: Arc::new(values),
        }
    }
}

/// Resolve every declared secret. Missing secrets are an error: an agent
/// must not start half-configured.
pub async fn resolve_all(
    declared: &BTreeMap<String, SecretRef>,
    platform: Option<&dyn PlatformSecrets>,
) -> Result<Secrets, SecretError> {
    let mut out = BTreeMap::new();
    for (name, r) in declared {
        let key = r.key.clone().unwrap_or_else(|| name.clone());
        let value = match r.source.as_str() {
            "env" => std::env::var(&key).ok(),
            "platform" => match platform {
                Some(p) => p.resolve(&key).await,
                None => return Err(SecretError::Unsupported(name.clone(), r.source.clone())),
            },
            // OS keychain support lands with the installer phase.
            other => return Err(SecretError::Unsupported(name.clone(), other.to_string())),
        };
        match value {
            Some(v) if !v.is_empty() && v.len() < MIN_SECRET_LEN => return Err(SecretError::TooShort(name.clone())),
            Some(v) if !v.is_empty() => {
                out.insert(name.clone(), v);
            }
            _ => return Err(SecretError::Missing(name.clone(), r.source.clone())),
        }
    }
    Ok(Secrets::from_map(out))
}

/// Replaces registered secret values with `[REDACTED:NAME]`.
#[derive(Clone, Default)]
pub struct Redactor {
    // (value, name), longest first so overlapping values redact fully.
    values: Arc<RwLock<Vec<(String, String)>>>,
}

/// Secrets shorter than this are rejected: they cannot be redacted without
/// mangling ordinary text.
pub const MIN_SECRET_LEN: usize = 4;

/// Check every secret can be redacted.
pub fn validate(secrets: &Secrets) -> Result<(), SecretError> {
    for name in secrets.names() {
        if secrets.get(name).unwrap_or_default().len() < MIN_SECRET_LEN {
            return Err(SecretError::TooShort(name.to_string()));
        }
    }
    Ok(())
}

impl Redactor {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn register(&self, name: &str, value: &str) {
        if value.len() < MIN_SECRET_LEN {
            return;
        }
        let mut v = self.values.write().unwrap();
        if !v.iter().any(|(val, _)| val == value) {
            v.push((value.to_string(), name.to_string()));
            v.sort_by_key(|(val, _)| std::cmp::Reverse(val.len()));
        }
    }

    pub fn register_all(&self, secrets: &Secrets) {
        for name in secrets.names() {
            self.register(name, secrets.get(name).unwrap_or_default());
        }
    }

    pub fn redact(&self, text: &str) -> String {
        let v = self.values.read().unwrap();
        let mut out = text.to_string();
        for (value, name) in v.iter() {
            if out.contains(value.as_str()) {
                out = out.replace(value.as_str(), &format!("[REDACTED:{name}]"));
            }
        }
        out
    }

    /// Registered secret values (longest first).
    fn values(&self) -> Vec<String> {
        self.values.read().unwrap().iter().map(|(v, _)| v.clone()).collect()
    }

    /// Redact every string inside a JSON value.
    pub fn redact_json(&self, v: &serde_json::Value) -> serde_json::Value {
        use serde_json::Value;
        match v {
            Value::String(s) => Value::String(self.redact(s)),
            Value::Array(a) => Value::Array(a.iter().map(|x| self.redact_json(x)).collect()),
            Value::Object(o) => Value::Object(o.iter().map(|(k, x)| (k.clone(), self.redact_json(x))).collect()),
            other => other.clone(),
        }
    }
}

/// Redacts a text stream delivered in chunks. A secret split across chunks
/// is still caught: any tail that could be the start of a secret is held back
/// until the next chunk (or [`StreamRedactor::finish`]) decides it.
pub struct StreamRedactor {
    redactor: Redactor,
    held: String,
}

impl StreamRedactor {
    pub fn new(redactor: Redactor) -> Self {
        Self {
            redactor,
            held: String::new(),
        }
    }

    /// Feed a chunk; returns the text that is safe to emit now.
    pub fn push(&mut self, chunk: &str) -> String {
        let mut text = std::mem::take(&mut self.held);
        text.push_str(chunk);
        let text = self.redactor.redact(&text);
        let hold = longest_secret_prefix_suffix(&text, &self.redactor.values());
        let cut = text.len() - hold;
        self.held = text[cut..].to_string();
        text[..cut].to_string()
    }

    /// End of stream: emit whatever is held.
    pub fn finish(&mut self) -> String {
        let t = std::mem::take(&mut self.held);
        self.redactor.redact(&t)
    }

    /// Drop held text (e.g. the attempt is being retried).
    pub fn reset(&mut self) {
        self.held.clear();
    }
}

/// Length in bytes of the longest suffix of `text` that is a proper prefix of
/// some secret (always on a char boundary).
fn longest_secret_prefix_suffix(text: &str, secrets: &[String]) -> usize {
    let mut best = 0;
    for secret in secrets {
        let max = secret.len().saturating_sub(1).min(text.len());
        for len in (best + 1..=max).rev() {
            let start = text.len() - len;
            if text.is_char_boundary(start) && secret.starts_with(&text[start..]) {
                best = len;
                break;
            }
        }
    }
    best
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn redacts_longest_first_and_json() {
        let r = Redactor::new();
        r.register("SHORT", "abc");
        r.register("A", "sk-123456");
        r.register("B", "sk-123456789");
        assert_eq!(
            r.redact("key=sk-123456789 other=sk-123456 abc"),
            "key=[REDACTED:B] other=[REDACTED:A] abc"
        );
        let v = r.redact_json(&serde_json::json!({"k": ["sk-123456", {"n": "x sk-123456789"}], "n": 5}));
        assert_eq!(
            v,
            serde_json::json!({"k": ["[REDACTED:A]", {"n": "x [REDACTED:B]"}], "n": 5})
        );
    }

    #[test]
    fn stream_redactor_catches_secrets_split_across_chunks() {
        let r = Redactor::new();
        r.register("K", "sk-SECRET-123");
        let mut s = StreamRedactor::new(r);
        let mut out = String::new();
        for chunk in ["hello sk-SE", "CRET-", "123 and sk-", "nope ", "done"] {
            out.push_str(&s.push(chunk));
            assert!(!out.contains("sk-SECRET-123"));
        }
        out.push_str(&s.finish());
        assert_eq!(out, "hello [REDACTED:K] and sk-nope done");
        // Text that merely starts like a secret is released at the end.
        let mut s2 = StreamRedactor::new(Redactor::new());
        assert_eq!(s2.push("plain"), "plain");
    }

    #[test]
    fn short_secrets_are_rejected() {
        let mut m = BTreeMap::new();
        m.insert("PIN".to_string(), "123".to_string());
        assert!(matches!(validate(&Secrets::from_map(m)), Err(SecretError::TooShort(_))));
    }

    #[tokio::test]
    async fn resolves_env_and_platform_and_fails_on_missing() {
        struct P;
        #[async_trait]
        impl PlatformSecrets for P {
            async fn resolve(&self, name: &str) -> Option<String> {
                (name == "PLAT").then(|| "platform-value".to_string())
            }
        }
        std::env::set_var("AGEN_TEST_SECRET_X", "env-value");
        let mut declared = BTreeMap::new();
        declared.insert(
            "X".to_string(),
            SecretRef {
                source: "env".into(),
                key: Some("AGEN_TEST_SECRET_X".into()),
            },
        );
        declared.insert(
            "PLAT".to_string(),
            SecretRef {
                source: "platform".into(),
                key: None,
            },
        );
        let s = resolve_all(&declared, Some(&P)).await.unwrap();
        assert_eq!(s.get("X"), Some("env-value"));
        assert_eq!(s.get("PLAT"), Some("platform-value"));
        assert!(matches!(
            resolve_all(&declared, None).await,
            Err(SecretError::Unsupported(..))
        ));
        declared.insert(
            "NOPE".to_string(),
            SecretRef {
                source: "env".into(),
                key: Some("AGEN_TEST_SECRET_MISSING".into()),
            },
        );
        assert!(matches!(
            resolve_all(&declared, Some(&P)).await,
            Err(SecretError::Missing(..))
        ));
    }
}
