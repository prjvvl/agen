# Getting started

This walkthrough ends with an agent that answers questions by reading web
pages with a real model and a real tool, remembers the conversation, and
shows you what it did. It takes about ten minutes.

You need:

- Agen installed ([install from a release](https://github.com/prjvvl/agen#install)).
- An [OpenRouter](https://openrouter.ai) API key. The example uses
  `deepseek/deepseek-v4-flash`; the whole walkthrough costs well under a cent.
- [uv](https://docs.astral.sh/uv/), which runs the
  [fetch MCP server](https://github.com/modelcontextprotocol/servers/tree/main/src/fetch)
  the agent uses as its tool.

## 1. Start a local fleet

The agent's instances inherit the environment of `agen up`, so set the key
first:

```sh
git clone https://github.com/prjvvl/agen && cd agen
export OPENROUTER_API_KEY=sk-or-...   # PowerShell: $env:OPENROUTER_API_KEY = "sk-or-..."
agen up                               # keeps running; use a second terminal for the rest
```

`agen up` runs a Hub and one Nest on localhost with a SQLite store under
`~/.agen`. `agen ui` prints a sign-in link for the web UI.

## 2. Look at the bundle

`examples/bundles/researcher` is a complete agent:

| File | What it says |
|---|---|
| `plugin.json` | The name, `researcher`. |
| `x-agen/agent.md` | The system prompt, and `maxTurns: 8`. |
| `x-agen/harness.json` | The model: OpenRouter, `deepseek/deepseek-v4-flash`. |
| `x-agen/secrets.json` | It needs `OPENROUTER_API_KEY`, from the environment. |
| `mcp.json` | Its tool server: `uvx mcp-server-fetch`, so it gets a `fetch.fetch` tool. |
| `x-agen/config.json` | A pool of up to 2 instances that sleeps after 5 minutes; a token budget per run and a dollar budget per day; tools are denied unless allowed, and `fetch.*` is allowed. |

## 3. Check it and deploy it

```sh
agen deploy examples/bundles/researcher --validate
agen deploy examples/bundles/researcher --replicas 1
agen ps --all
```

`--validate` checks the bundle and warns about likely mistakes (an agent with
no tools, a permission rule for a server that does not exist, a scripted
model) without deploying. `agen ps --all` shows the instance and how many
tools it loaded; the first start downloads the fetch server, so give it a
few seconds.

## 4. Ask it something

```sh
agen run researcher "What is the title of the page at https://example.com?" --conversation demo --label project=docs
agen run researcher "Which URL did you read in my previous question?" --conversation demo
```

Both tasks use the conversation key `demo`, so the second run sees the first
one's messages. Without `--conversation` every task starts fresh (see
[Memory](guides/memory.md)). The label travels with the task to its run,
trace and tool calls.

## 5. See what happened

```sh
agen tasks researcher
agen trace <task-id>
agen logs researcher
```

The trace shows each model call and each tool call with its timing; the logs
show the instance starting and each run with its token usage. The web UI
shows the same, and the cost of each run.

## 6. Clean up

```sh
agen rm researcher
agen down
```

## Next

- [Giving agents tools](guides/tools.md): MCP servers, side effects and retries.
- [Permissions and approvals](guides/permissions.md): let a person approve risky calls.
- [Connecting agents](guides/agents.md): one agent handing work to another.
- [Triggers](guides/triggers.md): run agents on a schedule or from webhooks.
- [Use Agen from Claude or another MCP client](guides/mcp.md).
- [Concepts](concepts.md) and [Troubleshooting](troubleshooting.md).
