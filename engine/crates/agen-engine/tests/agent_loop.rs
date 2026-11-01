use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use agen_engine::agent::{Agent, AgentConfig, ApprovalDecision, ApprovalRequest, Approver, BundleOptions, RunOptions};
use agen_engine::bundle::{Action, Bundle, PermissionRule, Permissions, Skill};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::provider::replay::RecordingProvider;
use agen_engine::provider::{Message, ModelProvider, Role, ToolCall};
use agen_engine::secrets::Secrets;
use agen_engine::store::{now_ms, RunRecord, RunStatus, Store};
use agen_engine::tools::{FnTool, Tool, ToolError};
use agen_engine::trace::TraceContext;
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

fn say(text: &str, expect: Option<&str>) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        expect: expect.map(Into::into),
        ..Default::default()
    }
}

fn add_tool() -> Arc<dyn Tool> {
    Arc::new(FnTool::new(
        "add",
        "Add two numbers",
        json!({"type":"object"}),
        false,
        |a: Value, _| async move { Ok::<_, ToolError>((a["a"].as_i64().unwrap() + a["b"].as_i64().unwrap()).to_string()) },
    ))
}

fn cfg() -> AgentConfig {
    AgentConfig::new("calc", "You add numbers.", "fake-1")
}

#[tokio::test]
async fn tool_loop_produces_answer_messages_and_spans() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        call("add", json!({"a": 2, "b": 3})),
        say("The answer is 5", Some("5")),
    ]));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(p.clone())
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("what is 2+3?", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(r.output, "The answer is 5");
    assert!(r.usage.input_tokens > 0);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let roles: Vec<Role> = msgs.iter().map(|m| m.role).collect();
    assert_eq!(roles, [Role::User, Role::Assistant, Role::Tool, Role::Assistant]);
    assert_eq!(msgs[2].content, "5");
    let spans = s.trace(&r.trace_id).await.unwrap();
    let names: Vec<&str> = spans.iter().map(|s| s.name.as_str()).collect();
    for n in ["agen.run", "gen_ai.chat", "agen.tool"] {
        assert!(names.contains(&n), "{names:?}");
    }
    assert_eq!(names[0], "agen.run", "the root span sorts first: {names:?}");
    let run_span = spans.iter().find(|s| s.name == "agen.run").unwrap();
    assert!(spans
        .iter()
        .filter(|s| s.name != "agen.run")
        .all(|s| s.parent_span_id == run_span.span_id));
    let rec = s.get_run(&r.run_id).await.unwrap();
    assert_eq!((rec.step, rec.status), (2, RunStatus::Succeeded));
}

#[tokio::test]
async fn skills_load_progressively() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.skills = vec![Skill {
        name: "math".into(),
        description: "how to add".into(),
        body: "Always show your work.".into(),
    }];
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        call("load_skill", json!({"name": "math"})),
        say("ok", Some("Always show your work.")),
    ])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let first = &rec.cassette().interactions[0].request;
    assert!(first.system.contains("- math: how to add"));
    assert!(
        !first.system.contains("Always show your work."),
        "skill body must load on demand"
    );
}

struct Fixed(ApprovalDecision, Arc<Mutex<Vec<ApprovalRequest>>>);
#[async_trait]
impl Approver for Fixed {
    async fn decide(&self, req: ApprovalRequest) -> ApprovalDecision {
        self.1.lock().unwrap().push(req);
        self.0
    }
}

