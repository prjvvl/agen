//! Guards inside a run: identical errors, a run repeating itself, answers
//! cut off at the output limit, denied tools, and what compaction keeps.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::{Action, PermissionRule};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::provider::Role;
use agen_engine::store::Store;
use agen_engine::tools::{FnTool, Tool, ToolError};
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

fn counting_tool(name: &str, n: Arc<AtomicUsize>, result: Result<&'static str, &'static str>) -> Arc<dyn Tool> {
    Arc::new(FnTool::new(
        name,
        "test tool",
        json!({"type": "object"}),
        false,
        move |_: Value, _| {
            let n = n.clone();
            async move {
                n.fetch_add(1, Ordering::SeqCst);
                result.map(String::from).map_err(ToolError::new)
            }
        },
    ))
}

// 2.14: the same call failing the same way is stopped after three tries.
#[tokio::test]
async fn identical_errors_are_not_repeated() {
    let (s, _d) = store().await;
    let n = Arc::new(AtomicUsize::new(0));
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.max_turns = 5;
    cfg.limits.stuck_repeats = Some(0);
    let p = Arc::new(FakeProvider::new(vec![call("flaky", json!({"id": 1}))]).cycling());
    let agent = Agent::builder(cfg, s.clone())
        .provider(p)
        .tool(counting_tool("flaky", n.clone(), Err("boom: upstream 500")))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(
        n.load(Ordering::SeqCst),
        3,
        "the tool ran three times, then was refused"
    );
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    assert!(
        msgs.iter()
            .any(|m| m.content.contains("failed 3 times in a row with the same error")),
        "{msgs:?}"
    );
}

// 2.18: a run making the same call with the same result is nudged once,
// then stops and reports.
#[tokio::test]
async fn a_repeating_run_is_nudged_then_stopped() {
    let (s, _d) = store().await;
    let n = Arc::new(AtomicUsize::new(0));
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.max_turns = 30;
    cfg.limits.max_identical_tool_calls = Some(0);
    let p = Arc::new(FakeProvider::new(vec![call("status", json!({}))]).cycling());
    let agent = Agent::builder(cfg, s.clone())
        .provider(p)
        .tool(counting_tool("status", n.clone(), Ok("still pending")))
        .build()
        .unwrap();
    let r = agent.run("wait for it", RunOptions::default()).await.unwrap();
    let o = r.outcome.unwrap();
    assert_eq!((o.status.as_str(), o.reason.as_str()), ("partial", "stuck"));
    // Four repeats, the nudge, four more.
    assert_eq!(n.load(Ordering::SeqCst), 8);
    let msgs = s.run_messages(&r.run_id).await.unwrap();
    let nudges = msgs
        .iter()
        .filter(|m| m.role == Role::User && m.content.contains("same call with the same result"))
        .count();
    assert_eq!(nudges, 1);

    // Results that change are not a repeat.
    let (s, _d) = store().await;
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.max_turns = 6;
    cfg.limits.max_identical_tool_calls = Some(0);
    let k = Arc::new(AtomicUsize::new(0));
    let k2 = k.clone();
    let tool: Arc<dyn Tool> = Arc::new(FnTool::new(
        "tick",
        "t",
        json!({"type": "object"}),
        false,
        move |_: Value, _| {
            let k = k2.clone();
            async move { Ok::<_, ToolError>(format!("tick {}", k.fetch_add(1, Ordering::SeqCst))) }
        },
    ));
    let p = Arc::new(FakeProvider::new(vec![call("tick", json!({}))]).cycling());
    let agent = Agent::builder(cfg, s).provider(p).tool(tool).build().unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.outcome.unwrap().reason, "max_turns");
}

