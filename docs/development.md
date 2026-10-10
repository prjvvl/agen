# Developing Agen

## Build from source

Needs Rust (stable), Go 1.26 and, for the web UI, Node.js 22.12 or newer. The
scripts are POSIX shell; on Windows run them from Git Bash.

```sh
git clone https://github.com/prjvvl/agen && cd agen
sh scripts/build-web.sh                           # the web UI, embedded into agen (optional)
cargo build --release -p agen-host
go build -o target/release/ ./platform/cmd/agen   # agen finds agen-host next to it
```

`target/release` then holds `agen` and `agen-host`; put it on `PATH`, or make
release archives with `scripts/release.sh` and install one with
`scripts/install.sh <archive>` / `scripts/install.ps1 <archive>`.

## Tests

| What | Command |
|---|---|
| Engine | `cargo fmt --check && cargo clippy --workspace --all-targets -- -D warnings && cargo test --workspace` |
| Platform | `cd platform && go vet ./... && go test ./...` |
| Postgres too | set `AGEN_TEST_POSTGRES_URL=postgres://...` (and `AGEN_REQUIRE_PG=1` to fail instead of skip) |
| Web UI | `cd web && npm ci && npx tsc --noEmit && npx playwright test` |
| SDKs | see each README in `sdks/` |
| Distributed cluster | `docker build -f deploy/docker/Dockerfile -t agen:dev .`, then `AGEN_CLUSTER_E2E=1 go test ./e2e -run TestDistributedCluster` in `platform/` |
| Docs site | `cd site && npm ci && npm run check && npm run build` |
| Install | `scripts/test-install.sh <archive>` / `scripts/test-install.ps1 <archive>` |

Tests never call a real model; they use the `fake` and `replay` providers.
Opt-in live smoke tests read `OPENROUTER_API_KEY` with `AGEN_LIVE_TESTS=1`.

## Generated files

Contracts live in `spec/` (bundle JSON Schemas, protobufs, SQL migrations).
After changing them, run `sh scripts/gen.sh`, which regenerates the Go API
code and copies the schemas, migrations and example bundles into the
platform. The CLI and API reference pages are generated from the code:

```sh
cd platform && AGEN_UPDATE_DOCS=1 go test ./internal/cli -run TestReferenceDocs
```

CI fails when generated files are out of date, when the docs use a command
or flag the CLI does not have, or when the local walkthrough in
[Running Agen](deploy.md) does not run as written.

## Layout

| Path | What |
|---|---|
| `spec/` | Bundle JSON Schemas, API protobufs, SQL migrations (the contracts) |
| `engine/` | Rust engine, `agen-host`, SDK core, C ABI, test kit |
| `sdks/` | Python, Node.js and Go SDKs |
| `platform/` | Go: Hub, Nest (Manager + Gateway), CLI; `platform/e2e` runs the distributed and Kubernetes scenarios |
| `web/` | React web UI |
| `docs/`, `site/` | Documentation, and the website that publishes it, built on [Trestle](https://github.com/prjvvl/trestle) |
| `examples/` | Example bundles (also built into `agen init`) and SDK apps |
| `deploy/` | Docker image, compose cluster, Kubernetes manifests |
| `scripts/` | Code generation, release, install |

`AGENTS.md` has the working rules for this repository, and
[Architecture](architecture.md) the design.
