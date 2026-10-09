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

## Guides

- [Running Agen](deploy.md): local, distributed (Hubs, Nests, Postgres) and
  Kubernetes.
- [Security](security.md): operating a fleet securely.
- [Architecture](architecture.md): how the engine and the platform fit
  together.

The source, examples and issue tracker are on
[GitHub](https://github.com/prjvvl/agen).
