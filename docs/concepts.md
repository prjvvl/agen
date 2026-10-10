# Concepts

The words Agen uses, from the agent outwards.

| Term | Meaning |
|---|---|
| **Bundle** | A directory that defines an agent: `plugin.json`, `x-agen/agent.md` (system prompt), `x-agen/harness.json` (model), optional `mcp.json` (tools), `skills/`, `x-agen/config.json` (permissions, scale, budget, triggers, delegates) and `x-agen/secrets.json`. See the [bundle reference](reference/bundle.md). |
| **Definition** | One immutable version of a bundle, named by the SHA-256 digest of its files (`sha256:…`). Deploying a changed bundle creates a new definition. |
| **Engine** | The Rust library that runs the agent loop: model calls, tool calls, permissions, budgets, memory and tracing. SDKs embed it; `agen-host` runs it as a process. |
| **Agent loop** | One run: call the model, run the tools it asks for, feed back the results, repeat until it answers or hits `maxTurns` or a budget. Every kind of deployment runs the full loop. |
| **Hub** | The control plane. It stores deployments and tasks, schedules instances onto Nests, autoscales, fires triggers and serves the API, the web UI and MCP. It never runs agents. |
| **Nest** | A machine (or Kubernetes namespace) that runs agents: a **Manager** that starts and stops instances and hands them tasks, and a **Gateway** that serves A2A calls. `agen up` runs a Hub and one Nest in one process. |
| **Namespace** | A name that groups deployments, secrets and tokens; tokens can be limited to namespaces. The default is `default`. |
| **Deployment** | A definition running in a namespace with a kind, scale, budget, triggers and placement. Its address is `<namespace>/<name>`. |
| **Kind** | `pool` (min..max interchangeable instances sharing a queue), `singleton` (0 or 1 instance with one long-lived conversation) or `task` (one instance per task). |
| **Instance** | One `agen-host` process (or pod) running one deployment. Instances scale to zero when idle and wake on demand. |
| **Task** | A durable request to a deployment: input, state (`queued`, `leased`, `running`, `succeeded`, `failed`, `cancelled`), result. Tasks come from the API/CLI, triggers and delegation. A task runs at most once; if its instance dies, the next attempt resumes the same run. |
| **Run** | One execution of the agent loop for a task or an A2A call, with its usage, cost and trace. |
| **Session** | The memory container that runs belong to. A run without a conversation key gets a fresh session; a singleton has one session. |
| **Conversation** | The message history inside a session. Runs in the same conversation see earlier turns. Tasks with the same **conversation key** (or A2A messages with the same `contextId`) continue one conversation. See [Memory](guides/memory.md). |
| **Labels** | Free-form `key=value` pairs on a task, copied to its runs, traces, tool calls and delegated tasks, for grouping work by project, workspace or tenant. |
| **Trace / span** | The record of a run: one span per model call, tool call, approval wait and delegation, across agents. `agen trace <task-id>`. |
| **Tool** | Something the model can call: tools of MCP servers in `mcp.json` (named `<server>.<tool>`), `load_skill` for skills, and `call_agent` for delegates. |
| **Skill** | A Markdown file of instructions (`skills/<name>/SKILL.md`) the agent loads when it needs it. |
| **Delegate** | Another deployment this agent may call over A2A through its `call_agent` tool. See [Connecting agents](guides/agents.md). |
| **Permission** | A rule that allows, asks for approval, or denies a tool call. See [Permissions and approvals](guides/permissions.md). |
| **Approval** | A durable request for a person to allow one tool call. The principal whose work asked cannot decide it. |
| **Trigger** | A cron schedule or webhook that submits tasks. See [Triggers](guides/triggers.md). |
| **Secret** | A value the agent needs (an API key), named in `x-agen/secrets.json` and taken from the environment or the Hub's secret store; never stored in the bundle and redacted from logs, traces and messages. |
| **Token** | An API credential with scopes: `viewer` (read), `operator` (deploy, run tasks), `approver` (decide approvals), `admin` (everything). `agen whoami` shows yours. |
| **A2A** | The Agent2Agent protocol. Every deployment is reachable over A2A at its Gateway; agents use it to delegate. |
| **MCP** | The Model Context Protocol. Agents use MCP servers as tools; the Hub is itself an MCP server (`/mcp`) so clients such as Claude can manage the fleet. |
