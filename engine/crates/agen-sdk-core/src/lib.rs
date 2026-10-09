//! Language-neutral facade over the Agen engine, shared by every SDK binding
//! (Python, Node, and the C ABI used by Go).
//!
//! Everything crosses the boundary as JSON strings:
//! - [`AgentSpec`] (JSON) creates an agent: a bundle path, or inline
//!   instructions/model/provider, plus host tools and options.
//! - Host-language tools and approvals are requests sent to a single host
//!   callback; the host answers later with [`SdkAgent::complete`]. The engine
//!   never blocks a runtime thread waiting on the host language.
//! - Runs stream events (`delta`, `reset`, `done`) to an event callback.
//!
//! The facade owns one multi-threaded tokio runtime per process.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use agen_engine::agent::{
    Agent, AgentConfig, AgentError, ApprovalDecision, ApprovalRequest, Approver, BundleOptions, RunOptions, RunResult,
};
use agen_engine::bundle::{Action, Bundle, Permissions};
use agen_engine::provider::fake::{FakeProvider, FakeResponse};
use agen_engine::provider::openai::OpenAiCompatible;
use agen_engine::provider::{ModelProvider, ToolSpec};
use agen_engine::secrets::Secrets;
use agen_engine::store::Store;
use agen_engine::tools::{Tool, ToolContext, ToolError};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::{json, Value};
use tokio::sync::oneshot;
use tokio_util::sync::CancellationToken;

#[derive(Debug, thiserror::Error)]
pub enum SdkError {
    #[error("invalid spec: {0}")]
    Spec(String),
    #[error(transparent)]
    Agent(#[from] AgentError),
    #[error("{0}")]
    Other(String),
    #[error("agent is closed")]
    Closed,
    #[error(
        "run() cannot be called from inside an agent callback (it would deadlock); hand the work to another thread"
    )]
    Reentrant,
}

impl SdkError {
    /// Stable machine-readable code for bindings.
    pub fn code(&self) -> &'static str {
        match self {
            SdkError::Spec(_) => "invalid_spec",
            SdkError::Agent(AgentError::Config(_)) => "invalid_config",
            SdkError::Agent(AgentError::Store(_)) => "store",
            SdkError::Agent(AgentError::Provider(_)) => "provider",
            SdkError::Other(_) => "internal",
            SdkError::Closed => "closed",
            SdkError::Reentrant => "reentrant",
        }
    }

    pub fn to_json(&self) -> String {
        json!({"code": self.code(), "message": self.to_string()}).to_string()
    }
}

pub fn runtime() -> &'static tokio::runtime::Runtime {
    static RT: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RT.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .thread_name("agen")
            .build()
            .expect("tokio runtime")
    })
}

/// A tool implemented in the host language.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct HostToolSpec {
    pub name: String,
    #[serde(default)]
    pub description: String,
    #[serde(default = "default_params")]
    pub parameters: Value,
    /// Host tools are side-effecting unless declared otherwise.
    #[serde(default = "yes")]
    pub side_effect: bool,
    /// Seconds before the call is abandoned as outcome-unknown.
    pub timeout_seconds: Option<u64>,
}

fn default_params() -> Value {
    json!({"type": "object"})
}
fn yes() -> bool {
    true
}

