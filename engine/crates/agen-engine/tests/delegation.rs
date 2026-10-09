//! call_agent: A2A message/send with lineage, trace context and limits.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use agen_engine::agent::{Agent, BundleOptions, RunOptions};
use agen_engine::bundle::Bundle;
use agen_engine::store::{RunStatus, Store};
use axum::extract::State;
use axum::routing::post;
use axum::{Json, Router};
use serde_json::{json, Value};

type Seen = Arc<Mutex<Vec<Value>>>;

/// A fake Gateway that answers every message/send with "Draft ready.".
async fn fake_gateway() -> (String, Seen) {
    let seen: Seen = Arc::default();
    async fn handle(State(seen): State<Seen>, Json(body): Json<Value>) -> Json<Value> {
        seen.lock().unwrap().push(body.clone());
        let id = format!("a2a-{}", body["params"]["message"]["messageId"].as_str().unwrap_or(""));
        Json(json!({"jsonrpc": "2.0", "id": body["id"], "result": {
            "kind": "task", "id": id, "contextId": "c",
            "status": {"state": "completed"},
            "artifacts": [{"artifactId": "output", "parts": [{"kind": "text", "text": "Draft ready."}]}]
        }}))
    }
    let app = Router::new()
        .route("/a2a/default/writer", post(handle))
        .with_state(seen.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/a2a/default/writer", listener.local_addr().unwrap());
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (url, seen)
}

fn copy_dir(src: &Path, dst: &Path) {
    std::fs::create_dir_all(dst).unwrap();
    for e in std::fs::read_dir(src).unwrap() {
        let e = e.unwrap();
        let to = dst.join(e.file_name());
        if e.file_type().unwrap().is_dir() {
            copy_dir(&e.path(), &to);
        } else {
            std::fs::copy(e.path(), to).unwrap();
        }
    }
}

/// The hello bundle with delegates and a scripted model.
fn boss_bundle(dir: &Path, config: Value, script: Value) -> Bundle {
    let hello = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../examples/bundles/hello");
    copy_dir(&hello, dir);
    std::fs::write(dir.join("x-agen/config.json"), config.to_string()).unwrap();
    std::fs::write(dir.join("x-agen/fake-script.json"), script.to_string()).unwrap();
    Bundle::load(dir).unwrap()
}

fn call_agent(message: &str) -> Value {
    json!({"toolCalls": [{"name": "call_agent", "arguments": {"agent": "writer", "message": message}}]})
}

#[tokio::test]
async fn call_agent_sends_lineage_and_trace_and_enforces_fan_out() {
    let (url, seen) = fake_gateway().await;
    let d = tempfile::tempdir().unwrap();
    let bundle = boss_bundle(
        &d.path().join("boss"),
        json!({"delegates": [{"name": "writer", "url": url, "description": "Writes drafts"}], "limits": {"maxFanOut": 2},
            "permissions": {"default": "deny", "rules": [{"tool": "call_agent", "action": "allow"}]}}),
        json!({"responses": [
            call_agent("draft it"),
            {"toolCalls": [{"name": "call_agent", "arguments": {"agent": "writer", "message": "again"}}], "expect": "Draft ready."},
            {"toolCalls": [{"name": "call_agent", "arguments": {"agent": "writer", "message": "third"}}], "expect": "Draft ready."},
            {"text": "done", "expect": "max_fan_out"}
        ]}),
    );
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let agent = Agent::from_bundle(&bundle, store.clone(), BundleOptions::default())
        .await
        .unwrap();
    let spec = agent.tools().get("call_agent").expect("call_agent registered").spec();
    assert!(spec.description.contains("writer: Writes drafts"));
    let r = agent.run("write something", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(r.output, "done");

    let seen = seen.lock().unwrap().clone();
    assert_eq!(seen.len(), 2, "third call must be stopped by max_fan_out");
    let first = &seen[0]["params"];
    assert_eq!(first["message"]["parts"][0]["text"], "draft it");
    let meta = &first["metadata"];
    assert_eq!(meta["agen.depth"], 1);
    assert_eq!(meta["agen.parent_run_id"], r.run_id);
    assert_eq!(meta["agen.root_run_id"], r.run_id);
    let tp = meta["traceparent"].as_str().unwrap();
    assert!(tp.starts_with(&format!("00-{}-", r.trace_id)), "{tp}");
    // The traceparent's span is this run's agen.tool span.
    let spans = store.trace(&r.trace_id).await.unwrap();
    let tool_span = spans
        .iter()
        .find(|s| s.name == "agen.tool" && tp.contains(&s.span_id))
        .expect("traceparent names the tool span");
    assert_eq!(tool_span.attributes["gen_ai.tool.name"], "call_agent");
    // Message ids are stable per run/step/call (so a retry is the same task)
    // and distinct across calls.
    let m0 = first["message"]["messageId"].as_str().unwrap();
    let m1 = seen[1]["params"]["message"]["messageId"].as_str().unwrap();
    assert!(m0.starts_with(&format!("{}.", r.run_id)) && m0 != m1);
}

#[tokio::test]
async fn call_agent_refuses_beyond_max_delegation_depth() {
    let (url, seen) = fake_gateway().await;
    let d = tempfile::tempdir().unwrap();
    let bundle = boss_bundle(
        &d.path().join("boss"),
        json!({"delegates": [{"name": "writer", "url": url}], "limits": {"maxDelegationDepth": 2},
            "permissions": {"default": "deny", "rules": [{"tool": "call_agent", "action": "allow"}]}}),
        json!({"responses": [call_agent("x"), {"text": "stopped", "expect": "max_delegation_depth"}]}),
    );
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let agent = Agent::from_bundle(&bundle, store, BundleOptions::default())
        .await
        .unwrap();
    // This run is itself at depth 2 (a delegated call of a delegated call).
    let r = agent
        .run(
            "go",
            RunOptions {
                depth: 2,
                root_run_id: "root-1".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert!(seen.lock().unwrap().is_empty());
}

#[tokio::test]
async fn delegations_count_once_per_message_id() {
    let d = tempfile::tempdir().unwrap();
    let s = Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap();
    assert_eq!(s.record_delegation("m1", "root", "r1").await.unwrap(), Some((1, 1)));
    // A retry or resume of the same call is not counted again.
    assert_eq!(s.record_delegation("m1", "root", "r1").await.unwrap(), None);
    assert_eq!(s.record_delegation("m2", "root", "r2").await.unwrap(), Some((1, 2)));
    // A refused call is forgotten, so its retry is checked again.
    s.forget_delegation("m2").await.unwrap();
    assert_eq!(s.record_delegation("m2", "root", "r2").await.unwrap(), Some((1, 2)));
}

#[tokio::test]
async fn call_agent_enforces_max_total_delegations_across_the_tree() {
    let (url, seen) = fake_gateway().await;
    let d = tempfile::tempdir().unwrap();
    let bundle = boss_bundle(
        &d.path().join("boss"),
        json!({"delegates": [{"name": "writer", "url": url}], "limits": {"maxTotalDelegations": 2},
            "permissions": {"default": "deny", "rules": [{"tool": "call_agent", "action": "allow"}]}}),
        json!({"responses": [
            call_agent("one"),
            {"toolCalls": [{"name": "call_agent", "arguments": {"agent": "writer", "message": "two"}}], "expect": "Draft ready."},
            {"text": "done", "expect": "max_total_delegations"}
        ]}),
    );
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    // One call was already made elsewhere in this tree.
    store.record_delegation("other", "root-1", "sibling").await.unwrap();
    let agent = Agent::from_bundle(&bundle, store, BundleOptions::default())
        .await
        .unwrap();
    let r = agent
        .run(
            "go",
            RunOptions {
                depth: 1,
                root_run_id: "root-1".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(seen.lock().unwrap().len(), 1, "second call exceeds the tree total");
}

/// Cancelling the caller's run stops the callee: the engine sends
/// tasks/cancel (by message id) to the Gateway it was calling.
#[tokio::test]
async fn cancelling_the_caller_cancels_the_callee() {
    let cancels: Seen = Arc::default();
    async fn handle(State(cancels): State<Seen>, Json(body): Json<Value>) -> Json<Value> {
        if body["method"] == "tasks/cancel" {
            cancels.lock().unwrap().push(body.clone());
            return Json(
                json!({"jsonrpc": "2.0", "id": body["id"], "result": {"kind": "task", "status": {"state": "canceled"}}}),
            );
        }
        tokio::time::sleep(std::time::Duration::from_secs(30)).await; // a long callee
        Json(json!({"jsonrpc": "2.0", "id": body["id"], "result": {"status": {"state": "completed"}}}))
    }
    let app = Router::new()
        .route("/a2a/default/writer", post(handle))
        .with_state(cancels.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/a2a/default/writer", listener.local_addr().unwrap());
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });

    let d = tempfile::tempdir().unwrap();
    let bundle = boss_bundle(
        &d.path().join("boss"),
        json!({"delegates": [{"name": "writer", "url": url}],
            "permissions": {"default": "deny", "rules": [{"tool": "call_agent", "action": "allow"}]}}),
        json!({"perRun": true, "responses": [call_agent("long job"), {"text": "never"}]}),
    );
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let agent = Agent::from_bundle(&bundle, store, BundleOptions::default())
        .await
        .unwrap();
    let cancel = tokio_util::sync::CancellationToken::new();
    let c = cancel.clone();
    tokio::spawn(async move {
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        c.cancel();
    });
    let r = agent
        .run(
            "go",
            RunOptions {
                cancel,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r.status, RunStatus::Cancelled);
    // The cancel notice is sent in the background as the call is dropped.
    for _ in 0..50 {
        if !cancels.lock().unwrap().is_empty() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    }
    let cancels = cancels.lock().unwrap().clone();
    assert_eq!(cancels.len(), 1, "callee must be told to cancel");
    let mid = cancels[0]["params"]["metadata"]["messageId"].as_str().unwrap();
    assert!(mid.starts_with(&format!("{}.", r.run_id)), "{mid}");
}
