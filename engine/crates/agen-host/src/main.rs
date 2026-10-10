//! `agen-host`: runs one Agen agent instance.
//!
//! - `agen-host run <bundle> --input "..."`: run once, streaming to stdout.
//! - `agen-host resume <run-id> <bundle>`: continue an unfinished run.
//! - `agen-host serve <bundle>`: managed mode. Serves the HostService control
//!   API (Connect JSON: `POST /agen.v1.HostService/<Method>`) for the Manager
//!   and prints `listening http://<addr>` on stdout once ready. Unfinished
//!   runs are resumed only when the Manager re-sends their task (RunTask is
//!   idempotent per task id).

use std::collections::HashMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use agen_engine::agent::{Agent, AgentError, BundleOptions, RunOptions, RunResult};
use agen_engine::bundle::Bundle;
use agen_engine::store::{RunStatus, Store, StoreError};
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use axum::{Json, Router};
use clap::{Parser, Subcommand};
use serde::Deserialize;
use serde_json::{json, Value};
use tokio_util::sync::CancellationToken;

#[derive(Parser)]
#[command(name = "agen-host", version, about = "Run one Agen agent instance")]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(clap::Args, Clone)]
struct Common {
    /// Store URL (sqlite:<path> or postgres://...). Default: ~/.agen/agen.db
    #[arg(long, env = "AGEN_STORE")]
    store: Option<String>,
    #[arg(long, default_value = "default")]
    namespace: String,
    /// Deployment name (default: the bundle name).
    #[arg(long)]
    deployment: Option<String>,
    /// Owner identity for runs (default: random per process).
    #[arg(long, env = "AGEN_INSTANCE_ID")]
    instance_id: Option<String>,
}

#[derive(Subcommand)]
enum Cmd {
    /// Run the agent once.
    Run {
        bundle: PathBuf,
        #[arg(long, short)]
        input: String,
        /// Continue this session.
        #[arg(long)]
        session: Option<String>,
        /// Use the deployment's single session (singleton agents).
        #[arg(long)]
        singleton: bool,
        /// Print the result as JSON instead of streaming text.
        #[arg(long)]
        json: bool,
        #[command(flatten)]
        common: Common,
    },
    /// Continue an unfinished run from its last checkpoint.
    Resume {
        run_id: String,
        bundle: PathBuf,
        #[arg(long)]
        json: bool,
        #[command(flatten)]
        common: Common,
    },
    /// Serve the HostService control API (managed mode).
    Serve {
        bundle: PathBuf,
        #[arg(long, default_value = "127.0.0.1:0")]
        listen: String,
        #[arg(long, default_value_t = 1)]
        max_concurrency: usize,
        /// Run every task in the deployment's single session.
        #[arg(long)]
        singleton: bool,
        #[command(flatten)]
        common: Common,
    },
}

fn default_store() -> anyhow::Result<String> {
    let home = std::env::var_os("USERPROFILE")
        .or_else(|| std::env::var_os("HOME"))
        .ok_or_else(|| anyhow::anyhow!("cannot find home directory; pass --store"))?;
    let dir = PathBuf::from(home).join(".agen");
    std::fs::create_dir_all(&dir)?;
    Ok(format!(
        "sqlite:{}",
        dir.join("agen.db").display().to_string().replace('\\', "/")
    ))
}

