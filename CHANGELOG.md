# Changelog

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

### Docs
- New docs site, with install, getting started, embedding, concepts, how-to
  guides, generated CLI and API references, and troubleshooting.

## v0.1.0

First public release: the Agen engine with Python, Node.js and Go SDKs, and
the platform (Hub, Nest, CLI, web UI, REST and MCP) for local, distributed and
Kubernetes fleets.
