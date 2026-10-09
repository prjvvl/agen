//! Storage port: sessions, conversations, messages, runs, spans, the effect
//! ledger, delegation counters and logs. One implementation over sqlx `Any`
//! serves SQLite (local) and Postgres (distributed); both run the same SQL
//! (numbered `$N` placeholders, used in order) and the migrations in
//! `spec/sql/<dialect>/`.

use serde::{Deserialize, Serialize};
use sqlx::any::{AnyPoolOptions, AnyRow};
use sqlx::{AnyPool, Row};

use crate::provider::{Message, Usage};

macro_rules! run_cols {
    () => {
        "id, session_id, conversation_id, namespace, deployment, definition_digest, task_id, parent_run_id, root_run_id, status, input, output, error, step, input_tokens, output_tokens, cost_usd, trace_id, started_ms, ended_ms, requested_by"
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
];

// Postgres advisory lock id guarding migrations: 1634166126 = 0x6167656e ("agen").
// Must match the Go migrator.

pub fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

pub fn new_id() -> String {
    ulid::Ulid::generate().to_string()
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Session {
    pub id: String,
    pub agent: String,
    pub namespace: String,
    pub deployment: String,
    pub memory: serde_json::Value,
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

    pub async fn set_memory(&self, session_id: &str, memory: &serde_json::Value) -> Result<()> {
        sqlx::query("UPDATE sessions SET memory = $1, updated_ms = $2 WHERE id = $3")
            .bind(memory.to_string())
            .bind(now_ms())
            .bind(session_id)
            .execute(&self.pool)
            .await?;
        Ok(())
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
            "INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, definition_digest, task_id, parent_run_id, root_run_id, status, input, trace_id, started_ms, owner, epoch, requested_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 1, $15)",
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
    /// optionally append a message. The loop's only write path.
    pub async fn checkpoint(
        &self,
        run_id: &str,
        epoch: i64,
        conversation_id: &str,
        msg: Option<&Message>,
        progress: Option<(i64, RunStatus, Usage)>,
    ) -> Result<()> {
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
        if let Some(m) = msg {
            insert_message(&mut tx, conversation_id, run_id, m).await?;
        }
        tx.commit().await?;
        Ok(())
    }

    pub async fn finish_run(
        &self,
        id: &str,
        epoch: i64,
        status: RunStatus,
        output: &str,
        error: &str,
        usage: Usage,
    ) -> Result<()> {
        let r = sqlx::query("UPDATE runs SET status = $1, output = $2, error = $3, input_tokens = $4, output_tokens = $5, cost_usd = $6, ended_ms = $7 WHERE id = $8 AND epoch = $9 AND ended_ms IS NULL")
            .bind(status.as_str())
            .bind(output)
            .bind(error)
            .bind(usage.input_tokens as i64)
            .bind(usage.output_tokens as i64)
            .bind(usage.cost_usd)
            .bind(now_ms())
            .bind(id)
            .bind(epoch)
            .execute(&self.pool)
            .await?;
        if r.rows_affected() != 1 {
            return Err(StoreError::Fenced(id.to_string()));
        }
        Ok(())
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
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)",
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
            cost_usd: r.get(16),
        },
        trace_id: r.get(17),
        started_ms: r.get(18),
        ended_ms: r.get(19),
        requested_by: r.get(20),
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
