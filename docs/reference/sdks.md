# SDKs

The SDKs embed the same Rust engine that runs in a fleet, so an agent behaves
the same in your app and on the platform. They are thin wrappers over one
shared core; every SDK passes the conformance suite in `spec/conformance`.

| Language | Package | README | Example |
|---|---|---|---|
| Python | `sdks/python` (native module, built with maturin; needs Rust) | [sdks/python](https://github.com/prjvvl/agen/tree/main/sdks/python) | [examples/python-app](https://github.com/prjvvl/agen/tree/main/examples/python-app) |
| Node.js | `sdks/node` (napi-rs) | [sdks/node](https://github.com/prjvvl/agen/tree/main/sdks/node) | [examples/node-app](https://github.com/prjvvl/agen/tree/main/examples/node-app) |
| Go | `sdks/go` (cgo over a static library) | [sdks/go](https://github.com/prjvvl/agen/tree/main/sdks/go) | [examples/go-app](https://github.com/prjvvl/agen/tree/main/examples/go-app) |
| Rust | the `agen-sdk-core` and `agen-engine` crates (the core the other SDKs wrap) | [engine](https://github.com/prjvvl/agen/tree/main/engine) | `engine/crates/agen-sdk-core/tests` |

What every SDK offers:

- An agent from code (instructions, model, provider, tools) or from a bundle
  directory.
- Tools written in the host language; side-effecting unless marked read-only,
  with the same effect ledger as the fleet.
- Streaming text, cancellation, and an approval callback for `ask`
  permissions (default: deny).
- Memory: one agent object is one conversation; a store (`sqlite:...`) keeps
  sessions across restarts.
- Models and budgets: fallback models, routing models with cost tiers (per
  agent and per run), reasoning effort, OpenRouter settings, compaction
  (`contextTokens`) and `maxTokensPerRun`; see [Models and providers](../guides/models.md).
  A router's `budgetTier` applies to deployed agents only (the Hub tracks
  daily spend).
- With `bundle`, the bundle's own settings are used: options passed next to
  it are refused.

Callbacks (tools, approvals, events) run on one dispatcher thread per agent,
in order: return quickly and hand slow work to another thread. Calling `run`
from inside a callback is rejected.
