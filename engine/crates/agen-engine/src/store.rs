//! Storage port: sessions, conversations, messages, runs, spans, the effect
//! ledger, delegation counters and logs. One implementation over sqlx `Any`
//! serves SQLite (local) and Postgres (distributed); both run the same SQL
//! (numbered `$N` placeholders, used in order) and the migrations in
//! `spec/sql/<dialect>/`.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};
use sqlx::any::{AnyPoolOptions, AnyRow};
use sqlx::{AnyPool, Row};

use crate::provider::{Message, Usage};

macro_rules! run_cols {
    () => {
        "id, session_id, conversation_id, namespace, deployment, definition_digest, task_id, parent_run_id, root_run_id, status, input, output, error, step, input_tokens, output_tokens, cost_usd, trace_id, started_ms, ended_ms, requested_by, labels, outcome"
    };
}

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("store: {0}")]
    Db(#[from] sqlx::Error),
    #[error("store: not found: {0}")]
    NotFound(String),
    #[error("store: {0}")]
    Invalid(String),
    #[error("store: conflict: {0}")]
    Conflict(String),
    /// Another process has taken ownership of this run.
    #[error("store: run {0} is owned by another process")]
    Fenced(String),
}

pub type Result<T> = std::result::Result<T, StoreError>;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dialect {
    Sqlite,
    Postgres,
}

struct Migration {
    version: i64,
    sqlite: &'static str,
    postgres: &'static str,
}

const MIGRATIONS: &[Migration] = &[
    Migration {
        version: 1,
        sqlite: include_str!("../../../../spec/sql/sqlite/0001_run_data.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0001_run_data.sql"),
    },
    Migration {
        version: 2,
        sqlite: include_str!("../../../../spec/sql/sqlite/0002_run_ownership.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0002_run_ownership.sql"),
    },
    Migration {
        version: 5,
        sqlite: include_str!("../../../../spec/sql/sqlite/0005_platform.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0005_platform.sql"),
    },
    Migration {
        version: 4,
        sqlite: include_str!("../../../../spec/sql/sqlite/0004_span_seq.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0004_span_seq.sql"),
    },
    Migration {
        version: 3,
        sqlite: include_str!("../../../../spec/sql/sqlite/0003_task_idempotency.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0003_task_idempotency.sql"),
    },
    Migration {
        version: 6,
        sqlite: include_str!("../../../../spec/sql/sqlite/0006_deployment_paused.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0006_deployment_paused.sql"),
    },
    Migration {
        version: 7,
        sqlite: include_str!("../../../../spec/sql/sqlite/0007_task_submitter.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0007_task_submitter.sql"),
    },
    Migration {
        version: 8,
        sqlite: include_str!("../../../../spec/sql/sqlite/0008_webhook_secrets.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0008_webhook_secrets.sql"),
    },
    Migration {
        version: 9,
        sqlite: include_str!("../../../../spec/sql/sqlite/0009_delegation_calls.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0009_delegation_calls.sql"),
    },
    Migration {
        version: 10,
        sqlite: include_str!("../../../../spec/sql/sqlite/0010_hub_ca.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0010_hub_ca.sql"),
    },
    Migration {
        version: 11,
        sqlite: include_str!("../../../../spec/sql/sqlite/0011_hub_token_key.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0011_hub_token_key.sql"),
    },
    Migration {
        version: 12,
        sqlite: include_str!("../../../../spec/sql/sqlite/0012_run_requested_by.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0012_run_requested_by.sql"),
    },
    Migration {
        version: 13,
        sqlite: include_str!("../../../../spec/sql/sqlite/0013_approval_pending_unique.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0013_approval_pending_unique.sql"),
    },
    Migration {
        version: 14,
        sqlite: include_str!("../../../../spec/sql/sqlite/0014_approval_pending_hash.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0014_approval_pending_hash.sql"),
    },
    Migration {
        version: 15,
        sqlite: include_str!("../../../../spec/sql/sqlite/0015_platform_secrets.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0015_platform_secrets.sql"),
    },
    Migration {
        version: 16,
        sqlite: include_str!("../../../../spec/sql/sqlite/0016_nest_prev_cert.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0016_nest_prev_cert.sql"),
    },
    Migration {
        version: 17,
        sqlite: include_str!("../../../../spec/sql/sqlite/0017_token_key_retired.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0017_token_key_retired.sql"),
    },
    Migration {
        version: 18,
        sqlite: include_str!("../../../../spec/sql/sqlite/0018_platform_secret_deployments.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0018_platform_secret_deployments.sql"),
    },
    Migration {
        version: 19,
        sqlite: include_str!("../../../../spec/sql/sqlite/0019_work_identity.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0019_work_identity.sql"),
    },
    Migration {
        version: 20,
        sqlite: include_str!("../../../../spec/sql/sqlite/0020_console.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0020_console.sql"),
    },
    Migration {
        version: 21,
        sqlite: include_str!("../../../../spec/sql/sqlite/0021_task_schedule_tier.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0021_task_schedule_tier.sql"),
    },
    Migration {
        version: 22,
        sqlite: include_str!("../../../../spec/sql/sqlite/0022_provider_holds.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0022_provider_holds.sql"),
    },
    Migration {
        version: 23,
        sqlite: include_str!("../../../../spec/sql/sqlite/0023_task_reply.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0023_task_reply.sql"),
    },
    Migration {
        version: 24,
        sqlite: include_str!("../../../../spec/sql/sqlite/0024_deployment_versions.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0024_deployment_versions.sql"),
    },
    Migration {
        version: 25,
        sqlite: include_str!("../../../../spec/sql/sqlite/0025_outcomes.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0025_outcomes.sql"),
    },
    Migration {
        version: 26,
        sqlite: include_str!("../../../../spec/sql/sqlite/0026_events.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0026_events.sql"),
    },
    Migration {
        version: 27,
        sqlite: include_str!("../../../../spec/sql/sqlite/0027_subscriptions.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0027_subscriptions.sql"),
    },
    Migration {
        version: 28,
        sqlite: include_str!("../../../../spec/sql/sqlite/0028_event_triggers.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0028_event_triggers.sql"),
    },
    Migration {
        version: 29,
        sqlite: include_str!("../../../../spec/sql/sqlite/0029_task_overrides.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0029_task_overrides.sql"),
    },
    Migration {
        version: 30,
        sqlite: include_str!("../../../../spec/sql/sqlite/0030_task_scheduling.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0030_task_scheduling.sql"),
    },
    Migration {
        version: 31,
        sqlite: include_str!("../../../../spec/sql/sqlite/0031_feedback.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0031_feedback.sql"),
    },
    Migration {
        version: 32,
        sqlite: include_str!("../../../../spec/sql/sqlite/0032_task_priority.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0032_task_priority.sql"),
    },
    Migration {
        version: 33,
        sqlite: include_str!("../../../../spec/sql/sqlite/0033_trust.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0033_trust.sql"),
    },
    Migration {
        version: 34,
        sqlite: include_str!("../../../../spec/sql/sqlite/0034_memory_blocks.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0034_memory_blocks.sql"),
    },
    Migration {
        version: 35,
        sqlite: include_str!("../../../../spec/sql/sqlite/0035_locks.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0035_locks.sql"),
    },
    Migration {
        version: 36,
        sqlite: include_str!("../../../../spec/sql/sqlite/0036_artifacts.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0036_artifacts.sql"),
    },
    Migration {
        version: 37,
        sqlite: include_str!("../../../../spec/sql/sqlite/0037_run_inbox.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0037_run_inbox.sql"),
    },
    Migration {
        version: 38,
        sqlite: include_str!("../../../../spec/sql/sqlite/0038_on_parent_end.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0038_on_parent_end.sql"),
    },
    Migration {
        version: 39,
        sqlite: include_str!("../../../../spec/sql/sqlite/0039_root_task_id.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0039_root_task_id.sql"),
    },
    Migration {
        version: 40,
        sqlite: include_str!("../../../../spec/sql/sqlite/0040_task_progress.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0040_task_progress.sql"),
    },
    Migration {
        version: 41,
        sqlite: include_str!("../../../../spec/sql/sqlite/0041_conversation_queue_mode.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0041_conversation_queue_mode.sql"),
    },
    Migration {
        version: 42,
        sqlite: include_str!("../../../../spec/sql/sqlite/0042_task_hold.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0042_task_hold.sql"),
    },
    Migration {
        version: 43,
        sqlite: include_str!("../../../../spec/sql/sqlite/0043_cassettes.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0043_cassettes.sql"),
    },
    Migration {
        version: 44,
        sqlite: include_str!("../../../../spec/sql/sqlite/0044_task_merge_keys.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0044_task_merge_keys.sql"),
    },
    Migration {
        version: 45,
        sqlite: include_str!("../../../../spec/sql/sqlite/0045_lock_fencing.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0045_lock_fencing.sql"),
    },
    Migration {
        version: 46,
        sqlite: include_str!("../../../../spec/sql/sqlite/0046_conversation_rollover.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0046_conversation_rollover.sql"),
    },
    Migration {
        version: 47,
        sqlite: include_str!("../../../../spec/sql/sqlite/0047_eval_reports.sql"),
        postgres: include_str!("../../../../spec/sql/postgres/0047_eval_reports.sql"),
    },
];

