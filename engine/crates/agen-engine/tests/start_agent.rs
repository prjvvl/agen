//! start_agent: async delegation through a task starter (the Manager in
//! managed mode).

use std::sync::{Arc, Mutex};

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::{Delegate, Limits};
use agen_engine::delegate::{StartAgent, StartRequest, TaskStarter, WaitOutcome};
use agen_engine::overrides::TaskOverrides;
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::{RunStatus, Store};
use async_trait::async_trait;
use serde_json::json;

type Recorded = (String, String, String, String, Option<TaskOverrides>, String, String);

#[derive(Default)]
struct Recorder(Mutex<Vec<Recorded>>);

#[async_trait]
impl TaskStarter for Recorder {
    async fn start(&self, req: StartRequest<'_>) -> Result<String, String> {
        self.0.lock().unwrap().push((
            req.name.to_string(),
            req.input.to_string(),
            req.reply_conversation_key.to_string(),
            req.idempotency_key.to_string(),
            req.overrides.cloned(),
            req.model_tier.to_string(),
            req.trust.as_str().to_string(),
        ));
        Ok("task-1".into())
    }

    async fn wait(&self, _task_id: &str, _wait: std::time::Duration) -> Result<WaitOutcome, String> {
        // These tests exercise start_agent (fire-and-forget); nothing waits.
        Ok(WaitOutcome::Pending)
    }

    async fn report_progress(&self, _task_id: &str, _progress: serde_json::Value) -> Result<(), String> {
        Ok(())
    }

    async fn query_descendant(&self, _caller_task_id: &str, _task_id: &str) -> Result<serde_json::Value, String> {
        Ok(serde_json::Value::Null)
    }

    async fn signal_descendant(&self, _caller_task_id: &str, _task_id: &str, _message: &str) -> Result<(), String> {
        Ok(())
    }

    async fn drain_inbox(&self, _task_id: &str) -> Result<Vec<serde_json::Value>, String> {
        Ok(vec![])
    }

