//! Memory blocks (3.10, part): attached blocks render into the system
//! prompt, and `memory_update`'s mode gating (read/append/write) and
//! compare-and-set.

use std::sync::Arc;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::{MemoryAttachment, MemoryMode};
use agen_engine::memory::MemoryUpdate;
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::provider::replay::RecordingProvider;
use agen_engine::store::Store;
use axum::http::StatusCode;
use axum::routing::get;
use axum::Router;
use serde_json::{json, Value};

/// Serves `body` at "/" for every request.
async fn serve_live(body: &'static str) -> String {
    let app = Router::new().route("/", get(move || async move { body }));
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(l, app).await.unwrap() });
    format!("http://{addr}/")
}

async fn serve_failing() -> String {
    async fn handler() -> StatusCode {
        StatusCode::INTERNAL_SERVER_ERROR
    }
    let app = Router::new().route("/", get(handler));
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(l, app).await.unwrap() });
    format!("http://{addr}/")
}

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

fn cfg_with(attachments: Vec<MemoryAttachment>) -> AgentConfig {
    let mut c = AgentConfig::new("notes-agent", "You keep notes.", "fake-1");
    c.memory = attachments;
    c
}

#[tokio::test]
async fn an_attached_block_is_rendered_into_the_system_prompt() {
    let (s, _d) = store().await;
    s.set_memory_block("default", "notes", "Notes", "scratch", "Remember: the sky is blue.", 0)
        .await
        .unwrap();
    let c = cfg_with(vec![MemoryAttachment {
        block: Some("notes".into()),
        mode: MemoryMode::Read,
        ..Default::default()
    }]);
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![say("ok")])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    agent.run("hi", RunOptions::default()).await.unwrap();
    let system = &rec.cassette().interactions[0].request.system;
    assert!(system.contains("## Memory: Notes (scratch)"), "{system}");
    assert!(system.contains("Remember: the sky is blue."), "{system}");
}

// A block never written yet renders as nothing (no empty "## Memory:" noise).
#[tokio::test]
async fn an_unwritten_block_renders_as_nothing() {
    let (s, _d) = store().await;
    let c = cfg_with(vec![MemoryAttachment {
        block: Some("notes".into()),
        mode: MemoryMode::Read,
        ..Default::default()
    }]);
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![say("ok")])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    agent.run("hi", RunOptions::default()).await.unwrap();
    let system = &rec.cassette().interactions[0].request.system;
    assert!(!system.contains("## Memory"), "{system}");
}