/// Model provider for inline (non-bundle) agents.
#[derive(Debug, Clone, Deserialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum ProviderSpec {
    /// Scripted responses (tests, examples).
    Fake {
        responses: Vec<FakeResponse>,
        #[serde(default)]
        cycle: bool,
    },
    Openrouter {
        #[serde(rename = "apiKey")]
        api_key: Option<String>,
        #[serde(rename = "baseUrl")]
        base_url: Option<String>,
    },
    Openai {
        #[serde(rename = "apiKey")]
        api_key: Option<String>,
        #[serde(rename = "baseUrl")]
        base_url: Option<String>,
    },
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct AgentSpec {
    /// Load this bundle directory (instructions/model/provider come from it).
    pub bundle: Option<String>,
    pub name: Option<String>,
    pub instructions: Option<String>,
    pub model: Option<String>,
    pub provider: Option<ProviderSpec>,
    pub max_turns: Option<u32>,
    pub max_output_tokens: Option<u32>,
    pub temperature: Option<f64>,
    /// Store URL. Default: in-memory SQLite (nothing persists).
    pub store: Option<String>,
    pub namespace: Option<String>,
    pub deployment: Option<String>,
    pub tools: Vec<HostToolSpec>,
    /// Secret values the host passes in (redacted everywhere).
    pub secrets: BTreeMap<String, String>,
    /// Permission policy for inline agents: {"default": "allow", "rules": [...]}.
    pub permissions: Option<Value>,
    /// Send "ask" approvals to the host callback (otherwise they are denied).
    pub host_approvals: bool,
    pub approval_timeout_seconds: Option<u64>,
    pub max_tokens_per_run: Option<u64>,
    pub owner: Option<String>,
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct RunSpec {
    pub session_id: Option<String>,
    pub singleton: bool,
    pub new_conversation: bool,
    pub task_id: String,
    pub traceparent: Option<String>,
    /// Key under which this run can be cancelled with [`SdkAgent::cancel`].
    pub cancel_key: Option<String>,
    /// Emit `delta` events (default true).
    pub stream: Option<bool>,
}

/// Called with a JSON request the host must answer via [`SdkAgent::complete`]:
/// `{"id","kind":"tool","name","arguments","runId","callId"}` or
/// `{"id","kind":"approval","tool","arguments","runId"}`.
/// Also receives `{"id","kind":"cancel"}` when a pending request is abandoned
/// (run cancelled, timed out): the host should stop that work.
/// Always invoked on one dedicated dispatcher thread, never on an engine
/// worker, so a slow host (e.g. waiting for the Python GIL) cannot stall runs.
pub type HostCallback = Arc<dyn Fn(String) + Send + Sync>;
/// Receives run events as JSON: `{"type":"delta","text"}`, `{"type":"reset"}`,
/// `{"type":"done","result":{...}}`. Invoked on the dispatcher thread, in
/// order; all events of a run are delivered before the run call returns.
pub type EventCallback = Arc<dyn Fn(String) + Send + Sync>;

type Pending = Arc<Mutex<HashMap<String, oneshot::Sender<Result<String, String>>>>>;

enum Dispatch {
    Host(String),
    Event(EventCallback, String),
    Flush(oneshot::Sender<()>),
}

/// Single thread that delivers all host requests and events in order.
#[derive(Clone)]
struct Dispatcher {
    tx: std::sync::mpsc::Sender<Dispatch>,
}

thread_local! {
    /// True on a dispatcher thread: callbacks run there, so a nested blocking
    /// run() from a callback would wait on itself.
    static IN_DISPATCH: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

fn in_dispatch() -> bool {
    IN_DISPATCH.with(|f| f.get())
}

impl Dispatcher {
    fn start(host: Option<HostCallback>) -> Self {
        let (tx, rx) = std::sync::mpsc::channel::<Dispatch>();
        std::thread::Builder::new()
            .name("agen-dispatch".into())
            .spawn(move || {
                IN_DISPATCH.with(|f| f.set(true));
                // Ends when every Dispatcher clone (the agent) is dropped.
                for msg in rx {
                    let r = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| match msg {
                        Dispatch::Host(req) => {
                            if let Some(h) = &host {
                                h(req)
                            }
                        }
                        Dispatch::Event(cb, ev) => cb(ev),
                        Dispatch::Flush(done) => {
                            let _ = done.send(());
                        }
                    }));
                    if r.is_err() {
                        eprintln!("agen: host callback panicked");
                    }
                }
            })
            .expect("spawn dispatcher thread");
        Self { tx }
    }

    fn send(&self, m: Dispatch) {
        let _ = self.tx.send(m);
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        self.send(Dispatch::Flush(tx));
        let _ = rx.await;
    }
}

#[derive(Clone)]
struct Bridge {
    has_host: bool,
    pending: Pending,
    dispatch: Dispatcher,
    closed: Arc<std::sync::atomic::AtomicBool>,
}

/// Removes a pending request when its waiter goes away for any reason; if it
/// was still unanswered, tells the host to stop (`kind: cancel`).
struct PendingGuard {
    id: String,
    pending: Pending,
    dispatch: Dispatcher,
}

impl Drop for PendingGuard {
    fn drop(&mut self) {
        if self.pending.lock().unwrap().remove(&self.id).is_some() {
            self.dispatch
                .send(Dispatch::Host(json!({"id": self.id, "kind": "cancel"}).to_string()));
        }
    }
}

