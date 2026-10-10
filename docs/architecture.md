# Agen Architecture

Status: describes the current implementation.

## 1. Overview

Agen is one agent engine used two ways:

- **Embedded**: an application links the engine through an SDK (Python, Node,
  Go) and owns the agent's lifecycle. No platform needed.
- **Managed**: the Agen platform deploys, scales, wakes and supervises agents
  across one machine or many. Every managed agent runs the same engine inside
  the `agen-host` process.

```text
             clients: CLI · Web UI · REST · MCP
                          │
                ┌─────────▼─────────┐        ┌────────────┐
                │        Hub        │◄──────►│   Store    │ SQLite (local)
                │  control plane    │        │            │ Postgres (distributed)
                └─────────▲─────────┘        └─────▲──────┘
          assignments ↓   │  ↑ status/heartbeat    │ sessions, runs, spans
                ┌─────────┴─────────┐              │
     machine    │ Nest              │              │
                │  ├ Manager ───────┼── spawns ──► agen-host (Rust engine) ×N
                │  └ Gateway ◄──────┼── A2A ─────► agen-host
                └───────────────────┘
```

## 2. Components and languages

| Component | Language | Role |
|---|---|---|
| `agen-engine` | Rust crate | The agent: loop, providers, tools, MCP client, skills, permissions, secrets, memory, sessions, tracing, storage port. |
| `agen-host` | Rust binary | Runs one agent instance from a bundle. Standalone (`agen-host run ./bundle`) or managed (control + A2A HTTP server). |
| `agen-sdk-core` | Rust crate | Language-neutral SDK facade: JSON agent spec, run events and host tool/approval requests over one dispatcher thread; cancel keys. Every binding goes through it. |
| `agen-ffi` | Rust crate | C ABI over `agen-sdk-core`, used by the Go SDK. |
| SDKs | Rust (PyO3, napi-rs), Go (cgo) | Thin wrappers over `agen-sdk-core`; no agent logic of their own. |
| Hub | Go | Control plane: API, desired state, scheduler, autoscaler, triggers, task queue, registry, fleet view, identity issuance. |
| Nest | Go | One Manager + one Gateway + a backend: native (child processes) or Kubernetes (one pod per instance). Docker is a planned backend. |
| Manager | Go | Sole execution authority inside its Nest. Pulls assignments from the Hub, starts/stops `agen-host` instances, dispatches tasks, reports status. |
| Gateway | Go | Stable A2A address for agents in the Nest; load-balances across instances; wakes sleeping deployments (activator). |
| CLI `agen` | Go | Client of the Hub API. Also runs all-in-one mode (`agen up`). |
| Web UI | TypeScript + React | Client of the Hub API, served by the Hub at `/`. |

Local and distributed use the same binaries. Local all-in-one (`agen up`) runs
the Hub and one Nest in a single `agen` process with SQLite. In a distributed
fleet each Nest is its own `agen nest run` process; Agen does not restart it
itself, so run it under a service manager (systemd, or the StatefulSet in
`deploy/kube`).

## 3. Authority split

- **Hub decides**: stores desired state, schedules, autoscales, fires triggers,
  queues tasks, issues identities. It never executes agents and is never on the
  agent data path.
- **Manager executes**: only a Nest's own Manager starts or stops processes in
  that Nest. Managers pull assignments; the Hub never connects into a Nest.
- **Agents talk directly**: A2A over HTTP, through the target Nest's Gateway.

If the Hub is down, Managers keep their last assignments and A2A keeps working.
Scheduling, scaling, triggers and the fleet view pause until it returns.

## 4. Resource model

| Resource | Meaning |
|---|---|
| **Definition** | An immutable, content-addressed bundle version: `name@sha256:…`. Stored in the Store. |
| **Deployment** | Desired state: definition digest, kind, scale policy, placement, budget, triggers, namespace. |
| **Instance** | One `agen-host` process in one Nest, running one Deployment. |
| **Task** | A durable unit of work queued for a Deployment. |
| **Nest** | An enrolled execution environment with capacity and labels. |

Deployment kinds:

| Kind | Instances | Behaviour |
|---|---|---|
| `singleton` | 0 or 1 | One identity with its own session. Sleep = scale to 0; wake = scale to 1. |
| `pool` | min..max | Interchangeable workers sharing a task queue. |
| `task` | one per task | Instance is created for one task and exits when it finishes (the autoscaler starts up to `max` at once for queued tasks). |

