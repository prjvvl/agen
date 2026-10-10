# Agen

One agent engine, two ways to use it:

- **Embed** an agent in your app with the Python, Node or Go SDK.
- **Run a fleet** with the Agen platform: deploy agents, autoscale them
  (including to zero), wake them on demand, and manage them from the CLI, web
  UI, REST API or MCP, on one machine or many.

Agen is in early development; interfaces may still change.

## Install

Download a release from
[GitHub Releases](https://github.com/prjvvl/agen/releases) and install it
with the script for your OS, as shown in the
[README](https://github.com/prjvvl/agen#install). Then, with the example
bundles from the repository (`hello` needs no API key):

```sh
git clone https://github.com/prjvvl/agen && cd agen
agen up        # keeps running; use a second terminal for the rest
agen deploy examples/bundles/hello --replicas 1
agen run hello "hi"
agen down
```

## Start here

- [Getting started](getting-started.md): an agent with a real model and a real
  tool, in ten minutes.
- [Concepts](concepts.md): Hub, Nest, deployment, task, run, conversation and
  the other words Agen uses.

## Guides

- [Giving agents tools](guides/tools.md): MCP servers, side effects, retries.
- [Connecting agents](guides/agents.md): delegation and its limits.
- [Permissions and approvals](guides/permissions.md)
- [Budgets](guides/budgets.md)
- [Triggers](guides/triggers.md): cron and webhooks.
- [Memory and conversations](guides/memory.md): conversation keys and labels.
- [Using Agen from Claude and other MCP clients](guides/mcp.md)
- [Running Agen](deploy.md): local, distributed (Hubs, Nests, Postgres) and
  Kubernetes.
- [Security](security.md): operating a fleet securely.
- [Troubleshooting](troubleshooting.md)

## Reference

- [Bundle](reference/bundle.md): every file and field.
- [CLI](reference/cli.md)
- [API](reference/api.md): REST, gRPC and MCP.
- [SDKs](reference/sdks.md): Python, Node.js and Go.
- [Architecture](architecture.md): how the engine and the platform fit
  together.

The source, examples and issue tracker are on
[GitHub](https://github.com/prjvvl/agen).
