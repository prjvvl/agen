# Contributing

Thanks for helping. Bug reports, docs fixes and pull requests are all welcome.

- **Questions and bugs**: open an [issue](https://github.com/prjvvl/agen/issues);
  for bugs include `agen version`, your OS and how you installed Agen.
- **Security problems**: never in a public issue; see [SECURITY.md](SECURITY.md).
- **Building and testing**: [docs/development.md](docs/development.md) has the
  commands; [AGENTS.md](AGENTS.md) the working rules (contracts in `spec/`
  first, then code; `docs/architecture.md` is the design).

Before opening a pull request:

1. Run the checks for what you changed: `cargo fmt`, `cargo clippy --workspace
   --all-targets -- -D warnings` and `cargo test --workspace` for Rust;
   `go vet ./...` and `go test ./...` in `platform/` for Go.
2. If you changed `spec/`, run `sh scripts/gen.sh` and commit the result; if you
   changed CLI commands or the API, regenerate the reference pages (see
   [docs/development.md](docs/development.md#generated-files)).
3. Update the docs and `CHANGELOG.md` for anything a user would notice.
4. Tests must not call real model providers; use the `fake` or `replay`
   provider.

CI runs the same checks on Linux, macOS and Windows.
