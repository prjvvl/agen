# Changelog

## v0.1.3

### Models and providers
- `harness.json`: `fallbackModels` (tried in order when a model keeps failing
  or gives no answer), `reasoningEffort`, `openrouter` (`provider` routing
  and `plugins`, sent as given) and `router` for routing models
  (`typesafe/jev-router` with tiers low to high, `openrouter/auto` and
  `openrouter/auto-beta` with tiers low to max): `tier`, `maxTier`,
  `budgetTier`, `allowedModels`, `excludedModels`. Tiers are checked per
  router at deploy and start.
- Model-call spans record the model that served the call
  (`gen_ai.response.model`), the router's tier and decision, and fallbacks.
- An empty answer is asked again once, then falls back; a run that gets no
  answer from any model fails with `no_answer`.
- Account errors (401, 402, 403; not a 403 for input flagged by moderation)
  are never retried or failed over. The Hub
  holds the provider account: its deployments take no new tasks, do not
  scale up or wake, and one probe goes through after a backoff (5 min,
  doubling to 1 h); a run that succeeds ends the hold. `ListProviderHolds`,
  `ResumeProvider`, `agen providers [resume]`, and `provider.held` /
  `provider.resumed` notifications.
- Every way to add an agent can pick a routing model: `agen init --model
  --tier --max-tier --fallback`, the console's New agent and Templates
  forms, and the Python, Node and Go SDKs (`router`, `fallback_models`,
  `reasoning_effort`, `openrouter`, `context_tokens`; a run's
  `model_tier`), covered by the shared conformance cases.
- The Python, Node and Go SDKs take `maxTokensPerRun` (the core already did).
- New guide: [Models and providers](docs/guides/models.md).

### Tasks and schedules
- Tasks take a model tier (`agen run --tier`, `modelTier`, A2A metadata
  `agen.model_tier`) and a start time (`agen run --at` / `--in`,
  `notBefore`); a task waiting for its start time holds up nothing.
- At 80% of the daily budget, leased tasks carry `budget_pressure` and
  routing models use the bundle's `budgetTier`.
- `agen wait <task>`; `agen run --timeout 0` waits without a limit.
- `agen cancel <task>`; `agen tasks --scheduled` and `--state`;
  `agen init --list --json`.
- A conversation key read back from a task can be sent again as is.
- Cron triggers carry `nextDueAt` and `lastDueAt`; `ListTasks` with
  `scheduled`; `ListTriggerEvents` across deployments and namespaces.

### Delegation
- `start_agent`: delegate without waiting. The task is queued on the
  delegate and the result comes back as a task in the caller's conversation
  (source `reply:<agent>`). A run started over A2A can start child tasks too.
- A2A calls wait up to 10 min for a free instance (asleep, starting, busy or
  waiting for Nest capacity) instead of failing after 60 s, and a running
  call keeps its deployment awake. `call_agent` waits up to 75 min.
- A run on an instance that is draining after a scale-down or redeploy can
  keep delegating.
- Idle deployments give their capacity back after 1 min while another
  deployment waits for room on a full Nest.
- Deleting a deployment ends its unfinished runs.

### Engine
- `contextTokens` (default 64000): a long run's oldest tool results are
  shortened in what is sent to the model once the run outgrows it; the Store
  keeps them whole.
- `agent.md` `preloadSkills`: skills whose instructions are always in the
  system prompt.
- Failed tool spans carry `error.message`.

### Console
- Overview: a compact Needs attention (counts by kind, the full list in the
  Inbox), a live fleet map (entry points, calls, async starts and results,
  self-calls, travelling dots; pan, zoom and find for large fleets), Now
  (every run in progress and what it is doing), and a per-agent timeline.
- Schedules: cron triggers with their next and last run and Run now, tasks
  waiting for their start time, webhooks with New secret, and missed and
  refused events.
- Costs by served model, and held provider accounts with Resume.
- Deployments: bundle versions with roll back; tasks with start time, model
  tier, where an async result goes, and Cancel; the run form takes a start
  time and a tier. Fleet: add a Nest (join command) and revoke one.

### API
- `GET /api/v1/events` sends step events (a model or tool call starting or
  ending).
- `GetActivity`: unfinished runs and what they are doing, queues, and
  warnings (near limits, quiet runs, long approval waits, queues waiting or
  blocked by an approval, bursts, model errors, held providers, missed
  schedules, refused webhooks).
- `GetMetrics`: per served model, entry points, and async calls, calls in
  flight and p95 per edge.

### Tools and templates
- Built-in tool servers: `agen mcp workspace --root DIR [--no-run]` (read,
  list, grep, edit, write and run, confined to one folder) and
  `agen mcp sql [--dsn-env NAME]` (read-only SQL on Postgres or SQLite).
  Instances get `AGEN_BIN`, so `"command": "agen"` works without `agen` on
  `PATH`.
- New templates: coder, ci-fixer, support, incident-responder and
  data-analyst. fleet-steward also reports live warnings and writes the
  daily cost report. cost-watchdog and support-triage are removed.

### Testing
- `spec/features.yaml` lists every feature and the surfaces it must work on
  (SDKs, API, MCP, CLI, console, agent-to-agent calls, installers, Docker,
  Kubernetes). Tests tag what they cover, and a check fails when an API
  method or field, bundle key, CLI command or SDK option belongs to no
  feature, or a feature is untested on a surface without a recorded gap. The
  [support matrix](docs/reference/support.md) is generated from it.
- Scenarios (`spec/scenarios`) are written once as API calls and played
  through the API, MCP and the CLI.
- `agen notify set --event` lists every event, provider events included.

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
  you confirm, with your permissions (see Templates). The chat shows each
  step as it works; the panel resizes and keeps past chats.

### Traces and metrics
- `GetTranscript` returns a run's conversation, tool calls and results
  included, and its system prompt; `agen transcript`.
- `ListRuns` filters by status, labels, time and task, pages with
  `pageToken`, and with `rootsOnly` lists one row per trace with the usage of
  the whole run tree; `agen runs` (with `--since`, `--task` and each run's
  error).
- `GetMetrics`: runs, failures, duration percentiles, tokens and cost per
  deployment over a window, and the delegation edges between deployments.
- `GET /api/v1/events` streams changes (server-sent events).
- Model-call, tool-call and approval spans are recorded when they start, so
  a running trace shows what is in progress; model-call spans link to their
  messages (`agen.message.seq`) and record their cost.
- A task an agent submits through the Hub's API or MCP tools joins the
  agent's trace and the fleet map, and counts towards the delegation depth.
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
  Each brings its tool servers and skills, which `agen init --list`, the
  console's Templates page and `ListTemplates` show;
  `agen init <dir> --template NAME`.
- Agents that talk to the Hub each keep their own token
  (`AGEN_TOKEN.<agent>`); deploying one no longer replaces another's token
  or access to the shared `AGEN_HUB_URL`.

### Fleet
- An instance that crashes on start is reported with the host's error (for
  example a missing secret), once per distinct error, until one starts: the
  console and `agen ps --all` show why an agent does not start instead of
  "starting".

### MCP
- The instructions MCP clients receive (and `get_bundle_guide`) cover
  investigating failures (runs, transcripts, traces, metrics), templates,
  the limits that end runs, per-agent Hub tokens and on-behalf tokens.

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
