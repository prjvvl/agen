//! call_agent's unified delegation path (3.2b): a platform delegate (no
//! fixed url, no `via: "a2a"`) is started as a child task and waited on
//! inline; if it is not done in time, the run suspends (3.3) instead of
//! blocking or erroring, and resumes once woken.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::{Delegate, Limits};
use agen_engine::delegate::{CallAgent, StartRequest, TaskStarter, WaitOutcome};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::{RunStatus, Store};
use async_trait::async_trait;
use serde_json::json;

type Started = (String, String, String, String);

/// Starts a child task (always "task-1") and answers `wait` Pending for the
/// first `pending_waits` calls, Done after.
struct ScriptedStarter {
    starts: Mutex<Vec<Started>>,
    /// How many times start() was called, including idempotent repeats.
    start_calls: Mutex<u32>,
    waits: Mutex<u32>,
    pending_waits: u32,
    done: Mutex<Option<WaitOutcome>>,
}

impl ScriptedStarter {
    fn new(pending_waits: u32, done: WaitOutcome) -> Self {
        Self {
            starts: Mutex::new(vec![]),
            start_calls: Mutex::new(0),
            waits: Mutex::new(0),
            pending_waits,
            done: Mutex::new(Some(done)),
        }
    }
}

#[async_trait]
impl TaskStarter for ScriptedStarter {
    async fn start(&self, req: StartRequest<'_>) -> Result<String, String> {
        *self.start_calls.lock().unwrap() += 1;
        // Like the real SubmitChildTask: the same idempotency key (call_agent
        // re-calling start() on resume) returns the existing task, not a new
        // one.
        let mut starts = self.starts.lock().unwrap();
        if starts.iter().any(|s| s.2 == req.idempotency_key) {
            return Ok("task-1".into());
        }
        starts.push((
            req.name.to_string(),
            req.input.to_string(),
            req.idempotency_key.to_string(),
            req.reply_conversation_key.to_string(),
        ));
        Ok("task-1".into())
    }

    async fn wait(&self, task_id: &str, _wait: Duration) -> Result<WaitOutcome, String> {
        assert_eq!(task_id, "task-1");
        let mut n = self.waits.lock().unwrap();
        *n += 1;
        if *n <= self.pending_waits {
            return Ok(WaitOutcome::Pending);
        }
        Ok(self
            .done
            .lock()
            .unwrap()
            .take()
            .expect("wait() called again after the child was already reported done"))
    }

    async fn report_progress(&self, _task_id: &str, _progress: serde_json::Value) -> Result<(), String> {
        Ok(())
    }
}

fn call_agent(message: &str) -> FakeResponse {
    FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: "call_agent".into(),
            arguments: json!({"agent": "writer", "message": message}),
        }],
        ..Default::default()
    }
}

fn say(text: &str, expect: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        expect: Some(expect.into()),
        ..Default::default()
    }
}

#[tokio::test]
async fn call_agent_suspends_then_resumes_when_the_child_finishes() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(ScriptedStarter::new(
        1,
        WaitOutcome::Done {
            success: true,
            output: "Draft ready.".into(),
            error: String::new(),
            outcome: None,
        },
    ));
    let delegates = vec![Delegate {
        name: "writer".into(),
        ..Default::default()
    }];
    let tool = Arc::new(CallAgent::new(
        delegates,
        "default",
        None,
        Some(starter.clone()),
        Limits::default(),
        store.clone(),
    ));
    let p = Arc::new(FakeProvider::new(vec![
        call_agent("draft it"),
        say("done: Draft ready.", "Draft ready."),
    ]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
        .build()
        .unwrap();

    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Running, "suspended, not finished: {r:?}");
    assert_eq!(r.wait_for, "child:task-1");
    assert_eq!(starter.starts.lock().unwrap().len(), 1, "one child task started");
    assert_eq!(*starter.start_calls.lock().unwrap(), 1);
    assert_eq!(*starter.waits.lock().unwrap(), 1);

    // Woken (a run_inbox row matching "child:task-1" arrived): the Manager
    // re-sends the same task id, which resumes this exact run.
    let r2 = agent.resume(&r.run_id, RunOptions::default()).await.unwrap();
    assert_eq!(r2.status, RunStatus::Succeeded, "{}", r2.error);
    assert_eq!(r2.output, "done: Draft ready.");
    // call_agent calls start() again on resume (same idempotency key), but
    // that must resolve to the same child, not a second one.
    assert_eq!(
        starter.starts.lock().unwrap().len(),
        1,
        "only one distinct child task across both calls"
    );
    assert_eq!(
        *starter.start_calls.lock().unwrap(),
        2,
        "start() called again on resume"
    );
    assert_eq!(*starter.waits.lock().unwrap(), 2);
}

#[tokio::test]
async fn call_agent_does_not_suspend_when_the_child_is_already_done() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(ScriptedStarter::new(
        0,
        WaitOutcome::Done {
            success: true,
            output: "Draft ready.".into(),
            error: String::new(),
            outcome: None,
        },
    ));
    let delegates = vec![Delegate {
        name: "writer".into(),
        ..Default::default()
    }];
    let tool = Arc::new(CallAgent::new(
        delegates,
        "default",
        None,
        Some(starter.clone()),
        Limits::default(),
        store.clone(),
    ));
    let p = Arc::new(FakeProvider::new(vec![
        call_agent("draft it"),
        say("done: Draft ready.", "Draft ready."),
    ]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
        .build()
        .unwrap();

    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(
        r.status,
        RunStatus::Succeeded,
        "fast child: no suspension needed: {}",
        r.error
    );
    assert_eq!(r.wait_for, "");
    assert_eq!(r.output, "done: Draft ready.");
}

/// A delegate's `via: "a2a"` opts it out of the unified path even when a
/// task starter is configured, so it never suspends.
#[tokio::test]
async fn delegate_via_a2a_skips_the_unified_path() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let starter = Arc::new(ScriptedStarter::new(
        99,
        WaitOutcome::Done {
            success: true,
            output: "unused".into(),
            error: String::new(),
            outcome: None,
        },
    ));
    let delegates = vec![Delegate {
        name: "writer".into(),
        via: Some("a2a".into()),
        ..Default::default()
    }];
    let tool = Arc::new(CallAgent::new(
        delegates,
        "default",
        None,
        Some(starter.clone()),
        Limits::default(),
        store.clone(),
    ));
    let p = Arc::new(FakeProvider::new(vec![
        call_agent("draft it"),
        say("done", "no resolver is configured"),
    ]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
        .build()
        .unwrap();

    let r = agent.run("go", RunOptions::default()).await.unwrap();
    // No resolver and no url: the A2A path's own error reaches the model
    // (and the run finishes normally from there), proving the call used
    // A2A rather than the starter.
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(r.output, "done");
    assert!(
        starter.starts.lock().unwrap().is_empty(),
        "the starter must not be used"
    );
}
