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
Capacity pressure: when a deployment wants more instances than are placed and
a Nest it may run on is full, deployments holding instances on that Nest that
have had no work for 1 min (`PressureIdle`) scale down without waiting out
their idle timeout, so the waiting work gets the room. A deployment whose
provider account is held (§8) does not scale up for queued tasks until the
hold's retry time.

**Wake on call**: A2A request hits a Gateway for a deployment with 0 ready
instances → Gateway holds the request and calls `Hub.RequestWake` → desired ≥ 1 →
instance starts → Gateway forwards the held request. A call waits up to
`WakeTimeout` (10 min) for a free instance, whether the deployment is asleep,
starting, busy or waiting for Nest capacity; then it gets 503 with a retry
hint. While a call runs, the Gateway reports activity every 15 s, so a call
that outlasts the idle timeout does not see its deployment scaled to zero (and
its assignment, which its instance needs to delegate, removed) under it.

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
or in flight, so they run one at a time, in submission order. A key read back
from a task (already carrying its source prefix) is accepted as is. Labels are
copied to the task's runs, tool calls and child tasks. A task may carry
`not_before` (at most 90 days ahead): until then it is not leased, does not
count as load for the autoscaler and does not hold up its conversation, though
it counts towards `maxQueuedTasks`. It may carry `not_after`: the leader's
scheduler tick fails a still-queued task past it (`ExpireOverdueTasks`,
reason `expired`) instead of leaving it queued indefinitely. It may carry
`schedule_key`: `SubmitTask` cancels, in the same transaction as the insert,
the queued task of that deployment that already has the key, so at most one
is ever queued per key. It may carry `priority` (`urgent`, `high`, `normal`,
`background`; default `normal`): `LeaseTasks` orders a deployment's own
queue by it before submission order, never across deployments and after a
conversation's own ordering. It may carry `locks` (3.11, part): names it
must hold all of at once before `LeaseTasks` leases it (so no deadlock
from taking them one at a time), granted for as long as its lease and
refreshed when the lease is extended, released when it ends
(`task_locks` is what a task declared, `locks` what is currently
granted, one task per namespace and name); a task waiting on one reports
`wait_reason: "locks"`. The built-in `acquire_lock`/`release_lock` tools
for locks named mid-run are not yet implemented. A task may be in state
`waiting` (3.3): it holds no instance or lease and is not autoscaler load,
and carries `wait_for` (`"<kind>:<ref>"`, e.g. `"child:<task id>"`)
describing what it is waiting for — unlike `wait_reason`, which says why a
still-queued task has not started, `wait_for` is set only while suspended
and cleared on resume. `run_inbox` holds what has happened for a waiting
task while it waits; writing a row whose kind and ref match a waiting
task's `wait_for` requeues it at `urgent` priority without counting an
attempt (`Store.WriteInbox`), and a child task finishing always writes its
outcome into its parent's inbox. A tool call suspends a run by returning a
`ToolError` carrying `suspend` instead of a result: the engine does not
write a tool message for it (it stays pending, exactly as if the process
had died there) and `RunTask` returns early with `wait_for` set instead of
an outcome. The Manager then calls the Hub's `SuspendTask` (giving up the
lease, `Store.SuspendTask`) instead of `CompleteTask`, freeing the instance
for other work. Woken, the same task id is leased and dispatched again; the
engine resumes from its last checkpoint, finds the same call still pending
and retries it from scratch (not from a cached result) — which is why
`call_agent`'s unified path (below) checks the child task's own status
again on resume, rather than trusting the `run_inbox` payload directly.
It may carry `model_tier`, the cost tier
for a routing model (§8). When the deployment has spent 80% of its daily
budget, leased tasks carry `budget_pressure`, and a routing model then uses
the bundle's `router.budgetTier`. When the Hub fails a
task (lease expired too often) or cancels it, it also ends the task's
unfinished run and bumps its epoch, so the conversation is not left blocked
and a host still running it is fenced off. Deleting a deployment cancels its
unfinished tasks and ends every unfinished run (task or A2A) the same way.
The Hub logs instance starts,
failures and exits (from Nest reports) and task releases and lease expiries
on the deployment's log.

**Task overrides**: a task may carry `overrides` (`TaskOverrides`: `tools`,
`skills`, `systemAddendum`, `permissions` (deny/ask only), `budget`), which
can only narrow what its bundle allows, never widen it. `SubmitTask` and
`SubmitChildTask` both validate it (an `allow` permission rule, or an
unparsable `budget.maxDuration`, is refused) and store it on the task as
JSON. A child task's effective overrides are its parent's combined with its
own (`store.CombineOverrides`): `tools` and `skills` allowlists intersect,
`permissions` concatenate, `systemAddendum` appends, and `budget` fields
take the smaller of each side — so restrictions accumulate down a tree and
a descendant can never regain what an ancestor gave up. The engine applies
them per run (never by mutating the shared `AgentConfig`, since one agent
process can run several tasks concurrently): a tool outside the `tools`
allowlist is hidden from the model outright (stronger than a plain `deny`),
`permissions` rules are merged in ahead of the bundle's own, `skills` adds
to the bundle's own `preloadSkills`, `systemAddendum` is appended last to
the system prompt, and `budget` narrows `maxTurns`, `maxRunDuration`,
`maxTokensPerRun` and `maxUsdPerRun` (a bundle's own, lower limit is never
widened by a looser override). `start_agent` can set overrides only on a
delegate whose `x-agen/config.json` `delegates[].overrides` is `true`; the
calling engine enforces this before the request reaches the Hub.