async fn build_agent(bundle_dir: &Path, common: &Common) -> anyhow::Result<Agent> {
    let mut bundle = Bundle::load(bundle_dir)?;
    // Managed mode: the definition the Manager unpacked. The directory can
    // hold files tool servers wrote since, which would change a recomputed digest.
    if let Some(d) = std::env::var("AGEN_DEFINITION_DIGEST").ok().filter(|d| !d.is_empty()) {
        bundle.digest = d;
    }
    let url = match &common.store {
        Some(u) => u.clone(),
        None => default_store()?,
    };
    let store = Arc::new(Store::open(&url).await?);
    let opts = BundleOptions {
        namespace: Some(common.namespace.clone()),
        deployment: common.deployment.clone(),
        owner: common.instance_id.clone(),
        // Managed mode: delegates are resolved and "ask" approvals are
        // raised through the Nest Manager.
        resolver: agen_engine::delegate::ManagerResolver::from_env()
            .map(|r| Arc::new(r) as Arc<dyn agen_engine::delegate::Resolver>),
        approver: agen_engine::managed::ManagerApprover::from_env(
            bundle
                .config
                .permissions
                .approval_timeout
                .as_deref()
                .and_then(agen_engine::bundle::parse_duration)
                .unwrap_or(std::time::Duration::from_secs(3600)),
        )
        .map(|a| Arc::new(a) as Arc<dyn agen_engine::agent::Approver>),
        // Platform secrets come from the Hub through the Manager.
        platform_secrets: agen_engine::managed::ManagerSecrets::from_env()
            .map(|s| Arc::new(s) as Arc<dyn agen_engine::secrets::PlatformSecrets>),
        ..Default::default()
    };
    Ok(Agent::from_bundle(&bundle, store, opts).await?)
}

fn result_json(r: &RunResult) -> Value {
    json!({
        "id": r.run_id,
        "sessionId": r.session_id,
        "conversationId": r.conversation_id,
        "status": r.status.as_str(),
        "output": r.output,
        "error": r.error,
        "usage": {"inputTokens": r.usage.input_tokens, "outputTokens": r.usage.output_tokens, "costUsd": r.usage.cost_usd},
        "traceId": r.trace_id,
    })
}

fn stream_options(json: bool, cancel: CancellationToken) -> RunOptions {
    let mut opts = RunOptions {
        cancel,
        ..Default::default()
    };
    if !json {
        opts.on_delta = Some(Arc::new(|d: &str| {
            print!("{d}");
            let _ = std::io::stdout().flush();
        }));
        opts.on_reset = Some(Arc::new(|| eprintln!("\n[agen-host] retrying model request")));
    }
    opts
}

fn cancel_on_ctrl_c() -> CancellationToken {
    let cancel = CancellationToken::new();
    let c = cancel.clone();
    tokio::spawn(async move {
        if tokio::signal::ctrl_c().await.is_ok() {
            c.cancel();
        }
    });
    cancel
}

fn finish(r: RunResult, json: bool) -> i32 {
    if json {
        println!("{}", result_json(&r));
    } else {
        println!();
        if r.status != RunStatus::Succeeded {
            eprintln!("[agen-host] run {} {}: {}", r.run_id, r.status.as_str(), r.error);
        }
    }
    match r.status {
        RunStatus::Succeeded => 0,
        RunStatus::Cancelled => 130,
        _ => 1,
    }
}

#[tokio::main]
async fn main() {
    let code = match real_main().await {
        Ok(code) => code,
        Err(e) => {
            eprintln!("agen-host: {e:#}");
            2
        }
    };
    std::process::exit(code);
}

async fn real_main() -> anyhow::Result<i32> {
    match Cli::parse().cmd {
        Cmd::Run {
            bundle,
            input,
            session,
            singleton,
            json,
            common,
        } => {
            let agent = build_agent(&bundle, &common).await?;
            let mut opts = stream_options(json, cancel_on_ctrl_c());
            opts.session_id = session;
            opts.singleton = singleton;
            let r = agent.run(&input, opts).await?;
            Ok(finish(r, json))
        }
        Cmd::Resume {
            run_id,
            bundle,
            json,
            common,
        } => {
            let agent = build_agent(&bundle, &common).await?;
            let r = agent.resume(&run_id, stream_options(json, cancel_on_ctrl_c())).await?;
            Ok(finish(r, json))
        }
        Cmd::Serve {
            bundle,
            listen,
            max_concurrency,
            singleton,
            common,
        } => {
            let agent = Arc::new(build_agent(&bundle, &common).await?);
            serve(agent, &listen, max_concurrency, singleton).await?;
            Ok(0)
        }
    }
}

