# Bundle reference

A bundle is a directory. Every file has a JSON Schema in
[`spec/bundle/`](https://github.com/prjvvl/agen/tree/main/spec/bundle) (also
returned by `GetBundleGuide`), and `agen deploy --validate` checks a bundle
and warns about likely mistakes without deploying it. Unknown fields in
`x-agen/` files are errors.

```text
bundle/
  plugin.json
  mcp.json                 optional
  skills/<name>/SKILL.md   optional
  x-agen/
    agent.md
    harness.json
    config.json            optional
    secrets.json           optional
```

`.git/`, `node_modules/`, `__pycache__/`, `.venv/`, `.DS_Store`, `*~`,
`*.swp` and `*.pyc` are left out of bundles.

## plugin.json

| Field | Type | |
|---|---|---|
| `name` | string, `[a-z0-9][a-z0-9-]{0,62}` | Required. The default deployment name; must match `agent.md`'s `name`. |
| `version`, `description` | string | Informational. Other fields are kept. |

## x-agen/agent.md

YAML frontmatter, then the system prompt.

| Field | Type | |
|---|---|---|
| `name` | string | Required; same as `plugin.json`. |
| `description` | string | Required. Shown to people and to other agents. |
| `maxTurns` | 1-1000 | Model calls per run (default 16). |
| `skills` | list of strings | Skills that must exist in `skills/`. |
| `preloadSkills` | list of strings | Skills (from `skills`) whose instructions are always in the system prompt, instead of loaded with `load_skill` when needed. |

```markdown
---
name: coder
description: Makes code changes in a project folder and checks them with the project's own tests
skills: [coding-workflow]
preloadSkills: [coding-workflow]
maxTurns: 80
---

You are a careful software engineer working in one project folder through the
workspace tools ...
```

Preload a skill the agent uses on every run; the others stay a short list
of names and descriptions until the model loads one.

## x-agen/harness.json

| Field | Type | |
|---|---|---|
| `provider` | `openrouter`, `openai`, `fake`, `replay` | Required. `fake` replies from a script and `replay` from a recorded cassette; neither calls a model. |
| `model` | string | Required, e.g. `deepseek/deepseek-v4-flash`, or a routing model (`typesafe/jev-router`, `openrouter/auto`, `openrouter/auto-beta`). |
| `fallbackModels` | list of strings, at most 5 | Models tried in order when the model keeps failing (after retries) or gives no answer. Never used for account errors. |
| `reasoningEffort` | `minimal`, `low`, `medium`, `high` | For models that reason (OpenRouter `reasoning.effort`, OpenAI `reasoning_effort`). |
| `router` | object | Routing models only: `tier`, `maxTier`, `budgetTier` (`low`, `medium`, `high`, `xhigh`, `max`, as far as the router offers), `allowedModels`, `excludedModels`. |
| `openrouter` | object | Provider `openrouter` only: `provider` (OpenRouter provider routing) and `plugins` (each with an `id`), sent with every request as given. |
| `temperature` | 0-2 | |
| `maxOutputTokens` | integer | Output limit per model call; lowered further near the token budget. |
| `contextTokens` | integer, at least 4000 | About how many tokens of conversation one model call may send (default 64000). See below. |
| `parallelToolCalls` | boolean | Run the tool calls of one turn at the same time (default `true`). |
| `traceToolArguments` | boolean | Record tool call arguments (redacted, up to 4 KiB) in traces (default `false`). |
| `baseUrl` | string | An OpenAI-compatible endpoint. |
| `apiKeySecret` | string | The secret holding the API key, if not the provider's default (`OPENROUTER_API_KEY`, `OPENAI_API_KEY`). |
| `script` | path | `fake`: the reply script. |
| `cassette` | path | `replay`: the recording. |

```json
{
  "provider": "openrouter",
  "model": "typesafe/jev-router",
  "router": { "tier": "medium", "maxTier": "high", "budgetTier": "low" },
  "fallbackModels": ["deepseek/deepseek-v4-flash"],
  "reasoningEffort": "low",
  "openrouter": { "provider": { "sort": "throughput" } },
  "maxOutputTokens": 8000,
  "contextTokens": 96000
}
```

`router` and `openrouter` are refused on other providers, `router` on a model
that is not a routing model, and tiers the router does not offer. See
[Models and providers](../guides/models.md).

**`contextTokens`**: before each model call, messages of earlier runs in the
conversation are dropped, oldest first, to fit it. If the current run's own
messages exceed it, its oldest tool results are shortened (their start is
kept, with a note saying how much was left out) in blocks of eight, until the
run fits in about two thirds of the budget. Short results and the latest four
long ones stay whole, and the Store and the transcript keep every result in
full. Shortening in blocks means the start of the request changes rarely,
which helps the provider's prompt cache.

## x-agen/secrets.json

Secret names, never values: `{"NAME": {"source": "env" | "keychain" | "platform", "key": "LOOKUP"}}`.

- `env`: the instance's environment (in a fleet, the Nest's environment or
  `--host-env`).
- `keychain`: reserved for the OS keychain; not supported yet.
- `platform`: the Hub's secret store (`agen secret set NAME --for DEPLOYMENT`).

Values are redacted from logs, traces, stored messages and tool results.

## mcp.json

`{"mcpServers": {"<server>": ...}}`, server names `[A-Za-z0-9_-]{1,32}`.

| Field | |
|---|---|
| `command`, `args`, `env` | A stdio server the instance starts. `${NAME}` in `env` is a secret. |
| `url`, `headers` | A streamable HTTP server. `${NAME}` in headers is a secret. |

See [Giving agents tools](../guides/tools.md).

## x-agen/config.json

| Field | |
|---|---|
| `kind` | `pool` (default), `singleton` or `task`. |
| `scale` | `min`, `max`, `targetQueuePerInstance`, `idleTimeout` (`30s`, `5m`, ...), `maxConcurrency` (tasks per instance). |
| `budget` | `maxTokensPerRun`, `maxUsdPerRun`, `maxUsdPerDay`. See [Budgets and limits](../guides/budgets.md). |
| `limits` | `maxRunDuration`, `maxIdenticalToolCalls`, `modelRequestTimeout`, `toolTimeout`, `maxQueuedTasks`, `maxDelegationDepth`, `maxFanOut`, `maxTotalDelegations`, each with a default. See [Budgets and limits](../guides/budgets.md#limits). |
| `permissions` | `default` (`allow`, `ask`, `deny`; default `ask`), `rules` (`[{"tool", "action"}]`), `approvalTimeout`, `notify` (`url`, `secret`). See [Permissions](../guides/permissions.md). |
| `triggers` | Cron and webhook triggers. See [Triggers](../guides/triggers.md). |
| `delegates` | `[{"name", "namespace", "url", "description"}]`: deployments the agent may call. |
| `tools` | Per tool (`"<server>.<tool>"`): `sideEffect`, `idempotent`. See [Giving agents tools](../guides/tools.md#side-effects-and-retries). |
| `workspace` | Reserved. |

Deploy-time settings (`kind`, `scale`, `budget`, `limits`, `triggers`) can be
overridden per deployment through `CreateDeployment` / `UpdateDeployment`;
the bundle stays the source of truth when a new version is deployed.
