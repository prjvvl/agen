# Working in this repo

- Read `docs/architecture.md` first. It is the contract; changing a decision there
  needs a matching code change and a note in the commit message.
- Contracts live in `spec/`. Change the schema or proto first, then the code.
- Rust: `cargo fmt`, `cargo clippy --workspace --all-targets -- -D warnings`,
  `cargo test --workspace`.
- Go (in `platform/`): `go vet ./...`, `go test ./...`.
- Tests never call real model providers. Use the `fake` or `replay` provider.
  Real-provider smoke tests are opt-in via `AGEN_LIVE_TESTS=1` and read the key
  from `OPENROUTER_API_KEY`.
- Never commit secrets. Bundles hold secret names only.