#[tokio::test]
async fn read_mode_refuses_any_update() {
    let (s, _d) = store().await;
    s.set_memory_block("default", "notes", "", "", "v1", 0).await.unwrap();
    let attachments = vec![MemoryAttachment {
        block: Some("notes".into()),
        mode: MemoryMode::Read,
        ..Default::default()
    }];
    let c = cfg_with(attachments.clone());
    let p = Arc::new(FakeProvider::new(vec![
        call("memory_update", json!({"block": "notes", "op": "append", "text": "x"})),
        say("done"),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(Arc::new(MemoryUpdate::new(s.clone(), "default", attachments)))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, agen_engine::store::RunStatus::Succeeded, "{}", r.error);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let tool_result = msgs
        .iter()
        .find(|m| m.role == agen_engine::provider::Role::Tool)
        .unwrap();
    assert!(tool_result.content.contains("read-only"), "{}", tool_result.content);
    let unchanged = s.get_memory_block("default", "notes").await.unwrap().unwrap();
    assert_eq!(unchanged.value, "v1");
}

#[tokio::test]
async fn append_mode_allows_append_but_not_replace() {
    let (s, _d) = store().await;
    s.set_memory_block("default", "notes", "", "", "v1", 0).await.unwrap();
    let attachments = vec![MemoryAttachment {
        block: Some("notes".into()),
        mode: MemoryMode::Append,
        ..Default::default()
    }];
    let c = cfg_with(attachments.clone());
    let p = Arc::new(FakeProvider::new(vec![
        call(
            "memory_update",
            json!({"block": "notes", "op": "replace", "text": "nope"}),
        ),
        say("done"),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(Arc::new(MemoryUpdate::new(s.clone(), "default", attachments)))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let tool_result = msgs
        .iter()
        .find(|m| m.role == agen_engine::provider::Role::Tool)
        .unwrap();
    assert!(tool_result.content.contains("only accepts"), "{}", tool_result.content);
    let unchanged = s.get_memory_block("default", "notes").await.unwrap().unwrap();
    assert_eq!(unchanged.value, "v1");
}

#[tokio::test]
async fn write_mode_allows_replace_and_updates_the_block() {
    let (s, _d) = store().await;
    s.set_memory_block("default", "notes", "", "", "v1", 0).await.unwrap();
    let attachments = vec![MemoryAttachment {
        block: Some("notes".into()),
        mode: MemoryMode::Write,
        ..Default::default()
    }];
    let c = cfg_with(attachments.clone());
    let p = Arc::new(FakeProvider::new(vec![
        call(
            "memory_update",
            json!({"block": "notes", "op": "replace", "text": "v2"}),
        ),
        say("done"),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(Arc::new(MemoryUpdate::new(s.clone(), "default", attachments)))
        .build()
        .unwrap();
    agent.run("go", RunOptions::default()).await.unwrap();
    let updated = s.get_memory_block("default", "notes").await.unwrap().unwrap();
    assert_eq!((updated.value.as_str(), updated.version), ("v2", 2));
}

// A tool call naming a block the agent has no "memory" attachment for is
// refused, even if the block exists in the store.
#[tokio::test]
async fn an_unattached_block_is_refused() {
    let (s, _d) = store().await;
    s.set_memory_block("default", "other", "", "", "v1", 0).await.unwrap();
    let attachments = vec![MemoryAttachment {
        block: Some("notes".into()),
        mode: MemoryMode::Write,
        ..Default::default()
    }];
    let c = cfg_with(attachments.clone());
    let p = Arc::new(FakeProvider::new(vec![
        call("memory_update", json!({"block": "other", "op": "replace", "text": "x"})),
        say("done"),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(Arc::new(MemoryUpdate::new(s.clone(), "default", attachments)))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let tool_result = msgs
        .iter()
        .find(|m| m.role == agen_engine::provider::Role::Tool)
        .unwrap();
    assert!(
        tool_result.content.contains("is not a memory block"),
        "{}",
        tool_result.content
    );
}

// A `url` live context source (1.8) is fetched fresh and rendered after
// the durable blocks, read-only (no memory_update applies to it).
#[tokio::test]
async fn a_live_url_source_is_fetched_and_rendered() {
    let (s, _d) = store().await;
    let url = serve_live("today's status: all green").await;
    let c = cfg_with(vec![MemoryAttachment {
        url: Some(url.clone()),
        ..Default::default()
    }]);
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![say("ok")])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    agent.run("hi", RunOptions::default()).await.unwrap();
    let system = &rec.cassette().interactions[0].request.system;
    assert!(system.contains(&format!("## Live: {url}")), "{system}");
    assert!(system.contains("today's status: all green"), "{system}");
}

// max_chars (default 4000) truncates a live source's fetched body.
#[tokio::test]
async fn a_live_url_source_is_truncated_to_max_chars() {
    let (s, _d) = store().await;
    let url = serve_live("0123456789").await;
    let c = cfg_with(vec![MemoryAttachment {
        url: Some(url),
        max_chars: Some(4),
        ..Default::default()
    }]);
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![say("ok")])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    agent.run("hi", RunOptions::default()).await.unwrap();
    let system = &rec.cassette().interactions[0].request.system;
    assert!(system.contains("## Live:") && system.contains("0123"), "{system}");
    assert!(!system.contains("456789"), "{system}");
}

// A live source that fails to fetch (non-2xx here) is left out of the
// system prompt entirely, never failing the run over something optional.
#[tokio::test]
async fn a_failed_live_fetch_is_skipped_silently() {
    let (s, _d) = store().await;
    let url = serve_failing().await;
    let c = cfg_with(vec![MemoryAttachment {
        url: Some(url),
        ..Default::default()
    }]);
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![say("ok")])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, agen_engine::store::RunStatus::Succeeded, "{}", r.error);
    let system = &rec.cassette().interactions[0].request.system;
    assert!(!system.contains("## Live"), "{system}");
}