**Tree budgets** (3.5, part): a root task's own `overrides.budget` is
also the whole tree's budget — not a separate field. When a child is
submitted (`SubmitChildTask`, or a header-linked task via
`linkToCaller`), `Hub.narrowForTreeBudget` finds the root (via
`RootTaskID`), reads its `overrides.budget`, and — if it set
`maxTokens`/`maxUsd` — computes the tree's remainder (that limit minus
`Store.TreeUsage`'s sum of every run of every task of the tree so far,
joined through each run's `task_id`) and narrows the child's own
combined budget down to the smaller of its own request and that
remainder, through the exact same `budget` field and narrowing semantics
described above. No new engine code, proto field, or table: the existing
per-run `budget_exceeded` check (unchanged) is what actually enforces
whatever this narrows a child's budget down to, the same as it always
has for a plain per-run override. A remainder already used up floors at
a small positive value (1 token / $0.0001) rather than 0, since 0 reads
as "unset, no limit" to the existing narrowing logic; the floor still
lets a child's own first turn run (no usage yet is not over a 1-token
limit), so it is cut off on its very next check instead — the same
"overshoot of at most one model call" the deployment's own daily budget
already documents and accepts.

This is deliberately the "napkin math" slice, computed once at each
child's own submission, not a continuously re-checked live budget: a
sibling spending heavily *while* another is already running does not
retroactively shrink the running one's own already-fixed budget, only
the next child submitted after it. A live per-turn check (mirroring
`is_held`/`unfinished_children`'s `TaskStarter` round trip) would close
that gap, but needs its own new RPC chain
(`TaskStarter::tree_budget_exceeded` → `ManagerService` →
`NestService` → `Store`) comparable in size to join or hold, and is not
built. `budget.threshold` events, `GetMetrics` rolled up per tree/label,
and showing budget status to the model mid-run are separate, additive
pieces of the original design, not part of enforcement itself, and are
not built either.

**Label budgets** (3.5, part), scoped down from the plan's own
`labelBudgets` namespace-config wording to a per-deployment rule set,
since there is no namespace-level budget config surface today (budgets
are otherwise deployment-scoped) and building one is a separate, larger
piece of product surface than this needed: `Budget.label_usd_per_day`
(`"key=value" → maxUsdPerDay`, validated the same way a real task label
is — `checkBudget`, `store.CheckLabels`). Unlike `max_usd_per_day`
(which holds the *whole* deployment at zero instances once exhausted —
see §5's autoscaler), an exhausted label only excludes the candidates
that carry it:
`Hub.exhaustedLabels` computes which configured rules have hit their
`Store.LabelSpentToday` sum (bounded-fetch-then-filter-in-Go over today's
runs, the same shape `ListEvents`'s own label filter already uses, since
labels are JSON and this stays cross-dialect), and `NestAPI.LeaseTasks`
releases a leased candidate carrying one right back
(`Store.ReleaseTask(notStarted=true)`) rather than excluding it before
the lease — deliberately avoiding any change to `Store.LeaseTasks`'s own
SQL or signature (32+ existing call sites). A task waiting on an
exhausted label reports `wait_reason: "label_budget"` (cached per
deployment per response, in the same `deploymentWait` struct the
deployment-wide reason already uses) — distinct from `"budget"`, since
only *its own* work is held back, not the deployment's.

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
missed window once (`redelivered`), unless it is older than `catchUpWindow`.
`jitter` spreads firing within a duration after the window is due
(deterministic per window, so re-evaluating it is idempotent). `overlap`
(`skip`, `buffer_one`, default `allow`) and `minStartSpacing` refuse to fire
a window while an earlier firing's task is unfinished, or too soon after the
previous firing; such a window is recorded `skipped`, not `missed`. Firing is exactly-once: the task's
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
logged on the deployment, not returned to the caller. Event triggers read the
namespace's event log from a cursor per trigger (`trigger_cursors`, created
at the newest event); each firing's idempotency key is `event:<trigger>:<seq>`,
a trigger skips the events of its own tasks, `on.count` holds matching seqs
until the count is reached within `on.within`, and a task records how many
event triggers led to it (`event_chain`): at 5 the next firing is rejected. A
held provider account keeps the cursor where it is.

**Event coalescing** (M4, scenario S13): a trigger's `coalesce` (a Go
duration, e.g. `"30s"`) merges a matching event that arrives while it
already has a task queued in the target conversation into that task,
instead of firing a separate one — the exact same
`Store.MergeIntoQueuedConversationTask` mechanism "collect" queue mode
uses, called from `fireEventTask` before it falls back to a plain
`SubmitTask`. Unset (the default): every matching event still fires its
own task, same as before this field existed.

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
leases, or, for a run started over A2A (which has no task), an unfinished run
owned by the calling instance on that Nest; its depth is the parent's depth +
1 (for an A2A run, the number of runs above it) and its root run is the
parent's.
Delegation stays within the parent's namespace. On the synchronous A2A path,
the lineage metadata (`agen.depth`, `agen.parent_run_id`, `agen.root_run_id`)
is used only from authenticated agent callers (a Hub-signed call token), and
the host checks it against the Store: the parent run must be a run of the
calling deployment. Anonymous callers, allowed only by a loopback Gateway
(`agen up`), start a new tree at depth 0.

**Nest scope**: a Nest token may only lease, complete, release and raise
approvals for deployments assigned to it, fetch definitions assigned to it,
and wake deployments it is eligible to run (placement labels). In its status
reports it may add instance rows only for deployments it is eligible to run;
instances it already reported keep reporting while they drain. Approval
`requested_by` is set by the Hub.

## 6. Addressing and A2A

- An agent's address is its deployment: `<namespace>/<deployment>`.
- `Hub.Resolve(ns/deployment)` returns the Gateway URLs of Nests assigned to it.
- Gateway routes: `/a2a/<ns>/<deployment>` (JSON-RPC: `message/send`,
  `message/stream`, `tasks/get`) and
  `/a2a/<ns>/<deployment>/.well-known/agent-card.json`. The agent card
  advertises A2A **1.0** (`gateway.A2AVersion`); the JSON-RPC methods and
  the Task/Message/Part shapes this Gateway serves are unchanged from 0.3,
  so an older client keeps working without a separate wire path.
- A2A request metadata carries `traceparent`, `agen.depth`, `agen.root_run_id`,
  `agen.parent_run_id`, optional `agen.labels` and `agen.model_tier`, and the
  request carries the caller's call token (§10). Tasks carry the same lineage
  fields.
- A message's `contextId` (generated when absent) is its conversation: messages
  of one context from one caller continue one conversation, and a Gateway runs
  them one at a time. Two Gateways may still race; the later message then
  fails with "conversation already has an unfinished run".

