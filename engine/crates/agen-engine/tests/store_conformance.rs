//! Storage conformance suite, run against SQLite and (when
//! `AGEN_TEST_POSTGRES_URL` is set) Postgres. Both backends must behave the same.

use agen_engine::provider::{Message, ToolCall, Usage};
use agen_engine::store::{new_id, now_ms, EffectBegin, RunRecord, RunStatus, SpanRecord, Store, StoreError};

async fn sqlite() -> (Store, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    (Store::open_sqlite_path(&dir.path().join("agen.db")).await.unwrap(), dir)
}

async fn postgres() -> Option<Store> {
    // CI sets the variable to "" on runners without Postgres: same as unset.
    let Some(url) = std::env::var("AGEN_TEST_POSTGRES_URL").ok().filter(|u| !u.is_empty()) else {
        // CI sets AGEN_REQUIRE_PG=1 so a missing database fails loudly instead of skipping.
        assert!(
            std::env::var("AGEN_REQUIRE_PG").as_deref() != Ok("1"),
            "AGEN_REQUIRE_PG=1 but AGEN_TEST_POSTGRES_URL is unset"
        );
        return None;
    };
    Some(Store::open(&url).await.unwrap())
}

fn run(session: &str, conv: &str, root: &str) -> RunRecord {
    RunRecord {
        id: new_id(),
        session_id: session.into(),
        conversation_id: conv.into(),
        namespace: "default".into(),
        deployment: format!("dep-{}", new_id()),
        definition_digest: "sha256:x".into(),
        task_id: String::new(),
        parent_run_id: String::new(),
        root_run_id: root.into(),
        status: RunStatus::Running,
        input: "do it".into(),
        output: String::new(),
        error: String::new(),
        step: 0,
        usage: Usage::default(),
        trace_id: "t".repeat(32),
        started_ms: now_ms(),
        ended_ms: None,
        requested_by: String::new(),
    }
}

async fn sessions_conversations_messages(s: &Store) {
    let sess = s.create_session("agent", "default", "d1").await.unwrap();
    assert_eq!(s.get_session(&sess.id).await.unwrap(), sess);
    s.set_memory(&sess.id, &serde_json::json!({"fact": "sky is blue"}))
        .await
        .unwrap();
    assert_eq!(s.get_session(&sess.id).await.unwrap().memory["fact"], "sky is blue");

    let c1 = s.current_conversation(&sess.id).await.unwrap();
    assert_eq!(
        c1,
        s.current_conversation(&sess.id).await.unwrap(),
        "reuses open conversation"
    );
    let r = run(&sess.id, &c1, "");
    s.create_run(&r, "owner-a").await.unwrap();
    let call = ToolCall {
        id: "c1".into(),
        name: "t".into(),
        arguments: serde_json::json!({"x": 1}),
    };
    assert_eq!(s.append_message(&c1, &r.id, &Message::user("hi")).await.unwrap(), 1);
    assert_eq!(
        s.append_message(&c1, &r.id, &Message::assistant("", vec![call.clone()]))
            .await
            .unwrap(),
        2
    );
    assert_eq!(
        s.append_message(&c1, &r.id, &Message::tool("c1", "out")).await.unwrap(),
        3
    );
    let msgs = s.messages(&c1).await.unwrap();
    assert_eq!(msgs.len(), 3);
    assert_eq!(msgs[1].tool_calls[0], call);
    assert_eq!(s.run_messages(&r.id).await.unwrap(), msgs);

    s.close_conversation(&c1).await.unwrap();
    let c2 = s.current_conversation(&sess.id).await.unwrap();
    assert_ne!(c1, c2, "closing starts a new conversation");
    assert!(s.messages(&c2).await.unwrap().is_empty());

    // Singleton deployments get one stable session.
    let a = s.session_for_deployment("agent", "default", "single").await.unwrap();
    let b = s.session_for_deployment("agent", "default", "single").await.unwrap();
    assert_eq!(a.id, b.id);
}

