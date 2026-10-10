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

use crate::bundle::{default_limits, Action, Budget, Bundle, Limits, Permissions, Skill};
use crate::permissions;
use crate::provider::{self, Message, ModelProvider, ModelRequest, ProviderError, Role, ToolCall, Usage};
use crate::secrets::{PlatformSecrets, Redactor, Secrets, StreamRedactor};
use crate::store::{new_id, now_ms, EffectBegin, RunRecord, RunStatus, Store, StoreError};
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
    pub model: String,
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
}

impl AgentConfig {
    pub fn new(name: &str, system_prompt: &str, model: &str) -> Self {
        Self {
            name: name.into(),
            system_prompt: system_prompt.into(),
            skills: vec![],
            model: model.into(),
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
    pub fn build(mut self) -> Result<Agent, AgentError> {
        let provider = self
            .provider
            .ok_or_else(|| AgentError::Config("no model provider".into()))?;
        crate::secrets::validate(&self.secrets).map_err(|e| AgentError::Config(e.to_string()))?;
        let mut tools = ToolRegistry::new();
        if !self.cfg.skills.is_empty() && !self.tools.iter().any(|t| t.spec().name == "load_skill") {
            self.tools.push(Arc::new(LoadSkill::new(&self.cfg.skills)));
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
}

impl Live {
    fn new(rec: RunRecord, epoch: i64) -> Self {
        Self {
            rec,
            epoch,
            started: std::time::Instant::now(),
            approval_wait_ms: Default::default(),
            calls: Default::default(),
        }
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
        cfg.temperature = bundle.harness.temperature;
        cfg.max_output_tokens = Some(
            bundle
                .harness
                .max_output_tokens
                .unwrap_or(default_limits::MAX_OUTPUT_TOKENS),
        );
        cfg.parallel_tool_calls = bundle.harness.parallel_tool_calls.unwrap_or(true);
        cfg.trace_tool_arguments = bundle.harness.trace_tool_arguments.unwrap_or(false);
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
        let namespace = opts.namespace.unwrap_or_else(|| "default".into());
        cfg.namespace = namespace.clone();
        cfg.deployment = opts.deployment.unwrap_or_else(|| bundle.name().into());
        cfg.definition_digest = bundle.digest.clone();
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
                bundle.config.limits.clone(),
                store.clone(),
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
                &opts.mcp,
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
        };
        let epoch = self.store.create_run(&rec, &self.owner).await?;
        let first = self.redact_message(&Message::user(input));
        self.store
            .checkpoint(&run_id, epoch, &conversation_id, Some(&first), None)
            .await?;
        self.drive(Live::new(rec, epoch), &parent, "agen.run", opts).await
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
        self.drive(Live::new(rec, epoch), &parent, "agen.run.resume", &opts)
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
        let (status, output, error) = match outcome {
            Ok(Outcome::Done(text)) => (RunStatus::Succeeded, text, String::new()),
            Ok(Outcome::Failed(err)) => (RunStatus::Failed, String::new(), err),
            Ok(Outcome::Cancelled) => (RunStatus::Cancelled, String::new(), "cancelled".into()),
            // Store failures (including losing ownership) leave the run as
            // it is: resumable, or owned by someone else.
            Err(AgentError::Store(e)) => {
                tracer.end(span, "error").await;
                return Err(AgentError::Store(e));
            }
            Err(e) => (RunStatus::Failed, String::new(), self.redactor.redact(&e.to_string())),
        };
        // Never leave tool calls without results: the conversation must stay
        // valid for the next run.
        self.close_dangling_calls(&live, status).await?;
        span.set("gen_ai.usage.input_tokens", live.rec.usage.input_tokens);
        span.set("gen_ai.usage.output_tokens", live.rec.usage.output_tokens);
        span.set("agen.run.status", status.as_str());
        let output = self.redactor.redact(&output);
        self.store
            .finish_run(&live.rec.id, live.epoch, status, &output, &error, live.rec.usage)
            .await?;
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
                return Ok(Outcome::Done(text));
            }
            // Calls that may wait for an approval run one at a time, so the
            // run's waiting state and the approval order stay simple.
            let concurrent = self.cfg.parallel_tool_calls
                && pending
                    .iter()
                    .all(|c| permissions::evaluate(&self.cfg.permissions, &c.name) != Action::Ask);
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
            // Results are recorded in call order as they become available.
            while let Some(r) = results.next().await {
                let Some((id, result)) = r? else {
                    return Ok(Outcome::Cancelled);
                };
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
            if opts.cancel.is_cancelled() {
                return Ok(Outcome::Cancelled);
            }
            if live.rec.step >= self.cfg.max_turns as i64 {
                return Ok(Outcome::Failed(format!("max_turns ({}) exceeded", self.cfg.max_turns)));
            }
            let max = self.cfg.max_run_duration;
            if !max.is_zero() && live.worked() > max {
                return Ok(Outcome::Failed(format!(
                    "max_run_duration ({}s) exceeded",
                    max.as_secs()
                )));
            }
            if let Some(e) = self.budget_exceeded(&live.rec.usage) {
                return Ok(Outcome::Failed(e));
            }
            let req = ModelRequest {
                model: self.cfg.model.clone(),
                system: self.redactor.redact(&self.system_prompt()),
                messages: self.request_messages(&live.rec).await?,
                tools: self.tools.specs(),
                temperature: self.cfg.temperature,
                max_output_tokens: self.output_cap(&live.rec.usage),
            };
            let mut span = tracer.start(ctx, "gen_ai.chat");
            span.set("gen_ai.operation.name", "chat");
            span.set("gen_ai.provider.name", self.provider.name().to_string());
            span.set("gen_ai.request.model", self.cfg.model.clone());
            span.set("agen.request.messages", req.messages.len() as u64);
            tracer.record_open(&span).await;
            let resp = tokio::select! {
                r = self.complete_with_retry(&req, opts) => r,
                _ = opts.cancel.cancelled() => {
                    tracer.end(span, "cancelled").await;
                    return Ok(Outcome::Cancelled);
                }
            };
            let resp = match resp {
                Ok(r) => r,
                Err(e) => {
                    span.set("error.type", e.to_string());
                    tracer.end(span, "error").await;
                    return Err(e.into());
                }
            };
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

    /// Conversation history for the model: earlier runs' messages (trimmed
    /// oldest-first to fit `context_tokens`, starting at a user message) plus
    /// all of the current run's messages, with orphaned tool calls answered.
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
        history.extend(current);
        Ok(answer_orphans(history))
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
    fn output_cap(&self, u: &Usage) -> Option<u32> {
        const UNSET_CEILING: u32 = 8192;
        let remaining = self.cfg.budget.max_tokens_per_run.map(|max| {
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

    fn budget_exceeded(&self, u: &Usage) -> Option<String> {
        if let Some(max) = self.cfg.budget.max_tokens_per_run {
            if u.total_tokens() >= max {
                return Some(format!(
                    "budget_exceeded: {} tokens used, limit {max}",
                    u.total_tokens()
                ));
            }
        }
        // Only enforceable when the provider reports cost (OpenRouter does).
        if let Some(max) = self.cfg.budget.max_usd_per_run {
            if u.cost_usd >= max {
                return Some(format!("budget_exceeded: ${:.4} used, limit ${max}", u.cost_usd));
            }
        }
        None
    }

    fn system_prompt(&self) -> String {
        let mut s = self.cfg.system_prompt.clone();
        if !self.cfg.skills.is_empty() {
            s.push_str("\n\n## Skills\nCall load_skill with a name to read a skill before using it.\n");
            for sk in &self.cfg.skills {
                s.push_str(&format!("- {}: {}\n", sk.name, sk.description));
            }
        }
        s
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
    ) -> Result<String, AgentError> {
        let mut span = tracer.start(ctx, "agen.tool");
        span.set("gen_ai.tool.name", call.name.clone());
        span.set("gen_ai.tool.call.id", call.id.clone());
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
        span.set("agen.tool.result_chars", result.len() as u64);
        tracer.end(span, status).await;
        Ok(result)
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
            let key = format!(
                "{}
{}",
                call.name,
                args_hash(&call.arguments)
            );
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
        let decision = permissions::evaluate(&self.cfg.permissions, &call.name);
        span.set("agen.permission.action", format!("{decision:?}").to_lowercase());
        match decision {
            Action::Deny => return Ok(("error: permission denied by policy".into(), "denied")),
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
        };
        if !tool.side_effect() {
            let r = self.invoke(tool.as_ref(), call, tctx).await;
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
                },
            },
            _ = cancel.cancelled() => return Invoked { text: "error: cancelled".into(), status: "cancelled", uncertain: true },
        };
        match r {
            Ok(text) => Invoked {
                text: self.redactor.redact(&text),
                status: "ok",
                uncertain: false,
            },
            Err(e) => Invoked {
                text: self.redactor.redact(&format!("error: {e}")),
                status: "error",
                uncertain: e.outcome_unknown,
            },
        }
    }
}

struct Invoked {
    text: String,
    status: &'static str,
    /// The outcome of a side effect is unknown (cancelled, timed out, lost).
    uncertain: bool,
}

enum Outcome {
    Done(String),
    Failed(String),
    Cancelled,
}

fn duration_setting(name: &str, value: &str) -> Result<Duration, AgentError> {
    crate::bundle::parse_duration(value).ok_or_else(|| AgentError::Config(format!("invalid {name} {value:?}")))
}

fn backoff(attempt: u32) -> Duration {
    Duration::from_millis(500u64.saturating_mul(1 << attempt.min(6))).min(Duration::from_secs(30))
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

    #[test]
    fn backoff_grows_and_caps() {
        assert_eq!(backoff(0), Duration::from_millis(500));
        assert_eq!(backoff(1), Duration::from_millis(1000));
        assert_eq!(backoff(10), Duration::from_secs(30));
    }
}