Scale policy: `{min, max, target_queue_per_instance, idle_timeout, max_concurrency}`.

A deployment can be **paused** (`agen stop`, `PauseDeployment`): its instances
drain and stop, the autoscaler and wake-ups leave it at 0, queued tasks and
A2A calls wait (calls get 503), and scaling it is refused until it is resumed
(`agen start`, back to `min`).

## 5. Control flows

**Deploy**: client → `Hub.CreateDeployment` → Hub stores Definition + Deployment →
scheduler writes Assignments (deployment, nest, count) → Manager's
`WatchAssignments` stream delivers them → Manager starts instances → instances
become `ready` → Manager reports via `ReportStatus`.

**Scale**: `Hub.ScaleDeployment` (manual) or autoscaler updates desired count →
scheduler adjusts Assignments → Managers start instances, or drain then stop them.

**Backends**: the Manager logic is the same on every backend; only how an
instance (one agen-host) is started differs.
- *native*: a child process on localhost, tied to the Manager's lifetime (Job
  Object / Pdeathsig).
  - `--host-memory-limit` caps each host's memory (a Windows Job Object
    process limit, Linux `RLIMIT_AS`). A limit that cannot be applied fails
    the start.
  - CPU is not limited, and hosts run as the Nest's OS user with no further
    sandbox, so native Nests are for trusted agents. Use the Kubernetes
    backend (pod CPU/memory limits, seccomp, separate pods) to isolate
    agents from each other.
- *kubernetes* (`agen nest run --backend kubernetes`, the Nest runs in a pod
  with deploy/kube/nest.yaml, in a namespace of its own):
  - Each instance is a pod in the Nest's namespace (image `--kube-image`,
    `restartPolicy: Never`, non-root, seccomp RuntimeDefault, CPU/memory
    limits, no service-account token). The Manager replaces a failed pod as
    it replaces a crashed process.
  - The definition is published as an immutable ConfigMap per digest and
    Nest pod (up to 700 KiB), mounted read-only.
  - The host's settings (Store DSN, ManagerService and host tokens, and the
    variables of `--host-env` / `--host-env-dir`, e.g. provider keys) go in
    a per-instance Secret, never in the pod spec.
  - Pods, Secrets and ConfigMaps are owned by the Nest pod, so Kubernetes
    removes them when it goes. A restarted Nest (StatefulSet, enrolment on
    its volume) deletes any pods left under its Nest id, then starts fresh
    ones. If the API server is unreachable while stopping, the Manager
    gives up waiting after 90 s.
  - The Nest may create pods, so it gets a namespace without other secrets
    (the Hub's admin token lives in the Hub's namespace).
- On every backend, each instance requires a per-instance host token
  (`AGEN_HOST_TOKEN`, generated by its Manager) on its HostService API, so
  nothing but its Manager can drive it. On Kubernetes a NetworkPolicy adds
  network isolation where the CNI enforces it.

**Autoscaler** (runs on the Hub leader, every tick):
want = clamp(ceil(queued_and_running_tasks / target_queue_per_instance), min, max)
(target defaults to 1). Scale up to `want` at once; scale down to `want` only
after no task or A2A activity for `idle_timeout` (default 5 min for pools and
singletons, 0 for `task` deployments, which therefore stop as soon as their
queue is empty). A manual scale counts as activity, so it holds for one idle
window. A2A calls are load the queue does not show: a Gateway whose callers wait
on busy instances asks the Hub (`ReportActivity` with `saturated`) to raise
desired to one more than the live instances, up to `max`. A deployment whose
spend today (UTC) reached `budget.max_usd_per_day` is held at 0: no leases,
no wake-ups or manual scale-ups (callers get 503 with the reset time); runs in
flight finish (within the Manager's drain timeout, 5 min by default; a longer
run is stopped and its task requeued) and queued work waits for the next day.
The check is made when work starts, so spend can overshoot by the runs
already in flight (bounded by `max_usd_per_run` × concurrency); a run's cost
counts on the UTC day it started. A Gateway reports saturation only when this
Nest's ready instances are all full (not while one is starting). `task`
deployments (idle timeout 0) are for queued tasks, not A2A traffic.

