//! Artifacts (3.8, part): named, content-addressed text an agent saves
//! during a run and reads back later, so a long result does not have to
//! stay in the model's context. Available to every agent (like `finish`),
//! not gated by bundle config.

use async_trait::async_trait;
use serde_json::{json, Value};

use crate::provider::ToolSpec;
use crate::store::Store;
use crate::tools::{Tool, ToolContext, ToolError};

pub const SAVE_ARTIFACT: &str = "save_artifact";
pub const READ_ARTIFACT: &str = "read_artifact";

/// 25 MB: the largest single artifact `save_artifact` accepts.
pub const MAX_ARTIFACT_BYTES: usize = 25 * 1024 * 1024;
/// 500 MB: the most artifact bytes one namespace may hold.
pub const DEFAULT_NAMESPACE_QUOTA: i64 = 500 * 1024 * 1024;
/// How much of an artifact `read_artifact` returns when `length` is not given.
const DEFAULT_READ_CHARS: usize = 4_000;

pub struct SaveArtifact {
    store: std::sync::Arc<Store>,
    quota: i64,
}

impl SaveArtifact {
    pub fn new(store: std::sync::Arc<Store>) -> Self {
        Self {
            store,
            quota: DEFAULT_NAMESPACE_QUOTA,
        }
    }
}

#[async_trait]
impl Tool for SaveArtifact {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: SAVE_ARTIFACT.into(),
            description: format!(
                "Save text as an artifact (at most {} MB) and get back its id. Use this for a long result \
                 you do not need in context right away; read it back later with read_artifact.",
                MAX_ARTIFACT_BYTES / (1024 * 1024)
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "name": {"type": "string", "description": "A short, human-readable name"},
                    "content": {"type": "string"},
                    "mediaType": {"type": "string", "description": "Default: text/plain"}
                },
                "required": ["name", "content"],
                "additionalProperties": false
            }),
        }
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let name = args.get("name").and_then(Value::as_str).unwrap_or_default();
        let content = args.get("content").and_then(Value::as_str).unwrap_or_default();
        if name.is_empty() {
            return Err(ToolError::new("name is required"));
        }
        if content.len() > MAX_ARTIFACT_BYTES {
            return Err(ToolError::new(format!(
                "content is {} bytes, over the {} MB limit",
                content.len(),
                MAX_ARTIFACT_BYTES / (1024 * 1024)
            )));
        }
        let media_type = args.get("mediaType").and_then(Value::as_str).unwrap_or("text/plain");
        let a = self
            .store
            .save_artifact(
                &ctx.namespace,
                crate::store::NewArtifact {
                    name,
                    content,
                    media_type,
                    task_id: &ctx.task_id,
                    run_id: &ctx.run_id,
                },
                self.quota,
            )
            .await
            .map_err(|e| ToolError::new(e.to_string()))?;
        Ok(format!(
            "saved artifact {} ({:?}, {} bytes). Read it back with read_artifact.",
            a.id, a.name, a.size
        ))
    }
}

pub struct ReadArtifact {
    store: std::sync::Arc<Store>,
}

impl ReadArtifact {
    pub fn new(store: std::sync::Arc<Store>) -> Self {
        Self { store }
    }
}

#[async_trait]
impl Tool for ReadArtifact {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: READ_ARTIFACT.into(),
            description: format!(
                "Read back an artifact saved with save_artifact. offset and length (default {DEFAULT_READ_CHARS} \
                 characters) page through a long one."
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "id": {"type": "string"},
                    "offset": {"type": "integer", "minimum": 0},
                    "length": {"type": "integer", "minimum": 1}
                },
                "required": ["id"],
                "additionalProperties": false
            }),
        }
    }

    fn side_effect(&self) -> bool {
        false
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let id = args.get("id").and_then(Value::as_str).unwrap_or_default();
        let a = self
            .store
            .get_artifact(&ctx.namespace, id)
            .await
            .map_err(|e| ToolError::new(e.to_string()))?
            .ok_or_else(|| ToolError::new(format!("no artifact {id:?}")))?;
        let offset = args.get("offset").and_then(Value::as_u64).unwrap_or(0) as usize;
        let length = args
            .get("length")
            .and_then(Value::as_u64)
            .unwrap_or(DEFAULT_READ_CHARS as u64) as usize;
        let chars: Vec<char> = a.content.chars().collect();
        if offset >= chars.len() {
            return Ok(format!(
                "(artifact {id} is {} characters; offset {offset} is past its end)",
                chars.len()
            ));
        }
        let end = (offset + length).min(chars.len());
        let slice: String = chars[offset..end].iter().collect();
        if end < chars.len() {
            return Ok(format!(
                "{slice}\n(truncated: {} of {} characters shown; read_artifact({id:?}, offset={end}) for more)",
                end - offset,
                chars.len()
            ));
        }
        Ok(slice)
    }
}