impl Bridge {
    /// Send a request to the host and wait for its answer.
    async fn ask(
        &self,
        mut request: Value,
        cancel: &CancellationToken,
        timeout: Option<Duration>,
    ) -> Result<String, ToolError> {
        if !self.has_host {
            return Err(ToolError::new("no host callback registered"));
        }
        if self.closed.load(std::sync::atomic::Ordering::SeqCst) {
            return Err(ToolError::unknown("agent closed"));
        }
        let id = agen_engine::store::new_id();
        request["id"] = json!(id);
        let (tx, rx) = oneshot::channel();
        self.pending.lock().unwrap().insert(id.clone(), tx);
        let _guard = PendingGuard {
            id,
            pending: self.pending.clone(),
            dispatch: self.dispatch.clone(),
        };
        self.dispatch.send(Dispatch::Host(request.to_string()));
        let wait = async {
            match timeout {
                Some(t) => tokio::time::timeout(t, rx)
                    .await
                    .map_err(|_| ToolError::unknown(format!("host did not answer within {t:?}"))),
                None => Ok(rx.await),
            }
        };
        let r = tokio::select! {
            r = wait => r,
            _ = cancel.cancelled() => Err(ToolError::unknown("cancelled")),
        };
        match r? {
            Ok(Ok(v)) => Ok(v),
            Ok(Err(e)) => Err(ToolError::new(e)),
            Err(_) => Err(ToolError::unknown("host dropped the request")),
        }
    }
}

struct HostTool {
    spec: HostToolSpec,
    bridge: Bridge,
}

#[async_trait]
impl Tool for HostTool {
    fn spec(&self) -> ToolSpec {
        ToolSpec {
            name: self.spec.name.clone(),
            description: self.spec.description.clone(),
            parameters: self.spec.parameters.clone(),
        }
    }
    fn side_effect(&self) -> bool {
        self.spec.side_effect
    }
    async fn call(&self, args: Value, ctx: ToolContext) -> Result<String, ToolError> {
        let req = json!({"kind": "tool", "name": self.spec.name, "arguments": args, "runId": ctx.run_id, "callId": ctx.call_id});
        self.bridge
            .ask(req, &ctx.cancel, self.spec.timeout_seconds.map(Duration::from_secs))
            .await
    }
}

struct HostApprover {
    bridge: Bridge,
}

#[async_trait]
impl Approver for HostApprover {
    async fn decide(&self, req: ApprovalRequest) -> ApprovalDecision {
        // The engine applies the approval timeout and run cancellation by
        // dropping this future; the pending guard then cancels the request.
        let r = json!({"kind": "approval", "tool": req.tool, "arguments": req.arguments, "runId": req.run_id});
        match self.bridge.ask(r, &CancellationToken::new(), None).await.as_deref() {
            Ok("approved") | Ok("approve") | Ok("true") => ApprovalDecision::Approved,
            Ok("expired") => ApprovalDecision::Expired,
            _ => ApprovalDecision::Denied,
        }
    }
}

/// Cancellation state per `cancelKey`. A cancel that arrives before its run
/// registers is remembered (briefly) so the run starts cancelled.
enum CancelEntry {
    Running(CancellationToken),
    Early(std::time::Instant),
}

const EARLY_CANCEL_TTL: Duration = Duration::from_secs(60);

pub struct SdkAgent {
    agent: Arc<Agent>,
    bridge: Bridge,
    cancels: Mutex<HashMap<String, CancelEntry>>,
}

impl SdkAgent {
    /// Create an agent from a JSON [`AgentSpec`].
    pub fn create(spec_json: &str, host: Option<HostCallback>) -> Result<SdkAgent, SdkError> {
        let spec: AgentSpec = serde_json::from_str(spec_json).map_err(|e| SdkError::Spec(e.to_string()))?;
        runtime().block_on(Self::create_async(spec, host))
    }