**Wake on call**: A2A request hits a Gateway for a deployment with 0 ready
instances → Gateway holds the request and calls `Hub.RequestWake` → desired ≥ 1 →
instance starts → Gateway forwards the held request. Timeout → 503 with retry hint.

**Tasks**: `Hub.SubmitTask` stores a task (`queued`). Managers lease tasks for
their deployments (`SELECT … FOR UPDATE SKIP LOCKED` on Postgres; a single
writer on SQLite), dispatch to an instance, and report the result. Leases expire,
so tasks held by a dead Nest go back to `queued`. Every lease gets a new
`lease_id` fencing token; `ExtendLease` and `CompleteTask` must present the
current one, so a partitioned Nest cannot complete a task that was re-leased.
A task may carry a **conversation key** (scoped by its source: `api:`,
`webhook:<trigger>:`; A2A uses `a2a:<caller>:<contextId>`) and **labels**. Tasks
of one deployment with the same key share a session and so continue one
conversation (a singleton keeps its one conversation and ignores keys); a task is leased only when no older task with its key is queued
or in flight, so they run one at a time, in submission order. Labels are
copied to the task's runs, tool calls and child tasks. When the Hub fails a
task (lease expired too often) or cancels it, it also ends the task's
unfinished run and bumps its epoch, so the conversation is not left blocked
and a host still running it is fenced off. The Hub logs instance starts,
failures and exits (from Nest reports) and task releases and lease expiries
on the deployment's log.

**Hub leader**: several Hub replicas can serve the API; exactly one is leader
and runs the scheduler, autoscaler and triggers. Leadership is a Store row
`(name='hub-leader', holder, epoch, expires_at)` renewed every few seconds.
Taking over an expired lease increments `epoch`; assignment and trigger writes
happen in a transaction that checks the writer's epoch, so a stale leader's
writes are rejected.

**Triggers**: owned by the Hub leader. Cron and webhooks create Tasks, so a
trigger that fires while no Nest is available still waits durably in the queue.
Webhooks arrive at `POST /hooks/<ns>/<deployment>/<trigger>` on the Hub with a
per-trigger secret. Missed cron windows (Hub down) are recorded as `missed`
trigger events (`ListTriggerEvents`), never dropped silently.
Cron schedules are standard 5-field expressions or descriptors (`@hourly`,
`@every 30s`), evaluated in UTC unless prefixed `CRON_TZ=<zone>`. A window
fires if the leader evaluates it within a minute of its due time; older
windows are `missed`, except that a trigger with `catchUp` fires the latest
missed window once (`redelivered`). Firing is exactly-once: the task's
idempotency key is `cron:<trigger>:<due>` and events are unique per trigger
and due time. A webhook caller presents the trigger's secret
(`CreateWebhookSecret`, `agen webhook-secret`; only a hash is stored) as a
bearer token, or, with `auth.type: hmac`, signs the raw body with a platform
secret the deployment is granted (header, prefix, sha256/sha1, hex/base64
configurable). The body (1 MiB max) follows the trigger's `input` as the task
input; the idempotency key (the `Idempotency-Key` header, or the header or
JSON body field the trigger names) makes redeliveries return the same task;
an optional conversation key and the trigger's labels go on the task.
Rejected calls are recorded as `rejected` events; configuration errors are
logged on the deployment, not returned to the caller.

**Nest failure**: heartbeat missing for `nest_lost_after` → Nest marked `lost`,
its Assignments are rescheduled, its task leases expire. Singleton sessions
resume from the Store.

**Placement** (scheduler, every tick on the leader): a deployment's instances
go to `active` Nests whose labels match `placement.labels`. Existing instances
stay where they are; surplus is removed from the most loaded Nest first;
missing instances go to the Nest with the most free capacity (capacity 0 = no
limit). Instances that fit nowhere stay unplaced until capacity appears.