**Delegation** (sub-agents): an agent delegates only to another Deployment,
either unified through a child Task that `call_agent` itself waits on
(3.2b, the default for a platform delegate) or synchronously over A2A
(`message/send`/`message/stream`, `call_agent`'s own older path, kept for
embedded/standalone agents, a delegate with a fixed `url`, or one with
`via: "a2a"`), or asynchronously as a fire-and-forget child Task that the
caller never waits on (`NestService.SubmitChildTask`, lineage derived by
the Hub; the `start_agent` tool). There are no hidden in-process sub-agents;
skills cover in-agent specialisation. Limits (`Limits` on the Deployment):
`max_delegation_depth` is checked by the receiving Gateway/Hub from
`agen.depth` (A2A) or by the Hub at submission (a child task); `max_fan_out`
is checked by the calling engine per run; `max_total_delegations` is an
atomic counter per `root_run_id` in the Store, checked by the calling engine
before each delegation (once per call, by its `<run>.<step>.<call>` key, so
a retry, resume or suspend/resume cycle is never counted twice).

A bundle lists the deployments it may call in `x-agen/config.json`
`delegates` (`name`, optional `namespace`, `url`, `description`, `via`); the
engine exposes them through the `call_agent` tool (and `start_agent`,
below).

**Unified delegation** (`call_agent`, 3.2b, the default for a platform
delegate without a fixed `url` and without `via: "a2a"`): the same
`ManagerService.StartTask` path as `start_agent` (below), but with no
`reply_conversation_key` — the child's result comes back as this call's
own result, not as a new message — and `call_agent` then waits on the
child inline, up to `limits.inlineWait` (default 20s, `ManagerService.WaitTask`
proxying the Hub's `NestService.WaitTask`, which polls like `GetTask`,
capped at 60s per call). A child that finishes within the wait answers
normally: its outcome's status and reason prefix the text (`[partial:
max_turns] <report>`; nothing for a plain `done`), followed by its
structured `data` if any — the same formatting the A2A path below has
always used. A child that does not finish within the wait makes the whole
run suspend (3.3) instead of blocking the instance for it. `call_agent`'s idempotency
key for the child start is `call:<run>.<step>.<call>`, so re-checking it
on resume (after a suspend, or a retry) always finds the same child rather
than starting another one.

**Synchronous A2A** (`call_agent`'s exception path: `url` set, embedded or
standalone use, or `via: "a2a"`): without a fixed `url` the target is
resolved through the Nest Manager's localhost `ManagerService.Resolve`
(each instance gets `AGEN_MANAGER_URL` and its own `AGEN_MANAGER_TOKEN`), which
asks the Hub (`NestService.Resolve`) and caches the answer, so delegation
keeps working from the cache while the Hub is down — the reason this path
still exists once 3.2b's unified path, which needs the Hub throughout, is
the default. `Resolve` accepts a caller deployment that is assigned to the
asking Nest or still has a live instance there (starting, ready, busy or
draining), so a run on an instance draining after a scale-down or redeploy
can keep delegating. The A2A message id is `<run>.<step>.<call>` and
Gateways run a repeated message id as the same task, so this path is safe
to repeat on retry or resume too. It waits up to 75 min for an answer (a
Gateway lets a call wait 10 min for an instance and run for 1 h), blocking
the instance for the whole wait — no suspension. The callee's run continues
the caller's trace (the traceparent names the caller's `agen.tool` span,
which records the target as `agen.delegate.target`) and records
`parent_run_id` / `root_run_id`.

**Async delegation** (`start_agent`, managed mode, delegates without a fixed
`url`): the engine asks its Manager (`ManagerService.StartTask`), which
submits a child task through `NestService.SubmitChildTask` (retrying through a
short Hub outage) and returns its id; the tool returns at once and the
caller's run finishes its turn. The child task carries `reply_deployment`
(the caller) and `reply_conversation_key`: the caller's conversation key, or,
when its session has none, `async:<session>`, which the engine sets on the
session. When the child ends (succeeded, failed or cancelled), the Hub submits
its result once (idempotency key `reply:<child>`) as a task of the caller's
deployment with that key and source `reply:<child deployment>`, so the caller
continues the same conversation. The reply task keeps the child's root run
and labels but no parent run or trace context: it starts a trace of its own.
The idempotency key of the start is `start:<run>.<step>.<call>`, so a retried
or resumed call returns the same task; `start_agent` counts against
`max_fan_out` and `max_total_delegations` like `call_agent`.

**Parent-close policy** (3.4, part): `Task.on_parent_end` ("cancel", the
default and also what empty means, or "abandon") decides what happens to a
task's own unfinished children when it ends (Temporal's term for the same
idea). `taskFinished` (the Store hook every task-ending path already runs
through, for events, locks and the parent's run_inbox) applies it: on
"cancel" it finds the task's direct *joined* children (`reply_deployment`
unset) still in `queued`, `leased`, `running` or `waiting`, cancels each one
(ending its run the same way `CancelTask` does), and recurses into
`taskFinished` for it — so a whole branch unwinds together, regardless of
how deep it delegated. A child with `reply_deployment` set (every
`start_agent` child today) is async by design — the whole point is to
outlive the task that started it — so it is never cascaded, on either
policy. "abandon"
skips this and leaves them running with no parent left to report back to
(their own `reply_deployment`/inbox target, if any, still applies). It is
set on `SubmitTask`/`SubmitChildTask`/`agen run --on-parent-end`; neither
`call_agent` nor `start_agent` expose it yet (their children keep the
default).

**Tree queries** (3.4, part): `Task.root_task_id` names the tree a task
belongs to — a task with no parent is its own root; `SubmitTask` defaults
it to the task's own id when a caller leaves it unset, and `linkToCaller`
and `SubmitChildTask` both carry the parent's own `root_task_id` down
rather than deriving it fresh, so it stays correct no matter how deep a
chain delegates. Unlike `root_run_id` (empty until a run attaches to the
task), it is always set, which is what makes `GetTaskTree(task_id)` able to
resolve a whole tree from any task in it, including one still queued:
it looks up that task, then returns every task sharing its `root_task_id`
(`agen tree <task-id>`).

**Tree-wide cancel** (3.4, part): `CancelTask(id, tree=true)` cancels every
unfinished task of `id`'s whole tree, resolved through `root_task_id` the
same way `GetTaskTree` does — one flat query, not a parent-by-parent walk.
Unlike the parent-close policy's automatic cascade (which deliberately
skips async children), this touches every task of the tree, including an
async one: cancelling a whole tree on purpose is a different thing from a
task quietly ending. It still runs through the same `taskFinished`, so a
task the parent-close policy already cascaded onto while this walk was
running (processed moments earlier in the same transaction) is picked up
and reported too, not double-cancelled. Only hard cancel exists today
(stopped at once, same as the single-task path) — a graceful mode (give
each run a last report turn, the Hub finishing stragglers after a grace
period) is not built yet, nor is `tree=true` wired into `agen cancel`'s A2A
counterpart.

**Progress and query** (3.4, part: 2.33's query half and 2.34): the
built-in `report_progress(step, done?, total?, note?, artifacts?)` tool —
registered unconditionally in `AgentBuilder::build()`, like `finish` and
`save_artifact` — calls out to the Manager (`ManagerService.ReportProgress`
→ `NestService.ReportProgress`, mirroring `WaitTask`'s engine-initiated
round trip) when one is configured (`AgentBuilder::task_starter`; outside
managed mode the tool no-ops with a friendly result instead of erroring).
`NestAPI.ReportProgress` is deliberately low-stakes: unlike
`CompleteTask`/`SuspendTask` there is no lease fencing (the engine has no
lease id to send), just `requireNest` and a no-op on an already-terminal
task, since the engine has no way to know that before calling. It stores
the latest value on `Task.progress` and appends a `task.progress` event,
rate-limited to one per 10-second bucket per task (`AppendEventOnce` with
a `progress:<task>:<bucket>` dedupe key) — the event log is the history,
the column only ever holds the latest. `QueryTask(id)` reads it back
alongside a simplified cut of `GetActivity`'s own per-run activity
(`model`/`tool`/`idle`/`approval`, from the task's open span and pending
approval; not `agent` for one waiting on a delegate, and none of
`GetActivity`'s fleet-wide warnings) and the outcome, without `GetTask`'s
`wait_seconds` — a lighter read for a caller or a person checking in on
long work.

**Signals and queries** (3.4, part: 2.33): `signal_agent(task, message)`
and `query_agent(task)` are the same idea as `start_agent`/`call_agent`
but without going through delegation bookkeeping — a run checking in on,
or sending a message to, a task it (directly or transitively) delegated
to. Both are built-in, registered unconditionally like `report_progress`,
and both reach the Hub through a *descendant-scoped* round trip:
`ManagerService.{Query,Signal}ChildTask` → `NestService.{Query,Signal}ChildTask`,
each carrying the calling task's own id (`ToolContext::task_id`) alongside
the target's. `NestAPI.isDescendant` walks the target's `parent_task_id`
chain up to the caller (bounded, generous next to any realistic
`max_delegation_depth`) and refuses `PermissionDenied` if it never reaches
it — "only your own descendants," not any task you happen to know the id
of. `QueryChildTask` is `QueryTask`'s own logic (factored into
`Hub.queryTaskProto`, shared by both RPCs) with that check in front.
`SignalChildTask` writes a `run_inbox` row (`kind: "signal"`, `ref`: the
caller's task id) and emits `task.signalled`, waking the target if it
happens to be waiting on exactly this signal.

**Draining the inbox into a running run** (3.4/3.9, part): the gap 3.3
originally left — "turning a `run_inbox` row back into a tool result or a
message is engine work, not implemented yet" — is now closed for rows
other than `"child"`. Before each model call, `Agent::drive_inner` calls
`AgentBuilder::task_starter`'s `drain_inbox(task_id)` (managed mode only;
a no-op without a starter, or without a `task_id` — standalone/embedded
runs have neither), which reaches `ManagerService.DrainInbox` →
`NestService.DrainInbox` → `Store.DrainInbox`: claims (sets
`consumed_ms`, atomically with the read) every unconsumed row of the
task other than `"child"` — those stay the `call_agent` resume path's own
business, never read generically — oldest first. A `"signal"` row becomes
a new user message, `"[Signal from task <ref>] <payload>"`, folded into
the very next request to the model; any other kind is claimed (so it
does not pile up) but not yet turned into anything, ready for `"cancel"`
(graceful tree cancel) or `"steer"` (queue mode) rows once those exist.
Like `ReportProgress`, `NestAPI.DrainInbox` has no lease fencing
(`requireNest` only) — low stakes, since claiming is idempotent and the
worst case is a stale instance's drain racing a real one for the same
row. A failed drain (the Hub unreachable, say) is logged and the turn
goes on without it — this must never be the reason a run stalls, the
same invariant the direct-Store write path protects for runs/spans.

**Join** (3.4, part): `start_agent` without `detach: true` (the default,
decided by the user 2026-10-23, §9 item 2 of the implementation plan — a
deliberate behaviour change from every earlier version) ties the child to
the caller's own task instead of replying into the conversation: no
`reply_conversation_key` is sent, so `Task.reply_deployment` stays unset,
the same signal the parent-close policy already uses to mean "joined."
Two checks share one `TaskStarter::unfinished_children(task_id)` →
`ManagerService.UnfinishedChildren` → `NestService.UnfinishedChildren` →
`Store.CountUnfinishedChildren` round trip (counts only direct, unfinished,
joined children — async ones and grandchildren never count): the built-in
`finish` tool refuses a `done` call while it is positive (`"N child
task(s) you started are still running; wait for them (query_agent) or
cancel them, then call finish again"`, a normal tool error, not a run
failure), and `Agent::drive_inner` checks it unconditionally at the same
point it drains the inbox — before every model call, not only after
`finish` is attempted — suspending with `wait_for: "children"` instead of
asking the model again. Unlike `call_agent`'s inline-wait suspend, this
one does not wait for the child to be slow at all: a run that starts a
joined child and has nothing else pending suspends immediately. Waking
reuses 3.3's own `run_inbox`/`taskFinished` wake-on-match, extended
(`writeInboxRow`'s matching SQL) to also satisfy a `"children"` wait on
*any* `"child"` kind row, not only an exact `"child:<id>"` — so whichever
child finishes first wakes it, the model decides whether to keep working
or try `finish` again. Neither check ever blocks or suspends on a failed
RPC (the Hub unreachable, say) — only a confirmed positive count does,
the same fail-open posture as every other platform-only check. Found by
the real end-to-end test that was written specifically because this
change was considered the riskiest of the session so far, not caught by
any mock: `UnfinishedChildrenResponse.count` is an `int64`, which protojson
renders as a JSON *string* (to avoid precision loss in JS) — the Rust
client's first version read it with `Value::as_i64()`, which returns
`None` for a JSON string, silently defaulting to 0 and never suspending.
A positive count was never once detected outside of unit tests using a
mock that skipped real JSON entirely. Fixed by accepting either a JSON
number or a numeric string.

**Hold** (3.4, part): `PauseTask(id, tree=false, until?, reason?)` /
`ResumeTask(id, tree=false)` reuse join's own suspend mechanism rather than
adding a new one. `Task` gains `held`, `held_until` and `hold_reason`;
`LeaseTasks` skips a held, still-`queued` task lazily (its own `WHERE`
clause, `held = 0 OR held_until_ms <= now`, needs no active sweep since
the leasing query itself already looks at every queued task each tick).
A `leased`/`running` task is not touched at hold time — the engine notices
at its own next turn boundary, the same place it checks
`unfinished_children`: `TaskStarter::is_held(task_id)` →
`ManagerService.IsHeld` → `NestService.IsHeld` → `Store.IsHeld` (effective
held: `held && (held_until_ms == 0 || held_until_ms > now)`, so a passed
deadline reads as not-held without anything having to clear the stored
flag first) and, if held, `drive_inner` returns `Outcome::Suspended("resume")`
instead of calling the model again. `ResumeTask` is the only thing that
wakes a task waiting specifically on `wait_for: "resume"` — unlike join's
wait, nothing else would, since a hold is not an event anything else
writes to `run_inbox` for. A `waiting` task's own deadline (if any) is
woken by a new scheduler-tick sweep, `Store.WakeExpiredHolds`, mirroring
`ExpireOverdueTasks`'s reason for existing: a waiting task holds no lease,
so nothing else would notice its deadline passed. `PauseTree`/`ResumeTree`
(like `CancelTree`) touch every task of the whole tree by `root_task_id`,
including async ones — an explicit hold is not the automatic
`on_parent_end` cascade. `is_held` returns a bare bool rather than the
deadline itself: a deadline is the Hub's job to act on (lazily or via the
sweep), not the engine's, and a bool sidesteps the int64/protojson string
pitfall `unfinished_children` hit entirely. The deployment-wide
`PauseDeployment(hold_runs=true)` extension from the plan's wording is not
built — scoped down to the per-task case, which is what `agen pause`/
`agen resume` and the `PauseTask`/`ResumeTask` RPCs expose today.

