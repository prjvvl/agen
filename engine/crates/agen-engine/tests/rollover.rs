//! Rollover (1.8, part), off by default (`x-agen/config.json` `rollover`):
//! once a conversation's history no longer fits `context_tokens`, the
//! part about to be dropped is folded into a running summary (one extra
//! call to the agent's own provider) instead of just disappearing, so a
//! long conversation keeps the model's memory of what happened even
//! though the exact words are gone from its context.

use std::sync::Arc;

use agen_engine::agent::{Agent, AgentConfig, RunOptions};
use agen_engine::provider::fake::{FakeProvider, FakeResponse};
use agen_engine::provider::replay::RecordingProvider;
use agen_engine::provider::Role;
use agen_engine::store::Store;

async fn store() -> (Arc<Store>, tempfile::TempDir) {
    let d = tempfile::tempdir().unwrap();
    (
        Arc::new(Store::open_sqlite_path(&d.path().join("t.db")).await.unwrap()),
        d,
    )
}

fn say(text: &str) -> FakeResponse {
    FakeResponse {
        text: text.into(),
        ..Default::default()
    }
}

fn cfg() -> AgentConfig {
    let mut c = AgentConfig::new("a", "x", "fake-1");
    c.context_tokens = 60;
    c.rollover_enabled = true;
    c
}

// A turn dropped to fit the context budget is folded into a running
// summary (one extra provider call) and that summary, not the raw
// turn, is what the next real model call actually sees.
#[tokio::test]
async fn a_dropped_turn_is_summarized_and_kept_in_context_as_a_summary() {
    let (s, _d) = store().await;
    let long = "x".repeat(120); // ~34 tokens per message
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        say(&long),
        say(&long),
        say("turn one asked about the weather; turn two about the news"), // the rollover call
        say("third"),
    ])));
    let agent = Agent::builder(cfg(), s.clone()).provider(rec.clone()).build().unwrap();
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
    let r3 = agent
        .run(
            "three",
            RunOptions {
                session_id: sid,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(r3.output, "third", "{}", r3.error);

    let interactions = &rec.cassette().interactions;
    assert_eq!(interactions.len(), 4, "one, two, the rollover call, then three");
    let final_msgs = &interactions[3].request.messages;
    let summary_msg = final_msgs
        .iter()
        .find(|m| m.content.contains("[Earlier conversation summary]"))
        .unwrap_or_else(|| panic!("no summary message in {final_msgs:?}"));
    assert!(
        summary_msg.content.contains("turn one asked about the weather"),
        "{}",
        summary_msg.content
    );
    assert_eq!(final_msgs[0].role, Role::User, "the summary note comes first");
    assert!(
        !final_msgs.iter().any(|m| m.content == "one"),
        "the dropped turn's raw text should not be resent: {final_msgs:?}"
    );

    // The dropped turn is gone from what's SENT to the model, but never
    // from the Store: reading the conversation back directly still
    // shows it, same as before rollover existed.
    let stored = s.messages(&r3.conversation_id).await.unwrap();
    assert!(
        stored.iter().any(|m| m.content == "one"),
        "the dropped turn must still be in the Store: {stored:?}"
    );
}

// With rollover off (the default), a dropped turn is just gone, exactly
// as it always was -- no extra provider call, no summary.
#[tokio::test]
async fn without_rollover_a_dropped_turn_is_just_gone_as_before() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.rollover_enabled = false;
    let long = "x".repeat(120);
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
    let interactions = &rec.cassette().interactions;
    assert_eq!(interactions.len(), 3, "no extra rollover call made");
    let final_msgs = &interactions[2].request.messages;
    assert!(
        !final_msgs
            .iter()
            .any(|m| m.content.contains("[Earlier conversation summary]")),
        "{final_msgs:?}"
    );
}

// A custom rollover.prompt replaces the built-in summarization
// instruction.
#[tokio::test]
async fn a_custom_rollover_prompt_replaces_the_built_in_one() {
    let (s, _d) = store().await;
    let mut c = cfg();
    c.rollover_prompt = Some("CUSTOM INSTRUCTION".into());
    let long = "x".repeat(120);
    let rec = Arc::new(RecordingProvider::new(FakeProvider::new(vec![
        say(&long),
        say(&long),
        say("summary"),
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
    let interactions = &rec.cassette().interactions;
    let rollover_req = &interactions[2].request;
    assert!(
        rollover_req.messages[0].content.contains("CUSTOM INSTRUCTION"),
        "{:?}",
        rollover_req.messages
    );
}