    pub async fn create_async(spec: AgentSpec, host: Option<HostCallback>) -> Result<SdkAgent, SdkError> {
        let bridge = Bridge {
            has_host: host.is_some(),
            pending: Arc::new(Mutex::new(HashMap::new())),
            dispatch: Dispatcher::start(host),
            closed: Arc::new(std::sync::atomic::AtomicBool::new(false)),
        };
        let store_url = spec.store.clone().unwrap_or_else(|| "sqlite::memory:".into());
        let store = Arc::new(Store::open(&store_url).await.map_err(|e| SdkError::Agent(e.into()))?);
        let host_tools: Vec<Arc<dyn Tool>> = spec
            .tools
            .iter()
            .map(|t| {
                Arc::new(HostTool {
                    spec: t.clone(),
                    bridge: bridge.clone(),
                }) as Arc<dyn Tool>
            })
            .collect();
        let approver: Option<Arc<dyn Approver>> = spec
            .host_approvals
            .then(|| Arc::new(HostApprover { bridge: bridge.clone() }) as Arc<dyn Approver>);

        let agent = if let Some(path) = &spec.bundle {
            let bundle = Bundle::load(path).map_err(|e| SdkError::Spec(e.to_string()))?;
            let opts = BundleOptions {
                namespace: spec.namespace.clone(),
                deployment: spec.deployment.clone(),
                approver,
                extra_tools: host_tools,
                owner: spec.owner.clone(),
                ..Default::default()
            };
            Agent::from_bundle(&bundle, store, opts).await?
        } else {
            let name = spec
                .name
                .clone()
                .ok_or_else(|| SdkError::Spec("\"name\" is required without \"bundle\"".into()))?;
            let provider_spec = spec
                .provider
                .clone()
                .ok_or_else(|| SdkError::Spec("\"provider\" is required without \"bundle\"".into()))?;
            let model = spec.model.clone().unwrap_or_else(|| "fake-1".into());
            let mut cfg = AgentConfig::new(
                &name,
                spec.instructions.as_deref().unwrap_or("You are a helpful assistant."),
                &model,
            );
            if let Some(n) = spec.max_turns {
                cfg.max_turns = n;
            }
            cfg.max_output_tokens = spec.max_output_tokens;
            cfg.temperature = spec.temperature;
            cfg.budget.max_tokens_per_run = spec.max_tokens_per_run;
            if let Some(ns) = &spec.namespace {
                cfg.namespace = ns.clone();
            }
            if let Some(d) = &spec.deployment {
                cfg.deployment = d.clone();
            }
            if let Some(t) = spec.approval_timeout_seconds {
                cfg.approval_timeout = Duration::from_secs(t);
            }
            cfg.permissions = match &spec.permissions {
                Some(p) => {
                    serde_json::from_value(p.clone()).map_err(|e| SdkError::Spec(format!("permissions: {e}")))?
                }
                None => Permissions {
                    approval_timeout: None,
                    default: Action::Allow,
                    rules: vec![],
                },
            };
            let provider = build_provider(&provider_spec, &spec.secrets)?;
            // An API key passed inline is a secret too: register it so it is redacted.
            let mut secrets = spec.secrets.clone();
            if let ProviderSpec::Openrouter { api_key: Some(k), .. } | ProviderSpec::Openai { api_key: Some(k), .. } =
                &provider_spec
            {
                secrets.insert("PROVIDER_API_KEY".into(), k.clone());
            }
            let mut b = Agent::builder(cfg, store)
                .provider(provider)
                .secrets(Secrets::from_map(secrets));
            if let Some(a) = approver {
                b = b.approver(a);
            }
            if let Some(o) = &spec.owner {
                b = b.owner(o);
            }
            for t in host_tools {
                b = b.tool(t);
            }
            b.build()?
        };
        Ok(SdkAgent {
            agent: Arc::new(agent),
            bridge,
            cancels: Mutex::new(HashMap::new()),
        })
    }

    /// Answer a host request (tool result, or approval "approved"/"denied").
    /// Returns false if the request is unknown (e.g. it timed out).
    pub fn complete(&self, request_id: &str, result: Result<String, String>) -> bool {
        match self.bridge.pending.lock().unwrap().remove(request_id) {
            Some(tx) => tx.send(result).is_ok(),
            None => false,
        }
    }

    /// Close the agent: reject new runs, cancel running ones, and fail every
    /// pending host request, so in-flight runs return promptly. Idempotent.
    /// `complete`/`cancel` keep working; resources are freed when dropped.
    pub fn shutdown(&self) {
        self.bridge.closed.store(true, std::sync::atomic::Ordering::SeqCst);
        for e in self.cancels.lock().unwrap().values() {
            if let CancelEntry::Running(t) = e {
                t.cancel();
            }
        }
        let pending: Vec<_> = self.bridge.pending.lock().unwrap().drain().collect();
        for (_, tx) in pending {
            let _ = tx.send(Err("agent closed".into()));
        }
    }

    pub fn is_closed(&self) -> bool {
        self.bridge.closed.load(std::sync::atomic::Ordering::SeqCst)
    }

    /// Cancel the run started with `cancelKey` = `key`. If that run has not
    /// started yet, it will start cancelled. Returns true if a running run
    /// was cancelled.
    pub fn cancel(&self, key: &str) -> bool {
        let mut map = self.cancels.lock().unwrap();
        map.retain(|_, e| !matches!(e, CancelEntry::Early(t) if t.elapsed() > EARLY_CANCEL_TTL));
        match map.get(key) {
            Some(CancelEntry::Running(t)) => {
                t.cancel();
                true
            }
            _ => {
                map.insert(key.to_string(), CancelEntry::Early(std::time::Instant::now()));
                false
            }
        }
    }

