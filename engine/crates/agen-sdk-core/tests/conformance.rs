//! Reference runner for the shared SDK conformance suite
//! (spec/conformance/cases): the same fixtures every SDK must pass, driven
//! directly against agen-sdk-core's host callback interface.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use agen_sdk_core::{EventCallback, HostCallback, SdkAgent};
use serde_json::{json, Value};

const CASE_KEYS: &[&str] = &[
    "name",
    "description",
    "agent",
    "tools",
    "approvals",
    "runs",
    "expect",
    "createError",
];
const AGENT_KEYS: &[&str] = &[
    "name",
    "bundle",
    "instructions",
    "model",
    "provider",
    "permissions",
    "maxTurns",
    "approvalTimeoutSeconds",
];
const TOOL_KEYS: &[&str] = &["name", "readOnly", "timeoutSeconds", "behavior"];
const RUN_KEYS: &[&str] = &[
    "input",
    "options",
    "sameSession",
    "cancelAfterMs",
    "cancelBeforeStart",
    "closeAfterMs",
    "concurrent",
];
const OPTION_KEYS: &[&str] = &["taskId"];
const EXPECT_KEYS: &[&str] = &[
    "status",
    "output",
    "minDeltas",
    "deltasEqualOutput",
    "toolCalls",
    "approvalsAsked",
    "toolCancelled",
    "sameConversationAsPrevious",
    "sameRunAsPrevious",
    "maxWallMs",
    "traceIncludes",
];

/// Fail on keys the runner does not understand, so no fixture is half-checked.
fn strict(v: &Value, allowed: &[&str], what: &str) {
    if let Some(o) = v.as_object() {
        for k in o.keys() {
            assert!(
                allowed.contains(&k.as_str()),
                "{what}: unknown key {k:?} (update the runner)"
            );
        }
    }
}

fn root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../..")
}

#[derive(Default)]
struct State {
    calls: Vec<String>,
    approvals: Vec<String>,
    tool_cancelled: bool,
    // request id -> channel that fires when the host is told to cancel
    cancels: HashMap<String, mpsc::Sender<()>>,
}

type Shared = Arc<Mutex<State>>;
type AgentSlot = Arc<Mutex<Option<Arc<SdkAgent>>>>;

fn host(case: &Value, state: Shared, slot: AgentSlot) -> HostCallback {
    let tools: HashMap<String, Value> = case["tools"]
        .as_array()
        .cloned()
        .unwrap_or_default()
        .into_iter()
        .map(|t| (t["name"].as_str().unwrap().to_string(), t["behavior"].clone()))
        .collect();
    let approve = match case["approvals"].as_str() {
        Some("approve") => Some(true),
        Some("deny") => Some(false),
        Some("hang") | None => None,
        Some(other) => panic!("unknown approvals mode {other}"),
    };
    Arc::new(move |req: String| {
        let req: Value = serde_json::from_str(&req).unwrap();
        let id = req["id"].as_str().unwrap().to_string();
        match req["kind"].as_str().unwrap() {
            "cancel" => {
                if let Some(tx) = state.lock().unwrap().cancels.remove(&id) {
                    let _ = tx.send(());
                }
            }
            "approval" => {
                state
                    .lock()
                    .unwrap()
                    .approvals
                    .push(req["tool"].as_str().unwrap().to_string());
                if approve.is_some() {
                    let a = slot.lock().unwrap().clone().unwrap();
                    a.complete(
                        &id,
                        Ok(if approve == Some(true) { "approved" } else { "denied" }.into()),
                    );
                } // "hang": never answer
            }
            "tool" => {
                let name = req["name"].as_str().unwrap().to_string();
                state.lock().unwrap().calls.push(name.clone());
                let b = tools[&name].clone();
                let (tx, rx) = mpsc::channel();
                state.lock().unwrap().cancels.insert(id.clone(), tx);
                let (state, slot, args) = (state.clone(), slot.clone(), req["arguments"].clone());
                // Answer from another thread, like a host language would.
                std::thread::spawn(move || {
                    let result: Result<String, String> = if let Some(v) = b.get("return") {
                        Ok(v.as_str().map(String::from).unwrap_or_else(|| v.to_string()))
                    } else if b.get("echoArgs").is_some() {
                        Ok(args.to_string())
                    } else if let Some(e) = b.get("error") {
                        Err(e.as_str().unwrap().to_string())
                    } else if let Some(ms) = b.get("sleepMs") {
                        if rx.recv_timeout(Duration::from_millis(ms.as_u64().unwrap())).is_ok() {
                            state.lock().unwrap().tool_cancelled = true;
                        }
                        Ok("slept".into())
                    } else if b.get("nonJson").is_some() {
                        Err("value is not JSON".into())
                    } else {
                        Err("unknown behavior".into())
                    };
                    state.lock().unwrap().cancels.remove(&id);
                    if let Some(a) = slot.lock().unwrap().clone() {
                        a.complete(&id, result);
                    }
                });
            }
            other => panic!("unexpected request kind {other}"),
        }
    })
}

