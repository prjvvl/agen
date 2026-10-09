# SDK conformance suite

One set of fixtures (`cases/*.json`) that every SDK must pass: the Rust
reference runner in `agen-sdk-core`, and the Go, Python and Node runners.
Runners implement the `behavior` of each host tool as a real host-language
tool, so native dispatch is exercised in every language.

## Case format

| Field | Meaning |
|---|---|
| `agent` | Agent spec (JSON as accepted by every SDK); `bundle` paths are relative to the repo root. Tools are declared in `tools`, not here. |
| `tools[]` | `{name, readOnly?, timeoutSeconds?, behavior}` host tools |
| `approvals` | `"approve"`, `"deny"`, `"hang"` (never answers), or absent (no approval handler) |
| `runs[]` | `{input, options?: {taskId}, sameSession?, cancelAfterMs?, cancelBeforeStart?, closeAfterMs?, concurrent?}` executed in order |
| `expect[]` | one entry per run (see below) |
| `createError` | expected error code when creating the agent fails |

Tool `behavior` (exactly one):
- `{"return": <any>}`: return the value (strings as-is, others as JSON)
- `{"echoArgs": true}`: return the arguments as JSON
- `{"error": "<message>"}`: fail with that message
- `{"sleepMs": n}`: wait up to `n` ms, stopping early when the call is cancelled, then return `"slept"`
- `{"nonJson": true}`: return a value the language cannot encode as JSON (Go: an error, since Go tools return strings)

Expectations per run (all optional except `status`):
`status`, `output`, `minDeltas`, `deltasEqualOutput`, `toolCalls` (host tool
calls in order), `approvalsAsked`, `toolCancelled`, `sameConversationAsPrevious`,
`sameRunAsPrevious`, `maxWallMs` (wall time for the run, or for the whole
batch with `concurrent`), `traceIncludes` (span names that must appear in the
run's trace). With `concurrent: n`, every one of the `n` runs must meet the
expectation.

Runners must **fail on any key they do not recognise** (case, agent, tool,
run, options, expectation), so a fixture can never be silently half-checked.
Allowed `agent` keys: `name, bundle, instructions, model, provider,
permissions, maxTurns, approvalTimeoutSeconds`.