// Postgres advisory lock id guarding migrations: 1634166126 = 0x6167656e ("agen").
// Must match the Go migrator.

pub fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// IDs made by one process sort in creation order, also within a millisecond.
pub fn new_id() -> String {
    static GENERATOR: std::sync::Mutex<ulid::Generator> = std::sync::Mutex::new(ulid::Generator::new());
    let mut g = GENERATOR.lock().unwrap_or_else(|e| e.into_inner());
    g.generate().unwrap_or_else(|_| ulid::Ulid::generate()).to_string()
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Session {
    pub id: String,
    pub agent: String,
    pub namespace: String,
    pub deployment: String,
    pub memory: serde_json::Value,
}

/// A durable, named block of text an agent reads and updates across runs
/// and conversations (`x-agen/config.json` `memory`), namespace-scoped so
/// a namespace's deployments can share one.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct MemoryBlock {
    pub namespace: String,
    pub id: String,
    pub label: String,
    pub description: String,
    pub value: String,
    /// 0: no limit.
    pub limit_chars: i64,
    /// Bumped on every update; `update_memory_block`'s `expected_version`
    /// is checked against it so parallel writers cannot silently clobber
    /// each other.
    pub version: i64,
    pub updated_ms: i64,
}

/// How `update_memory_block` changes a block's value.
#[derive(Debug, Clone)]
pub enum MemoryOp {
    Replace(String),
    Append(String),
    /// Exactly one occurrence of `old` in the current value becomes `new`.
    StrReplace {
        old: String,
        new: String,
    },
}

/// Named, content-addressed text an agent saves during a run and reads
/// back later (`save_artifact` / `read_artifact`), so a long result does
/// not have to stay in the model's context.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Artifact {
    pub namespace: String,
    /// "sha256:<hex>" of content.
    pub id: String,
    pub name: String,
    pub media_type: String,
    /// Byte length of content.
    pub size: i64,
    pub content: String,
    pub task_id: String,
    pub run_id: String,
    pub created_ms: i64,
}

/// What `save_artifact` needs beyond the namespace and quota.
pub struct NewArtifact<'a> {
    pub name: &'a str,
    pub content: &'a str,
    pub media_type: &'a str,
    pub task_id: &'a str,
    pub run_id: &'a str,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct RunRecord {
    pub id: String,
    pub session_id: String,
    pub conversation_id: String,
    pub namespace: String,
    pub deployment: String,
    pub definition_digest: String,
    pub task_id: String,
    pub parent_run_id: String,
    pub root_run_id: String,
    pub status: RunStatus,
    pub input: String,
    pub output: String,
    pub error: String,
    pub step: i64,
    pub usage: Usage,
    pub trace_id: String,
    pub started_ms: i64,
    pub ended_ms: Option<i64>,
    /// Who asked for this run when no Hub task says so: the principal of a
    /// verified A2A call token (set by the Gateway). Used for approvals.
    pub requested_by: String,
    /// The task's labels, passed on to tool calls and delegated tasks.
    #[serde(default)]
    pub labels: BTreeMap<String, String>,
    /// How the run ended (status, reason, report, data); None while it runs.
    #[serde(default)]
    pub outcome: Option<crate::outcome::RunOutcome>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RunStatus {
    Running,
    WaitingApproval,
    Succeeded,
    Failed,
    Cancelled,
}

impl RunStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            RunStatus::Running => "running",
            RunStatus::WaitingApproval => "waiting_approval",
            RunStatus::Succeeded => "succeeded",
            RunStatus::Failed => "failed",
            RunStatus::Cancelled => "cancelled",
        }
    }
    fn parse(s: &str) -> Result<Self> {
        Ok(match s {
            "running" => RunStatus::Running,
            "waiting_approval" => RunStatus::WaitingApproval,
            "succeeded" => RunStatus::Succeeded,
            "failed" => RunStatus::Failed,
            "cancelled" => RunStatus::Cancelled,
            other => return Err(StoreError::Invalid(format!("run status {other:?}"))),
        })
    }
    pub fn is_terminal(self) -> bool {
        matches!(self, RunStatus::Succeeded | RunStatus::Failed | RunStatus::Cancelled)
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SpanRecord {
    pub span_id: String,
    pub trace_id: String,
    pub parent_span_id: String,
    pub run_id: String,
    pub name: String,
    pub start_ms: i64,
    /// Start order within the run (ties on `start_ms` are ordered by this).
    #[serde(default)]
    pub seq: i64,
    pub end_ms: i64,
    pub status: String,
    pub attributes: serde_json::Value,
}

/// Outcome of registering a side-effecting tool call in the ledger.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EffectBegin {
    /// First time: execute the call, then `complete_effect`.
    Proceed,
    /// Already executed and recorded: reuse the result, do not execute again.
    Completed { result: String },
    /// A previous attempt started but never recorded completion (crash): the
    /// effect may or may not have happened.
    Unknown,
}

pub struct Store {
    pool: AnyPool,
    dialect: Dialect,
    url: String,
}

