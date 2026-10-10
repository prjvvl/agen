//! Conversation keys, labels and task identity, concurrent tool calls, output
//! caps from the token budget, and traces of interrupted runs.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::provider::{DeltaSink, ModelProvider, ModelRequest, ModelResponse, ProviderError, Usage};
use agen_engine::store::{RunStatus, Store};
use agen_engine::tools::{FnTool, Tool, ToolContext, ToolError};
use async_trait::async_trait;
use serde_json::{json, Value};

async fn store() -> (Arc<Store>, tempfile::TempDir) {
    let d = tempfile::tempdir().unwrap();
    (
        Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap()),
        d,
    )
}

fn calls(names: &[&str]) -> FakeResponse {
    FakeResponse {
        tool_calls: names
            .iter()
            .map(|n| ScriptedToolCall {
                name: n.to_string(),
                arguments: json!({}),
            })
            .collect(),
        ..Default::default()
    }
}

fn say(text: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        ..Default::default()
    }
}

/// A tool that takes 150 ms and records how many calls ran at once.
fn slow_tool(name: &str, running: Arc<AtomicUsize>, peak: Arc<AtomicUsize>) -> Arc<dyn Tool> {
    let out = name.to_string();
    Arc::new(FnTool::new(
        name,
        "slow",
        json!({"type": "object"}),
        false,
        move |_: Value, _| {
            let (running, peak, out) = (running.clone(), peak.clone(), out.clone());
            async move {
                let now = running.fetch_add(1, Ordering::SeqCst) + 1;
                peak.fetch_max(now, Ordering::SeqCst);
                tokio::time::sleep(Duration::from_millis(150)).await;
                running.fetch_sub(1, Ordering::SeqCst);
                Ok::<_, ToolError>(out)
            }
        },
    ))
}

async fn peak_concurrency(parallel: bool) -> usize {
    let (s, _d) = store().await;
    let (running, peak) = (Arc::new(AtomicUsize::new(0)), Arc::new(AtomicUsize::new(0)));
    let mut c = AgentConfig::new("calc", "x", "fake-1");
    c.parallel_tool_calls = parallel;
    let agent = Agent::builder(c, s.clone())
        .provider(Arc::new(FakeProvider::new(vec![calls(&["a", "b", "c"]), say("done")])))
        .tool(slow_tool("a", running.clone(), peak.clone()))
        .tool(slow_tool("b", running.clone(), peak.clone()))
        .tool(slow_tool("c", running.clone(), peak.clone()))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    // Results are recorded in the order the model asked for them.
    let results: Vec<String> = s
        .run_messages(&r.run_id)
        .await
        .unwrap()
        .into_iter()
        .filter(|m| m.tool_call_id.is_some())
        .map(|m| m.content)
        .collect();
    assert_eq!(results, ["a", "b", "c"]);
    peak.load(Ordering::SeqCst)
}

#[tokio::test]
async fn tool_calls_of_one_turn_run_concurrently() {
    assert_eq!(peak_concurrency(true).await, 3);
}

#[tokio::test]
async fn parallel_tool_calls_can_be_turned_off() {
    assert_eq!(peak_concurrency(false).await, 1);
}

/// Records each request's output limit; answers with a fixed usage.
struct Recorder {
    inner: FakeProvider,
    caps: Mutex<Vec<Option<u32>>>,
}

#[async_trait]
impl ModelProvider for Recorder {
    fn name(&self) -> &str {
        "recorder"
    }
    async fn complete(&self, req: &ModelRequest, on_delta: DeltaSink<'_>) -> Result<ModelResponse, ProviderError> {
        self.caps.lock().unwrap().push(req.max_output_tokens);
        self.inner.complete(req, on_delta).await
    }
}

#[tokio::test]
async fn output_is_capped_by_the_remaining_token_budget() {
    let (s, _d) = store().await;
    let usage = Usage {
        input_tokens: 300,
        output_tokens: 100,
        cost_usd: 0.0,
    };
    let mut first = calls(&["noop"]);
    first.usage = Some(usage);
    let mut last = say("done");
    last.usage = Some(usage);
    let p = Arc::new(Recorder {
        inner: FakeProvider::new(vec![first, last]),
        caps: Mutex::default(),
    });
    let mut c = AgentConfig::new("calc", "x", "fake-1");
    c.max_output_tokens = Some(800);
    c.budget.max_tokens_per_run = Some(1000);
    let noop: Arc<dyn Tool> = Arc::new(FnTool::new(
        "noop",
        "x",
        json!({"type": "object"}),
        false,
        |_: Value, _| async { Ok::<_, ToolError>("ok".to_string()) },
    ));
    let agent = Agent::builder(c, s).provider(p.clone()).tool(noop).build().unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    // Turn 1: the configured 800 (1000 remain); turn 2: 1000 - 400 used.
    assert_eq!(*p.caps.lock().unwrap(), [Some(800), Some(600)]);
}

