//! `agen-host eval` as a real process: mocked tools stand in for a real MCP
//! server, rule checks and the LLM-judge rubric score each run, and
//! pass@1/pass^k aggregate across repeated runs of one scenario.

use std::path::Path;
use std::process::Command;

use serde_json::{json, Value};

const HOST: &str = env!("CARGO_BIN_EXE_agen-host");

fn test_server() -> String {
    agen_testkit::bin_path("agen-testkit", "mcp-test-server")
        .to_str()
        .unwrap()
        .to_string()
}

fn store_url(dir: &Path) -> String {
    format!(
        "sqlite:{}",
        dir.join("agen.db").display().to_string().replace('\\', "/")
    )
}

fn write_bundle(dir: &Path, mcp: Option<Value>, script: Value) {
    std::fs::create_dir_all(dir.join("x-agen")).unwrap();
    std::fs::write(dir.join("plugin.json"), r#"{"name":"evaluee"}"#).unwrap();
    std::fs::write(
        dir.join("x-agen/agent.md"),
        "---\nname: evaluee\ndescription: eval tests\n---\nDo the task.\n",
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/harness.json"),
        r#"{"provider":"fake","model":"fake-1","script":"x-agen/script.json"}"#,
    )
    .unwrap();
    std::fs::write(dir.join("x-agen/script.json"), script.to_string()).unwrap();
    std::fs::write(dir.join("x-agen/config.json"), r#"{"permissions":{"default":"allow"}}"#).unwrap();
    if let Some(m) = mcp {
        std::fs::write(dir.join("mcp.json"), m.to_string()).unwrap();
    }
}

fn eval(bundle: &Path, scenario: &Path, runs: u32, store: &Path) -> (bool, Value, String) {
    let out = Command::new(HOST)
        .args([
            "eval",
            bundle.to_str().unwrap(),
            "--scenario",
            scenario.to_str().unwrap(),
            "--runs",
            &runs.to_string(),
            "--store",
            &store_url(store),
        ])
        .output()
        .unwrap();
    let stderr = String::from_utf8_lossy(&out.stderr).to_string();
    let report: Value = serde_json::from_slice(&out.stdout).unwrap_or_else(|e| {
        panic!(
            "report did not parse as JSON: {e}\nstdout: {}\nstderr: {stderr}",
            String::from_utf8_lossy(&out.stdout)
        )
    });
    (out.status.success(), report, stderr)
}

// A scenario's mockedTools entry stands in for a real tool: the model sees
// the real tool's spec (learned by briefly connecting to the test server)
// but the call returns the scenario's canned value, never the server's
// real "echo: <text>" response. Rule checks and the rubric's LLM-judge
// call both run against the resulting output.
#[test]
fn eval_scores_mocked_tool_rules_and_rubric() {
    let dir = tempfile::tempdir().unwrap();
    let bundle = dir.path().join("bundle");
    write_bundle(
        &bundle,
        Some(json!({"mcpServers": {"test": {"command": test_server()}}})),
        json!({"responses": [
            {"toolCalls": [{"name": "test.echo", "arguments": {"text": "ping"}}]},
            {"text": "Echo says: MOCKED-VALUE. All done.", "expect": "MOCKED-VALUE"},
            {"text": "{\"score\": 0.9, \"reasons\": \"it reports what the tool returned\"}"}
        ]}),
    );
    let scenario = dir.path().join("happy.json");
    std::fs::write(
        &scenario,
        json!({
            "input": "Use the echo tool and tell me what it returned.",
            "mockedTools": {"test.echo": "MOCKED-VALUE"},
            "rules": {"mustContain": ["MOCKED-VALUE"], "mustNotContain": ["echo: ping"]},
            "rubric": {"criteria": ["mentions what the echo tool returned"], "passThreshold": 0.5}
        })
        .to_string(),
    )
    .unwrap();

    let (ok, report, stderr) = eval(&bundle, &scenario, 1, dir.path());
    assert!(ok, "eval should exit 0: {report}\nstderr: {stderr}");
    assert_eq!(report["scenario"], "happy");
    assert_eq!(report["pass1"], true, "{report}");
    assert_eq!(report["passK"], true, "{report}");
    let run = &report["runs"][0];
    assert_eq!(run["rules"]["pass"], true, "{run}");
    assert_eq!(run["rubric"]["pass"], true, "{run}");
    assert!((run["rubric"]["score"].as_f64().unwrap() - 0.9).abs() < 1e-9, "{run}");
    assert!(run["output"].as_str().unwrap().contains("MOCKED-VALUE"));
}

// Calling a tool the scenario did not mock is a clear error, not a real
// call: agen eval never reaches a live server.
#[test]
fn eval_errors_clearly_on_an_unmocked_tool_call() {
    let dir = tempfile::tempdir().unwrap();
    let bundle = dir.path().join("bundle");
    write_bundle(
        &bundle,
        Some(json!({"mcpServers": {"test": {"command": test_server()}}})),
        json!({"responses": [
            {"toolCalls": [{"name": "test.whoami", "arguments": {}}]},
            {"text": "sorry, I could not look that up", "expect": "agen eval:"}
        ]}),
    );
    let scenario = dir.path().join("unmocked.json");
    std::fs::write(
        &scenario,
        json!({
            "input": "Who am I?",
            "mockedTools": {"test.echo": "irrelevant"}
        })
        .to_string(),
    )
    .unwrap();

    let (ok, report, stderr) = eval(&bundle, &scenario, 1, dir.path());
    assert!(
        ok,
        "the run itself succeeds (the agent handles the tool error in text): {report}\nstderr: {stderr}"
    );
    assert!(
        report["runs"][0]["output"].as_str().unwrap().contains("could not look"),
        "{report}"
    );
}

// pass@1/pass^k aggregate correctly across repeated runs of one scenario;
// a scenario with no mcp.json and no rubric needs neither a tool
// connection nor a judge call.
#[test]
fn eval_aggregates_pass1_and_passk_over_repeated_runs() {
    let dir = tempfile::tempdir().unwrap();
    let bundle = dir.path().join("bundle");
    write_bundle(&bundle, None, json!({"responses": [{"text": "ok"}], "cycle": true}));
    let scenario = dir.path().join("repeat.json");
    std::fs::write(
        &scenario,
        json!({"input": "say ok", "rules": {"mustContain": ["ok"]}}).to_string(),
    )
    .unwrap();

    let (ok, report, stderr) = eval(&bundle, &scenario, 3, dir.path());
    assert!(ok, "{report}\nstderr: {stderr}");
    assert_eq!(report["runs"].as_array().unwrap().len(), 3, "{report}");
    assert_eq!(report["pass1"], true, "{report}");
    assert_eq!(report["passK"], true, "{report}");
    for run in report["runs"].as_array().unwrap() {
        assert_eq!(run["pass"], true, "{run}");
    }
}
