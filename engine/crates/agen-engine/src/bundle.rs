//! Agent bundle loading and validation.
//!
//! A bundle is an Agent Plugins 1.0 directory plus an `x-agen/` extension
//! directory (see docs/architecture.md §9). Loading validates every file
//! against the JSON Schemas in `spec/bundle/` and reports all problems at once.

use std::collections::BTreeMap;
use std::fmt;
use std::path::{Path, PathBuf};

use serde::Deserialize;
use serde_json::Value;
use sha2::{Digest, Sha256};

const PLUGIN_SCHEMA: &str = include_str!("../../../../spec/bundle/plugin.schema.json");
const MCP_SCHEMA: &str = include_str!("../../../../spec/bundle/mcp.schema.json");
const AGENT_SCHEMA: &str = include_str!("../../../../spec/bundle/agent.schema.json");
const HARNESS_SCHEMA: &str = include_str!("../../../../spec/bundle/harness.schema.json");
const CONFIG_SCHEMA: &str = include_str!("../../../../spec/bundle/config.schema.json");
const SECRETS_SCHEMA: &str = include_str!("../../../../spec/bundle/secrets.schema.json");

/// One problem found in a bundle.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Issue {
    /// Bundle-relative file, e.g. `x-agen/harness.json`.
    pub file: String,
    /// JSON pointer inside the file (empty for whole-file problems).
    pub path: String,
    pub message: String,
}

impl fmt::Display for Issue {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.path.is_empty() {
            write!(f, "{}: {}", self.file, self.message)
        } else {
            write!(f, "{} at {}: {}", self.file, self.path, self.message)
        }
    }
}