    /// Run to completion; returns the result JSON. Also emits a `done` event.
    pub async fn run_async(
        &self,
        input: &str,
        spec: RunSpec,
        on_event: Option<EventCallback>,
    ) -> Result<String, SdkError> {
        if in_dispatch() {
            return Err(SdkError::Reentrant);
        }
        if self.is_closed() {
            return Err(SdkError::Closed);
        }
        let cancel = CancellationToken::new();
        // Every run is registered (under its key or an internal one) so
        // shutdown() can cancel it.
        let key = spec
            .cancel_key
            .clone()
            .unwrap_or_else(|| format!("__run-{}", agen_engine::store::new_id()));
        {
            let mut map = self.cancels.lock().unwrap();
            match map.get(&key) {
                Some(CancelEntry::Running(_)) => {
                    return Err(SdkError::Spec(format!(
                        "cancelKey {key:?} is already in use by a running run"
                    )));
                }
                Some(CancelEntry::Early(_)) => cancel.cancel(),
                None => {}
            }
            map.insert(key.clone(), CancelEntry::Running(cancel.clone()));
            // A shutdown that raced with registration still cancels this run.
            if self.is_closed() {
                cancel.cancel();
            }
        }
        let mut opts = RunOptions {
            session_id: spec.session_id.clone(),
            singleton: spec.singleton,
            new_conversation: spec.new_conversation,
            task_id: spec.task_id.clone(),
            traceparent: spec.traceparent.clone(),
            cancel,
            ..Default::default()
        };
        if let Some(cb) = &on_event {
            if spec.stream.unwrap_or(true) {
                let (d, dd) = (cb.clone(), self.bridge.dispatch.clone());
                opts.on_delta = Some(Arc::new(move |t: &str| {
                    dd.send(Dispatch::Event(
                        d.clone(),
                        json!({"type": "delta", "text": t}).to_string(),
                    ))
                }));
                let (r, rd) = (cb.clone(), self.bridge.dispatch.clone());
                opts.on_reset = Some(Arc::new(move || {
                    rd.send(Dispatch::Event(r.clone(), json!({"type": "reset"}).to_string()))
                }));
            }
        }
        let result = self.agent.run(input, opts).await;
        self.cancels.lock().unwrap().remove(&key);
        let result = result.map(|r| result_json(&r));
        if let Some(cb) = &on_event {
            if let Ok(r) = &result {
                self.bridge.dispatch.send(Dispatch::Event(
                    cb.clone(),
                    json!({"type": "done", "result": r}).to_string(),
                ));
            }
            // Deliver every event of this run before returning.
            self.bridge.dispatch.flush().await;
        }
        Ok(result?.to_string())
    }

    /// Blocking variant for bindings without async (C ABI, Python sync).
    pub fn run_blocking(
        &self,
        input: &str,
        spec_json: &str,
        on_event: Option<EventCallback>,
    ) -> Result<String, SdkError> {
        if in_dispatch() {
            return Err(SdkError::Reentrant);
        }
        let spec: RunSpec = if spec_json.trim().is_empty() {
            RunSpec::default()
        } else {
            serde_json::from_str(spec_json).map_err(|e| SdkError::Spec(e.to_string()))?
        };
        runtime().block_on(self.run_async(input, spec, on_event))
    }

    pub fn tool_names(&self) -> Vec<String> {
        self.agent.tools().specs().into_iter().map(|t| t.name).collect()
    }

    pub fn store(&self) -> &Arc<Store> {
        self.agent.store()
    }

    /// Spans of a trace as JSON: `[{"spanId","parentSpanId","name","runId",
    /// "startMs","endMs","status","attributes"}]` (ordered by start).
    pub async fn trace_async(&self, trace_id: &str) -> Result<String, SdkError> {
        let spans = self
            .agent
            .store()
            .trace(trace_id)
            .await
            .map_err(|e| SdkError::Agent(e.into()))?;
        let out: Vec<Value> = spans
            .into_iter()
            .map(|s| {
                json!({"spanId": s.span_id, "parentSpanId": s.parent_span_id, "name": s.name, "runId": s.run_id,
                       "startMs": s.start_ms, "endMs": s.end_ms, "status": s.status, "attributes": s.attributes})
            })
            .collect();
        Ok(Value::Array(out).to_string())
    }

    pub fn trace_blocking(&self, trace_id: &str) -> Result<String, SdkError> {
        runtime().block_on(self.trace_async(trace_id))
    }

