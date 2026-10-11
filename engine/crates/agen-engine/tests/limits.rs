//! Run limits (identical calls, tool timeout, run duration) and the link from
//! model-call spans to the messages they produced.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::{RunStatus, Store};
use agen_engine::tools::{FnTool, Tool, ToolError};
use serde_json::{json, Value};

async fn store() -> (Arc<Store>, tempfile::TempDir) {
    let d = tempfile::tempdir().unwrap();
    (
        Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap()),
        d,
    )
}

fn call(name: &str, args: Value) -> FakeResponse {
    FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: name.into(),
            arguments: args,
        }],
        ..Default::default()
    }
}

fn say(text: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        ..Default::default()
    }
}

/// A tool that counts its calls and takes `delay` to answer.
fn slow_tool(count: Arc<AtomicUsize>, delay: Duration) -> Arc<dyn Tool> {
    Arc::new(FnTool::new(
        "check",
        "Check something",
        json!({"type":"object"}),
        false,
        move |_a: Value, _| {
            let count = count.clone();
            async move {
                count.fetch_add(1, Ordering::SeqCst);
                tokio::time::sleep(delay).await;
                Ok::<_, ToolError>("still pending".to_string())
            }
        },
    ))
}

#[tokio::test]
async fn chat_spans_name_their_reply_message() {
    let (s, _d) = store().await;
    let count = Arc::new(AtomicUsize::new(0));
    let p = Arc::new(FakeProvider::new(vec![call("check", json!({})), say("done")]));
    let agent = Agent::builder(AgentConfig::new("a", "x", "fake-1"), s.clone())
        .provider(p)
        .tool(slow_tool(count, Duration::ZERO))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let seqs: Vec<i64> = s
        .trace(&r.trace_id)
        .await
        .unwrap()
        .iter()
        .filter(|sp| sp.name == "gen_ai.chat")
        .map(|sp| sp.attributes["agen.message.seq"].as_i64().unwrap())
        .collect();
    // user (1), assistant call (2), tool result (3), assistant answer (4).
    assert_eq!(seqs, [2, 4]);
}

#[tokio::test]
async fn identical_tool_calls_stop_being_repeated() {
    let (s, _d) = store().await;
    let count = Arc::new(AtomicUsize::new(0));
    let mut script: Vec<FakeResponse> = (0..7).map(|_| call("check", json!({"id": 1}))).collect();
    script.push(say("gave up"));
    let agent = Agent::builder(AgentConfig::new("a", "x", "fake-1"), s.clone())
        .provider(Arc::new(FakeProvider::new(script)))
        .tool(slow_tool(count.clone(), Duration::ZERO))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(count.load(Ordering::SeqCst), 5, "the default allows 5 identical calls");
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let refused = msgs
        .iter()
        .filter(|m| m.content.contains("already made 5 times"))
        .count();
    assert_eq!(refused, 2);
}

#[tokio::test]
async fn slow_tools_time_out() {
    let (s, _d) = store().await;
    let count = Arc::new(AtomicUsize::new(0));
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.tool_timeout = Duration::from_millis(50);
    let agent = Agent::builder(cfg, s.clone())
        .provider(Arc::new(FakeProvider::new(vec![call("check", json!({})), say("ok")])))
        .tool(slow_tool(count, Duration::from_secs(30)))
        .build()
        .unwrap();
    let started = std::time::Instant::now();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert!(started.elapsed() < Duration::from_secs(10));
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    assert!(
        msgs.iter().any(|m| m.content.contains("limits.toolTimeout")),
        "{msgs:?}"
    );
}

#[tokio::test]
async fn runs_stop_at_max_run_duration() {
    let (s, _d) = store().await;
    let count = Arc::new(AtomicUsize::new(0));
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.max_run_duration = Duration::from_millis(100);
    let agent = Agent::builder(cfg, s.clone())
        .provider(Arc::new(FakeProvider::new(vec![call("check", json!({})), say("ok")])))
        .tool(slow_tool(count, Duration::from_millis(300)))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert!(r.error.contains("max_run_duration"), "{}", r.error);
}