// ---- managed mode: HostService over Connect JSON ----

struct HostState {
    agent: Arc<Agent>,
    running: Mutex<HashMap<String, CancellationToken>>,
    draining: AtomicBool,
    max_concurrency: usize,
    singleton: bool,
    shutdown: CancellationToken,
}

/// Removes a task from `running` when dropped, whatever happens to the run.
struct RunningGuard {
    state: Arc<HostState>,
    task_id: String,
}

impl Drop for RunningGuard {
    fn drop(&mut self) {
        self.state.running.lock().unwrap().remove(&self.task_id);
    }
}

/// Connect protocol error: HTTP status + `{"code", "message"}`.
fn connect_error(status: StatusCode, code: &str, message: impl Into<String>) -> Response {
    (status, Json(json!({"code": code, "message": message.into()}))).into_response()
}

/// Parse a Connect JSON request body, answering `invalid_argument` on error.
fn parse<T: serde::de::DeserializeOwned + Default>(body: &[u8]) -> Result<T, Box<Response>> {
    if body.iter().all(|b| b.is_ascii_whitespace()) {
        return Ok(T::default());
    }
    serde_json::from_slice(body).map_err(|e| {
        Box::new(connect_error(
            StatusCode::BAD_REQUEST,
            "invalid_argument",
            format!("invalid JSON body: {e}"),
        ))
    })
}

#[derive(Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
struct TaskIn {
    id: String,
    input: String,
    parent_run_id: String,
    root_run_id: String,
    traceparent: String,
    depth: u32,
    requested_by: String,
    /// The verified A2A caller ("agent:<ns>/<deployment>"), set by the
    /// Gateway: its lineage claims are checked against the Store.
    caller: String,
    conversation_key: String,
    labels: std::collections::BTreeMap<String, String>,
    attempts: u32,
}

#[derive(Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
struct RunTaskIn {
    task: TaskIn,
    traceparent: String,
}

#[derive(Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
struct CancelIn {
    task_id: String,
}

async fn serve(agent: Arc<Agent>, listen: &str, max_concurrency: usize, singleton: bool) -> anyhow::Result<()> {
    let state = Arc::new(HostState {
        agent,
        running: Mutex::new(HashMap::new()),
        draining: AtomicBool::new(false),
        // A singleton has one conversation; runs in it are serialised.
        max_concurrency: if singleton { 1 } else { max_concurrency.max(1) },
        singleton,
        shutdown: CancellationToken::new(),
    });
    let app = Router::new()
        .route("/agen.v1.HostService/Health", post(health))
        .route("/agen.v1.HostService/RunTask", post(run_task))
        .route("/agen.v1.HostService/CancelTask", post(cancel_task))
        .route("/agen.v1.HostService/Drain", post(drain))
        .with_state(state.clone())
        .layer(axum::middleware::from_fn(require_host_token));
    let listener = tokio::net::TcpListener::bind(listen).await?;
    let mut addr = listener.local_addr()?;
    if addr.ip().is_unspecified() {
        addr.set_ip(if addr.is_ipv4() {
            std::net::Ipv4Addr::LOCALHOST.into()
        } else {
            std::net::Ipv6Addr::LOCALHOST.into()
        });
    }
    println!("listening http://{addr}");
    std::io::stdout().flush()?;

    // Ctrl-C / SIGTERM: stop taking work, let running tasks finish, exit.
    let st = state.clone();
    tokio::spawn(async move {
        shutdown_signal().await;
        st.draining.store(true, Ordering::SeqCst);
        while !st.running.lock().unwrap().is_empty() {
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        }
        st.shutdown.cancel();
    });
    let s = state.shutdown.clone();
    axum::serve(listener, app)
        .with_graceful_shutdown(async move { s.cancelled().await })
        .await?;
    Ok(())
}