impl Store {
    /// Open a store. Accepts `sqlite://path` / `sqlite::memory:` or
    /// `postgres://…`. Applies pending migrations.
    pub async fn open(url: &str) -> Result<Store> {
        sqlx::any::install_default_drivers();
        let dialect = if url.starts_with("sqlite:") {
            Dialect::Sqlite
        } else if url.starts_with("postgres:") || url.starts_with("postgresql:") {
            Dialect::Postgres
        } else {
            return Err(StoreError::Invalid(format!("unsupported store url {url:?}")));
        };
        let url = normalize_sqlite_url(url);
        let url = url.as_str();
        let url = if dialect == Dialect::Sqlite && !url.contains("mode=") && !url.contains(":memory:") {
            format!("{url}{}mode=rwc", if url.contains('?') { "&" } else { "?" })
        } else {
            url.to_string()
        };
        let mut opts = AnyPoolOptions::new();
        if dialect == Dialect::Sqlite {
            // In-memory SQLite is per-connection; keep one so tests share it.
            if url.contains(":memory:") {
                opts = opts.max_connections(1);
            }
            opts = opts.after_connect(|conn, _| {
                Box::pin(async move {
                    for pragma in [
                        "PRAGMA journal_mode=WAL",
                        "PRAGMA busy_timeout=10000",
                        "PRAGMA foreign_keys=ON",
                        "PRAGMA synchronous=NORMAL",
                    ] {
                        sqlx::query(pragma).execute(&mut *conn).await?;
                    }
                    Ok(())
                })
            });
        }
        let pool = opts.connect(&url).await?;
        let store = Store {
            pool,
            dialect,
            url: url.to_string(),
        };
        store.migrate().await?;
        Ok(store)
    }

    /// Open (creating if needed) a SQLite store at a filesystem path.
    pub async fn open_sqlite_path(path: &std::path::Path) -> Result<Store> {
        Store::open(&format!("sqlite:{}", path.display().to_string().replace('\\', "/"))).await
    }

    /// The (normalised) URL this store was opened with. May contain
    /// credentials; never log it.
    pub fn url(&self) -> String {
        self.url.clone()
    }

    pub fn dialect(&self) -> Dialect {
        self.dialect
    }

    async fn migrate(&self) -> Result<()> {
        let mut conn = self.pool.acquire().await?;
        // Already up to date: no DDL and no lock, so a least-privilege role
        // (agen store host-role: run-data tables only) can open the Store.
        if let Ok(rows) = sqlx::query("SELECT version FROM schema_migrations")
            .fetch_all(&mut *conn)
            .await
        {
            let applied: Vec<i64> = rows.iter().map(|r| r.get::<i64, _>(0)).collect();
            if MIGRATIONS.iter().all(|m| applied.contains(&m.version)) {
                return Ok(());
            }
        }
        match self.dialect {
            Dialect::Postgres => {
                sqlx::query("SELECT pg_advisory_lock(1634166126)")
                    .execute(&mut *conn)
                    .await?;
            }
            Dialect::Sqlite => {
                sqlx::query("BEGIN IMMEDIATE").execute(&mut *conn).await?;
            }
        }
        let result = async {
            // Re-check under the lock: another process (a Hub) may have just
            // applied everything, and a restricted role must not attempt DDL.
            if let Ok(rows) = sqlx::query("SELECT version FROM schema_migrations")
                .fetch_all(&mut *conn)
                .await
            {
                let applied: Vec<i64> = rows.iter().map(|r| r.get::<i64, _>(0)).collect();
                if MIGRATIONS.iter().all(|m| applied.contains(&m.version)) {
                    return Ok(());
                }
            }
            sqlx::query(
                "CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, applied_ms BIGINT NOT NULL)",
            )
            .execute(&mut *conn)
            .await?;
            let applied: Vec<i64> = sqlx::query("SELECT version FROM schema_migrations")
                .fetch_all(&mut *conn)
                .await?
                .iter()
                .map(|r| r.get::<i64, _>(0))
                .collect();
            for m in MIGRATIONS {
                if applied.contains(&m.version) {
                    continue;
                }
                let sql = match self.dialect {
                    Dialect::Sqlite => m.sqlite,
                    Dialect::Postgres => m.postgres,
                };
                if self.dialect == Dialect::Postgres {
                    sqlx::query("BEGIN").execute(&mut *conn).await?;
                }
                for stmt in split_sql(sql) {
                    sqlx::query(stmt).execute(&mut *conn).await?;
                }
                sqlx::query("INSERT INTO schema_migrations (version, applied_ms) VALUES ($1, $2)")
                    .bind(m.version)
                    .bind(now_ms())
                    .execute(&mut *conn)
                    .await?;
                if self.dialect == Dialect::Postgres {
                    sqlx::query("COMMIT").execute(&mut *conn).await?;
                }
            }
            Ok::<_, sqlx::Error>(())
        }
        .await;
        match self.dialect {
            Dialect::Postgres => {
                if result.is_err() {
                    let _ = sqlx::query("ROLLBACK").execute(&mut *conn).await;
                }
                sqlx::query("SELECT pg_advisory_unlock(1634166126)")
                    .execute(&mut *conn)
                    .await?;
            }
            Dialect::Sqlite => {
                let end = if result.is_ok() { "COMMIT" } else { "ROLLBACK" };
                sqlx::query(end).execute(&mut *conn).await?;
            }
        }
        result.map_err(|e| {
            // A least-privilege role cannot migrate: say what to do.
            if e.to_string().contains("permission denied") {
                StoreError::Invalid(format!(
                    "the Store schema is older than this agen-host and this role may not migrate it \
                     (start a Hub of this version, or run agen migrate, first): {e}"
                ))
            } else {
                e.into()
            }
        })
    }

    // ---- sessions & conversations ----

    pub async fn create_session(&self, agent: &str, namespace: &str, deployment: &str) -> Result<Session> {
        let s = Session {
            id: new_id(),
            agent: agent.into(),
            namespace: namespace.into(),
            deployment: deployment.into(),
            memory: serde_json::json!({}),
        };
        let now = now_ms();
        sqlx::query("INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ($1, $2, $3, $4, $5, $6, $7)")
            .bind(&s.id)
            .bind(&s.agent)
            .bind(&s.namespace)
            .bind(&s.deployment)
            .bind(s.memory.to_string())
            .bind(now)
            .bind(now)
            .execute(&self.pool)
            .await?;
        Ok(s)
    }

    pub async fn get_session(&self, id: &str) -> Result<Session> {
        let row = sqlx::query("SELECT id, agent, namespace, deployment, memory FROM sessions WHERE id = $1")
            .bind(id)
            .fetch_optional(&self.pool)
            .await?
            .ok_or_else(|| StoreError::NotFound(format!("session {id}")))?;
        Ok(Session {
            id: row.get(0),
            agent: row.get(1),
            namespace: row.get(2),
            deployment: row.get(3),
            memory: parse_json(&row.get::<String, _>(4))?,
        })
    }

    /// The single session of a singleton deployment, created on first use.
    /// Safe under concurrency: a unique index admits one singleton session.
    pub async fn session_for_deployment(&self, agent: &str, namespace: &str, deployment: &str) -> Result<Session> {
        let now = now_ms();
        sqlx::query(
            "INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms, singleton) \
             VALUES ($1, $2, $3, $4, '{}', $5, $6, 1) ON CONFLICT (namespace, deployment) WHERE singleton = 1 DO NOTHING",
        )
        .bind(new_id())
        .bind(agent)
        .bind(namespace)
        .bind(deployment)
        .bind(now)
        .bind(now)
        .execute(&self.pool)
        .await?;
        let row = sqlx::query("SELECT id FROM sessions WHERE namespace = $1 AND deployment = $2 AND singleton = 1")
            .bind(namespace)
            .bind(deployment)
            .fetch_one(&self.pool)
            .await?;
        self.get_session(&row.get::<String, _>(0)).await
    }