#[tokio::test]
async fn a_conversation_key_continues_one_conversation() {
    let (s, _d) = store().await;
    let agent = Agent::builder(AgentConfig::new("calc", "x", "fake-1"), s.clone())
        .provider(Arc::new(FakeProvider::new(vec![say("one")]).cycling()))
        .build()
        .unwrap();
    let keyed = |k: &str| RunOptions {
        conversation_key: k.into(),
        ..Default::default()
    };
    let a = agent.run("first", keyed("chat-1")).await.unwrap();
    let b = agent.run("second", keyed("chat-1")).await.unwrap();
    let other = agent.run("third", keyed("chat-2")).await.unwrap();
    let fresh = agent.run("fourth", RunOptions::default()).await.unwrap();
    assert_eq!(
        (a.session_id.as_str(), a.conversation_id.as_str()),
        (b.session_id.as_str(), b.conversation_id.as_str())
    );
    assert_ne!(a.conversation_id, other.conversation_id);
    assert_ne!(a.conversation_id, fresh.conversation_id);
    let history = s.messages(&b.conversation_id).await.unwrap();
    assert!(history.iter().any(|m| m.content == "first"), "{history:?}");
}

#[tokio::test]
async fn tools_see_the_task_run_conversation_and_labels() {
    let (s, _d) = store().await;
    let seen: Arc<Mutex<Option<ToolContext>>> = Arc::default();
    let probe = seen.clone();
    let tool: Arc<dyn Tool> = Arc::new(FnTool::new(
        "probe",
        "x",
        json!({"type": "object"}),
        false,
        move |_: Value, ctx| {
            *probe.lock().unwrap() = Some(ctx);
            async { Ok::<_, ToolError>("ok".to_string()) }
        },
    ));
    let agent = Agent::builder(AgentConfig::new("calc", "x", "fake-1"), s.clone())
        .provider(Arc::new(FakeProvider::new(vec![calls(&["probe"]), say("done")])))
        .tool(tool)
        .build()
        .unwrap();
    let opts = RunOptions {
        task_id: "task-1".into(),
        conversation_key: "chat-9".into(),
        labels: [("project".to_string(), "apollo".to_string())].into(),
        ..Default::default()
    };
    let r = agent.run("go", opts).await.unwrap();
    let ctx = seen.lock().unwrap().clone().expect("tool was called");
    assert_eq!(ctx.task_id, "task-1");
    assert_eq!(ctx.run_id, r.run_id);
    assert_eq!(ctx.conversation_id, r.conversation_id);
    assert_eq!(ctx.conversation_key, "chat-9");
    assert_eq!(ctx.labels["project"], "apollo");
    assert_eq!((ctx.namespace.as_str(), ctx.deployment.as_str()), ("default", "calc"));
    let run = s.get_run(&r.run_id).await.unwrap();
    assert_eq!(run.labels["project"], "apollo");
    let spans = s.trace(&r.trace_id).await.unwrap();
    let root = spans.iter().find(|s| s.name == "agen.run").unwrap();
    assert_eq!(root.attributes["agen.label.project"], "apollo");
    assert_eq!(root.status, "ok");
}

#[tokio::test]
async fn a_resumed_run_hangs_under_its_interrupted_span() {
    let (s, _d) = store().await;
    let started = Arc::new(AtomicUsize::new(0));
    let hit = started.clone();
    let hang: Arc<dyn Tool> = Arc::new(FnTool::new(
        "lookup",
        "x",
        json!({"type": "object"}),
        false,
        move |_: Value, _| {
            hit.fetch_add(1, Ordering::SeqCst);
            async {
                tokio::time::sleep(Duration::from_secs(3600)).await;
                Ok::<_, ToolError>("never".to_string())
            }
        },
    ));
    let agent = Arc::new(
        Agent::builder(AgentConfig::new("calc", "x", "fake-1"), s.clone())
            .provider(Arc::new(FakeProvider::new(vec![calls(&["lookup"])])))
            .tool(hang)
            .build()
            .unwrap(),
    );
    let a = agent.clone();
    let task = tokio::spawn(async move { a.run("go", RunOptions::default()).await });
    while started.load(Ordering::SeqCst) == 0 {
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    task.abort();
    let _ = task.await;
    let run_id = s.unfinished_runs("default", "calc").await.unwrap()[0].id.clone();

    let quick: Arc<dyn Tool> = Arc::new(FnTool::new(
        "lookup",
        "x",
        json!({"type": "object"}),
        false,
        |_: Value, _| async { Ok::<_, ToolError>("found".to_string()) },
    ));
    let agent = Agent::builder(AgentConfig::new("calc", "x", "fake-1"), s.clone())
        .provider(Arc::new(FakeProvider::new(vec![say("done")])))
        .tool(quick)
        .build()
        .unwrap();
    let opts = RunOptions {
        attempt: 2,
        ..Default::default()
    };
    let r = agent.resume(&run_id, opts).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let spans = s.trace(&r.trace_id).await.unwrap();
    let first = spans.iter().find(|s| s.name == "agen.run").unwrap();
    assert_eq!(first.status, "interrupted");
    assert!(first.end_ms >= first.start_ms);
    let resumed = spans.iter().find(|s| s.name == "agen.run.resume").unwrap();
    assert_eq!(resumed.parent_span_id, first.span_id);
    assert_eq!(resumed.attributes["agen.task.attempt"], 2);
    assert_eq!(resumed.status, "ok");
}