async fn runs_lifecycle(s: &Store) {
    let sess = s.create_session("agent", "default", "d").await.unwrap();
    let conv = s.current_conversation(&sess.id).await.unwrap();
    let mut r = run(&sess.id, &conv, "");
    r.root_run_id = r.id.clone();
    let epoch = s.create_run(&r, "owner-a").await.unwrap();
    let unfinished = s.unfinished_runs(&r.namespace, &r.deployment).await.unwrap();
    assert_eq!(unfinished.len(), 1);
    let u = Usage {
        input_tokens: 10,
        output_tokens: 5,
        cost_usd: 0.25,
    };
    s.checkpoint(
        &r.id,
        epoch,
        &conv,
        Some(&Message::user("x")),
        Some((2, RunStatus::WaitingApproval, u)),
    )
    .await
    .unwrap();
    let got = s.get_run(&r.id).await.unwrap();
    assert_eq!((got.step, got.status, got.usage), (2, RunStatus::WaitingApproval, u));
    assert_eq!(s.run_messages(&r.id).await.unwrap().len(), 1);

    // A second active run in the same conversation is refused.
    let other = run(&sess.id, &conv, "");
    assert!(matches!(
        s.create_run(&other, "owner-b").await,
        Err(StoreError::Conflict(_))
    ));

    // Ownership: a new owner fences off the old one.
    let epoch2 = s.claim_run(&r.id, "owner-b").await.unwrap();
    assert_eq!(epoch2, epoch + 1);
    assert!(matches!(
        s.checkpoint(&r.id, epoch, &conv, Some(&Message::user("stale")), None)
            .await,
        Err(StoreError::Fenced(_))
    ));
    assert_eq!(
        s.run_messages(&r.id).await.unwrap().len(),
        1,
        "fenced write must not append"
    );
    assert!(matches!(
        s.finish_run(&r.id, epoch, RunStatus::Succeeded, "x", "", u).await,
        Err(StoreError::Fenced(_))
    ));

    s.finish_run(&r.id, epoch2, RunStatus::Succeeded, "done", "", u)
        .await
        .unwrap();
    let got = s.get_run(&r.id).await.unwrap();
    assert_eq!(got.status, RunStatus::Succeeded);
    assert_eq!(got.output, "done");
    assert!(got.ended_ms.is_some());
    assert!(s.unfinished_runs(&r.namespace, &r.deployment).await.unwrap().is_empty());
    assert!(
        s.claim_run(&r.id, "owner-c").await.is_err(),
        "finished runs cannot be claimed"
    );

    // The conversation is free again.
    let mut child = run(&sess.id, &conv, &r.id);
    child.parent_run_id = r.id.clone();
    s.create_run(&child, "owner-a").await.unwrap();
    let tree = s.runs_by_root(&r.id).await.unwrap();
    assert_eq!(tree.len(), 2);
    assert_eq!(s.list_runs(&r.namespace, &r.deployment, 10).await.unwrap().len(), 1);
    assert!(s.get_run("missing").await.is_err());
}

async fn concurrent_singleton_sessions_and_conversations_are_unique(s: &Store) {
    let dep = format!("single-{}", new_id());
    let (a, b, c) = tokio::join!(
        s.session_for_deployment("x", "default", &dep),
        s.session_for_deployment("x", "default", &dep),
        s.session_for_deployment("x", "default", &dep)
    );
    let ids = [a.unwrap().id, b.unwrap().id, c.unwrap().id];
    assert!(ids.iter().all(|i| *i == ids[0]), "{ids:?}");
    let (c1, c2) = tokio::join!(s.current_conversation(&ids[0]), s.current_conversation(&ids[0]));
    assert_eq!(c1.unwrap(), c2.unwrap());
}

/// A fresh run owned by "owner-a"; returns (run id, epoch).
async fn owned_run(s: &Store) -> (String, i64) {
    let sess = s.create_session("agent", "default", "ledger").await.unwrap();
    let conv = s.current_conversation(&sess.id).await.unwrap();
    let r = run(&sess.id, &conv, "");
    let epoch = s.create_run(&r, "owner-a").await.unwrap();
    (r.id, epoch)
}