    /// The session for a caller-chosen conversation key, created on first
    /// use. Safe under concurrency: a unique index admits one per key.
    pub async fn session_for_key(&self, agent: &str, namespace: &str, deployment: &str, key: &str) -> Result<Session> {
        let now = now_ms();
        sqlx::query(
            "INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms, conversation_key) \
             VALUES ($1, $2, $3, $4, '{}', $5, $6, $7) ON CONFLICT (namespace, deployment, conversation_key) WHERE conversation_key <> '' DO NOTHING",
        )
        .bind(new_id())
        .bind(agent)
        .bind(namespace)
        .bind(deployment)
        .bind(now)
        .bind(now)
        .bind(key)
        .execute(&self.pool)
        .await?;
        let row =
            sqlx::query("SELECT id FROM sessions WHERE namespace = $1 AND deployment = $2 AND conversation_key = $3")
                .bind(namespace)
                .bind(deployment)
                .bind(key)
                .fetch_one(&self.pool)
                .await?;
        self.get_session(&row.get::<String, _>(0)).await
    }

    pub async fn set_memory(&self, session_id: &str, memory: &serde_json::Value) -> Result<()> {
        sqlx::query("UPDATE sessions SET memory = $1, updated_ms = $2 WHERE id = $3")
            .bind(memory.to_string())
            .bind(now_ms())
            .bind(session_id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    fn memory_block_row(row: AnyRow) -> MemoryBlock {
        MemoryBlock {
            namespace: row.get(0),
            id: row.get(1),
            label: row.get(2),
            description: row.get(3),
            value: row.get(4),
            limit_chars: row.get(5),
            version: row.get(6),
            updated_ms: row.get(7),
        }
    }

    pub async fn get_memory_block(&self, namespace: &str, id: &str) -> Result<Option<MemoryBlock>> {
        let row = sqlx::query(
            "SELECT namespace, id, label, description, value, limit_chars, version, updated_ms \
             FROM memory_blocks WHERE namespace = $1 AND id = $2",
        )
        .bind(namespace)
        .bind(id)
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(Self::memory_block_row))
    }

    /// A namespace's memory blocks, newest-updated first (the API:
    /// ListMemoryBlocks).
    pub async fn list_memory_blocks(&self, namespace: &str) -> Result<Vec<MemoryBlock>> {
        let rows = sqlx::query(
            "SELECT namespace, id, label, description, value, limit_chars, version, updated_ms \
             FROM memory_blocks WHERE namespace = $1 ORDER BY updated_ms DESC, id",
        )
        .bind(namespace)
        .fetch_all(&self.pool)
        .await?;
        Ok(rows.into_iter().map(Self::memory_block_row).collect())
    }

    /// Replaces a block's value outright (the API: SetMemoryBlock),
    /// creating it if it does not exist yet. Bumps the version.
    pub async fn set_memory_block(
        &self,
        namespace: &str,
        id: &str,
        label: &str,
        description: &str,
        value: &str,
        limit_chars: i64,
    ) -> Result<MemoryBlock> {
        let now = now_ms();
        sqlx::query(
            "INSERT INTO memory_blocks (namespace, id, label, description, value, limit_chars, version, updated_ms) \
             VALUES ($1, $2, $3, $4, $5, $6, 1, $7) \
             ON CONFLICT (namespace, id) DO UPDATE SET label = excluded.label, description = excluded.description, \
             value = excluded.value, limit_chars = excluded.limit_chars, version = memory_blocks.version + 1, updated_ms = excluded.updated_ms",
        )
        .bind(namespace)
        .bind(id)
        .bind(label)
        .bind(description)
        .bind(value)
        .bind(limit_chars)
        .bind(now)
        .execute(&self.pool)
        .await?;
        self.get_memory_block(namespace, id)
            .await?
            .ok_or_else(|| StoreError::NotFound(format!("memory block {namespace}/{id}")))
    }

    /// Compare-and-set update (the `memory_update` tool): applies `op` to
    /// the block's current value, creating it (at version 0) on first
    /// write. With `expected_version`, a concurrent writer is a conflict
    /// the caller must retry deliberately; without one, a concurrent
    /// write is retried once against the fresh value.
    pub async fn update_memory_block(
        &self,
        namespace: &str,
        id: &str,
        op: &MemoryOp,
        expected_version: Option<i64>,
    ) -> Result<MemoryBlock> {
        let now = now_ms();
        sqlx::query(
            "INSERT INTO memory_blocks (namespace, id, label, description, value, limit_chars, version, updated_ms) \
             VALUES ($1, $2, '', '', '', 0, 0, $3) ON CONFLICT (namespace, id) DO NOTHING",
        )
        .bind(namespace)
        .bind(id)
        .bind(now)
        .execute(&self.pool)
        .await?;
        for _ in 0..10 {
            let cur = self
                .get_memory_block(namespace, id)
                .await?
                .ok_or_else(|| StoreError::NotFound(format!("memory block {namespace}/{id}")))?;
            if let Some(want) = expected_version {
                if want != cur.version {
                    return Err(StoreError::Conflict(format!(
                        "memory block {namespace}/{id} is at version {}, not {want} (someone else updated it)",
                        cur.version
                    )));
                }
            }
            let new_value = match op {
                MemoryOp::Replace(text) => text.clone(),
                MemoryOp::Append(text) => format!("{}{}", cur.value, text),
                MemoryOp::StrReplace { old, new } => {
                    let n = cur.value.matches(old.as_str()).count();
                    if n != 1 {
                        return Err(StoreError::Invalid(format!(
                            "{old:?} occurs {n} times in memory block {namespace}/{id}; it must occur exactly once"
                        )));
                    }
                    cur.value.replacen(old.as_str(), new, 1)
                }
            };
            if cur.limit_chars > 0 && new_value.chars().count() as i64 > cur.limit_chars {
                return Err(StoreError::Invalid(format!(
                    "memory block {namespace}/{id} would be {} chars, over its limit of {}",
                    new_value.chars().count(),
                    cur.limit_chars
                )));
            }
            let res = sqlx::query(
                "UPDATE memory_blocks SET value = $1, version = version + 1, updated_ms = $2 WHERE namespace = $3 AND id = $4 AND version = $5",
            )
            .bind(&new_value)
            .bind(now)
            .bind(namespace)
            .bind(id)
            .bind(cur.version)
            .execute(&self.pool)
            .await?;
            if res.rows_affected() == 1 {
                return self
                    .get_memory_block(namespace, id)
                    .await?
                    .ok_or_else(|| StoreError::NotFound(format!("memory block {namespace}/{id}")));
            }
            if expected_version.is_some() {
                return Err(StoreError::Conflict(format!(
                    "memory block {namespace}/{id} changed concurrently; try again"
                )));
            }
            // No specific version was demanded: retry against the value as it now stands.
        }
        Err(StoreError::Conflict(format!(
            "memory block {namespace}/{id} changed concurrently too many times; try again"
        )))
    }

    fn artifact_row(row: AnyRow) -> Artifact {
        Artifact {
            namespace: row.get(0),
            id: row.get(1),
            name: row.get(2),
            media_type: row.get(3),
            size: row.get(4),
            content: row.get(5),
            task_id: row.get(6),
            run_id: row.get(7),
            created_ms: row.get(8),
        }
    }

    /// Saves content as an artifact (`save_artifact`), content-addressed:
    /// saving the same bytes again in the same namespace returns the
    /// existing row (its name and task/run stay the first save's).
    /// `quota` (0: no limit) bounds the namespace's total artifact bytes.
    pub async fn save_artifact(&self, namespace: &str, new: NewArtifact<'_>, quota: i64) -> Result<Artifact> {
        let NewArtifact {
            name,
            content,
            media_type,
            task_id,
            run_id,
        } = new;
        use sha2::{Digest, Sha256};
        let id = format!("sha256:{}", hex::encode(Sha256::digest(content.as_bytes())));
        if let Some(existing) = self.get_artifact(namespace, &id).await? {
            return Ok(existing);
        }
        let size = content.len() as i64;
        if quota > 0 {
            let used: i64 = sqlx::query("SELECT COALESCE(SUM(size), 0) FROM artifacts WHERE namespace = $1")
                .bind(namespace)
                .fetch_one(&self.pool)
                .await?
                .get(0);
            if used + size > quota {
                return Err(StoreError::Invalid(format!(
                    "saving this artifact ({size} bytes) would put {namespace} over its {quota}-byte artifact quota ({used} used)"
                )));
            }
        }
        let now = now_ms();
        sqlx::query(
            "INSERT INTO artifacts (namespace, id, name, media_type, size, content, task_id, run_id, created_ms) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) \
             ON CONFLICT (namespace, id) DO NOTHING",
        )
        .bind(namespace)
        .bind(&id)
        .bind(name)
        .bind(media_type)
        .bind(size)
        .bind(content)
        .bind(task_id)
        .bind(run_id)
        .bind(now)
        .execute(&self.pool)
        .await?;
        self.get_artifact(namespace, &id)
            .await?
            .ok_or_else(|| StoreError::NotFound(format!("artifact {namespace}/{id}")))
    }

    pub async fn get_artifact(&self, namespace: &str, id: &str) -> Result<Option<Artifact>> {
        let row = sqlx::query(
            "SELECT namespace, id, name, media_type, size, content, task_id, run_id, created_ms FROM artifacts WHERE namespace = $1 AND id = $2",
        )
        .bind(namespace)
        .bind(id)
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(Self::artifact_row))
    }

