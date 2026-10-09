//! Regression tests for loop robustness issues found in review: duplicate
//! call ids, cancellation, concurrency, ownership fencing, approval timeouts,
//! retries while streaming, context trimming, and redaction edge cases.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use agen_engine::agent::{Agent, AgentConfig, AgentError, ApprovalDecision, ApprovalRequest, Approver, RunOptions};
use agen_engine::bundle::{Action, PermissionRule, Permissions};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::provider::replay::RecordingProvider;
use agen_engine::provider::{
    DeltaSink, ModelProvider, ModelRequest, ModelResponse, ProviderError, Role, ToolCall, Usage,
};
use agen_engine::secrets::Secrets;
use agen_engine::store::{RunStatus, Store, StoreError};
use agen_engine::tools::{FnTool, Tool, ToolContext, ToolError};
use async_trait::async_trait;
use serde_json::{json, Value};
use tokio::sync::Notify;

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

fn cfg() -> AgentConfig {
    AgentConfig::new("rb", "Be helpful.", "fake-1")
}

fn counter_tool(name: &str, side_effect: bool, count: Arc<AtomicUsize>) -> Arc<dyn Tool> {
    Arc::new(FnTool::new(
        name,
        "counts",
        json!({"type":"object"}),
        side_effect,
        move |_a: Value, _| {
            let count = count.clone();
            async move {
                let n = count.fetch_add(1, Ordering::SeqCst) + 1;
                Ok::<_, ToolError>(format!("call #{n}"))
            }
        },
    ))
}

/// A provider that always reuses the same tool call id (as some do).
struct DupIds(AtomicUsize);
#[async_trait]
impl ModelProvider for DupIds {
    fn name(&self) -> &str {
        "dup"
    }
    async fn complete(&self, _req: &ModelRequest, _d: DeltaSink<'_>) -> Result<ModelResponse, ProviderError> {
        let n = self.0.fetch_add(1, Ordering::SeqCst);
        let tool_calls = if n < 2 {
            vec![ToolCall {
                id: "call_0".into(),
                name: "pay".into(),
                arguments: json!({"n": n}),
            }]
        } else {
            vec![]
        };
        Ok(ModelResponse {
            text: if n < 2 { String::new() } else { "done".into() },
            tool_calls,
            usage: Usage::default(),
            finish_reason: String::new(),
        })
    }
}

#[tokio::test]
async fn reused_call_ids_across_turns_each_execute_once() {
    let (s, _d) = store().await;
    let count = Arc::new(AtomicUsize::new(0));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(Arc::new(DupIds(AtomicUsize::new(0))))
        .tool(counter_tool("pay", true, count.clone()))
        .build()
        .unwrap();
    let r = agent.run("pay twice", RunOptions::default()).await.unwrap();
    assert_eq!(
        (r.status, r.output.as_str()),
        (RunStatus::Succeeded, "done"),
        "{}",
        r.error
    );
    assert_eq!(count.load(Ordering::SeqCst), 2);
    let tools: Vec<String> = s
        .run_messages(&r.run_id)
        .await
        .unwrap()
        .into_iter()
        .filter(|m| m.role == Role::Tool)
        .map(|m| m.content)
        .collect();
    assert_eq!(tools, ["call #1", "call #2"]);
}

