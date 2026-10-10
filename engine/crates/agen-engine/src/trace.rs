//! OTel-shaped tracing written to the Store. Trace context travels in
//! [`TraceContext`] values (never globals) and as W3C `traceparent` strings.

use std::sync::Arc;

use serde_json::Value;

use crate::secrets::Redactor;
use crate::store::{now_ms, SpanRecord, Store};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TraceContext {
    /// 32 lowercase hex chars.
    pub trace_id: String,
    /// 16 lowercase hex chars; the parent for new child spans.
    pub span_id: String,
}

fn random_u128() -> u128 {
    ulid::Ulid::generate().0 ^ (ulid::Ulid::generate().0.rotate_left(64))
}

pub fn new_trace_id() -> String {
    format!("{:032x}", random_u128())
}

pub fn new_span_id() -> String {
    format!("{:016x}", random_u128() as u64)
}

impl TraceContext {
    pub fn new_root() -> Self {
        Self {
            trace_id: new_trace_id(),
            span_id: String::new(),
        }
    }

    /// Parse `00-<trace>-<span>-<flags>`.
    pub fn from_traceparent(tp: &str) -> Option<Self> {
        let parts: Vec<&str> = tp.trim().split('-').collect();
        let hex = |s: &str, n: usize| s.len() == n && s.bytes().all(|b| b.is_ascii_hexdigit());
        if parts.len() != 4 || parts[0] != "00" || !hex(parts[1], 32) || !hex(parts[2], 16) {
            return None;
        }
        if parts[1].bytes().all(|b| b == b'0') || parts[2].bytes().all(|b| b == b'0') {
            return None;
        }
        Some(Self {
            trace_id: parts[1].to_lowercase(),
            span_id: parts[2].to_lowercase(),
        })
    }

    pub fn traceparent(&self) -> String {
        format!("00-{}-{}-01", self.trace_id, self.span_id)
    }

    pub fn child(&self) -> Self {
        Self {
            trace_id: self.trace_id.clone(),
            span_id: new_span_id(),
        }
    }
}

/// A span being recorded. Finish it with [`Tracer::end`].
pub struct OpenSpan {
    pub ctx: TraceContext,
    parent_span_id: String,
    name: String,
    start_ms: i64,
    /// Start order within the run: parents always sort before children,
    /// even when they start in the same millisecond.
    seq: i64,
    pub attributes: serde_json::Map<String, Value>,
}

impl OpenSpan {
    pub fn set(&mut self, key: &str, value: impl Into<Value>) {
        self.attributes.insert(key.to_string(), value.into());
    }
}

#[derive(Clone)]
pub struct Tracer {
    store: Arc<Store>,
    redactor: Redactor,
    run_id: String,
    seq: Arc<std::sync::atomic::AtomicI64>,
}

impl Tracer {
    pub fn new(store: Arc<Store>, redactor: Redactor, run_id: String) -> Self {
        Self {
            store,
            redactor,
            run_id,
            seq: Arc::default(),
        }
    }

    pub fn start(&self, parent: &TraceContext, name: &str) -> OpenSpan {
        OpenSpan {
            ctx: parent.child(),
            parent_span_id: parent.span_id.clone(),
            name: name.to_string(),
            start_ms: now_ms(),
            seq: self.seq.fetch_add(1, std::sync::atomic::Ordering::SeqCst),
            attributes: serde_json::Map::new(),
        }
    }

    /// Persist a long-lived span now as `unfinished`, so it survives if the
    /// process dies before [`Tracer::end`] replaces it.
    pub async fn record_open(&self, span: &OpenSpan) {
        self.write(span, span.start_ms, "unfinished").await;
    }

    /// Persist the span (attributes redacted). Tracing failures never fail a run.
    pub async fn end(&self, span: OpenSpan, status: &str) {
        self.write(&span, now_ms(), status).await;
    }

    async fn write(&self, span: &OpenSpan, end_ms: i64, status: &str) {
        let rec = SpanRecord {
            span_id: span.ctx.span_id.clone(),
            trace_id: span.ctx.trace_id.clone(),
            parent_span_id: span.parent_span_id.clone(),
            run_id: self.run_id.clone(),
            name: span.name.clone(),
            start_ms: span.start_ms,
            seq: span.seq,
            end_ms,
            status: status.to_string(),
            attributes: self.redactor.redact_json(&Value::Object(span.attributes.clone())),
        };
        if let Err(e) = self.store.insert_span(&rec).await {
            eprintln!("agen: failed to record span: {e}");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn traceparent_roundtrip_and_validation() {
        let root = TraceContext::new_root().child();
        assert_eq!(root.trace_id.len(), 32);
        assert_eq!(root.span_id.len(), 16);
        let parsed = TraceContext::from_traceparent(&root.traceparent()).unwrap();
        assert_eq!(parsed, root);
        assert!(TraceContext::from_traceparent("00-abc-def-01").is_none());
        assert!(TraceContext::from_traceparent(&format!("00-{}-{}-01", "0".repeat(32), "1".repeat(16))).is_none());
        assert_ne!(new_trace_id(), new_trace_id());
    }
}
