# Agen

One agent engine, two ways to use it:

- **Embed** an agent in your app with the Python, Node or Go SDK.
- **Run a fleet** with the Agen platform: deploy agents, autoscale them
  (including to zero), wake them on demand, and manage them from the CLI, web
  UI, REST API or MCP — on one machine or many.

Status: early development. See [docs/deploy.md](docs/deploy.md) to run it, [docs/security.md](docs/security.md) to operate it securely and [docs/architecture.md](docs/architecture.md) for the design.

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

## License

MIT