**Manager loop**: keep the latest assignment set (and keep running it while the
Hub is unreachable); start `agen-host serve` per missing instance with the
Store URL and instance id in its environment; unpack each Definition once per
digest; drain then stop surplus instances and instances on an old digest;
replace instances that exit or stay unhealthy (crash loops back off); lease
tasks only up to free slots, mark them running, extend leases while they run
(a lost lease cancels the run on the host) and complete them with the lease
id. A task the host could not take is released back to the queue at once
(`ReleaseTask`, attempt not counted). If a host dies mid-task the task is
released (attempt counted) and re-leased, and its run resumes on another
instance; a task whose lease expires 5 times (`Scheduler.MaxTaskAttempts`)
fails instead of looping. Instances are tied to the Manager's lifetime (Job
Object on Windows, parent-death signal on Linux); a host that ignores SIGTERM
is killed after a grace period. Downloaded definitions are checked against
their digest.

**Partition**: a Manager that cannot reach the Hub for `nest_lost_after` stops
its singleton instances and does not restart them until the Hub is back,
because the Hub reschedules them elsewhere after the same time. Pools keep
serving from their last assignments.

**Delegation lineage** of child tasks is derived by the Hub, never taken from
the caller: a child task needs a parent task the calling Nest currently
leases; its depth is the parent's depth + 1 and its root run is the parent's.
Delegation stays within the parent's namespace. On the synchronous A2A path,
the lineage metadata (`agen.depth`, `agen.parent_run_id`, `agen.root_run_id`)
is used only from authenticated agent callers (a Hub-signed call token), and
the host checks it against the Store: the parent run must be a run of the
calling deployment. Anonymous callers, allowed only by a loopback Gateway
(`agen up`), start a new tree at depth 0.

**Nest scope**: a Nest token may only lease, complete, release and raise
approvals for deployments assigned to it, fetch definitions assigned to it,
and wake deployments it is eligible to run (placement labels). Approval
`requested_by` is set by the Hub.

## 6. Addressing and A2A

- An agent's address is its deployment: `<namespace>/<deployment>`.
- `Hub.Resolve(ns/deployment)` returns the Gateway URLs of Nests assigned to it.
- Gateway routes: `/a2a/<ns>/<deployment>` (JSON-RPC: `message/send`,
  `message/stream`, `tasks/get`) and
  `/a2a/<ns>/<deployment>/.well-known/agent-card.json`.
- A2A request metadata carries `traceparent`, `agen.depth`, `agen.root_run_id`,
  `agen.parent_run_id`, optional `agen.labels`, and the request carries the
  caller's call token (§10). Tasks carry the same lineage fields.
- A message's `contextId` (generated when absent) is its conversation: messages
  of one context from one caller continue one conversation, and a Gateway runs
  them one at a time. Two Gateways may still race; the later message then
  fails with "conversation already has an unfinished run".

**Delegation** (sub-agents): an agent delegates only to another Deployment,
either synchronously over A2A (`message/send`/`message/stream`) or
asynchronously as a child Task (`NestService.SubmitChildTask`, lineage derived
by the Hub; not yet exposed to agents as a tool). There are no
hidden in-process sub-agents; skills cover in-agent specialisation. Limits
(`Limits` on the Deployment): `max_delegation_depth` is checked by the receiving
Gateway/Hub from `agen.depth`; `max_fan_out` is checked by the calling engine
per run; `max_total_delegations` is an atomic counter per `root_run_id` in the
Store, checked by the calling engine before each delegation.

A bundle lists the deployments it may call in `x-agen/config.json`
`delegates` (`name`, optional `namespace`, `url`, `description`); the engine
exposes them through one `call_agent` tool. Without a fixed `url` the target
is resolved through the Nest Manager's localhost `ManagerService.Resolve`
(each instance gets `AGEN_MANAGER_URL` and its own `AGEN_MANAGER_TOKEN`), which
asks the Hub (`NestService.Resolve`) and caches the answer, so delegation
keeps working from the cache while the Hub is down. The A2A message id is
`<run>.<step>.<call>` and Gateways run a repeated message id as the same task,
so `call_agent` is safe to repeat on retry or resume. The callee's run
continues the caller's trace (the traceparent names the caller's `agen.tool`
span) and records `parent_run_id` / `root_run_id`.

## 7. Engine model

Agent → Session → Conversation → Run → Span.

- IDs are ULIDs; `gen_ai.conversation.id` is used for conversations.
- Everything is persisted through the storage port (SQLite and Postgres
  adapters). Spans live in the Store, not in local files; JSONL and OTLP are
  export options.
