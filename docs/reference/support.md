# Support matrix

<!-- Generated from spec/features.yaml and the `features:` tags in tests: AGEN_UPDATE_DOCS=1 go test ./internal/features -->

What works where, and what proves it. ✓ is tested on that surface, ◐ is offered there but not tested yet (or, with a note, not fully offered yet), – is deliberately not offered there (see the note), and an empty cell does not apply.

MCP tools are generated from the same API methods, so a feature on the API is reachable over MCP; the MCP column shows where that is also tested. Platform tests run on Linux, macOS and Windows in CI.

## Core (embed an agent in your program)

| Feature | Rust | Python | Node | Go |
|---|:-:|:-:|:-:|:-:|
| Instructions, model and provider | ✓ | ✓ | ✓ | ✓ |
| Agents from bundles | ✓ | ✓ | ✓ | ✓ |
| Skills (on demand and preloaded) | ✓ | ✓ | ✓ | ✓ |
| Fallback models | ✓ | ✓ | ✓ | ✓ |
| Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) | ◐[^1] | –[^2] | –[^3] | –[^4] |
| Routing models (Jev Router, Auto Router) and cost tiers | ✓ | ✓ | ✓ | ✓ |
| Reasoning effort | ◐[^5] | ◐[^6] | ◐[^7] | ◐[^8] |
| OpenRouter request settings (provider routing, plugins) | ✓ | ✓ | ✓ | ✓ |
| Long-run compaction (contextTokens) | ◐[^9] | ◐[^10] | ◐[^11] | ◐[^12] |
| Limits (turns, tokens, durations, queue) | ✓ | ✓ | ✓ | ✓ |
| Run outcomes (finish, result schema, report at a limit) | ✓ | ✓ | ✓ | ✓ |
| Host tools (functions in your program) | ✓ | ✓ | ✓ | ✓ |
| Permissions and approvals | ✓ | ✓ | ✓ | ✓ |
| Run tasks and wait for results | ✓ | ✓ | ✓ | ✓ |
| Cancel a task or run | ✓ | ✓ | ✓ | ✓ |
| Stream output while a run is in progress | ✓ | ✓ | ✓ | ✓ |
| Conversations and labels | ✓ | ✓ | ✓ | ✓ |
| Per-task cost tier | ✓ | ✓ | ✓ | ✓ |
| Runs, traces and transcripts | ✓ | ✓ | ✓ | ✓ |

## Fleet (run agents on the Hub and Nests)

| Feature | API | MCP | CLI | Console | Agent to agent |
|---|:-:|:-:|:-:|:-:|:-:|
| Instructions, model and provider | ✓ | ✓ | ✓ | ✓ |  |
| Agents from bundles | ✓ | ✓ | ✓ | ✓ |  |
| Skills (on demand and preloaded) | ✓ |  | ✓ | ✓ |  |
| Fallback models | ✓ | ✓ | ✓ | ✓ |  |
| Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) | –[^13] | –[^14] | –[^15] | ◐[^16] |  |
| Routing models (Jev Router, Auto Router) and cost tiers | ✓ | ✓ | ✓ | ✓ |  |
| Reasoning effort | ✓ | ✓ | ✓ |  |  |
| OpenRouter request settings (provider routing, plugins) | ✓ | ✓ | ✓ |  |  |
| Provider account holds and resume | ✓ | ✓ | ✓ | ◐[^17] |  |
| Long-run compaction (contextTokens) | ✓ | ✓ | ✓ |  |  |
| Limits (turns, tokens, durations, queue) | ✓ |  | ✓ |  |  |
| Run outcomes (finish, result schema, report at a limit) | ✓ | ✓ | ✓ | ✓ | ✓ |
| Event log (ListEvents, event stream with a cursor) | ✓ | ✓ | ✓ | –[^18] |  |
| Host tools (functions in your program) | –[^19] |  |  |  |  |
| Tools from MCP servers in the bundle | ✓ |  | ✓ |  |  |
| Built-in workspace and SQL tool servers |  |  | ✓ |  |  |
| Permissions and approvals | ✓ | ✓ | ✓ | ✓ | ✓ |
| Record and list feedback on tasks (a person, a check, or a judge model) | ✓ | ✓ | ✓ | –[^20] |  |
| Memory blocks (durable, named text an agent reads and updates across runs), live context sources and conversation rollover | ✓ | ✓ | ✓ | –[^21] |  |
| Artifacts (save_artifact / read_artifact; a long result need not stay in context) | ✓ | ✓ | ✓ | –[^22] |  |
| Run tasks and wait for results | ✓ | ✓ | ✓ | ✓ |  |
| Every task of the tree a task belongs to (its root and descendants) | ✓ | ✓ | ✓ |  |  |
| How a busy conversation's queue behaves (followup, the default, queues separately; collect merges; steer delivers into the running turn; interrupt gracefully cancels it first); SubmitTask.delivery overrides it for one submission | ✓ | ✓ | ✓ |  |  |
| An agent reports its own progress; query a task's progress, activity and outcome without waiting on it | ✓ | ✓ | ✓ |  |  |
| Cancel a task or run | ✓ | ✓ | ✓ | ✓ |  |
| Cancel a task gracefully, giving each run a last turn to call finish before it is stopped | ✓ | ✓ | ✓ |  |  |
| Hold a task, and resume it; a queued one is skipped by leasing, a running one suspends at its next model call | ✓ | ✓ | ✓ |  |  |
| Stream output while a run is in progress | –[^23] |  |  |  |  |
| Conversations and labels | ✓ | ✓ | ✓ |  |  |
| Per-task cost tier | ✓ | ✓ | ✓ | ✓ |  |
| Tasks with a start time, an expiry, or at most one queued per key | ✓ | ✓ | ✓ | ✓ |  |
| Cron, webhook, event and random triggers; a burst of events a trigger matches within coalesce merges into one task | ✓ | ✓ | ✓ | ✓ |  |
| Agents calling agents (call_agent) | ✓ |  | ✓ |  | ✓ |
| Start an agent without waiting (start_agent) | ✓ |  |  |  | ✓ |
| Deploy, update, scale, pause and delete | ✓ | ✓ | ✓ | ✓ |  |
| Check a bundle before deploying | ✓ | ✓ | ✓ | ✓ |  |
| Bundle versions and roll back | ✓ | ✓ |  | ✓ |  |
| Start from a template | ✓ | ✓ | ✓ | ✓ |  |
| Daily budgets | ✓ | ✓ | ✓ | ✓ |  |
| Platform secrets | ✓ | ✓ | ✓ | ✓ |  |
| Runs, traces and transcripts | ✓ | ✓ | ✓ | ✓ |  |
| Deployment logs | ✓ | ✓ | ✓ | ✓ |  |
| Record a deployment's model calls and replay a run from its cassette offline | ✓ | ✓ | ✓ |  |  |
| Run scenario files against a bundle with mocked tools; report pass@1/pass^k; eval history per deployment and deploy --require-eval | ✓ | ✓ | ✓ | –[^24] |  |
| Metrics, costs and live activity | ✓ | ✓ |  | ✓ |  |
| Notification targets | ✓ | ✓ | ✓ | ✓ |  |
| API tokens, scopes and namespaces | ✓ | ✓ | ✓ | ✓ |  |
| Local fleet (agen up) |  |  | ✓ |  |  |
| Nests (machines that run agents) | ✓ | ✓ | ✓ | ✓ |  |

