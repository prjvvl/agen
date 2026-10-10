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
| Any task or call to a `singleton` without a key | The deployment's one long-lived conversation. |
| A2A `message/send` | One per `contextId` and caller. A message without `contextId` gets a new one, returned in the answer; send it with the next message to continue. |
| A delegated call (`call_agent`) | A new one for each call. |

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
  A2A caller), so one source can never read another's conversation.
- A run that is still unfinished blocks its conversation. If its task is
  cancelled or fails, the run is closed so the next task can start.

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
keep long-lived conversations focused, or start a new key.
