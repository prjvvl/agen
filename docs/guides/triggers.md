# Triggers

Triggers submit tasks without anyone calling the API: on a schedule (cron),
when another system calls a webhook, or when something happens in the fleet
(an event). Each firing becomes a durable task, so a
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
with bad credentials). For one task at a later time, without a trigger, see
[One-off tasks at a later time](#one-off-tasks-at-a-later-time).

## Cron

- `schedule` is a 5-field cron expression or a descriptor (`@hourly`,
  `@every 30s`), in UTC unless it starts with `CRON_TZ=<zone>`.
- Each window fires exactly once. A window the Hub was not running for is
  recorded as `missed`; with `"catchUp": true` the latest missed window fires
  once when the Hub is back.
- A trigger added later never back-fills windows from before it existed.
- The API returns each cron trigger with `nextDueAt` and `lastDueAt` (on
  `GetDeployment` and `ListDeployments`), computed the way the scheduler
  fires them.

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

## Events

An `event` trigger submits a task when the namespace's [event log](events.md)
records a matching event, so an agent can react to other agents' work
without polling:

```json
{
  "type": "event", "name": "failures",
  "input": "A task failed. Find out why and report.",
  "on": { "types": ["task.finished"], "deployment": "researcher", "labels": { "project": "apollo" } },
  "conversation": "{{event.labels.ticket}}",
  "labels": { "via": "failures" }
}
```

- `on` picks the events: `types` (event types, or prefixes such as
  `task.`), and optionally `deployment`, `labels` and `severity`.
- The task's input is the trigger's `input`, then `[Events from trigger
  <name>]` and each event as one line of JSON (type, deployment, task id,
  labels, data such as the task's state and outcome).
- The task gets the event's labels, then the trigger's own.
- `conversation` takes the task's conversation key from the event:
  `{{event.taskId}}`, `{{event.parentTaskId}}`, `{{event.deployment}}`,
  `{{event.type}}` or `{{event.labels.<key>}}`. Events with the same value
  continue one conversation.
- With `"count": 5, "within": "10m"` in `on`, the trigger fires once five
  matching events arrived within ten minutes, with all of them in one task.
- A new trigger starts with the events after it was deployed. Each event
  fires at most once (`agen triggers <name>` lists the firings).
- While the deployment's provider account is held, its event triggers wait,
  and fire for the waiting events once the hold ends.
- A trigger never reacts to the tasks it submitted itself, and when event
  triggers lead from task to task five times in a row, the next firing is
  refused and recorded as `rejected`, so triggers that set each other off
  stop.

## One-off tasks at a later time

A task can wait for a start time instead of a trigger:

```sh
agen run reporter "Summarize this week's incidents" --at 2026-10-19T09:00:00Z --no-wait
agen run reporter "Check that the migration finished" --in 30m --no-wait
```

`SubmitTask` takes `notBefore`, at most 90 days ahead. Until then the task is
`queued` but not due: it is not leased, does not wake or scale up the
deployment, and does not hold up other tasks of its conversation. It still
counts towards `maxQueuedTasks`. `ListTasks` with `scheduled` lists the tasks
waiting for their start time, soonest first (`agen tasks --scheduled`);
cancel one with `CancelTask` (`agen cancel <task>`).

## The Schedules page

The console's **Schedules** page shows everything that starts work on its
own:

- **Recurring**: cron triggers, their schedule in words, the next and last
  run, whether they catch up, recent misses, and **Run now** (submits the
  trigger's task at once).
- **One-off tasks waiting for their start time**, soonest first, with their
  model tier and **Cancel**.
- **Webhooks**: path, authentication, the last delivery and refusals, and
  **New secret** (creates or rotates the bearer secret; the old one stops
  working).
- **Missed and refused** trigger events.

Schedules that missed a window and webhooks that refused calls in the last
day also appear as warnings on the Overview.