- **Write path**: `agen-host` writes sessions, runs, spans, effects and logs
  directly to the Store; nothing goes through the Hub, so agents keep working
  while the Hub is down. The Manager passes the connection string to each
  instance as a secret environment variable (local: the SQLite path;
  distributed: a per-Nest Postgres role limited to run-data tables). The Hub
  reads the same tables for the fleet view, logs and daily budgets. If the
  Store is unreachable, new runs fail fast and in-flight runs stop at their next
  checkpoint write.
- **Run ownership**: every run has an `owner` and an `epoch`. Every
 checkpoint (message + progress, one transaction) and the final status write
 check the epoch; `resume` claims the run and increments it, so a previous
 owner that is still alive is fenced off at its next write. A conversation
 has at most one unfinished run; a second concurrent run is refused.
- **Tasks are idempotent**: a task maps to at most one run (unique
  `task_id`). Running a task again returns its result if finished, or claims
  and resumes its run if not. This is how a re-leased task continues on a new
  host after its Nest died. Only the Manager (by re-sending tasks) resumes
  runs; hosts never resume other instances' work on their own.
- **Effect ledger ownership**: ledger writes are epoch-checked like
  checkpoints, so a fenced-off owner cannot claim or complete an effect.
  Timeouts, cancellations and lost connections leave the effect `started`
  and the model is told `effect_unknown`.
- **Model context**: history from earlier runs in the conversation is trimmed
 oldest-first to fit `context_tokens` (always starting at a user turn); the
 current run is always sent in full. Tool calls without a recorded result
 (e.g. cancelled) get a synthetic error result so the history stays valid.
- **Budgets**: token budgets always apply; USD budgets only when the provider
 reports cost (OpenRouter does, direct OpenAI does not).
- **MCP server isolation**: stdio servers get a cleared environment
  (a small allowlist such as PATH/HOME/TEMP) plus only their declared `env`,
  never the host's credentials. They run in a Job Object (Windows) or process
  group with parent-death signal (Linux) so they die with the host. A dead
  server makes the host report `ready: false` in Health so the Manager
  replaces it.
- **Host callbacks**: a running agent reaches the platform only through its
  Manager's localhost `ManagerService` (approvals, platform secrets, resolve
  with call tokens). Hosts never call the Hub.
- **Platform secrets** are bundle secrets with `source: platform`:
  - They are stored per namespace in the Store (`agen secret set NAME`, value
    on stdin), sealed with `AGEN_HUB_KEK` when set. The API never returns
    values, and setting them is an admin action.
  - The Hub gives a value to a Nest only for a deployment assigned to it
    whose current definition declares that lookup key with source
    `platform`.
  - The Manager keeps values in memory only, so an instance restarted while
    the Hub is down still starts. The engine registers them with the
    Redactor like every secret.
- Checkpoint after every step. A tool marked `side_effect: true` (the default for
  MCP tools unless the server marks them read-only) is written to the
  **effect ledger** before it runs and completed after. On resume, a started but
  unconfirmed effect is not re-run; the run fails the step with
  `effect_unknown` unless the tool declares an idempotency key. MCP tools are
  idempotent (keyed by their arguments) when the server marks them
  `idempotentHint` or `config.json` `tools.<server>.<tool>.idempotent` says so.
- The tool calls of one model turn run concurrently (`harness.parallelToolCalls`
  turns it off), unless one of them needs an approval; results are recorded in
  call order.
- Every MCP call carries `_meta` keys `agen/namespace`, `agen/deployment`,
  `agen/taskId`, `agen/runId`, `agen/rootRunId`, `agen/conversationId`,
  `agen/conversationKey`, `agen/labels` and the `traceparent`.
- A run's top-level span is written as `unfinished` when the run starts. On
  resume after a crash it is closed as `interrupted` and the resumed run's
  span hangs under it, carrying the task attempt.
- Each model call's `max_tokens` is the configured `maxOutputTokens` lowered
  to the remaining `maxTokensPerRun` (without a configured maximum, only once
  fewer than 8192 tokens remain).
- In managed mode the Manager passes the definition digest it unpacked
  (`AGEN_DEFINITION_DIGEST`); hosts do not re-hash the directory, which tool
  servers may write to. Health reports the loaded tools and each MCP server's
  state; the Manager forwards them to the Hub's instance view.