#[tokio::test]
async fn cancel_between_tool_calls_leaves_a_valid_conversation() {
    let (s, _d) = store().await;
    let opts = RunOptions::default();
    let cancel = opts.cancel.clone();
    let first = Arc::new(FnTool::new(
        "first",
        "cancels the run",
        json!({"type":"object"}),
        false,
        move |_a: Value, _| {
            let cancel = cancel.clone();
            async move {
                cancel.cancel();
                Ok::<_, ToolError>("ok".into())
            }
        },
    ));
    let second = counter_tool("second", false, Arc::new(AtomicUsize::new(0)));
    let two_calls = FakeResponse {
        tool_calls: vec![
            ScriptedToolCall {
                name: "first".into(),
                arguments: json!({}),
            },
            ScriptedToolCall {
                name: "second".into(),
                arguments: json!({}),
            },
        ],
        ..Default::default()
    };
    let agent = Agent::builder(cfg(), s.clone())
        .provider(Arc::new(FakeProvider::new(vec![two_calls])))
        .tool(first)
        .tool(second)
        .build()
        .unwrap();
    let r1 = agent.run("go", opts).await.unwrap();
    assert_eq!(r1.status, RunStatus::Cancelled);

    // The next run in the same conversation sends a well-formed history.
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![say("fine")])));
    let agent2 = Agent::builder(cfg(), s.clone()).provider(rec.clone()).build().unwrap();
    let r2 = agent2
        .run(
            "again",
            RunOptions {
                session_id: Some(r1.session_id.clone()),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r2.status, RunStatus::Succeeded, "{}", r2.error);
    let msgs = &rec.cassette().interactions[0].request.messages;
    let assistant = msgs.iter().find(|m| !m.tool_calls.is_empty()).unwrap();
    for c in &assistant.tool_calls {
        assert!(
            msgs.iter().any(|m| m.tool_call_id.as_deref() == Some(c.id.as_str())),
            "unanswered call {}",
            c.id
        );
    }
}

#[tokio::test]
async fn cancelled_in_flight_side_effect_stays_unknown() {
    let (s, _d) = store().await;
    let opts = RunOptions::default();
    let cancel = opts.cancel.clone();
    let hang = Arc::new(FnTool::new(
        "pay",
        "hangs",
        json!({"type":"object"}),
        true,
        move |_a: Value, _| {
            let cancel = cancel.clone();
            async move {
                cancel.cancel();
                std::future::pending::<()>().await;
                Ok::<_, ToolError>("never".into())
            }
        },
    ));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(Arc::new(FakeProvider::new(vec![call("pay", json!({"to": "x"}))])))
        .tool(hang)
        .build()
        .unwrap();
    let r = agent.run("pay", opts).await.unwrap();
    assert_eq!(r.status, RunStatus::Cancelled);
    let effects = s.dump_text("effects").await.unwrap();
    assert!(
        effects.contains("started"),
        "cancelled effect must stay unconfirmed: {effects}"
    );
    assert!(!effects.contains("completed"));
}