    /// A task's artifacts, newest first.
    pub async fn list_artifacts(&self, namespace: &str, task_id: &str) -> Result<Vec<Artifact>> {
        let rows = sqlx::query(
            "SELECT namespace, id, name, media_type, size, content, task_id, run_id, created_ms FROM artifacts \
             WHERE namespace = $1 AND ($2 = '' OR task_id = $2) ORDER BY created_ms DESC, id DESC",
        )
        .bind(namespace)
        .bind(task_id)
        .fetch_all(&self.pool)
        .await?;
        Ok(rows.into_iter().map(Self::artifact_row).collect())
    }

    /// The conversation key of a conversation's session, giving the session
    /// one ("async:<session id>") if it has none, so results of async
    /// delegations can be sent back to it.
    pub async fn ensure_conversation_key(&self, conversation_id: &str) -> Result<String> {
        let row = sqlx::query("SELECT session_id FROM conversations WHERE id = $1")
            .bind(conversation_id)
            .fetch_one(&self.pool)
            .await?;
        let session: String = row.get(0);
        sqlx::query("UPDATE sessions SET conversation_key = $1 WHERE id = $2 AND conversation_key = ''")
            .bind(format!("async:{session}"))
            .bind(&session)
            .execute(&self.pool)
            .await?;
        let row = sqlx::query("SELECT conversation_key FROM sessions WHERE id = $1")
            .bind(&session)
            .fetch_one(&self.pool)
            .await?;
        Ok(row.get(0))
    }

    /// Close the session's open conversation (if any) and open a new one.
    pub async fn open_conversation(&self, session_id: &str) -> Result<String> {
        let id = new_id();
        let now = now_ms();
        let mut tx = self.pool.begin().await?;
        sqlx::query("UPDATE conversations SET closed_ms = $1 WHERE session_id = $2 AND closed_ms IS NULL")
            .bind(now)
            .bind(session_id)
            .execute(&mut *tx)
            .await?;
        sqlx::query("INSERT INTO conversations (id, session_id, created_ms) VALUES ($1, $2, $3)")
            .bind(&id)
            .bind(session_id)
            .bind(now)
            .execute(&mut *tx)
            .await
            .map_err(conflict("session already has an open conversation"))?;
        tx.commit().await?;
        Ok(id)
    }

    /// The session's open conversation, opening one if none. Safe under
    /// concurrency: a unique index admits one open conversation per session.
    pub async fn current_conversation(&self, session_id: &str) -> Result<String> {
        sqlx::query(
            "INSERT INTO conversations (id, session_id, created_ms) VALUES ($1, $2, $3) \
             ON CONFLICT (session_id) WHERE closed_ms IS NULL DO NOTHING",
        )
        .bind(new_id())
        .bind(session_id)
        .bind(now_ms())
        .execute(&self.pool)
        .await?;
        let row = sqlx::query("SELECT id FROM conversations WHERE session_id = $1 AND closed_ms IS NULL")
            .bind(session_id)
            .fetch_one(&self.pool)
            .await?;
        Ok(row.get(0))
    }