#[tokio::test]
async fn permissions_deny_ask_allow() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.permissions = Permissions {
        approval_timeout: None,
        default: Action::Allow,
        rules: vec![PermissionRule {
            tool: "add".into(),
            action: Action::Deny,
            when: None,
        }],
    };
    let p = Arc::new(FakeProvider::new(vec![
        call("add", json!({"a": 1, "b": 1})),
        say("blocked", Some("permission denied")),
    ]));
    let agent = Agent::builder(c.clone(), s.clone())
        .provider(p)
        .tool(add_tool())
        .build()
        .unwrap();
    assert_eq!(
        agent.run("x", RunOptions::default()).await.unwrap().status,
        RunStatus::Succeeded
    );

    for (decision, expect) in [
        (ApprovalDecision::Approved, "2"),
        (ApprovalDecision::Denied, "not approved (denied)"),
    ] {
        c.permissions.rules[0].action = Action::Ask;
        let seen = Arc::new(Mutex::new(vec![]));
        let p = Arc::new(FakeProvider::new(vec![
            call("add", json!({"a": 1, "b": 1})),
            say("done", Some(expect)),
        ]));
        let agent = Agent::builder(c.clone(), s.clone())
            .provider(p)
            .tool(add_tool())
            .approver(Arc::new(Fixed(decision, seen.clone())))
            .build()
            .unwrap();
        let r = agent.run("x", RunOptions::default()).await.unwrap();
        assert_eq!(r.status, RunStatus::Succeeded, "{decision:?}: {}", r.error);
        assert_eq!(seen.lock().unwrap()[0].tool, "add");
    }
}

/// A side-effecting tool that counts executions and optionally hangs forever
/// (to simulate the process dying mid-call).
fn pay_tool(count: Arc<AtomicUsize>, hang: bool, idempotent: bool) -> Arc<dyn Tool> {
    struct Pay {
        count: Arc<AtomicUsize>,
        hang: bool,
        idempotent: bool,
    }
    #[async_trait]
    impl Tool for Pay {
        fn spec(&self) -> agen_engine::provider::ToolSpec {
            agen_engine::provider::ToolSpec {
                name: "pay".into(),
                description: "Send money".into(),
                parameters: json!({"type":"object"}),
            }
        }
        fn idempotency_key(&self, args: &Value) -> Option<String> {
            self.idempotent
                .then(|| args["ref"].as_str().unwrap_or_default().to_string())
        }
        async fn call(&self, _args: Value, _ctx: agen_engine::tools::ToolContext) -> Result<String, ToolError> {
            self.count.fetch_add(1, Ordering::SeqCst);
            if self.hang {
                std::future::pending::<()>().await;
            }
            Ok("paid".into())
        }
    }
    Arc::new(Pay {
        count,
        hang,
        idempotent,
    })
}

async fn crash_mid_tool(s: &Arc<Store>, idempotent: bool) -> (String, Arc<AtomicUsize>) {
    let count = Arc::new(AtomicUsize::new(0));
    let p = Arc::new(FakeProvider::new(vec![call(
        "pay",
        json!({"to": "bob", "ref": "order-1"}),
    )]));
    let agent = Arc::new(
        Agent::builder(cfg(), s.clone())
            .provider(p)
            .tool(pay_tool(count.clone(), true, idempotent))
            .build()
            .unwrap(),
    );
    let a = agent.clone();
    let task = tokio::spawn(async move { a.run("pay bob", RunOptions::default()).await });
    while count.load(Ordering::SeqCst) == 0 {
        tokio::time::sleep(std::time::Duration::from_millis(5)).await;
    }
    task.abort(); // the "process" dies while the payment is in flight
    let _ = task.await;
    let runs = s.unfinished_runs("default", "calc").await.unwrap();
    assert_eq!(runs.len(), 1);
    (runs[0].id.clone(), count)
}

