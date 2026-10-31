//! `agen eval`: run a bundle against a scenario file, with its tools
//! mocked, and score each run with rule checks and one LLM-judge call
//! (§6 learning and testing).
//!
//! Mocked tools are discovered by connecting to the bundle's own mcp.json
//! servers once, to learn their real tool specs (name, description,
//! parameters), and then never actually calling them: a mocked tool's call
//! returns the scenario's own canned response directly, and calling a tool
//! the scenario does not mock is an error — `agen eval` never makes a real
//! tool call, so repeated runs stay deterministic and side-effect free.
//!
//! Comparing a report against the deployed definition's own last recorded
//! eval, and `deploy --require-eval`, both need somewhere to persist eval
//! history per deployment (a new Hub RPC and storage, mirroring how
//! cassettes were added for `agen replay`) — a separately scoped addition,
//! not attempted here.

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;

use agen_engine::agent::{Agent, BundleOptions, RunOptions};
use agen_engine::bundle::Bundle;
use agen_engine::mcp::McpOptions;
use agen_engine::provider::{no_deltas, Message, ModelProvider, ModelRequest, ToolSpec};
use agen_engine::secrets::Redactor;
use agen_engine::store::{RunStatus, Store};
use agen_engine::tools::{Tool, ToolContext, ToolError};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::Common;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Scenario {
    input: String,
    #[serde(default)]
    mocked_tools: BTreeMap<String, Value>,
    #[serde(default)]
    rules: Rules,
    rubric: Option<Rubric>,
}

