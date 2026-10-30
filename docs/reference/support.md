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
| Routing models (Jev Router, Auto Router) and cost tiers | ✓ | ✓ | ✓ | ✓ |
| Reasoning effort | ◐[^1] | ◐[^2] | ◐[^3] | ◐[^4] |
| OpenRouter request settings (provider routing, plugins) | ✓ | ✓ | ✓ | ✓ |
| Long-run compaction (contextTokens) | ◐[^5] | ◐[^6] | ◐[^7] | ◐[^8] |
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
| Routing models (Jev Router, Auto Router) and cost tiers | ✓ | ✓ | ✓ | ✓ |  |
| Reasoning effort | ✓ | ✓ | ✓ |  |  |
| OpenRouter request settings (provider routing, plugins) | ✓ | ✓ | ✓ |  |  |
| Provider account holds and resume | ✓ | ✓ | ✓ | ◐[^9] |  |
| Long-run compaction (contextTokens) | ✓ | ✓ | ✓ |  |  |
| Limits (turns, tokens, durations, queue) | ✓ |  | ✓ |  |  |
| Run outcomes (finish, result schema, report at a limit) | ✓ | ✓ | ✓ | ✓ | ✓ |
| Event log (ListEvents, event stream with a cursor) | ✓ | ✓ | ✓ | –[^10] |  |
| Host tools (functions in your program) | –[^11] |  |  |  |  |
| Tools from MCP servers in the bundle | ✓ |  | ✓ |  |  |
| Built-in workspace and SQL tool servers |  |  | ✓ |  |  |
| Permissions and approvals | ✓ | ✓ | ✓ | ✓ | ✓ |
| Record and list feedback on tasks (a person, a check, or a judge model) | ✓ | ✓ | ✓ | –[^12] |  |
| Memory blocks (durable, named text an agent reads and updates across runs) | ✓ | ✓ | ✓ | –[^13] |  |
| Artifacts (save_artifact / read_artifact; a long result need not stay in context) | ✓ | ✓ | ✓ | –[^14] |  |
| Run tasks and wait for results | ✓ | ✓ | ✓ | ✓ |  |
| Every task of the tree a task belongs to (its root and descendants) | ✓ | ✓ | ✓ |  |  |
| How a busy conversation's queue behaves (collect merges messages instead of running them one at a time; followup is today's default) | ✓ | ✓ | ✓ |  |  |
| An agent reports its own progress; query a task's progress, activity and outcome without waiting on it | ✓ | ✓ | ✓ |  |  |
| Cancel a task or run | ✓ | ✓ | ✓ | ✓ |  |
| Stream output while a run is in progress | –[^15] |  |  |  |  |
| Conversations and labels | ✓ | ✓ | ✓ |  |  |
| Per-task cost tier | ✓ | ✓ | ✓ | ✓ |  |
| Tasks with a start time, an expiry, or at most one queued per key | ✓ | ✓ | ✓ | ✓ |  |
| Cron and webhook triggers | ✓ | ✓ | ✓ | ✓ |  |
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

[^1]: Reasoning effort on Rust: engine tests only: the scripted test provider ignores reasoning effort
[^2]: Reasoning effort on Python: engine tests only
[^3]: Reasoning effort on Node: engine tests only
[^4]: Reasoning effort on Go: engine tests only
[^5]: Long-run compaction (contextTokens) on Rust: engine tests only: no SDK case runs long enough to compact
[^6]: Long-run compaction (contextTokens) on Python: engine tests only
[^7]: Long-run compaction (contextTokens) on Node: engine tests only
[^8]: Long-run compaction (contextTokens) on Go: engine tests only
[^9]: Provider account holds and resume on Console: a held account cannot be made from the console test; Hub tests cover holds
[^10]: Event log (ListEvents, event stream with a cursor) on Console: the console follows changes live through the same stream; it has no event log view yet
[^11]: Host tools (functions in your program) on API: deployed agents get tools from MCP servers in the bundle (tools.mcp)
[^12]: Record and list feedback on tasks (a person, a check, or a judge model) on Console: no UI yet; use agen feedback or the API
[^13]: Memory blocks (durable, named text an agent reads and updates across runs) on Console: no UI yet; use agen memory or the API
[^14]: Artifacts (save_artifact / read_artifact; a long result need not stay in context) on Console: no UI yet; use agen artifacts or the API
[^15]: Stream output while a run is in progress on API: tasks return the final result; follow live steps on /api/v1/events
