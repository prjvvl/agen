# Budgets and limits

Budgets stop an agent from spending more than you meant it to. They are set in
`x-agen/config.json` (or per deployment with `UpdateDeployment`):

```json
{
  "budget": { "maxTokensPerRun": 50000, "maxUsdPerRun": 0.10, "maxUsdPerDay": 5 }
}
```

| Budget | Enforced by | What happens |
|---|---|---|
| `maxTokensPerRun` | the engine | The run fails with `budget_exceeded` once its input + output tokens reach the limit. |
| `maxUsdPerRun` | the engine | The same, by cost. Needs a provider that reports cost (OpenRouter does; direct OpenAI does not). |
| `maxUsdPerDay` | the Hub | Once today's (UTC) spend of the deployment reaches it, no new work starts until the next UTC day: no task leases, no wake-ups, no scale-up (callers get 503 with the reset time). Runs already in flight finish. |

## How close the limits are

- Before each model call, the output limit (`max_tokens`) is lowered to what
  is left of `maxTokensPerRun`, so a single answer cannot jump far past it.
  This applies to the `maxOutputTokens` you set in `x-agen/harness.json`; if
  you set none, the remaining budget is only sent once it is below 8192
  tokens, because some providers reject or pre-charge large limits.
- The input of the next call still counts, so a run can end slightly over
  `maxTokensPerRun`. Keep `maxOutputTokens` modest for tight budgets.
- `maxUsdPerDay` is checked when work starts, so the day's spend can overshoot
  by the runs already in flight (at most `maxUsdPerRun` × concurrency).

## Seeing spend

Each run records its tokens and cost: `agen logs <name>` prints them per run,
`ListRuns` returns them, and the web UI shows them per deployment. `agen ps`
marks a deployment whose daily budget is used up.

## Limits

Limits stop a run that goes wrong (a loop, a hung tool, a runaway
delegation) even without a budget. Every limit has a default; set one to `0`
to turn it off.

```json
{
  "limits": { "maxRunDuration": "2h", "maxIdenticalToolCalls": 10, "maxQueuedTasks": 5000 }
}
```

| Limit | Where | Default | What happens |
|---|---|---|---|
| `maxTurns` | `x-agen/agent.md` | 16 | Model calls per run; the run fails past it. |
| `maxOutputTokens` | `x-agen/harness.json` | 8192 | The most one model answer may produce. |
| `maxRunDuration` | `limits` | `1h` | Working time of a run, not counting approval waits; the run fails past it. |
| `maxIdenticalToolCalls` | `limits` | 5 | The same tool with the same arguments in one run; further identical calls are refused and the model is told to change course. |
| `modelRequestTimeout` | `limits` | `10m` | One model request; it fails past it. |
| `toolTimeout` | `limits` | `10m` | One tool call (not `call_agent`, which the callee's limits bound); the model gets a timeout error. |
| `maxQueuedTasks` | `limits` | 1000 | Queued tasks per deployment; more are refused (`RESOURCE_EXHAUSTED`, webhooks get 429). |
| `maxDelegationDepth`, `maxFanOut`, `maxTotalDelegations` | `limits` | 3, 20, 50 | See [Connecting agents](agents.md#limits). |
| `approvalTimeout` | `permissions` | `1h` | How long an ask waits; at most `168h`. |

A deployment's page in the [console](console.md#deployments) shows where it
stands against its spend and queue limits.
