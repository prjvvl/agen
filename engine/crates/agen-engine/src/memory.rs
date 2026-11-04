//! Memory blocks (`x-agen/config.json` `memory`): durable, named text an
//! agent reads and updates across runs and conversations, rendered into
//! the system prompt and changed through the `memory_update` built-in
//! tool with compare-and-set (`store::update_memory_block`), so parallel
//! conversations cannot silently overwrite each other. The same `memory`
//! list also carries live context sources (1.8): read-only content
//! fetched fresh before every model call instead of coming from the
//! Store (today: a plain `url`), appended after the durable blocks so a
//! stable prefix is disturbed as little as possible.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{json, Value};

use crate::bundle::{MemoryAttachment, MemoryMode};
use crate::provider::ToolSpec;
use crate::store::{MemoryOp, Store, StoreError};
use crate::tools::{Tool, ToolContext, ToolError};

pub const MEMORY_UPDATE: &str = "memory_update";

const DEFAULT_LIVE_MAX_CHARS: usize = 4000;
const DEFAULT_LIVE_TIMEOUT_SECONDS: u64 = 5;

/// Renders a namespace's attached memory blocks and live context sources
/// into the system prompt. A block never written yet (or written empty)
/// is left out; a live source that fails or times out to fetch is left
/// out too, never turning into a run error (1.8).
pub async fn render(
    http: &reqwest::Client,
    store: &Store,
    namespace: &str,
    attachments: &[MemoryAttachment],
) -> Result<String, StoreError> {
    let mut s = String::new();
    for a in attachments {
        if let Some(block) = &a.block {
            let Some(b) = store.get_memory_block(namespace, block).await? else {
                continue;
            };
            if b.value.is_empty() {
                continue;
            }
            s.push_str("\n\n## Memory: ");
            s.push_str(if b.label.is_empty() { block } else { &b.label });
            if !b.description.is_empty() {
                s.push_str(" (");
                s.push_str(&b.description);
                s.push(')');
            }
            s.push('\n');
            s.push_str(&b.value);
        } else if let Some(url) = &a.url {
            if let Some(body) = fetch_live(http, url, a.max_chars, a.timeout_seconds).await {
                s.push_str("\n\n## Live: ");
                s.push_str(url);
                s.push('\n');
                s.push_str(&body);
            }
        }
    }
    Ok(s)
}

/// Fetches a live context source's current content (1.8): `None` on any
/// error (unreachable, non-2xx, timeout, not valid text) rather than
/// failing the turn over something optional outside agen's control.
async fn fetch_live(
    http: &reqwest::Client,
    url: &str,
    max_chars: Option<usize>,
    timeout_seconds: Option<u64>,
) -> Option<String> {
    let timeout = Duration::from_secs(timeout_seconds.unwrap_or(DEFAULT_LIVE_TIMEOUT_SECONDS));
    let resp = http.get(url).timeout(timeout).send().await.ok()?;
    if !resp.status().is_success() {
        return None;
    }
    let text = resp.text().await.ok()?;
    let max = max_chars.unwrap_or(DEFAULT_LIVE_MAX_CHARS);
    Some(text.chars().take(max).collect())
}

/// The `memory_update` tool: `op` `replace` (the given `text`), `append`
/// (adds `text`), or `str_replace` (`old`, occurring exactly once, becomes
/// `new`). A block's `mode` (its `memory` attachment) gates which ops an
/// agent may use on it: `read` (none), `append` (append only), `write`
/// (any).
pub struct MemoryUpdate {
    store: Arc<Store>,
    namespace: String,
    attachments: Vec<MemoryAttachment>,
}

impl MemoryUpdate {
    pub fn new(store: Arc<Store>, namespace: &str, attachments: Vec<MemoryAttachment>) -> Self {
        Self {
            store,
            namespace: namespace.into(),
            attachments,
        }
    }
}

#[async_trait]
impl Tool for MemoryUpdate {
    fn spec(&self) -> ToolSpec {
        let names: Vec<&str> = self.attachments.iter().filter_map(|a| a.block.as_deref()).collect();
        ToolSpec {
            name: MEMORY_UPDATE.into(),
            description: "Update a memory block: durable text kept across runs and conversations. \
                \"replace\" rewrites the whole block from text. \"append\" adds text to the end. \"str_replace\" \
                needs old (must occur exactly once in the block) and new. Pass expectedVersion (the block's \
                last-seen version) to refuse the update instead of overwriting a concurrent change."
                .into(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "block": {"type": "string", "enum": names},
                    "op": {"type": "string", "enum": ["replace", "append", "str_replace"]},
                    "text": {"type": "string", "description": "replace/append: the text"},
                    "old": {"type": "string", "description": "str_replace: must occur exactly once in the block"},
                    "new": {"type": "string", "description": "str_replace: its replacement"},
                    "expectedVersion": {"type": "integer"}
                },
                "required": ["block", "op"],
                "additionalProperties": false
            }),
        }
    }

    async fn call(&self, args: Value, _ctx: ToolContext) -> Result<String, ToolError> {
        let block = args.get("block").and_then(Value::as_str).unwrap_or_default();
        let a = self
            .attachments
            .iter()
            .find(|a| a.block.as_deref() == Some(block))
            .ok_or_else(|| ToolError::new(format!("{block:?} is not a memory block of this agent")))?;
        let op_name = args.get("op").and_then(Value::as_str).unwrap_or_default();
        match a.mode {
            MemoryMode::Read => {
                return Err(ToolError::new(format!(
                    "memory block {block:?} is read-only for this agent"
                )))
            }
            MemoryMode::Append if op_name != "append" => {
                return Err(ToolError::new(format!(
                    "memory block {block:?} only accepts \"append\" for this agent"
                )))
            }
            _ => {}
        }
        let text = |args: &Value| args.get("text").and_then(Value::as_str).unwrap_or_default().to_string();
        let op = match op_name {
            "replace" => MemoryOp::Replace(text(&args)),
            "append" => MemoryOp::Append(text(&args)),
            "str_replace" => {
                let old = args.get("old").and_then(Value::as_str).unwrap_or_default().to_string();
                let new = args.get("new").and_then(Value::as_str).unwrap_or_default().to_string();
                if old.is_empty() {
                    return Err(ToolError::new("str_replace needs a non-empty \"old\""));
                }
                MemoryOp::StrReplace { old, new }
            }
            other => {
                return Err(ToolError::new(format!(
                    "op must be replace, append or str_replace, not {other:?}"
                )))
            }
        };
        let expected = args.get("expectedVersion").and_then(Value::as_i64);
        let b = self
            .store
            .update_memory_block(&self.namespace, block, &op, expected)
            .await
            .map_err(|e| ToolError::new(e.to_string()))?;
        Ok(format!("memory block {block:?} updated (now version {})", b.version))
    }
}
