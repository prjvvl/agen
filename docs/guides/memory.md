# Memory and conversations

An agent remembers what is in its conversation: the messages of earlier runs
in the same conversation are sent to the model with the new input (oldest
turns are dropped first when they no longer fit the context budget). Which
conversation a run joins depends on how it was started.

## Fleet

| How the work arrives | Conversation |
|---|---|
| Task without a conversation key (`agen run`, `SubmitTask`, a trigger) on a `pool` or `task` deployment | A new one for each task. |
| Task with a **conversation key** | One per key and deployment: every task with the same key continues it. |
| Any task or call to a `singleton` | The deployment's one long-lived conversation (keys and `contextId` are ignored). |
| A2A `message/send` to a `pool` or `task` deployment | One per `contextId` and caller. A message without `contextId` gets a new one, returned in the answer; send it with the next message to continue. |
| A delegated call (`call_agent`) | A new one for each call. |
| A task started with `start_agent` | A new one for the task; its result comes back as a task in the caller's conversation (see [Connecting agents](agents.md#starting-work-without-waiting)). |

Set the key per task:

```sh
agen run support "My order hasn't arrived" --conversation ticket-4711
agen run support "It was order 1234" --conversation ticket-4711
```

or with `conversationKey` on `SubmitTask`, or per webhook trigger from a header
or body field (see [Triggers](triggers.md#conversations)).

- Tasks of one conversation run one at a time, in the order they were
  submitted, so the history stays consistent. Different keys run in parallel.
- Keys are scoped by where they come from (API, each webhook trigger, each
  A2A caller), so one source can never read another's conversation. A task
  shows its key with that scope (`api:ticket-4711`); sending that value back
  to `SubmitTask` continues the same conversation.
- A run that is still unfinished blocks its conversation. If its task is
  cancelled or fails, the run is closed so the next task can start.

### Queue modes for a busy conversation

"One at a time, in order" (above) is `followup`, a deployment's default.
`x-agen/config.json`'s `conversations.queueMode` changes it:

```json
{ "conversations": { "queueMode": "collect", "debounce": "2s" } }
```

- `collect`: a task submitted while an earlier one of the same
  conversation is still queued (not yet leased) is merged into it instead
  of queuing separately — its input is appended, and the merged task's
  `not_before` moves `debounce` (default 2s) further out, so a burst of
  quick messages (someone typing, a flaky webhook retried) becomes one
  run instead of several back-to-back ones. `SubmitTask`'s response is
  the task it merged into, not a new one; keep using that id.
  `debounce` only resets while the task is still queued — once it is
  leased, the next submission starts a fresh one.
- Not yet implemented: `steer` (delivered into an already-running run's
  inbox, picked up before its next model call) and `interrupt` (the
  running run gets a last report turn, then the new task runs);
  `SubmitTask.delivery` to ask for a mode per task rather than per
  deployment; and `collect` is not idempotency-key aware — a merged
  submission's own key is not recorded, so retrying it with the same key
  merges again rather than returning the earlier result.

## Labels

Labels are not memory, but they group work the same way you might use a
conversation key: `--label project=apollo` (or `labels` on `SubmitTask`, A2A
metadata `agen.labels`, or a trigger) is copied to the task's runs, traces,
tool calls (`_meta`) and delegated tasks. Up to 32 labels; keys of letters,
digits and `._/-`.

## Embedded

With an SDK, one `Agent` object is one conversation: each `run` continues it.
Pass a session id to continue a stored conversation later, or ask for a new
conversation; see the SDK README for the exact options.

## What is stored

Sessions, conversations, messages, runs and traces live in the Store
(SQLite locally, Postgres in a distributed fleet). Secrets are redacted before
anything is stored. There is no automatic summarisation of long histories yet:
keep long-lived conversations focused, or start a new key. Within one long
run, the oldest tool results are shortened in what is sent to the model once
the run outgrows `contextTokens`; the Store keeps them whole (see the
[bundle reference](../reference/bundle.md#x-agenharnessjson)).

## Memory blocks

A conversation's history is per-conversation; a **memory block** is durable,
named text that outlives any one conversation and that an agent can update
on its own — a running summary, a user's preferences, a project's open
questions. Blocks are namespace-scoped, so a namespace's deployments can
share one.

Attach one in `x-agen/config.json`:

```json
{ "memory": [{ "block": "user-prefs", "mode": "write" }] }
```

- `mode` gates what the agent may do with it: `read` (see it only, the
  default), `append` (add to it), or `write` (also replace it or edit it in
  place).
- An attached block's current value is rendered into the system prompt as
  `## Memory: <label> (<description>)`; one never written yet renders as
  nothing.
- With at least one non-`read` attachment, the agent gets a `memory_update`
  tool: `op` `replace` (the given `text`), `append` (adds `text`), or
  `str_replace` (`old`, which must occur exactly once in the block, becomes
  `new`). Pass `expectedVersion` (the block's last-seen version) to refuse
  the update instead of silently overwriting a concurrent change from
  another conversation.

People manage blocks directly, without an agent:

```sh
printf %s "Prefers metric units; working in Go" | agen memory set user-prefs --title "User prefs"
agen memory ls
```

`agen memory set` (`SetMemoryBlock`) always replaces a block's value
outright and bumps its version; it is for people, not for an agent's own
updates (which go through `memory_update` so a stale read cannot clobber a
newer write).