/// The Manager's credential for this host (`AGEN_HOST_TOKEN`). When it is
/// set, every HostService call must carry it as a bearer token, so only the
/// Manager that started this instance can drive it, whatever the network
/// lets through (e.g. pods on a CNI without NetworkPolicy).
fn host_token() -> Option<&'static str> {
    static TOKEN: std::sync::OnceLock<Option<String>> = std::sync::OnceLock::new();
    TOKEN
        .get_or_init(|| std::env::var("AGEN_HOST_TOKEN").ok().filter(|t| !t.is_empty()))
        .as_deref()
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

async fn require_host_token(req: axum::extract::Request, next: axum::middleware::Next) -> Response {
    if let Some(want) = host_token() {
        let got = req
            .headers()
            .get(axum::http::header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok())
            .and_then(|v| v.strip_prefix("Bearer "))
            .unwrap_or("");
        if !constant_time_eq(got.as_bytes(), want.as_bytes()) {
            return connect_error(
                StatusCode::UNAUTHORIZED,
                "unauthenticated",
                "missing or wrong host token",
            );
        }
    }
    next.run(req).await
}

/// Checks an agent caller's lineage claim against the Store: the parent run
/// must be a run of the calling deployment. Returns the root run and depth
/// derived from the parent chain (the caller's own claims are not used).
async fn verify_lineage(agent: &Agent, caller: &str, parent: &str) -> Result<(String, u32), String> {
    let (ns, dep) = caller
        .strip_prefix("agent:")
        .and_then(|s| s.split_once('/'))
        .ok_or_else(|| format!("caller {caller:?} is not an agent"))?;
    let store = agent.store();
    let p = store
        .get_run(parent)
        .await
        .map_err(|_| format!("parent run {parent} not found"))?;
    if p.namespace != ns || p.deployment != dep {
        return Err(format!("parent run {parent} is not a run of {ns}/{dep}"));
    }
    let root = if p.root_run_id.is_empty() {
        p.id.clone()
    } else {
        p.root_run_id.clone()
    };
    let mut depth = 1u32;
    let mut cur = p;
    while !cur.parent_run_id.is_empty() && depth < 64 {
        match store.get_run(&cur.parent_run_id).await {
            Ok(r) => {
                depth += 1;
                cur = r;
            }
            // An ancestor that cannot be read would undercount the depth.
            Err(e) => return Err(format!("cannot check the lineage of {parent}: {e}")),
        }
    }
    Ok((root, depth))
}

async fn shutdown_signal() {
    #[cfg(unix)]
    {
        let mut term =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()).expect("SIGTERM handler");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}

async fn health(State(st): State<Arc<HostState>>) -> Response {
    let running = st.running.lock().unwrap().len();
    let draining = st.draining.load(Ordering::SeqCst);
    let problems = st.agent.health_problems();
    let servers: Vec<Value> = st
        .agent
        .tool_servers()
        .into_iter()
        .map(|s| json!({"name": s.name, "state": if s.connected { "connected" } else { "disconnected" }, "toolCount": s.tool_count}))
        .collect();
    Json(json!({
        "instanceId": st.agent.owner(),
        "ready": !draining && problems.is_empty(),
        "runningTasks": running,
        "draining": draining,
        "problems": problems,
        "tools": st.agent.tools().specs().into_iter().map(|t| t.name).collect::<Vec<_>>(),
        "toolServers": servers,
    }))
    .into_response()
}