#[derive(Debug, thiserror::Error)]
#[error("invalid bundle {root}:\n{}", issues.iter().map(|i| format!("  - {i}")).collect::<Vec<_>>().join("\n"))]
pub struct BundleError {
    pub root: String,
    pub issues: Vec<Issue>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct AgentFront {
    pub name: String,
    pub description: String,
    #[serde(default)]
    pub skills: Vec<String>,
    #[serde(default, rename = "preloadSkills")]
    pub preload_skills: Vec<String>,
    #[serde(rename = "maxTurns")]
    pub max_turns: Option<u32>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Harness {
    pub provider: String,
    pub model: String,
    #[serde(default)]
    pub fallback_models: Vec<String>,
    pub reasoning_effort: Option<String>,
    pub openrouter: Option<OpenRouterOptions>,
    pub router: Option<crate::provider::router::RouterSettings>,
    pub temperature: Option<f64>,
    pub max_output_tokens: Option<u32>,
    pub context_tokens: Option<u64>,
    pub parallel_tool_calls: Option<bool>,
    pub trace_tool_arguments: Option<bool>,
    /// A tool result longer than this many characters is saved as an
    /// artifact instead of going into the conversation whole; the
    /// conversation gets a preview plus the artifact id and how to read
    /// the rest. 0: no cap. Default `default_limits::MAX_TOOL_RESULT_CHARS`.
    pub max_tool_result_chars: Option<u32>,
    pub base_url: Option<String>,
    pub api_key_secret: Option<String>,
    pub script: Option<String>,
    pub cassette: Option<String>,
    /// The structured result a run returns through `finish`.
    pub result: Option<ResultSpec>,
    /// Tried in order, each as its own full provider/account, only after
    /// an account error (bad key, no credit) on the primary — never for a
    /// transient one, and never just because a model on the primary kept
    /// failing (that's what fallback_models is for). Off by default (2.30).
    #[serde(default)]
    pub fallback_providers: Vec<Harness>,
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct ResultSpec {
    pub schema: serde_json::Value,
}

/// OpenRouter request settings sent as given (`harness.json` `openrouter`).
#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct OpenRouterOptions {
    pub provider: Option<serde_json::Value>,
    pub plugins: Option<Vec<serde_json::Value>>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum Action {
    Allow,
    #[default]
    Ask,
    Deny,
}

/// A task's trust, fixed in this release as operator > trusted > untrusted.
/// Set by its source (an API token's `trust`, default trusted; a webhook
/// trigger's, default untrusted; cron, always trusted) and never raised by
/// a child or by taint: `Ord` reflects this ranking directly.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum Trust {
    Untrusted,
    #[default]
    Trusted,
    Operator,
}

impl Trust {
    /// Parses a stored or wire trust string; "" and anything unrecognized
    /// mean the default, trusted.
    pub fn parse(s: &str) -> Trust {
        match s {
            "untrusted" => Trust::Untrusted,
            "operator" => Trust::Operator,
            _ => Trust::Trusted,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Trust::Untrusted => "untrusted",
            Trust::Trusted => "trusted",
            Trust::Operator => "operator",
        }
    }
}

/// A permission rule's extra condition (1.3; `rate`, a count in a window,
/// is not yet implemented — it needs its own persistent counter, a bigger
/// piece than the others here).
#[derive(Debug, Clone, Deserialize, Default)]
pub struct When {
    /// The run's current trust is exactly this.
    pub trust: Option<Trust>,
    /// The run's current trust is below this.
    #[serde(rename = "trustBelow")]
    pub trust_below: Option<Trust>,
    /// Every named argument must match (missing from the call: never
    /// matches). Checked only against a specific call's own arguments;
    /// never narrows which tools are merely listed to the model.
    #[serde(default)]
    pub args: std::collections::BTreeMap<String, ArgMatch>,
    /// Every named label of the task must equal this value.
    #[serde(default)]
    pub labels: std::collections::BTreeMap<String, String>,
    /// A time-of-day/day-of-week window, in UTC (no time zone database
    /// dependency here, unlike the Hub's own random triggers).
    pub time: Option<TimeWindow>,
}

impl When {
    fn matches(&self, trust: Trust, args: &Value, labels: &std::collections::BTreeMap<String, String>) -> bool {
        self.trust.is_none_or(|t| t == trust)
            && self.trust_below.is_none_or(|t| trust < t)
            && self.args.iter().all(|(k, m)| m.matches(args.get(k)))
            && self.labels.iter().all(|(k, v)| labels.get(k) == Some(v))
            && self.time.as_ref().is_none_or(TimeWindow::now_within)
    }
}

/// One `when.args` condition (1.3): an exact JSON value, or a tagged
/// comparison. Tried in this order (an untagged enum uses the first
/// variant that deserializes), so a bare value — a string, a number, a
/// bool — always falls through to `Exact` once the tagged shapes above
/// it don't match.
#[derive(Debug, Clone, Deserialize)]
#[serde(untagged)]
pub enum ArgMatch {
    Glob { glob: String },
    Lt { lt: f64 },
    Lte { lte: f64 },
    Gt { gt: f64 },
    Gte { gte: f64 },
    Exact(Value),
}

impl ArgMatch {
    fn matches(&self, val: Option<&Value>) -> bool {
        match self {
            ArgMatch::Glob { glob } => val
                .and_then(Value::as_str)
                .is_some_and(|s| crate::permissions::glob_match(glob, s)),
            ArgMatch::Lt { lt } => val.and_then(Value::as_f64).is_some_and(|n| n < *lt),
            ArgMatch::Lte { lte } => val.and_then(Value::as_f64).is_some_and(|n| n <= *lte),
            ArgMatch::Gt { gt } => val.and_then(Value::as_f64).is_some_and(|n| n > *gt),
            ArgMatch::Gte { gte } => val.and_then(Value::as_f64).is_some_and(|n| n >= *gte),
            ArgMatch::Exact(want) => val == Some(want),
        }
    }
}

/// `when.time` (1.3): a "HH:MM-HH:MM" UTC time-of-day window, optionally
/// also limited to `days` ("mon".."sun"; empty: every day).
#[derive(Debug, Clone, Deserialize)]
pub struct TimeWindow {
    pub window: String,
    #[serde(default)]
    pub days: Vec<String>,
}

impl TimeWindow {
    fn now_within(&self) -> bool {
        self.contains(std::time::SystemTime::now())
    }

    fn contains(&self, now: std::time::SystemTime) -> bool {
        let Some((from, to)) = parse_time_window(&self.window) else {
            return false;
        };
        let secs = now
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0);
        if !self.days.is_empty() {
            const NAMES: [&str; 7] = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"];
            // 1970-01-01 (epoch day 0) was a Thursday (index 4).
            let weekday = NAMES[((secs / 86_400 + 4) % 7) as usize];
            if !self.days.iter().any(|d| d == weekday) {
                return false;
            }
        }
        let sod = (secs % 86_400) as i64;
        from <= sod && sod < to
    }
}

/// Parses "HH:MM-HH:MM" into seconds-since-midnight bounds.
fn parse_time_window(w: &str) -> Option<(i64, i64)> {
    let (s, e) = w.split_once('-')?;
    let parse_hm = |t: &str| -> Option<i64> {
        let (h, m) = t.split_once(':')?;
        Some(h.parse::<i64>().ok()? * 3600 + m.parse::<i64>().ok()? * 60)
    };
    let (from, to) = (parse_hm(s)?, parse_hm(e)?);
    (to > from).then_some((from, to))
}

#[derive(Debug, Clone, Deserialize)]
pub struct PermissionRule {
    pub tool: String,
    pub action: Action,
    #[serde(default)]
    pub when: Option<When>,
}

impl PermissionRule {
    pub fn matches(&self, trust: Trust, args: &Value, labels: &std::collections::BTreeMap<String, String>) -> bool {
        self.when.as_ref().is_none_or(|w| w.matches(trust, args, labels))
    }
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct Permissions {
    #[serde(default)]
    pub default: Action,
    #[serde(default)]
    pub rules: Vec<PermissionRule>,
    /// How long an "ask" waits for a decision ("30s", "10m", "1h").
    #[serde(default, rename = "approvalTimeout")]
    pub approval_timeout: Option<String>,
}

/// Parses "<n>ms|s|m|h".
pub fn parse_duration(s: &str) -> Option<std::time::Duration> {
    let s = s.trim();
    let (num, unit) = s.split_at(s.find(|c: char| !c.is_ascii_digit())?);
    let n: u64 = num.parse().ok()?;
    Some(match unit {
        "ms" => std::time::Duration::from_millis(n),
        "s" => std::time::Duration::from_secs(n),
        "m" => std::time::Duration::from_secs(n * 60),
        "h" => std::time::Duration::from_secs(n * 3600),
        _ => return None,
    })
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct Budget {
    pub max_tokens_per_run: Option<u64>,
    pub max_usd_per_run: Option<f64>,
    pub max_usd_per_day: Option<f64>,
    /// How much a cached input token counts against max_tokens_per_run
    /// (2.21), relative to a fresh one. Default 0.1 (a cache hit costs a
    /// tenth as much as a fresh prompt token).
    pub cached_input_weight: Option<f64>,
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct Limits {
    pub max_delegation_depth: Option<u32>,
    pub max_fan_out: Option<u32>,
    pub max_total_delegations: Option<u32>,
    /// Wall-clock time a run may take, not counting approval waits ("1h").
    pub max_run_duration: Option<String>,
    /// How often a run may make the same tool call (name and arguments).
    pub max_identical_tool_calls: Option<u32>,
    /// How long one model request may take ("10m").
    pub model_request_timeout: Option<String>,
    /// How long one tool call may take ("10m"); delegated calls are bounded
    /// by the callee's own limits instead.
    pub tool_timeout: Option<String>,
    /// Output limit of the report call a run gets when it stops at a limit
    /// (0: no report call; the run fails as before).
    pub report_tokens: Option<u32>,
    /// The same step with the same result this many turns in a row counts as
    /// stuck: the run is nudged once, then stops and reports (0: off).
    pub stuck_repeats: Option<u32>,
    /// How long `call_agent`'s unified delegation path (a platform delegate,
    /// no fixed url, not `via: "a2a"`) waits for the child task's answer
    /// before suspending the run instead of blocking on it ("20s").
    pub inline_wait: Option<String>,
}

/// Limit values used when a bundle does not set them. A limit set to 0 is
/// off.
pub mod default_limits {
    use std::time::Duration;
    pub const MAX_DELEGATION_DEPTH: u32 = 3;
    pub const MAX_FAN_OUT: u32 = 20;
    pub const MAX_TOTAL_DELEGATIONS: u32 = 50;
    pub const MAX_IDENTICAL_TOOL_CALLS: u32 = 5;
    pub const MAX_RUN_DURATION: Duration = Duration::from_secs(3600);
    pub const MODEL_REQUEST_TIMEOUT: Duration = Duration::from_secs(600);
    pub const TOOL_TIMEOUT: Duration = Duration::from_secs(600);
    pub const MAX_OUTPUT_TOKENS: u32 = 8192;
    pub const MAX_TOOL_RESULT_CHARS: u32 = 32_000;
    pub const CACHED_INPUT_WEIGHT: f64 = 0.1;
    pub const MAX_APPROVAL_TIMEOUT: Duration = Duration::from_secs(7 * 24 * 3600);
    pub const REPORT_TOKENS: u32 = 1024;
    pub const STUCK_REPEATS: u32 = 4;
    pub const INLINE_WAIT: Duration = Duration::from_secs(20);
}

impl Limits {
    pub fn delegation_depth(&self) -> u32 {
        self.max_delegation_depth
            .unwrap_or(default_limits::MAX_DELEGATION_DEPTH)
    }
    pub fn fan_out(&self) -> u32 {
        self.max_fan_out.unwrap_or(default_limits::MAX_FAN_OUT)
    }
    pub fn total_delegations(&self) -> u32 {
        self.max_total_delegations
            .unwrap_or(default_limits::MAX_TOTAL_DELEGATIONS)
    }
    pub fn identical_tool_calls(&self) -> u32 {
        self.max_identical_tool_calls
            .unwrap_or(default_limits::MAX_IDENTICAL_TOOL_CALLS)
    }
    pub fn report_tokens(&self) -> u32 {
        self.report_tokens.unwrap_or(default_limits::REPORT_TOKENS)
    }
    pub fn stuck_repeats(&self) -> u32 {
        self.stuck_repeats.unwrap_or(default_limits::STUCK_REPEATS)
    }
    pub fn inline_wait(&self) -> std::time::Duration {
        self.inline_wait
            .as_deref()
            .and_then(parse_duration)
            .unwrap_or(default_limits::INLINE_WAIT)
    }
}

/// Another deployment this agent may call (`call_agent`, `start_agent`).
#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct Delegate {
    /// Deployment name.
    pub name: String,
    /// Namespace (default: the caller's).
    pub namespace: Option<String>,
    /// Fixed A2A endpoint (standalone use); otherwise resolved by the platform.
    pub url: Option<String>,
    /// Shown to the model.
    pub description: Option<String>,
    /// Accept narrowing overrides from the caller's brief (default false:
    /// only deployments meant as generic workers should set this).
    #[serde(default)]
    pub overrides: bool,
    /// `"a2a"` opts a platform delegate (no fixed `url`) out of
    /// `call_agent`'s unified delegation (child task + suspend/resume) back
    /// into the plain synchronous A2A call it always had. A delegate with a
    /// fixed `url` always uses A2A regardless of this.
    pub via: Option<String>,
}

impl Delegate {
    /// True if `call_agent` must use the old synchronous A2A call for this
    /// delegate rather than the unified child-task path (3.2b): a fixed
    /// endpoint has no task to start, and `via: "a2a"` opts out explicitly.
    pub fn sync_a2a(&self) -> bool {
        self.url.is_some() || self.via.as_deref() == Some("a2a")
    }
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct ToolMeta {
    pub side_effect: Option<bool>,
    pub idempotent: Option<bool>,
    /// The result may contain content from outside the agent's control
    /// (default: the MCP server's `openWorldHint`). A run that receives one
    /// drops to untrusted for the rest of the run.
    pub untrusted_output: Option<bool>,
}

/// `x-agen/config.json` `workspace` (2.45).
#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceConfig {
    /// "per_task" (its own directory, cleaned up when the task ends
    /// unless retain is set): "per_conversation" (shared by every task of
    /// one conversation, never cleaned up here): "shared" (one directory
    /// for the whole deployment, never cleaned up here).
    pub mode: WorkspaceMode,
    /// per_task only: keep the directory after the task ends instead of
    /// removing it. Default false.
    #[serde(default)]
    pub retain: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkspaceMode {
    PerTask,
    PerConversation,
    Shared,
}

impl WorkspaceConfig {
    /// The directory for one run (2.45): under `AGEN_WORKSPACE_ROOT`
    /// (default the OS temp directory's own `agen-workspaces`), namespaced
    /// by namespace/deployment, then keyed by mode — a task with no
    /// task_id (standalone/embedded use) or no conversation_key falls
    /// back to run_id, so per_task/per_conversation still give each run
    /// its own directory rather than colliding on an empty key.
    pub fn dir(
        &self,
        namespace: &str,
        deployment: &str,
        task_id: &str,
        conversation_key: &str,
        run_id: &str,
    ) -> std::path::PathBuf {
        let root = std::env::var_os("AGEN_WORKSPACE_ROOT")
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|| std::env::temp_dir().join("agen-workspaces"));
        let key = match self.mode {
            WorkspaceMode::Shared => "shared".to_string(),
            WorkspaceMode::PerConversation if !conversation_key.is_empty() => {
                format!("conversation-{}", sanitize_key(conversation_key))
            }
            WorkspaceMode::PerConversation => format!("run-{run_id}"),
            WorkspaceMode::PerTask if !task_id.is_empty() => format!("task-{task_id}"),
            WorkspaceMode::PerTask => format!("run-{run_id}"),
        };
        root.join(sanitize_key(namespace))
            .join(sanitize_key(deployment))
            .join(key)
    }
}

/// Keeps a key filesystem-safe: only ASCII letters, digits, `-` and `_`
/// pass through; anything else (including `/`, which would otherwise
/// escape the intended directory) becomes `_`.
fn sanitize_key(s: &str) -> String {
    s.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' || c == '_' {
                c
            } else {
                '_'
            }
        })
        .collect()
}

