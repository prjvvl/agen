# Connecting agents

An agent hands work to another agent by calling its deployment over A2A.
There are no hidden sub-agents: every agent it can call is a deployment you
can see, scale and trace on its own.

## Delegates

List the deployments an agent may call in its `x-agen/config.json`:

```json
{
  "delegates": [
    { "name": "researcher", "description": "Reads web pages and answers factual questions" },
    { "name": "writer", "description": "Turns notes into a polished summary" }
  ],
  "limits": { "maxDelegationDepth": 2, "maxFanOut": 10, "maxTotalDelegations": 30 }
}
```

The agent then has a `call_agent` tool with arguments `agent` (one of the
names) and `message`, and, in a fleet, a `start_agent` tool with the same
arguments (see [below](#starting-work-without-waiting)). The descriptions are
shown to the model, so write them for it. `agen deploy --validate` warns about
a delegate that is not deployed in the namespace.

- Delegates are resolved in the caller's namespace; a delegate with a fixed
  `url` is called at that address instead.
- A call wakes a sleeping delegate, waits for its answer and returns the
  answer as the tool result. It waits up to 10 minutes for a free instance
  of the delegate (asleep, starting, busy, or waiting for room on a full
  Nest) and up to 75 minutes in all.
- Repeating a call is safe: its A2A message id is derived from the run, the
  turn and the call, so a retried or resumed call returns the same task
  instead of running the delegate twice.
- The delegate's run joins the caller's trace (`agen trace` shows the whole
  tree) and inherits the task's labels.
- The delegate's run is a fresh conversation each time; give it the context it
  needs in the message.

## Sending work to several agents at once

When the model calls `call_agent` several times in one turn, the calls run at
the same time and the agent continues when all have answered. Ask for it in
the system prompt, for example: "When a question has independent parts, ask
the researcher about all of them in the same turn."

## Starting work without waiting

`call_agent` keeps the caller's run open until the delegate answers. For long
work, `start_agent` queues a task on the delegate and returns at once with the
task's id; the caller finishes its turn. When the task ends (succeeded,
failed or cancelled), the Hub sends its result back into the caller's
conversation as a new task, with input that starts
`[Result of the task you started on <agent> (task <id>, <state>)]`, and the
caller continues that conversation with it.

| | `call_agent` | `start_agent` |
|---|---|---|
| The caller's run | waits for the answer | goes on and can finish |
| The answer | the tool result | a new task in the caller's conversation (source `reply:<agent>`) |
| The delegate's work | an A2A call | a queued task |
| Which delegates | all | in a fleet, those without a fixed `url` |

- The result goes to the conversation the call came from. If that
  conversation has no key (for example a task without `--conversation`), it
  gets one (`async:<session>`) so the result can find it.
- Repeating a call is safe: a retried or resumed call returns the same task.
- `start_agent` counts against the same limits as `call_agent`.
- The started task joins the caller's trace; the result starts a trace of its
  own. The console's fleet map draws started work as a dashed line and
  results going back as a dotted one.
- A failed or cancelled task's result says so, so tell the caller in its
  system prompt what to do then.

A run started over A2A (no task of its own) can start tasks too: the Hub
accepts its unfinished run as the parent and derives the depth from its
lineage.

## Limits

| Limit | Default | Checked by | Meaning |
|---|---|---|---|
| `maxDelegationDepth` | 3 | the receiving deployment | How deep a chain of delegations may go. |
| `maxFanOut` | 20 | the calling engine | Delegated calls per run. |
| `maxTotalDelegations` | 50 | the calling engine (shared counter) | Delegated calls in a whole tree, from its root run. |

Set a limit to `0` to turn it off. `call_agent` and `start_agent` count
alike.

Each delegate's own budget still applies to its runs.

## Calling an agent from outside

Every deployment is an A2A agent:

```sh
agen call researcher "What is the title of https://example.com?"
agen resolve researcher
```

`agen resolve` prints the A2A endpoints and a short-lived call token for
other A2A clients. To continue a conversation over A2A, send the `contextId`
of the previous answer with the next message (see [Memory](memory.md)).