fn run_once(agent: &Arc<SdkAgent>, run: &Value, prev: Option<&Value>, key: String) -> (Value, Vec<String>) {
    strict(&run["options"], OPTION_KEYS, "options");
    let mut opts = json!({"cancelKey": key});
    if let Some(t) = run["options"]["taskId"].as_str() {
        opts["taskId"] = json!(t);
    }
    if run["sameSession"].as_bool() == Some(true) {
        opts["sessionId"] = prev.unwrap()["sessionId"].clone();
    }
    if run["cancelBeforeStart"].as_bool() == Some(true) {
        agent.cancel(&key);
    }
    if let Some(ms) = run["closeAfterMs"].as_u64() {
        let a = agent.clone();
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(ms));
            a.shutdown();
        });
    }
    if let Some(ms) = run["cancelAfterMs"].as_u64() {
        let (a, k) = (agent.clone(), key.clone());
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(ms));
            a.cancel(&k);
        });
    }
    let deltas = Arc::new(Mutex::new(Vec::<String>::new()));
    let d = deltas.clone();
    let on_event: EventCallback = Arc::new(move |ev: String| {
        let ev: Value = serde_json::from_str(&ev).unwrap();
        match ev["type"].as_str() {
            Some("delta") => d.lock().unwrap().push(ev["text"].as_str().unwrap().to_string()),
            Some("reset") => d.lock().unwrap().clear(),
            _ => {}
        }
    });
    let out = agent
        .run_blocking(run["input"].as_str().unwrap(), &opts.to_string(), Some(on_event))
        .unwrap();
    let deltas = deltas.lock().unwrap().clone();
    (serde_json::from_str(&out).unwrap(), deltas)
}

