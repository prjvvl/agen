# Models and providers

An agent's model is set in its bundle's `x-agen/harness.json`. This page
covers choosing a provider and model, routing models and their cost tiers,
fallback models, reasoning effort, OpenRouter request settings, and what
happens when a provider account stops working.

## Providers

| `provider` | Calls | API key (default secret) |
|---|---|---|
| `openrouter` | [OpenRouter](https://openrouter.ai), any model it lists. Reports cost per call. | `OPENROUTER_API_KEY` |
| `openai` | An OpenAI-compatible API: OpenAI itself, or another endpoint with `baseUrl`. Reports no cost. | `OPENAI_API_KEY` |
| `fake` | Nothing: replies from a script in the bundle. | none |
| `replay` | Nothing: replays a recorded cassette. | none |

```json
{
  "provider": "openrouter",
  "model": "deepseek/deepseek-v4-flash",
  "maxOutputTokens": 4000
}
```

`apiKeySecret` names another secret for the key; declare it in
`x-agen/secrets.json`. Every field is listed in the
[bundle reference](../reference/bundle.md#x-agenharnessjson).

Each model call is a span in the run's trace. It records the model that
actually served the call as `gen_ai.response.model`, which can differ from the
one requested (a routing model, a provider alias or a fallback). The console's
**Costs** page groups calls by that model.

## Routing models and tiers

A routing model picks the serving model for each request. Agen knows three,
all on OpenRouter:

| `model` | Plugin id | Tiers |
|---|---|---|
| `typesafe/jev-router` | `jev-router` | `low`, `medium`, `high` |
| `openrouter/auto` | `auto-router` | `low`, `medium`, `high`, `xhigh`, `max` |
| `openrouter/auto-beta` | `auto-beta-router` | `low`, `medium`, `high`, `xhigh`, `max` |

Configure the router in `router`; Agen sends it as the router's plugin entry:

```json
{
  "provider": "openrouter",
  "model": "typesafe/jev-router",
  "router": {
    "tier": "medium",
    "maxTier": "high",
    "budgetTier": "low",
    "allowedModels": ["deepseek/*", "z-ai/*"],
    "excludedModels": ["openai/*"]
  },
  "fallbackModels": ["deepseek/deepseek-v4-flash"]
}
```

| Field | Meaning |
|---|---|
| `tier` | The default cost tier. Without one, the router chooses. |
| `maxTier` | The highest tier a task may ask for; higher requests are lowered to it. |
| `budgetTier` | The tier used once the deployment has spent 80% of its daily budget (default `low`). See [Budgets](budgets.md#budget-pressure). |
| `allowedModels`, `excludedModels` | Model patterns the router may or may not pick, such as `deepseek/*`. |

A tier is a band, not a cap: the router picks among the models of that band,
so a lower tier usually costs less but is not guaranteed to. Set budgets for
hard limits.

`agen deploy --validate` (and the engine at start) refuses `router` on a model
that is not a routing model, a tier the router does not offer (for example
`xhigh` on `typesafe/jev-router`), a `tier` above `maxTier`, and the router's
own plugin in `openrouter.plugins` (configure it in `router` instead).

### Choosing a tier per task

A task can ask for a tier, within `maxTier`:

```sh
agen run coder "Rename the config loader and fix its tests" --tier high
```

`SubmitTask` takes `modelTier`, and an A2A message takes the metadata key
`agen.model_tier`. A tier on a task whose agent has no routing model is
ignored. The console's run form has a tier field too.

### What the router decided

Besides the served model, the model-call span records the tier sent
(`agen.router.tier`) and, when the router reports it, its decision:
`agen.router.task` (the kind of request it saw), `agen.router.reason`,
`agen.router.effort` and `agen.router.summary`, among others. The trace
inspector lists them under the call's attributes.

## Fallback models

`fallbackModels` (up to five) are tried in order when the model fails:

- A model call is retried first: rate limits (429) and server errors back
  off and try again, up to 5 times. If it still fails, or fails with an error
  that is not retried, the next model in the list is called.
- An empty answer (no text and no tool calls) is asked again once; if it is
  empty again, the next model is called.
- If every model gives no answer, the run fails with `no_answer`. A
  `finish_reason` of `length` there usually means reasoning used the whole
  output limit: raise `maxOutputTokens`.
- Account errors are never failed over (below).

The engine logs each switch (`model X failed (...); trying Y`), and the span of
a call served by a fallback records the model sent (`gen_ai.request.model`)
and `agen.fallback`. Fallbacks use the same provider and key as the primary
model.

## Reasoning effort

`reasoningEffort` (`minimal`, `low`, `medium`, `high`) is sent to models that
reason: as `reasoning.effort` on OpenRouter and `reasoning_effort` on
`openai`. A routing model may choose the effort itself; the span records it as
`agen.router.effort` when it reports one.

## OpenRouter request settings

`openrouter` (provider `openrouter` only) is sent with every request as given:

```json
{
  "provider": "openrouter",
  "model": "deepseek/deepseek-v4-flash",
  "openrouter": {
    "provider": { "sort": "throughput", "ignore": ["some-provider"], "zdr": true },
    "plugins": [{ "id": "web" }]
  }
}
```

- `provider`: OpenRouter's provider routing (order, ignore, sort, data
  policy and so on).
- `plugins`: OpenRouter plugins; each needs an `id`.

See OpenRouter's documentation for the values. Agen does not check them.

## Account errors and provider holds

A provider answer of 401, 402 or 403 (an invalid or expired key, no credit, a
key limit reached) is an **account error**. It is not retried and no fallback
model is tried, because another model on the same account will not help. The
task fails with `provider account error: ...`. A 403 because moderation
flagged the input is about that request only: it fails over like other
request errors and does not hold the account.

The Hub then **holds** the provider account: the namespace, the provider and
the name of its key secret (plus `baseUrl` if set), such as
`default/openrouter:OPENROUTER_API_KEY`. Every deployment of that namespace
that uses that account:

- takes no new tasks; queued work waits instead of failing one task at a time,
- does not scale up for queued work and, until the next try, is not woken
  for A2A calls (callers get an error naming the hold and the next try),
- shows **provider held** in the console and in `agen ps` (`providerHold` on
  the deployment in the API).

After a backoff (5 minutes, doubling with each failed probe up to 1 hour), one
probe task goes through. Any run of the account that succeeds after the hold
began (a task or an A2A call) ends it. To end it at once, for example after
adding credit or fixing the key:

```sh
agen providers                                      # held accounts, the reason and the next try
agen providers resume default/openrouter:OPENROUTER_API_KEY # admin
```

The API has `ListProviderHolds` and `ResumeProvider`; the console lists held
accounts on the **Costs** page with **Resume**. A new hold sends
`provider.held`, and a hold that a successful run ends sends
`provider.resumed`, to [notification targets](notifications.md) that take
every event.
