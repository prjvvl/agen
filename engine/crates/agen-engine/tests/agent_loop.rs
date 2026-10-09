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
async fn budget_and_max_turns_stop_runs() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.budget.max_tokens_per_run = Some(1);
    let p = Arc::new(FakeProvider::new(vec![
        call("add", json!({"a": 1, "b": 2})),
        say("never", None),
    ]));
    let agent = Agent::builder(c, s.clone())
        .provider(p)
        .tool(add_tool())
        .build()
        .unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert!(r.error.contains("budget_exceeded"), "{}", r.error);

    // $ per run, from the cost the provider reports.
    let mut c = cfg();
    c.budget.max_usd_per_run = Some(0.05);
    let costly = |resp: FakeResponse| FakeResponse {
        usage: Some(agen_engine::provider::Usage {
            input_tokens: 10,
            output_tokens: 5,
            cost_usd: 0.04,
        }),
        ..resp
    };
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

    let mut c = cfg();
    c.max_turns = 2;
    let p = Arc::new(FakeProvider::new(vec![call("add", json!({"a": 1, "b": 2}))]).cycling());
    let agent = Agent::builder(c, s).provider(p).tool(add_tool()).build().unwrap();
    let r = agent.run("x", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert!(r.error.contains("max_turns"), "{}", r.error);
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