#[allow(clippy::too_many_arguments)] // test helper mirrors the fixture fields
fn check(
    agent: &SdkAgent,
    name: &str,
    exp: &Value,
    result: &(Value, Vec<String>),
    prev: Option<&Value>,
    state: &Shared,
    before: (usize, usize),
    single: bool,
) {
    let (got, deltas) = (&result.0, &result.1);
    assert_eq!(got["status"], exp["status"], "{name}: {got}");
    if let Some(o) = exp.get("output") {
        assert_eq!(&got["output"], o, "{name}");
    }
    if let Some(n) = exp["minDeltas"].as_u64() {
        assert!(deltas.len() as u64 >= n, "{name}: {deltas:?}");
    }
    if exp["deltasEqualOutput"].as_bool() == Some(true) {
        assert_eq!(deltas.concat(), got["output"].as_str().unwrap(), "{name}");
    }
    let s = state.lock().unwrap();
    if let (Some(tc), true) = (exp.get("toolCalls"), single) {
        let want: Vec<String> = serde_json::from_value(tc.clone()).unwrap();
        assert_eq!(s.calls[before.0..].to_vec(), want, "{name}");
    }
    if let Some(ap) = exp.get("approvalsAsked") {
        let want: Vec<String> = serde_json::from_value(ap.clone()).unwrap();
        assert_eq!(s.approvals[before.1..].to_vec(), want, "{name}");
    }
    drop(s);
    if exp["toolCancelled"].as_bool() == Some(true) {
        let mut ok = false;
        for _ in 0..30 {
            if state.lock().unwrap().tool_cancelled {
                ok = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        assert!(ok, "{name}: tool never saw the cancel");
    }
    if exp["sameConversationAsPrevious"].as_bool() == Some(true) {
        assert_eq!(got["conversationId"], prev.unwrap()["conversationId"], "{name}");
    }
    if exp["sameRunAsPrevious"].as_bool() == Some(true) {
        assert_eq!(got["runId"], prev.unwrap()["runId"], "{name}");
    }
    if let Some(want) = exp["traceIncludes"].as_array() {
        let spans: Value =
            serde_json::from_str(&agent.trace_blocking(got["traceId"].as_str().unwrap()).unwrap()).unwrap();
        let names: Vec<&str> = spans
            .as_array()
            .unwrap()
            .iter()
            .map(|s| s["name"].as_str().unwrap())
            .collect();
        for w in want {
            assert!(
                names.contains(&w.as_str().unwrap()),
                "{name}: trace lacks {w}: {names:?}"
            );
        }
    }
}

#[test]
fn conformance_suite() {
    let dir = root().join("spec/conformance/cases");
    let mut files: Vec<PathBuf> = std::fs::read_dir(&dir).unwrap().flatten().map(|e| e.path()).collect();
    files.sort();
    assert!(files.len() >= 10, "expected conformance cases in {}", dir.display());
    let mut key = 0;
    for f in files {
        let case: Value = serde_json::from_str(&std::fs::read_to_string(&f).unwrap()).unwrap();
        let name = case["name"].as_str().unwrap().to_string();
        strict(&case, CASE_KEYS, "case");
        strict(&case["agent"], AGENT_KEYS, "agent");
        for t in case["tools"].as_array().cloned().unwrap_or_default() {
            strict(&t, TOOL_KEYS, "tool");
        }
        let mut spec = case["agent"].clone();
        if let Some(b) = spec["bundle"].as_str() {
            spec["bundle"] = json!(root().join(b).display().to_string());
        }
        spec["tools"] = json!(case["tools"]
            .as_array()
            .cloned()
            .unwrap_or_default()
            .iter()
            .map(|t| {
                let mut spec = json!({"name": t["name"], "sideEffect": !t["readOnly"].as_bool().unwrap_or(false)});
                if let Some(ts) = t.get("timeoutSeconds") {
                    spec["timeoutSeconds"] = ts.clone();
                }
                spec
            })
            .collect::<Vec<_>>());
        spec["hostApprovals"] = json!(case["approvals"].is_string());
        let state: Shared = Arc::default();
        let slot: AgentSlot = Arc::default();
        let created = SdkAgent::create(&spec.to_string(), Some(host(&case, state.clone(), slot.clone())));
        if let Some(code) = case["createError"].as_str() {
            assert_eq!(created.err().map(|e| e.code()), Some(code), "{name}");
            continue;
        }
        let agent = Arc::new(created.unwrap_or_else(|e| panic!("{name}: {e}")));
        *slot.lock().unwrap() = Some(agent.clone());
        let runs = case["runs"].as_array().unwrap();
        let expects = case["expect"].as_array().unwrap();
        assert_eq!(runs.len(), expects.len(), "{name}");
        let mut prev: Option<Value> = None;
        for (run, exp) in runs.iter().zip(expects) {
            strict(run, RUN_KEYS, "run");
            strict(exp, EXPECT_KEYS, "expect");
            let n = run["concurrent"].as_u64().unwrap_or(1) as usize;
            let started = std::time::Instant::now();
            let before = {
                let s = state.lock().unwrap();
                (s.calls.len(), s.approvals.len())
            };
            let handles: Vec<_> = (0..n)
                .map(|_| {
                    key += 1;
                    let (a, r, p, k) = (agent.clone(), run.clone(), prev.clone(), format!("ref-{key}"));
                    std::thread::spawn(move || run_once(&a, &r, p.as_ref(), k))
                })
                .collect();
            let results: Vec<(Value, Vec<String>)> = handles.into_iter().map(|h| h.join().unwrap()).collect();
            if let Some(max) = exp["maxWallMs"].as_u64() {
                let took = started.elapsed().as_millis() as u64;
                assert!(took <= max, "{name}: took {took} ms > {max} ms");
            }
            for result in &results {
                check(&agent, &name, exp, result, prev.as_ref(), &state, before, n == 1);
            }
            prev = Some(results.last().unwrap().0.clone());
        }
        *slot.lock().unwrap() = None;
    }
}
