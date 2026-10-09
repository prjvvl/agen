//! MCP server used by Agen's tests. Serves over stdio by default, or over
//! streamable HTTP with `--http <addr>`.
//!
//! Tools:
//! - `echo` (read-only): returns its text
//! - `write_note` (side effect): appends to the file in `$NOTES_FILE`
//! - `slow` (read-only): sleeps `ms` milliseconds
//! - `write_note_slow` (side effect): writes the note, then sleeps `ms`
//! - `fail`: returns an MCP tool error
//! - `whoami` (read-only): returns `$TEST_TOKEN` (for secret env tests)
//! - `getenv` (read-only): returns an env var, `<unset>` if missing
//! - `die`: exits the server process
//!
//! It also writes a line to stderr on start, so stderr handling is exercised.

use std::sync::Arc;

use rmcp::handler::server::router::tool::ToolRouter;
use rmcp::handler::server::wrapper::Parameters;
use rmcp::model::{CallToolResult, ContentBlock, ServerCapabilities, ServerConfig};
use rmcp::{schemars, tool, tool_handler, tool_router, ErrorData, ServerHandler, ServiceExt};

#[derive(Debug, serde::Deserialize, schemars::JsonSchema)]
struct Text {
    text: String,
}

#[derive(Debug, serde::Deserialize, schemars::JsonSchema)]
struct Ms {
    ms: u64,
}

#[derive(Debug, serde::Deserialize, schemars::JsonSchema)]
struct SlowNote {
    text: String,
    ms: u64,
}

#[derive(Debug, Clone)]
struct TestServer {
    tool_router: ToolRouter<Self>,
}

#[tool_router]
impl TestServer {
    fn new() -> Self {
        Self {
            tool_router: Self::tool_router(),
        }
    }

    #[tool(description = "Echo the text back", annotations(read_only_hint = true))]
    async fn echo(&self, Parameters(Text { text }): Parameters<Text>) -> Result<CallToolResult, ErrorData> {
        Ok(CallToolResult::success(vec![ContentBlock::text(format!(
            "echo: {text}"
        ))]))
    }

    #[tool(description = "Append a note to the notes file")]
    async fn write_note(&self, Parameters(Text { text }): Parameters<Text>) -> Result<CallToolResult, ErrorData> {
        use std::io::Write;
        let path = std::env::var("NOTES_FILE").map_err(|_| ErrorData::internal_error("NOTES_FILE not set", None))?;
        let mut f = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&path)
            .map_err(|e| ErrorData::internal_error(e.to_string(), None))?;
        writeln!(f, "{text}").map_err(|e| ErrorData::internal_error(e.to_string(), None))?;
        Ok(CallToolResult::success(vec![ContentBlock::text("noted")]))
    }

    #[tool(description = "Sleep for ms milliseconds", annotations(read_only_hint = true))]
    async fn slow(&self, Parameters(Ms { ms }): Parameters<Ms>) -> Result<CallToolResult, ErrorData> {
        tokio::time::sleep(std::time::Duration::from_millis(ms)).await;
        Ok(CallToolResult::success(vec![ContentBlock::text("slept")]))
    }

    #[tool(description = "Append a note, then stall for ms milliseconds (for crash tests)")]
    async fn write_note_slow(
        &self,
        Parameters(SlowNote { text, ms }): Parameters<SlowNote>,
    ) -> Result<CallToolResult, ErrorData> {
        self.write_note(Parameters(Text { text })).await?;
        tokio::time::sleep(std::time::Duration::from_millis(ms)).await;
        Ok(CallToolResult::success(vec![ContentBlock::text("noted slowly")]))
    }

    #[tool(
        description = "Read an environment variable (for isolation tests)",
        annotations(read_only_hint = true)
    )]
    async fn getenv(&self, Parameters(Text { text }): Parameters<Text>) -> Result<CallToolResult, ErrorData> {
        let v = std::env::var(&text).unwrap_or_else(|_| "<unset>".into());
        Ok(CallToolResult::success(vec![ContentBlock::text(v)]))
    }

    #[tool(description = "Exit the server process immediately")]
    async fn die(&self) -> Result<CallToolResult, ErrorData> {
        std::process::exit(3)
    }

    #[tool(description = "Always fails")]
    async fn fail(&self) -> Result<CallToolResult, ErrorData> {
        Ok(CallToolResult::error(vec![ContentBlock::text("the operation failed")]))
    }

    #[tool(description = "Return the configured token", annotations(read_only_hint = true))]
    async fn whoami(&self) -> Result<CallToolResult, ErrorData> {
        let token = std::env::var("TEST_TOKEN").unwrap_or_else(|_| "anonymous".into());
        Ok(CallToolResult::success(vec![ContentBlock::text(format!(
            "token={token}"
        ))]))
    }
}

#[tool_handler(router = self.tool_router)]
impl ServerHandler for TestServer {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args: Vec<String> = std::env::args().collect();
    eprintln!(
        "mcp-test-server starting (token={})",
        std::env::var("TEST_TOKEN").unwrap_or_default()
    );
    if let Ok(path) = std::env::var("PID_FILE") {
        std::fs::write(path, std::process::id().to_string())?;
    }
    if let Some(i) = args.iter().position(|a| a == "--http") {
        use rmcp::transport::streamable_http_server::session::local::LocalSessionManager;
        use rmcp::transport::streamable_http_server::{StreamableHttpServerConfig, StreamableHttpService};
        let addr = args.get(i + 1).cloned().unwrap_or_else(|| "127.0.0.1:0".into());
        let service = StreamableHttpService::new(
            || Ok(TestServer::new()),
            Arc::new(LocalSessionManager::default()),
            StreamableHttpServerConfig::default(),
        );
        let app = axum::Router::new().nest_service("/mcp", service);
        let listener = tokio::net::TcpListener::bind(&addr).await?;
        // Tests read the bound address from stdout.
        println!("listening http://{}/mcp", listener.local_addr()?);
        axum::serve(listener, app).await?;
        return Ok(());
    }
    let service = TestServer::new().serve(rmcp::transport::stdio()).await?;
    service.waiting().await?;
    Ok(())
}
