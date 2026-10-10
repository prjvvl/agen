# Triggers

Triggers submit tasks without anyone calling the API: on a schedule (cron) or
when another system calls a webhook. Each firing becomes a durable task, so a
trigger that fires while the agent is asleep or no Nest is up still runs once
it can.

```json
{
  "triggers": [
    { "type": "cron", "name": "daily-report", "schedule": "0 9 * * 1-5", "input": "Write today's report.", "labels": { "team": "ops" } },
    { "type": "webhook", "name": "deploy", "input": "A deploy happened:" }
  ]
}
```

`agen triggers <name>` lists every firing: `fired` (with its task),
`missed` (a cron window while the Hub was down) and `rejected` (a webhook call
with bad credentials).

## Cron

- `schedule` is a 5-field cron expression or a descriptor (`@hourly`,
  `@every 30s`), in UTC unless it starts with `CRON_TZ=<zone>`.
- Each window fires exactly once. A window the Hub was not running for is
  recorded as `missed`; with `"catchUp": true` the latest missed window fires
  once when the Hub is back.
- A trigger added later never back-fills windows from before it existed.

## Webhooks

A webhook trigger is called at `POST /hooks/<namespace>/<deployment>/<trigger>`
on the Hub. The request body (up to 1 MiB) becomes the task input, after the
trigger's `input`. The Hub answers `202` with `{"task_id": "..."}`.

### Authentication

By default the caller presents the trigger's secret as a bearer token:

```sh
agen webhook-secret my-agent deploy     # prints the secret and the path (rotates it if run again)
curl -X POST -H "Authorization: Bearer $SECRET" --data '{"sha":"abc"}' \
  https://hub.example/hooks/default/my-agent/deploy
```

Services that sign their webhooks instead (GitHub, Stripe-style, Shopify) use
`hmac`: the Hub checks a signature of the raw body made with a secret you
share with the sender, stored as a platform secret:

```json
{
  "type": "webhook",
  "name": "github",
  "auth": {
    "type": "hmac",
    "header": "X-Hub-Signature-256",
    "prefix": "sha256=",
    "algorithm": "sha256",
    "secret": "GITHUB_WEBHOOK_SECRET"
  },
  "idempotencyKey": { "header": "X-GitHub-Delivery" },
  "conversationKey": { "field": "issue.number" }
}
```

```sh
printf %s "$GITHUB_WEBHOOK_SECRET" | agen secret set GITHUB_WEBHOOK_SECRET --for my-agent
```

`algorithm` is `sha256` (default) or `sha1`; `encoding` is `hex` (default) or
`base64`. Only the configured header is checked, so replay protection comes
from the idempotency key: configure one.

### Duplicate deliveries

Senders retry. A repeated delivery with the same idempotency key returns the
task of the first one instead of creating another. By default the key is the
`Idempotency-Key` header; `idempotencyKey` names another header (`"header"`)
or a field of a JSON body (`"field"`, dots reach nested fields, e.g.
`"event.id"`).

### Conversations

`conversationKey` (a header or a body field, like `idempotencyKey`) makes
webhook tasks with the same value continue one conversation, for example one
per issue or ticket. See [Memory](memory.md).

### Labels

`labels` on a trigger are set on every task it submits, so its work can be
told apart in tasks, runs, traces and tool calls.
