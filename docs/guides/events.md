# Events

The Hub keeps an **event log**: one entry per notable change, in the order
the changes happened. Read it to learn what happened without polling tasks
one by one: from the API (`ListEvents`), over MCP (`list_events`), with
`agen events`, or as a stream (`GET /api/v1/events`).

| Type | When | Data |
|---|---|---|
| `task.queued` | A task was submitted (API, trigger, delegation, reply). | `source`, `notBeforeMs` |
| `task.started` | An instance took it. | `instanceId`, `attempt` |
| `task.finished` | It ended: succeeded, failed or cancelled. | `state`, `outcome` (status, reason, inputFrom, a short summary), `error`, `runId` |
| `task.needs_input` | It ended asking for an answer (outcome `needs_input`). | `summary`, `inputFrom` |
| `approval.pending` | A run waits for an approval. | `approvalId`, `tool`, `runId`, `requestedBy` |
| `approval.decided` | It was approved, denied or expired. | the same, `state`, `decidedBy` |
| `provider.held` / `provider.resumed` | A provider account was held after an account error, or works again. | `account`, `reason` |
| `task.stuck` | A running task made no progress for a while (`alerts.quietRun`, default 5 min). | `runId`, `message` |
| `budget.threshold` | A deployment spent 50, 80 or 100% of its daily budget (each once per UTC day). | `percent`, `spentUsd`, `limitUsd` |
| `activity.warning` | A warning from `GetActivity` (quiet run, long approval wait, queue waiting, a burst, model errors, answers cut at the output limit, ...), each once per subject and day. | `kind`, `message`, `runId` |
| `trigger.missed` / `webhook.rejected` | A cron window was missed, or a webhook call was refused. | `trigger`, `message` |
| `deployment.created` / `.updated` / `.deleted` | A deployment changed through the API (not autoscaling). | `digest`, `paused` |

A deployment's `x-agen/config.json` can set when it warns:

```json
{ "alerts": { "quietRun": "15m", "waitingApproval": "2h", "queueWaiting": "10m" } }
```

[Notification targets](notifications.md) receive these events as signed
webhooks.

Every event has a `seq` (its place in the log), the namespace, deployment,
task and parent task it is about, the task's labels, a `severity` (`info`,
`warning` or `critical`) and its time. Events are kept for 7 days.

## Reading the log

```sh
agen events                              # the latest events in the namespace
agen events researcher --type task.      # task events of one deployment
agen events --severity warning --label project=apollo
agen events -f                           # keep printing new events
```

`ListEvents` takes `after_seq` (a cursor: only events after it), `types`
(exact, or a prefix ending in `.`), `deployment`, `labels`, `severity`,
`limit` and `wait_seconds` (up to 300: wait for the first matching event).
It returns the events and `next_seq`, the cursor for the next call; it moves
on past events that did not match, so a filtered reader never reads the same
stretch twice.

```sh
curl -s "$HUB/agen.v1.HubService/ListEvents" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"afterSeq": 1040, "types": ["task.finished"], "waitSeconds": 60}'
```

A token sees the events of its namespaces; approval events need the
`approver` scope, as approvals do.

## The stream

`GET /api/v1/events` is a server-sent event stream. Each log event arrives as

```text
id: 1041
event: task.finished
data: {"seq": "1041", "namespace": "default", "type": "task.finished", ...}
```

A client that reconnects with `Last-Event-ID: 1041` (browsers do this on
their own) or `?after=1041` gets every event after it, so nothing is lost.
Without a cursor the stream starts at the newest event. `?type=`,
`?deployment=`, `?label=k=v` and `?severity=` filter it. The stream also
carries `change` messages, which the console uses to refresh what it shows;
`?kinds=log` leaves them out (`?kinds=changes` sends only those).

Events are written in the same transaction as the change they report, and
their seq follows commit order, so a reader following the cursor sees each
event exactly once.