/// `x-agen/config.json`. Platform-only sections (scale, triggers, kind) are
/// kept as raw JSON; the platform owns their meaning.
#[derive(Debug, Clone, Deserialize, Default)]
pub struct Config {
    /// A directory the host creates and shares with every MCP server
    /// (via `_meta` `agen/workspace`) and the model (2.45). Unset: no
    /// workspace directory at all.
    pub workspace: Option<WorkspaceConfig>,
    #[serde(default)]
    pub permissions: Permissions,
    #[serde(default)]
    pub budget: Budget,
    #[serde(default)]
    pub limits: Limits,
    #[serde(default)]
    pub tools: BTreeMap<String, ToolMeta>,
    #[serde(default)]
    pub delegates: Vec<Delegate>,
    pub kind: Option<String>,
    pub scale: Option<Value>,
    #[serde(default)]
    pub triggers: Vec<Value>,
    #[serde(default)]
    pub memory: Vec<MemoryAttachment>,
    /// Record every model call of every run as a replayable cassette
    /// (3.6, part: `agen replay`). Off by default: a cassette holds full
    /// prompts and tool-call payloads.
    #[serde(default, rename = "recordInteractions")]
    pub record_interactions: bool,
}

/// How much of a memory block (`x-agen/config.json` `memory`) an agent may
/// do with it: see it, append, or fully rewrite it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum MemoryMode {
    #[default]
    Read,
    Append,
    Write,
}