    pub async fn close_conversation(&self, id: &str) -> Result<()> {
        sqlx::query("UPDATE conversations SET closed_ms = $1 WHERE id = $2 AND closed_ms IS NULL")
            .bind(now_ms())
            .bind(id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    // ---- messages ----

    /// Append a message without run ownership checks (setup and tests; the
    /// agent loop uses [`Store::checkpoint`]).
    pub async fn append_message(&self, conversation_id: &str, run_id: &str, msg: &Message) -> Result<i64> {
        let mut tx = self.pool.begin().await?;
        let seq = insert_message(&mut tx, conversation_id, run_id, msg).await?;
        tx.commit().await?;
        Ok(seq)
    }

    pub async fn messages(&self, conversation_id: &str) -> Result<Vec<Message>> {
        self.load_messages(
            "SELECT body FROM messages WHERE conversation_id = $1 ORDER BY seq",
            conversation_id,
        )
        .await
    }

    pub async fn run_messages(&self, run_id: &str) -> Result<Vec<Message>> {
        self.load_messages("SELECT body FROM messages WHERE run_id = $1 ORDER BY seq", run_id)
            .await
    }

    /// A conversation's rolling summary and how many of its own messages
    /// (oldest first) are already folded into it (1.8, part: rollover).
    /// `("", 0)` for a conversation that has never needed one.
    pub async fn conversation_summary(&self, conversation_id: &str) -> Result<(String, i64)> {
        let row = sqlx::query("SELECT summary, summarized_through FROM conversations WHERE id = $1")
            .bind(conversation_id)
            .fetch_optional(&self.pool)
            .await?;
        Ok(match row {
            Some(r) => (r.get::<String, _>(0), r.get::<i64, _>(1)),
            None => (String::new(), 0),
        })
    }

    /// Replaces a conversation's rolling summary and advances
    /// `summarized_through` (1.8, part: rollover) — never retreats, so a
    /// retried or concurrent write can never un-summarize a message.
    pub async fn set_conversation_summary(
        &self,
        conversation_id: &str,
        summary: &str,
        summarized_through: i64,
    ) -> Result<()> {
        sqlx::query(
            "UPDATE conversations SET summary = $1, summarized_through = $2 WHERE id = $3 AND summarized_through < $2",
        )
        .bind(summary)
        .bind(summarized_through)
        .bind(conversation_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn load_messages(&self, sql: &'static str, key: &str) -> Result<Vec<Message>> {
        sqlx::query(sql)
            .bind(key)
            .fetch_all(&self.pool)
            .await?
            .iter()
            .map(|r| serde_json::from_str(&r.get::<String, _>(0)).map_err(|e| StoreError::Invalid(e.to_string())))
            .collect()
    }

    // ---- runs ----

    /// Create a run owned by `owner`, returning its ownership epoch. Fails
    /// with [`StoreError::Conflict`] if the conversation already has an
    /// unfinished run.
    pub async fn create_run(&self, r: &RunRecord, owner: &str) -> Result<i64> {
        sqlx::query(
            "INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, definition_digest, task_id, parent_run_id, root_run_id, status, input, trace_id, started_ms, owner, epoch, requested_by, labels) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 1, $15, $16)",
        )
        .bind(&r.id)
        .bind(&r.session_id)
        .bind(&r.conversation_id)
        .bind(&r.namespace)
        .bind(&r.deployment)
        .bind(&r.definition_digest)
        .bind(&r.task_id)
        .bind(&r.parent_run_id)
        .bind(&r.root_run_id)
        .bind(r.status.as_str())
        .bind(&r.input)
        .bind(&r.trace_id)
        .bind(r.started_ms)
        .bind(owner)
        .bind(&r.requested_by)
        .bind(serde_json::to_string(&r.labels).expect("labels serialize"))
        .execute(&self.pool)
        .await
        .map_err(conflict("conversation already has an unfinished run, or the task already has a run"))?;
        Ok(1)
    }

    /// Take ownership of an unfinished run (e.g. to resume it after a crash).
    /// Any previous owner is fenced off: its writes fail from now on.
    pub async fn claim_run(&self, run_id: &str, owner: &str) -> Result<i64> {
        let row = sqlx::query(
            "UPDATE runs SET owner = $1, epoch = epoch + 1 WHERE id = $2 AND ended_ms IS NULL RETURNING epoch",
        )
        .bind(owner)
        .bind(run_id)
        .fetch_optional(&self.pool)
        .await?
        .ok_or_else(|| StoreError::NotFound(format!("unfinished run {run_id}")))?;
        Ok(row.get(0))
    }

    /// Atomically: verify ownership (`epoch`), optionally record progress, and
    /// optionally append a message, returning its sequence number. The loop's
    /// only write path.
    pub async fn checkpoint(
        &self,
        run_id: &str,
        epoch: i64,
        conversation_id: &str,
        msg: Option<&Message>,
        progress: Option<(i64, RunStatus, Usage)>,
    ) -> Result<Option<i64>> {
        let mut tx = self.pool.begin().await?;
        let updated = match progress {
            Some((step, status, usage)) => sqlx::query("UPDATE runs SET step = $1, status = $2, input_tokens = $3, output_tokens = $4, cost_usd = $5 WHERE id = $6 AND epoch = $7 AND ended_ms IS NULL")
                .bind(step)
                .bind(status.as_str())
                .bind(usage.input_tokens as i64)
                .bind(usage.output_tokens as i64)
                .bind(usage.cost_usd)
                .bind(run_id)
                .bind(epoch)
                .execute(&mut *tx)
                .await?,
            None => sqlx::query("UPDATE runs SET epoch = epoch WHERE id = $1 AND epoch = $2 AND ended_ms IS NULL")
                .bind(run_id)
                .bind(epoch)
                .execute(&mut *tx)
                .await?,
        };
        if updated.rows_affected() != 1 {
            tx.rollback().await?;
            return Err(StoreError::Fenced(run_id.to_string()));
        }
        let seq = match msg {
            Some(m) => Some(insert_message(&mut tx, conversation_id, run_id, m).await?),
            None => None,
        };
        tx.commit().await?;
        Ok(seq)
    }

    #[allow(clippy::too_many_arguments)] // one column per argument
    pub async fn finish_run(
        &self,
        id: &str,
        epoch: i64,
        status: RunStatus,
        output: &str,
        error: &str,
        usage: Usage,
        outcome: Option<&crate::outcome::RunOutcome>,
    ) -> Result<()> {
        let outcome = crate::outcome::RunOutcome::to_json(&outcome.cloned());
        let r = sqlx::query("UPDATE runs SET status = $1, output = $2, error = $3, input_tokens = $4, output_tokens = $5, cost_usd = $6, ended_ms = $7, outcome = $8 WHERE id = $9 AND epoch = $10 AND ended_ms IS NULL")
            .bind(status.as_str())
            .bind(output)
            .bind(error)
            .bind(usage.input_tokens as i64)
            .bind(usage.output_tokens as i64)
            .bind(usage.cost_usd)
            .bind(now_ms())
            .bind(outcome)
            .bind(id)
            .bind(epoch)
            .execute(&self.pool)
            .await?;
        if r.rows_affected() != 1 {
            return Err(StoreError::Fenced(id.to_string()));
        }
        Ok(())
    }

    /// Persists a run's recorded interactions (3.6, part: `agen replay`),
    /// only called when the provider actually recorded one (deployments
    /// with `recordInteractions` on). One row per run; a retried write
    /// (e.g. a resumed run re-recording) replaces the previous one.
    pub async fn save_cassette(&self, run_id: &str, data: &str) -> Result<()> {
        sqlx::query(
            "INSERT INTO cassettes (run_id, data, created_ms) VALUES ($1, $2, $3) \
             ON CONFLICT (run_id) DO UPDATE SET data = excluded.data, created_ms = excluded.created_ms",
        )
        .bind(run_id)
        .bind(data)
        .bind(now_ms())
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// A run's recorded interactions, if any were saved for it.
    pub async fn get_cassette(&self, run_id: &str) -> Result<Option<String>> {
        let row: Option<(String,)> = sqlx::query_as("SELECT data FROM cassettes WHERE run_id = $1")
            .bind(run_id)
            .fetch_optional(&self.pool)
            .await?;
        Ok(row.map(|(data,)| data))
    }

    pub async fn get_run(&self, id: &str) -> Result<RunRecord> {
        let row = sqlx::query(concat!("SELECT ", run_cols!(), " FROM runs WHERE id = $1"))
            .bind(id)
            .fetch_optional(&self.pool)
            .await?
            .ok_or_else(|| StoreError::NotFound(format!("run {id}")))?;
        run_from_row(&row)
    }

    /// Runs that were in progress (e.g. when the process died).
    pub async fn unfinished_runs(&self, namespace: &str, deployment: &str) -> Result<Vec<RunRecord>> {
        sqlx::query(concat!(
            "SELECT ",
            run_cols!(),
            " FROM runs WHERE namespace = $1 AND deployment = $2 AND ended_ms IS NULL ORDER BY started_ms"
        ))
        .bind(namespace)
        .bind(deployment)
        .fetch_all(&self.pool)
        .await?
        .iter()
        .map(run_from_row)
        .collect()
    }

    pub async fn list_runs(&self, namespace: &str, deployment: &str, limit: i64) -> Result<Vec<RunRecord>> {
        sqlx::query(concat!(
            "SELECT ",
            run_cols!(),
            " FROM runs WHERE namespace = $1 AND deployment = $2 ORDER BY started_ms DESC, id DESC LIMIT $3"
        ))
        .bind(namespace)
        .bind(deployment)
        .bind(limit)
        .fetch_all(&self.pool)
        .await?
        .iter()
        .map(run_from_row)
        .collect()
    }

    pub async fn runs_by_root(&self, root_run_id: &str) -> Result<Vec<RunRecord>> {
        sqlx::query(concat!(
            "SELECT ",
            run_cols!(),
            " FROM runs WHERE root_run_id = $1 OR id = $1 ORDER BY started_ms"
        ))
        .bind(root_run_id)
        .fetch_all(&self.pool)
        .await?
        .iter()
        .map(run_from_row)
        .collect()
    }

    // ---- spans ----

    pub async fn insert_span(&self, s: &SpanRecord) -> Result<()> {
        sqlx::query(
            "INSERT INTO spans (span_id, trace_id, parent_span_id, run_id, name, start_ms, end_ms, status, attributes, seq) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) \
             ON CONFLICT (span_id) DO UPDATE SET end_ms = excluded.end_ms, status = excluded.status, attributes = excluded.attributes",
        )
        .bind(&s.span_id)
        .bind(&s.trace_id)
        .bind(&s.parent_span_id)
        .bind(&s.run_id)
        .bind(&s.name)
        .bind(s.start_ms)
        .bind(s.end_ms)
        .bind(&s.status)
        .bind(s.attributes.to_string())
        .bind(s.seq)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// The run's top-level span that a dead owner left `unfinished`, if any.
    pub async fn unfinished_run_span(&self, run_id: &str) -> Result<Option<SpanRecord>> {
        let row = sqlx::query(
            "SELECT span_id, trace_id, parent_span_id, run_id, name, start_ms, end_ms, status, attributes, seq FROM spans \
             WHERE run_id = $1 AND status = 'unfinished' ORDER BY start_ms, seq LIMIT 1",
        )
        .bind(run_id)
        .fetch_optional(&self.pool)
        .await?;
        row.map(|r| {
            Ok(SpanRecord {
                span_id: r.get(0),
                trace_id: r.get(1),
                parent_span_id: r.get(2),
                run_id: r.get(3),
                name: r.get(4),
                start_ms: r.get(5),
                end_ms: r.get(6),
                status: r.get(7),
                attributes: parse_json(&r.get::<String, _>(8))?,
                seq: r.get(9),
            })
        })
        .transpose()
    }

    pub async fn trace(&self, trace_id: &str) -> Result<Vec<SpanRecord>> {
        sqlx::query("SELECT span_id, trace_id, parent_span_id, run_id, name, start_ms, end_ms, status, attributes, seq FROM spans WHERE trace_id = $1 ORDER BY start_ms, seq, span_id")
            .bind(trace_id)
            .fetch_all(&self.pool)
            .await?
            .iter()
            .map(|r| {
                Ok(SpanRecord {
                    span_id: r.get(0),
                    trace_id: r.get(1),
                    parent_span_id: r.get(2),
                    run_id: r.get(3),
                    name: r.get(4),
                    start_ms: r.get(5),
                    end_ms: r.get(6),
                    status: r.get(7),
                    attributes: parse_json(&r.get::<String, _>(8))?,
                    seq: r.get(9),
                })
            })
            .collect()
    }

    // ---- effect ledger ----

    /// Register a side-effecting tool call before executing it.
    pub async fn begin_effect(
        &self,
        run_id: &str,
        epoch: i64,
        call_id: &str,
        tool: &str,
        arguments_hash: &str,
        idempotency_key: &str,
    ) -> Result<EffectBegin> {
        let now = now_ms();
        // Insert only while we still own the run (epoch), so a fenced-off
        // owner can never claim an effect.
        let inserted = sqlx::query(
            "INSERT INTO effects (run_id, call_id, tool, arguments_hash, idempotency_key, state, created_ms, updated_ms) \
             SELECT $1, $2, $3, $4, $5, 'started', $6, $7 WHERE EXISTS (SELECT 1 FROM runs WHERE id = $1 AND epoch = $8) \
             ON CONFLICT (run_id, call_id) DO NOTHING",
        )
        .bind(run_id)
        .bind(call_id)
        .bind(tool)
        .bind(arguments_hash)
        .bind(idempotency_key)
        .bind(now)
        .bind(now)
        .bind(epoch)
        .execute(&self.pool)
        .await?
        .rows_affected();
        if inserted == 1 {
            return Ok(EffectBegin::Proceed);
        }
        self.check_epoch(run_id, epoch).await?;
        let row = sqlx::query("SELECT state, result, arguments_hash FROM effects WHERE run_id = $1 AND call_id = $2")
            .bind(run_id)
            .bind(call_id)
            .fetch_one(&self.pool)
            .await?;
        let (state, result, hash): (String, String, String) = (row.get(0), row.get(1), row.get(2));
        if hash != arguments_hash {
            return Err(StoreError::Invalid(format!(
                "effect {run_id}/{call_id} replayed with different arguments"
            )));
        }
        Ok(match state.as_str() {
            "completed" => EffectBegin::Completed { result },
            _ => EffectBegin::Unknown,
        })
    }

    async fn check_epoch(&self, run_id: &str, epoch: i64) -> Result<()> {
        let current: Option<i64> = sqlx::query("SELECT epoch FROM runs WHERE id = $1")
            .bind(run_id)
            .fetch_optional(&self.pool)
            .await?
            .map(|r| r.get(0));
        if current != Some(epoch) {
            return Err(StoreError::Fenced(run_id.to_string()));
        }
        Ok(())
    }

    pub async fn complete_effect(&self, run_id: &str, epoch: i64, call_id: &str, result: &str) -> Result<()> {
        let r = sqlx::query(
            "UPDATE effects SET state = 'completed', result = $1, updated_ms = $2 WHERE run_id = $3 AND call_id = $4 \
             AND EXISTS (SELECT 1 FROM runs WHERE id = $3 AND epoch = $5)",
        )
        .bind(result)
        .bind(now_ms())
        .bind(run_id)
        .bind(call_id)
        .bind(epoch)
        .execute(&self.pool)
        .await?;
        if r.rows_affected() != 1 {
            self.check_epoch(run_id, epoch).await?;
            return Err(StoreError::NotFound(format!("effect {run_id}/{call_id}")));
        }
        Ok(())
    }

    /// Mark an effect as re-attemptable (the tool declared an idempotency key).
    pub async fn reset_effect(&self, run_id: &str, epoch: i64, call_id: &str) -> Result<()> {
        let r = sqlx::query(
            "UPDATE effects SET state = 'started', updated_ms = $1 WHERE run_id = $2 AND call_id = $3 \
             AND EXISTS (SELECT 1 FROM runs WHERE id = $2 AND epoch = $4)",
        )
        .bind(now_ms())
        .bind(run_id)
        .bind(call_id)
        .bind(epoch)
        .execute(&self.pool)
        .await?;
        if r.rows_affected() != 1 {
            self.check_epoch(run_id, epoch).await?;
            return Err(StoreError::NotFound(format!("effect {run_id}/{call_id}")));
        }
        Ok(())
    }

    /// The run created for a task, if any (tasks map to at most one run).
    /// The run of a task of this deployment (tasks never match another
    /// deployment's run).
    pub async fn run_by_task(&self, namespace: &str, deployment: &str, task_id: &str) -> Result<Option<RunRecord>> {
        sqlx::query(concat!(
            "SELECT ",
            run_cols!(),
            " FROM runs WHERE task_id = $1 AND namespace = $2 AND deployment = $3"
        ))
        .bind(task_id)
        .bind(namespace)
        .bind(deployment)
        .fetch_optional(&self.pool)
        .await?
        .as_ref()
        .map(run_from_row)
        .transpose()
    }

    // ---- delegation counters ----

    /// Atomically increment and return the delegation count under a root run.
    pub async fn incr_delegations(&self, root_run_id: &str) -> Result<i64> {
        let row = sqlx::query(
            "INSERT INTO delegations (root_run_id, count) VALUES ($1, 1) \
             ON CONFLICT (root_run_id) DO UPDATE SET count = delegations.count + 1 RETURNING count",
        )
        .bind(root_run_id)
        .fetch_one(&self.pool)
        .await?;
        Ok(row.get(0))
    }

    /// Records a delegated call once per message id. Returns `None` if this
    /// message id was already recorded (a retry or resume of the same call),
    /// else the number of calls now recorded for the run and for the root run.
    pub async fn record_delegation(
        &self,
        message_id: &str,
        root_run_id: &str,
        run_id: &str,
    ) -> Result<Option<(i64, i64)>> {
        let res = sqlx::query(
            "INSERT INTO delegation_calls (message_id, root_run_id, run_id, created_ms) VALUES ($1, $2, $3, $4) \
             ON CONFLICT (message_id) DO NOTHING",
        )
        .bind(message_id)
        .bind(root_run_id)
        .bind(run_id)
        .bind(now_ms())
        .execute(&self.pool)
        .await?;
        if res.rows_affected() == 0 {
            return Ok(None);
        }
        let per_run: i64 = sqlx::query("SELECT COUNT(*) FROM delegation_calls WHERE run_id = $1")
            .bind(run_id)
            .fetch_one(&self.pool)
            .await?
            .get(0);
        let total: i64 = sqlx::query("SELECT COUNT(*) FROM delegation_calls WHERE root_run_id = $1")
            .bind(root_run_id)
            .fetch_one(&self.pool)
            .await?
            .get(0);
        Ok(Some((per_run, total)))
    }

    /// Forgets a delegated call that was refused (so a retry is checked again).
    pub async fn forget_delegation(&self, message_id: &str) -> Result<()> {
        sqlx::query("DELETE FROM delegation_calls WHERE message_id = $1")
            .bind(message_id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    // ---- logs ----

    pub async fn append_log(
        &self,
        instance_id: &str,
        namespace: &str,
        deployment: &str,
        level: &str,
        message: &str,
    ) -> Result<()> {
        sqlx::query("INSERT INTO logs (instance_id, namespace, deployment, time_ms, level, message) VALUES ($1, $2, $3, $4, $5, $6)")
            .bind(instance_id)
            .bind(namespace)
            .bind(deployment)
            .bind(now_ms())
            .bind(level)
            .bind(message)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    /// Raw access for tests that assert on stored bytes (e.g. redaction).
    pub async fn dump_text(&self, table: &str) -> Result<String> {
        const ALLOWED: [&str; 7] = [
            "sessions",
            "messages",
            "runs",
            "spans",
            "effects",
            "logs",
            "conversations",
        ];
        if !ALLOWED.contains(&table) {
            return Err(StoreError::Invalid(format!("table {table}")));
        }
        // `table` is checked against ALLOWED above.
        let rows = sqlx::query(sqlx::AssertSqlSafe(format!("SELECT * FROM {table}")))
            .fetch_all(&self.pool)
            .await?;
        let mut out = String::new();
        for r in rows {
            for i in 0..r.len() {
                if let Ok(Some(s)) = r.try_get::<Option<String>, _>(i) {
                    out.push_str(&s);
                    out.push('\t');
                }
            }
            out.push('\n');
        }
        Ok(out)
    }

    pub async fn close(&self) {
        self.pool.close().await;
    }
}

fn run_from_row(r: &AnyRow) -> Result<RunRecord> {
    Ok(RunRecord {
        id: r.get(0),
        session_id: r.get(1),
        conversation_id: r.get(2),
        namespace: r.get(3),
        deployment: r.get(4),
        definition_digest: r.get(5),
        task_id: r.get(6),
        parent_run_id: r.get(7),
        root_run_id: r.get(8),
        status: RunStatus::parse(&r.get::<String, _>(9))?,
        input: r.get(10),
        output: r.get(11),
        error: r.get(12),
        step: r.get(13),
        usage: Usage {
            input_tokens: r.get::<i64, _>(14) as u64,
            output_tokens: r.get::<i64, _>(15) as u64,
            // Not persisted (2.21): a resumed run's cached-token discount
            // resets to 0, counting its earlier cache hits at full price
            // instead of their cached weight — conservative (it can only
            // undercount the discount, never overcount a run's spend).
            cached_input_tokens: 0,
            cost_usd: r.get(16),
        },
        trace_id: r.get(17),
        started_ms: r.get(18),
        ended_ms: r.get(19),
        requested_by: r.get(20),
        labels: serde_json::from_str(&r.get::<String, _>(21)).map_err(|e| StoreError::Invalid(e.to_string()))?,
        outcome: crate::outcome::RunOutcome::from_json(&r.get::<String, _>(22)),
    })
}

fn parse_json(s: &str) -> Result<serde_json::Value> {
    serde_json::from_str(s).map_err(|e| StoreError::Invalid(e.to_string()))
}

async fn insert_message(
    tx: &mut sqlx::Transaction<'_, sqlx::Any>,
    conversation_id: &str,
    run_id: &str,
    msg: &Message,
) -> Result<i64> {
    let body = serde_json::to_string(msg).map_err(|e| StoreError::Invalid(e.to_string()))?;
    let row = sqlx::query(
        "INSERT INTO messages (conversation_id, seq, run_id, body, created_ms) \
         VALUES ($1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE conversation_id = $1), $2, $3, $4) RETURNING seq",
    )
    .bind(conversation_id)
    .bind(run_id)
    .bind(body)
    .bind(now_ms())
    .fetch_one(&mut **tx)
    .await?;
    Ok(row.get(0))
}

/// Map a unique-constraint violation to [`StoreError::Conflict`].
fn conflict(msg: &'static str) -> impl Fn(sqlx::Error) -> StoreError {
    move |e| match &e {
        sqlx::Error::Database(d) if d.is_unique_violation() => StoreError::Conflict(msg.to_string()),
        _ => StoreError::Db(e),
    }
}

/// `sqlite://C:/x.db` (Windows drive paths) is not understood by the SQLite
/// driver; rewrite it to `sqlite:C:/x.db`. Other URLs pass through.
fn normalize_sqlite_url(url: &str) -> String {
    if let Some(rest) = url.strip_prefix("sqlite://") {
        let b = rest.as_bytes();
        if b.len() > 2 && b[0].is_ascii_alphabetic() && b[1] == b':' && (b[2] == b'/' || b[2] == b'\\') {
            return format!("sqlite:{rest}");
        }
    }
    url.to_string()
}

/// Split a migration file into statements (no semicolons inside our SQL).
fn split_sql(sql: &'static str) -> impl Iterator<Item = &'static str> {
    sql.split(';').map(str::trim).filter(|s| !s.is_empty())
}

#[cfg(test)]
mod migration_list_tests {
    use super::MIGRATIONS;

    /// Every file in spec/sql/<dialect> must be listed here, in order: the
    /// Go platform applies the directory, so a missing entry leaves the
    /// engine on an older schema.
    #[test]
    fn migrations_list_matches_spec_sql() {
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../spec/sql/sqlite");
        let mut files: Vec<String> = std::fs::read_dir(&dir)
            .unwrap()
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n.ends_with(".sql"))
            .collect();
        files.sort();
        assert_eq!(files.len(), MIGRATIONS.len(), "spec/sql has {files:?}");
        for f in &files {
            let want = std::fs::read_to_string(dir.join(f)).unwrap().replace("\r\n", "\n");
            assert!(
                MIGRATIONS.iter().any(|m| m.sqlite.replace("\r\n", "\n") == want),
                "{f} is not in MIGRATIONS"
            );
        }
    }
}
