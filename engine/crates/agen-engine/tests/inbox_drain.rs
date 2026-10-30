//! Draining the task's run_inbox into a run before its next model call
//! (3.4/3.9, part): the missing piece behind signal_agent actually doing
//! anything, and the eventual "steer" queue mode.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::delegate::{StartRequest, TaskStarter, WaitOutcome};
use agen_engine::provider::fake::{FakeProvider, FakeResponse};
use agen_engine::store::{RunStatus, Store};
use async_trait::async_trait;
use serde_json::{json, Value};

/// Returns the scripted entries once, then nothing (like the real
/// Store.DrainInbox: claiming is a one-shot read).
struct ScriptedInbox {
    entries: Mutex<Option<Vec<Value>>>,
    calls: Mutex<u32>,
}

impl ScriptedInbox {
    fn new(entries: Vec<Value>) -> Self {
        Self {
            entries: Mutex::new(Some(entries)),
            calls: Mutex::new(0),
        }
    }
}

#[async_trait]
impl TaskStarter for ScriptedInbox {
    async fn start(&self, _req: StartRequest<'_>) -> Result<String, String> {
        unreachable!("not exercised by these tests")
    }
    async fn wait(&self, _task_id: &str, _wait: Duration) -> Result<WaitOutcome, String> {
        unreachable!("not exercised by these tests")
    }
    async fn report_progress(&self, _task_id: &str, _progress: Value) -> Result<(), String> {
        Ok(())
    }
    async fn query_descendant(&self, _caller_task_id: &str, _task_id: &str) -> Result<Value, String> {
        Ok(Value::Null)
    }
    async fn signal_descendant(&self, _caller_task_id: &str, _task_id: &str, _message: &str) -> Result<(), String> {
        Ok(())
    }
    async fn drain_inbox(&self, task_id: &str) -> Result<Vec<Value>, String> {
        assert_eq!(task_id, "task-1");
        *self.calls.lock().unwrap() += 1;
        Ok(self.entries.lock().unwrap().take().unwrap_or_default())
    }

    async fn unfinished_children(&self, _task_id: &str) -> Result<i64, String> {
        Ok(0)
    }
}

fn say(text: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        ..Default::default()
    }
}

#[tokio::test]
async fn a_signal_is_drained_into_a_message_before_the_next_model_call() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(ScriptedInbox::new(vec![json!({
        "kind": "signal", "ref": "caller-1", "payload": "hurry up"
    })]));
    let p = Arc::new(FakeProvider::new(vec![say("ok")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p.clone())
        .task_starter(starter.clone())
        .build()
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
    assert_eq!(*starter.calls.lock().unwrap(), 1);

    let reqs = p.requests();
    assert_eq!(reqs.len(), 1);
    let texts: Vec<&str> = reqs[0].messages.iter().map(|m| m.content.as_str()).collect();
    assert!(
        texts.iter().any(|t| t.contains("hurry up") && t.contains("caller-1")),
        "expected a drained signal message, got {texts:?}"
    );
}

#[tokio::test]
async fn a_task_without_a_task_id_never_drains() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(ScriptedInbox::new(vec![
        json!({"kind": "signal", "ref": "x", "payload": "y"}),
    ]));
    let p = Arc::new(FakeProvider::new(vec![say("ok")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .task_starter(starter.clone())
        .build()
        .unwrap();

    // No task_id (standalone-style run): drain_inbox is never called, so
    // its scripted entry (which would panic on a second use) is untouched.
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(*starter.calls.lock().unwrap(), 0);
}

#[tokio::test]
async fn a_drain_error_does_not_fail_the_turn() {
    struct FailingInbox;
    #[async_trait]
    impl TaskStarter for FailingInbox {
        async fn start(&self, _req: StartRequest<'_>) -> Result<String, String> {
            unreachable!()
        }
        async fn wait(&self, _task_id: &str, _wait: Duration) -> Result<WaitOutcome, String> {
            unreachable!()
        }
        async fn report_progress(&self, _task_id: &str, _progress: Value) -> Result<(), String> {
            Ok(())
        }
        async fn query_descendant(&self, _caller_task_id: &str, _task_id: &str) -> Result<Value, String> {
            Ok(Value::Null)
        }
        async fn signal_descendant(&self, _caller_task_id: &str, _task_id: &str, _message: &str) -> Result<(), String> {
            Ok(())
        }
        async fn drain_inbox(&self, _task_id: &str) -> Result<Vec<Value>, String> {
            Err("manager unreachable".into())
        }
        async fn unfinished_children(&self, _task_id: &str) -> Result<i64, String> {
            Ok(0)
        }
    }
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let p = Arc::new(FakeProvider::new(vec![say("ok")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .task_starter(Arc::new(FailingInbox))
        .build()
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
    assert_eq!(
        r.status,
        RunStatus::Succeeded,
        "a failed drain must not fail the run: {}",
        r.error
    );
}