    async fn unfinished_children(&self, _task_id: &str) -> Result<i64, String> {
        Ok(0)
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

fn start_detached(agent: &str) -> FakeResponse {
    FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: "start_agent".into(),
            arguments: json!({"agent": agent, "message": "do the long thing", "detach": true}),
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

// detach: true (3.4, part) keeps start_agent's old behaviour: the child's
// result comes back as a reply task, addressed by a conversation key.
#[tokio::test]
async fn start_agent_detach_true_queues_and_replies_into_the_conversation() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let rec = Arc::new(Recorder::default());
    let delegates = vec![Delegate {
        name: "writer".into(),
        ..Default::default()
    }];
    let limits = Limits {
        max_fan_out: Some(1),
        ..Default::default()
    };
    let tool = Arc::new(StartAgent::new(
        delegates,
        "default",
        rec.clone(),
        limits,
        store.clone(),
    ));
    let p = Arc::new(FakeProvider::new(vec![
        start_detached("writer"),
        say("started", "Started task task-1 on writer"),
        start_detached("writer"),
        start_detached("writer"),
        say("one only", "max_fan_out 1"),
    ]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
        .build()
        .unwrap();

    // No conversation key: the session gets one so the result can come back.
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let calls = rec.0.lock().unwrap().clone();
    assert_eq!(calls.len(), 1);
    let (name, input, key, idem, overrides, tier, trust) = &calls[0];
    assert_eq!((name.as_str(), input.as_str()), ("writer", "do the long thing"));
    assert!(key.starts_with("async:"), "{key}");
    assert!(idem.starts_with(&format!("start:{}.", r.run_id)), "{idem}");
    assert_eq!(overrides, &None, "no overrides given, none sent");
    assert_eq!(tier, "");
    assert_eq!(trust, "trusted", "the caller's own trust (no taint yet)");
    // The key is the session's from now on (a second ensure is the same).
    let again = store.ensure_conversation_key(&r.conversation_id).await.unwrap();
    assert_eq!(&again, key);

    // With a conversation key, that key is the reply address; fan-out limits
    // count start_agent like call_agent.
    let r2 = agent
        .run(
            "again",
            RunOptions {
                conversation_key: "api:c9".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r2.status, RunStatus::Succeeded, "{}", r2.error);
    let calls = rec.0.lock().unwrap().clone();
    assert_eq!(calls.len(), 2, "the second start in one run is over max_fan_out");
    assert_eq!(calls[1].2, "api:c9");
}

// Without detach (3.4, part: the new default, "join"), the child has no
// reply_conversation_key at all: it is this task's own child, not a reply
// address lookup.
#[tokio::test]
async fn start_agent_joins_by_default_with_no_reply_key() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let rec = Arc::new(Recorder::default());
    let delegates = vec![Delegate {
        name: "writer".into(),
        ..Default::default()
    }];
    let tool = Arc::new(StartAgent::new(
        delegates,
        "default",
        rec.clone(),
        Limits::default(),
        store.clone(),
    ));
    let p = Arc::new(FakeProvider::new(vec![start("writer"), say("ok", "joined")]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let calls = rec.0.lock().unwrap().clone();
    assert_eq!(calls.len(), 1);
    assert_eq!(
        calls[0].2, "",
        "no reply key: this is a joined child, not a reply-task delivery"
    );
}

#[tokio::test]
async fn start_agent_passes_overrides_only_to_delegates_that_accept_them() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let rec = Arc::new(Recorder::default());
    let delegates = vec![
        Delegate {
            name: "worker".into(),
            overrides: true,
            ..Default::default()
        },
        Delegate {
            name: "writer".into(),
            ..Default::default()
        },
    ];
    let tool = Arc::new(StartAgent::new(
        delegates,
        "default",
        rec.clone(),
        Limits::default(),
        store.clone(),
    ));
    let with_overrides = FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: "start_agent".into(),
            arguments: json!({
                "agent": "worker",
                "message": "count the files",
                "tools": ["read.*"],
                "budget": {"maxTokens": 2000, "maxDuration": "5m"},
                "modelTier": "low"
            }),
        }],
        ..Default::default()
    };
    let rejected = FakeResponse {
        tool_calls: vec![ScriptedToolCall {
            name: "start_agent".into(),
            arguments: json!({"agent": "writer", "message": "draft it", "tools": ["read.*"]}),
        }],
        ..Default::default()
    };
    let p = Arc::new(FakeProvider::new(vec![
        with_overrides,
        say("ok", "Started task task-1 on worker"),
        rejected,
        say("done", "does not accept overrides"),
    ]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let calls = rec.0.lock().unwrap().clone();
    assert_eq!(calls.len(), 1, "writer's call was refused before reaching the starter");
    let (.., overrides, tier, _trust) = &calls[0];
    let o = overrides.clone().expect("worker accepts overrides");
    assert_eq!(o.tools, vec!["read.*"]);
    assert_eq!(o.budget.unwrap().max_tokens, Some(2000));
    assert_eq!(tier, "low");
}

// A run that has been tainted (called an untrusted-output tool) starts its
// child at its own current trust, not the task's original one.
#[tokio::test]
async fn start_agent_passes_the_callers_current_trust_not_its_original_one() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let rec = Arc::new(Recorder::default());
    let delegates = vec![agen_engine::bundle::Delegate {
        name: "writer".into(),
        ..Default::default()
    }];
    let tool = Arc::new(StartAgent::new(
        delegates,
        "default",
        rec.clone(),
        Limits::default(),
        store.clone(),
    ));
    struct Fetch;
    #[async_trait]
    impl agen_engine::tools::Tool for Fetch {
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
            true
        }
        async fn call(
            &self,
            _args: serde_json::Value,
            _ctx: agen_engine::tools::ToolContext,
        ) -> Result<String, agen_engine::tools::ToolError> {
            Ok("page content".into())
        }
    }
    let p = Arc::new(FakeProvider::new(vec![
        FakeResponse {
            tool_calls: vec![ScriptedToolCall {
                name: "fetch".into(),
                arguments: json!({}),
            }],
            ..Default::default()
        },
        start("writer"),
        say("started", "Started task task-1 on writer"),
    ]));
    let agent = Agent::builder(AgentConfig::new("lead", "Lead.", "fake-1"), store.clone())
        .provider(p)
        .tool(tool)
        .tool(Arc::new(Fetch))
        .build()
        .unwrap();
    let r = agent.run("go", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    let calls = rec.0.lock().unwrap().clone();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].6, "untrusted", "tainted before starting the child");
}