#[tokio::test]
async fn second_concurrent_run_in_a_conversation_is_refused() {
    let (s, _d) = store().await;
    let gate = Arc::new(Notify::new());
    let g = gate.clone();
    let wait = Arc::new(FnTool::new(
        "wait",
        "waits",
        json!({"type":"object"}),
        false,
        move |_a: Value, _| {
            let g = g.clone();
            async move {
                g.notified().await;
                Ok::<_, ToolError>("ok".into())
            }
        },
    ));
    let agent = Arc::new(
        Agent::builder(cfg(), s.clone())
            .provider(Arc::new(FakeProvider::new(vec![
                call("wait", json!({})),
                say("first done"),
            ])))
            .tool(wait)
            .build()
            .unwrap(),
    );
    let session = s.create_session("rb", "default", "rb").await.unwrap();
    let a = agent.clone();
    let sid = session.id.clone();
    let first = tokio::spawn(async move {
        a.run(
            "one",
            RunOptions {
                session_id: Some(sid),
                ..Default::default()
            },
        )
        .await
    });
    while s.unfinished_runs("default", "rb").await.unwrap().is_empty() {
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    let err = agent
        .run(
            "two",
            RunOptions {
                session_id: Some(session.id.clone()),
                ..Default::default()
            },
        )
        .await
        .unwrap_err();
    assert!(matches!(err, AgentError::Store(StoreError::Conflict(_))), "{err}");
    gate.notify_one();
    assert_eq!(first.await.unwrap().unwrap().status, RunStatus::Succeeded);
}

#[tokio::test]
async fn resume_by_new_owner_fences_the_old_process() {
    let (s, _d) = store().await;
    let gate = Arc::new(Notify::new());
    let started = Arc::new(AtomicUsize::new(0));
    let (g, st) = (gate.clone(), started.clone());
    let slow = Arc::new(FnTool::new(
        "lookup",
        "slow read",
        json!({"type":"object"}),
        false,
        move |_a: Value, _| {
            let (g, st) = (g.clone(), st.clone());
            async move {
                st.fetch_add(1, Ordering::SeqCst);
                g.notified().await;
                Ok::<_, ToolError>("old".into())
            }
        },
    ));
    let old = Arc::new(
        Agent::builder(cfg(), s.clone())
            .provider(Arc::new(FakeProvider::new(vec![
                call("lookup", json!({})),
                say("old done"),
            ])))
            .tool(slow)
            .build()
            .unwrap(),
    );
    let o = old.clone();
    let old_task = tokio::spawn(async move { o.run("look", RunOptions::default()).await });
    while started.load(Ordering::SeqCst) == 0 {
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    let run_id = s.unfinished_runs("default", "rb").await.unwrap()[0].id.clone();

    // A new process takes over (e.g. the Nest was presumed dead).
    let fast = Arc::new(FnTool::new(
        "lookup",
        "fast read",
        json!({"type":"object"}),
        false,
        |_a: Value, _| async { Ok::<_, ToolError>("new".into()) },
    ));
    let new = Agent::builder(cfg(), s.clone())
        .provider(Arc::new(FakeProvider::new(vec![say("new done")])))
        .tool(fast)
        .build()
        .unwrap();
    let r = new.resume(&run_id, RunOptions::default()).await.unwrap();
    assert_eq!(
        (r.status, r.output.as_str()),
        (RunStatus::Succeeded, "new done"),
        "{}",
        r.error
    );

    // The old process wakes up and must not write anything.
    gate.notify_one();
    let old_result = old_task.await.unwrap();
    assert!(
        matches!(old_result, Err(AgentError::Store(StoreError::Fenced(_)))),
        "{old_result:?}"
    );
    let tool_msgs: Vec<String> = s
        .run_messages(&run_id)
        .await
        .unwrap()
        .into_iter()
        .filter(|m| m.role == Role::Tool)
        .map(|m| m.content)
        .collect();
    assert_eq!(tool_msgs, ["new"]);
}

struct Never;
#[async_trait]
impl Approver for Never {
    async fn decide(&self, _req: ApprovalRequest) -> ApprovalDecision {
        std::future::pending().await
    }
}

#[tokio::test]
async fn approvals_time_out() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.permissions = Permissions {
        approval_timeout: None,
        default: Action::Ask,
        rules: vec![],
    };
    c.approval_timeout = Duration::from_millis(50);
    let count = Arc::new(AtomicUsize::new(0));
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        call("pay", json!({})),
        say("gave up"),
    ])));
    let agent = Agent::builder(c, s)
        .provider(rec.clone())
        .tool(counter_tool("pay", true, count.clone()))
        .approver(Arc::new(Never))
        .build()
        .unwrap();
    let r = tokio::time::timeout(Duration::from_secs(5), agent.run("pay", RunOptions::default()))
        .await
        .expect("run must not hang")
        .unwrap();
    assert_eq!(r.status, RunStatus::Succeeded);
    assert_eq!(count.load(Ordering::SeqCst), 0);
    let last = rec.cassette().interactions[1]
        .request
        .messages
        .last()
        .unwrap()
        .content
        .clone();
    assert!(last.contains("not approved (expired)"), "{last}");
}