## Install and run the fleet

| Feature | Installer (Linux, macOS) | Installer (Windows) | Docker cluster | Kubernetes |
|---|:-:|:-:|:-:|:-:|
| Agents calling agents (call_agent) |  |  | ✓ |  |
| Deploy, update, scale, pause and delete |  |  | ✓ | ✓ |
| Local fleet (agen up) | ✓ | ✓ |  |  |
| Nests (machines that run agents) |  |  | ✓ | ✓ |

[^1]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on Rust: tested at the engine level (engine/crates/agen-engine/tests/model_routing.rs), not yet exposed through agen-sdk-core's own AgentSpec
[^2]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on Python: bundle-config only, no SDK surface
[^3]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on Node: bundle-config only, no SDK surface
[^4]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on Go: bundle-config only, no SDK surface
[^5]: Reasoning effort on Rust: engine tests only: the scripted test provider ignores reasoning effort
[^6]: Reasoning effort on Python: engine tests only
[^7]: Reasoning effort on Node: engine tests only
[^8]: Reasoning effort on Go: engine tests only
[^9]: Long-run compaction (contextTokens) on Rust: engine tests only: no SDK case runs long enough to compact
[^10]: Long-run compaction (contextTokens) on Python: engine tests only
[^11]: Long-run compaction (contextTokens) on Node: engine tests only
[^12]: Long-run compaction (contextTokens) on Go: engine tests only
[^13]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on API: bundle-config only, no SDK surface
[^14]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on MCP: bundle-config only, no SDK surface
[^15]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on CLI: bundle-config only, no SDK surface
[^16]: Fallback providers, each a full account, tried in order only after an account error on the primary (not a transient one, and not just a failing model) on Console: no dedicated UI yet; the generic trace/span inspector already shows gen_ai.provider.name and agen.fallback_provider when one fires, not verified by an e2e test
[^17]: Provider account holds and resume on Console: a held account cannot be made from the console test; Hub tests cover holds
[^18]: Event log (ListEvents, event stream with a cursor) on Console: the console follows changes live through the same stream; it has no event log view yet
[^19]: Host tools (functions in your program) on API: deployed agents get tools from MCP servers in the bundle (tools.mcp)
[^20]: Record and list feedback on tasks (a person, a check, or a judge model) on Console: no UI yet; use agen feedback or the API
[^21]: Memory blocks (durable, named text an agent reads and updates across runs), live context sources and conversation rollover on Console: no UI yet; use agen memory or the API
[^22]: Artifacts (save_artifact / read_artifact; a long result need not stay in context) on Console: no UI yet; use agen artifacts or the API
[^23]: Stream output while a run is in progress on API: tasks return the final result; follow live steps on /api/v1/events
[^24]: Run scenario files against a bundle with mocked tools; report pass@1/pass^k; eval history per deployment and deploy --require-eval on Console: no UI yet; use agen eval/deploy --require-eval or the API
