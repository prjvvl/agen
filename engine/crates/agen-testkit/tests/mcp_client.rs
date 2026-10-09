//! Engine MCP client against the real test server over stdio and HTTP.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use agen_engine::agent::{Agent, BundleOptions, RunOptions};
use agen_engine::bundle::Bundle;
use agen_engine::mcp::{self, McpOptions};
use agen_engine::provider::Role;
use agen_engine::secrets::{Redactor, Secrets};
use agen_engine::store::{RunStatus, Store};
use agen_engine::tools::ToolContext;
use serde_json::{json, Value};

const SERVER: &str = env!("CARGO_BIN_EXE_mcp-test-server");
const TOKEN: &str = "tok-SECRET-4242";

fn write_bundle(dir: &Path, mcp: Value, script: Value) {
    std::fs::create_dir_all(dir.join("x-agen")).unwrap();
    std::fs::write(dir.join("plugin.json"), r#"{"name":"mcpuser"}"#).unwrap();
    std::fs::write(
        dir.join("x-agen/agent.md"),
        "---\nname: mcpuser\ndescription: uses MCP\n---\nUse your tools.\n",
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/harness.json"),
        r#"{"provider":"fake","model":"fake-1","script":"x-agen/script.json"}"#,
    )
    .unwrap();
    std::fs::write(dir.join("x-agen/script.json"), script.to_string()).unwrap();
    std::fs::write(dir.join("x-agen/config.json"), r#"{"permissions":{"default":"allow"}}"#).unwrap();
    std::fs::write(
        dir.join("x-agen/secrets.json"),
        r#"{"TOKEN":{"source":"env","key":"AGEN_TEST_MCP_TOKEN"}}"#,
    )
    .unwrap();
    std::fs::write(dir.join("mcp.json"), mcp.to_string()).unwrap();
}

fn step(tool: &str, args: Value, expect: Option<&str>) -> Value {
    let mut v = json!({"toolCalls": [{"name": tool, "arguments": args}]});
    if let Some(e) = expect {
        v["expect"] = json!(e);
    }
    v
}

fn process_alive(pid: u32) -> bool {
    if cfg!(windows) {
        let out = std::process::Command::new("tasklist")
            .args(["/FI", &format!("PID eq {pid}"), "/NH"])
            .output()
            .unwrap();
        String::from_utf8_lossy(&out.stdout).contains(&pid.to_string())
    } else {
        std::process::Command::new("kill")
            .args(["-0", &pid.to_string()])
            .status()
            .map(|s| s.success())
            .unwrap_or(false)
    }
}

#[tokio::test]
async fn stdio_server_tools_run_through_the_agent() {
    std::env::set_var("AGEN_TEST_MCP_TOKEN", TOKEN);
    let dir = tempfile::tempdir().unwrap();
    let notes = dir.path().join("notes.txt");
    let pid_file = dir.path().join("pid.txt");
    let b = dir.path().join("bundle");
    write_bundle(
        &b,
        json!({"mcpServers": {"test": {
            "command": SERVER,
            "env": {"TEST_TOKEN": "${TOKEN}", "NOTES_FILE": notes.display().to_string(), "PID_FILE": pid_file.display().to_string()}
        }}}),
        json!({"responses": [
            step("test.echo", json!({"text": "hi"}), None),
            step("test.write_note", json!({"text": "first note"}), Some("echo: hi")),
            step("test.whoami", json!({}), Some("noted")),
            step("test.fail", json!({}), Some("token=[REDACTED:TOKEN]")),
            {"text": "all done", "expect": "the operation failed"}
        ]}),
    );
    let bundle = Bundle::load(&b).unwrap();
    let store = Arc::new(Store::open_sqlite_path(&dir.path().join("s.db")).await.unwrap());
    let agent = Agent::from_bundle(&bundle, store.clone(), BundleOptions::default())
        .await
        .unwrap();

    let echo = agent.tools().get("test.echo").unwrap();
    let note = agent.tools().get("test.write_note").unwrap();
    assert!(!echo.side_effect(), "readOnlyHint tools are not side-effecting");
    assert!(
        note.side_effect(),
        "tools without readOnlyHint default to side-effecting"
    );
    assert!(echo.spec().parameters["properties"]["text"].is_object());

    let r = agent.run("do things", RunOptions::default()).await.unwrap();
    assert_eq!(
        (r.status, r.output.as_str()),
        (RunStatus::Succeeded, "all done"),
        "{}",
        r.error
    );
    assert_eq!(std::fs::read_to_string(&notes).unwrap(), "first note\n");
    let effects = store.dump_text("effects").await.unwrap();
    assert!(
        effects.contains("test.write_note") && !effects.contains("test.echo"),
        "{effects}"
    );
    for table in ["messages", "spans", "effects", "runs"] {
        assert!(
            !store.dump_text(table).await.unwrap().contains(TOKEN),
            "token leaked into {table}"
        );
    }
    let tool_msgs: Vec<String> = store
        .run_messages(&r.run_id)
        .await
        .unwrap()
        .into_iter()
        .filter(|m| m.role == Role::Tool)
        .map(|m| m.content)
        .collect();
    assert!(tool_msgs[3].starts_with("error: the operation failed"), "{tool_msgs:?}");

    // Dropping the agent kills the stdio server.
    let pid: u32 = std::fs::read_to_string(&pid_file).unwrap().trim().parse().unwrap();
    assert!(process_alive(pid));
    drop(agent);
    let mut gone = false;
    for _ in 0..50 {
        tokio::time::sleep(Duration::from_millis(100)).await;
        if !process_alive(pid) {
            gone = true;
            break;
        }
    }
    assert!(gone, "mcp server process {pid} still running after agent drop");
}

struct HttpServer {
    child: std::process::Child,
    url: String,
}

impl Drop for HttpServer {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}

fn start_http_server() -> HttpServer {
    use std::io::BufRead;
    let mut child = std::process::Command::new(SERVER)
        .args(["--http", "127.0.0.1:0"])
        .env("TEST_TOKEN", "http-token")
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::null())
        .spawn()
        .unwrap();
    let mut line = String::new();
    std::io::BufReader::new(child.stdout.take().unwrap())
        .read_line(&mut line)
        .unwrap();
    let url = line.trim().strip_prefix("listening ").unwrap().to_string();
    HttpServer { child, url }
}

#[tokio::test]
async fn http_server_tools_run_through_the_agent() {
    std::env::set_var("AGEN_TEST_MCP_TOKEN", TOKEN);
    let server = start_http_server();
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("bundle");
    write_bundle(
        &b,
        json!({"mcpServers": {"remote": {"url": server.url, "headers": {"X-Api-Key": "${TOKEN}"}}}}),
        json!({"responses": [
            step("remote.echo", json!({"text": "over http"}), None),
            {"text": "ok", "expect": "echo: over http"}
        ]}),
    );
    let bundle = Bundle::load(&b).unwrap();
    let store = Arc::new(Store::open_sqlite_path(&dir.path().join("s.db")).await.unwrap());
    let agent = Agent::from_bundle(&bundle, store, BundleOptions::default())
        .await
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(
        (r.status, r.output.as_str()),
        (RunStatus::Succeeded, "ok"),
        "{}",
        r.error
    );
}

#[tokio::test]
async fn calls_time_out_and_cancel() {
    let dir = tempfile::tempdir().unwrap();
    let mut servers = BTreeMap::new();
    servers.insert(
        "t".to_string(),
        serde_json::from_value(json!({"command": SERVER})).unwrap(),
    );
    let opts = McpOptions {
        call_timeout: Duration::from_millis(200),
        ..Default::default()
    };
    let (conns, tools) = mcp::connect(
        dir.path(),
        &servers,
        &BTreeMap::new(),
        &Secrets::default(),
        &Redactor::new(),
        &opts,
    )
    .await
    .unwrap();
    assert_eq!(conns.servers(), ["t"]);
    let slow = tools.iter().find(|t| t.spec().name == "t.slow").unwrap();
    let ctx = || ToolContext {
        run_id: "r".into(),
        call_id: "c".into(),
        secrets: Secrets::default(),
        ..Default::default()
    };
    let e = slow.call(json!({"ms": 5000}), ctx()).await.unwrap_err();
    assert!(e.message.contains("timed out") && e.outcome_unknown, "{e}");
    assert_eq!(slow.call(json!({"ms": 10}), ctx()).await.unwrap(), "slept");
    let c = ctx();
    let cancel = c.cancel.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        cancel.cancel();
    });
    let long = McpOptions {
        call_timeout: Duration::from_secs(30),
        ..Default::default()
    };
    let (_c2, tools2) = mcp::connect(
        dir.path(),
        &servers,
        &BTreeMap::new(),
        &Secrets::default(),
        &Redactor::new(),
        &long,
    )
    .await
    .unwrap();
    let slow2 = tools2.iter().find(|t| t.spec().name == "t.slow").unwrap();
    let started = std::time::Instant::now();
    assert!(slow2
        .call(json!({"ms": 10000}), c)
        .await
        .unwrap_err()
        .message
        .contains("cancelled"));
    assert!(started.elapsed() < Duration::from_secs(5));
    conns.close().await;
}

