//! Fallback models, empty answers, account errors and routing models.

use std::sync::Arc;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::provider::fake::{FakeProvider, FakeResponse};
use agen_engine::provider::router::RouterSettings;
use agen_engine::store::{RunStatus, SpanRecord, Store};
use serde_json::{json, Value};

async fn store() -> (Arc<Store>, tempfile::TempDir) {
    let d = tempfile::tempdir().unwrap();
    (
        Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap()),
        d,
    )
}

fn answer(model: &str, text: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        expect_model: Some(model.into()),
        ..Default::default()
    }
}

fn failing(model: &str, kind: &str) -> FakeResponse {
    FakeResponse {
        error: Some(format!("{model} is down")),
        error_kind: Some(kind.into()),
        expect_model: Some(model.into()),
        ..Default::default()
    }
}

fn empty(model: &str) -> FakeResponse {
    FakeResponse {
        expect_model: Some(model.into()),
        ..Default::default()
    }
}

fn cfg(model: &str, fallbacks: &[&str]) -> AgentConfig {
    let mut c = AgentConfig::new("a", "Be brief.", model);
    c.fallback_models = fallbacks.iter().map(|s| s.to_string()).collect();
    c
}

async fn chat_spans(s: &Store, trace: &str) -> Vec<SpanRecord> {
    s.trace(trace)
        .await
        .unwrap()
        .into_iter()
        .filter(|s| s.name == "gen_ai.chat")
        .collect()
}

#[tokio::test]
async fn falls_back_when_the_model_fails() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        failing("primary/m", "request"),
        answer("backup/m", "from the backup"),
    ]));
    let agent = Agent::builder(cfg("primary/m", &["backup/m"]), s.clone())
        .provider(p.clone())
        .build()
        .unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert_eq!(r.output, "from the backup");
    let spans = chat_spans(&s, &r.trace_id).await;
    assert_eq!(spans.len(), 1);
    assert_eq!(spans[0].attributes["gen_ai.request.model"], "backup/m");
    assert_eq!(spans[0].attributes["gen_ai.response.model"], "backup/m");
    assert_eq!(spans[0].attributes["agen.fallback"], true);
}

#[tokio::test]
async fn account_errors_do_not_fall_back() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        failing("primary/m", "account"),
        answer("backup/m", "never"),
    ]));
    let agent = Agent::builder(cfg("primary/m", &["backup/m"]), s)
        .provider(p.clone())
        .build()
        .unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert!(r.error.contains("provider account error"), "{}", r.error);
    assert_eq!(p.calls(), 1, "no other model is tried on an account error");
}

#[tokio::test]
async fn an_empty_answer_is_asked_again_then_falls_back() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        empty("primary/m"),
        empty("primary/m"),
        answer("backup/m", "finally"),
    ]));
    let agent = Agent::builder(cfg("primary/m", &["backup/m"]), s)
        .provider(p.clone())
        .build()
        .unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(
        (r.status, r.output.as_str()),
        (RunStatus::Succeeded, "finally"),
        "{}",
        r.error
    );
    assert_eq!(p.calls(), 3);

    // Asked again once: a second try that answers needs no fallback.
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        empty("primary/m"),
        answer("primary/m", "second try"),
    ]));
    let agent = Agent::builder(cfg("primary/m", &[]), s).provider(p).build().unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.output, "second try");
}

#[tokio::test]
async fn no_answer_from_any_model_fails_the_run_and_says_why() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        empty("primary/m"),
        empty("primary/m"),
        empty("backup/m"),
        empty("backup/m"),
    ]));
    let agent = Agent::builder(cfg("primary/m", &["backup/m"]), s.clone())
        .provider(p)
        .build()
        .unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Failed);
    assert!(r.error.starts_with("no_answer:"), "{}", r.error);
    let spans = chat_spans(&s, &r.trace_id).await;
    assert_eq!(spans[0].status, "error");
}

fn routed(tier: &str, served: &str) -> FakeResponse {
    let mut extra = serde_json::Map::new();
    extra.insert("plugins".into(), json!([{"id": "jev-router", "cost_tier": tier}]));
    FakeResponse {
        text: format!("{tier} answer"),
        model: Some(served.into()),
        expect_model: Some("typesafe/jev-router".into()),
        expect_extra: Some(extra),
        ..Default::default()
    }
}

#[tokio::test]
async fn routing_model_tiers_and_served_model() {
    let (s, _d) = store().await;
    let p = Arc::new(FakeProvider::new(vec![
        routed("medium", "vendor/mid-model"),
        routed("high", "vendor/big-model"),
        routed("high", "vendor/big-model"),
        routed("low", "vendor/small-model"),
    ]));
    let mut c = cfg("typesafe/jev-router", &[]);
    c.router = Some(RouterSettings {
        tier: Some("medium".into()),
        max_tier: Some("high".into()),
        ..Default::default()
    });
    let agent = Agent::builder(c, s.clone()).provider(p).build().unwrap();
    let run = |tier: Option<&str>, near_budget: bool| RunOptions {
        model_tier: tier.map(Into::into),
        near_budget,
        ..Default::default()
    };
    // The default tier; a task asking for high; asking above maxTier is
    // capped at it; near the daily budget it drops to low.
    let r1 = agent.run("a", run(None, false)).await.unwrap();
    let r2 = agent.run("b", run(Some("high"), false)).await.unwrap();
    let r3 = agent.run("c", run(Some("xhigh"), false)).await.unwrap();
    let r4 = agent.run("d", run(Some("high"), true)).await.unwrap();
    for (r, tier, served) in [
        (&r1, "medium", "vendor/mid-model"),
        (&r2, "high", "vendor/big-model"),
        (&r3, "high", "vendor/big-model"),
        (&r4, "low", "vendor/small-model"),
    ] {
        assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
        let span = &chat_spans(&s, &r.trace_id).await[0];
        assert_eq!(span.attributes["agen.router.tier"], Value::from(tier));
        assert_eq!(span.attributes["gen_ai.response.model"], Value::from(served));
    }
}
