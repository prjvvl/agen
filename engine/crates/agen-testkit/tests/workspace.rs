//! Task workspaces (2.45): a directory the host creates and shares with
//! every MCP server (`_meta` `agen/workspace`) and the model.

use std::path::Path;
use std::sync::Arc;

use agen_engine::agent::{Agent, BundleOptions, RunOptions};
use agen_engine::bundle::Bundle;
use agen_engine::provider::Role;
use agen_engine::store::{RunStatus, Store};
use serde_json::json;

const SERVER: &str = env!("CARGO_BIN_EXE_mcp-test-server");

fn write_bundle(dir: &Path, config: &str) {
    std::fs::create_dir_all(dir.join("x-agen")).unwrap();
    std::fs::write(dir.join("plugin.json"), r#"{"name":"wsuser"}"#).unwrap();
    std::fs::write(
        dir.join("x-agen/agent.md"),
        "---\nname: wsuser\ndescription: uses a workspace\n---\nDo the task.\n",
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/harness.json"),
        r#"{"provider":"fake","model":"fake-1","script":"x-agen/script.json"}"#,
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/script.json"),
        json!({"responses": [
            {"toolCalls": [{"name": "t.context", "arguments": {}}]},
            {"text": "done"}
        ]})
        .to_string(),
    )
    .unwrap();
    std::fs::write(dir.join("x-agen/config.json"), config).unwrap();
    std::fs::write(
        dir.join("mcp.json"),
        json!({"mcpServers": {"t": {"command": SERVER}}}).to_string(),
    )
    .unwrap();
}

async fn store() -> (Arc<Store>, tempfile::TempDir) {
    let d = tempfile::tempdir().unwrap();
    (
        Arc::new(Store::open_sqlite_path(&d.path().join("s.db")).await.unwrap()),
        d,
    )
}

// A per_task workspace is created before the run's first tool call, its
// path reaches both the model (the system prompt) and every MCP server
// (`_meta` `agen/workspace`), and it is removed once the run ends, since
// retain was not set.
#[tokio::test]
async fn per_task_workspace_is_shared_and_removed_after() {
    let (s, _d) = store().await;
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("bundle");
    write_bundle(
        &b,
        r#"{"permissions":{"default":"allow"},"workspace":{"mode":"per_task"}}"#,
    );
    let bundle = Bundle::load(&b).unwrap();
    let agent = Agent::from_bundle(&bundle, s.clone(), BundleOptions::default())
        .await
        .unwrap();
    let r = agent
        .run(
            "go",
            RunOptions {
                task_id: "task-1".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let tool_msg = msgs.iter().find(|m| m.role == Role::Tool).unwrap();
    let meta: serde_json::Value = serde_json::from_str(&tool_msg.content).unwrap();
    let ws = meta["agen/workspace"].as_str().unwrap().to_string();
    assert!(
        ws.contains("task-1"),
        "workspace path should be keyed by the task id: {ws}"
    );
    assert!(
        !Path::new(&ws).exists(),
        "a per_task workspace with no retain should be removed once the run ends: {ws}"
    );
}

// retain: true keeps a per_task workspace after the run ends.
#[tokio::test]
async fn per_task_workspace_with_retain_is_kept() {
    let (s, _d) = store().await;
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("bundle");
    write_bundle(
        &b,
        r#"{"permissions":{"default":"allow"},"workspace":{"mode":"per_task","retain":true}}"#,
    );
    let bundle = Bundle::load(&b).unwrap();
    let agent = Agent::from_bundle(&bundle, s.clone(), BundleOptions::default())
        .await
        .unwrap();
    let r = agent
        .run(
            "go",
            RunOptions {
                task_id: "task-2".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let meta: serde_json::Value =
        serde_json::from_str(&msgs.iter().find(|m| m.role == Role::Tool).unwrap().content).unwrap();
    let ws = meta["agen/workspace"].as_str().unwrap().to_string();
    assert!(
        Path::new(&ws).is_dir(),
        "retain: true should keep the workspace directory: {ws}"
    );
}

// Two tasks of the same conversation share one per_conversation
// workspace, and it is never removed automatically (unlike per_task).
#[tokio::test]
async fn per_conversation_workspace_is_shared_across_tasks_and_not_removed() {
    let (s, _d) = store().await;
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("bundle");
    write_bundle(
        &b,
        r#"{"permissions":{"default":"allow"},"workspace":{"mode":"per_conversation"}}"#,
    );
    let bundle = Bundle::load(&b).unwrap();

    let ws_of = |content: &str| -> String {
        let v: serde_json::Value = serde_json::from_str(content).unwrap();
        v["agen/workspace"].as_str().unwrap().to_string()
    };

    let agent1 = Agent::from_bundle(&bundle, s.clone(), BundleOptions::default())
        .await
        .unwrap();
    let r1 = agent1
        .run(
            "go",
            RunOptions {
                task_id: "task-a".into(),
                conversation_key: "conv-1".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    let msgs1 = s.run_messages(&r1.run_id).await.unwrap();
    let ws1 = ws_of(&msgs1.iter().find(|m| m.role == Role::Tool).unwrap().content);

    let agent2 = Agent::from_bundle(&bundle, s.clone(), BundleOptions::default())
        .await
        .unwrap();
    let r2 = agent2
        .run(
            "go",
            RunOptions {
                task_id: "task-b".into(),
                conversation_key: "conv-1".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    let msgs2 = s.run_messages(&r2.run_id).await.unwrap();
    let ws2 = ws_of(&msgs2.iter().find(|m| m.role == Role::Tool).unwrap().content);

    assert_eq!(
        ws1, ws2,
        "two tasks of the same conversation should share one workspace directory"
    );
    assert!(
        Path::new(&ws1).is_dir(),
        "per_conversation workspace should not be removed after a task ends"
    );
}