**Graceful cancel** (3.4, part): `CancelTask(id, tree=false, mode="hard"|"graceful", grace_period_seconds?)`.
`mode: "graceful"` gives every task of the scope (just `id`, or the whole
tree with `tree=true`) that is currently `leased` or `running` a last
turn before `CancelTask`/`CancelTree`'s existing hard stop reaches it:
`Hub.gracefulCancel` writes each one a `"cancel"`-kind `run_inbox` entry
(`Store.WriteInbox`, the same table/mechanism `signal_agent` and drain
already use) and then blocks — polling, the same `h.Poll`-interval
pattern `GetTask`'s `wait_seconds` uses — until either all of them reach
a terminal state or `grace_period_seconds` passes (default 20s, capped at
5 minutes), before falling through to the unchanged hard
`CancelTask`/`CancelTree` call, which only has an effect on whatever is
still unfinished. A `queued` or `waiting` task has no live turn to give a
last chance to, so it is always stopped at once regardless of mode — only
`leased`/`running` tasks get the cancel entry. On the engine side,
`drive_inner`'s existing inbox drain (built for `signal_agent`) now also
turns a `"cancel"`-kind entry into a user message asking the model to
call `finish` with whatever it has, phrased so the model understands it
will be stopped shortly either way; this needed no new engine mechanism,
only a second case in the same `kind` match the drain already had.
Exposed via `agen cancel --mode graceful --grace <duration>` and
`CancelTask.mode`/`.grace_period_seconds` (feature `tasks.cancel-graceful`).
No new Task columns, no new sweep: the grace period is enforced entirely
by `CancelTask`'s own call blocking, so a caller's response already
carries the final outcome (`SUCCEEDED`/`FAILED`/etc. if the run wrapped up
in time, `CANCELLED` otherwise) with no further polling needed on the
caller's side either.