#[tokio::test]
async fn crash_mid_side_effect_is_not_repeated_on_resume() {
    let (s, _d) = store().await;
    let (run_id, count) = crash_mid_tool(&s, false).await;
    let p = Arc::new(FakeProvider::new(vec![say(
        "could not confirm payment",
        Some("effect_unknown"),
    )]));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(p)
        .tool(pay_tool(count.clone(), false, false))
        .build()
        .unwrap();
    let r = agent.resume(&run_id, RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(count.load(Ordering::SeqCst), 1, "payment must not be sent twice");
    assert_eq!(r.run_id, run_id);
}

#[tokio::test]
async fn crash_mid_idempotent_call_is_retried_once() {
    let (s, _d) = store().await;
    let (run_id, count) = crash_mid_tool(&s, true).await;
    let p = Arc::new(FakeProvider::new(vec![say("paid", Some("paid"))]));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(p)
        .tool(pay_tool(count.clone(), false, true))
        .build()
        .unwrap();
    let r = agent.resume(&run_id, RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(count.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn resume_reuses_completed_effect_without_calling_tool() {
    let (s, _d) = store().await;
    // Checkpoint: the model asked to pay, the payment completed, then the
    // process died before the tool result message was written.
    let sess = s.create_session("calc", "default", "calc").await.unwrap();
    let conv = s.current_conversation(&sess.id).await.unwrap();
    let run = RunRecord {
        id: agen_engine::store::new_id(),
        session_id: sess.id.clone(),
        conversation_id: conv.clone(),
        namespace: "default".into(),
        deployment: "calc".into(),
        definition_digest: String::new(),
        task_id: String::new(),
        parent_run_id: String::new(),
        root_run_id: String::new(),
        status: RunStatus::Running,
        input: "pay".into(),
        output: String::new(),
        error: String::new(),
        step: 1,
        usage: Default::default(),
        trace_id: agen_engine::trace::new_trace_id(),
        started_ms: now_ms(),
        ended_ms: None,
        requested_by: String::new(),
        labels: Default::default(),
        outcome: None,
    };
    let epoch = s.create_run(&run, "crashed-owner").await.unwrap();
    s.append_message(&conv, &run.id, &Message::user("pay")).await.unwrap();
    let c = ToolCall {
        id: "call_0_0".into(),
        name: "pay".into(),
        arguments: json!({"to": "bob"}),
    };
    s.checkpoint(
        &run.id,
        epoch,
        &conv,
        Some(&Message::assistant("", vec![c.clone()])),
        Some((1, RunStatus::Running, Default::default())),
    )
    .await
    .unwrap();
    let hash = agen_engine::agent::args_hash(&c.arguments);
    let eid = agen_engine::agent::effect_id(1, "call_0_0");
    s.begin_effect(&run.id, epoch, &eid, "pay", &hash, "").await.unwrap();
    s.complete_effect(&run.id, epoch, &eid, "receipt-42").await.unwrap();

    let count = Arc::new(AtomicUsize::new(0));
    let p = Arc::new(FakeProvider::new(vec![say("done", Some("receipt-42"))]));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(p)
        .tool(pay_tool(count.clone(), false, false))
        .build()
        .unwrap();
    let r = agent.resume(&run.id, RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(count.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn secrets_never_reach_store_traces_model_or_stream() {
    let (s, _d) = store().await;
    const SECRET: &str = "sk-live-TOPSECRET-9f8e7d";
    let mut m = std::collections::BTreeMap::new();
    m.insert("API_KEY".to_string(), SECRET.to_string());
    let leak = Arc::new(FnTool::new(
        "whoami",
        "Returns creds",
        json!({"type":"object"}),
        false,
        |_a: Value, ctx: agen_engine::tools::ToolContext| async move {
            Ok::<_, ToolError>(format!("token={}", ctx.secrets.get("API_KEY").unwrap()))
        },
    ));
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        call("whoami", json!({})),
        say(&format!("your key is {SECRET}"), Some("[REDACTED:API_KEY]")),
    ])));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(rec.clone())
        .tool(leak)
        .secrets(Secrets::from_map(m))
        .build()
        .unwrap();
    let streamed = Arc::new(Mutex::new(String::new()));
    let sc = streamed.clone();
    let opts = RunOptions {
        on_delta: Some(Arc::new(move |d: &str| sc.lock().unwrap().push_str(d))),
        ..Default::default()
    };
    let r = agent.run(&format!("use {SECRET}"), opts).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert!(!r.output.contains(SECRET));
    for table in [
        "sessions",
        "messages",
        "runs",
        "spans",
        "effects",
        "logs",
        "conversations",
    ] {
        let dump = s.dump_text(table).await.unwrap();
        assert!(!dump.contains(SECRET), "secret leaked into {table}");
    }
    let sent = serde_json::to_string(&rec.cassette()).unwrap();
    // The model's own (fake) reply contained the secret, but nothing the engine
    // sent to the model did.
    for it in rec.cassette().interactions {
        assert!(
            !serde_json::to_string(&it.request).unwrap().contains(SECRET),
            "secret sent to model"
        );
    }
    assert!(sent.contains("[REDACTED:API_KEY]"));
    assert!(!streamed.lock().unwrap().contains(SECRET), "secret streamed");
}

#[tokio::test]
async fn budget_status_line_appears_past_half_the_limit() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.budget.max_usd_per_run = Some(0.20);
    let costly = |resp: FakeResponse| FakeResponse {
        usage: Some(agen_engine::provider::Usage {
            input_tokens: 10,
            output_tokens: 5,
            cached_input_tokens: 0,
            cost_usd: 0.12,
        }),
        ..resp
    };
    let p = Arc::new(FakeProvider::new(vec![
        costly(call("add", json!({"a": 1, "b": 2}))),
        say("done", None),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p.clone())
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let reqs = p.requests();
    assert_eq!(reqs.len(), 2);
    assert!(
        !reqs[0].system.contains("[agen]"),
        "nothing spent yet on the first turn: {}",
        reqs[0].system
    );
    assert!(
        reqs[1].system.contains("[agen] turn 2 of 16") && reqs[1].system.contains("$0.12 of $0.20"),
        "{}",
        reqs[1].system
    );
}

// A cached input token counts at budget.cachedInputWeight (default 0.1,
// 2.21), not its full cost: a turn whose tokens would exceed
// max_tokens_per_run if counted in full stays under it once most of them
// are cache hits.
#[tokio::test]
async fn cached_input_tokens_count_at_a_discount_against_the_token_budget() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.budget.max_tokens_per_run = Some(100);
    let cached = FakeResponse {
        usage: Some(agen_engine::provider::Usage {
            input_tokens: 500,
            cached_input_tokens: 480,
            output_tokens: 0,
            cost_usd: 0.0,
        }),
        ..call("add", json!({"a": 1, "b": 2}))
    };
    let p = Arc::new(FakeProvider::new(vec![cached, say("done", None)]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    // 20 fresh + 480*0.1 = 68 counted tokens: under the 100 limit, so the
    // run finishes normally rather than stopping as "partial"/"budget".
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(r.outcome.unwrap().status, "done");
}

// The system prompt mentions a run's workspace directory once it has one
// (2.45); with no `workspace` configured, it says nothing extra.
#[tokio::test]
async fn system_prompt_mentions_the_workspace_directory_when_configured() {
    let (s, _d) = store().await;
    let base = tempfile::tempdir().unwrap();
    let mut c = cfg();
    c.workspace = Some(agen_engine::bundle::WorkspaceConfig {
        mode: agen_engine::bundle::WorkspaceMode::PerTask,
        retain: true,
    });
    std::env::set_var("AGEN_WORKSPACE_ROOT", base.path());
    let p = Arc::new(FakeProvider::new(vec![say("ok", None)]));
    let agent = Agent::builder(c, s.clone()).provider(p.clone()).build().unwrap();
    let r = agent
        .run(
            "x",
            RunOptions {
                task_id: "t-workspace".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let req = &p.requests()[0];
    assert!(
        req.system.contains("## Workspace") && req.system.contains("t-workspace"),
        "{}",
        req.system
    );
}

#[tokio::test]
async fn budget_and_max_turns_stop_runs() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.budget.max_tokens_per_run = Some(1);
    let p = Arc::new(FakeProvider::new(vec![
        call("add", json!({"a": 1, "b": 2})),
        say("I added 1 and 2; the rest is undone.", None),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    // Stopped at the limit, the run still reports what it has.
    assert_eq!(r.status, RunStatus::Succeeded);
    let o = r.outcome.clone().unwrap();
    assert_eq!((o.status.as_str(), o.reason.as_str()), ("partial", "budget"));
    assert_eq!(o.summary, "I added 1 and 2; the rest is undone.");
    assert_eq!(r.output, o.summary);

    // $ per run, from the cost the provider reports.
    let mut c = cfg();
    c.budget.max_usd_per_run = Some(0.05);
    let costly = |resp: FakeResponse| FakeResponse {
        usage: Some(agen_engine::provider::Usage {
            input_tokens: 10,
            output_tokens: 5,
            cached_input_tokens: 0,
            cost_usd: 0.04,
        }),
        ..resp
    };
    let p = Arc::new(FakeProvider::new(vec![
        costly(call("add", json!({"a": 1, "b": 2}))),
        costly(call("add", json!({"a": 3, "b": 4}))),
        say("report", None),
    ]));
    let calls = p.clone();
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    let o = r.outcome.clone().unwrap();
    assert_eq!(
        (o.status.as_str(), o.reason.as_str(), o.summary.as_str()),
        ("partial", "budget", "report")
    );
    assert_eq!(calls.calls(), 3, "only the report call after the budget is exceeded");
    let reqs = calls.requests();
    let last = reqs.last().unwrap();
    assert_eq!(
        last.extra.get("tool_choice"),
        Some(&json!("none")),
        "the report call runs no tools"
    );
    assert_eq!(last.max_output_tokens, Some(1024));
    assert!(last.messages.last().unwrap().content.contains("budget_exceeded"));

    // With reportTokens 0 a limit fails the run, as before.
    let mut c = cfg();
    c.budget.max_usd_per_run = Some(0.05);
    c.report_tokens = 0;
    let p = Arc::new(FakeProvider::new(vec![
        costly(call("add", json!({"a": 1, "b": 2}))),
        costly(call("add", json!({"a": 3, "b": 4}))),
        say("never", None),
    ]));
    let calls = p.clone();
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert!(
        r.error.contains("budget_exceeded") && r.error.contains("$0.08"),
        "{}",
        r.error
    );
    assert_eq!(calls.calls(), 2, "no model call after the budget is exceeded");
    let o = r.outcome.unwrap();
    assert_eq!((o.status.as_str(), o.reason.as_str()), ("failed", "budget"));

    // A report call that answers with tool calls ends partial with no text.
    let mut c = cfg();
    c.max_turns = 2;
    let p = Arc::new(FakeProvider::new(vec![call("add", json!({"a": 1, "b": 2}))]).cycling());
    let agent = Agent::builder(c, s).provider(p).tool(add_tool()).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    let o = r.outcome.unwrap();
    assert_eq!(
        (o.status.as_str(), o.reason.as_str(), o.summary.as_str()),
        ("partial", "max_turns", "")
    );
}

#[tokio::test]
async fn finish_ends_a_run_with_an_outcome() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        call("add", json!({"a": 1, "b": 2})),
        call(
            "finish",
            json!({"status": "needs_input", "summary": "Which account?", "inputFrom": "human"}),
        ),
        say("never", None),
    ]));
    let calls = p.clone();
    let agent = Agent::builder(cfg(), s.clone())
        .provider(p)
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded);
    assert_eq!(calls.calls(), 2, "finish ends the run without another model call");
    let o = r.outcome.clone().unwrap();
    assert_eq!(
        (o.status.as_str(), o.summary.as_str(), o.input_from.as_str()),
        ("needs_input", "Which account?", "human")
    );
    assert!(calls.requests()[0].tools.iter().any(|t| t.name == "finish"));
    // Stored on the run.
    let stored = s.get_run(&r.run_id).await.unwrap();
    assert_eq!(stored.outcome, Some(o));

    // finish(failed) fails the run with its summary as the error.
    let p = Arc::new(FakeProvider::new(vec![call(
        "finish",
        json!({"status": "failed", "summary": "no access to the repo"}),
    )]));
    let agent = Agent::builder(cfg(), s.clone()).provider(p).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert_eq!(r.error, "no access to the repo");

    // Invalid arguments are refused and the model tries again.
    let p = Arc::new(FakeProvider::new(vec![
        call("finish", json!({"status": "great", "summary": "x"})),
        call("finish", json!({"status": "done", "summary": "fixed"})),
    ]));
    let agent = Agent::builder(cfg(), s.clone()).provider(p).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.outcome.unwrap().summary, "fixed");

    // A plain answer is a done outcome.
    let p = Arc::new(FakeProvider::new(vec![say("hello", None)]));
    let agent = Agent::builder(cfg(), s.clone()).provider(p).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    let o = r.outcome.unwrap();
    assert_eq!(
        (o.status.as_str(), o.summary.as_str(), o.reason.as_str()),
        ("done", "hello", "")
    );

    // finish never waits for an approval, even when everything else asks.
    let mut c = cfg();
    c.permissions.default = agen_engine::bundle::Action::Ask;
    let p = Arc::new(FakeProvider::new(vec![call(
        "finish",
        json!({"status": "done", "summary": "ok"}),
    )]));
    let agent = Agent::builder(c, s).provider(p).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.outcome.unwrap().status, "done");
}

#[tokio::test]
async fn result_schema_is_enforced() {
    let (s, _d) = store().await;
    let schema = agen_engine::outcome::ResultSchema::new(json!({
        "type": "object",
        "properties": {"count": {"type": "integer"}},
        "required": ["count"]
    }))
    .unwrap();
    let mut c = cfg();
    c.result_schema = Some(schema);
    // A plain answer is sent back once, then bad data is refused, then the
    // result is accepted.
    let p = Arc::new(FakeProvider::new(vec![
        say("there are 3", None),
        call(
            "finish",
            json!({"status": "done", "summary": "3", "data": {"count": "three"}}),
        ),
        call(
            "finish",
            json!({"status": "done", "summary": "3", "data": {"count": 3}}),
        ),
    ]));
    let calls = p.clone();
    let agent = Agent::builder(c.clone(), s.clone()).provider(p).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    let o = r.outcome.unwrap();
    assert_eq!(o.data, Some(json!({"count": 3})));
    assert_eq!(calls.calls(), 3);
    let finish = calls.requests()[0]
        .tools
        .iter()
        .find(|t| t.name == "finish")
        .cloned()
        .unwrap();
    assert_eq!(finish.parameters["properties"]["data"]["required"], json!(["count"]));

    // Two plain answers: the run fails.
    let p = Arc::new(FakeProvider::new(vec![say("3", None), say("I said 3", None)]));
    let agent = Agent::builder(c, s).provider(p).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert_eq!(r.outcome.unwrap().reason, "error");
}

#[tokio::test]
async fn cancellation_stops_a_run() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![FakeResponse {
        text: "slow".into(),
        delay_ms: 10_000,
        ..Default::default()
    }]));
    let agent = Agent::builder(cfg(), s).provider(p).build().unwrap();
    let opts = RunOptions::default();
    let cancel = opts.cancel.clone();
    tokio::spawn(async move {
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        cancel.cancel();
    });
    let started = std::time::Instant::now();
    let r = agent.run("x", opts).await.unwrap();
    assert_eq!(r.status, RunStatus::Cancelled);
    assert!(started.elapsed().as_secs() < 5);
}

