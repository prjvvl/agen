//! The agent loop.
//!
//! Every step is checkpointed in the Store (atomically, and only by the run's
//! current owner) before the next one starts:
//! 1. user input → message
//! 2. model response → assistant message + run progress (one transaction)
//! 3. each tool call → effect ledger (side-effecting tools) → tool message
//!
//! [`Agent::resume`] takes ownership of an unfinished run and continues from
//! whatever the Store holds, so a process that dies at any point can be
//! restarted without repeating a recorded side effect. A previous owner that
//! is still alive is fenced off and stops at its next write.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use futures_util::StreamExt;
use serde_json::Value;
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use crate::bundle::{default_limits, Action, Budget, Bundle, Limits, PermissionRule, Permissions, Skill, Trust};
use crate::outcome::RunOutcome;
use crate::permissions;
use crate::provider::{self, Message, ModelProvider, ModelRequest, ProviderError, Role, ToolCall, Usage};
use crate::secrets::{PlatformSecrets, Redactor, Secrets, StreamRedactor};
use crate::store::{new_id, now_ms, EffectBegin, NewArtifact, RunRecord, RunStatus, Store, StoreError};
use crate::tools::{LoadSkill, Tool, ToolContext, ToolRegistry};
use crate::trace::{OpenSpan, TraceContext, Tracer};