#[tokio::test]
async fn side_effect_override_and_missing_secret() {
    let dir = tempfile::tempdir().unwrap();
    let mut servers = BTreeMap::new();
    servers.insert(
        "t".to_string(),
        serde_json::from_value(json!({"command": SERVER})).unwrap(),
    );
    let mut meta = BTreeMap::new();
    meta.insert(
        "t.echo".to_string(),
        serde_json::from_value(json!({"sideEffect": true})).unwrap(),
    );
    let (_c, tools) = mcp::connect(
        dir.path(),
        &servers,
        &meta,
        &Secrets::default(),
        &Redactor::new(),
        &McpOptions::default(),
    )
    .await
    .unwrap();
    assert!(
        tools.iter().find(|t| t.spec().name == "t.echo").unwrap().side_effect(),
        "config override wins"
    );

    let mut bad = BTreeMap::new();
    bad.insert(
        "t".to_string(),
        serde_json::from_value(json!({"command": SERVER, "env": {"X": "${NOPE}"}})).unwrap(),
    );
    let err = mcp::connect(
        dir.path(),
        &bad,
        &BTreeMap::new(),
        &Secrets::default(),
        &Redactor::new(),
        &McpOptions::default(),
    )
    .await
    .err()
    .unwrap();
    assert!(err.to_string().contains("NOPE"), "{err}");
    let _ = PathBuf::new();
}

