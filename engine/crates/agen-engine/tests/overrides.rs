//! Per-task overrides applied to a run: hidden tools, extra deny/ask
//! rules, a narrowed budget, preloaded skills and a system addendum.

use std::sync::Arc;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::Skill;
use agen_engine::overrides::{OverrideRule, RunBudgetOverride, TaskOverrides};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::{RunStatus, Store};
use agen_engine::tools::{FnTool, ToolError};
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

fn echo_tool(name: &str) -> Arc<dyn agen_engine::tools::Tool> {
    Arc::new(FnTool::new(
        name,
        "t",
        json!({"type": "object"}),
        false,
        |_: Value, _| async move { Ok::<_, ToolError>("ok".into()) },
    ))
}

fn cfg() -> AgentConfig {
    let mut c = AgentConfig::new("worker", "You help.", "fake-1");
    c.max_turns = 20;
    c.permissions.default = agen_engine::bundle::Action::Allow;
    c
}

// 2.32: a task's `tools` allowlist hides everything else, even when the
// bundle's own permissions allow it.
#[tokio::test]
async fn tools_override_hides_everything_else() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        call("write", json!({})),
        say("could not write"),
    ]));
    let calls = p.clone();
    let agent = Agent::builder(cfg(), s)
        .provider(p)
        .tool(echo_tool("read"))
        .tool(echo_tool("write"))
        .build()
        .unwrap();
    let opts = RunOptions {
        overrides: Some(TaskOverrides {
            tools: vec!["read".into()],
            ..Default::default()
        }),
        ..Default::default()
    };
    let r = agent.run("go", opts).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    // write was never offered to the model at all.
    let tools: Vec<String> = calls.requests()[0].tools.iter().map(|t| t.name.clone()).collect();
    assert!(!tools.contains(&"write".to_string()), "{tools:?}");
    assert!(tools.contains(&"read".to_string()), "{tools:?}");
    assert!(tools.contains(&"finish".to_string()), "finish always stays: {tools:?}");
}

// 2.32: extra deny/ask rules are checked alongside the bundle's own
// (deny wins, as usual), and never loosen it (a rule asking "allow" is
// rejected before the run even starts — covered by a unit test in
// overrides.rs — so there is nothing to allow here beyond the bundle).
#[tokio::test]
async fn extra_permission_rules_narrow_the_bundles_own() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        call("danger", json!({})),
        say("could not use danger"),
    ]));
    let agent = Agent::builder(cfg(), s)
        .provider(p)
        .tool(echo_tool("danger"))
        .build()
        .unwrap();
    let opts = RunOptions {
        overrides: Some(TaskOverrides {
            permissions: vec![OverrideRule {
                tool: "danger".into(),
                action: "deny".into(),
            }],
            ..Default::default()
        }),
        ..Default::default()
    };
    let r = agent.run("go", opts).await.unwrap();
    assert_eq!(r.output, "could not use danger", "{}", r.error);
}

// 2.32: budget.maxTurns and budget.maxTokens narrow the bundle's own run
// limits (never widen them), and the run still gets its report call.
#[tokio::test]
async fn budget_override_narrows_max_turns() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![call("read", json!({}))]).cycling());
    let agent = Agent::builder(cfg(), s)
        .provider(p)
        .tool(echo_tool("read"))
        .build()
        .unwrap();
    let opts = RunOptions {
        overrides: Some(TaskOverrides {
            budget: Some(RunBudgetOverride {
                max_turns: Some(1),
                ..Default::default()
            }),
            ..Default::default()
        }),
        ..Default::default()
    };
    let r = agent.run("go", opts).await.unwrap();
    let o = r.outcome.unwrap();
    assert_eq!((o.status.as_str(), o.reason.as_str()), ("partial", "max_turns"));

    // The bundle's own (lower) max_turns is not widened by a larger override.
    let (s, _d) = store().await;
    let mut c = cfg();
    c.max_turns = 1;
    let p = Arc::new(FakeProvider::new(vec![call("read", json!({}))]).cycling());
    let agent = Agent::builder(c, s)
        .provider(p)
        .tool(echo_tool("read"))
        .build()
        .unwrap();
    let opts = RunOptions {
        overrides: Some(TaskOverrides {
            budget: Some(RunBudgetOverride {
                max_turns: Some(100),
                ..Default::default()
            }),
            ..Default::default()
        }),
        ..Default::default()
    };
    let r = agent.run("go", opts).await.unwrap();
    let o = r.outcome.unwrap();
    assert_eq!(o.reason, "max_turns", "the bundle's lower limit still applies");
}

// 2.32: skills to preload (in addition to the bundle's own) and a system
// addendum both land in the system prompt sent to the model.
#[tokio::test]
async fn skills_and_system_addendum_are_applied() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.skills = vec![Skill {
        name: "greeting".into(),
        description: "How to greet".into(),
        body: "Say hello warmly.".into(),
    }];
    let p = Arc::new(FakeProvider::new(vec![say("hi")]));
    let calls = p.clone();
    let agent = Agent::builder(c, s).provider(p).build().unwrap();
    let opts = RunOptions {
        overrides: Some(TaskOverrides {
            skills: vec!["greeting".into()],
            system_addendum: "Reply in French.".into(),
            ..Default::default()
        }),
        ..Default::default()
    };
    let r = agent.run("go", opts).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let system = &calls.requests()[0].system;
    assert!(system.contains("Say hello warmly."), "{system}");
    assert!(system.contains("Reply in French."), "{system}");
    assert!(
        system.ends_with("Reply in French."),
        "the addendum is appended last: {system}"
    );
}

// Without overrides, nothing changes (today's behaviour).
#[tokio::test]
async fn no_overrides_means_the_bundle_as_is() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![call("read", json!({})), say("done")]));
    let agent = Agent::builder(cfg(), s)
        .provider(p)
        .tool(echo_tool("read"))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
}