async fn effect_ledger(s: &Store) {
    let (run_id, e) = owned_run(s).await;
    assert_eq!(
        s.begin_effect(&run_id, e, "c1", "pay", "h1", "").await.unwrap(),
        EffectBegin::Proceed
    );
    // Crash before completion: a second attempt must not proceed.
    assert_eq!(
        s.begin_effect(&run_id, e, "c1", "pay", "h1", "").await.unwrap(),
        EffectBegin::Unknown
    );
    s.complete_effect(&run_id, e, "c1", "{\"ok\":true}").await.unwrap();
    assert_eq!(
        s.begin_effect(&run_id, e, "c1", "pay", "h1", "").await.unwrap(),
        EffectBegin::Completed {
            result: "{\"ok\":true}".into()
        }
    );
    // Same call id with different arguments is a bug, not a replay.
    assert!(s.begin_effect(&run_id, e, "c1", "pay", "h2", "").await.is_err());
    // Independent calls are independent.
    assert_eq!(
        s.begin_effect(&run_id, e, "c2", "pay", "h1", "").await.unwrap(),
        EffectBegin::Proceed
    );
    s.reset_effect(&run_id, e, "c2").await.unwrap();
}

async fn fenced_owner_cannot_touch_the_ledger(s: &Store) {
    let (run_id, old) = owned_run(s).await;
    assert_eq!(
        s.begin_effect(&run_id, old, "c1", "pay", "h", "").await.unwrap(),
        EffectBegin::Proceed
    );
    let new = s.claim_run(&run_id, "owner-b").await.unwrap();
    assert!(matches!(
        s.begin_effect(&run_id, old, "c2", "pay", "h", "").await,
        Err(StoreError::Fenced(_))
    ));
    assert!(matches!(
        s.complete_effect(&run_id, old, "c1", "late").await,
        Err(StoreError::Fenced(_))
    ));
    assert!(matches!(
        s.reset_effect(&run_id, old, "c1").await,
        Err(StoreError::Fenced(_))
    ));
    // The new owner sees the in-flight effect as unknown and can proceed with new calls.
    assert_eq!(
        s.begin_effect(&run_id, new, "c1", "pay", "h", "").await.unwrap(),
        EffectBegin::Unknown
    );
    assert_eq!(
        s.begin_effect(&run_id, new, "c2", "pay", "h", "").await.unwrap(),
        EffectBegin::Proceed
    );
    // Tasks map to one run, and only within their deployment.
    assert!(s.run_by_task("ns", "d", "no-such-task").await.unwrap().is_none());
    let sess = s.create_session("agent", "default", "tasks").await.unwrap();
    let conv = s.current_conversation(&sess.id).await.unwrap();
    let mut r = run(&sess.id, &conv, "");
    r.task_id = format!("task-{}", new_id());
    s.create_run(&r, "owner-a").await.unwrap();
    let found = s.run_by_task("default", &r.deployment, &r.task_id).await.unwrap();
    assert_eq!(found.map(|f| f.id), Some(r.id.clone()));
    assert!(s
        .run_by_task("default", "another-deployment", &r.task_id)
        .await
        .unwrap()
        .is_none());
    assert!(s
        .run_by_task("other-ns", &r.deployment, &r.task_id)
        .await
        .unwrap()
        .is_none());
}

async fn concurrent_effects_only_one_proceeds(s: &Store) {
    let (run_id, e) = owned_run(s).await;
    let results =
        futures_util::future::join_all((0..8).map(|_| s.begin_effect(&run_id, e, "same", "t", "h", ""))).await;
    let proceeds = results.iter().filter(|r| matches!(r, Ok(EffectBegin::Proceed))).count();
    assert_eq!(proceeds, 1, "{results:?}");
    assert!(results.iter().all(|r| r.is_ok()), "{results:?}");
}

async fn spans_delegations_logs(s: &Store) {
    let trace = new_id();
    for (i, name) in ["run", "gen_ai.chat", "tool.execute"].iter().enumerate() {
        s.insert_span(&SpanRecord {
            span_id: format!("{trace}-{i}"),
            trace_id: trace.clone(),
            parent_span_id: if i == 0 { String::new() } else { format!("{trace}-0") },
            run_id: "r".into(),
            name: name.to_string(),
            start_ms: 1000 + i as i64,
            seq: i as i64,
            end_ms: 2000,
            status: "ok".into(),
            attributes: serde_json::json!({"gen_ai.request.model": "fake-1"}),
        })
        .await
        .unwrap();
    }
    let spans = s.trace(&trace).await.unwrap();
    assert_eq!(spans.len(), 3);
    assert_eq!(spans[1].parent_span_id, spans[0].span_id);
    assert_eq!(spans[1].attributes["gen_ai.request.model"], "fake-1");

    let root = new_id();
    for want in 1..=3 {
        assert_eq!(s.incr_delegations(&root).await.unwrap(), want);
    }
    s.append_log("i1", "default", "d", "info", "hello log").await.unwrap();
    assert!(s.dump_text("logs").await.unwrap().contains("hello log"));
    assert!(s.dump_text("pg_user").await.is_err());
}

