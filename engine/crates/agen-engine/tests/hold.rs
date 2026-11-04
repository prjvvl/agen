//! Hold (3.4, part): a task `PauseTask` has held suspends at its next
//! model call instead of asking the model again, with `wait_for: "resume"`.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::delegate::{StartRequest, TaskStarter, WaitOutcome};
use agen_engine::provider::fake::{FakeProvider, FakeResponse};
use agen_engine::store::{RunStatus, Store};
use async_trait::async_trait;
use serde_json::Value;

/// Reports a settable held status; everything else needed to exercise
/// the hold check is a plain stub.
struct HeldSwitch {
    held: AtomicBool,
    fail: bool,
}

impl HeldSwitch {
    fn new(held: bool) -> Self {
        Self {
            held: AtomicBool::new(held),
            fail: false,
        }
    }
    fn failing() -> Self {
        Self {
            held: AtomicBool::new(true),
            fail: true,
        }
    }
    fn set(&self, held: bool) {
        self.held.store(held, Ordering::SeqCst);
    }
}

#[async_trait]
impl TaskStarter for HeldSwitch {
    async fn start(&self, _req: StartRequest<'_>) -> Result<String, String> {
        unreachable!("these tests never start a child")
    }
    async fn wait(&self, _task_id: &str, _wait: Duration) -> Result<WaitOutcome, String> {
        unreachable!("these tests never wait on a child")
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
        Ok(vec![])
    }
    async fn unfinished_children(&self, _task_id: &str) -> Result<i64, String> {
        Ok(0)
    }
    async fn is_held(&self, _task_id: &str) -> Result<bool, String> {
        if self.fail {
            return Err("manager unreachable".into());
        }
        Ok(self.held.load(Ordering::SeqCst))
    }
    async fn acquire_lock(&self, _task_id: &str, _name: &str, _ttl: Duration) -> Result<i64, String> {
        Ok(1)
    }
    async fn release_lock(&self, _task_id: &str, _name: &str) -> Result<(), String> {
        Ok(())
    }
}

fn say(text: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        ..Default::default()
    }
}

#[tokio::test]
async fn a_held_task_suspends_before_ever_calling_the_model_and_resumes_once_unheld() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(HeldSwitch::new(true));
    // No scripted responses at all: if the run asked the model despite
    // being held, FakeProvider would panic for running out of responses.
    let p = Arc::new(FakeProvider::new(vec![]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
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
    assert_eq!(r.status, RunStatus::Running, "suspended, not finished: {r:?}");
    assert_eq!(r.wait_for, "resume");

    starter.set(false);
    let p2 = Arc::new(FakeProvider::new(vec![say("back at it")]));
    let agent2 = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p2)
        .task_starter(starter.clone())
        .build()
        .unwrap();
    let r2 = agent2.resume(&r.run_id, RunOptions::default()).await.unwrap();
    assert_eq!(r2.status, RunStatus::Succeeded, "{}", r2.error);
    assert_eq!(r2.output, "back at it");
}

#[tokio::test]
async fn without_a_task_id_the_hold_check_never_runs() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    // Always reports held, but RunOptions below has no task_id, so the
    // check must never fire.
    let starter = Arc::new(HeldSwitch::new(true));
    let p = Arc::new(FakeProvider::new(vec![say("done")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .task_starter(starter.clone())
        .build()
        .unwrap();

    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
}

#[tokio::test]
async fn a_failed_hold_check_does_not_suspend_the_turn() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(HeldSwitch::failing());
    let p = Arc::new(FakeProvider::new(vec![say("carried on")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
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
    assert_eq!(r.output, "carried on");
}
