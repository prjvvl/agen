//! Live tree-budget check (3.5, part): before every model call, the
//! engine asks the configured `TaskStarter` whether the current task's
//! tree has, right now, spent at least as much as its root's own
//! declared budget. This catches a sibling's concurrent spend that the
//! submission-time-only narrowing (`narrowForTreeBudget`) alone can't: a
//! running sibling's own, already-fixed budget never shrinks just
//! because another one is spending in parallel.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::delegate::{StartRequest, TaskStarter, WaitOutcome};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::{RunStatus, Store};
use agen_engine::tools::{FnTool, Tool, ToolError};
use async_trait::async_trait;
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

fn add_tool() -> Arc<dyn Tool> {
    Arc::new(FnTool::new(
        "add",
        "Add two numbers",
        json!({"type": "object"}),
        false,
        |a: Value, _| async move { Ok::<_, ToolError>((a["a"].as_i64().unwrap() + a["b"].as_i64().unwrap()).to_string()) },
    ))
}

/// Reports the tree as over budget from its `after`th check onward (0 =
/// immediately); otherwise a plain stub, since these tests never start,
/// wait for, hold or lock anything.
struct TreeOverBudgetAfter {
    checks: AtomicUsize,
    after: usize,
    fail: bool,
}

impl TreeOverBudgetAfter {
    fn after(n: usize) -> Self {
        Self {
            checks: AtomicUsize::new(0),
            after: n,
            fail: false,
        }
    }
    fn failing() -> Self {
        Self {
            checks: AtomicUsize::new(0),
            after: 0,
            fail: true,
        }
    }
}

#[async_trait]
impl TaskStarter for TreeOverBudgetAfter {
    async fn start(&self, _req: StartRequest<'_>) -> Result<String, String> {
        unreachable!("these tests never start a child")
    }
    async fn wait(&self, _task_id: &str, _wait: Duration) -> Result<WaitOutcome, String> {
        unreachable!()
    }
    async fn report_progress(&self, _task_id: &str, _progress: Value) -> Result<(), String> {
        unreachable!()
    }
    async fn query_descendant(&self, _caller_task_id: &str, _task_id: &str) -> Result<Value, String> {
        unreachable!()
    }
    async fn signal_descendant(&self, _caller_task_id: &str, _task_id: &str, _message: &str) -> Result<(), String> {
        unreachable!()
    }
    async fn drain_inbox(&self, _task_id: &str) -> Result<Vec<Value>, String> {
        Ok(vec![])
    }
    async fn unfinished_children(&self, _task_id: &str) -> Result<i64, String> {
        Ok(0)
    }
    async fn is_held(&self, _task_id: &str) -> Result<bool, String> {
        Ok(false)
    }
    async fn acquire_lock(&self, _task_id: &str, _name: &str, _ttl: Duration) -> Result<i64, String> {
        unreachable!()
    }
    async fn release_lock(&self, _task_id: &str, _name: &str) -> Result<(), String> {
        unreachable!()
    }
    async fn tree_budget_exceeded(&self, _task_id: &str) -> Result<bool, String> {
        if self.fail {
            return Err("manager unreachable".into());
        }
        Ok(self.checks.fetch_add(1, Ordering::SeqCst) >= self.after)
    }
}

// A tree already over budget at the very start of a turn ends the run as
// a graceful partial finish, with no model call at all when
// report_tokens is off (mirrors the per-run budget_exceeded check's own
// "reportTokens 0 fails the run outright" behavior).
#[tokio::test]
async fn a_tree_already_over_budget_ends_the_run_without_ever_calling_the_model() {
    let (s, _d) = store().await;
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.report_tokens = 0;
    let starter = Arc::new(TreeOverBudgetAfter::after(0));
    // No scripted responses: a model call here would panic the fake
    // provider for running out of responses.
    let p = Arc::new(FakeProvider::new(vec![]));
    let agent = Agent::builder(cfg, s)
        .provider(p)
        .task_starter(starter)
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
    assert_eq!(r.status, RunStatus::Failed, "{r:?}");
    assert!(r.error.contains("tree_budget_exceeded"), "{}", r.error);
    let o = r.outcome.unwrap();
    assert_eq!((o.status.as_str(), o.reason.as_str()), ("failed", "tree_budget"));
}

// A tree that goes over budget partway through a run (a sibling's
// concurrent spend, simulated here as the check flipping after the
// first turn) ends with a graceful partial report, the same as the
// per-run budget_exceeded check already does — proving the live check
// fires mid-run, not only at the very first turn.
#[tokio::test]
async fn a_tree_that_goes_over_budget_mid_run_ends_with_a_partial_report() {
    let (s, _d) = store().await;
    let cfg = AgentConfig::new("a", "x", "fake-1");
    let starter = Arc::new(TreeOverBudgetAfter::after(1));
    let p = Arc::new(FakeProvider::new(vec![
        call("add", json!({"a": 1, "b": 2})),
        say("added 1 and 2; the rest is undone"),
    ]));
    let calls = p.clone();
    let agent = Agent::builder(cfg, s)
        .provider(p)
        .tool(add_tool())
        .task_starter(starter)
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
    let o = r.outcome.unwrap();
    assert_eq!(
        (o.status.as_str(), o.reason.as_str(), o.summary.as_str()),
        ("partial", "tree_budget", "added 1 and 2; the rest is undone")
    );
    assert_eq!(calls.calls(), 2, "the real turn, then only the report call");
}

// A failed check (the Manager unreachable) never stops or suspends the
// run on its own, same as unfinished_children/is_held's own failure
// handling — this must never be the reason a run breaks.
#[tokio::test]
async fn a_failed_tree_budget_check_never_stops_the_run() {
    let (s, _d) = store().await;
    let cfg = AgentConfig::new("a", "x", "fake-1");
    let starter = Arc::new(TreeOverBudgetAfter::failing());
    let p = Arc::new(FakeProvider::new(vec![say("done")]));
    let agent = Agent::builder(cfg, s)
        .provider(p)
        .task_starter(starter)
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
    assert_eq!(r.output, "done");
}

// Without a task_id (standalone/embedded use, no managed platform) the
// check never runs at all, same as is_held/unfinished_children.
#[tokio::test]
async fn without_a_task_id_the_check_never_runs() {
    let (s, _d) = store().await;
    let cfg = AgentConfig::new("a", "x", "fake-1");
    let starter = Arc::new(TreeOverBudgetAfter::after(0));
    let p = Arc::new(FakeProvider::new(vec![say("done")]));
    let agent = Agent::builder(cfg, s)
        .provider(p)
        .task_starter(starter)
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(r.output, "done");
}
