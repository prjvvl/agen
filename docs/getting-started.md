# Getting started

In about ten minutes: run a local fleet, deploy an agent, then give one a
real model and a real tool, and see what it did. You need nothing but Agen
for the first part.

## 1. Install

```sh
curl -fsSL https://prjvvl.github.io/agen/install.sh | sh
```

On Windows, in PowerShell: `irm https://prjvvl.github.io/agen/install.ps1 | iex`.
Open a new terminal and check it with `agen version`. [Install](install.md)
has the details.

## 2. Start a local fleet

```sh
agen up
```

```text
agen is up: hub http://127.0.0.1:7070 (credentials in ~/.agen/local.json)
web UI: http://127.0.0.1:7070/ (run 'agen ui' for a sign-in link)
```

`agen up` runs a Hub (the control plane) and one Nest (which runs agents) on
your machine and keeps running; use a second terminal for the rest.
[Concepts](concepts.md) explains the words.

## 3. Deploy an agent

```sh
agen init
agen deploy hello --replicas 1
agen run hello "hi"
```

```text
wrote the hello example to hello
next: agen deploy hello --replicas 1
deployed default/hello definition <digest> desired 1
Hello! Nice to meet you.
```

`agen init` writes an example agent (a "bundle") into `./hello`. Its model is
scripted, so this works without an API key; the reply is canned. Look at the
files: `x-agen/agent.md` is the system prompt, `x-agen/harness.json` picks the
model, `x-agen/config.json` sets scaling and permissions.

## 4. A real model and a real tool

The `researcher` example answers questions by reading web pages, with a model
on [OpenRouter](https://openrouter.ai) and the
[fetch MCP server](https://github.com/modelcontextprotocol/servers/tree/main/src/fetch)
as its tool. It needs an OpenRouter API key (this walkthrough costs well under
a cent) and [uv](https://docs.astral.sh/uv/), which runs the tool server.

Agents get their keys from the environment of `agen up`, so stop it (Ctrl+C
in its terminal, or `agen down`), set the key, and start it again:

```sh
agen down
export OPENROUTER_API_KEY=sk-or-...
agen up
```

On Windows: `$env:OPENROUTER_API_KEY = "sk-or-..."` before `agen up`. Then, in
the second terminal:

```sh
agen init --example researcher
agen deploy researcher --validate
agen deploy researcher --replicas 1
agen run researcher "What is the title of the page at https://example.com?" --conversation demo --label project=docs
agen run researcher "Which URL did you read in my previous question?" --conversation demo
```

```text
bundle is valid
deployed default/researcher definition <digest> desired 1
Example Domain
https://example.com
```

- `--validate` checks a bundle and warns about likely mistakes without
  deploying it.
- Both tasks use the conversation key `demo`, so the second sees the first
  (see [Memory](guides/memory.md)). Without `--conversation` every task
  starts fresh.
- The label travels with the task to its run, trace and tool calls.

## 5. See what happened

```sh
agen ps --all
agen tasks researcher
agen logs researcher
agen trace <task-id>
```

`agen ps --all` shows each instance and how many tools it loaded; the trace
shows every model call and tool call with its timing; the logs show the
instance starting and each run's token usage. `agen ui` prints a sign-in link
for the web UI, which shows the same, with the cost of each run.

## 6. Clean up

```sh
agen rm researcher
agen rm hello
agen down
```

## Next

- [Giving agents tools](guides/tools.md)
- [Permissions and approvals](guides/permissions.md)
- [Connecting agents](guides/agents.md)
- [Triggers](guides/triggers.md): schedules and webhooks
- [Use Agen from Claude or another MCP client](guides/mcp.md)
- [Embed an agent in your app](embed.md) instead of running a fleet