- Permissions: rules evaluated deny → ask → allow, first match wins; default ask.
  A rule matches a tool by its registered name (`server.tool`) or its wire
  name (`server_tool`).
  "Ask" creates a durable Approval (embedded mode: callback to the host app).
  Approval arguments are redacted before they are persisted or shown. With
  `permissions.notify`, the Hub POSTs each new pending approval to that URL,
  signed with a granted platform secret if one is named.
- Secrets: resolved by name (env or the platform secret store; the OS keychain
  is reserved). Values
  are registered with a redactor that scrubs logs, spans, stored messages and
  tool results before they are persisted or sent to a model.
- Budgets per run (tokens, USD) enforced by the engine; per-day budgets enforced
  by the Hub from reported usage.

## 7a. SDK contract

- Host tool calls, approvals and run events are delivered on one dispatcher
  thread per agent, in order. Callbacks must return quickly (hand real work
  to another thread); a slow callback delays every run on that agent.
- Calling `run()` from inside a callback is rejected with `reentrant`.
- `close()` rejects new runs, cancels running ones and fails pending host
  requests, so in-flight runs return promptly (status `cancelled`).
- Every SDK passes the shared conformance suite in `spec/conformance`.

## 8. Providers

The `ModelProvider` port has adapters: `openrouter`, `openai` (direct), `fake`
(scripted, deterministic) and `replay` (cassettes recorded from a real provider).
The provider is chosen in `harness.json` only.

## 9. Bundle format

Agent Plugins 1.0 layout plus an `x-agen/` extension directory:

```text
bundle/
  plugin.json            # {"$schema", "name", ...}
  skills/<name>/SKILL.md
  mcp.json               # {"mcpServers": {...}}
  x-agen/
    agent.md             # frontmatter + system prompt body
    harness.json         # provider, model, parameters
    config.json          # permissions, triggers, scale, budget, workspace
    secrets.json         # secret names + source, never values
```

Every file has a JSON Schema in `spec/bundle/`. Unknown fields in standard files
are preserved; unknown fields in `x-agen/` files are errors.

**Definition digest**: files are keyed by `/`-separated bundle-relative path;
ignored paths (`.git/`, `node_modules/`, `__pycache__/`, `.venv/`, `.DS_Store`,
`*~`, `*.swp`, `*.pyc`) are skipped; the rest are hashed in UTF-8 byte order of
path as `path || 0x00 || u64le(len) || bytes` with SHA-256, written
`sha256:<hex>`. Rust (`agen-engine`) and Go (`platform/internal/definition`)
share a golden test vector.

## 10. Identity and access