// 2.47: an answer cut off at the output limit with nothing usable is asked
// again once with twice the room.
#[tokio::test]
async fn answers_cut_at_the_output_limit_are_retried_with_more_room() {
    let (s, _d) = store().await;
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.max_output_tokens = Some(500);
    let p = Arc::new(FakeProvider::new(vec![
        FakeResponse {
            finish_reason: Some("length".into()),
            expect_max_output: Some(500),
            ..Default::default()
        },
        FakeResponse {
            text: "Here it is.".into(),
            expect_max_output: Some(1000),
            ..Default::default()
        },
    ]));
    let agent = Agent::builder(cfg, s.clone()).provider(p).build().unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.output, "Here it is.", "{}", r.error);
    let spans = s.trace(&r.trace_id).await.unwrap();
    let chat = spans.iter().find(|s| s.name == "gen_ai.chat").unwrap();
    assert_eq!(chat.attributes["agen.output.retried_with"], json!(1000));
}

// 2.22: tools the permissions deny are not offered to the model, and a call
// that names one anyway says it is not available.
#[tokio::test]
async fn denied_tools_are_hidden() {
    let (s, _d) = store().await;
    let n = Arc::new(AtomicUsize::new(0));
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.permissions.rules = vec![PermissionRule {
        tool: "danger".into(),
        action: Action::Deny,
        when: None,
    }];
    let p = Arc::new(FakeProvider::new(vec![
        call("danger", json!({})),
        FakeResponse {
            text: "ok".into(),
            expect: Some("not available to this agent".into()),
            ..Default::default()
        },
    ]));
    let calls = p.clone();
    let agent = Agent::builder(cfg, s)
        .provider(p)
        .tool(counting_tool("danger", n.clone(), Ok("done")))
        .tool(counting_tool("safe", n.clone(), Ok("done")))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.output, "ok", "{}", r.error);
    assert_eq!(n.load(Ordering::SeqCst), 0);
    let names: Vec<String> = calls.requests()[0].tools.iter().map(|t| t.name.clone()).collect();
    assert_eq!(names, ["finish", "safe"]);
}

// 2.15 / 2.16: compaction keeps skill instructions whole and its note names
// the call and the turn and says not to fetch it again.
#[tokio::test]
async fn compaction_keeps_skills_and_names_what_it_shortened() {
    let (s, _d) = store().await;
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.context_tokens = 4_000;
    cfg.max_turns = 30;
    cfg.skills = vec![agen_engine::bundle::Skill {
        name: "manual".into(),
        description: "The manual".into(),
        body: "RULE ".repeat(600),
    }];
    let big: Arc<dyn Tool> = Arc::new(FnTool::new(
        "read",
        "r",
        json!({"type": "object"}),
        false,
        |a: Value, _| async move { Ok::<_, ToolError>(format!("page {} {}", a["page"], "x".repeat(3000))) },
    ));
    let mut script = vec![call("load_skill", json!({"name": "manual"}))];
    for i in 0..12 {
        script.push(call("read", json!({"page": i})));
    }
    script.push(FakeResponse {
        text: "done".into(),
        ..Default::default()
    });
    let p = Arc::new(FakeProvider::new(script));
    let calls = p.clone();
    let agent = Agent::builder(cfg, s).provider(p).tool(big).build().unwrap();
    let r = agent.run("read it all", RunOptions::default()).await.unwrap();
    assert_eq!(r.output, "done", "{}", r.error);
    let last = calls.requests().last().unwrap().clone();
    let tool_msgs: Vec<&str> = last
        .messages
        .iter()
        .filter(|m| m.role == Role::Tool)
        .map(|m| m.content.as_str())
        .collect();
    assert!(
        tool_msgs[0].starts_with("RULE") && !tool_msgs[0].contains("left out"),
        "the skill stays whole"
    );
    let short: Vec<&&str> = tool_msgs.iter().filter(|m| m.contains("left out")).collect();
    assert!(!short.is_empty(), "some page results were shortened");
    assert!(
        short[0].contains("this result of read (turn 2)") && short[0].contains("do not call it again"),
        "{}",
        short[0]
    );
}

#[test]
fn context_default_follows_the_model_window() {
    use agen_engine::agent::default_context_tokens;
    assert_eq!(default_context_tokens(1_048_576), 64_000);
    assert_eq!(default_context_tokens(32_768), 16_384);
    assert_eq!(default_context_tokens(4_096), 4_000);
}