#[tokio::test]
async fn continues_incoming_trace_and_keeps_conversation_history() {
    let (s, _d) = store().await;
    let parent = TraceContext::new_root().child();
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        say("one", None),
        say("two", None),
    ])));
    let agent = Agent::builder(cfg(), s.clone()).provider(rec.clone()).build().unwrap();
    let r1 = agent
        .run(
            "first",
            RunOptions {
                traceparent: Some(parent.traceparent()),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r1.trace_id, parent.trace_id);
    let spans = s.trace(&parent.trace_id).await.unwrap();
    assert_eq!(
        spans.iter().find(|s| s.name == "agen.run").unwrap().parent_span_id,
        parent.span_id
    );

    let r2 = agent
        .run(
            "second",
            RunOptions {
                session_id: Some(r1.session_id.clone()),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r2.conversation_id, r1.conversation_id);
    let second_req = &rec.cassette().interactions[1].request;
    let contents: Vec<&str> = second_req.messages.iter().map(|m| m.content.as_str()).collect();
    assert_eq!(contents, ["first", "one", "second"]);
}

#[tokio::test]
async fn runs_hello_bundle_end_to_end() {
    let (s, _d) = store().await;
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../examples/bundles/hello");
    let bundle = Bundle::load(&root).unwrap();
    let agent = Agent::from_bundle(&bundle, s.clone(), BundleOptions::default())
        .await
        .unwrap();
    assert!(agent.tools().get("load_skill").is_some());
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(r.output, "Hello! Nice to meet you.");
    assert_eq!(s.get_run(&r.run_id).await.unwrap().definition_digest, bundle.digest);
}

#[tokio::test]
async fn invalid_tool_arguments_are_never_executed() {
    let (s, _d) = store().await;
    let count = Arc::new(AtomicUsize::new(0));
    let p = Arc::new(FakeProvider::new(vec![
        call("pay", json!({"_invalid_json": "{\"to\": \"bo"})),
        say("stopped", Some("not valid JSON")),
    ]));
    let agent = Agent::builder(cfg(), s)
        .provider(p)
        .tool(pay_tool(count.clone(), false, false))
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(count.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn provider_retry_then_success() {
    let (s, _d) = store().await;
    let p: Arc<dyn ModelProvider> = Arc::new(FakeProvider::new(vec![
        FakeResponse {
            error: Some("429".into()),
            ..Default::default()
        },
        say("recovered", None),
    ]));
    let agent = Agent::builder(cfg(), s).provider(p).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!((r.status, r.output.as_str()), (RunStatus::Succeeded, "recovered"));
}

#[tokio::test]
async fn failed_tool_span_says_why() {
    let (s, _d) = store().await;
    let broken: Arc<dyn Tool> = Arc::new(FnTool::new(
        "fetch",
        "Fetch a page",
        json!({"type":"object"}),
        false,
        |_a: Value, _| async move { Err::<String, _>(ToolError::new("upstream answered 502")) },
    ));
    let p = Arc::new(FakeProvider::new(vec![
        call("fetch", json!({})),
        say("could not fetch", Some("502")),
    ]));
    let agent = Agent::builder(cfg(), s.clone())
        .provider(p)
        .tool(broken)
        .build()
        .unwrap();
    let r = agent.run("get it", RunOptions::default()).await.unwrap();
    let spans = s.trace(&r.trace_id).await.unwrap();
    let tool = spans.iter().find(|s| s.name == "agen.tool").unwrap();
    assert_eq!(tool.status, "error");
    let msg = tool.attributes["error.message"].as_str().unwrap_or_default();
    assert!(msg.contains("upstream answered 502"), "{:?}", tool.attributes);
}

#[tokio::test]
async fn preloaded_skills_are_in_the_system_prompt() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.skills = vec![
        Skill {
            name: "style".into(),
            description: "house style".into(),
            body: "Answer in one line.".into(),
        },
        Skill {
            name: "math".into(),
            description: "how to add".into(),
            body: "Always show your work.".into(),
        },
    ];
    c.preload_skills = vec!["style".into()];
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![say("ok", None)])));
    let agent = Agent::builder(c, s).provider(rec.clone()).build().unwrap();
    agent.run("hi", RunOptions::default()).await.unwrap();
    let system = &rec.cassette().interactions[0].request.system;
    assert!(system.contains("## Skill: style\nAnswer in one line."), "{system}");
    assert!(
        !system.contains("- style:"),
        "a preloaded skill is not offered for loading"
    );
    assert!(system.contains("- math: how to add") && !system.contains("Always show your work."));
}
