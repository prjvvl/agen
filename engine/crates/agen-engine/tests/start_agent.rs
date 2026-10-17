//! start_agent: async delegation through a task starter (the Manager in
//! managed mode).

use std::sync::{Arc, Mutex};

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::bundle::{Delegate, Limits};
use agen_engine::delegate::{StartAgent, StartRequest, TaskStarter};
use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
use agen_engine::store::{RunStatus, Store};
use async_trait::async_trait;
use serde_json::json;

#[derive(Default)]
struct Recorder(Mutex<Vec<(String, String, String, String)>>);

#[async_trait]
impl TaskStarter for Recorder {
    async fn start(&self, req: StartRequest<'_>) -> Result<String, String> {
        self.0.lock().unwrap().push((
            req.name.to_string(),
            req.input.to_string(),
            req.reply_conversation_key.to_string(),
            req.idempotency_key.to_string(),
        ));
        Ok("task-1".into())
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

fn say(text: &str, expect: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        expect: Some(expect.into()),
        ..Default::default()
    }
}

#[tokio::test]
async fn start_agent_queues_and_returns_at_once() {
    let d = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap());
    let rec = Arc::new(Recorder::default());
    let delegates = vec![Delegate {
        name: "writer".into(),
        namespace: None,
        url: None,
        description: None,
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
        start("writer"),
        say("started", "Started task task-1 on writer"),
        start("writer"),
        start("writer"),
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
    let (name, input, key, idem) = &calls[0];
    assert_eq!((name.as_str(), input.as_str()), ("writer", "do the long thing"));
    assert!(key.starts_with("async:"), "{key}");
    assert!(idem.starts_with(&format!("start:{}.", r.run_id)), "{idem}");
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
