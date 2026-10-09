//! MCP client: connects the servers in a bundle's `mcp.json` and exposes their
//! tools to the agent as `<server>.<tool>`.
//!
//! - stdio servers run as child processes in their own process group (Unix)
//!   or Job Object (Windows), so the whole tree is killed on drop.
//! - Streamable HTTP servers are reached by URL with optional headers.
//! - `${NAME}` in `env` values and headers is replaced with the secret `NAME`.
//! - Tools are side-effecting unless the server marks them `readOnlyHint`;
//!   `x-agen/config.json` `tools.<server>.<tool>.sideEffect` overrides.
//! - Every call has a timeout and honours run cancellation.
//! - Server stderr is redacted and forwarded to the agent's log.

use std::collections::{BTreeMap, HashMap};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use rmcp::model::{CallToolRequestParams, ContentBlock};
use rmcp::service::{Peer, RunningService};
use rmcp::{RoleClient, ServiceExt};
use serde_json::Value;

use crate::bundle::{McpServer, ToolMeta};
use crate::provider::ToolSpec;
use crate::secrets::{Redactor, Secrets};
use crate::tools::{Tool, ToolContext, ToolError};

#[derive(Debug, thiserror::Error)]
pub enum McpError {
    #[error("mcp server {0}: {1}")]
    Connect(String, String),
    #[error("mcp server {0}: {1}")]
    Config(String, String),
}

/// Live connections; dropping this closes them and kills stdio servers.
pub struct McpConnections {
    services: Vec<(String, RunningService<RoleClient, ()>)>,
}

impl McpConnections {
    pub fn servers(&self) -> Vec<&str> {
        self.services.iter().map(|(n, _)| n.as_str()).collect()
    }

    /// Servers whose connection has closed (e.g. the process died). The
    /// host reports itself unhealthy so its Manager replaces it.
    pub fn closed_servers(&self) -> Vec<&str> {
        self.services
            .iter()
            .filter(|(_, s)| s.is_transport_closed())
            .map(|(n, _)| n.as_str())
            .collect()
    }

    pub async fn close(self) {
        for (_, s) in self.services {
            let _ = s.cancel().await;
        }
    }
}

pub struct McpOptions {
    pub call_timeout: Duration,
    pub connect_timeout: Duration,
}

impl Default for McpOptions {
    fn default() -> Self {
        Self {
            call_timeout: Duration::from_secs(120),
            connect_timeout: Duration::from_secs(30),
        }
    }
}

/// Environment variables passed through to stdio servers.
const INHERITED_ENV: &[&str] = &[
    "PATH",
    "HOME",
    "USERPROFILE",
    "SYSTEMROOT",
    "SystemRoot",
    "WINDIR",
    "COMSPEC",
    "PATHEXT",
    "TEMP",
    "TMP",
    "TMPDIR",
    "LANG",
    "LC_ALL",
    "APPDATA",
    "LOCALAPPDATA",
    "PROGRAMDATA",
    "PROGRAMFILES",
    "ProgramFiles",
];

/// Substitute `${NAME}` with secret values.
fn expand(server: &str, value: &str, secrets: &Secrets) -> Result<String, McpError> {
    let mut out = String::new();
    let mut rest = value;
    while let Some(start) = rest.find("${") {
        out.push_str(&rest[..start]);
        let after = &rest[start + 2..];
        let end = after
            .find('}')
            .ok_or_else(|| McpError::Config(server.into(), format!("unterminated ${{ in {value:?}")))?;
        let name = &after[..end];
        let v = secrets
            .get(name)
            .ok_or_else(|| McpError::Config(server.into(), format!("secret {name} is not declared in secrets.json")))?;
        out.push_str(v);
        rest = &after[end + 1..];
    }
    out.push_str(rest);
    Ok(out)
}

