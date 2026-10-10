# Budgets

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

Other limits that bound cost: `maxTurns` in `x-agen/agent.md` (model calls per
run, default 16), and the delegation limits in
[Connecting agents](agents.md#limits).
