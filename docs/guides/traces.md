# Traces

Every run records what it did: spans for the run itself, each model call,
each tool call, approval waits and delegated calls (the callee's run joins
the caller's trace), and the conversation's messages, tool calls and results
included. The [console](console.md#runs-and-traces) shows them as a timeline
with an inspector; the CLI and API return the same data.

## From the CLI

```sh
agen runs                       # recent runs
agen runs --roots --status failed
agen runs researcher --label team=ops
agen trace <task-id|trace-id>   # the span tree across agents
agen transcript <task-id|run-id> --history --system
```

`agen runs --roots` lists one row per trace, with the tokens and cost of the
whole run tree.

## From the API

| Call | Returns |
|---|---|
| `ListRuns` | Runs, newest first. Filters: `status`, `labels`, `since`, `taskId`; `rootsOnly` for one row per trace with `treeUsage` and `treeRuns`; paged with `pageToken`. |
| `GetTrace` | Every span and run of a trace. |
| `GetTranscript` | A run's messages (role, content, tool calls with arguments, tool results), with `includeHistory` the earlier messages of its conversation, and the system prompt of its bundle version. |
| `GetMetrics` | Per deployment over a window: runs, failures, p50/p95 duration, tokens, cost, per-bucket counts, and the delegation edges between deployments. |

A model-call span (`gen_ai.chat`) carries `agen.message.seq`, the position of
its reply in the conversation, and `agen.request.messages`, how many messages
it sent; a tool span (`agen.tool`) carries `gen_ai.tool.call.id`, which the
tool's result message answers. That is how a span maps to its messages.

Messages are stored with secret values redacted, like logs and spans. Any
token that may read a namespace may read its transcripts.

## Live changes

`GET /api/v1/events` (bearer token, viewer scope) is a server-sent event
stream of changes in the namespaces the token may see:

```text
event: change
data: {"kind":"run","id":"01J...","namespace":"default","deployment":"researcher","state":"running","traceId":"4bf9..."}
```

`kind` is `task`, `run`, `approval` (only with the approver scope),
`instance` or `deployment`; `?namespace=` narrows the stream. Events say what
changed, not everything about it: fetch the details with the API. A client
that reconnects should refetch what it shows.

## Retention

The Hub deletes spans, log lines and trigger events older than 30 days, once
an hour. Change it with `agen hub serve --retention 720h` (or `agen up
--retention`); `0` keeps everything. Runs and messages stay: they are
conversation state that later tasks continue.