/// A memory block attached to this bundle.
#[derive(Debug, Clone, Deserialize)]
pub struct MemoryAttachment {
    pub block: String,
    #[serde(default)]
    pub mode: MemoryMode,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SecretRef {
    pub source: String,
    pub key: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct McpServer {
    pub command: Option<String>,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    pub url: Option<String>,
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct McpConfig {
    #[serde(rename = "mcpServers")]
    pub servers: BTreeMap<String, McpServer>,
}

#[derive(Debug, Clone)]
pub struct Skill {
    pub name: String,
    pub description: String,
    pub body: String,
}

#[derive(Debug, Clone)]
pub struct Bundle {
    pub root: PathBuf,
    pub plugin: Value,
    pub agent: AgentFront,
    pub system_prompt: String,
    pub harness: Harness,
    pub config: Config,
    pub secrets: BTreeMap<String, SecretRef>,
    pub mcp: Option<McpConfig>,
    pub skills: Vec<Skill>,
    /// `sha256:<hex>` over all bundle files; identifies a Definition.
    pub digest: String,
}

impl Bundle {
    pub fn name(&self) -> &str {
        &self.agent.name
    }

    /// Resolve a bundle-relative path.
    pub fn path(&self, rel: &str) -> PathBuf {
        self.root.join(rel)
    }

    /// Load and validate a bundle directory.
    pub fn load(root: impl AsRef<Path>) -> Result<Bundle, BundleError> {
        let root = root.as_ref().to_path_buf();
        let mut v = Validation {
            root: root.clone(),
            issues: Vec::new(),
        };

        let plugin = v.json("plugin.json", PLUGIN_SCHEMA, true);
        let mcp_raw = v.json("mcp.json", MCP_SCHEMA, false);
        let harness_raw = v.json("x-agen/harness.json", HARNESS_SCHEMA, true);
        let config_raw = v.json("x-agen/config.json", CONFIG_SCHEMA, false);
        let secrets_raw = v.json("x-agen/secrets.json", SECRETS_SCHEMA, false);
        let agent = v.agent_md();
        let skills = v.skills();

        if let (Some(p), Some((front, _))) = (&plugin, &agent) {
            if let Some(pname) = p.get("name").and_then(Value::as_str) {
                if pname != front.name {
                    v.issue(
                        "x-agen/agent.md",
                        "/name",
                        format!("name {:?} must match plugin.json name {:?}", front.name, pname),
                    );
                }
            }
        }
        if let Some((front, _)) = &agent {
            for s in &front.preload_skills {
                if !front.skills.contains(s) {
                    v.issue(
                        "x-agen/agent.md",
                        "/preloadSkills",
                        format!("preloaded skill {s:?} is not in skills"),
                    );
                }
            }
            for s in &front.skills {
                if !skills.iter().any(|k| &k.name == s) {
                    v.issue(
                        "x-agen/agent.md",
                        "/skills",
                        format!("skill {s:?} not found in skills/"),
                    );
                }
            }
        }
        let harness: Option<Harness> = harness_raw.and_then(|h| v.typed("x-agen/harness.json", h));
        if let Some(h) = &harness {
            match h.provider.as_str() {
                "fake" if h.script.is_none() => v.issue(
                    "x-agen/harness.json",
                    "/script",
                    "fake provider requires \"script\"".into(),
                ),
                "replay" if h.cassette.is_none() => v.issue(
                    "x-agen/harness.json",
                    "/cassette",
                    "replay provider requires \"cassette\"".into(),
                ),
                _ => {}
            }
            if h.provider != "openrouter" {
                for (field, set) in [("/openrouter", h.openrouter.is_some()), ("/router", h.router.is_some())] {
                    if set {
                        v.issue(
                            "x-agen/harness.json",
                            field,
                            format!("{} applies to provider \"openrouter\" only", &field[1..]),
                        );
                    }
                }
            }
            match (&h.router, crate::provider::router::router_for(&h.model)) {
                (Some(_), None) => v.issue(
                    "x-agen/harness.json",
                    "/router",
                    format!(
                        "model {:?} is not a routing model (typesafe/jev-router, openrouter/auto, openrouter/auto-beta)",
                        h.model
                    ),
                ),
                (Some(r), Some(kind)) => {
                    for (field, msg) in r.check(kind) {
                        v.issue("x-agen/harness.json", field, msg);
                    }
                }
                _ => {}
            }
            if let Some(plugins) = h.openrouter.as_ref().and_then(|o| o.plugins.as_ref()) {
                if let (Some(kind), true) = (crate::provider::router::router_for(&h.model), h.router.is_some()) {
                    if plugins
                        .iter()
                        .any(|p| p.get("id").and_then(|i| i.as_str()) == Some(kind.plugin_id))
                    {
                        v.issue(
                            "x-agen/harness.json",
                            "/openrouter/plugins",
                            format!("plugin {:?} is set by \"router\"; configure it there", kind.plugin_id),
                        );
                    }
                }
            }
            if h.fallback_models.iter().any(|m| m == &h.model) {
                v.issue(
                    "x-agen/harness.json",
                    "/fallbackModels",
                    "fallbackModels repeats the primary model".into(),
                );
            }
            for (field, rel) in [("/script", &h.script), ("/cassette", &h.cassette)] {
                if let Some(rel) = rel {
                    if !root.join(rel).is_file() {
                        v.issue(
                            "x-agen/harness.json",
                            field,
                            format!("file {rel:?} not found in bundle"),
                        );
                    }
                }
            }
        }
        let config: Config = config_raw
            .and_then(|c| v.typed("x-agen/config.json", c))
            .unwrap_or_default();
        let secrets: BTreeMap<String, SecretRef> = secrets_raw
            .and_then(|s| v.typed("x-agen/secrets.json", s))
            .unwrap_or_default();
        let mcp: Option<McpConfig> = mcp_raw.and_then(|m| v.typed("mcp.json", m));

        if !v.issues.is_empty() {
            return Err(BundleError {
                root: root.display().to_string(),
                issues: v.issues,
            });
        }
        let (agent, system_prompt) = agent.expect("validated");
        let digest = digest_dir(&root).map_err(|e| BundleError {
            root: root.display().to_string(),
            issues: vec![Issue {
                file: String::new(),
                path: String::new(),
                message: format!("digest: {e}"),
            }],
        })?;
        Ok(Bundle {
            root,
            plugin: plugin.expect("validated"),
            agent,
            system_prompt,
            harness: harness.expect("validated"),
            config,
            secrets,
            mcp,
            skills,
            digest,
        })
    }
}

struct Validation {
    root: PathBuf,
    issues: Vec<Issue>,
}

impl Validation {
    fn issue(&mut self, file: &str, path: &str, message: String) {
        self.issues.push(Issue {
            file: file.into(),
            path: path.into(),
            message,
        });
    }

    fn read(&mut self, rel: &str, required: bool) -> Option<String> {
        let p = self.root.join(rel);
        match std::fs::read_to_string(&p) {
            Ok(s) => Some(s),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                if required {
                    self.issue(rel, "", "required file is missing".into());
                }
                None
            }
            Err(e) => {
                self.issue(rel, "", format!("cannot read: {e}"));
                None
            }
        }
    }

    fn check(&mut self, rel: &str, schema: &str, value: &Value) -> bool {
        let schema: Value = serde_json::from_str(schema).expect("embedded schema is valid JSON");
        let validator = jsonschema::validator_for(&schema).expect("embedded schema compiles");
        let before = self.issues.len();
        for err in validator.iter_errors(value) {
            let path = err.instance_path().to_string();
            self.issue(rel, &path, err.to_string());
        }
        self.issues.len() == before
    }

    fn json(&mut self, rel: &str, schema: &str, required: bool) -> Option<Value> {
        let text = self.read(rel, required)?;
        let value: Value = match serde_json::from_str(&text) {
            Ok(v) => v,
            Err(e) => {
                self.issue(rel, "", format!("invalid JSON: {e}"));
                return None;
            }
        };
        self.check(rel, schema, &value).then_some(value)
    }

    fn typed<T: serde::de::DeserializeOwned>(&mut self, rel: &str, value: Value) -> Option<T> {
        match serde_json::from_value(value) {
            Ok(t) => Some(t),
            Err(e) => {
                self.issue(rel, "", e.to_string());
                None
            }
        }
    }

    fn agent_md(&mut self) -> Option<(AgentFront, String)> {
        const REL: &str = "x-agen/agent.md";
        let text = self.read(REL, true)?;
        let Some((front, body)) = split_frontmatter(&text) else {
            self.issue(REL, "", "missing YAML frontmatter (--- ... ---)".into());
            return None;
        };
        let value: Value = match serde_yaml_ng::from_str(front) {
            Ok(v) => v,
            Err(e) => {
                self.issue(REL, "", format!("invalid frontmatter YAML: {e}"));
                return None;
            }
        };
        if !self.check(REL, AGENT_SCHEMA, &value) {
            return None;
        }
        let front: AgentFront = self.typed(REL, value)?;
        if body.trim().is_empty() {
            self.issue(REL, "", "system prompt body is empty".into());
            return None;
        }
        Some((front, body.trim().to_string()))
    }

    fn skills(&mut self) -> Vec<Skill> {
        let dir = self.root.join("skills");
        let Ok(entries) = std::fs::read_dir(&dir) else {
            return Vec::new();
        };
        let mut out = Vec::new();
        let mut dirs: Vec<_> = entries.flatten().filter(|e| e.path().is_dir()).collect();
        dirs.sort_by_key(|e| e.file_name());
        for e in dirs {
            let folder = e.file_name().to_string_lossy().to_string();
            let rel = format!("skills/{folder}/SKILL.md");
            let Some(text) = self.read(&rel, true) else {
                continue;
            };
            let Some((front, body)) = split_frontmatter(&text) else {
                self.issue(&rel, "", "missing YAML frontmatter".into());
                continue;
            };
            #[derive(Deserialize)]
            struct F {
                name: String,
                description: String,
            }
            match serde_yaml_ng::from_str::<F>(front) {
                Ok(f) if f.name == folder => out.push(Skill {
                    name: f.name,
                    description: f.description,
                    body: body.trim().to_string(),
                }),
                Ok(f) => self.issue(
                    &rel,
                    "/name",
                    format!("skill name {:?} must match folder {folder:?}", f.name),
                ),
                Err(e) => self.issue(&rel, "", format!("frontmatter needs name and description: {e}")),
            }
        }
        out
    }
}

/// Split `---\n<yaml>\n---\n<body>`.
fn split_frontmatter(text: &str) -> Option<(&str, &str)> {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let rest = text.strip_prefix("---")?.trim_start_matches([' ', '\t']);
    let rest = rest.strip_prefix("\r\n").or_else(|| rest.strip_prefix('\n'))?;
    let mut offset = 0;
    for line in rest.split_inclusive('\n') {
        if line.trim_end() == "---" {
            return Some((&rest[..offset], &rest[offset + line.len()..]));
        }
        offset += line.len();
    }
    None
}

/// True for paths that are never part of a bundle (VCS, caches, editor files).
/// Must match `platform/internal/definition.Ignored`.
pub fn is_ignored(rel: &str) -> bool {
    const DIRS: [&str; 4] = [".git", "node_modules", "__pycache__", ".venv"];
    let name = rel.rsplit('/').next().unwrap_or(rel);
    rel.split('/').any(|c| DIRS.contains(&c))
        || name == ".DS_Store"
        || name.ends_with('~')
        || name.ends_with(".swp")
        || name.ends_with(".pyc")
}

/// All bundle files keyed by `/`-separated relative path (byte order).
pub fn read_files(root: &Path) -> std::io::Result<BTreeMap<String, Vec<u8>>> {
    let mut out = BTreeMap::new();
    for e in walkdir::WalkDir::new(root) {
        let e = e.map_err(std::io::Error::other)?;
        if !e.file_type().is_file() {
            continue;
        }
        let rel = e
            .path()
            .strip_prefix(root)
            .unwrap_or(e.path())
            .to_string_lossy()
            .replace('\\', "/");
        if !is_ignored(&rel) {
            out.insert(rel, std::fs::read(e.path())?);
        }
    }
    Ok(out)
}

/// Definition digest: sha256 over each file in UTF-8 byte order of its path,
/// as `path || 0x00 || u64le(len) || bytes`. Must match the Go implementation
/// in `platform/internal/definition` (shared golden vector in both test suites).
pub fn digest_files(files: &BTreeMap<String, Vec<u8>>) -> String {
    let mut h = Sha256::new();
    for (rel, bytes) in files {
        h.update(rel.as_bytes());
        h.update([0]);
        h.update((bytes.len() as u64).to_le_bytes());
        h.update(bytes);
    }
    format!("sha256:{}", hex::encode(h.finalize()))
}

pub fn digest_dir(root: &Path) -> std::io::Result<String> {
    Ok(digest_files(&read_files(root)?))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn example(name: &str) -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../../examples/bundles")
            .join(name)
    }

    fn copy_dir(src: &Path, dst: &Path) {
        for e in walkdir::WalkDir::new(src).into_iter().flatten() {
            let rel = e.path().strip_prefix(src).unwrap();
            let target = dst.join(rel);
            if e.file_type().is_dir() {
                std::fs::create_dir_all(&target).unwrap();
            } else {
                std::fs::copy(e.path(), &target).unwrap();
            }
        }
    }

    fn temp_hello() -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        copy_dir(&example("hello"), dir.path());
        dir
    }

