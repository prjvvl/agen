//! Trust and taint (3.7): a run starts at its task's trust and only ever
//! drops it, when a tool whose result may carry untrusted content is
//! called; a conditional permission rule can act on the run's current
//! trust.

use std::sync::Arc;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::{Action, PermissionRule, Trust, When};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::Store;
use agen_engine::tools::{Tool, ToolContext, ToolError};
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

/// A tool whose result may carry content from outside the agent's control
/// (like a web search), and one that never does.
struct Fetch {
    untrusted: bool,
}

#[async_trait]
impl Tool for Fetch {
    fn spec(&self) -> agen_engine::provider::ToolSpec {
        agen_engine::provider::ToolSpec {
            name: "fetch".into(),
            description: "Read a page".into(),
            parameters: json!({"type": "object"}),
        }
    }
    fn side_effect(&self) -> bool {
        false
    }
    fn untrusted_output(&self) -> bool {
        self.untrusted
    }
    async fn call(&self, _args: Value, _ctx: ToolContext) -> Result<String, ToolError> {
        Ok("page content".into())
    }
}

#[tokio::test]
async fn calling_an_untrusted_output_tool_lowers_trust_and_marks_the_span() {
    let (s, _d) = store().await;
    let cfg = AgentConfig::new("a", "x", "fake-1");
    let p = Arc::new(FakeProvider::new(vec![
        call("fetch", json!({})),
        FakeResponse {
            text: "done".into(),
            ..Default::default()
        },
    ]));
    let agent = Agent::builder(cfg, s.clone())
        .provider(p)
        .tool(Arc::new(Fetch { untrusted: true }))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.output, "done", "{}", r.error);
    let spans = s.trace(&r.trace_id).await.unwrap();
    let tool_span = spans.iter().find(|sp| sp.name == "agen.tool").unwrap();
    assert_eq!(tool_span.attributes["agen.trust.lowered"], json!("fetch"));
}

#[tokio::test]
async fn a_tool_without_untrusted_output_never_lowers_trust() {
    let (s, _d) = store().await;
    let cfg = AgentConfig::new("a", "x", "fake-1");
    let p = Arc::new(FakeProvider::new(vec![
        call("fetch", json!({})),
        FakeResponse {
            text: "done".into(),
            ..Default::default()
        },
    ]));
    let agent = Agent::builder(cfg, s.clone())
        .provider(p)
        .tool(Arc::new(Fetch { untrusted: false }))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.output, "done", "{}", r.error);
    let spans = s.trace(&r.trace_id).await.unwrap();
    let tool_span = spans.iter().find(|sp| sp.name == "agen.tool").unwrap();
    assert!(tool_span.attributes.get("agen.trust.lowered").is_none());
}

// The lethal-trifecta rule: a run that has read untrusted content must ask
// before acting outward, even though the acting tool itself is fine on its
// own.
#[tokio::test]
async fn trust_below_rule_asks_only_after_a_tainting_call() {
    let (s, _d) = store().await;
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.permissions.rules = vec![PermissionRule {
        tool: "send".into(),
        action: Action::Ask,
        when: Some(When {
            trust: None,
            trust_below: Some(Trust::Trusted),
        }),
    }];
    struct DenyAll;
    #[async_trait]
    impl agen_engine::agent::Approver for DenyAll {
        async fn decide(&self, _req: agen_engine::agent::ApprovalRequest) -> agen_engine::agent::ApprovalDecision {
            agen_engine::agent::ApprovalDecision::Denied
        }
    }
    let p = Arc::new(FakeProvider::new(vec![
        call("fetch", json!({})),
        call("send", json!({})),
    ]));
    let agent = Agent::builder(cfg, s.clone())
        .provider(p)
        .tool(Arc::new(Fetch { untrusted: true }))
        .tool(Arc::new(agen_engine::tools::FnTool::new(
            "send",
            "Send a message",
            json!({"type": "object"}),
            true,
            |_: Value, _| async { Ok("sent".into()) },
        )))
        .approver(Arc::new(DenyAll))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    // fetch (untrusted) taints the run; send is then asked for and denied.
    let spans = s.trace(&r.trace_id).await.unwrap();
    let send_span = spans.iter().rfind(|sp| sp.name == "agen.permission.check").unwrap();
    assert_eq!(send_span.attributes["agen.permission.decision"], json!("denied"));
}

#[tokio::test]
async fn a_task_submitted_untrusted_asks_from_the_start() {
    let (s, _d) = store().await;
    let mut cfg = AgentConfig::new("a", "x", "fake-1");
    cfg.permissions.rules = vec![PermissionRule {
        tool: "send".into(),
        action: Action::Ask,
        when: Some(When {
            trust: None,
            trust_below: Some(Trust::Trusted),
        }),
    }];
    struct DenyAll;
    #[async_trait]
    impl agen_engine::agent::Approver for DenyAll {
        async fn decide(&self, _req: agen_engine::agent::ApprovalRequest) -> agen_engine::agent::ApprovalDecision {
            agen_engine::agent::ApprovalDecision::Denied
        }
    }
    let p = Arc::new(FakeProvider::new(vec![call("send", json!({}))]));
    let agent = Agent::builder(cfg, s.clone())
        .provider(p)
        .tool(Arc::new(agen_engine::tools::FnTool::new(
            "send",
            "Send a message",
            json!({"type": "object"}),
            true,
            |_: Value, _| async { Ok("sent".into()) },
        )))
        .approver(Arc::new(DenyAll))
        .build()
        .unwrap();
    let r = agent
        .run(
            "go",
            RunOptions {
                trust: Trust::Untrusted,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    let spans = s.trace(&r.trace_id).await.unwrap();
    let send_span = spans.iter().find(|sp| sp.name == "agen.permission.check").unwrap();
    assert_eq!(send_span.attributes["agen.permission.decision"], json!("denied"));
}
