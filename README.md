# Agen

[![engine](https://github.com/prjvvl/agen/actions/workflows/engine.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/engine.yml)
[![platform](https://github.com/prjvvl/agen/actions/workflows/platform.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/platform.yml)
[![sdks](https://github.com/prjvvl/agen/actions/workflows/sdks.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/sdks.yml)
[![cluster](https://github.com/prjvvl/agen/actions/workflows/cluster.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/cluster.yml)

One agent engine, two ways to use it:

- **Run a fleet**: deploy agents, autoscale them (including to zero), wake
  them on demand, and manage them from the CLI, web UI, REST API or MCP, on
  one machine or many.
- **Embed** an agent in your app with the Python, Node.js or Go SDK.

Status: early development. Interfaces may still change.

**Docs: [prjvvl.github.io/agen](https://prjvvl.github.io/agen/)**

## Install

macOS and Linux:

```sh
curl -fsSL https://prjvvl.github.io/agen/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://prjvvl.github.io/agen/install.ps1 | iex
```

Supported platforms, options, upgrading and uninstalling:
[Install](docs/install.md).

## Quickstart

```sh
agen up                           # Hub + one Nest on this machine; keeps running, so use a second terminal
agen init                         # writes the "hello" example agent to ./hello
agen deploy hello --replicas 1
agen run hello "hi"               # -> Hello! Nice to meet you.
agen down
```

`hello` has a scripted model, so it needs no API key.
[Getting started](docs/getting-started.md) goes on to an agent with a real
model and a real tool.

## What an agent can do

- Use tools: any MCP server, or your own functions (SDK).
- Load skills when it needs them, and call other agents over A2A, with
  depth and fan-out limits.
- Ask a person first: an `ask` permission pauses the run until someone
  approves it from the CLI, web UI or MCP.
- Survive crashes: a run resumes from its last checkpoint, and a side effect
  that already happened is not repeated.
- Remember conversations, and carry labels from a task to every run and tool
  call it causes.
- Keep secrets out of logs, traces, storage and model input.
- Be traced end to end, across agents.
- Use any model through OpenRouter or an OpenAI-compatible API.

## Docs

- [Install](docs/install.md), [Getting started](docs/getting-started.md),
  [Embed an agent](docs/embed.md), [Concepts](docs/concepts.md)
- How-to guides: [tools](docs/guides/tools.md),
  [connecting agents](docs/guides/agents.md),
  [permissions and approvals](docs/guides/permissions.md),
  [budgets](docs/guides/budgets.md), [triggers](docs/guides/triggers.md),
  [memory](docs/guides/memory.md), [MCP clients](docs/guides/mcp.md),
  [running a fleet](docs/deploy.md), [troubleshooting](docs/troubleshooting.md)
- Reference: [bundle](docs/reference/bundle.md), [CLI](docs/reference/cli.md),
  [API](docs/reference/api.md), [SDKs](docs/reference/sdks.md)
- Explanation: [architecture](docs/architecture.md), [security](docs/security.md)
- [Changelog](CHANGELOG.md), [contributing](CONTRIBUTING.md), and
  [developing Agen](docs/development.md) (building from source, tests, layout)

## Security

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

MIT