**`steer`/`interrupt` queue modes** (3.9, part): `conversations.queueMode`'s
remaining two values, alongside `followup` (default) and `collect`.
`steer` delivers a new submission into the conversation's currently
`leased`/`running` task as a `"steer"`-kind `run_inbox` entry
(`Store.RunningConversationTask` finds it; conversation ordering
guarantees at most one) instead of queuing it separately — the
drive_inner's inbox drain turns it into a user message
(`"[New input for this conversation] <input>"`) before the run's next
model call. This is deliberately shaped like a Temporal Signal rather
than an Update: fire-and-forget, no independent result of its own (the
`SubmitTaskResponse` is the running task itself, the same way `collect`
returns the task it merged into rather than a new one); a caller after a
result should query that task, not treat the steer submission as having
one. `steer`'s own idempotency-key gap stands (a repeated steer delivers
its payload again); `collect`'s is now closed, see below. `interrupt` reuses graceful cancel's own mechanism
(`Hub.gracefulCancelReason`) on the conversation's running task — a new
`Hub.InterruptGrace` field (default `defaultGracePeriod`, since
`SubmitTask` has no field of its own for a longer one, unlike
`CancelTask`'s request-level override) — then queues the new submission
as its own, independent task once the old one ends, one way or the
other. This mirrors Temporal's own cancellation model: cooperative (the
run decides how to respond, same "cancel" inbox entry/wrap-up-and-finish
path graceful cancel uses) and a run that finishes on its own within the
grace period ends however it finished, not cancelled — only a survivor
past the deadline is hard-stopped. Neither mode needed a new Task column,
proto field, or RPC: both are entirely `SubmitTask`-side branches on the
existing `conversations.queueMode` string and `Store.WriteInbox`/
`Hub.gracefulCancel`, which already existed for signal_agent and graceful
cancel respectively. With nothing running in the conversation right now
(it's idle, queued, or suspended waiting on something else), both modes
fall through and queue normally, same as `followup`.

**`collect`'s idempotency-key gap, closed**: a merge now checks a new
`task_merge_keys` table (namespace, deployment, idempotency_key → task_id)
before merging, in `Store.MergeIntoQueuedConversationTask` — a submission
whose key was already used, either as some task's own original
`idempotency_key` or recorded here by an earlier merge, returns that same
task instead of merging a second time; a successful merge then records
its own key there, if it has one. Event-trigger coalescing (above) shares
this same merge call, and the same fix, for free.

**`SubmitTask.delivery`** (3.9, part): overrides `conversations.queueMode`
for one submission, when the deployment allows a mode that reaches it
(any value, including `""`/`followup`, which always falls through to a
plain queued submission regardless of the deployment's own policy) —
`Hub.SubmitTask` picks `req.Msg.Delivery` over `PolicyOf(d).Conversations`
when it is set, then runs the exact same `collect`/`steer`/`interrupt`
branches as before. Lets a `followup` deployment still have one urgent
submission interrupt or steer the running conversation without changing
the deployment's default for everyone else. `agen run --delivery MODE`.

The rest of the "Tree" design is otherwise done; 3.9's queue modes are
now all four built. These eight (parent-close, `GetTaskTree`, tree-wide
cancel, progress/query, signals/queries, join, hold, graceful cancel) are
3.4's pieces so far.

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
- **Cassettes and `agen replay`** (3.6, part): a deployment whose
  `x-agen/config.json` sets `recordInteractions` has its real provider
  wrapped in `RecordingProvider` (`from_harness`, which already builds the
  "fake"/"replay"/"openrouter"/"openai" providers from the same file —
  this was the only new wiring needed, since `ReplayProvider` itself
  already existed, built for hand-authored test cassettes via
  `harness.json`'s own `{"provider": "replay", "cassette": "..."}`, just
  never driven by a production run's own recording before). Once a run
  ends, `agent.rs` saves the wrapped provider's recorded interactions
  (full prompts and tool-call payloads — a `ModelProvider::recorded_cassette()`
  trait method, `None` for every other provider) straight to the Store, the
  same direct write path as everything else here; off by default. `agen
  replay <run>` (`GetCassette` + `GetDefinition`) reassembles the run's own
  bundle with its `harness.json` rewritten to point at the saved cassette,
  then drives it with `agen-host run`'s existing standalone path —
  deterministic, offline, no Hub task. Cassettes age out with the same
  retention sweep as spans and logs (`PruneObservability`), not a separate
  setting. Resuming from a specific turn with a live model from there on
  (`--turn`/`--model`) needs a provider that can switch mid-replay from the
  cassette to a live call, which does not exist yet.
- **`agen eval`** (§6 learning and testing, part): `agen eval <bundle-dir>
  --scenarios DIR --runs N` runs every scenario file N times and reports
  pass@1/pass^k. A scenario names an `input`, optional `mockedTools`
  (tool name → canned response), optional `rules` (`mustContain` /
  `mustNotContain` substring checks) and an optional `rubric` (`criteria`
  plus `passThreshold`, scored 0–1 by one extra call to the bundle's own
  model provider — a judge prompt, not a new provider). Mocking connects
  to the bundle's real `mcp.json` servers once, to learn their tool specs,
  then never calls them again: a mocked tool's call returns the scenario's
  canned value directly, and any tool the scenario does not mock errors
  instead of reaching a live server, so repeated eval runs stay
  deterministic and side-effect free. All of this is local: `agen-host
  eval` (new subcommand) does the run+score work per scenario file, `agen
  eval` loops it the same way `agen replay` drives `agen-host` directly,
  no Hub task or deployment involved. Comparing a report against the
  deployed definition's own last recorded eval, and `deploy
  --require-eval`, both need somewhere to persist eval history per
  deployment (a new Hub RPC and storage, as cassettes needed for `agen
  replay`) — not attempted here.
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
 oldest-first to fit `context_tokens` (harness `contextTokens`, default
 64000, lowered to half the model's window when OpenRouter reports a smaller
 one; always starting at a user turn). When the current run's own messages
 exceed it, their oldest tool results (longer than 600 characters, except the
 latest four and `load_skill` results) are cut to their first 300 characters
 plus a note naming the tool and turn, in blocks of
 eight, until the run fits in about two thirds of the budget; working in
 blocks makes the request prefix (and so the provider's prompt cache) change
 rarely. The Store keeps every message in full. Tool calls without a recorded result
 (e.g. cancelled) get a synthetic error result so the history stays valid.
- **System prompt**: `agent.md`'s body, then the full instructions of each
 skill in `preloadSkills`, then a list of the other skills (name and
 description) to load with `load_skill`.
- **Model turn**: the configured model, retried on rate limits and server
 errors, then each of `fallbackModels` in order. An empty answer (no text, no
 tool calls) is asked again once before moving on, and an answer cut off at
 the output limit with nothing usable is asked again once with twice the
 limit; if every model gives none, the run fails with `no_answer`. Account errors (HTTP 401/402/403) end the turn
 at once (§8). A model-call span records the served model
 (`gen_ai.response.model`), the model sent and `agen.fallback` when a fallback
 served it, and for routing models the tier (`agen.router.tier`) and the
 router's reported decision (`agen.router.*`).
- **Budgets**: token budgets always apply; USD budgets only when the provider
 reports cost (OpenRouter does, direct OpenAI does not).
- **Guards**: tools a static `deny` rule matches are left out of the model's
 tool list; the same call failing with the same error 3 times in a row is
 refused; `limits.stuckRepeats` identical turns (calls and results) get one
 nudge, then the report call with reason `stuck`. Direct OpenAI requests send
 `max_completion_tokens` instead of `max_tokens`.
- **Outcome**: every finished run stores an outcome (`runs.outcome`, JSON:
 status, reason, summary, data, artifacts, inputFrom) and the Manager copies
 it to the task (`CompleteTask.outcome`, `tasks.outcome`). The engine's
 built-in `finish` tool sets it (checked against harness `result.schema`; it
 bypasses permissions); its accepted call is recognised from the stored turn,
 so a resumed run ends the same way. A plain answer is `done`. At
 `maxTurns`, `maxRunDuration` or a run budget the engine adds a user note and
 makes one call with `tool_choice: none` and `limits.reportTokens` output, and
 the run ends `partial` with that report (`reportTokens: 0`: it fails as
 before). Work ended by the Hub (cancel, lease expiry) or by older hosts gets
 the outcome its state implies in API responses. The Gateway maps
 `needs_input`/`rejected` to A2A `input-required`/`rejected` and adds
 `agen.outcome` metadata and a data part.
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
- A failed tool call's span carries `error.message` (redacted, at most 1000
  characters), so a trace explains the failure without the transcript.
- A run's top-level span is written as `unfinished` when the run starts. On
  resume after a crash it is closed as `interrupted` and the resumed run's
  span hangs under it, carrying the task attempt.
- Each model call's `max_tokens` is the configured `maxOutputTokens` lowered
  to the remaining `maxTokensPerRun` (bundles default to 8192; an SDK agent
  without a maximum sends one only once fewer than 8192 tokens remain).
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
- Trust (3.7): every task has one (`operator` > `trusted` > `untrusted`), set
  by its source (an API token's `trust`, default `trusted`; a webhook
  trigger's, default `untrusted`; cron, always `trusted`) and narrowed, never
  raised, by a child (`start_agent` sends the caller's current trust). A tool
  marked `untrustedOutput` (bundle config, or an MCP server's `openWorldHint`)
  drops the run's trust to `untrusted` for the rest of the run the first time
  it is called, logged as `agen.trust.lowered` on the tool span. A permission
  rule's `when.trust` / `when.trustBelow` conditions it on the run's current
  trust, evaluated alongside the plain deny → ask → allow match (the engine
  tracks the run's trust in memory; it is not persisted, so a resumed run
  restarts from the task's original trust). The fuller conditional rule
  language (`args`, `labels`, `time`, `rate`) is not yet implemented.
- Feedback: a judgement on how a task went (score 0.0-1.0, labels, a note, who
  gave it), recorded by `RecordFeedback` and read back by `ListFeedback`.
  `DecideApproval` records one too (`by: "approval"`, score 1.0/0.0), so
  approvals and explicit ratings land in the same place.
- Memory blocks (3.10, part): durable, namespace-scoped named text
  (`memory_blocks`: namespace, id, label, description, value, limit_chars, a
  version bumped on every update), attached in `x-agen/config.json` `memory`
  (`block`, `mode`: `read` / `append` / `write`) and rendered into the system
  prompt. With a non-`read` attachment the engine registers a
  `memory_update` tool (`replace` / `append` / `str_replace`, an optional
  `expectedVersion` compare-and-set) so parallel conversations cannot
  silently overwrite each other; `SetMemoryBlock` (people, via
  `agen memory set`) always replaces the value outright. Live `mcp:`/`url:`
  context providers and conversation rollover (continue-as-new with an
  auto-summary) are not yet implemented.
- Artifacts (3.8, part): named, content-addressed text (`artifacts`:
  namespace, id = `sha256:<hex>` of content, name, media_type, size, content,
  task_id, run_id) an agent saves with the built-in `save_artifact` tool
  and reads back with `read_artifact` (paged by `offset`/`length`), or
  people read with `GetArtifact`/`ListArtifacts` (`agen artifacts`). Like
  `finish`, both tools are available to every agent and bypass the
  bundle's permissions and any task overrides entirely (storage mechanics,
  not an action on the outside world). Saving the same bytes again in a
  namespace returns the existing row. Limits: 25 MB per artifact, 500 MB
  per namespace. **Tool-result cap** (2.23/2.36, part): `harness.maxToolResultChars`
  (default 32000 characters, 0 off) is applied in `Agent::invoke` to every
  successful tool call's result, after redaction — MCP tools, `call_agent`/
  `start_agent`/`query_agent` (delegate answers reach here the same way any
  other tool's result does, no separate mechanism needed) and
  `save_artifact`/`read_artifact` themselves alike. An over-cap result is
  saved as its own artifact (the redacted text, so a secret never reaches
  durable storage unmasked; identical content dedupes to the artifact
  already saved) and replaced with a preview plus the artifact id and a
  `read_artifact` call for the rest. Carrying an artifact id over A2A, in
  `finish`'s own text, and Hub tools returning compact rows by default are
  not yet implemented.
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

- **Request extras**: `harness.openrouter` (`provider`, `plugins`) is merged
  into every OpenRouter request as given; `reasoningEffort` becomes
  `reasoning.effort` (OpenRouter) or `reasoning_effort` (OpenAI).
- **Routing models** (`provider/router.rs`): `typesafe/jev-router` (plugin
  `jev-router`, tiers `low`..`high`), `openrouter/auto` (`auto-router`) and
  `openrouter/auto-beta` (`auto-beta-router`), both `low`..`max`. A run's tier
  is the task's `model_tier`, else `router.tier`, capped by `router.maxTier`
  and the router's top tier; under budget pressure it is at most
  `router.budgetTier` (default `low`). The tier and `allowedModels` /
  `excludedModels` go in the router's plugin entry (`cost_tier`), and the
  request asks for OpenRouter metadata so the decision can be recorded. A
  tier is a band the router picks within, not a price cap. Tiers are checked
  per router when a bundle is deployed (Go, `bundle/harness.go`) and loaded
  (engine).
- **Error kinds**: 429 and 5xx are retryable (with backoff, honouring
  `Retry-After`); 401, 402 and 403 are account errors (`provider account
  error`), never retried or failed over, except a 403 for flagged input
  (moderation); other errors fail over to the next fallback model.
- **Provider holds** (Hub, `hub/providers.go`): a provider account is
  `<namespace>/<provider>:<key secret name>[@<baseUrl>]` (a namespace's
  platform secrets are its own). When a task fails with an account error, the
  Hub records a hold (`provider_holds`) on that account: its deployments lease
  no tasks, do not scale up for
  queued work and are not woken until `retry_at`. Then one probe task is
  leased; a probe that fails with an account error backs off further (5 min,
  doubling, at most 1 h). Any run of the account's deployments that succeeds
  after the hold began (task or A2A) ends it (checked by the scheduler each
  tick). `ResumeProvider` (admin) ends it at once. New holds and holds ended
  by a successful run send `provider.held` / `provider.resumed` to
  notification targets that take every event.

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
    The Hub checks that the caller deployment runs on that Nest (assigned
    there, or with a live instance there, such as one draining), and the
    Manager caches the token so delegation survives a Hub outage until it
    expires. Because live instance rows vouch for a caller, a Nest may only
    add instance rows for deployments it is eligible to run (§5).
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
- On-behalf tokens (`CreateApiToken` with `on_behalf`) are for agents that
  work for people, like the console assistant. Each call must name a running
  task (`X-Agen-Task`, which the MCP endpoint fills from the call's `_meta`
  `agen/taskId`); the Hub then acts as that task's submitter with the
  intersection of both principals' scopes (admin never passes) and
  namespaces, and refuses `DecideApproval`. A task started by a trigger has
  no person to act for and is refused.

## 11. API surface

One source: protobuf services in `spec/proto/agen/v1/`, served with Connect by the
Hub, so the same handlers answer gRPC, Connect and plain JSON over HTTP
(`POST /agen.v1.HubService/<Method>`). The MCP server at `/mcp` exposes the same
RPCs as tools, generated from the proto descriptors, so REST, MCP and CLI
cannot drift. The CLI and UI use the same API. `initialize` negotiates the
MCP protocol revision: it echoes the client's `protocolVersion` back when
this server still implements it (its `initialize`/`tools/list`/`tools/call`
shape has not changed), else answers with its own latest
(`mcpserver.ProtocolVersion`, **2025-11-25**).

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
return warnings without deploying. `GetTask` waits up to 300 s and returns
early when the task starts waiting for an approval.

**Event log**: the Store's `events` table holds one row per notable change
(task queued/started/finished/needs_input, approvals, provider holds, missed
triggers and rejected webhooks, deployment changes through the API), written
in the same transaction as the change. Its `seq` comes from the single
`event_seq` row, taken as the transaction's last write: the row lock keeps seq
order equal to commit order, so a reader following a cursor never skips an
event that commits late. `ListEvents` reads after a cursor with type, label,
deployment and severity filters and an optional wait; the stream below
carries the same events with `id: <seq>` and resumes from `Last-Event-ID`.
Events are kept 7 days (pruned hourly by the leader); `agen migrate` copies
them and the seq. Events announced once (budget thresholds, activity
warnings, `task.stuck`) carry a unique dedupe key, so several Hubs or ticks
write them once. The leader records `GetActivity` warnings every 30 s and
delivers notification targets every tick: per target, in seq order from its
stored cursor, one event at a time; a failure pauses the target (growing to
5 min) with the error stored on it, and an event failing for an hour is
dropped and counted.

`GET /api/v1/events` streams change notices (server-sent events) for the
caller's namespaces: tasks, runs, approvals, instances and deployments, and
`step` events: a model call (`gen_ai.chat`) or tool call (`agen.tool`)
starting (state `unfinished`) or ending (its status), with its run, tool,
the agent a delegation waits for (`target`) and its start and end times. One
watcher per Hub polls the Store once a second while anyone listens (hosts
write run data straight to the Store, so the Store is where changes appear)
and fans the changes out; clients refetch what they show. The web console
uses it instead of polling each view.

A model-call span records `agen.message.seq`, the reply's place in its
conversation, and `GetTranscript` returns the conversation, so a trace view
can show what each call sent and received.

Live and fleet views:

- `GetActivity`: every unfinished run with what it is doing now (`model`,
  `tool`, `agent`, `approval` or `idle`, and since when), its turns against
  `maxTurns`, usage, caller and source; queues (due and scheduled, oldest
  wait); and warnings with a kind and severity: `near_turn_limit`,
  `near_run_budget`, `near_daily_budget` (80% of a limit), `quiet_run` (no
  step for 5 min), `waiting_approval` (30 min), `queue_waiting` (5 min),
  `blocked_by_approval` (queued tasks while every run waits for an approval),
  `burst` (10 tasks in one conversation in 10 min), `model_errors` (30% of at
  least 5 calls in 15 min), `provider_held`, `trigger_missed` and
  `webhook_rejected` (in the last day).
- `GetMetrics` adds `models` (calls per served model: failures, fallbacks,
  p50/p95 latency, usage), `entries` (runs without a parent run, by source:
  `api`, `cron`, `webhook`, outside `a2a` callers, and `reply` results with
  the agent they came from), and per call edge `async_calls`, `active` (in
  flight) and `p95_ms`.
- `ListProviderHolds` and `ResumeProvider` (§8); deployments carry
  `provider_hold` while held.
- Cron triggers in deployment responses carry `next_due_at` and
  `last_due_at`, computed as the scheduler fires them. `ListTasks` with
  `scheduled` lists tasks waiting for `not_before`, soonest first.
  `ListTriggerEvents` with an empty deployment name lists every deployment's
  events (of the namespace, or of every namespace the caller may see).

## 12. Storage

One schema, two engines (SQLite, Postgres), versioned migrations. Tables:
definitions, deployments, assignments, nests, instances, tasks, triggers,
trigger_log, approvals, tokens, sessions, conversations, runs, spans, effects,
usage, provider_holds. `agen migrate --to postgres://…` copies a local install.

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
site/        the docs website (Astro, from Trestle), built from docs/
```