#[tokio::test]
async fn stdio_servers_do_not_inherit_host_credentials() {
    std::env::set_var("AGEN_TEST_HOST_ONLY_SECRET", "must-not-leak-123");
    let dir = tempfile::tempdir().unwrap();
    let mut servers = BTreeMap::new();
    servers.insert(
        "t".to_string(),
        serde_json::from_value(json!({"command": SERVER, "env": {"DECLARED": "yes"}})).unwrap(),
    );
    let (_c, tools) = mcp::connect(
        dir.path(),
        &servers,
        &BTreeMap::new(),
        &Secrets::default(),
        &Redactor::new(),
        &McpOptions::default(),
    )
    .await
    .unwrap();
    let getenv = tools.iter().find(|t| t.spec().name == "t.getenv").unwrap();
    let ctx = || ToolContext {
        run_id: "r".into(),
        call_id: "c".into(),
        secrets: Secrets::default(),
        ..Default::default()
    };
    assert_eq!(
        getenv
            .call(json!({"text": "AGEN_TEST_HOST_ONLY_SECRET"}), ctx())
            .await
            .unwrap(),
        "<unset>"
    );
    assert_eq!(getenv.call(json!({"text": "DECLARED"}), ctx()).await.unwrap(), "yes");
    assert_ne!(getenv.call(json!({"text": "PATH"}), ctx()).await.unwrap(), "<unset>");
}

#[tokio::test]
async fn dead_server_gives_unknown_outcome_and_is_reported() {
    let dir = tempfile::tempdir().unwrap();
    let mut servers = BTreeMap::new();
    servers.insert(
        "t".to_string(),
        serde_json::from_value(json!({"command": SERVER})).unwrap(),
    );
    let opts = McpOptions {
        call_timeout: Duration::from_secs(5),
        ..Default::default()
    };
    let (conns, tools) = mcp::connect(
        dir.path(),
        &servers,
        &BTreeMap::new(),
        &Secrets::default(),
        &Redactor::new(),
        &opts,
    )
    .await
    .unwrap();
    assert!(conns.closed_servers().is_empty());
    let die = tools.iter().find(|t| t.spec().name == "t.die").unwrap();
    let ctx = ToolContext {
        run_id: "r".into(),
        call_id: "c".into(),
        secrets: Secrets::default(),
        ..Default::default()
    };
    let e = die.call(json!({}), ctx).await.unwrap_err();
    assert!(e.outcome_unknown, "{e}");
    let mut closed = false;
    for _ in 0..50 {
        if conns.closed_servers() == ["t"] {
            closed = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    assert!(closed, "dead server not reported");
}
