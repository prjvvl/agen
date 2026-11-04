//! "join" (3.4, part): a `start_agent` child without `detach: true` ties
//! its parent to it — the parent cannot finish `done`, and its turn loop
//! suspends instead of calling the model again, while any such child is
//! still unfinished.

use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::{Delegate, Limits};
use agen_engine::delegate::{StartAgent, StartRequest, TaskStarter, WaitOutcome};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::{RunStatus, Store};
use async_trait::async_trait;
use serde_json::{json, Value};

/// Reports a settable count of unfinished joined children; everything
/// else needed to exercise start_agent is a plain stub.
struct JoinChildren {
    count: AtomicI64,
}

impl JoinChildren {
    fn new(n: i64) -> Self {
        Self {
            count: AtomicI64::new(n),
        }
    }
    fn set(&self, n: i64) {
        self.count.store(n, Ordering::SeqCst);
    }
}

#[async_trait]
impl TaskStarter for JoinChildren {
    async fn start(&self, _req: StartRequest<'_>) -> Result<String, String> {
        Ok("task-1".into())
    }
    async fn wait(&self, _task_id: &str, _wait: Duration) -> Result<WaitOutcome, String> {
        unreachable!("these tests never wait on a child directly")
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
        Ok(self.count.load(Ordering::SeqCst))
    }
    async fn is_held(&self, _task_id: &str) -> Result<bool, String> {
        Ok(false)
    }
    async fn acquire_lock(&self, _task_id: &str, _name: &str, _ttl: Duration) -> Result<i64, String> {
        Ok(1)
    }
    async fn release_lock(&self, _task_id: &str, _name: &str) -> Result<(), String> {
        Ok(())
    }
    async fn tree_budget_exceeded(&self, _task_id: &str) -> Result<bool, String> {
        Ok(false)
    }
}

fn start(agent: &str) -> FakeResponse {
    FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: "start_agent".into(),
            arguments: json!({"agent": agent, "message": "do the long thing"}),
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
async fn a_joined_child_suspends_the_run_and_resumes_once_it_is_done() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(JoinChildren::new(1));
    let delegates = vec![Delegate {
        name: "writer".into(),
        ..Default::default()
    }];
    let tool = Arc::new(StartAgent::new(
        delegates,
        "default",
        starter.clone(),
        Limits::default(),
        store.clone(),
    ));
    // Only one scripted turn until resume: if the run asked the model
    // again instead of suspending, FakeProvider would panic for running
    // out of responses.
    let p = Arc::new(FakeProvider::new(vec![start("writer"), say("all set")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
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
    assert_eq!(r.wait_for, "children");

    // The child is done now: resuming proceeds to call the model again.
    starter.set(0);
    let r2 = agent.resume(&r.run_id, RunOptions::default()).await.unwrap();
    assert_eq!(r2.status, RunStatus::Succeeded, "{}", r2.error);
    assert_eq!(r2.output, "all set");
}

#[tokio::test]
async fn finish_done_is_refused_while_a_joined_child_is_still_running() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(JoinChildren::new(1));
    // Only one scripted turn: finish is refused and the run suspends right
    // after, in the same turn boundary, without asking the model again.
    let p = Arc::new(FakeProvider::new(vec![FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: "finish".into(),
            arguments: json!({"status": "done", "summary": "all done"}),
        }],
        ..Default::default()
    }]));
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
    assert_eq!(
        r.status,
        RunStatus::Running,
        "finish done must be refused while a joined child is outstanding: {r:?}"
    );
    assert_eq!(r.wait_for, "children");
}

#[tokio::test]
async fn finish_done_succeeds_once_there_is_nothing_left_to_join() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(JoinChildren::new(0));
    let p = Arc::new(FakeProvider::new(vec![FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: "finish".into(),
            arguments: json!({"status": "done", "summary": "all done"}),
        }],
        ..Default::default()
    }]));
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
}

#[tokio::test]
async fn without_a_task_id_neither_check_ever_runs() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    // Always reports a blocking child; if either check ran despite no
    // task_id (a standalone-style run), this would suspend or refuse
    // finish instead of succeeding.
    let starter = Arc::new(JoinChildren::new(1));
    let p = Arc::new(FakeProvider::new(vec![say("just an answer")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .task_starter(starter.clone())
        .build()
        .unwrap();

    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
}
