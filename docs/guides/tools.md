# Giving agents tools

An agent's tools come from three places, and nowhere else:

- **MCP servers** listed in the bundle's `mcp.json`. Each server's tools are
  named `<server>.<tool>` (the model sees `<server>_<tool>`, because providers
  do not allow dots; permission rules and settings accept either form).
- **Skills** in `skills/<name>/SKILL.md`: the agent gets a `load_skill` tool to
  read one when it needs it (or has it in its system prompt from the start,
  with `preloadSkills` in `x-agen/agent.md`).
- **Delegates** in `x-agen/config.json`: the agent gets `call_agent` and, in a
  fleet, `start_agent` for the deployments listed there (see
  [Connecting agents](agents.md)).

An embedded agent can also have tools written in the host language; see the
SDK READMEs.

## MCP servers

```json
{
  "mcpServers": {
    "fetch": { "command": "uvx", "args": ["mcp-server-fetch==2026.8.18"] },
    "files": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "./data"] },
    "crm":   { "url": "https://crm.example/mcp", "headers": { "Authorization": "Bearer ${CRM_TOKEN}" } }
  }
}
```

- A `command` server is started by the instance, with the bundle directory as
  its working directory. A relative command that exists in the bundle runs from
  there; anything else is looked up on `PATH`.
- A `url` server is reached over streamable HTTP.
- Pin the versions of servers fetched at start (`mcp-server-fetch==2026.8.18`,
  `npx -y pkg@1.2.3`) so a new release cannot change what your agent runs.
  A web fetcher reaches whatever its Nest can, the Hub on `localhost` and
  cloud metadata addresses included: run such agents on Nests without access
  to internal services.
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

## Built-in tool servers

The `agen` binary has two tool servers of its own, for bundles that need
files, a shell or a database. They speak MCP over stdio. In a fleet, the
Manager gives every instance `AGEN_BIN`, the path of its own `agen` binary,
and a `"command": "agen"` runs that binary, so `agen` need not be on the
hosts' `PATH`.

### Workspace: files and a shell in one folder

```json
{
  "mcpServers": {
    "workspace": { "command": "agen", "args": ["mcp", "workspace", "--root", "/srv/projects/shop"] }
  }
}
```

| Tool | |
|---|---|
| `read` | A text file with line numbers, 400 lines at a time (`offset`, `limit`); long lines are cut. |
| `list` | Files matching a glob (`src/**/*.go`). |
| `grep` | Lines matching a regular expression, as `path:line: text`. |
| `edit` | Replaces one exact piece of text; it must occur once unless `replace_all` is set. |
| `write` | Creates or replaces a whole file. |
| `run` | Runs a shell command (`sh -c`, or `cmd /C` on Windows) in the folder; returns the exit code and the last 20,000 characters of output. |

- Every path is confined to `--root`, symlinks included; `list` and `grep`
  skip `.git`, `node_modules`, `target`, `.venv` and similar folders.
- `run` stops a command after 2 minutes (`--run-timeout`); a call may ask for
  up to 15 minutes. `--no-run` leaves the `run` tool out (files only). For
  read-only access, also allow only `read`, `list` and `grep` in
  `permissions`.
- `read`, `list` and `grep` are marked read-only; `edit`, `write` and `run`
  are side effects (see below).
- `run` is a real shell as the Nest's user. It is confined to the folder only
  as a working directory, not as a sandbox: a command can reach anything that
  user can. Make it `ask` in `permissions` unless the agent runs somewhere
  isolated (the `coder` template does this), and use `--no-run` when the
  agent only needs files.

```sh
agen mcp workspace --root ./docs --no-run
```

### SQL: read-only queries

```json
{
  "mcpServers": {
    "sql": {
      "command": "agen",
      "args": ["mcp", "sql", "--dsn-env", "DATABASE_URL"],
      "env": { "DATABASE_URL": "${DATABASE_URL}" }
    }
  }
}
```

Declare `DATABASE_URL` in `x-agen/secrets.json` (for example with source
`platform`) so the connection string never appears in `mcp.json` or on a
command line. `--dsn-env` names the variable (default `DATABASE_URL`).

| Tool | |
|---|---|
| `tables` | Tables and views. |
| `describe` | A table's columns: name, type, nullable. |
| `query` | One query; up to 100 rows by default, at most 1000 (`limit`). |

- `postgres://` and `postgresql://` URLs run each query in a `READ ONLY`
  transaction with a statement timeout (30 s, `--timeout`). Anything else is
  a SQLite file, opened read-only.
- Long cells and large results are cut, with a note to narrow the query.
- Read-only transactions stop writes, not expensive reads. For Postgres, also
  connect as a role that may only read what the agent should see.

## What a tool server is told

Every call carries the work it belongs to in the MCP request's `_meta`, so a
tool server can attribute, scope or audit it:

| Key | Value |
|---|---|
| `agen/namespace`, `agen/deployment` | Where the agent runs. |
| `agen/taskId` | The task, or the A2A task for an A2A call. |
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