/// Connect every server and return the tools they expose.
pub async fn connect(
    bundle_root: &Path,
    servers: &BTreeMap<String, McpServer>,
    tool_meta: &BTreeMap<String, ToolMeta>,
    secrets: &Secrets,
    redactor: &Redactor,
    opts: &McpOptions,
) -> Result<(McpConnections, Vec<Arc<dyn Tool>>), McpError> {
    let mut services = Vec::new();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::new();
    for (name, cfg) in servers {
        let service = tokio::time::timeout(
            opts.connect_timeout,
            connect_one(bundle_root, name, cfg, secrets, redactor),
        )
        .await
        .map_err(|_| McpError::Connect(name.clone(), "timed out connecting".into()))??;
        let listed = tokio::time::timeout(opts.connect_timeout, service.list_all_tools())
            .await
            .map_err(|_| McpError::Connect(name.clone(), "timed out listing tools".into()))?
            .map_err(|e| McpError::Connect(name.clone(), format!("list tools: {e}")))?;
        for t in listed {
            let qualified = format!("{name}.{}", t.name);
            let read_only = t.annotations.as_ref().and_then(|a| a.read_only_hint).unwrap_or(false);
            let side_effect = tool_meta
                .get(&qualified)
                .and_then(|m| m.side_effect)
                .unwrap_or(!read_only);
            tools.push(Arc::new(McpTool {
                spec: ToolSpec {
                    name: qualified,
                    description: t.description.as_deref().unwrap_or_default().to_string(),
                    parameters: Value::Object((*t.input_schema).clone()),
                },
                remote_name: t.name.to_string(),
                side_effect,
                peer: service.peer().clone(),
                timeout: opts.call_timeout,
            }));
        }
        services.push((name.clone(), service));
    }
    Ok((McpConnections { services }, tools))
}

