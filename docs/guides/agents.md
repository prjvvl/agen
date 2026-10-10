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
  "limits": { "maxDelegationDepth": 3, "maxFanOut": 10, "maxTotalDelegations": 50 }
}
```

The agent then has one `call_agent` tool with arguments `agent` (one of the
names) and `message`. The descriptions are shown to the model, so write them
for it. `agen deploy --validate` warns about a delegate that is not deployed
in the namespace.

- Delegates are resolved in the caller's namespace; a delegate with a fixed
  `url` is called at that address instead.
- A call wakes a sleeping delegate, waits for its answer and returns the
  answer as the tool result.
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

## Limits

| Limit | Checked by | Meaning |
|---|---|---|
| `maxDelegationDepth` | the receiving deployment | How deep a chain of delegations may go. |
| `maxFanOut` | the calling engine | Delegated calls per run. |
| `maxTotalDelegations` | the calling engine (shared counter) | Delegated calls in a whole tree, from its root run. |

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
