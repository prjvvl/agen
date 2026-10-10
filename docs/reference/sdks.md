# SDKs

The SDKs embed the same Rust engine that runs in a fleet, so an agent behaves
the same in your app and on the platform. They are thin wrappers over one
shared core; every SDK passes the conformance suite in `spec/conformance`.

| Language | Package | README | Example |
|---|---|---|---|
| Python | `sdks/python` (native module, built with maturin; needs Rust) | [sdks/python](https://github.com/prjvvl/agen/tree/main/sdks/python) | [examples/python-app](https://github.com/prjvvl/agen/tree/main/examples/python-app) |
| Node.js | `sdks/node` (napi-rs) | [sdks/node](https://github.com/prjvvl/agen/tree/main/sdks/node) | [examples/node-app](https://github.com/prjvvl/agen/tree/main/examples/node-app) |
| Go | `sdks/go` (cgo over a static library) | [sdks/go](https://github.com/prjvvl/agen/tree/main/sdks/go) | [examples/go-app](https://github.com/prjvvl/agen/tree/main/examples/go-app) |

What every SDK offers:

- An agent from code (instructions, model, provider, tools) or from a bundle
  directory.
- Tools written in the host language; side-effecting unless marked read-only,
  with the same effect ledger as the fleet.
- Streaming text, cancellation, and an approval callback for `ask`
  permissions (default: deny).
- Memory: one agent object is one conversation; a store (`sqlite:...`) keeps
  sessions across restarts.

Callbacks (tools, approvals, events) run on one dispatcher thread per agent,
in order: return quickly and hand slow work to another thread. Calling `run`
from inside a callback is rejected.
