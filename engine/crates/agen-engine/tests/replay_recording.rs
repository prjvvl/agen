//! Opt-in interaction recording (3.6, part: `agen replay`): a bundle
//! whose config.json sets `recordInteractions` has its real provider
//! wrapped in a `RecordingProvider`, and once a run ends, its cassette is
//! saved to the Store — all through `Agent::from_bundle`, the same path
//! a managed instance uses, not constructed by hand.

use std::sync::Arc;

use agen_engine::agent::{Agent, BundleOptions, RunOptions};
use agen_engine::bundle::Bundle;
use agen_engine::store::{RunStatus, Store};

fn write_fake_bundle(dir: &std::path::Path, record_interactions: bool) {
    std::fs::create_dir_all(dir.join("x-agen")).unwrap();
    std::fs::write(dir.join("plugin.json"), r#"{"name":"greeter"}"#).unwrap();
    std::fs::write(
        dir.join("x-agen/agent.md"),
        "---\nname: greeter\ndescription: says hi\n---\nGreet the user.\n",
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/harness.json"),
        r#"{"provider":"fake","model":"fake-1","script":"x-agen/fake-script.json"}"#,
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/fake-script.json"),
        r#"{"responses":[{"text":"hello there"}]}"#,
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/config.json"),
        format!(r#"{{"permissions":{{"default":"allow"}},"recordInteractions":{record_interactions}}}"#),
    )
    .unwrap();
}

#[tokio::test]
async fn a_run_of_a_recording_deployment_saves_its_cassette() {
    let dir = tempfile::tempdir().unwrap();
    write_fake_bundle(dir.path(), true);
    let bundle = Bundle::load(dir.path()).unwrap();
    let store = Arc::new(Store::open_sqlite_path(&dir.path().join("s.db")).await.unwrap());
    let agent = Agent::from_bundle(&bundle, store.clone(), BundleOptions::default())
        .await
        .unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);

    let cassette = store.get_cassette(&r.run_id).await.unwrap();
    let data = cassette.expect("a recording deployment's run should have saved a cassette");
    assert!(data.contains("hello there"), "{data}");
    assert!(data.contains("\"fingerprint\""), "{data}");
}

#[tokio::test]
async fn a_run_of_a_non_recording_deployment_saves_no_cassette() {
    let dir = tempfile::tempdir().unwrap();
    write_fake_bundle(dir.path(), false);
    let bundle = Bundle::load(dir.path()).unwrap();
    let store = Arc::new(Store::open_sqlite_path(&dir.path().join("s.db")).await.unwrap());
    let agent = Agent::from_bundle(&bundle, store.clone(), BundleOptions::default())
        .await
        .unwrap();
    let r = agent.run("hi", RunOptions::default()).await.unwrap();
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);

    assert!(
        store.get_cassette(&r.run_id).await.unwrap().is_none(),
        "recordInteractions off (the default) should never save a cassette"
    );
}
