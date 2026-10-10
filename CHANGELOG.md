# Changelog

## v0.1.2

### Console
- A new web console. The overview shows what needs attention (approvals,
  failures, budgets, full Nests, broken tool servers), a live map of who calls
  whom, and activity as it happens. Updates arrive live instead of by polling.
- Traces: a timeline of every span across agents, with an inspector that
  shows what each model call sent and received and each tool call's arguments
  and result, and a transcript of every run.
- An inbox for approvals (with what the agent was doing) and failed tasks;
  costs per deployment over 24 hours, 7 or 30 days; deployment pages with
  limits, instances, live logs and the bundle's files; `Ctrl+K` to jump
  anywhere; light and dark themes.
- A chat assistant that answers questions about the fleet and makes changes
  you confirm, with your permissions (see Templates).

### Traces and metrics
- `GetTranscript` returns a run's conversation, tool calls and results
  included, and its system prompt; `agen transcript`.
- `ListRuns` filters by status, labels, time and task, pages with
  `pageToken`, and with `rootsOnly` lists one row per trace with the usage of
  the whole run tree; `agen runs`.
- `GetMetrics`: runs, failures, duration percentiles, tokens and cost per
  deployment over a window, and the delegation edges between deployments.
- `GET /api/v1/events` streams changes (server-sent events).
- Model-call, tool-call and approval spans are recorded when they start, so
  a running trace shows what is in progress; model-call spans link to their
  messages (`agen.message.seq`) and record their cost.
- The Hub deletes spans, log lines and trigger events older than 30 days
  (`--retention`).

### Limits
- New limits with defaults: `maxRunDuration` (1 h, not counting approval
  waits), `maxIdenticalToolCalls` (5), `modelRequestTimeout` (10 min),
  `toolTimeout` (10 min) and `maxQueuedTasks` (1000). Delegation limits now
  default to depth 3, 20 calls per run and 50 per tree; `maxOutputTokens`
  defaults to 8192; `approvalTimeout` is at most 168 h. `0` turns a limit off.
- Overriding one limit in `CreateDeployment`/`UpdateDeployment` keeps the
  others.

### Approvals and notifications
- `GetTask` returns early when the task starts waiting for an approval, and
  tasks show the approval they wait for (`agen run` says so).
- Notification targets per namespace (`agen notify`): `approval.pending`,
  `task.failed` and `budget.exhausted`, POSTed as signed JSON.
- On-behalf tokens (`agen token create --on-behalf`) act for the person whose
  task an agent works on, with the scopes both hold, and never decide
  approvals.

### Templates
- Ten ready-made agents: hello, researcher, writer, editor (delegates to the
  researcher and the writer), support triage, pull request reviewer, fleet
  steward, cost watchdog, approval triage and the console assistant.
  `agen init --list`, `agen init <dir> --template NAME`, and the console's
  Templates page; `ListTemplates` in the API.

## v0.1.1

### Install and first run
- One-line installers: `curl -fsSL https://prjvvl.github.io/agen/install.sh | sh`
  and `irm https://prjvvl.github.io/agen/install.ps1 | iex` pick the archive
  for the machine and the latest release (`AGEN_VERSION` pins one), verify its
  checksum, and add `~/.agen/bin` to `PATH`.
- `agen init [dir] [--example hello|researcher]` writes an example bundle, so
  a release install needs no clone of the repository.

### Work identity
- Conversation keys: tasks with the same key (`agen run --conversation`,
  `conversationKey`, a webhook header or body field, or an A2A `contextId`)
  continue one conversation, one task at a time, in order.
- Labels on tasks (`agen run --label`), copied to runs, traces, tool calls and
  delegated tasks.
- Every MCP tool call carries the namespace, deployment, task, run, root run,
  conversation and labels in `_meta`.

### Engine
- The tool calls of one model turn run concurrently (`parallelToolCalls: false`
  turns it off).
- Interrupted calls to idempotent tools (MCP `idempotentHint`, or
  `tools.<server>.<tool>.idempotent`) are retried.
- Permission rules match `server.tool` and `server_tool`.
- Each model call's output limit is lowered to the remaining token budget.
- Tool servers get UTF-8 stdio by default.
- A crashed run's trace keeps its root span, marked `interrupted`, with the
  resumed run and the attempt number under it.
- Opt-in `traceToolArguments` records redacted tool arguments.
- Fixed: runs reported a different definition digest than their deployment
  when tool servers wrote files into the bundle directory.

### Platform, API and MCP
- MCP: instructions explain bundles, tools and permissions; new
  `get_bundle_guide` and `who_am_i`; every method is documented.
- `validateOnly` deploys check a bundle and return warnings (`agen deploy
  --validate`); bundles can be sent as text (`bundleText`).
- Webhooks: HMAC signature verification with a platform secret, configurable
  idempotency and conversation keys, trigger labels.
- Approvals: task, decision time and readable names; optional notification
  webhook (`permissions.notify`); clearer errors for missing scopes and
  self-approval.
- Lifecycle log lines for instances and task retries; instances report their
  loaded tools and tool-server state (`agen ps --all`).
- `GetTask` waits up to 300 s.
- Fixed: `agen start` printed the usage of `agen stop`.
- The web UI links to the docs.

### Docs
- New docs site, with install, getting started, embedding, concepts, how-to
  guides, generated CLI and API references, and troubleshooting.
- `CONTRIBUTING.md`, issue and pull request templates; release notes come from
  this changelog.

## v0.1.0

First public release: the Agen engine with Python, Node.js and Go SDKs, and
the platform (Hub, Nest, CLI, web UI, REST and MCP) for local, distributed and
Kubernetes fleets.
