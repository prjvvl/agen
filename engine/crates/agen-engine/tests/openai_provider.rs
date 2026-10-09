//! OpenAI-compatible provider against a local mock server, plus an opt-in
//! live OpenRouter smoke test (AGEN_LIVE_TESTS=1, cheap model, tiny output).

use std::sync::{Arc, Mutex};

use agen_engine::agent::{Agent, AgentConfig, BundleOptions, RunOptions};
use agen_engine::bundle::Bundle;
use agen_engine::provider::openai::OpenAiCompatible;
use agen_engine::provider::{no_deltas, Message, ModelProvider, ModelRequest, ProviderError, ToolSpec};
use agen_engine::store::{RunStatus, Store};
use agen_engine::tools::{FnTool, ToolError};
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::routing::post;
use axum::{Json, Router};
use serde_json::{json, Value};

/// (Authorization header, request body) per request.
type Seen = Arc<Mutex<Vec<(Option<String>, Value)>>>;

#[derive(Clone, Default)]
struct Mock {
    seen: Seen,
    // Queue of (status, SSE body) responses.
    replies: Arc<Mutex<Vec<(u16, String)>>>,
}

fn sse(chunks: &[Value]) -> String {
    let mut s = String::from(": OPENROUTER PROCESSING\n\n");
    for c in chunks {
        s.push_str(&format!("data: {c}\n\n"));
    }
    s.push_str("data: [DONE]\n\n");
    s
}

async fn handler(State(m): State<Mock>, headers: HeaderMap, Json(body): Json<Value>) -> impl IntoResponse {
    let auth = headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .map(String::from);
    m.seen.lock().unwrap().push((auth, body));
    let (status, text) = m.replies.lock().unwrap().remove(0);
    (
        StatusCode::from_u16(status).unwrap(),
        [("content-type", "text/event-stream")],
        text,
    )
}

async fn serve(m: Mock) -> String {
    let app = Router::new().route("/v1/chat/completions", post(handler)).with_state(m);
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(l, app).await.unwrap() });
    format!("http://{addr}/v1")
}

fn req() -> ModelRequest {
    ModelRequest {
        model: "deepseek/deepseek-v4-flash".into(),
        system: "s".into(),
        messages: vec![Message::user("hi")],
        tools: vec![],
        temperature: None,
        max_output_tokens: Some(16),
    }
}