async fn agent_run_persists_everything(s: &Store) {
    use agen_engine::agent::{Agent, AgentConfig, RunOptions};
    use agen_engine::provider::fake::{FakeProvider, FakeResponse, ScriptedToolCall};
    use agen_engine::tools::{FnTool, ToolError};
    use std::sync::Arc;
    let store = Arc::new(Store::open(&s.url()).await.unwrap());
    let mut cfg = AgentConfig::new("pg-agent", "Use tools.", "fake-1");
    cfg.deployment = format!("dep-{}", new_id());
    let tool = Arc::new(FnTool::new(
        "pay",
        "pays",
        serde_json::json!({"type":"object"}),
        true,
        |_a: serde_json::Value, _| async { Ok::<_, ToolError>("paid".to_string()) },
    ));
    let p = Arc::new(FakeProvider::new(vec![
        FakeResponse {
            tool_calls: vec![ScriptedToolCall {
                name: "pay".into(),
                arguments: serde_json::json!({"to": "bob"}),
            }],
            ..Default::default()
        },
        FakeResponse {
            text: "done".into(),
            expect: Some("paid".into()),
            ..Default::default()
        },
    ]));
    let agent = Agent::builder(cfg, store.clone())
        .provider(p)
        .tool(tool)
        .build()
        .unwrap();
    let r = agent
        .run(
            "pay bob",
            RunOptions {
                singleton: true,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(
        (r.status, r.output.as_str()),
        (RunStatus::Succeeded, "done"),
        "{}",
        r.error
    );
    let rec = store.get_run(&r.run_id).await.unwrap();
    assert_eq!(rec.step, 2);
    assert_eq!(store.run_messages(&r.run_id).await.unwrap().len(), 4);
    let names: Vec<String> = store
        .trace(&r.trace_id)
        .await
        .unwrap()
        .into_iter()
        .map(|s| s.name)
        .collect();
    assert!(
        names.contains(&"agen.run".to_string()) && names.contains(&"agen.tool".to_string()),
        "{names:?}"
    );
    assert!(store.dump_text("effects").await.unwrap().contains(&r.run_id));
}

macro_rules! conformance {
    ($($name:ident),*) => {
        mod sqlite_backend {
            use super::*;
            $(#[tokio::test] async fn $name() { let (s, _d) = sqlite().await; super::$name(&s).await; })*
        }
        mod postgres_backend {
            use super::*;
            $(#[tokio::test] async fn $name() {
                let Some(s) = postgres().await else { eprintln!("skip: AGEN_TEST_POSTGRES_URL not set"); return; };
                super::$name(&s).await;
            })*
        }
    };
}

conformance!(
    fenced_owner_cannot_touch_the_ledger,
    agent_run_persists_everything,
    concurrent_singleton_sessions_and_conversations_are_unique,
    sessions_conversations_messages,
    runs_lifecycle,
    effect_ledger,
    concurrent_effects_only_one_proceeds,
    spans_delegations_logs
);

#[tokio::test]
async fn reopening_sqlite_keeps_data_and_does_not_remigrate() {
    let dir = tempfile::tempdir().unwrap();
    let url = format!(
        "sqlite://{}",
        dir.path().join("a.db").display().to_string().replace('\\', "/")
    );
    let s = Store::open(&url).await.unwrap();
    let sess = s.create_session("a", "n", "d").await.unwrap();
    s.close().await;
    let s = Store::open(&url).await.unwrap();
    assert_eq!(s.get_session(&sess.id).await.unwrap().agent, "a");
}

#[tokio::test]
async fn rejects_unknown_url() {
    assert!(Store::open("mysql://x").await.is_err());
}
