//! Artifacts (3.8, part): save_artifact / read_artifact are available to
//! every agent, content-addressed, size-limited, and quota-checked.

use std::sync::Arc;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::provider::Role;
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

#[tokio::test]
async fn save_then_read_an_artifact() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        call("save_artifact", json!({"name": "report", "content": "hello world"})),
        say("saved"),
    ]));
    let agent = Agent::builder(AgentConfig::new("a", "x", "fake-1"), s.clone())
        .provider(p)
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, agen_engine::store::RunStatus::Succeeded, "{}", r.error);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let saved = msgs.iter().find(|m| m.role == Role::Tool).unwrap();
    assert!(saved.content.starts_with("saved artifact sha256:"), "{}", saved.content);
    let id = saved.content.split_whitespace().nth(2).unwrap();

    let p2 = Arc::new(FakeProvider::new(vec![
        call("read_artifact", json!({"id": id})),
        say("read it"),
    ]));
    let agent2 = Agent::builder(AgentConfig::new("a", "x", "fake-1"), s.clone())
        .provider(p2)
        .build()
        .unwrap();
    let r2 = agent2.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r2.status, agen_engine::store::RunStatus::Succeeded, "{}", r2.error);
    let msgs2 = s.run_messages(&r2.run_id).await.unwrap();
    let read = msgs2.iter().find(|m| m.role == Role::Tool).unwrap();
    assert_eq!(read.content, "hello world");
}

// Saving the exact same content twice in one namespace is one artifact: the
// id is content-addressed.
#[tokio::test]
async fn saving_the_same_content_twice_is_one_artifact() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        call("save_artifact", json!({"name": "a", "content": "same bytes"})),
        call("save_artifact", json!({"name": "b", "content": "same bytes"})),
        say("done"),
    ]));
    let agent = Agent::builder(AgentConfig::new("a", "x", "fake-1"), s.clone())
        .provider(p)
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, agen_engine::store::RunStatus::Succeeded, "{}", r.error);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let saves: Vec<&str> = msgs
        .iter()
        .filter(|m| m.role == Role::Tool)
        .map(|m| m.content.split_whitespace().nth(2).unwrap())
        .collect();
    assert_eq!(saves.len(), 2);
    assert_eq!(saves[0], saves[1], "the same content shares one artifact id");
    let listed = s.list_artifacts("default", "").await.unwrap();
    assert_eq!(listed.len(), 1, "{listed:?}");
    assert_eq!(listed[0].name, "a", "the first save's name wins");
}

// read_artifact pages through a long artifact by offset/length.
#[tokio::test]
async fn read_artifact_pages_a_long_one() {
    let (s, _d) = store().await;
    s.save_artifact(
        "default",
        agen_engine::store::NewArtifact {
            name: "long",
            content: "0123456789",
            media_type: "text/plain",
            task_id: "",
            run_id: "",
        },
        0,
    )
    .await
    .unwrap();
    let id = s.list_artifacts("default", "").await.unwrap()[0].id.clone();
    let p = Arc::new(FakeProvider::new(vec![
        call("read_artifact", json!({"id": id, "offset": 2, "length": 3})),
        say("ok"),
    ]));
    let agent = Agent::builder(AgentConfig::new("a", "x", "fake-1"), s.clone())
        .provider(p)
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let read = msgs.iter().find(|m| m.role == Role::Tool).unwrap();
    assert!(read.content.starts_with("234\n(truncated"), "{}", read.content);
}

// A quota of 0 for the artifact bytes already saved refuses a new one over it.
#[tokio::test]
async fn save_artifact_refuses_a_name_that_is_empty() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        call("save_artifact", json!({"name": "", "content": "x"})),
        say("done"),
    ]));
    let agent = Agent::builder(AgentConfig::new("a", "x", "fake-1"), s.clone())
        .provider(p)
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let saved = msgs.iter().find(|m| m.role == Role::Tool).unwrap();
    assert!(saved.content.contains("name is required"), "{}", saved.content);
}

// A tool result over harness.maxToolResultChars is capped and the full
// text saved as its own artifact instead, so nothing is dropped without a
// way back (2.23/2.36, part) — proven here through read_artifact's own
// result, which goes through the exact same cap every other tool's does.
#[tokio::test]
async fn a_long_tool_result_is_capped_and_saved_as_an_artifact() {
    let (s, _d) = store().await;
    s.save_artifact(
        "default",
        agen_engine::store::NewArtifact {
            name: "long",
            content: "0123456789abcdefghij",
            media_type: "text/plain",
            task_id: "",
            run_id: "",
        },
        0,
    )
    .await
    .unwrap();
    let id = s.list_artifacts("default", "").await.unwrap()[0].id.clone();
    let p = Arc::new(FakeProvider::new(vec![
        call("read_artifact", json!({"id": id, "length": 20})),
        say("ok"),
    ]));
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.max_tool_result_chars = 5;
    let agent = Agent::builder(cfg, s.clone()).provider(p).build().unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, agen_engine::store::RunStatus::Succeeded, "{}", r.error);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let read = msgs.iter().find(|m| m.role == Role::Tool).unwrap();
    let want = format!("01234\n(truncated: 5 of 20 characters shown; the rest was saved as artifact {id:?}");
    assert!(read.content.starts_with(&want), "{}", read.content);
    // The uncapped result is exactly what was already stored under that id
    // (content-addressed): saving it again found the same artifact, not a
    // second one.
    let listed = s.list_artifacts("default", "").await.unwrap();
    assert_eq!(listed.len(), 1, "{listed:?}");
}

// max_tool_result_chars: 0 turns the cap off.
#[tokio::test]
async fn a_zero_cap_leaves_a_long_result_whole() {
    let (s, _d) = store().await;
    s.save_artifact(
        "default",
        agen_engine::store::NewArtifact {
            name: "long",
            content: "0123456789",
            media_type: "text/plain",
            task_id: "",
            run_id: "",
        },
        0,
    )
    .await
    .unwrap();
    let id = s.list_artifacts("default", "").await.unwrap()[0].id.clone();
    let p = Arc::new(FakeProvider::new(vec![
        call("read_artifact", json!({"id": id, "length": 10})),
        say("ok"),
    ]));
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.max_tool_result_chars = 0;
    let agent = Agent::builder(cfg, s.clone()).provider(p).build().unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let read = msgs.iter().find(|m| m.role == Role::Tool).unwrap();
    assert_eq!(read.content, "0123456789");
    assert_eq!(
        s.list_artifacts("default", "").await.unwrap().len(),
        1,
        "no second artifact was saved"
    );
}

#[tokio::test]
async fn save_artifact_enforces_the_namespace_quota() {
    let (s, _d) = store().await;
    let err = s
        .save_artifact(
            "default",
            agen_engine::store::NewArtifact {
                name: "a",
                content: "0123456789",
                media_type: "text/plain",
                task_id: "",
                run_id: "",
            },
            5, // quota smaller than the content
        )
        .await
        .unwrap_err();
    assert!(err.to_string().contains("quota"), "{err}");
}