async fn connect_one(
    root: &Path,
    name: &str,
    cfg: &McpServer,
    secrets: &Secrets,
    redactor: &Redactor,
) -> Result<RunningService<RoleClient, ()>, McpError> {
    let err = |e: String| McpError::Connect(name.into(), e);
    if let Some(command) = &cfg.command {
        use process_wrap::tokio::CommandWrap;
        let program = resolve_program(root, command);
        let mut cmd = tokio::process::Command::new(&program);
        cmd.args(&cfg.args).current_dir(root);
        // Servers only see an allowlisted base environment plus their declared
        // env: never the host's credentials (provider keys, store DSN, secrets).
        cmd.env_clear();
        for key in INHERITED_ENV {
            if let Some(v) = std::env::var_os(key) {
                cmd.env(key, v);
            }
        }
        for (k, v) in &cfg.env {
            cmd.env(k, expand(name, v, secrets)?);
        }
        #[cfg(target_os = "linux")]
        // SAFETY: prctl is async-signal-safe; runs in the child before exec.
        unsafe {
            cmd.pre_exec(|| {
                // Die with the host even if it is SIGKILLed.
                if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) != 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let mut wrap = CommandWrap::from(cmd);
        #[cfg(windows)]
        wrap.wrap(process_wrap::tokio::JobObject);
        #[cfg(unix)]
        wrap.wrap(process_wrap::tokio::ProcessGroup::leader());
        wrap.wrap(process_wrap::tokio::KillOnDrop);
        let (transport, stderr) = rmcp::transport::TokioChildProcess::builder(wrap)
            .stderr(std::process::Stdio::piped())
            .spawn()
            .map_err(|e| err(format!("spawn {program:?}: {e}")))?;
        if let Some(stderr) = stderr {
            let (server, redactor) = (name.to_string(), redactor.clone());
            tokio::spawn(async move {
                use tokio::io::AsyncBufReadExt;
                let mut lines = tokio::io::BufReader::new(stderr).lines();
                while let Ok(Some(line)) = lines.next_line().await {
                    eprintln!("agen [mcp:{server}] {}", redactor.redact(&line));
                }
            });
        }
        return ().serve(transport).await.map_err(|e| err(format!("initialize: {e}")));
    }
    if let Some(url) = &cfg.url {
        use rmcp::transport::streamable_http_client::StreamableHttpClientTransportConfig;
        use rmcp::transport::StreamableHttpClientTransport;
        let mut config = StreamableHttpClientTransportConfig::with_uri(expand(name, url, secrets)?);
        let mut headers = HashMap::new();
        for (k, v) in &cfg.headers {
            let v = expand(name, v, secrets)?;
            if k.eq_ignore_ascii_case("authorization") {
                if let Some(token) = v.strip_prefix("Bearer ") {
                    config = config.auth_header(token.to_string());
                    continue;
                }
            }
            let hn = http::HeaderName::from_bytes(k.as_bytes())
                .map_err(|e| McpError::Config(name.into(), format!("header {k}: {e}")))?;
            let hv = http::HeaderValue::from_str(&v)
                .map_err(|e| McpError::Config(name.into(), format!("header {k}: {e}")))?;
            headers.insert(hn, hv);
        }
        if !headers.is_empty() {
            config = config.custom_headers(headers);
        }
        let transport = StreamableHttpClientTransport::from_config(config);
        return ().serve(transport).await.map_err(|e| err(format!("initialize: {e}")));
    }
    Err(McpError::Config(name.into(), "needs \"command\" or \"url\"".into()))
}

/// Relative commands that exist inside the bundle run from there; others are
/// looked up on PATH (handles Windows `.cmd`/`.exe` shims).
fn resolve_program(root: &Path, command: &str) -> std::path::PathBuf {
    let local = root.join(command);
    if (command.contains('/') || command.contains('\\')) && local.exists() {
        return local;
    }
    rmcp::transport::which_command(command)
        .ok()
        .map(|c| c.as_std().get_program().into())
        .unwrap_or_else(|| command.into())
}

struct McpTool {
    spec: ToolSpec,
    remote_name: String,
    side_effect: bool,
    peer: Peer<RoleClient>,
    timeout: Duration,
}

#[async_trait]
impl Tool for McpTool {
    fn spec(&self) -> ToolSpec {
        self.spec.clone()
    }

    fn side_effect(&self) -> bool {
        self.side_effect
    }

    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let mut params = CallToolRequestParams::new(self.remote_name.clone());
        match args {
            Value::Object(map) => params = params.with_arguments(map),
            Value::Null => {}
            other => return Err(ToolError::new(format!("arguments must be an object, got {other}"))),
        }
        let call = self.peer.call_tool(params);
        let result = tokio::select! {
            r = tokio::time::timeout(self.timeout, call) => r.map_err(|_| ToolError::unknown(format!("timed out after {:?}", self.timeout)))?,
            _ = ctx.cancel.cancelled() => return Err(ToolError::unknown("cancelled")),
        }
        // Transport failures (server died, connection lost) leave the outcome unknown.
        .map_err(|e| ToolError::unknown(format!("mcp call failed: {e}")))?;
        let mut text = Vec::new();
        for block in &result.content {
            match block.as_text() {
                Some(t) => text.push(t.text.clone()),
                None => text.push(format!("[non-text content: {}]", content_kind(block))),
            }
        }
        if text.is_empty() {
            if let Some(s) = &result.structured_content {
                text.push(s.to_string());
            }
        }
        let text = text.join("\n");
        if result.is_error.unwrap_or(false) {
            return Err(ToolError::new(text));
        }
        Ok(text)
    }
}

fn content_kind(block: &ContentBlock) -> String {
    serde_json::to_value(block)
        .ok()
        .and_then(|v| v.get("type").and_then(Value::as_str).map(String::from))
        .unwrap_or_else(|| "unknown".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expands_secret_placeholders() {
        let mut m = BTreeMap::new();
        m.insert("TOKEN".to_string(), "tok-1234".to_string());
        let s = Secrets::from_map(m);
        assert_eq!(expand("x", "Bearer ${TOKEN}", &s).unwrap(), "Bearer tok-1234");
        assert_eq!(expand("x", "plain", &s).unwrap(), "plain");
        assert!(expand("x", "${MISSING}", &s).is_err());
        assert!(expand("x", "${TOKEN", &s).is_err());
    }
}