- Distributed: `agen hub serve --tls` runs HTTPS with a CA kept in the Store
  (shared by Hub replicas). `agen join-token` prints a single-use token and
  the CA hash; `agen nest run --hub https://… --join-token T --ca-hash H`
  pins that CA, generates its key locally, sends a CSR and receives a Nest
  certificate. NestService then requires that client certificate (its
  fingerprint is the Nest's identity); bearer tokens alone, certificates of
  other CAs and certificates of Nests that never enrolled are refused. The
  certificate is signed in the same transaction that uses up the join token,
  and the Nest checks that the CA it is handed matches the pinned hash. API
  clients use bearer tokens over TLS (`--ca-hash` pins the CA; the server
  certificate must also match the dialed host). `agen nests revoke ID`
  revokes a Nest's certificate and token, ends its assignment stream and
  reschedules its instances; a revoked Nest never comes back.
- Nest certificates are short-lived (30 days, `agen hub serve
  --nest-cert-lifetime`). The Manager renews its certificate over the current
  mTLS connection with a fresh key when less than `--cert-renew-before` (10
  days) remains, and saves it. The previous certificate keeps working until the
  new one is first used (so a lost reply can be retried); revocation ends both.
  Hub server certificates are re-issued at each start.
- `agen hub serve` refuses a non-loopback `--listen` without `--tls`
  (`AGEN_INSECURE=1` overrides it, for a TLS-terminating proxy in front).
- Nests and hosts need only the run-data tables. `agen store host-role`
  creates a Postgres role limited to them (`store.HostTables`, plus reading
  `schema_migrations`), and that role's DSN is what Nests get.
  - It cannot read the Hub's CA or call-token keys, API or join tokens,
    tasks or fleet state, and cannot write any of them.
  - The engine opens the Store without DDL when the schema is current.
  - A migration that adds a table hosts use must also extend `HostTables`,
    and the command must be re-run.
  - With `--namespace` (repeatable), row-level security limits the role to
    those namespaces' rows, for reads and writes. Child tables follow their
    session or run. A Nest serving several namespaces maps each to its role
    with `agen nest run --store-for NS=URL`.
  - The Hub, as table owner, is not subject to these policies.
  - Row-level security is enabled on the host tables the first time the
    command runs. A host role made by an older version has no policy then
    and sees nothing until the command is re-run for it.
- *Known limits:*
  - Hosts write their runs, so fields the Hub reads (`runs.owner`, lineage)
    are only as honest as the hosts of that namespace. `requested_by` holds
    the Hub-signed user call token, which a host cannot forge. A host of
    that namespace can still read it, and reuse it against that one
    deployment until it expires (10 min), or clear it. Approvals are only as
    strong as the hosts of their namespace.
  - Row-level security isolates namespaces only when their hosts are
    separated too: separate pods (Kubernetes backend), or separate OS users.
    Native hosts run as the Nest's user and can read its data dir and
    environment. `--store-for-only` refuses namespaces without a mapping, so
    none falls back to the default Store URL.
  - Files holding credentials (`nest.json`, `resolved.json`) are written
    0600. That has no effect on Windows; protect the Nest's data dir with
    ACLs there.
  - The Hub's keys (`hub_ca`, `hub_token_key`) and platform secrets are
    sealed with AES-256-GCM under `AGEN_HUB_KEK` when it is set.
    - Use at least 32 random bytes, the same value on every Hub, never given
      to Nests.
    - Keys stored before a KEK was set are sealed in place, but older copies
      remain in database backups or WAL: rotate them after enabling it.
    - Without a KEK (local `agen up`) they stay plaintext, and a TLS Hub warns.
    - `agen migrate` copies sealed secrets as they are: the target Hubs need
      the same KEK.
    - `agen hub rotate-kek` re-seals everything from `AGEN_HUB_KEK` to
      `AGEN_HUB_KEK_NEW` in one transaction. Hubs started with
      `AGEN_HUB_KEK_PREVIOUS` keep opening values sealed with the old key
      during the switch (docs/security.md has the procedure).
  - `agen hub rotate-token-key` adds a call-token key.
    - Hubs publish it at once and sign with it after a 5-minute grace (Hubs
      re-read keys every minute, and so do Managers).
    - The previous key keeps verifying live tokens until the next rotation,
      which retires it.
    - Retired keys are kept, only to verify stored requester records. A
      requester record that does not verify refuses the ask (fail closed).
    - Rotation is refused while the current key is younger than an agent
      token's lifetime plus twice the grace (`--force` overrides).
  - CA rollover is not automated: a new CA means re-enrolling Nests.
  - Platform secrets are granted to named deployments (`agen secret set
    --for`). Declaring a name in a bundle does not grant it.
  - Local SQLite stores are a single file: `agen up` has no roles.
- Local (`agen up`, plain HTTP on localhost): enrolment returns a Nest bearer
  token (scope `nest`, bound to that Nest's id) instead. Either way, a Nest may
  only act for itself and its assigned deployments; join tokens are
  single-use; user API tokens can never hold the `nest` scope.
- A2A callers present a **call token**: Hub-signed (Ed25519, key in the
  Store), naming the caller (`user:<token id>` or `agent:<ns>/<deployment>`)
  and the one deployment it may call, short-lived. `Resolve` returns one:
  - to operators, via HubService (10 min);
  - to a Manager resolving for one of its instances, via NestService (1 h).
    The Hub checks that the caller deployment runs on that Nest, and the
    Manager caches the token so delegation survives a Hub outage until it
    expires.
- Gateways verify tokens offline with the Hub's public keys. Nests fetch them
  with `GetTokenKeys` and keep them on disk, so a restart during a Hub outage
  still works.
- Nest Gateways (`agen nest run`) refuse calls without a valid token
  (`--gateway-auth required`, the default). Only a loopback Gateway
  (`agen up`) may accept anonymous callers.
- Lineage metadata (parent/root run, depth) is accepted only with agent
  tokens.
  - The callee's host checks it against the Store: the parent run must be a
    run of the calling deployment, and root and depth come from the parent
    chain, not from the claim. This works while the Hub is down.
  - So neither an outside caller nor another agent can attach a run to
    someone else's trace or reset its depth.
- A2A tasks belong to the caller that created them: `tasks/get`,
  `tasks/cancel` and a repeated message id from another caller are refused.
  Waking a sleeping deployment through its agent card also needs a token.
- Managers refresh cached call tokens while the Hub is up and keep them on
  disk (0600). During an outage a token is used until it expires (agents:
  up to 1 h after the last refresh), then calls fail with a clear error.
- A run started over A2A with a user token records that user as its
  requester (`runs.requested_by`). Its approvals then name that user, who
  cannot approve them. Delegated runs inherit their root run's requester.
- An ask must come from the instance that owns the run (its Manager names
  the instance; the Hub checks `runs.owner`). There is at most one pending
  approval per run, tool and arguments, enforced by a unique index, so
  concurrent asks share it.
- A Gateway gives an instance at most `RunTimeout` (1 h) to answer an A2A
  call; after that the call fails and the instance is told to cancel.
- Clients (CLI, UI, MCP) use API tokens with scopes `viewer`, `operator`,
  `approver`, `admin`, optionally limited to namespaces. An approval can't be
  granted by the principal that caused it.
- Local mode: a token is generated on first `agen up` and everything
  listens on localhost only (plain HTTP). `agen up` keeps its admin token and Hub URL in
  `AGEN_HOME/local.json` (0600; the user profile ACL on Windows) and its
  Nest credential in `AGEN_HOME/nest/nest.json`; `agen nest run` defaults to
  `AGEN_HOME/nests/<name>`. The CLI only falls back to the local token for
  the local Hub URL. A saved Nest credential the Hub rejects is replaced by a
  fresh enrolment when a join token is at hand. `agen down` stops a running
  `agen up` through an admin-only local endpoint.

## 11. API surface

One source: protobuf services in `spec/proto/agen/v1/`, served with Connect by the
Hub, so the same handlers answer gRPC, Connect and plain JSON over HTTP
(`POST /agen.v1.HubService/<Method>`). The MCP server at `/mcp` exposes the same
RPCs as tools, generated from the proto descriptors, so REST, MCP and CLI
cannot drift. The CLI and UI use the same API.

MCP transport is stateless streamable HTTP: JSON-RPC over `POST /mcp`
(`initialize`, `ping`, `tools/list`, `tools/call`), answered with
`application/json`. A tool call is executed by the same Connect handler as
REST with the caller's `Authorization: Bearer` token, so scopes, namespaces
and results are identical; API errors come back as tool results with
`isError` and the API's error body. Tool names are the snake_case RPC names;
arguments and results use protojson (bytes as base64; bundles may also be sent
as text in `bundleText`). The server's `instructions` carry a primer on
bundles, tools, permissions and work; `GetBundleGuide` returns the bundle
schemas and an example; `WhoAmI` returns the caller's scopes;
`CreateDeployment`/`UpdateDeployment` with `validateOnly` check a bundle and
return warnings without deploying. `GetTask` waits up to 300 s.

## 12. Storage

One schema, two engines (SQLite, Postgres), versioned migrations. Tables:
definitions, deployments, assignments, nests, instances, tasks, triggers,
trigger_log, approvals, tokens, sessions, conversations, runs, spans, effects,
usage. `agen migrate --to postgres://…` copies a local install.

## 13. Repository layout

```text
spec/        bundle JSON Schemas, protobuf, generated-code config
engine/      Rust: agen-engine, agen-host, agen-sdk-core, agen-ffi, agen-testkit
sdks/        python/, node/, go/
platform/    Go: cmd/agen, gen/ (generated API code), e2e/ (distributed and
             Kubernetes scenarios), internal/{hub,manager,gateway,store,cli,
             mcpserver,kube,pki,calltoken,...}
web/         React UI and its Playwright e2e
deploy/      docker (image), compose (cluster), kube (manifests)
scripts/     code generation, release, install
examples/    bundles and sample apps
docs/        this and user docs
```
