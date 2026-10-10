# Agen

[![engine](https://github.com/prjvvl/agen/actions/workflows/engine.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/engine.yml)
[![platform](https://github.com/prjvvl/agen/actions/workflows/platform.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/platform.yml)
[![sdks](https://github.com/prjvvl/agen/actions/workflows/sdks.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/sdks.yml)
[![cluster](https://github.com/prjvvl/agen/actions/workflows/cluster.yml/badge.svg)](https://github.com/prjvvl/agen/actions/workflows/cluster.yml)

One agent engine, two ways to use it:

- **Embed** an agent in your app with the Python, Node or Go SDK.
- **Run a fleet** with the Agen platform: deploy agents, autoscale them
  (including to zero), wake them on demand, and manage them from the CLI, web
  UI, REST API or MCP — on one machine or many.

Status: early development. Interfaces may still change.

## What an agent can do

- Use tools: your own functions (SDK) or any MCP server.
- Load skills when it needs them, and call other agents over A2A, with
  depth and fan-out limits.
- Ask a person first: an `ask` permission pauses the run until someone
  approves it from the CLI, web UI or MCP.
- Survive crashes: a run resumes from its last checkpoint, and a side effect
  that already happened is not repeated.
- Keep secrets out of logs, traces, storage and model input.
- Be traced end to end, across agents.
- Use any model through OpenRouter or an OpenAI-compatible API; tests use the
  `fake` and `replay` providers.

## Install

Each [release](https://github.com/prjvvl/agen/releases) has an archive per
platform: `agen_<version>_linux_amd64.tar.gz`, `_linux_arm64.tar.gz` (glibc
2.34 or newer: Ubuntu 22.04, Debian 12, RHEL 9 and later), `_darwin_arm64.tar.gz`
(Apple silicon) and `_windows_amd64.zip`. The installers check the archive's
checksum and put `agen` and `agen-host` in `~/.agen/bin`.

Linux / macOS (use the archive name for your platform):

```sh
curl -fsSLO https://raw.githubusercontent.com/prjvvl/agen/v0.1.0/scripts/install.sh
sh install.sh https://github.com/prjvvl/agen/releases/download/v0.1.0/agen_v0.1.0_linux_amd64.tar.gz
```

Windows (PowerShell):

```powershell
Invoke-WebRequest -UseBasicParsing https://raw.githubusercontent.com/prjvvl/agen/v0.1.0/scripts/install.ps1 -OutFile install.ps1
powershell -ExecutionPolicy Bypass -File install.ps1 https://github.com/prjvvl/agen/releases/download/v0.1.0/agen_v0.1.0_windows_amd64.zip
```

Uninstall with `sh install.sh --uninstall` or
`powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall`.

### Build from source

Needs Rust (stable), Go 1.26 and, for the web UI, Node 22.12 or newer.

```sh
sh scripts/build-web.sh                           # the web UI, embedded into agen (optional)
cargo build --release -p agen-host
go build -o target/release/ ./platform/cmd/agen   # agen finds agen-host next to it
```

## Quickstart: a local fleet

From a clone of this repository, for the example bundles (`hello` needs no
API key):

```sh
agen up                                          # Hub + one Nest; keeps running, so use a second terminal for the rest
agen deploy examples/bundles/hello --replicas 1
agen run hello "hi"
agen ps --all                                    # deployments and instances
agen ui                                          # sign-in link for the web UI
agen down                                        # stops everything agen up started
```

An agent is a folder (a "bundle"): `plugin.json`, skills, and Agen's own
settings in `x-agen/`: its prompt, model, scaling, permissions, budget,
triggers and the agents it may call. See
[examples/bundles/hello](examples/bundles/hello) and the schemas in
[spec/bundle](spec/bundle).

## Embed an agent (Python)

```sh
pip install ./sdks/python
```

```python
from agen import Agent, openrouter, tool

@tool(read_only=True)
def lookup_order(order_id: str) -> dict:
    """Look up an order by id."""
    return {"status": "shipped", "carrier": "UPS"}

agent = Agent("order-desk",
              instructions="Answer order questions using lookup_order.",
              model="deepseek/deepseek-v4-flash",
              provider=openrouter(),          # reads OPENROUTER_API_KEY
              tools=[lookup_order])

with agent:
    result = agent.run("Where is my order A-1001?")
    print(result.status)
    for span in agent.trace(result.trace_id):
        print(span["name"])
```

Runnable versions for each language, which need no API key:
[examples/python-app](examples/python-app),
[examples/node-app](examples/node-app) and
[examples/go-app](examples/go-app).

## Docs

- [docs/deploy.md](docs/deploy.md): run it locally, distributed (Hubs, Nests,
  Postgres) or on Kubernetes.
- [docs/security.md](docs/security.md): operate it securely.
- [docs/architecture.md](docs/architecture.md): the design.

## Layout

| Path | What |
|---|---|
| `spec/` | Bundle JSON Schemas and API protobufs (the contracts) |
| `engine/` | Rust engine, `agen-host`, C ABI |
| `sdks/` | Python, Node and Go SDKs |
| `platform/` | Go: Hub, Nest (Manager + Gateway), CLI; `platform/e2e` runs the distributed and Kubernetes scenarios |
| `web/` | React UI |
| `examples/` | Example bundles and apps |
| `deploy/` | Docker image, compose cluster, Kubernetes manifests |
| `scripts/` | Code generation, release, install |

## Security

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

MIT
