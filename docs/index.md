# Agen

One agent engine, two ways to use it:

- **Run a fleet**: deploy agents, autoscale them (including to zero), wake
  them on demand, and manage them from the CLI, web UI, REST API or MCP, on
  one machine or many.
- **Embed** an agent in your app with the Python, Node.js or Go SDK.

Agen is in early development; interfaces may still change.

## Install

```sh
curl -fsSL https://prjvvl.github.io/agen/install.sh | sh
```

On Windows, in PowerShell: `irm https://prjvvl.github.io/agen/install.ps1 | iex`.
See [Install](install.md) for platforms, options and uninstalling.

## Tutorials

- [Getting started](getting-started.md): a local fleet, an example agent, then
  a real model with a real tool.
- [Embed an agent](embed.md): the engine inside your own Python, Node.js or Go
  program.

## How-to guides

- [Giving agents tools](guides/tools.md): MCP servers, side effects, retries.
- [Connecting agents](guides/agents.md): delegation and its limits.
- [Permissions and approvals](guides/permissions.md)
- [Budgets](guides/budgets.md)
- [Triggers](guides/triggers.md): schedules and webhooks.
- [Memory and conversations](guides/memory.md): conversation keys and labels.
- [Using Agen from Claude and other MCP clients](guides/mcp.md)
- [Running Agen](deploy.md): local, distributed (Hubs, Nests, Postgres) and
  Kubernetes.
- [Troubleshooting](troubleshooting.md)

## Reference

- [Bundle](reference/bundle.md): every file and field.
- [CLI](reference/cli.md)
- [API](reference/api.md): REST, gRPC and MCP.
- [SDKs](reference/sdks.md)

## Explanation

- [Concepts](concepts.md): Hub, Nest, deployment, task, run, conversation and
  the other words Agen uses.
- [Architecture](architecture.md): how the engine and the platform fit
  together.
- [Security](security.md): how a fleet is secured, and how to operate it.

[Changelog](https://github.com/prjvvl/agen/blob/main/CHANGELOG.md) ·
[Developing Agen](development.md) ·
[GitHub](https://github.com/prjvvl/agen)
