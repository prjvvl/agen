# Giving agents tools

An agent's tools come from three places, and nowhere else:

- **MCP servers** listed in the bundle's `mcp.json`. Each server's tools are
  named `<server>.<tool>` (the model sees `<server>_<tool>`, because providers
  do not allow dots; permission rules and settings accept either form).
- **Skills** in `skills/<name>/SKILL.md`: the agent gets a `load_skill` tool to
  read one when it needs it.
- **Delegates** in `x-agen/config.json`: the agent gets a `call_agent` tool for
  the deployments listed there (see [Connecting agents](agents.md)).

An embedded agent can also have tools written in the host language; see the
SDK READMEs.

## MCP servers

```json
{
  "mcpServers": {
    "fetch": { "command": "uvx", "args": ["mcp-server-fetch"] },
    "files": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "./data"] },
    "crm":   { "url": "https://crm.example/mcp", "headers": { "Authorization": "Bearer ${CRM_TOKEN}" } }
  }
}
```

- A `command` server is started by the instance, with the bundle directory as
  its working directory. A relative command that exists in the bundle runs from
  there; anything else is looked up on `PATH`.
- A `url` server is reached over streamable HTTP.
- `${NAME}` in `env` values and headers is replaced with the secret `NAME`,
  which must be declared in `x-agen/secrets.json`.
- Command servers get a cleared environment: a small allowlist (`PATH`,
  `HOME`, `TEMP`, `LANG`, ...), their own `env`, and `PYTHONUTF8=1` /
  `PYTHONIOENCODING=utf-8` so non-English text survives on Windows. They never
  see the host's credentials.
- Every call has a timeout (2 minutes) and is cancelled with its run.
- If a server dies, the instance reports itself unhealthy and is replaced.

`agen ps --all` (and `GetDeployment` / `ListInstances`) shows how many tools
each instance loaded and any tool server that is not connected. If an agent
"has no tools", check that first, then the instance's logs.

## What a tool server is told

Every call carries the work it belongs to in the MCP request's `_meta`, so a
tool server can attribute, scope or audit it:

| Key | Value |
|---|---|
| `agen/namespace`, `agen/deployment` | Where the agent runs. |
| `agen/taskId` | The task (empty for an A2A call). |
| `agen/runId`, `agen/rootRunId` | This run, and the first run of its delegation tree. |
| `agen/conversationId`, `agen/conversationKey` | The conversation, and the caller's key for it if any. |
| `agen/labels` | The task's labels (an object of strings). |
| `traceparent` | The W3C trace context of the tool call's span. |

## Side effects and retries

Agen checkpoints every step, so a run whose instance dies resumes elsewhere.
For tools that change the world, it must never do the same thing twice by
accident:

- A tool is treated as **side-effecting** unless its server marks it
  `readOnlyHint`. Side-effecting calls go through an effect ledger: a call that
  completed is never repeated on resume, its recorded result is reused.
- If a side-effecting call was interrupted (timeout, crash, lost connection),
  Agen cannot know whether it happened. It does not repeat it; the model is
  told `effect_unknown` so it can check before trying again.
- A tool that is safe to repeat with the same arguments can say so: the
  server marks it `idempotentHint`, or the bundle does. An interrupted call to
  such a tool is retried instead.

Override what a server declares in `x-agen/config.json`:

```json
{
  "tools": {
    "fetch.fetch": { "sideEffect": false },
    "crm.upsert_contact": { "idempotent": true }
  }
}
```

## Parallel tool calls

When the model asks for several tools in one turn, they run at the same time
and their results are recorded in the order the model asked for them. A turn
that includes a call needing approval runs its calls one at a time. To always
run them one at a time, set `"parallelToolCalls": false` in
`x-agen/harness.json`.

## Seeing tool calls

Each tool call is a span in the run's trace (`agen trace <task-id>`) with its
duration and status. Arguments are not recorded by default because they may
hold personal data; set `"traceToolArguments": true` in `x-agen/harness.json`
to record them (secrets redacted, up to 4 KiB each).