/// Streams "partial", fails retryably, then streams the real answer.
struct FlakyStream(AtomicUsize);
#[async_trait]
impl ModelProvider for FlakyStream {
    fn name(&self) -> &str {
        "flaky"
    }
    async fn complete(&self, _req: &ModelRequest, on_delta: DeltaSink<'_>) -> Result<ModelResponse, ProviderError> {
        if self.0.fetch_add(1, Ordering::SeqCst) == 0 {
            on_delta("partial ");
            return Err(ProviderError::Retryable("connection reset".into()));
        }
        on_delta("full answer");
        Ok(ModelResponse {
            text: "full answer".into(),
            tool_calls: vec![],
            usage: Usage::default(),
            finish_reason: "stop".into(),
        })
    }
}

#[tokio::test]
async fn retry_resets_streamed_text() {
    let (s, _d) = store().await;
    let agent = Agent::builder(cfg(), s)
        .provider(Arc::new(FlakyStream(AtomicUsize::new(0))))
        .build()
        .unwrap();
    let text = Arc::new(Mutex::new(String::new()));
    let resets = Arc::new(AtomicUsize::new(0));
    let (t, rs) = (text.clone(), resets.clone());
    let t2 = text.clone();
    let opts = RunOptions {
        on_delta: Some(Arc::new(move |d: &str| t.lock().unwrap().push_str(d))),
        on_reset: Some(Arc::new(move || {
            rs.fetch_add(1, Ordering::SeqCst);
            t2.lock().unwrap().clear();
        })),
        ..Default::default()
    };
    let r = agent.run("hi", opts).await.unwrap();
    assert_eq!(r.output, "full answer");
    assert_eq!(resets.load(Ordering::SeqCst), 1);
    assert_eq!(*text.lock().unwrap(), "full answer");
}