    #[test]
    fn every_example_bundle_loads() {
        let dir = example("");
        let mut n = 0;
        for e in std::fs::read_dir(&dir).unwrap().flatten() {
            if e.path().join("plugin.json").is_file() {
                if let Err(err) = Bundle::load(e.path()) {
                    panic!("{err}");
                }
                n += 1;
            }
        }
        assert!(n >= 10, "{n} example bundles");
    }

    #[test]
    fn loads_hello_example() {
        let b = Bundle::load(example("hello")).unwrap();
        assert_eq!(b.name(), "hello");
        assert_eq!(b.harness.provider, "fake");
        assert_eq!(b.skills.len(), 1);
        assert_eq!(b.skills[0].name, "greeting");
        assert!(b.system_prompt.starts_with("You are a friendly assistant"));
        assert_eq!(b.config.permissions.default, Action::Ask);
        assert!(b.digest.starts_with("sha256:") && b.digest.len() == 71);
    }

    #[test]
    fn digest_golden_vector_matches_go() {
        // Same vector as platform/internal/definition/definition_test.go.
        let files: BTreeMap<String, Vec<u8>> = [
            ("plugin.json", b"{}\n".to_vec()),
            ("a-b/x", b"1".to_vec()),
            ("a/b", b"2".to_vec()),
            ("skills/s/SKILL.md", "h\u{e9}llo".as_bytes().to_vec()),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_string(), v))
        .collect();
        assert_eq!(
            digest_files(&files),
            "sha256:0a2a61b2870af2778739855371faf070f54a1497fa8408b338764d6fa24f3bb1"
        );
    }

    #[test]
    fn ignores_vcs_and_editor_files() {
        let d = temp_hello();
        let clean = Bundle::load(d.path()).unwrap().digest;
        std::fs::create_dir_all(d.path().join(".git")).unwrap();
        std::fs::write(d.path().join(".git/HEAD"), "ref").unwrap();
        std::fs::write(d.path().join("x-agen/agent.md~"), "backup").unwrap();
        assert_eq!(clean, Bundle::load(d.path()).unwrap().digest);
        assert!(is_ignored("a/.git/config") && is_ignored("x.pyc") && !is_ignored("skills/git/SKILL.md"));
    }

    #[test]
    fn harness_router_and_openrouter_settings_are_checked() {
        let d = temp_hello();
        let h = d.path().join("x-agen/harness.json");
        let issues = |json: &str| -> Vec<String> {
            std::fs::write(&h, json).unwrap();
            match Bundle::load(d.path()) {
                Ok(_) => vec![],
                Err(e) => e
                    .issues
                    .iter()
                    .map(|i| format!("{}{}: {}", i.file, i.path, i.message))
                    .collect(),
            }
        };
        let ok = r#"{"provider":"openrouter","model":"typesafe/jev-router","fallbackModels":["deepseek/deepseek-v4-flash"],
            "reasoningEffort":"low","router":{"tier":"low","maxTier":"high","allowedModels":["deepseek/*"]},
            "openrouter":{"provider":{"sort":"throughput"},"plugins":[{"id":"web"}]}}"#;
        assert!(issues(ok).is_empty(), "{:?}", issues(ok));
        let b = Bundle::load(d.path()).unwrap();
        assert_eq!(b.harness.router.unwrap().max_tier.as_deref(), Some("high"));
        let has = |json: &str, want: &str| {
            let got = issues(json);
            assert!(got.iter().any(|i| i.contains(want)), "want {want:?} in {got:?}");
        };
        has(
            r#"{"provider":"openrouter","model":"typesafe/jev-router","router":{"tier":"max"}}"#,
            "/router/tier",
        );
        has(
            r#"{"provider":"openrouter","model":"deepseek/deepseek-v4-flash","router":{"tier":"low"}}"#,
            "not a routing model",
        );
        has(
            r#"{"provider":"openai","model":"gpt-x","openrouter":{"provider":{}}}"#,
            "applies to provider",
        );
        has(
            r#"{"provider":"openrouter","model":"typesafe/jev-router","router":{"tier":"low"},"openrouter":{"plugins":[{"id":"jev-router"}]}}"#,
            "configure it there",
        );
        has(
            r#"{"provider":"openrouter","model":"a/b","fallbackModels":["a/b"]}"#,
            "repeats the primary",
        );
        has(
            r#"{"provider":"openrouter","model":"a/b","router":{"tier":"cheap"}}"#,
            "/router/tier",
        );
    }

    #[test]
    fn digest_is_stable_and_content_sensitive() {
        let d = temp_hello();
        let a = Bundle::load(d.path()).unwrap().digest;
        assert_eq!(a, Bundle::load(d.path()).unwrap().digest);
        std::fs::write(
            d.path().join("x-agen/agent.md"),
            "---\nname: hello\ndescription: x\nskills: [greeting]\n---\nDifferent prompt.\n",
        )
        .unwrap();
        assert_ne!(a, Bundle::load(d.path()).unwrap().digest);
    }

    #[test]
    fn reports_all_issues_with_locations() {
        let d = temp_hello();
        std::fs::write(
            d.path().join("x-agen/harness.json"),
            r#"{"provider":"nope","model":"m","extra":1}"#,
        )
        .unwrap();
        std::fs::write(
            d.path().join("x-agen/config.json"),
            r#"{"permissions":{"default":"maybe"}}"#,
        )
        .unwrap();
        std::fs::write(
            d.path().join("x-agen/secrets.json"),
            r#"{"API_KEY":{"source":"env","value":"sk-123"}}"#,
        )
        .unwrap();
        std::fs::remove_file(d.path().join("plugin.json")).unwrap();
        let err = Bundle::load(d.path()).unwrap_err();
        let text = err.to_string();
        let has = |file: &str, path: &str| err.issues.iter().any(|i| i.file == file && i.path.starts_with(path));
        assert!(has("plugin.json", ""), "{text}");
        assert!(has("x-agen/harness.json", "/provider"), "{text}");
        assert!(has("x-agen/harness.json", ""), "{text}");
        assert!(has("x-agen/config.json", "/permissions/default"), "{text}");
        assert!(
            has("x-agen/secrets.json", "/API_KEY"),
            "secret values must be rejected: {text}"
        );
        assert!(err.issues.len() >= 5, "{text}");
    }

    #[test]
    fn rejects_name_mismatch_missing_skill_and_script() {
        let d = temp_hello();
        std::fs::write(
            d.path().join("x-agen/agent.md"),
            "---\nname: other\ndescription: d\nskills: [missing]\n---\nPrompt\n",
        )
        .unwrap();
        std::fs::remove_file(d.path().join("x-agen/fake-script.json")).unwrap();
        let err = Bundle::load(d.path()).unwrap_err();
        let msgs: Vec<String> = err.issues.iter().map(|i| i.to_string()).collect();
        assert!(
            msgs.iter().any(|m| m.contains("must match plugin.json name")),
            "{msgs:?}"
        );
        assert!(
            msgs.iter().any(|m| m.contains("skill \"missing\" not found")),
            "{msgs:?}"
        );
        assert!(msgs.iter().any(|m| m.contains("not found in bundle")), "{msgs:?}");
    }

    #[test]
    fn frontmatter_parsing() {
        assert_eq!(split_frontmatter("---\na: 1\n---\nbody"), Some(("a: 1\n", "body")));
        assert_eq!(
            split_frontmatter("---\r\na: 1\r\n---\r\nbody"),
            Some(("a: 1\r\n", "body"))
        );
        assert_eq!(split_frontmatter("no frontmatter"), None);
        assert_eq!(split_frontmatter("---\nunterminated"), None);
    }

    #[test]
    fn when_matches_against_specific_argument_values() {
        let glob = When {
            trust: None,
            trust_below: None,
            args: BTreeMap::from([("path".to_string(), ArgMatch::Glob { glob: "/tmp/*".into() })]),
            labels: BTreeMap::new(),
            time: None,
        };
        assert!(glob.matches(Trust::Trusted, &serde_json::json!({"path": "/tmp/x"}), &BTreeMap::new()));
        assert!(!glob.matches(Trust::Trusted, &serde_json::json!({"path": "/etc/x"}), &BTreeMap::new()));

        let threshold = When {
            trust: None,
            trust_below: None,
            args: BTreeMap::from([("amount".to_string(), ArgMatch::Gt { gt: 100.0 })]),
            labels: BTreeMap::new(),
            time: None,
        };
        assert!(threshold.matches(Trust::Trusted, &serde_json::json!({"amount": 150}), &BTreeMap::new()));
        assert!(!threshold.matches(Trust::Trusted, &serde_json::json!({"amount": 50}), &BTreeMap::new()));
        assert!(
            !threshold.matches(Trust::Trusted, &serde_json::json!({}), &BTreeMap::new()),
            "a missing argument never matches"
        );
    }

    #[test]
    fn when_matches_against_task_labels() {
        let w = When {
            trust: None,
            trust_below: None,
            args: BTreeMap::new(),
            labels: BTreeMap::from([("env".to_string(), "prod".to_string())]),
            time: None,
        };
        assert!(w.matches(
            Trust::Trusted,
            &Value::Null,
            &BTreeMap::from([("env".to_string(), "prod".to_string())])
        ));
        assert!(!w.matches(
            Trust::Trusted,
            &Value::Null,
            &BTreeMap::from([("env".to_string(), "staging".to_string())])
        ));
        assert!(
            !w.matches(Trust::Trusted, &Value::Null, &BTreeMap::new()),
            "a missing label never matches"
        );
    }

    #[test]
    fn time_window_matches_within_bounds_and_day() {
        let w = TimeWindow {
            window: "09:00-17:00".into(),
            days: vec!["wed".into()],
        };
        // Epoch day 6 (1970-01-07) is a Wednesday.
        let within = std::time::UNIX_EPOCH + std::time::Duration::from_secs(6 * 86_400 + 10 * 3600);
        assert!(w.contains(within));
        let outside_hours = std::time::UNIX_EPOCH + std::time::Duration::from_secs(6 * 86_400 + 20 * 3600);
        assert!(!w.contains(outside_hours));
        // Epoch day 5 (1970-01-06) is a Tuesday, same time of day.
        let wrong_day = std::time::UNIX_EPOCH + std::time::Duration::from_secs(5 * 86_400 + 10 * 3600);
        assert!(!w.contains(wrong_day));
    }
}