#[tokio::test]
async fn streams_text_and_sends_auth() {
    let m = Mock::default();
    m.replies.lock().unwrap().push((
        200,
        sse(&[
            json!({"choices":[{"delta":{"content":"Hi "}}]}),
            json!({"choices":[{"delta":{"content":"there"}}]}),
            json!({"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"cost":0.00001}}),
        ]),
    ));
    let url = serve(m.clone()).await;
    let p = OpenAiCompatible::new("openrouter", &url, "test-key").unwrap();
    let got = Mutex::new(String::new());
    let sink = |d: &str| got.lock().unwrap().push_str(d);
    let r = p.complete(&req(), &sink).await.unwrap();
    assert_eq!(r.text, "Hi there");
    assert_eq!(*got.lock().unwrap(), "Hi there");
    assert_eq!(r.usage.output_tokens, 2);
    let seen = m.seen.lock().unwrap();
    assert_eq!(seen[0].0.as_deref(), Some("Bearer test-key"));
    assert_eq!(seen[0].1["model"], "deepseek/deepseek-v4-flash");
    assert_eq!(seen[0].1["stream"], true);
}

#[tokio::test]
async fn http_errors_are_classified() {
    let m = Mock::default();
    m.replies.lock().unwrap().push((429, "rate limited".into()));
    m.replies.lock().unwrap().push((401, "bad key".into()));
    let url = serve(m).await;
    let p = OpenAiCompatible::new("openai", &url, "k").unwrap();
    assert!(matches!(
        p.complete(&req(), &no_deltas).await,
        Err(ProviderError::Retryable(_))
    ));
    assert!(matches!(
        p.complete(&req(), &no_deltas).await,
        Err(ProviderError::Request(_))
    ));
}

#[tokio::test]
async fn agent_runs_tool_loop_through_bundle_configured_provider() {
    let m = Mock::default();
    m.replies.lock().unwrap().push((200, sse(&[json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"math_add","arguments":"{\"a\":2,"}}]}}]}), json!({"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"b\":3}"}}]}}]})])));
    m.replies
        .lock()
        .unwrap()
        .push((200, sse(&[json!({"choices":[{"delta":{"content":"5"}}]})])));
    let url = serve(m.clone()).await;

    // Bundle points at the mock via baseUrl; key comes from a named env secret.
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path();
    std::fs::create_dir_all(b.join("x-agen")).unwrap();
    std::fs::write(b.join("plugin.json"), r#"{"name":"calc"}"#).unwrap();
    std::fs::write(
        b.join("x-agen/agent.md"),
        "---\nname: calc\ndescription: adds\n---\nAdd numbers.\n",
    )
    .unwrap();
    std::fs::write(
        b.join("x-agen/harness.json"),
        format!(r#"{{"provider":"openai","model":"gpt-x","baseUrl":"{url}","apiKeySecret":"AGEN_TEST_MOCK_KEY"}}"#),
    )
    .unwrap();
    std::fs::write(b.join("x-agen/config.json"), r#"{"permissions":{"default":"allow"}}"#).unwrap();
    std::env::set_var("AGEN_TEST_MOCK_KEY", "mock-secret-key-123");
    let bundle = Bundle::load(b).unwrap();
    let store = Arc::new(Store::open_sqlite_path(&b.join("s.db")).await.unwrap());
    let add = Arc::new(FnTool::new(
        "math.add",
        "Add",
        json!({"type":"object"}),
        false,
        |a: Value, _| async move { Ok::<_, ToolError>((a["a"].as_i64().unwrap() + a["b"].as_i64().unwrap()).to_string()) },
    ));
    let agent = Agent::from_bundle(
        &bundle,
        store.clone(),
        BundleOptions {
            extra_tools: vec![add],
            ..Default::default()
        },
    )
    .await
    .unwrap();
    let r = agent.run("2+3?", RunOptions::default()).await.unwrap();
    assert_eq!(
        (r.status, r.output.as_str()),
        (RunStatus::Succeeded, "5"),
        "{}",
        r.error
    );
    let seen = m.seen.lock().unwrap();
    assert_eq!(seen[0].0.as_deref(), Some("Bearer mock-secret-key-123"));
    assert_eq!(seen[0].1["tools"][0]["function"]["name"], "math_add");
    let second = &seen[1].1["messages"];
    assert_eq!(second[2]["tool_calls"][0]["function"]["name"], "math_add");
    assert_eq!(second[3]["content"], "5");
    // The API key is registered for redaction even though it was not in secrets.json.
    assert_eq!(
        agent.redactor().redact("x mock-secret-key-123"),
        "x [REDACTED:AGEN_TEST_MOCK_KEY]"
    );
}

async fn key_usage(key: &str) -> f64 {
    let v: Value = reqwest::Client::new()
        .get("https://openrouter.ai/api/v1/key")
        .bearer_auth(key)
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    v["data"]["usage"].as_f64().unwrap_or(0.0)
}

/// Live smoke test. Costs a fraction of a cent. Run with AGEN_LIVE_TESTS=1.
#[tokio::test]
async fn live_openrouter_tool_call_smoke() {
    if std::env::var("AGEN_LIVE_TESTS").as_deref() != Ok("1") {
        eprintln!("skip: set AGEN_LIVE_TESTS=1");
        return;
    }
    let key = std::env::var("OPENROUTER_API_KEY").expect("OPENROUTER_API_KEY");
    let before = key_usage(&key).await;
    let store = Arc::new(Store::open("sqlite::memory:").await.unwrap());
    let mut cfg = AgentConfig::new(
        "live",
        "You are terse. Use the add tool for arithmetic, then answer with just the number.",
        "deepseek/deepseek-v4-flash",
    );
    // Reasoning models spend output tokens before answering; keep headroom.
    cfg.max_output_tokens = Some(400);
    cfg.max_turns = 4;
    cfg.budget.max_tokens_per_run = Some(4000);
    let add = Arc::new(FnTool::new(
        "add",
        "Add two integers a and b",
        json!({"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}),
        false,
        |a: Value, _| async move {
            Ok::<_, ToolError>((a["a"].as_i64().unwrap_or(0) + a["b"].as_i64().unwrap_or(0)).to_string())
        },
    ));
    let p = Arc::new(OpenAiCompatible::openrouter(&key).unwrap());
    let agent = Agent::builder(cfg, store.clone())
        .provider(p)
        .tool(add)
        .build()
        .unwrap();
    let r = agent.run("What is 1234 + 4321?", RunOptions::default()).await.unwrap();
    eprintln!("live: status={:?} output={:?} usage={:?}", r.status, r.output, r.usage);
    for m in store.run_messages(&r.run_id).await.unwrap() {
        eprintln!("live msg: {}", serde_json::to_string(&m).unwrap());
    }
    assert_eq!(r.status, RunStatus::Succeeded, "{}", r.error);
    assert!(r.output.contains("5555"), "{}", r.output);
    let msgs = store.run_messages(&r.run_id).await.unwrap();
    assert!(
        msgs.iter().any(|m| m.tool_call_id.is_some() && m.content == "5555"),
        "model did not use the tool"
    );
    let _ = ToolSpec {
        name: String::new(),
        description: String::new(),
        parameters: json!({}),
    };
    tokio::time::sleep(std::time::Duration::from_secs(2)).await;
    let after = key_usage(&key).await;
    eprintln!("live: key usage ${before:.6} -> ${after:.6}");
}