async fn run_task(State(st): State<Arc<HostState>>, body: axum::body::Bytes) -> Response {
    let req: RunTaskIn = match parse(&body) {
        Ok(r) => r,
        Err(e) => return *e,
    };
    if st.draining.load(Ordering::SeqCst) {
        return connect_error(StatusCode::SERVICE_UNAVAILABLE, "unavailable", "instance is draining");
    }
    let task_id = if req.task.id.is_empty() {
        agen_engine::store::new_id()
    } else {
        req.task.id.clone()
    };
    let cancel = CancellationToken::new();
    {
        let mut running = st.running.lock().unwrap();
        if running.contains_key(&task_id) {
            return connect_error(
                StatusCode::CONFLICT,
                "already_exists",
                format!("task {task_id} is already running"),
            );
        }
        if running.len() >= st.max_concurrency {
            return connect_error(
                StatusCode::TOO_MANY_REQUESTS,
                "resource_exhausted",
                "instance is at max concurrency",
            );
        }
        running.insert(task_id.clone(), cancel.clone());
    }
    let traceparent = if !req.traceparent.is_empty() {
        req.traceparent
    } else {
        req.task.traceparent
    };
    let (mut root_run_id, mut depth) = (req.task.root_run_id, req.task.depth);
    if !req.task.caller.is_empty() && !req.task.parent_run_id.is_empty() {
        match verify_lineage(&st.agent, &req.task.caller, &req.task.parent_run_id).await {
            Ok((root, d)) => (root_run_id, depth) = (root, d),
            Err(msg) => {
                st.running.lock().unwrap().remove(&task_id);
                return connect_error(StatusCode::FORBIDDEN, "permission_denied", msg);
            }
        }
        if let Some(max) = st.agent.config().limits.max_delegation_depth {
            if max > 0 && depth > max {
                st.running.lock().unwrap().remove(&task_id);
                return connect_error(
                    StatusCode::FORBIDDEN,
                    "permission_denied",
                    format!("delegation depth {depth} exceeds max_delegation_depth {max}"),
                );
            }
        }
    }
    let opts = RunOptions {
        singleton: st.singleton,
        task_id: task_id.clone(),
        parent_run_id: req.task.parent_run_id,
        root_run_id,
        depth,
        requested_by: req.task.requested_by,
        conversation_key: req.task.conversation_key,
        labels: req.task.labels,
        attempt: req.task.attempts,
        traceparent: (!traceparent.is_empty()).then_some(traceparent),
        cancel,
        ..Default::default()
    };
    // Run detached from the request: if the caller disconnects, the run
    // still finishes and its slot is released.
    let guard = RunningGuard {
        state: st.clone(),
        task_id,
    };
    let agent = st.agent.clone();
    let input = req.task.input;
    let handle = tokio::spawn(async move {
        let _guard = guard;
        agent.run(&input, opts).await
    });
    match handle.await {
        Ok(Ok(r)) => Json(json!({
            "success": r.status == RunStatus::Succeeded,
            "output": r.output,
            "error": r.error,
            "run": result_json(&r),
        }))
        .into_response(),
        // Retryable later (e.g. the conversation's current run must finish first).
        Ok(Err(AgentError::Store(StoreError::Conflict(m)))) => connect_error(StatusCode::CONFLICT, "aborted", m),
        // Not retryable here: another instance owns the run now.
        Ok(Err(AgentError::Store(StoreError::Fenced(m)))) => connect_error(
            StatusCode::PRECONDITION_FAILED,
            "failed_precondition",
            format!("run {m} was taken over by another instance"),
        ),
        Ok(Err(e)) => connect_error(StatusCode::INTERNAL_SERVER_ERROR, "internal", e.to_string()),
        Err(e) => connect_error(
            StatusCode::INTERNAL_SERVER_ERROR,
            "internal",
            format!("run task panicked: {e}"),
        ),
    }
}

async fn cancel_task(State(st): State<Arc<HostState>>, body: axum::body::Bytes) -> Response {
    let req: CancelIn = match parse(&body) {
        Ok(r) => r,
        Err(e) => return *e,
    };
    let token = st.running.lock().unwrap().get(&req.task_id).cloned();
    if let Some(t) = &token {
        t.cancel();
    }
    Json(json!({"cancelled": token.is_some()})).into_response()
}

async fn drain(State(st): State<Arc<HostState>>) -> Response {
    st.draining.store(true, Ordering::SeqCst);
    Json(json!({})).into_response()
}