    /// Number of host requests awaiting an answer (for tests / diagnostics).
    pub fn pending_requests(&self) -> usize {
        self.bridge.pending.lock().unwrap().len()
    }
}

fn build_provider(p: &ProviderSpec, secrets: &BTreeMap<String, String>) -> Result<Arc<dyn ModelProvider>, SdkError> {
    let key = |explicit: &Option<String>, env: &str| -> Result<String, SdkError> {
        explicit
            .clone()
            .or_else(|| secrets.get(env).cloned())
            .or_else(|| std::env::var(env).ok())
            .ok_or_else(|| SdkError::Spec(format!("no API key: set provider.apiKey, secrets.{env} or ${env}")))
    };
    Ok(match p {
        ProviderSpec::Fake { responses, cycle } => {
            let f = FakeProvider::new(responses.clone());
            Arc::new(if *cycle { f.cycling() } else { f })
        }
        ProviderSpec::Openrouter { api_key, base_url } => Arc::new(
            OpenAiCompatible::new(
                "openrouter",
                base_url
                    .as_deref()
                    .unwrap_or(agen_engine::provider::openai::OPENROUTER_URL),
                &key(api_key, "OPENROUTER_API_KEY")?,
            )
            .map_err(|e| SdkError::Spec(e.to_string()))?,
        ),
        ProviderSpec::Openai { api_key, base_url } => Arc::new(
            OpenAiCompatible::new(
                "openai",
                base_url.as_deref().unwrap_or(agen_engine::provider::openai::OPENAI_URL),
                &key(api_key, "OPENAI_API_KEY")?,
            )
            .map_err(|e| SdkError::Spec(e.to_string()))?,
        ),
    })
}

/// Parse any JSON value (for bindings that avoid a serde_json dependency).
pub fn parse_json(s: &str) -> Result<Value, SdkError> {
    serde_json::from_str(s).map_err(|e| SdkError::Spec(e.to_string()))
}

/// Parse a JSON [`RunSpec`].
pub fn parse_run_spec(json: &str) -> Result<RunSpec, SdkError> {
    serde_json::from_str(json).map_err(|e| SdkError::Spec(e.to_string()))
}