#[tokio::test]
async fn long_history_is_trimmed_to_fit_context() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.context_tokens = 60;
    let long = "x".repeat(120); // ~34 tokens per message
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        say(&long),
        say(&long),
        say("third"),
    ])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    let r1 = agent.run("one", RunOptions::default()).await.unwrap();
    let sid = Some(r1.session_id.clone());
    agent
        .run(
            "two",
            RunOptions {
                session_id: sid.clone(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    agent
        .run(
            "three",
            RunOptions {
                session_id: sid,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    let msgs = &rec.cassette().interactions[2].request.messages;
    assert_eq!(msgs.last().unwrap().content, "three");
    assert_eq!(msgs[0].role, Role::User, "history must start at a user turn");
    assert!(
        !msgs.iter().any(|m| m.content == "one"),
        "oldest turn should be dropped: {msgs:?}"
    );
}

#[tokio::test]
async fn secrets_in_side_effect_results_errors_and_approvals_are_redacted() {
    let (s, _d) = store().await;
    const SECRET: &str = "pw-ULTRA-secret-77";
    let mut m = BTreeMap::new();
    m.insert("DB_PASSWORD".to_string(), SECRET.to_string());
    let leaky_ok = Arc::new(FnTool::new(
        "connect",
        "side effect",
        json!({"type":"object"}),
        true,
        |_a: Value, ctx: ToolContext| async move {
            Ok::<_, ToolError>(format!("connected with {}", ctx.secrets.get("DB_PASSWORD").unwrap()))
        },
    ));
    let leaky_err = Arc::new(FnTool::new(
        "migrate",
        "side effect",
        json!({"type":"object"}),
        true,
        |_a: Value, ctx: ToolContext| async move {
            Err::<String, _>(ToolError::new(format!(
                "auth failed for {}",
                ctx.secrets.get("DB_PASSWORD").unwrap()
            )))
        },
    ));
    let seen = Arc::new(Mutex::new(vec![]));
    struct Record(Arc<Mutex<Vec<ApprovalRequest>>>);
    #[async_trait]
    impl Approver for Record {
        async fn decide(&self, req: ApprovalRequest) -> ApprovalDecision {
            self.0.lock().unwrap().push(req);
            ApprovalDecision::Approved
        }
    }
    let mut c = cfg();
    c.permissions = Permissions {
        approval_timeout: None,
        default: Action::Allow,
        rules: vec![PermissionRule {
            tool: "migrate".into(),
            action: Action::Ask,
        }],
    };
    let p = Arc::new(FakeProvider::new(vec![
        call("connect", json!({})),
        call("migrate", json!({"password": SECRET})), // model echoes a secret the user typed
        say("done"),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(leaky_ok)
        .tool(leaky_err)
        .secrets(Secrets::from_map(m))
        .approver(Arc::new(Record(seen.clone())))
        .build()
        .unwrap();
    let r = agent
        .run(&format!("my password is {SECRET}"), RunOptions::default())
        .await
        .unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    for table in [
        "sessions",
        "messages",
        "runs",
        "spans",
        "effects",
        "logs",
        "conversations",
    ] {
        assert!(
            !s.dump_text(table).await.unwrap().contains(SECRET),
            "secret leaked into {table}"
        );
    }
    {
        let approvals = seen.lock().unwrap();
        assert_eq!(approvals.len(), 1);
        assert!(!approvals[0].arguments.to_string().contains(SECRET));
    }
    assert!(s.dump_text("effects").await.unwrap().contains("[REDACTED:DB_PASSWORD]"));
}

#[tokio::test]
async fn too_short_secrets_are_rejected_at_build() {
    let (s, _d) = store().await;
    let mut m = BTreeMap::new();
    m.insert("PIN".to_string(), "12".to_string());
    let r = Agent::builder(cfg(), s)
        .provider(Arc::new(FakeProvider::new(vec![])))
        .secrets(Secrets::from_map(m))
        .build();
    assert!(matches!(r, Err(AgentError::Config(_))));
}

#[tokio::test]
async fn colliding_or_duplicate_tool_names_are_rejected() {
    let (s, _d) = store().await;
    let t = |n: &str| counter_tool(n, false, Arc::new(AtomicUsize::new(0)));
    let p = || Arc::new(FakeProvider::new(vec![]));
    let collide = Agent::builder(cfg(), s.clone())
        .provider(p())
        .tool(t("a.b_c"))
        .tool(t("a_b.c"))
        .build();
    assert!(matches!(collide, Err(AgentError::Config(m)) if m.contains("collide")));
    let dup = Agent::builder(cfg(), s).provider(p()).tool(t("x")).tool(t("x")).build();
    assert!(matches!(dup, Err(AgentError::Config(_))));
}

#[tokio::test]
async fn uncertain_tool_outcome_is_reported_and_not_confirmed() {
    let (s, _d) = store().await;
    let flaky = Arc::new(FnTool::new(
        "pay",
        "times out",
        json!({"type":"object"}),
        true,
        |_a: Value, _| async { Err::<String, _>(ToolError::unknown("timed out after 30s")) },
    ));
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        call("pay", json!({})),
        say("checking"),
    ])));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(rec.clone())
        .tool(flaky)
        .build()
        .unwrap();
    let r = agent.run("pay", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded);
    let last = rec.cassette().interactions[1]
        .request
        .messages
        .last()
        .unwrap()
        .content
        .clone();
    assert!(last.starts_with("error: effect_unknown: timed out"), "{last}");
    let effects = s.dump_text("effects").await.unwrap();
    assert!(
        effects.contains("started") && !effects.contains("completed"),
        "{effects}"
    );
}

#[tokio::test]
async fn task_runs_are_idempotent() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![say("first answer"), say("never used")]));
    let agent = Agent::builder(cfg(), s).provider(p.clone()).build().unwrap();
    let opts = || RunOptions {
        task_id: "task-1".into(),
        ..Default::default()
    };
    let a = agent.run("x", opts()).await.unwrap();
    let b = agent.run("x", opts()).await.unwrap();
    assert_eq!((a.run_id.clone(), a.output.clone()), (b.run_id, b.output));
    assert_eq!(p.calls(), 1, "a finished task must not run again");
}