#[derive(Deserialize, Default)]
#[serde(rename_all = "camelCase")]
struct Rules {
    #[serde(default)]
    must_contain: Vec<String>,
    #[serde(default)]
    must_not_contain: Vec<String>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Rubric {
    criteria: Vec<String>,
    #[serde(default = "default_pass_threshold")]
    pass_threshold: f64,
}

fn default_pass_threshold() -> f64 {
    0.7
}

/// Stands in for a real MCP tool during eval: same spec (so the model sees
/// the tool exactly as it would in production), but the call returns the
/// scenario's canned response instead of reaching the real server.
struct MockTool {
    tool_spec: ToolSpec,
    response: Option<Value>,
    scenario: String,
}

#[async_trait]
impl Tool for MockTool {
    fn spec(&self) -> ToolSpec {
        self.tool_spec.clone()
    }
    fn side_effect(&self) -> bool {
        false
    }
    async fn call(&self, _args: Value, _ctx: ToolContext) -> Result<String, ToolError> {
        match &self.response {
            Some(v) => Ok(serde_json::to_string(v).unwrap_or_default()),
            None => Err(ToolError::new(format!(
                "agen eval: {:?} was called but scenario {:?} has no mockedTools entry for it \
                 — agen eval never makes a real tool call",
                self.tool_spec.name, self.scenario
            ))),
        }
    }
}

/// Runs one scenario file `runs` times against `bundle_dir` and returns its
/// report: `{"scenario", "runs": [...], "pass1", "passK"}`.
pub async fn run_scenario(
    bundle_dir: &Path,
    scenario_path: &Path,
    runs: usize,
    store_url: &str,
    common: &Common,
) -> anyhow::Result<Value> {
    let scenario_name = scenario_path
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or("scenario")
        .to_string();
    let text =
        std::fs::read_to_string(scenario_path).map_err(|e| anyhow::anyhow!("{}: {e}", scenario_path.display()))?;
    let scenario: Scenario =
        serde_json::from_str(&text).map_err(|e| anyhow::anyhow!("{}: {e}", scenario_path.display()))?;

    let mut bundle = Bundle::load(bundle_dir)?;
    let secrets = agen_engine::secrets::resolve_all(&bundle.secrets, None).await?;
    let mut mocked_tools: Vec<Arc<dyn Tool>> = Vec::new();
    if let Some(m) = &bundle.mcp {
        let redactor = Redactor::new();
        redactor.register_all(&secrets);
        let (conns, tools) = agen_engine::mcp::connect(
            &bundle.root,
            &m.servers,
            &bundle.config.tools,
            &secrets,
            &redactor,
            &McpOptions::default(),
        )
        .await?;
        for t in tools {
            let spec = t.spec();
            mocked_tools.push(Arc::new(MockTool {
                response: scenario.mocked_tools.get(&spec.name).cloned(),
                tool_spec: spec,
                scenario: scenario_name.clone(),
            }));
        }
        // Connected only to learn the real tools' specs; agen eval never
        // calls them for real, so the connections (and the real server
        // processes) close right away.
        conns.close().await;
    }
    // The real servers are never reached during eval; mocked_tools above
    // (built from extra_tools) stands in for every tool they would expose.
    bundle.mcp = None;

    let store = Arc::new(Store::open(store_url).await?);
    let agent = Agent::from_bundle(
        &bundle,
        store,
        BundleOptions {
            namespace: Some(common.namespace.clone()),
            deployment: common.deployment.clone(),
            owner: common.instance_id.clone(),
            extra_tools: mocked_tools,
            ..Default::default()
        },
    )
    .await?;

    let mut run_reports = Vec::with_capacity(runs);
    let mut pass1 = None;
    let mut pass_k = true;
    for _ in 0..runs.max(1) {
        let r = agent.run(&scenario.input, RunOptions::default()).await?;
        let mut failures = Vec::new();
        for needle in &scenario.rules.must_contain {
            if !r.output.contains(needle.as_str()) {
                failures.push(format!("expected the reply to contain {needle:?}"));
            }
        }
        for needle in &scenario.rules.must_not_contain {
            if r.output.contains(needle.as_str()) {
                failures.push(format!("expected the reply to not contain {needle:?}"));
            }
        }
        if r.status != RunStatus::Succeeded {
            failures.push(format!("run did not succeed: {} {}", r.status.as_str(), r.error));
        }
        let rules_pass = failures.is_empty();

        let judge = match &scenario.rubric {
            Some(rubric) if r.status == RunStatus::Succeeded => Some(
                judge_run(
                    agent.provider(),
                    &bundle.harness.model,
                    &scenario.input,
                    &r.output,
                    rubric,
                )
                .await?,
            ),
            Some(_) => Some(json!({"score": 0.0, "pass": false, "reasons": "the run did not succeed"})),
            None => None,
        };
        let judge_pass = judge
            .as_ref()
            .map(|j| j["pass"].as_bool().unwrap_or(false))
            .unwrap_or(true);
        let pass = rules_pass && judge_pass;
        pass1.get_or_insert(pass);
        pass_k = pass_k && pass;
        run_reports.push(json!({
            "status": r.status.as_str(),
            "output": r.output,
            "rules": {"pass": rules_pass, "failures": failures},
            "rubric": judge,
            "pass": pass,
        }));
    }
    Ok(json!({
        "scenario": scenario_name,
        "runs": run_reports,
        "pass1": pass1.unwrap_or(false),
        "passK": pass_k,
    }))
}

/// One extra model call grading the run's output against the rubric's
/// criteria; reuses the agent's own provider rather than building a second.
async fn judge_run(
    provider: &Arc<dyn ModelProvider>,
    model: &str,
    input: &str,
    output: &str,
    rubric: &Rubric,
) -> anyhow::Result<Value> {
    let criteria = rubric
        .criteria
        .iter()
        .map(|c| format!("- {c}"))
        .collect::<Vec<_>>()
        .join("\n");
    let prompt = format!(
        "Score how well the agent's reply meets EACH criterion below, from 0 (fails) to 1 \
         (fully meets). Report the LOWEST of the per-criterion scores as the overall score — \
         one weak criterion fails the whole reply. Respond with ONLY a JSON object: \
         {{\"score\": <0-1 number>, \"reasons\": \"<one sentence>\"}}.\n\n\
         Criteria:\n{criteria}\n\n\
         Request given to the agent:\n{input}\n\n\
         Agent's final reply:\n{output}"
    );
    let req = ModelRequest {
        model: model.to_string(),
        system: "You are a strict, concise evaluation judge.".into(),
        messages: vec![Message::user(prompt)],
        tools: vec![],
        temperature: None,
        max_output_tokens: Some(300),
        extra: Default::default(),
    };
    let resp = provider.complete(&req, &no_deltas).await?;
    let parsed: Option<Value> = match (resp.text.find('{'), resp.text.rfind('}')) {
        (Some(s), Some(e)) if e >= s => serde_json::from_str(&resp.text[s..=e]).ok(),
        _ => None,
    };
    let score = parsed.as_ref().and_then(|v| v["score"].as_f64()).unwrap_or(0.0);
    let reasons = parsed
        .as_ref()
        .and_then(|v| v["reasons"].as_str())
        .map(str::to_string)
        .unwrap_or_else(|| format!("could not parse the judge's response: {}", resp.text));
    Ok(json!({"score": score, "pass": score >= rubric.pass_threshold, "reasons": reasons}))
}
