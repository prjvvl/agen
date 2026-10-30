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
        block: "notes".into(),
        mode: MemoryMode::Read,
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
        block: "notes".into(),
        mode: MemoryMode::Read,
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
        block: "notes".into(),
        mode: MemoryMode::Read,
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
        block: "notes".into(),
        mode: MemoryMode::Append,
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
        block: "notes".into(),
        mode: MemoryMode::Write,
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
        block: "notes".into(),
        mode: MemoryMode::Write,
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