#[derive(Debug, thiserror::Error)]
pub enum AgentError {
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Provider(#[from] ProviderError),
    #[error("configuration: {0}")]
    Config(String),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ApprovalDecision {
    Approved,
    Denied,
    Expired,
}

#[derive(Debug, Clone)]
pub struct ApprovalRequest {
    pub run_id: String,
    pub tool: String,
    /// Already redacted.
    pub arguments: Value,
}

/// Decides "ask" permissions. Managed mode: the Manager (durable approvals).
/// Embedded mode: the host application.
#[async_trait]
pub trait Approver: Send + Sync {
    async fn decide(&self, req: ApprovalRequest) -> ApprovalDecision;
}

/// Denies everything; the default when no approver is configured.
pub struct DenyAll;

#[async_trait]
impl Approver for DenyAll {
    async fn decide(&self, _req: ApprovalRequest) -> ApprovalDecision {
        ApprovalDecision::Denied
    }
}

#[derive(Debug, Clone)]
pub struct AgentConfig {
    pub name: String,
    pub system_prompt: String,
    pub skills: Vec<Skill>,
    /// Skills whose instructions are always in the system prompt.
    pub preload_skills: Vec<String>,
    pub model: String,
    /// Models tried in order when `model` keeps failing or gives no answer.
    pub fallback_models: Vec<String>,
    /// Provider-specific request fields (OpenRouter `provider`, `plugins`,
    /// `reasoning`), sent with every model request.
    pub request_extra: serde_json::Map<String, Value>,
    /// Settings for a routing model (cost tier, allowed models).
    pub router: Option<crate::provider::router::RouterSettings>,
    pub temperature: Option<f64>,
    pub max_output_tokens: Option<u32>,
    pub max_turns: u32,
    pub permissions: Permissions,
    pub budget: Budget,
    pub limits: Limits,
    pub namespace: String,
    pub deployment: String,
    pub definition_digest: String,
    /// Approximate token budget for conversation history sent to the model;
    /// older turns from earlier runs are dropped to fit.
    pub context_tokens: u64,
    /// How long an "ask" approval may wait before it counts as expired.
    pub approval_timeout: Duration,
    /// Run the tool calls of one model turn concurrently.
    pub parallel_tool_calls: bool,
    /// Record tool call arguments (redacted) on tool spans.
    pub trace_tool_arguments: bool,
    /// Wall-clock time a run may take, not counting approval waits.
    pub max_run_duration: Duration,
    /// How long one model request may take.
    pub model_request_timeout: Duration,
    /// How long one tool call may take (not `call_agent`).
    pub tool_timeout: Duration,
    /// The data `finish` must return (harness `result.schema`).
    pub result_schema: Option<crate::outcome::ResultSchema>,
    /// Output limit of the report call at a limit (0: no report call).
    pub report_tokens: u32,
    /// Memory blocks attached to this bundle (`x-agen/config.json`
    /// `memory`), rendered into the system prompt.
    pub memory: Vec<crate::bundle::MemoryAttachment>,
    /// A tool result longer than this many characters is saved as an
    /// artifact instead of going into the conversation whole (0: no cap).
    pub max_tool_result_chars: u32,
}

impl AgentConfig {
    pub fn new(name: &str, system_prompt: &str, model: &str) -> Self {
        Self {
            name: name.into(),
            system_prompt: system_prompt.into(),
            skills: vec![],
            preload_skills: vec![],
            model: model.into(),
            fallback_models: vec![],
            request_extra: Default::default(),
            router: None,
            temperature: None,
            max_output_tokens: None,
            max_turns: 16,
            permissions: Permissions {
                approval_timeout: None,
                default: Action::Allow,
                rules: vec![],
            },
            budget: Budget::default(),
            limits: Limits::default(),
            namespace: "default".into(),
            deployment: name.into(),
            definition_digest: String::new(),
            context_tokens: 64_000,
            approval_timeout: Duration::from_secs(3600),
            parallel_tool_calls: true,
            trace_tool_arguments: false,
            max_run_duration: default_limits::MAX_RUN_DURATION,
            model_request_timeout: default_limits::MODEL_REQUEST_TIMEOUT,
            tool_timeout: default_limits::TOOL_TIMEOUT,
            result_schema: None,
            report_tokens: default_limits::REPORT_TOKENS,
            memory: vec![],
            max_tool_result_chars: default_limits::MAX_TOOL_RESULT_CHARS,
        }
    }
}

pub type DeltaCallback = Arc<dyn Fn(&str) + Send + Sync>;
pub type ResetCallback = Arc<dyn Fn() + Send + Sync>;

#[derive(Clone, Default)]
pub struct RunOptions {
    /// Continue this session (default: a new session per run, or the
    /// deployment's session when `singleton` is set).
    pub session_id: Option<String>,
    pub singleton: bool,
    /// Start a fresh conversation in the session.
    pub new_conversation: bool,
    /// Continue the conversation of earlier runs with the same key (managed
    /// mode: the task's conversation key). Ignored when `session_id` is set,
    /// and by singletons, which have one conversation.
    pub conversation_key: String,
    /// Copied to the run, its tool calls and the tasks it delegates.
    pub labels: BTreeMap<String, String>,
    /// Which attempt at the task this is (1 for the first; 0 when unknown).
    pub attempt: u32,
    pub task_id: String,
    pub parent_run_id: String,
    pub root_run_id: String,
    /// Delegation depth of this run (from the task or the A2A call).
    pub depth: u32,
    /// Cost tier asked for this run (routing models only; capped by the
    /// harness `router.maxTier`).
    pub model_tier: Option<String>,
    /// The deployment has spent most of its daily budget: routing models use
    /// the harness `router.budgetTier` (low).
    pub near_budget: bool,
    /// Who asked for the run when it is not a Hub task (from a verified A2A
    /// call token); recorded on the run for approvals.
    pub requested_by: String,
    pub traceparent: Option<String>,
    pub cancel: CancellationToken,
    /// Streamed (redacted) text.
    pub on_delta: Option<DeltaCallback>,
    /// A model request is being retried: discard text streamed since the
    /// last reset.
    pub on_reset: Option<ResetCallback>,
    /// What this run may do, narrower than the bundle (already combined
    /// with every ancestor task's overrides, if any).
    pub overrides: Option<crate::overrides::TaskOverrides>,
    /// The task's trust at the start of the run (a child's is its caller's
    /// *current* trust, already lowered by any taint the caller picked up).
    /// Never raised during the run, only lowered by taint.
    pub trust: Trust,
}

#[derive(Debug, Clone, PartialEq)]
pub struct ToolServerStatus {
    pub name: String,
    pub connected: bool,
    pub tool_count: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RunResult {
    pub run_id: String,
    pub session_id: String,
    pub conversation_id: String,
    pub status: RunStatus,
    pub output: String,
    pub error: String,
    pub usage: Usage,
    pub trace_id: String,
    /// How the run ended: status (done, partial, needs_input, rejected,
    /// failed, cancelled), reason, report and data.
    pub outcome: Option<RunOutcome>,
    /// Set instead of the run having ended (3.3): what it is waiting for
    /// ("<kind>:<ref>"). `status` is unchanged (still Running: the run is
    /// not finished, only paused) and `outcome` is None.
    pub wait_for: String,
}

pub struct Agent {
    cfg: AgentConfig,
    provider: Arc<dyn ModelProvider>,
    tools: ToolRegistry,
    store: Arc<Store>,
    redactor: Redactor,
    secrets: Secrets,
    approver: Arc<dyn Approver>,
    /// Identity used to own runs in the Store.
    owner: String,
    /// MCP server connections (closed / killed when the agent is dropped).
    mcp: Option<crate::mcp::McpConnections>,
    log_sink: Option<LogSink>,
    /// The engine's `finish` tool is registered (no host tool took the name).
    finish: bool,
    /// Backs the run_inbox drain before each model call (3.4/3.9, part),
    /// same as `report_progress`/`signal_agent`/`query_agent`.
    task_starter: Option<Arc<dyn crate::delegate::TaskStarter>>,
}

/// Receives operational log lines (level, redacted message). Default: stderr.
pub type LogSink = Arc<dyn Fn(&str, &str) + Send + Sync>;

pub struct AgentBuilder {
    cfg: AgentConfig,
    provider: Option<Arc<dyn ModelProvider>>,
    tools: Vec<Arc<dyn Tool>>,
    log_sink: Option<LogSink>,
    store: Arc<Store>,
    secrets: Secrets,
    approver: Arc<dyn Approver>,
    owner: Option<String>,
    mcp: Option<crate::mcp::McpConnections>,
    /// Backs the always-on `report_progress` tool (3.4, part): reports to
    /// the Manager when set, no-ops with a friendly message otherwise.
    task_starter: Option<Arc<dyn crate::delegate::TaskStarter>>,
}

impl AgentBuilder {
    /// Keep MCP connections alive for the agent's lifetime.
    pub fn mcp(mut self, conns: crate::mcp::McpConnections) -> Self {
        self.mcp = Some(conns);
        self
    }
    pub fn provider(mut self, p: Arc<dyn ModelProvider>) -> Self {
        self.provider = Some(p);
        self
    }
    pub fn tool(mut self, t: Arc<dyn Tool>) -> Self {
        self.tools.push(t);
        self
    }
    /// Route operational logs (retries, truncation) somewhere other than stderr.
    pub fn log_sink(mut self, sink: LogSink) -> Self {
        self.log_sink = Some(sink);
        self
    }
    pub fn secrets(mut self, s: Secrets) -> Self {
        self.secrets = s;
        self
    }
    pub fn approver(mut self, a: Arc<dyn Approver>) -> Self {
        self.approver = a;
        self
    }
    /// Owner identity for runs (default: a fresh id per agent instance).
    pub fn owner(mut self, owner: &str) -> Self {
        self.owner = Some(owner.into());
        self
    }
    /// Backs `report_progress` (3.4, part); unset: it no-ops.
    pub fn task_starter(mut self, s: Arc<dyn crate::delegate::TaskStarter>) -> Self {
        self.task_starter = Some(s);
        self
    }
    pub fn build(mut self) -> Result<Agent, AgentError> {
        let provider = self
            .provider
            .ok_or_else(|| AgentError::Config("no model provider".into()))?;
        crate::secrets::validate(&self.secrets).map_err(|e| AgentError::Config(e.to_string()))?;
        let mut tools = ToolRegistry::new();
        if !self.cfg.skills.is_empty() && !self.tools.iter().any(|t| t.spec().name == "load_skill") {
            self.tools.push(Arc::new(LoadSkill::new(&self.cfg.skills)));
        }
        let finish = !self.tools.iter().any(|t| t.spec().name == crate::outcome::FINISH);
        if finish {
            self.tools.push(Arc::new(crate::outcome::Finish::new(
                self.cfg.result_schema.clone(),
                self.task_starter.clone(),
            )));
        }
        if !self
            .tools
            .iter()
            .any(|t| t.spec().name == crate::artifacts::SAVE_ARTIFACT)
        {
            self.tools
                .push(Arc::new(crate::artifacts::SaveArtifact::new(self.store.clone())));
        }
        if !self
            .tools
            .iter()
            .any(|t| t.spec().name == crate::artifacts::READ_ARTIFACT)
        {
            self.tools
                .push(Arc::new(crate::artifacts::ReadArtifact::new(self.store.clone())));
        }
        if !self
            .tools
            .iter()
            .any(|t| t.spec().name == crate::progress::REPORT_PROGRESS)
        {
            self.tools.push(Arc::new(crate::progress::ReportProgress::new(
                self.task_starter.clone(),
            )));
        }
        if !self.tools.iter().any(|t| t.spec().name == crate::signals::SIGNAL_AGENT) {
            self.tools
                .push(Arc::new(crate::signals::SignalAgent::new(self.task_starter.clone())));
        }
        if !self.tools.iter().any(|t| t.spec().name == crate::signals::QUERY_AGENT) {
            self.tools
                .push(Arc::new(crate::signals::QueryAgent::new(self.task_starter.clone())));
        }
        let mut wire: std::collections::BTreeMap<String, String> = Default::default();
        for t in self.tools {
            let name = t.spec().name;
            // Names must stay distinct after mapping to the provider wire format.
            if let Some(other) = wire.insert(crate::provider::openai::wire_name(&name), name.clone()) {
                return Err(AgentError::Config(format!("tool names {other:?} and {name:?} collide")));
            }
            tools.register(t).map_err(AgentError::Config)?;
        }
        let redactor = Redactor::new();
        redactor.register_all(&self.secrets);
        Ok(Agent {
            cfg: self.cfg,
            provider,
            tools,
            store: self.store,
            redactor,
            secrets: self.secrets,
            approver: self.approver,
            owner: self.owner.unwrap_or_else(new_id),
            mcp: self.mcp,
            log_sink: self.log_sink,
            finish,
            task_starter: self.task_starter,
        })
    }
}

/// Options for building an agent from a bundle.
#[derive(Default)]
pub struct BundleOptions {
    pub namespace: Option<String>,
    pub deployment: Option<String>,
    pub platform_secrets: Option<Arc<dyn PlatformSecrets>>,
    pub approver: Option<Arc<dyn Approver>>,
    pub extra_tools: Vec<Arc<dyn Tool>>,
    pub owner: Option<String>,
    pub mcp: crate::mcp::McpOptions,
    /// Resolves `delegates` without a fixed url (managed mode: the Manager).
    pub resolver: Option<Arc<dyn crate::delegate::Resolver>>,
    /// Queues async delegations (managed mode: the Manager); enables the
    /// `start_agent` tool.
    pub task_starter: Option<Arc<dyn crate::delegate::TaskStarter>>,
}

/// A run being driven by this process.
struct Live {
    rec: RunRecord,
    epoch: i64,
    started: std::time::Instant,
    /// Time spent waiting for approvals, which does not count towards
    /// `max_run_duration`.
    approval_wait_ms: std::sync::atomic::AtomicU64,
    /// Calls made so far per tool and arguments.
    calls: Mutex<BTreeMap<String, u32>>,
    /// The run was asked once to end through finish.
    nudged: bool,
    /// Per tool and arguments: the last error and how often in a row.
    errors: Mutex<BTreeMap<String, (String, u32)>>,
    /// Signatures of the latest turns (call, arguments and results), to
    /// notice a run repeating itself.
    turns: Vec<u64>,
    /// The run was told once that it repeats itself.
    stuck_nudged: bool,
    /// The run's trust right now: starts at the task's and only ever drops,
    /// when a tool whose result may carry untrusted content is called.
    current_trust: Mutex<Trust>,
}

impl Live {
    fn new(rec: RunRecord, epoch: i64, trust: Trust) -> Self {
        Self {
            rec,
            epoch,
            started: std::time::Instant::now(),
            approval_wait_ms: Default::default(),
            calls: Default::default(),
            nudged: false,
            errors: Default::default(),
            turns: vec![],
            stuck_nudged: false,
            current_trust: Mutex::new(trust),
        }
    }

    /// The run's trust right now.
    fn trust(&self) -> Trust {
        *self.current_trust.lock().unwrap()
    }

    /// Lowers the run's trust to untrusted if it isn't already, returning
    /// true the first time (so the caller logs it once).
    fn lower_trust(&self) -> bool {
        let mut t = self.current_trust.lock().unwrap();
        if *t == Trust::Untrusted {
            return false;
        }
        *t = Trust::Untrusted;
        true
    }

    /// Time the run has worked, excluding approval waits.
    fn worked(&self) -> Duration {
        let waited = self.approval_wait_ms.load(std::sync::atomic::Ordering::SeqCst);
        self.started.elapsed().saturating_sub(Duration::from_millis(waited))
    }
}

impl Agent {
    pub fn builder(cfg: AgentConfig, store: Arc<Store>) -> AgentBuilder {
        AgentBuilder {
            cfg,
            provider: None,
            tools: Vec::new(),
            log_sink: None,
            store,
            secrets: Secrets::default(),
            approver: Arc::new(DenyAll),
            owner: None,
            mcp: None,
            task_starter: None,
        }
    }

    /// Build from a loaded bundle: provider from `harness.json`, secrets from
    /// `secrets.json`, skills, permissions, budget and limits from the bundle.
    pub async fn from_bundle(bundle: &Bundle, store: Arc<Store>, opts: BundleOptions) -> Result<Agent, AgentError> {
        let secrets = crate::secrets::resolve_all(&bundle.secrets, opts.platform_secrets.as_deref())
            .await
            .map_err(|e| AgentError::Config(e.to_string()))?;
        let secrets = with_provider_key(bundle, secrets);
        let provider = crate::provider::from_harness(bundle, &secrets)?;
        let mut cfg = AgentConfig::new(bundle.name(), &bundle.system_prompt, &bundle.harness.model);
        cfg.skills = bundle.skills.clone();
        cfg.preload_skills = bundle.agent.preload_skills.clone();
        match bundle.harness.context_tokens {
            Some(n) => cfg.context_tokens = n,
            // Default: 64000, or half the model's window when that is smaller.
            None => {
                if let Some(w) = provider.context_window(&bundle.harness.model).await {
                    cfg.context_tokens = default_context_tokens(w);
                }
            }
        }
        cfg.temperature = bundle.harness.temperature;
        cfg.fallback_models = bundle.harness.fallback_models.clone();
        cfg.router = bundle.harness.router.clone();
        cfg.request_extra = request_extra(&bundle.harness);
        cfg.max_output_tokens = Some(
            bundle
                .harness
                .max_output_tokens
                .unwrap_or(default_limits::MAX_OUTPUT_TOKENS),
        );
        cfg.parallel_tool_calls = bundle.harness.parallel_tool_calls.unwrap_or(true);
        cfg.trace_tool_arguments = bundle.harness.trace_tool_arguments.unwrap_or(false);
        cfg.max_tool_result_chars = bundle
            .harness
            .max_tool_result_chars
            .unwrap_or(default_limits::MAX_TOOL_RESULT_CHARS);
        cfg.max_turns = bundle.agent.max_turns.unwrap_or(16);
        cfg.permissions = bundle.config.permissions.clone();
        if let Some(t) = bundle.config.permissions.approval_timeout.as_deref() {
            cfg.approval_timeout = duration_setting("permissions.approvalTimeout", t)?;
            if cfg.approval_timeout > default_limits::MAX_APPROVAL_TIMEOUT {
                return Err(AgentError::Config(format!(
                    "permissions.approvalTimeout {t:?} is longer than the maximum of 168h"
                )));
            }
        }
        cfg.budget = bundle.config.budget.clone();
        cfg.limits = bundle.config.limits.clone();
        cfg.memory = bundle.config.memory.clone();
        let limits = &bundle.config.limits;
        if let Some(t) = limits.max_run_duration.as_deref() {
            cfg.max_run_duration = duration_setting("limits.maxRunDuration", t)?;
        }
        if let Some(t) = limits.model_request_timeout.as_deref() {
            cfg.model_request_timeout = duration_setting("limits.modelRequestTimeout", t)?;
        }
        if let Some(t) = limits.tool_timeout.as_deref() {
            cfg.tool_timeout = duration_setting("limits.toolTimeout", t)?;
        }
        cfg.report_tokens = limits.report_tokens();
        if let Some(r) = &bundle.harness.result {
            cfg.result_schema = Some(crate::outcome::ResultSchema::new(r.schema.clone()).map_err(AgentError::Config)?);
        }
        let namespace = opts.namespace.unwrap_or_else(|| "default".into());
        cfg.namespace = namespace.clone();
        cfg.deployment = opts.deployment.unwrap_or_else(|| bundle.name().into());
        cfg.definition_digest = bundle.digest.clone();
        // An MCP call may take as long as limits.toolTimeout allows (0: no
        // limit), not the transport's own default.
        let mcp_opts = crate::mcp::McpOptions {
            call_timeout: if cfg.tool_timeout.is_zero() {
                Duration::from_secs(7 * 24 * 3600)
            } else {
                cfg.tool_timeout.max(opts.mcp.call_timeout)
            },
            connect_timeout: opts.mcp.connect_timeout,
        };
        let mut b = Agent::builder(cfg, store.clone())
            .provider(provider)
            .secrets(secrets.clone());
        if let Some(a) = opts.approver {
            b = b.approver(a);
        }
        if let Some(o) = opts.owner {
            b = b.owner(&o);
        }
        for t in opts.extra_tools {
            b = b.tool(t);
        }
        if !bundle.config.delegates.is_empty() {
            b = b.tool(Arc::new(crate::delegate::CallAgent::new(
                bundle.config.delegates.clone(),
                &namespace,
                opts.resolver.clone(),
                opts.task_starter.clone(),
                bundle.config.limits.clone(),
                store.clone(),
            )));
            if let Some(starter) = opts.task_starter.clone() {
                if bundle.config.delegates.iter().any(|d| d.url.is_none()) {
                    b = b.tool(Arc::new(crate::delegate::StartAgent::new(
                        bundle.config.delegates.clone(),
                        &namespace,
                        starter,
                        bundle.config.limits.clone(),
                        store.clone(),
                    )));
                }
            }
        }
        if let Some(starter) = opts.task_starter.clone() {
            b = b.task_starter(starter);
        }
        if !bundle.config.memory.is_empty() {
            b = b.tool(Arc::new(crate::memory::MemoryUpdate::new(
                store.clone(),
                &namespace,
                bundle.config.memory.clone(),
            )));
        }
        if let Some(m) = &bundle.mcp {
            let redactor = Redactor::new();
            redactor.register_all(&secrets);
            let (conns, tools) = crate::mcp::connect(
                &bundle.root,
                &m.servers,
                &bundle.config.tools,
                &secrets,
                &redactor,
                &mcp_opts,
            )
            .await
            .map_err(|e| AgentError::Config(e.to_string()))?;
            for t in tools {
                b = b.tool(t);
            }
            b = b.mcp(conns);
        }
        b.build()
    }

    pub fn config(&self) -> &AgentConfig {
        &self.cfg
    }

    pub fn store(&self) -> &Arc<Store> {
        &self.store
    }

    pub fn redactor(&self) -> &Redactor {
        &self.redactor
    }

    pub fn tools(&self) -> &ToolRegistry {
        &self.tools
    }

    /// The model provider this agent calls (`agen eval`'s judge call reuses
    /// it, rather than building a second one).
    pub fn provider(&self) -> &Arc<dyn ModelProvider> {
        &self.provider
    }

    pub fn owner(&self) -> &str {
        &self.owner
    }

    /// The agent's MCP servers, whether each is still connected, and how
    /// many tools it provides.
    pub fn tool_servers(&self) -> Vec<ToolServerStatus> {
        let Some(m) = &self.mcp else { return vec![] };
        let closed = m.closed_servers();
        let specs = self.tools.specs();
        m.servers()
            .into_iter()
            .map(|name| ToolServerStatus {
                name: name.to_string(),
                connected: !closed.contains(&name),
                tool_count: specs
                    .iter()
                    .filter(|t| t.name.strip_prefix(name).is_some_and(|r| r.starts_with('.')))
                    .count(),
            })
            .collect()
    }

    /// Problems that make this agent unable to work (e.g. a dead MCP server).
    pub fn health_problems(&self) -> Vec<String> {
        match &self.mcp {
            Some(m) => m
                .closed_servers()
                .into_iter()
                .map(|s| format!("mcp server {s} is disconnected"))
                .collect(),
            None => vec![],
        }
    }

    /// Start a new run. With a `task_id`, this is idempotent: a task that
    /// already has a run returns that run's result, or resumes it if it is
    /// unfinished (e.g. the task was re-leased after its Nest died).
    pub async fn run(&self, input: &str, opts: RunOptions) -> Result<RunResult, AgentError> {
        if !opts.task_id.is_empty() {
            if let Some(existing) = self
                .store
                .run_by_task(&self.cfg.namespace, &self.cfg.deployment, &opts.task_id)
                .await?
            {
                return self.resume(&existing.id, opts).await;
            }
        }
        match self.start_run(input, &opts).await {
            Err(AgentError::Store(StoreError::Conflict(_))) if !opts.task_id.is_empty() => {
                // Another instance created the task's run concurrently.
                match self
                    .store
                    .run_by_task(&self.cfg.namespace, &self.cfg.deployment, &opts.task_id)
                    .await?
                {
                    Some(existing) => self.resume(&existing.id, opts).await,
                    None => Err(AgentError::Store(StoreError::Conflict(
                        "conversation already has an unfinished run".into(),
                    ))),
                }
            }
            other => other,
        }
    }

    async fn start_run(&self, input: &str, opts: &RunOptions) -> Result<RunResult, AgentError> {
        let session = match (&opts.session_id, opts.singleton) {
            (Some(id), _) => self.store.get_session(id).await?,
            (None, false) if !opts.conversation_key.is_empty() => {
                self.store
                    .session_for_key(
                        &self.cfg.name,
                        &self.cfg.namespace,
                        &self.cfg.deployment,
                        &opts.conversation_key,
                    )
                    .await?
            }
            (None, true) => {
                self.store
                    .session_for_deployment(&self.cfg.name, &self.cfg.namespace, &self.cfg.deployment)
                    .await?
            }
            (None, false) => {
                self.store
                    .create_session(&self.cfg.name, &self.cfg.namespace, &self.cfg.deployment)
                    .await?
            }
        };
        let conversation_id = if opts.new_conversation {
            self.store.open_conversation(&session.id).await?
        } else {
            self.store.current_conversation(&session.id).await?
        };
        let parent = opts
            .traceparent
            .as_deref()
            .and_then(TraceContext::from_traceparent)
            .unwrap_or_else(TraceContext::new_root);
        let run_id = new_id();
        let rec = RunRecord {
            id: run_id.clone(),
            session_id: session.id.clone(),
            conversation_id: conversation_id.clone(),
            namespace: self.cfg.namespace.clone(),
            deployment: self.cfg.deployment.clone(),
            definition_digest: self.cfg.definition_digest.clone(),
            task_id: opts.task_id.clone(),
            requested_by: opts.requested_by.clone(),
            parent_run_id: opts.parent_run_id.clone(),
            root_run_id: if opts.root_run_id.is_empty() {
                run_id.clone()
            } else {
                opts.root_run_id.clone()
            },
            status: RunStatus::Running,
            input: self.redactor.redact(input),
            output: String::new(),
            error: String::new(),
            step: 0,
            usage: Usage::default(),
            trace_id: parent.trace_id.clone(),
            started_ms: now_ms(),
            ended_ms: None,
            labels: opts.labels.clone(),
            outcome: None,
        };
        let epoch = self.store.create_run(&rec, &self.owner).await?;
        let first = self.redact_message(&Message::user(input));
        self.store
            .checkpoint(&run_id, epoch, &conversation_id, Some(&first), None)
            .await?;
        self.drive(Live::new(rec, epoch, opts.trust), &parent, "agen.run", opts)
            .await
    }

    /// Take ownership of an unfinished run and continue it from its last
    /// checkpoint. Finished runs return their stored result.
    pub async fn resume(&self, run_id: &str, opts: RunOptions) -> Result<RunResult, AgentError> {
        let rec = self.store.get_run(run_id).await?;
        if rec.status.is_terminal() {
            return Ok(result_of(&rec));
        }
        let epoch = self.store.claim_run(run_id, &self.owner).await?;
        let mut parent = TraceContext {
            trace_id: rec.trace_id.clone(),
            span_id: String::new(),
        };
        // The previous owner died mid-run: close its span as interrupted and
        // hang the resumed run under it, so the trace keeps one root.
        if let Some(mut old) = self.store.unfinished_run_span(run_id).await? {
            old.end_ms = now_ms();
            old.status = "interrupted".into();
            if let Some(a) = old.attributes.as_object_mut() {
                a.insert("agen.run.interrupted_at_step".into(), rec.step.into());
            }
            self.store.insert_span(&old).await?;
            parent.span_id = old.span_id;
        }
        self.drive(Live::new(rec, epoch, opts.trust), &parent, "agen.run.resume", &opts)
            .await
    }

    async fn drive(
        &self,
        mut live: Live,
        parent: &TraceContext,
        span_name: &str,
        opts: &RunOptions,
    ) -> Result<RunResult, AgentError> {
        let tracer = Tracer::new(self.store.clone(), self.redactor.clone(), live.rec.id.clone());
        let mut span = tracer.start(parent, span_name);
        span.set("agen.run.id", live.rec.id.clone());
        span.set("agen.agent.name", self.cfg.name.clone());
        span.set("gen_ai.conversation.id", live.rec.conversation_id.clone());
        if !live.rec.task_id.is_empty() {
            span.set("agen.task.id", live.rec.task_id.clone());
        }
        if opts.attempt > 0 {
            span.set("agen.task.attempt", opts.attempt);
        }
        if !opts.conversation_key.is_empty() {
            span.set("agen.conversation.key", opts.conversation_key.clone());
        }
        for (k, v) in &live.rec.labels {
            span.set(&format!("agen.label.{k}"), v.clone());
        }
        tracer.record_open(&span).await;
        let started = std::time::Instant::now();
        let task = match (live.rec.task_id.is_empty(), opts.attempt) {
            (true, _) => String::new(),
            (false, a) if a > 1 => format!(" (task {}, attempt {a})", live.rec.task_id),
            (false, _) => format!(" (task {})", live.rec.task_id),
        };
        if live.rec.step == 0 {
            self.log("info", &format!("run {} started{task}", live.rec.id)).await;
        } else {
            self.log(
                "info",
                &format!(
                    "run {} resumed at step {}{task}: its previous owner stopped before finishing",
                    live.rec.id, live.rec.step
                ),
            )
            .await;
        }
        let outcome = self.drive_inner(&mut live, &tracer, &span.ctx, opts).await;
        if let Ok(Outcome::Suspended(wait_for)) = &outcome {
            // Not finished: the run stays open (no finish_run, no closing of
            // the pending call) exactly as if the process had died at this
            // point, except cleanly, so it resumes the same way.
            let wait_for = wait_for.clone();
            self.log(
                "info",
                &format!(
                    "run {} suspended at step {}: waiting for {wait_for}",
                    live.rec.id, live.rec.step
                ),
            )
            .await;
            tracer.end(span, "suspended").await;
            let mut result = result_of(&live.rec);
            result.wait_for = wait_for;
            return Ok(result);
        }
        let mut ended = match outcome {
            Ok(Outcome::Done(text)) => RunOutcome::new("done", "", &text),
            Ok(Outcome::Failed(err)) => RunOutcome::new("failed", failure_reason(&err), &err),
            Ok(Outcome::Cancelled) => RunOutcome::new("cancelled", "cancelled", ""),
            Ok(Outcome::Ended(o)) => o,
            Ok(Outcome::Suspended(_)) => unreachable!("handled above"),
            // Store failures (including losing ownership) leave the run as
            // it is: resumable, or owned by someone else.
            Err(AgentError::Store(e)) => {
                tracer.end(span, "error").await;
                return Err(AgentError::Store(e));
            }
            Err(e) => RunOutcome::new("failed", "error", &e.to_string()),
        };
        ended.summary = self.redactor.redact(&ended.summary);
        if let Some(d) = &ended.data {
            ended.data = Some(self.redactor.redact_json(d));
        }
        let (status, output, error) = match ended.status.as_str() {
            "failed" => (RunStatus::Failed, String::new(), ended.summary.clone()),
            "cancelled" => (RunStatus::Cancelled, String::new(), "cancelled".to_string()),
            _ => (RunStatus::Succeeded, ended.summary.clone(), String::new()),
        };
        // Never leave tool calls without results: the conversation must stay
        // valid for the next run.
        self.close_dangling_calls(&live, status).await?;
        span.set("gen_ai.usage.input_tokens", live.rec.usage.input_tokens);
        span.set("gen_ai.usage.output_tokens", live.rec.usage.output_tokens);
        span.set("agen.run.status", status.as_str());
        span.set("agen.outcome.status", ended.status.clone());
        if !ended.reason.is_empty() {
            span.set("agen.outcome.reason", ended.reason.clone());
        }
        self.store
            .finish_run(
                &live.rec.id,
                live.epoch,
                status,
                &output,
                &error,
                live.rec.usage,
                Some(&ended),
            )
            .await?;
        // 3.6, part: agen replay. Only a RecordingProvider (wired in for a
        // deployment with recordInteractions on) returns Some here; every
        // other provider is a no-op. Best-effort: a run's own result is
        // never held up by its cassette failing to save.
        if let Some(cassette) = self.provider.recorded_cassette() {
            match serde_json::to_string(&cassette) {
                Ok(data) => {
                    if let Err(e) = self.store.save_cassette(&live.rec.id, &data).await {
                        self.log(
                            "warn",
                            &format!("run {}: could not save its cassette: {e}", live.rec.id),
                        )
                        .await;
                    }
                }
                Err(e) => {
                    self.log(
                        "warn",
                        &format!("run {}: could not encode its cassette: {e}", live.rec.id),
                    )
                    .await;
                }
            }
        }
        let summary = format!(
            "run {} {} in {} ms, {} input / {} output tokens{}",
            live.rec.id,
            status.as_str(),
            started.elapsed().as_millis(),
            live.rec.usage.input_tokens,
            live.rec.usage.output_tokens,
            if error.is_empty() {
                String::new()
            } else {
                format!(": {error}")
            }
        );
        self.log(if status == RunStatus::Failed { "error" } else { "info" }, &summary)
            .await;
        tracer
            .end(span, if status == RunStatus::Succeeded { "ok" } else { "error" })
            .await;
        live.rec.status = status;
        live.rec.output = output;
        live.rec.error = error;
        live.rec.outcome = Some(ended);
        Ok(result_of(&live.rec))
    }

    /// Tool calls of the run's last assistant turn that have no result yet.
    async fn pending_calls(&self, run_id: &str) -> Result<(Vec<ToolCall>, Option<String>), AgentError> {
        let msgs = self.store.run_messages(run_id).await?;
        let Some(last_idx) = msgs.iter().rposition(|m| m.role == Role::Assistant) else {
            return Ok((vec![], None));
        };
        let last = &msgs[last_idx];
        if last.tool_calls.is_empty() {
            // A message after the answer (a nudge) asks for another turn.
            if msgs[last_idx + 1..].iter().any(|m| m.role == Role::User) {
                return Ok((vec![], None));
            }
            return Ok((vec![], Some(last.content.clone())));
        }
        let answered: Vec<&str> = msgs[last_idx + 1..]
            .iter()
            .filter_map(|m| m.tool_call_id.as_deref())
            .collect();
        let pending = last
            .tool_calls
            .iter()
            .filter(|c| !answered.contains(&c.id.as_str()))
            .cloned()
            .collect();
        Ok((pending, None))
    }

    async fn close_dangling_calls(&self, live: &Live, status: RunStatus) -> Result<(), AgentError> {
        let (pending, _) = self.pending_calls(&live.rec.id).await?;
        let reason = match status {
            RunStatus::Cancelled => "error: cancelled before this tool call completed",
            _ => "error: the run ended before this tool call completed",
        };
        for c in pending {
            self.store
                .checkpoint(
                    &live.rec.id,
                    live.epoch,
                    &live.rec.conversation_id,
                    Some(&Message::tool(&c.id, reason)),
                    None,
                )
                .await?;
        }
        Ok(())
    }

    async fn drive_inner(
        &self,
        live: &mut Live,
        tracer: &Tracer,
        ctx: &TraceContext,
        opts: &RunOptions,
    ) -> Result<Outcome, AgentError> {
        loop {
            if opts.cancel.is_cancelled() {
                return Ok(Outcome::Cancelled);
            }
            let (pending, final_text) = self.pending_calls(&live.rec.id).await?;
            if let Some(text) = final_text {
                if self.cfg.result_schema.is_some() && self.finish {
                    // The result must come through finish: ask once.
                    if live.nudged {
                        return Ok(Outcome::Ended(RunOutcome::new(
                            "failed",
                            "error",
                            "the agent ended without calling finish with its result",
                        )));
                    }
                    live.nudged = true;
                    self.add_user_note(
                        live,
                        "[agen] This agent must end by calling finish with data matching the result schema. Call finish now.",
                    )
                    .await?;
                    continue;
                }
                return Ok(Outcome::Done(text));
            }
            if pending.is_empty() {
                if let Some(o) = self.finished(&live.rec.id).await? {
                    return Ok(Outcome::Ended(o));
                }
            }
            // Calls that may wait for an approval run one at a time, so the
            // run's waiting state and the approval order stay simple.
            let concurrent = self.cfg.parallel_tool_calls
                && pending
                    .iter()
                    .all(|c| self.policy(opts, &c.name, live.trust()) != Action::Ask);
            let width = if concurrent { pending.len().max(1) } else { 1 };
            let shared: &Live = live;
            let mut results = futures_util::stream::iter(pending.into_iter().map(|call| async move {
                if opts.cancel.is_cancelled() {
                    return Ok(None);
                }
                let result = self.execute_call(shared, &call, tracer, ctx, opts).await?;
                Ok::<_, AgentError>(Some((call.id, result)))
            }))
            .buffered(width);
            // Results are recorded in call order as they become available. A
            // call that asks to suspend (3.3) gets no tool message: it stays
            // pending, exactly as if the process had died here, and is
            // retried from scratch once the run is driven again.
            let mut suspended: Option<String> = None;
            while let Some(r) = results.next().await {
                let Some((id, (result, status))) = r? else {
                    return Ok(Outcome::Cancelled);
                };
                if status == "suspended" {
                    if suspended.is_none() {
                        suspended = Some(result);
                    }
                    continue;
                }
                let msg = self.redact_message(&Message::tool(&id, result));
                self.store
                    .checkpoint(
                        &shared.rec.id,
                        shared.epoch,
                        &shared.rec.conversation_id,
                        Some(&msg),
                        None,
                    )
                    .await?;
            }
            drop(results);
            if let Some(wait_for) = suspended {
                return Ok(Outcome::Suspended(wait_for));
            }
            if opts.cancel.is_cancelled() {
                return Ok(Outcome::Cancelled);
            }
            if let Some(o) = self.finished(&live.rec.id).await? {
                return Ok(Outcome::Ended(o));
            }
            if self.repeating(live).await? {
                if live.stuck_nudged {
                    let why = format!(
                        "stuck: the same step gave the same result {} times in a row",
                        self.cfg.limits.stuck_repeats()
                    );
                    return self.report(live, tracer, ctx, opts, "stuck", why).await;
                }
                live.stuck_nudged = true;
                live.turns.clear();
                self.log(
                    "warn",
                    &format!("run {}: repeating the same step; nudging it", live.rec.id),
                )
                .await;
                self.add_user_note(
                    live,
                    "[agen] You have made the same call with the same result several times in a row. It will not change. Try a different approach, or call finish to report where you are.",
                )
                .await?;
            }
            let max_turns = narrow_max_turns(
                self.cfg.max_turns,
                opts.overrides.as_ref().and_then(|o| o.budget.as_ref()),
            );
            if live.rec.step >= max_turns as i64 {
                let why = format!("max_turns ({max_turns}) exceeded");
                return self.report(live, tracer, ctx, opts, "max_turns", why).await;
            }
            let max = narrow_max_run_duration(
                self.cfg.max_run_duration,
                opts.overrides.as_ref().and_then(|o| o.budget.as_ref()),
            );
            if !max.is_zero() && live.worked() > max {
                let why = format!("max_run_duration ({}s) exceeded", max.as_secs());
                return self.report(live, tracer, ctx, opts, "duration", why).await;
            }
            if let Some(e) = self.budget_exceeded(opts, &live.rec.usage) {
                return self.report(live, tracer, ctx, opts, "budget", e).await;
            }
            self.drain_inbox(live).await;
            // "join" (3.4, part): a turn that ends with unfinished joined
            // children (start_agent without detach: true) suspends instead
            // of asking the model again — freeing the instance, same as
            // call_agent's own inline-wait-then-suspend. Each child
            // finishing wakes it (any one, not a specific one: the Hub
            // matches wait_for "children" against every "child" row, not
            // just an exact "child:<id>"). A failed check never suspends
            // on its own, same as `finish`'s.
            if !live.rec.task_id.is_empty() {
                if let Some(starter) = &self.task_starter {
                    match starter.unfinished_children(&live.rec.task_id).await {
                        Ok(n) if n > 0 => return Ok(Outcome::Suspended("children".into())),
                        Ok(_) => {}
                        Err(e) => {
                            self.log(
                                "warn",
                                &format!("run {}: could not check for unfinished children: {e}", live.rec.id),
                            )
                            .await;
                        }
                    }
                }
            }
            // Hold (3.4, part): a task PauseTask has held suspends here
            // instead of calling the model again, with wait_for "resume".
            // A deadline (if any) is the Hub's job to wake it by, lazily
            // for a queued task and actively for this waiting one
            // (WakeExpiredHolds); the engine only needs the bool.
            if !live.rec.task_id.is_empty() {
                if let Some(starter) = &self.task_starter {
                    match starter.is_held(&live.rec.task_id).await {
                        Ok(true) => return Ok(Outcome::Suspended("resume".into())),
                        Ok(false) => {}
                        Err(e) => {
                            self.log("warn", &format!("run {}: could not check if held: {e}", live.rec.id))
                                .await;
                        }
                    }
                }
            }
            let req = ModelRequest {
                model: self.cfg.model.clone(),
                system: self.redactor.redact(&self.system_prompt(opts).await?),
                messages: self.request_messages(&live.rec).await?,
                tools: self.visible_tools(live, opts),
                temperature: self.cfg.temperature,
                max_output_tokens: self.output_cap(opts, &live.rec.usage),
                extra: Default::default(),
            };
            let mut span = tracer.start(ctx, "gen_ai.chat");
            span.set("gen_ai.operation.name", "chat");
            span.set("gen_ai.provider.name", self.provider.name().to_string());
            span.set("gen_ai.request.model", self.cfg.model.clone());
            span.set("agen.request.messages", req.messages.len() as u64);
            tracer.record_open(&span).await;
            // An empty answer only counts as the end of the work after tool
            // results; a run that got nothing at all asks again.
            let after_tools = req.messages.iter().any(|m| m.role == provider::Role::Tool);
            let mut wasted = Usage::default();
            let resp = tokio::select! {
                r = self.complete_with_fallback(req, opts, &mut wasted) => r,
                _ = opts.cancel.cancelled() => {
                    tracer.end(span, "cancelled").await;
                    return Ok(Outcome::Cancelled);
                }
            };
            let (resp, sent) = match resp {
                Ok(r) => r,
                Err(e) => {
                    live.rec.usage.add(wasted);
                    span.set("error.type", e.to_string());
                    tracer.end(span, "error").await;
                    return Err(e.into());
                }
            };
            if sent.model != self.cfg.model {
                span.set("gen_ai.request.model", sent.model.clone());
                span.set("agen.fallback", true);
            }
            if let Some(n) = sent.output_retry {
                span.set("agen.output.retried_with", n as u64);
            }
            if !resp.model.is_empty() {
                span.set("gen_ai.response.model", resp.model.clone());
            }
            if let Some(t) = &sent.tier {
                span.set("agen.router.tier", t.clone());
            }
            if let Some(route) = &resp.route {
                for (k, v) in route.as_object().into_iter().flatten() {
                    if let Some(v) = v.as_str() {
                        span.set(&format!("agen.router.{k}"), v.to_string());
                    }
                }
            }
            if resp.is_empty() && !empty_ends_work(after_tools, &resp) {
                // Every model tried came back with nothing to act on.
                let why = format!(
                    "no_answer: the model returned no text and no tool calls (finish_reason {:?}{})",
                    resp.finish_reason,
                    if resp.finish_reason == "length" {
                        "; raise maxOutputTokens, reasoning may have used it all"
                    } else {
                        ""
                    }
                );
                live.rec.usage.add(resp.usage);
                span.set("error.type", why.clone());
                tracer.end(span, "error").await;
                return Ok(Outcome::Failed(why));
            }
            span.set("gen_ai.usage.input_tokens", resp.usage.input_tokens);
            span.set("gen_ai.usage.output_tokens", resp.usage.output_tokens);
            span.set("gen_ai.response.tool_calls", resp.tool_calls.len() as u64);
            span.set("gen_ai.response.finish_reasons", resp.finish_reason.clone());
            if resp.usage.cost_usd > 0.0 {
                span.set("agen.usage.cost_usd", resp.usage.cost_usd);
            }
            if resp.finish_reason == "length" {
                self.log(
                    "warn",
                    &format!("run {}: model output hit max_output_tokens", live.rec.id),
                )
                .await;
            }

            live.rec.usage.add(resp.usage);
            live.rec.step += 1;
            let assistant = self.redact_message(&Message::assistant(resp.text, resp.tool_calls));
            let stored = self
                .store
                .checkpoint(
                    &live.rec.id,
                    live.epoch,
                    &live.rec.conversation_id,
                    Some(&assistant),
                    Some((live.rec.step, RunStatus::Running, live.rec.usage)),
                )
                .await;
            // The reply's place in the conversation, so a trace viewer can
            // show what this call sent and received.
            if let Ok(Some(seq)) = &stored {
                span.set("agen.message.seq", *seq);
            }
            tracer.end(span, if stored.is_ok() { "ok" } else { "error" }).await;
            stored?;
        }
    }

    /// The tools the model is offered: all but those its permissions deny.
    fn visible_tools(&self, live: &Live, opts: &RunOptions) -> Vec<crate::provider::ToolSpec> {
        self.tools
            .specs()
            .into_iter()
            .filter(|t| self.policy(opts, &t.name, live.trust()) != Action::Deny)
            .collect()
    }

    /// Whether the run's latest turns are the same step with the same
    /// results (`limits.stuckRepeats` in a row). Records the latest turn.
    async fn repeating(&self, live: &mut Live) -> Result<bool, AgentError> {
        let n = self.cfg.limits.stuck_repeats() as usize;
        if n == 0 {
            return Ok(false);
        }
        let msgs = self.store.run_messages(&live.rec.id).await?;
        let Some(last) = msgs.iter().rposition(|m| m.role == Role::Assistant) else {
            return Ok(false);
        };
        if msgs[last].tool_calls.is_empty() {
            return Ok(false);
        }
        let mut h = Sha256::new();
        h.update(msgs[last].content.as_bytes());
        for c in &msgs[last].tool_calls {
            h.update(c.name.as_bytes());
            h.update(args_hash(&c.arguments).as_bytes());
        }
        for m in &msgs[last + 1..] {
            h.update([0u8]);
            h.update(m.content.as_bytes());
        }
        let sig = u64::from_le_bytes(h.finalize()[..8].try_into().unwrap());
        live.turns.push(sig);
        if live.turns.len() > n {
            live.turns.remove(0);
        }
        Ok(live.turns.len() == n && live.turns.iter().all(|s| *s == sig))
    }

    /// The permission for a tool call. `finish` only ends the run, so it is
    /// never asked for or denied. A task's overrides can only narrow this:
    /// a tool outside its `tools` allowlist is denied outright, and its
    /// extra `permissions` rules (deny/ask only) are checked alongside the
    /// bundle's own.
    fn policy(&self, opts: &RunOptions, tool: &str, trust: Trust) -> Action {
        if self.finish && tool == crate::outcome::FINISH {
            return Action::Allow;
        }
        // Artifacts are engine mechanics available to every agent, like
        // finish; they are never gated by the bundle's own permissions.
        if tool == crate::artifacts::SAVE_ARTIFACT || tool == crate::artifacts::READ_ARTIFACT {
            return Action::Allow;
        }
        let Some(ov) = &opts.overrides else {
            return permissions::evaluate(&self.cfg.permissions, tool, trust);
        };
        if !override_allows_tool(ov, tool) {
            return Action::Deny;
        }
        if ov.permissions.is_empty() {
            return permissions::evaluate(&self.cfg.permissions, tool, trust);
        }
        let mut rules: Vec<PermissionRule> = ov
            .permissions
            .iter()
            .filter_map(|r| {
                let action = match r.action.as_str() {
                    "deny" => Some(Action::Deny),
                    "ask" => Some(Action::Ask),
                    _ => None,
                };
                action.map(|action| PermissionRule {
                    tool: r.tool.clone(),
                    action,
                    when: None,
                })
            })
            .collect();
        rules.extend(self.cfg.permissions.rules.iter().cloned());
        let combined = Permissions {
            default: self.cfg.permissions.default,
            rules,
            approval_timeout: self.cfg.permissions.approval_timeout.clone(),
        };
        permissions::evaluate(&combined, tool, trust)
    }

    /// The outcome of an accepted `finish` call in the run's last turn.
    async fn finished(&self, run_id: &str) -> Result<Option<RunOutcome>, AgentError> {
        if !self.finish {
            return Ok(None);
        }
        let msgs = self.store.run_messages(run_id).await?;
        let Some(last_idx) = msgs.iter().rposition(|m| m.role == Role::Assistant) else {
            return Ok(None);
        };
        for call in msgs[last_idx]
            .tool_calls
            .iter()
            .filter(|c| c.name == crate::outcome::FINISH)
        {
            let accepted = msgs[last_idx + 1..]
                .iter()
                .any(|m| m.tool_call_id.as_deref() == Some(&call.id) && m.content == crate::outcome::FINISHED);
            if accepted {
                if let Ok(o) = crate::outcome::from_finish(&call.arguments, self.cfg.result_schema.as_ref()) {
                    return Ok(Some(o));
                }
            }
        }
        Ok(None)
    }

    /// Record an engine note to the model as a user message of the run.
    /// Drains the task's run_inbox (3.4/3.9, part) into new user messages
    /// before the next model call, so a signal sent while this run is
    /// between turns is not lost — "picked up before its next model call,"
    /// same as a steered task's would be once that queue mode exists.
    /// Best-effort: managed mode only, and a failed drain (the Hub is
    /// unreachable, say) is logged and the turn goes on without it, the
    /// same way every other platform-only feature degrades standalone.
    async fn drain_inbox(&self, live: &Live) {
        if live.rec.task_id.is_empty() {
            return;
        }
        let Some(starter) = &self.task_starter else {
            return;
        };
        let entries = match starter.drain_inbox(&live.rec.task_id).await {
            Ok(e) => e,
            Err(e) => {
                self.log(
                    "warn",
                    &format!("run {}: could not drain its task inbox: {e}", live.rec.id),
                )
                .await;
                return;
            }
        };
        for entry in entries {
            let kind = entry.get("kind").and_then(Value::as_str).unwrap_or("");
            let payload = entry.get("payload").and_then(Value::as_str).unwrap_or("");
            // Other kinds (today only "child", handled by call_agent's own
            // re-check of the child's status on resume, never reaches
            // here) are claimed by the drain but turn into nothing yet.
            let text = match kind {
                "signal" => {
                    let from = entry.get("ref").and_then(Value::as_str).unwrap_or("");
                    format!("[Signal from task {from}] {payload}")
                }
                // Graceful cancel (3.4, part): a last turn to call finish
                // before the Hub's own hard CancelTask/CancelTree stops it
                // regardless, once its grace period passes.
                "cancel" => {
                    format!("[This task is being cancelled: {payload}. Wrap up now and call finish with whatever you have, or it will be stopped shortly.]")
                }
                // "steer" queue mode (3.9, part): a new submission to this
                // conversation delivered into the run already in progress,
                // the same fire-and-forget shape as a signal (no separate
                // task of its own).
                "steer" => {
                    format!("[New input for this conversation] {payload}")
                }
                _ => continue,
            };
            if let Err(e) = self.add_user_note(live, &text).await {
                self.log(
                    "warn",
                    &format!("run {}: could not record a drained inbox entry: {e}", live.rec.id),
                )
                .await;
            }
        }
    }

    async fn add_user_note(&self, live: &Live, text: &str) -> Result<(), AgentError> {
        self.store
            .checkpoint(
                &live.rec.id,
                live.epoch,
                &live.rec.conversation_id,
                Some(&Message::user(text)),
                None,
            )
            .await?;
        Ok(())
    }

    /// The run stopped at a limit (`reason`): one last call without tools
    /// asks the model for its report, and the run ends partial with it.
    /// With `limits.reportTokens` 0 the run fails as it did before.
    async fn report(
        &self,
        live: &mut Live,
        tracer: &Tracer,
        ctx: &TraceContext,
        opts: &RunOptions,
        reason: &str,
        why: String,
    ) -> Result<Outcome, AgentError> {
        if self.cfg.report_tokens == 0 {
            return Ok(Outcome::Failed(why));
        }
        self.log("warn", &format!("run {}: {why}; asking for a report", live.rec.id))
            .await;
        self.add_user_note(
            live,
            &format!(
                "[agen] This run has reached a limit ({why}) and must stop now. You cannot call tools any more. Write your report: what you did, what you found, what is left undone and what is needed to finish."
            ),
        )
        .await?;
        let mut extra = serde_json::Map::new();
        extra.insert("tool_choice".into(), Value::String("none".into()));
        let req = ModelRequest {
            model: self.cfg.model.clone(),
            system: self.redactor.redact(&self.system_prompt(opts).await?),
            messages: self.request_messages(&live.rec).await?,
            tools: self.visible_tools(live, opts),
            temperature: self.cfg.temperature,
            max_output_tokens: Some(self.cfg.report_tokens),
            extra,
        };
        let mut span = tracer.start(ctx, "gen_ai.chat");
        span.set("gen_ai.operation.name", "chat");
        span.set("gen_ai.provider.name", self.provider.name().to_string());
        span.set("gen_ai.request.model", self.cfg.model.clone());
        span.set("agen.report", reason.to_string());
        tracer.record_open(&span).await;
        let mut wasted = Usage::default();
        let resp = tokio::select! {
            r = self.complete_with_fallback(req, opts, &mut wasted) => r,
            _ = opts.cancel.cancelled() => {
                tracer.end(span, "cancelled").await;
                return Ok(Outcome::Cancelled);
            }
        };
        let text = match resp {
            Ok((resp, _)) => {
                span.set("gen_ai.usage.input_tokens", resp.usage.input_tokens);
                span.set("gen_ai.usage.output_tokens", resp.usage.output_tokens);
                live.rec.usage.add(resp.usage);
                // Tool calls are not run any more; only the text counts.
                let assistant = self.redact_message(&Message::assistant(resp.text.clone(), vec![]));
                self.store
                    .checkpoint(
                        &live.rec.id,
                        live.epoch,
                        &live.rec.conversation_id,
                        Some(&assistant),
                        Some((live.rec.step, RunStatus::Running, live.rec.usage)),
                    )
                    .await?;
                tracer.end(span, "ok").await;
                resp.text
            }
            Err(e) => {
                live.rec.usage.add(wasted);
                span.set("error.type", e.to_string());
                tracer.end(span, "error").await;
                String::new()
            }
        };
        Ok(Outcome::Ended(RunOutcome::new("partial", reason, &text)))
    }

    /// Conversation history for the model: earlier runs' messages (trimmed
    /// oldest-first to fit `context_tokens`, starting at a user message) plus
    /// the current run's messages (oldest tool results shortened when they
    /// alone exceed it), with orphaned tool calls answered.
    async fn request_messages(&self, rec: &RunRecord) -> Result<Vec<Message>, AgentError> {
        let all = self.store.messages(&rec.conversation_id).await?;
        let current = self.store.run_messages(&rec.id).await?;
        let split = all.len().saturating_sub(current.len());
        let mut history: Vec<Message> = all[..split].to_vec();
        let current_tokens: u64 = current.iter().map(message_tokens).sum();
        let budget = self.cfg.context_tokens.saturating_sub(current_tokens);
        let mut total: u64 = history.iter().map(message_tokens).sum();
        let mut drop = 0;
        while total > budget && drop < history.len() {
            total -= message_tokens(&history[drop]);
            drop += 1;
        }
        while drop < history.len() && history[drop].role != Role::User {
            drop += 1;
        }
        history.drain(..drop);
        history.extend(compact_run(current, self.cfg.context_tokens));
        Ok(answer_orphans(history))
    }

    /// The request for one model (extras, and a routing model's tier).
    fn request_for(&self, base: &ModelRequest, model: &str, opts: &RunOptions) -> (ModelRequest, Sent) {
        let mut req = base.clone();
        req.model = model.to_string();
        req.extra = self.cfg.request_extra.clone();
        req.extra.extend(base.extra.clone());
        let mut tier = None;
        if let Some(kind) = crate::provider::router::router_for(model) {
            let settings = self.cfg.router.clone().unwrap_or_default();
            tier = settings.tier_for(opts.model_tier.as_deref(), opts.near_budget, kind);
            if tier.is_some() || !settings.allowed_models.is_empty() || !settings.excluded_models.is_empty() {
                let plugin = settings.plugin(kind, tier.as_deref());
                match req.extra.get_mut("plugins").and_then(Value::as_array_mut) {
                    Some(list) => list.push(plugin),
                    None => {
                        req.extra.insert("plugins".into(), Value::Array(vec![plugin]));
                    }
                }
            }
        }
        (
            req,
            Sent {
                model: model.to_string(),
                tier,
                output_retry: None,
            },
        )
    }

    /// One model turn: the configured model (retried), then each fallback
    /// model, until one answers. An empty answer is asked once more before
    /// moving on. Account errors (bad key, no credit) end it at once: another
    /// model on the same account will not help.
    async fn complete_with_fallback(
        &self,
        base: ModelRequest,
        opts: &RunOptions,
        wasted: &mut Usage,
    ) -> Result<(provider::ModelResponse, Sent), ProviderError> {
        let after_tools = base.messages.iter().any(|m| m.role == provider::Role::Tool);
        let models: Vec<String> = std::iter::once(self.cfg.model.clone())
            .chain(self.cfg.fallback_models.iter().cloned())
            .collect();
        let mut last: Option<Result<(provider::ModelResponse, Sent), ProviderError>> = None;
        let mut spent = Usage::default();
        for (i, model) in models.iter().enumerate() {
            if i > 0 {
                let why = match &last {
                    Some(Err(e)) => e.to_string(),
                    _ => "no answer".into(),
                };
                self.log(
                    "warn",
                    &format!("model {} failed ({why}); trying {model}", models[i - 1]),
                )
                .await;
            }
            let (mut req, mut sent) = self.request_for(&base, model, opts);
            for attempt in 0..2 {
                match self.complete_with_retry(&req, opts).await {
                    Ok(r) if attempt == 0 && output_cut(&r) => {
                        // The output limit ran out before a usable answer
                        // (often spent on reasoning): once more with more room.
                        spent.add(r.usage);
                        let cur = req.max_output_tokens.unwrap_or(default_limits::MAX_OUTPUT_TOKENS);
                        let more = cur.saturating_mul(2).clamp(cur, MAX_RETRY_OUTPUT_TOKENS);
                        self.log(
                            "warn",
                            &format!("model {model} hit its output limit ({cur} tokens) without a usable answer; asking again with {more}"),
                        )
                        .await;
                        req.max_output_tokens = Some(more);
                        sent.output_retry = Some(more);
                        continue;
                    }
                    Ok(mut r) if r.is_empty() && !empty_ends_work(after_tools, &r) => {
                        spent.add(r.usage);
                        if attempt == 0 {
                            self.log("warn", &format!("model {model} returned no answer; asking again"))
                                .await;
                            continue;
                        }
                        r.usage = spent;
                        last = Some(Ok((r, sent.clone())));
                    }
                    Ok(mut r) => {
                        spent.add(r.usage);
                        r.usage = spent;
                        return Ok((r, sent));
                    }
                    // Another model on the same account will not help; a test
                    // script that did not match must not be hidden by one.
                    Err(e @ (ProviderError::Account(_) | ProviderError::Script(_))) => {
                        *wasted = spent;
                        return Err(e);
                    }
                    Err(e) => last = Some(Err(e)),
                }
                break;
            }
        }
        let last = last.expect("at least one model");
        if last.is_err() {
            *wasted = spent;
        }
        last
    }

    async fn complete_with_retry(
        &self,
        req: &ModelRequest,
        opts: &RunOptions,
    ) -> Result<provider::ModelResponse, ProviderError> {
        let stream = Arc::new(Mutex::new(StreamRedactor::new(self.redactor.clone())));
        let emitted = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let on_delta = opts.on_delta.clone();
        let (s2, e2) = (stream.clone(), emitted.clone());
        let sink = move |d: &str| {
            if let Some(cb) = &on_delta {
                let out = s2.lock().unwrap().push(d);
                if !out.is_empty() {
                    e2.store(true, std::sync::atomic::Ordering::SeqCst);
                    cb(&out);
                }
            }
        };
        let mut attempt: u32 = 0;
        let limit = self.cfg.model_request_timeout;
        loop {
            let result = if limit.is_zero() {
                self.provider.complete(req, &sink).await
            } else {
                tokio::time::timeout(limit, self.provider.complete(req, &sink))
                    .await
                    .unwrap_or_else(|_| {
                        Err(ProviderError::Request(format!(
                            "no complete response within {}s (limits.modelRequestTimeout)",
                            limit.as_secs()
                        )))
                    })
            };
            let wait = match &result {
                Err(ProviderError::Retryable(_)) if attempt < 5 => Some(backoff(attempt)),
                Err(ProviderError::RetryAfter(_, secs)) if attempt < 5 => Some(
                    Duration::from_secs(*secs)
                        .max(backoff(attempt))
                        .min(Duration::from_secs(60)),
                ),
                _ => None,
            };
            match wait {
                Some(wait) => {
                    attempt += 1;
                    let err = result.err().map(|e| e.to_string()).unwrap_or_default();
                    self.log("warn", &format!("provider retry {attempt} in {wait:?}: {err}"))
                        .await;
                    stream.lock().unwrap().reset();
                    if emitted.swap(false, std::sync::atomic::Ordering::SeqCst) {
                        if let Some(reset) = &opts.on_reset {
                            reset();
                        }
                    }
                    tokio::time::sleep(wait).await;
                }
                None => {
                    if result.is_ok() {
                        if let Some(cb) = &opts.on_delta {
                            let tail = stream.lock().unwrap().finish();
                            if !tail.is_empty() {
                                cb(&tail);
                            }
                        }
                    } else {
                        // A fallback model (or the failure) follows: what this
                        // attempt streamed is not the answer.
                        stream.lock().unwrap().reset();
                        if emitted.swap(false, std::sync::atomic::Ordering::SeqCst) {
                            if let Some(reset) = &opts.on_reset {
                                reset();
                            }
                        }
                    }
                    return result;
                }
            }
        }
    }

    /// The output limit for the next model turn: the configured maximum,
    /// lowered so the turn's output cannot take the run past its token budget.
    /// Without a configured maximum the remaining budget is only sent once it
    /// is small (providers reject or pre-charge large limits).
    fn output_cap(&self, opts: &RunOptions, u: &Usage) -> Option<u32> {
        const UNSET_CEILING: u32 = 8192;
        let budget = narrow_budget(
            &self.cfg.budget,
            opts.overrides.as_ref().and_then(|o| o.budget.as_ref()),
        );
        let remaining = budget.max_tokens_per_run.map(|max| {
            u32::try_from(max.saturating_sub(u.total_tokens()))
                .unwrap_or(u32::MAX)
                .max(1)
        });
        match (self.cfg.max_output_tokens, remaining) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (None, Some(b)) if b <= UNSET_CEILING => Some(b),
            (a, _) => a,
        }
    }

    fn budget_exceeded(&self, opts: &RunOptions, u: &Usage) -> Option<String> {
        let budget = narrow_budget(
            &self.cfg.budget,
            opts.overrides.as_ref().and_then(|o| o.budget.as_ref()),
        );
        if let Some(max) = budget.max_tokens_per_run {
            if u.total_tokens() >= max {
                return Some(format!(
                    "budget_exceeded: {} tokens used, limit {max}",
                    u.total_tokens()
                ));
            }
        }
        // Only enforceable when the provider reports cost (OpenRouter does).
        if let Some(max) = budget.max_usd_per_run {
            if u.cost_usd >= max {
                return Some(format!("budget_exceeded: ${:.4} used, limit ${max}", u.cost_usd));
            }
        }
        None
    }

    /// The bundle's system prompt, with skills preloaded (the bundle's
    /// `preloadSkills` plus any a task's overrides add), and the task's
    /// `overrides.systemAddendum`, if any, appended last.
    async fn system_prompt(&self, opts: &RunOptions) -> Result<String, AgentError> {
        let extra_preload: &[String] = opts.overrides.as_ref().map(|o| o.skills.as_slice()).unwrap_or(&[]);
        let mut s = self.cfg.system_prompt.clone();
        let (pre, lazy): (Vec<&Skill>, Vec<&Skill>) = self
            .cfg
            .skills
            .iter()
            .partition(|sk| self.cfg.preload_skills.contains(&sk.name) || extra_preload.contains(&sk.name));
        for sk in pre {
            s.push_str(&format!("\n\n## Skill: {}\n{}\n", sk.name, sk.body.trim()));
        }
        if !lazy.is_empty() {
            s.push_str("\n\n## Skills\nCall load_skill with a name to read a skill before using it.\n");
            for sk in lazy {
                s.push_str(&format!("- {}: {}\n", sk.name, sk.description));
            }
        }
        if let Some(addendum) = opts
            .overrides
            .as_ref()
            .map(|o| o.system_addendum.trim())
            .filter(|a| !a.is_empty())
        {
            s.push_str("\n\n");
            s.push_str(addendum);
        }
        if !self.cfg.memory.is_empty() {
            s.push_str(&crate::memory::render(&self.store, &self.cfg.namespace, &self.cfg.memory).await?);
        }
        Ok(s)
    }

    fn redact_message(&self, m: &Message) -> Message {
        let mut m = m.clone();
        m.content = self.redactor.redact(&m.content);
        for c in &mut m.tool_calls {
            c.arguments = self.redactor.redact_json(&c.arguments);
        }
        m
    }

    /// Operational log line (redacted), to the Store and the log sink. Without
    /// a sink, warnings and errors go to stderr; info lines only to the Store
    /// (embedded apps should not get a stderr line per run).
    async fn log(&self, level: &str, msg: &str) {
        let msg = self.redactor.redact(msg);
        match &self.log_sink {
            Some(sink) => sink(level, &msg),
            None if level != "info" => eprintln!("agen [{level}] {msg}"),
            None => {}
        }
        let _ = self
            .store
            .append_log(&self.owner, &self.cfg.namespace, &self.cfg.deployment, level, &msg)
            .await;
    }

    /// Run one tool call and return the text for the tool message.
    async fn execute_call(
        &self,
        live: &Live,
        call: &ToolCall,
        tracer: &Tracer,
        ctx: &TraceContext,
        opts: &RunOptions,
    ) -> Result<(String, &'static str), AgentError> {
        let mut span = tracer.start(ctx, "agen.tool");
        span.set("gen_ai.tool.name", call.name.clone());
        span.set("gen_ai.tool.call.id", call.id.clone());
        if call.name == crate::delegate::CALL_AGENT || call.name == crate::delegate::START_AGENT {
            // Which agent it waits for (live views draw the call).
            if let Some(a) = call.arguments.get("agent").and_then(Value::as_str) {
                span.set("agen.delegate.target", a.to_string());
            }
        }
        if self.cfg.trace_tool_arguments {
            let mut args = self.redactor.redact_json(&call.arguments).to_string();
            if args.len() > 4096 {
                let mut cut = 4096;
                while !args.is_char_boundary(cut) {
                    cut -= 1;
                }
                args.truncate(cut);
                args.push('…');
            }
            span.set("gen_ai.tool.call.arguments", args);
        }
        tracer.record_open(&span).await;
        let (result, status) = self.execute_call_inner(live, call, tracer, &mut span, opts).await?;
        if status == "suspended" {
            // Not a failure: `result` holds what it is waiting for, not a
            // message, so none of the error bookkeeping below applies.
            tracer.end(span, status).await;
            return Ok((result, status));
        }
        {
            let key = format!("{}\n{}", call.name, args_hash(&call.arguments));
            let mut errors = live.errors.lock().unwrap();
            if status == "error" && !result.contains("was not repeated") {
                let e = errors.entry(key).or_insert_with(|| (result.clone(), 0));
                if e.0 != result {
                    *e = (result.clone(), 0);
                }
                e.1 += 1;
            } else if status != "error" {
                errors.remove(&key);
            }
        }
        span.set("agen.tool.result_chars", result.len() as u64);
        if status != "ok" {
            // Why it failed, on the span itself (the tool message is in the
            // transcript, but a trace should explain a failure on its own).
            let mut msg = self.redactor.redact(result.trim_start_matches("error: "));
            if msg.len() > 1000 {
                let mut cut = 1000;
                while !msg.is_char_boundary(cut) {
                    cut -= 1;
                }
                msg.truncate(cut);
                msg.push('…');
            }
            span.set("error.message", msg);
        }
        tracer.end(span, status).await;
        Ok((result, status))
    }

    async fn execute_call_inner(
        &self,
        live: &Live,
        call: &ToolCall,
        tracer: &Tracer,
        span: &mut OpenSpan,
        opts: &RunOptions,
    ) -> Result<(String, &'static str), AgentError> {
        let run = &live.rec;
        let Some(tool) = self.tools.get(&call.name) else {
            return Ok((format!("error: unknown tool {:?}", call.name), "error"));
        };
        if call.arguments.get(crate::provider::INVALID_ARGS_KEY).is_some() {
            return Ok((
                "error: tool arguments were not valid JSON (the response may have been cut off by the output token limit); the tool was not called".into(),
                "error",
            ));
        }
        let max = self.cfg.limits.identical_tool_calls();
        if max > 0 {
            let key = format!("{}\n{}", call.name, args_hash(&call.arguments));
            let mut calls = live.calls.lock().unwrap();
            let n = calls.entry(key).or_default();
            *n += 1;
            if *n > max {
                return Ok((
                    format!(
                        "error: this exact call (same tool and arguments) was already made {max} times in this run and was not repeated; change the arguments or finish without it"
                    ),
                    "error",
                ));
            }
        }
        let key = format!("{}\n{}", call.name, args_hash(&call.arguments));
        if let Some((_, n)) = live.errors.lock().unwrap().get(&key) {
            if *n >= IDENTICAL_ERRORS {
                return Ok((
                    format!(
                        "error: this exact call failed {n} times in a row with the same error and was not repeated; change the arguments or try another way"
                    ),
                    "error",
                ));
            }
        }
        let decision = self.policy(opts, &call.name, live.trust());
        span.set("agen.permission.action", format!("{decision:?}").to_lowercase());
        match decision {
            Action::Deny => {
                return Ok((
                    "error: permission denied by policy: this tool is not available to this agent; do not call it again, use another tool or finish without it".into(),
                    "denied",
                ))
            }
            Action::Ask => {
                let mut pspan = tracer.start(&span.ctx, "agen.permission.check");
                tracer.record_open(&pspan).await;
                self.store
                    .checkpoint(
                        &run.id,
                        live.epoch,
                        &run.conversation_id,
                        None,
                        Some((run.step, RunStatus::WaitingApproval, run.usage)),
                    )
                    .await?;
                let req = ApprovalRequest {
                    run_id: run.id.clone(),
                    tool: call.name.clone(),
                    arguments: self.redactor.redact_json(&call.arguments),
                };
                let asked = std::time::Instant::now();
                let d = tokio::select! {
                    d = tokio::time::timeout(self.cfg.approval_timeout, self.approver.decide(req)) => d.unwrap_or(ApprovalDecision::Expired),
                    _ = opts.cancel.cancelled() => ApprovalDecision::Expired,
                };
                live.approval_wait_ms
                    .fetch_add(asked.elapsed().as_millis() as u64, std::sync::atomic::Ordering::SeqCst);
                self.store
                    .checkpoint(
                        &run.id,
                        live.epoch,
                        &run.conversation_id,
                        None,
                        Some((run.step, RunStatus::Running, run.usage)),
                    )
                    .await?;
                pspan.set("agen.permission.decision", format!("{d:?}").to_lowercase());
                tracer.end(pspan, "ok").await;
                if opts.cancel.is_cancelled() {
                    return Ok(("error: cancelled while waiting for approval".into(), "cancelled"));
                }
                if d != ApprovalDecision::Approved {
                    return Ok((
                        format!("error: tool call not approved ({})", format!("{d:?}").to_lowercase()),
                        "denied",
                    ));
                }
            }
            Action::Allow => {}
        }
        if tool.untrusted_output() && live.lower_trust() {
            span.set("agen.trust.lowered", call.name.clone());
            self.log(
                "warn",
                &format!("run {}: trust lowered to untrusted after calling {}", run.id, call.name),
            )
            .await;
        }
        let tctx = ToolContext {
            namespace: self.cfg.namespace.clone(),
            deployment: self.cfg.deployment.clone(),
            task_id: run.task_id.clone(),
            conversation_id: run.conversation_id.clone(),
            conversation_key: opts.conversation_key.clone(),
            labels: run.labels.clone(),
            run_id: run.id.clone(),
            call_id: call.id.clone(),
            step: run.step,
            secrets: self.secrets.clone(),
            cancel: opts.cancel.clone(),
            traceparent: span.ctx.traceparent(),
            root_run_id: if run.root_run_id == run.id {
                String::new()
            } else {
                run.root_run_id.clone()
            },
            depth: opts.depth,
            trust: live.trust(),
        };
        if !tool.side_effect() {
            let r = self.invoke(tool.as_ref(), call, tctx).await;
            if let Some(wait_for) = r.suspend {
                return Ok((wait_for, "suspended"));
            }
            return Ok((r.text, r.status));
        }
        let hash = args_hash(&call.arguments);
        let idem = tool.idempotency_key(&call.arguments).unwrap_or_default();
        // Scoped by turn: providers may reuse call ids across turns.
        let effect_id = effect_id(run.step, &call.id);
        match self
            .store
            .begin_effect(&run.id, live.epoch, &effect_id, &call.name, &hash, &idem)
            .await?
        {
            EffectBegin::Proceed => {}
            EffectBegin::Completed { result } => {
                span.set("agen.effect", "reused");
                return Ok((result, "ok"));
            }
            EffectBegin::Unknown if !idem.is_empty() => {
                span.set("agen.effect", "retried_idempotent");
                self.store.reset_effect(&run.id, live.epoch, &effect_id).await?;
            }
            EffectBegin::Unknown => {
                span.set("agen.effect", "unknown");
                return Ok((
                    "error: effect_unknown: an earlier attempt of this call was interrupted and may have taken effect; it was not repeated".into(),
                    "error",
                ));
            }
        }
        let r = self.invoke(tool.as_ref(), call, tctx).await;
        if let Some(wait_for) = r.suspend {
            // Leave the ledger entry "started": EffectBegin::Unknown resets
            // and retries it (as if interrupted) when this call runs again
            // on resume, by which point it is expected to be done.
            span.set("agen.effect", "suspended");
            return Ok((wait_for, "suspended"));
        }
        if r.uncertain {
            // Cancelled, timed out or connection lost: the call may or may not
            // have taken effect. Leave the ledger entry "started" (unknown)
            // and tell the model so it does not blindly retry.
            span.set("agen.effect", "unknown_after_call");
            let text = format!(
                "error: effect_unknown: {}; the call may or may not have taken effect, do not repeat it blindly",
                r.text.trim_start_matches("error: ")
            );
            return Ok((text, r.status));
        }
        self.store
            .complete_effect(&run.id, live.epoch, &effect_id, &r.text)
            .await?;
        Ok((r.text, r.status))
    }

    async fn invoke(&self, tool: &dyn Tool, call: &ToolCall, ctx: ToolContext) -> Invoked {
        let cancel = ctx.cancel.clone();
        let namespace = ctx.namespace.clone();
        let task_id = ctx.task_id.clone();
        let run_id = ctx.run_id.clone();
        // Delegated runs are bounded by the callee's own limits.
        let limit = match self.cfg.tool_timeout {
            t if t.is_zero() || call.name == crate::delegate::CALL_AGENT => Duration::MAX,
            t => t,
        };
        let r = tokio::select! {
            r = tokio::time::timeout(limit, tool.call(call.arguments.clone(), ctx)) => match r {
                Ok(r) => r,
                Err(_) => return Invoked {
                    text: format!("error: the tool did not finish within {}s (limits.toolTimeout)", limit.as_secs()),
                    status: "error",
                    uncertain: true,
                    suspend: None,
                },
            },
            _ = cancel.cancelled() => return Invoked { text: "error: cancelled".into(), status: "cancelled", uncertain: true, suspend: None },
        };
        match r {
            Ok(text) => {
                let text = self.redactor.redact(&text);
                let text = self.cap_result(&call.name, &namespace, &task_id, &run_id, text).await;
                Invoked {
                    text,
                    status: "ok",
                    uncertain: false,
                    suspend: None,
                }
            }
            Err(e) if e.suspend.is_some() => Invoked {
                text: String::new(),
                status: "suspended",
                uncertain: false,
                suspend: e.suspend,
            },
            Err(e) => Invoked {
                text: self.redactor.redact(&format!("error: {e}")),
                status: "error",
                uncertain: e.outcome_unknown,
                suspend: None,
            },
        }
    }

    /// Caps a tool result to `max_tool_result_chars` (2.23/2.36, part): a
    /// longer one is saved as an artifact (the already-redacted text, so a
    /// secret never reaches durable storage unmasked) and replaced with a
    /// preview plus how to read the rest. Called after redaction, on every
    /// successful call `invoke` makes — MCP tools, delegate calls
    /// (`call_agent`/`start_agent`/`query_agent`, which reach here the same
    /// way any other tool does) and `save_artifact`/`read_artifact`
    /// themselves alike, so no layer of agen drops part of a result
    /// without a way back. 0 (the bundle turned the cap off): unchanged.
    async fn cap_result(&self, tool_name: &str, namespace: &str, task_id: &str, run_id: &str, text: String) -> String {
        let cap = self.cfg.max_tool_result_chars as usize;
        if cap == 0 {
            return text;
        }
        let chars: Vec<char> = text.chars().collect();
        if chars.len() <= cap {
            return text;
        }
        let preview: String = chars[..cap].iter().collect();
        let total = chars.len();
        match self
            .store
            .save_artifact(
                namespace,
                NewArtifact {
                    name: &format!("{tool_name} result"),
                    content: &text,
                    media_type: "text/plain",
                    task_id,
                    run_id,
                },
                crate::artifacts::DEFAULT_NAMESPACE_QUOTA,
            )
            .await
        {
            Ok(a) => format!(
                "{preview}\n(truncated: {cap} of {total} characters shown; the rest was saved as artifact {:?} — read_artifact({:?}, offset={cap}) for more)",
                a.id, a.id
            ),
            Err(e) => format!("{preview}\n(truncated: {cap} of {total} characters shown; it could not be saved as an artifact: {e})"),
        }
    }
}

struct Invoked {
    text: String,
    status: &'static str,
    /// The outcome of a side effect is unknown (cancelled, timed out, lost).
    uncertain: bool,
    /// Set instead of text/status when the tool asked the run to suspend
    /// (3.3): what it is waiting for.
    suspend: Option<String>,
}

enum Outcome {
    Done(String),
    Failed(String),
    Cancelled,
    /// Ended with an explicit outcome (finish, or a report at a limit).
    Ended(RunOutcome),
    /// A tool call asked the whole run to pause here (3.3): what it is
    /// waiting for ("<kind>:<ref>"). The run is not finished: it stays
    /// resumable at this exact point (the call is still pending, with no
    /// tool message), the same as if the process had died here.
    Suspended(String),
}

/// The outcome reason of a run that failed with `err`.
fn failure_reason(err: &str) -> &'static str {
    if err.starts_with("no_answer") {
        "no_answer"
    } else if err.starts_with("max_turns") {
        "max_turns"
    } else if err.starts_with("max_run_duration") {
        "duration"
    } else if err.starts_with("budget_exceeded") {
        "budget"
    } else {
        "error"
    }
}

fn duration_setting(name: &str, value: &str) -> Result<Duration, AgentError> {
    crate::bundle::parse_duration(value).ok_or_else(|| AgentError::Config(format!("invalid {name} {value:?}")))
}

/// The conversation budget for a model with a `window`-token context: half
/// of it (room for the answer and the system prompt), at most the usual
/// 64000 and at least the 4000 a bundle may set.
pub fn default_context_tokens(window: u64) -> u64 {
    (window / 2).clamp(4_000, 64_000)
}

/// Identical calls that failed the same way this often in a row are not
/// made again.
const IDENTICAL_ERRORS: u32 = 3;

/// What was sent for a model turn.
#[derive(Debug, Clone)]
struct Sent {
    model: String,
    tier: Option<String>,
    /// The output limit of a retry after the answer was cut off.
    output_retry: Option<u32>,
}

/// The largest output limit a retry after a cut-off answer asks for.
const MAX_RETRY_OUTPUT_TOKENS: u32 = 32_768;

/// The answer stopped at the output limit with nothing usable: no text and
/// no calls, or calls whose arguments were cut off.
fn output_cut(r: &provider::ModelResponse) -> bool {
    r.finish_reason == "length"
        && (r.is_empty()
            || r.tool_calls
                .iter()
                .any(|c| c.arguments.get(crate::provider::INVALID_ARGS_KEY).is_some()))
}

/// Whether a task's overrides allow this tool at all (its `tools`
/// allowlist, empty = every tool; `finish` and `load_skill` always pass).
fn override_allows_tool(ov: &crate::overrides::TaskOverrides, tool: &str) -> bool {
    if ov.tools.is_empty() || tool == crate::outcome::FINISH || tool == "load_skill" {
        return true;
    }
    ov.tools.iter().any(|pat| permissions::glob_match(pat, tool))
}

/// A run budget narrowed by a task's `overrides.budget` (never widened: an
/// override field of 0 or unset leaves the bundle's value).
fn narrow_budget(base: &Budget, over: Option<&crate::overrides::RunBudgetOverride>) -> Budget {
    let Some(over) = over else { return base.clone() };
    Budget {
        max_tokens_per_run: narrow_opt(base.max_tokens_per_run, over.max_tokens),
        max_usd_per_run: narrow_opt_f(base.max_usd_per_run, over.max_usd),
        max_usd_per_day: base.max_usd_per_day,
    }
}

/// `max_turns` narrowed by `overrides.budget.maxTurns` (0/unset: unchanged).
fn narrow_max_turns(base: u32, over: Option<&crate::overrides::RunBudgetOverride>) -> u32 {
    match over.and_then(|o| o.max_turns).filter(|n| *n > 0) {
        Some(n) => base.min(n),
        None => base,
    }
}

/// `max_run_duration` narrowed by `overrides.budget.maxDuration` (an
/// unparsable or zero override duration leaves the bundle's value; the
/// bundle's own 0 means "no limit", so any override duration narrows it).
fn narrow_max_run_duration(base: Duration, over: Option<&crate::overrides::RunBudgetOverride>) -> Duration {
    let Some(d) = over
        .and_then(|o| o.max_duration.as_deref())
        .and_then(crate::bundle::parse_duration)
    else {
        return base;
    };
    if d.is_zero() {
        return base;
    }
    if base.is_zero() {
        d
    } else {
        base.min(d)
    }
}

fn narrow_opt(base: Option<u64>, over: Option<u64>) -> Option<u64> {
    match (base, over.filter(|n| *n > 0)) {
        (Some(b), Some(o)) => Some(b.min(o)),
        (None, Some(o)) => Some(o),
        (b, None) => b,
    }
}

fn narrow_opt_f(base: Option<f64>, over: Option<f64>) -> Option<f64> {
    match (base, over.filter(|n| *n > 0.0)) {
        (Some(b), Some(o)) => Some(b.min(o)),
        (None, Some(o)) => Some(o),
        (b, None) => b,
    }
}

/// Request extras from a bundle's harness: OpenRouter settings as given and
/// the reasoning effort in the provider's wire form.
fn request_extra(h: &crate::bundle::Harness) -> serde_json::Map<String, Value> {
    request_extras(&h.provider, h.openrouter.as_ref(), h.reasoning_effort.as_deref())
}

/// Request extras for a provider: OpenRouter settings as given and the
/// reasoning effort in the provider's wire form (also for agents defined in
/// code).
pub fn request_extras(
    provider: &str,
    openrouter: Option<&crate::bundle::OpenRouterOptions>,
    reasoning_effort: Option<&str>,
) -> serde_json::Map<String, Value> {
    let mut extra = serde_json::Map::new();
    if let Some(o) = openrouter {
        if let Some(p) = &o.provider {
            extra.insert("provider".into(), p.clone());
        }
        if let Some(p) = &o.plugins {
            extra.insert("plugins".into(), Value::Array(p.clone()));
        }
    }
    if let Some(e) = reasoning_effort {
        match provider {
            "openrouter" => {
                extra.insert("reasoning".into(), serde_json::json!({"effort": e}));
            }
            "openai" => {
                extra.insert("reasoning_effort".into(), Value::String(e.to_string()));
            }
            _ => {}
        }
    }
    extra
}

/// An empty answer after tool results is how some models end work done
/// through tools; it is only a failure when output was cut off.
fn empty_ends_work(after_tools: bool, r: &provider::ModelResponse) -> bool {
    after_tools && r.finish_reason != "length"
}

fn backoff(attempt: u32) -> Duration {
    Duration::from_millis(500u64.saturating_mul(1 << attempt.min(6))).min(Duration::from_secs(30))
}

/// Tool results always kept whole at the end of a run (the model is working
/// with them).
const KEEP_RECENT_RESULTS: usize = 4;
/// Results are shortened in blocks, so the request prefix (and the provider's
/// prompt cache) changes rarely.
const COMPACT_BLOCK: usize = 8;
/// A shortened result keeps this much of its start.
const COMPACT_HEAD_CHARS: usize = 300;

/// Shortens the oldest tool results of a run whose messages exceed `limit`
/// tokens, until they fit in about two thirds of it: each keeps its start and
/// says how much was left out. The latest results stay whole. Nothing is
/// lost: the full results stay in the Store and the transcript.
fn compact_run(mut msgs: Vec<Message>, limit: u64) -> Vec<Message> {
    let total: u64 = msgs.iter().map(message_tokens).sum();
    if limit == 0 || total <= limit {
        return msgs;
    }
    let target = limit * 2 / 3;
    // Which tool, in which model turn, each result answers.
    let mut calls: BTreeMap<String, (String, usize)> = BTreeMap::new();
    let mut turn = 0;
    for m in &msgs {
        if m.role == Role::Assistant {
            turn += 1;
            for c in &m.tool_calls {
                calls.insert(c.id.clone(), (c.name.clone(), turn));
            }
        }
    }
    let call_of = |m: &Message| m.tool_call_id.as_ref().and_then(|id| calls.get(id)).cloned();
    let results: Vec<usize> = msgs
        .iter()
        .enumerate()
        .filter(|(_, m)| m.role == Role::Tool && m.content.len() > COMPACT_HEAD_CHARS * 2)
        // Skill instructions stay whole: the run works by them.
        .filter(|(_, m)| call_of(m).is_none_or(|(name, _)| name != "load_skill"))
        .map(|(i, _)| i)
        .collect();
    let candidates = results.len().saturating_sub(KEEP_RECENT_RESULTS);
    let mut over = total.saturating_sub(target);
    let mut n = 0;
    while n < candidates && over > 0 {
        let m = &msgs[results[n]];
        over = over.saturating_sub(message_tokens(m).saturating_sub(COMPACT_HEAD_CHARS as u64 / 4 + 30));
        n += 1;
    }
    // Round up to a whole block (within the candidates).
    n = (n.div_ceil(COMPACT_BLOCK) * COMPACT_BLOCK).min(candidates);
    for &i in &results[..n] {
        let what = match call_of(&msgs[i]) {
            Some((name, turn)) => format!("this result of {name} (turn {turn})"),
            None => "this older tool result".to_string(),
        };
        let m = &mut msgs[i];
        let mut cut = COMPACT_HEAD_CHARS;
        while !m.content.is_char_boundary(cut) {
            cut -= 1;
        }
        let left = m.content.len() - cut;
        m.content = format!(
            "{}\n[… {left} more characters of {what} were left out to keep the conversation within its context budget. You have already read it: do not call it again unless you need a detail that is missing here.]",
            &m.content[..cut]
        );
    }
    msgs
}

fn message_tokens(m: &Message) -> u64 {
    let args: usize = m
        .tool_calls
        .iter()
        .map(|c| c.arguments.to_string().len() + c.name.len())
        .sum();
    ((m.content.len() + args) as u64).div_ceil(4) + 4
}

/// Give every assistant tool call a result before the next non-tool message,
/// so providers never see an unanswered tool call.
fn answer_orphans(msgs: Vec<Message>) -> Vec<Message> {
    let mut out = Vec::with_capacity(msgs.len());
    let mut open: Vec<String> = vec![];
    for m in msgs {
        match m.role {
            Role::Tool => {
                if let Some(id) = &m.tool_call_id {
                    open.retain(|x| x != id);
                }
                out.push(m);
                continue;
            }
            _ => {
                for id in open.drain(..) {
                    out.push(Message::tool(id, "error: no result was recorded for this tool call"));
                }
            }
        }
        if m.role == Role::Assistant {
            open = m.tool_calls.iter().map(|c| c.id.clone()).collect();
        }
        out.push(m);
    }
    out
}

fn result_of(r: &RunRecord) -> RunResult {
    RunResult {
        run_id: r.id.clone(),
        session_id: r.session_id.clone(),
        conversation_id: r.conversation_id.clone(),
        status: r.status,
        output: r.output.clone(),
        error: r.error.clone(),
        usage: r.usage,
        trace_id: r.trace_id.clone(),
        outcome: r.outcome.clone(),
        wait_for: String::new(),
    }
}

/// Effect-ledger key for a tool call made in model turn `step`.
pub fn effect_id(step: i64, call_id: &str) -> String {
    format!("{step}/{call_id}")
}

pub fn args_hash(args: &Value) -> String {
    hex::encode(Sha256::digest(args.to_string().as_bytes()))
}

/// A provider key not declared in `secrets.json` falls back to the
/// environment variable of the same name, and is registered as a secret so it
/// is redacted like any other.
fn with_provider_key(bundle: &Bundle, secrets: Secrets) -> Secrets {
    if !matches!(bundle.harness.provider.as_str(), "openrouter" | "openai") {
        return secrets;
    }
    let name = crate::provider::api_key_name(&bundle.harness);
    if secrets.get(&name).is_some() {
        return secrets;
    }
    match std::env::var(&name) {
        Ok(v) if !v.is_empty() => {
            let mut m: std::collections::BTreeMap<String, String> = secrets
                .names()
                .map(|n| (n.to_string(), secrets.get(n).unwrap_or_default().to_string()))
                .collect();
            m.insert(name, v);
            Secrets::from_map(m)
        }
        _ => secrets,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn orphaned_tool_calls_get_results() {
        let call = |id: &str| ToolCall {
            id: id.into(),
            name: "t".into(),
            arguments: serde_json::json!({}),
        };
        let msgs = vec![
            Message::user("a"),
            Message::assistant("", vec![call("1"), call("2")]),
            Message::tool("1", "ok"),
            Message::user("b"),
        ];
        let out = answer_orphans(msgs);
        let roles: Vec<Role> = out.iter().map(|m| m.role).collect();
        assert_eq!(roles, [Role::User, Role::Assistant, Role::Tool, Role::Tool, Role::User]);
        assert_eq!(out[3].tool_call_id.as_deref(), Some("2"));
        // Trailing orphans (end of list) are left for the loop to execute.
        let tail = answer_orphans(vec![Message::assistant("", vec![call("9")])]);
        assert_eq!(tail.len(), 1);
    }

    fn run_with_results(n: usize, size: usize) -> Vec<Message> {
        let mut v = vec![Message::user("go")];
        for i in 0..n {
            let id = format!("c{i}");
            v.push(Message::assistant(
                "",
                vec![ToolCall {
                    id: id.clone(),
                    name: "read".into(),
                    arguments: serde_json::json!({"i": i}),
                }],
            ));
            v.push(Message::tool(&id, format!("{i}:{}", "x".repeat(size))));
        }
        v
    }

    #[test]
    fn compaction_shortens_oldest_results_in_blocks() {
        // Under the limit: untouched.
        let small = run_with_results(5, 1000);
        assert_eq!(compact_run(small.clone(), 64_000), small);
        // 40 results of ~2.5k tokens each = ~100k tokens over a 40k limit.
        let big = run_with_results(40, 10_000);
        let out = compact_run(big.clone(), 40_000);
        let total: u64 = out.iter().map(message_tokens).sum();
        assert!(total <= 40_000 * 2 / 3 + 3_000, "still {total} tokens");
        let shortened: Vec<usize> = out
            .iter()
            .enumerate()
            .filter(|(_, m)| m.content.contains("were left out"))
            .map(|(i, _)| i)
            .collect();
        assert_eq!(shortened.len() % COMPACT_BLOCK, 0, "whole blocks");
        // Oldest first, the latest results stay whole, calls are untouched.
        assert_eq!(shortened[0], 2);
        let tools: Vec<&Message> = out.iter().filter(|m| m.role == Role::Tool).collect();
        assert!(tools[tools.len() - KEEP_RECENT_RESULTS..]
            .iter()
            .all(|m| !m.content.contains("left out")));
        assert_eq!(out.len(), big.len());
        assert!(out[shortened[0]].content.starts_with("0:xxx"));
        // Deterministic: the same input gives the same request.
        assert_eq!(compact_run(big, 40_000), out);
    }

    #[test]
    fn backoff_grows_and_caps() {
        assert_eq!(backoff(0), Duration::from_millis(500));
        assert_eq!(backoff(1), Duration::from_millis(1000));
        assert_eq!(backoff(10), Duration::from_secs(30));
    }
}