pub fn result_json(r: &RunResult) -> Value {
    json!({
        "runId": r.run_id,
        "sessionId": r.session_id,
        "conversationId": r.conversation_id,
        "status": r.status.as_str(),
        "output": r.output,
        "error": r.error,
        "usage": {"inputTokens": r.usage.input_tokens, "outputTokens": r.usage.output_tokens, "costUsd": r.usage.cost_usd},
        "traceId": r.trace_id,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(extra: Value) -> String {
        let mut s = json!({
            "name": "t",
            "instructions": "be terse",
            "provider": {"type": "fake", "responses": [
                {"toolCalls": [{"name": "greet", "arguments": {"who": "ada"}}]},
                {"text": "done", "expect": "hello ada"}
            ]},
            "tools": [{"name": "greet", "description": "greets", "sideEffect": false}]
        });
        for (k, v) in extra.as_object().unwrap() {
            s[k] = v.clone();
        }
        s.to_string()
    }

    #[test]
    fn host_tool_round_trip_through_callback() {
        let agent: Arc<Mutex<Option<Arc<SdkAgent>>>> = Arc::new(Mutex::new(None));
        let a2 = agent.clone();
        let host: HostCallback = Arc::new(move |req: String| {
            let v: Value = serde_json::from_str(&req).unwrap();
            let a = a2.lock().unwrap().clone().unwrap();
            // Answer from another thread, like a host language would.
            std::thread::spawn(move || {
                let who = v["arguments"]["who"].as_str().unwrap().to_string();
                assert!(a.complete(v["id"].as_str().unwrap(), Ok(format!("hello {who}"))));
            });
        });
        let a = Arc::new(SdkAgent::create(&spec(json!({})), Some(host)).unwrap());
        *agent.lock().unwrap() = Some(a.clone());
        let events = Arc::new(Mutex::new(vec![]));
        let ev = events.clone();
        let out = a
            .run_blocking("hi", "", Some(Arc::new(move |e: String| ev.lock().unwrap().push(e))))
            .unwrap();
        let v: Value = serde_json::from_str(&out).unwrap();
        assert_eq!(
            (v["status"].as_str(), v["output"].as_str()),
            (Some("succeeded"), Some("done")),
            "{v}"
        );
        let events = events.lock().unwrap();
        assert!(events.iter().any(|e| e.contains("\"delta\"")));
        assert!(events.last().unwrap().contains("\"done\""));
    }

    #[test]
    fn host_tool_errors_and_missing_host() {
        let agent: Arc<Mutex<Option<Arc<SdkAgent>>>> = Arc::new(Mutex::new(None));
        let a2 = agent.clone();
        let host: HostCallback = Arc::new(move |req: String| {
            let v: Value = serde_json::from_str(&req).unwrap();
            let a = a2.lock().unwrap().clone().unwrap();
            a.complete(v["id"].as_str().unwrap(), Err("boom".into()));
        });
        let s = spec(json!({"provider": {"type": "fake", "responses": [
            {"toolCalls": [{"name": "greet", "arguments": {}}]}, {"text": "handled", "expect": "error: boom"}
        ]}}));
        let a = Arc::new(SdkAgent::create(&s, Some(host)).unwrap());
        *agent.lock().unwrap() = Some(a.clone());
        let v: Value = serde_json::from_str(&a.run_blocking("hi", "", None).unwrap()).unwrap();
        assert_eq!(v["output"], "handled", "{v}");

        let no_host = SdkAgent::create(&s.replace("error: boom", "no host callback"), None).unwrap();
        let v: Value = serde_json::from_str(&no_host.run_blocking("hi", "", None).unwrap()).unwrap();
        assert_eq!(v["output"], "handled", "{v}");
    }

    #[test]
    fn cancel_by_key_and_bad_specs() {
        let s = spec(json!({"provider": {"type": "fake", "responses": [{"text": "slow", "delayMs": 10000}]}}));
        let a = Arc::new(SdkAgent::create(&s, None).unwrap());
        let a2 = a.clone();
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(200));
            while !a2.cancel("k1") {
                std::thread::sleep(Duration::from_millis(20));
            }
        });
        let v: Value = serde_json::from_str(&a.run_blocking("x", r#"{"cancelKey":"k1"}"#, None).unwrap()).unwrap();
        assert_eq!(v["status"], "cancelled");
        let err = SdkAgent::create("{}", None).err().unwrap();
        assert_eq!(err.code(), "invalid_spec");
        let err = SdkAgent::create("not json", None).err().unwrap();
        assert!(err.to_json().contains("invalid_spec"));
        assert!(a.run_blocking("x", "{bad", None).is_err());
    }

    #[test]
    fn bundle_spec_and_host_approvals() {
        let hello = concat!(env!("CARGO_MANIFEST_DIR"), "/../../../examples/bundles/hello");
        let a = SdkAgent::create(&json!({"bundle": hello}).to_string(), None).unwrap();
        let v: Value = serde_json::from_str(&a.run_blocking("hi", "", None).unwrap()).unwrap();
        assert_eq!(v["output"], "Hello! Nice to meet you.");

        let agent: Arc<Mutex<Option<Arc<SdkAgent>>>> = Arc::new(Mutex::new(None));
        let a2 = agent.clone();
        let host: HostCallback = Arc::new(move |req: String| {
            let v: Value = serde_json::from_str(&req).unwrap();
            let a = a2.lock().unwrap().clone().unwrap();
            let answer = if v["kind"] == "approval" {
                "denied".to_string()
            } else {
                "ran".to_string()
            };
            a.complete(v["id"].as_str().unwrap(), Ok(answer));
        });
        let s = spec(json!({
            "hostApprovals": true,
            "permissions": {"default": "ask"},
            "provider": {"type": "fake", "responses": [
                {"toolCalls": [{"name": "greet", "arguments": {}}]}, {"text": "ok", "expect": "not approved (denied)"}
            ]}
        }));
        let a = Arc::new(SdkAgent::create(&s, Some(host)).unwrap());
        *agent.lock().unwrap() = Some(a.clone());
        let v: Value = serde_json::from_str(&a.run_blocking("hi", "", None).unwrap()).unwrap();
        assert_eq!(v["output"], "ok", "{v}");
    }

    #[test]
    fn early_cancel_and_key_in_use() {
        let s = spec(
            json!({"provider": {"type": "fake", "responses": [{"text": "slow", "delayMs": 10000}], "cycle": true}}),
        );
        let a = Arc::new(SdkAgent::create(&s, None).unwrap());
        // Cancel before the run exists: the run starts cancelled.
        assert!(!a.cancel("early"));
        let v: Value = serde_json::from_str(&a.run_blocking("x", r#"{"cancelKey":"early"}"#, None).unwrap()).unwrap();
        assert_eq!(v["status"], "cancelled");
        // A key in use by a running run is rejected.
        let a2 = a.clone();
        let t = std::thread::spawn(move || a2.run_blocking("x", r#"{"cancelKey":"busy"}"#, None));
        std::thread::sleep(Duration::from_millis(200));
        let err = a.run_blocking("x", r#"{"cancelKey":"busy"}"#, None).unwrap_err();
        assert!(err.to_string().contains("already in use"), "{err}");
        assert!(a.cancel("busy"));
        let v: Value = serde_json::from_str(&t.join().unwrap().unwrap()).unwrap();
        assert_eq!(v["status"], "cancelled");
    }

    #[test]
    fn cancelled_host_requests_are_cleaned_up_and_host_is_told() {
        let seen = Arc::new(Mutex::new(Vec::<Value>::new()));
        let s2 = seen.clone();
        // A host that never answers tool calls.
        let host: HostCallback =
            Arc::new(move |req: String| s2.lock().unwrap().push(serde_json::from_str(&req).unwrap()));
        let s = spec(json!({}));
        let a = Arc::new(SdkAgent::create(&s, Some(host)).unwrap());
        let a2 = a.clone();
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(300));
            a2.cancel("k");
        });
        let v: Value = serde_json::from_str(&a.run_blocking("x", r#"{"cancelKey":"k"}"#, None).unwrap()).unwrap();
        assert_eq!(v["status"], "cancelled");
        assert_eq!(a.pending_requests(), 0, "pending map must not leak");
        std::thread::sleep(Duration::from_millis(100));
        let seen = seen.lock().unwrap();
        assert_eq!(seen.len(), 2, "{seen:?}");
        assert_eq!(
            (seen[0]["kind"].as_str(), seen[1]["kind"].as_str()),
            (Some("tool"), Some("cancel"))
        );
        assert_eq!(seen[0]["id"], seen[1]["id"]);
    }

    #[test]
    fn all_events_arrive_before_run_returns_and_callbacks_run_off_engine_threads() {
        let s = spec(json!({"provider": {"type": "fake", "responses": [{"text": "a b c d e f g"}]}}));
        let a = SdkAgent::create(&s, None).unwrap();
        let events = Arc::new(Mutex::new(vec![]));
        let (ev, names) = (events.clone(), Arc::new(Mutex::new(std::collections::HashSet::new())));
        let n2 = names.clone();
        a.run_blocking(
            "x",
            "",
            Some(Arc::new(move |e: String| {
                n2.lock()
                    .unwrap()
                    .insert(std::thread::current().name().unwrap_or("").to_string());
                ev.lock().unwrap().push(e)
            })),
        )
        .unwrap();
        let events = events.lock().unwrap();
        assert!(
            events.last().unwrap().contains("\"done\""),
            "done must be delivered before return"
        );
        assert_eq!(events.iter().filter(|e| e.contains("delta")).count(), 7);
        assert_eq!(
            *names.lock().unwrap(),
            ["agen-dispatch".to_string()].into_iter().collect()
        );
    }

    #[test]
    fn shutdown_during_run_fails_pending_requests_and_returns() {
        // Host never answers: only shutdown can release the run.
        let s = spec(json!({}));
        let a = Arc::new(SdkAgent::create(&s, Some(Arc::new(|_req: String| {}))).unwrap());
        let a2 = a.clone();
        let t = std::thread::spawn(move || a2.run_blocking("x", "", None));
        std::thread::sleep(Duration::from_millis(200));
        let started = std::time::Instant::now();
        a.shutdown();
        let v: Value = serde_json::from_str(&t.join().unwrap().unwrap()).unwrap();
        assert!(started.elapsed() < Duration::from_secs(5));
        assert_eq!(v["status"], "cancelled", "{v}");
        assert_eq!(a.pending_requests(), 0);
        assert_eq!(a.run_blocking("x", "", None).unwrap_err().code(), "closed");
        a.shutdown(); // idempotent
    }

    #[test]
    fn run_from_inside_a_callback_is_rejected_not_deadlocked() {
        let s = spec(json!({"provider": {"type": "fake", "responses": [{"text": "hi"}], "cycle": true}}));
        let a = Arc::new(SdkAgent::create(&s, None).unwrap());
        let a2 = a.clone();
        let nested = Arc::new(Mutex::new(None));
        let n2 = nested.clone();
        let cb: EventCallback = Arc::new(move |_e: String| {
            let mut n = n2.lock().unwrap();
            if n.is_none() {
                *n = Some(a2.run_blocking("inner", "", None).map_err(|e| e.code()));
            }
        });
        let out = a.run_blocking("outer", "", Some(cb));
        assert!(out.is_ok());
        assert_eq!(nested.lock().unwrap().clone(), Some(Err("reentrant")));
    }
}
