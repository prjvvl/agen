# The console

The Hub serves a web console at its address (`http://127.0.0.1:7070/` with
`agen up`). `agen ui` prints a sign-in link; any API token works, and the
console shows and allows what the token's scopes and namespaces allow.

```sh
agen ui
```

The console updates live: the Hub streams changes (tasks, runs, approvals,
instances, deployments) to it, so lists and traces change as work happens.
Press `Ctrl+K` anywhere to jump to a page or a deployment, or paste a trace or
task id.

## Overview

The first page answers "what is happening, and does anything need me?":

- **Needs attention**: approvals waiting for a decision, deployments with
  failed runs in the last 24 hours, daily budgets that ran out, instances or
  tool servers that failed, and deployments waiting for room because every
  Nest is full. Each item links to what to do about it.
- **Fleet map**: every deployment and who calls whom (delegations in the last
  24 hours, with call and failure counts). Agents that are working right now
  are outlined.
- **Activity**: tasks as they are queued, run and finish.
- **Latest traces**.

## Inbox

Approvals that agents wait for, with the tool, its arguments, who asked and
what the agent was doing (from its transcript), and tasks that failed since
your last visit. The person whose work asked for an approval cannot decide it
(see [Permissions](permissions.md)).

## Runs and traces

**Runs** lists one row per trace by default: the run a task or call started,
with the tokens, cost and number of agents of everything it delegated. Filter
by deployment, status, time and label; switch to **All runs** to see
delegated runs too.

Opening a run shows its trace:

- **Timeline**: every span across agents as a tree with bars on a shared time
  axis: agent runs, model calls, tool calls, delegated calls (with the
  callee's run nested under them) and approval waits. Spans that are still
  running show as they happen. Find a span by name, or show only failures;
  the arrow keys move through the tree.
- **Inspector**: select a span to see what happened in it. A model call shows
  the messages it sent and the reply it got (text and tool calls), its
  tokens, cost and finish reason. A tool call shows its arguments and its
  result, the permission decision and whether a side effect was reused. A run
  shows its input, output or error, usage and labels.
- **Transcript**: a run's whole conversation, tool calls with their results,
  and earlier messages of the conversation it continued.

**Explain** asks the [assistant](#assistant) to walk through the trace.

The same data is in the API and CLI: see [Traces](traces.md).

## Deployments

A deployment's page shows its last 24 hours (runs, failures, duration, tokens,
cost), where it stands against its limits (spend today, queued tasks,
instances), its instances with their tool servers, and tabs for runs, tasks,
logs (live), the bundle's files and triggers. **Run a task** submits one, with
an optional conversation key and labels.

## Costs

Spend per deployment over 24 hours, 7 or 30 days, with runs, failures,
duration percentiles and tokens, and spend over time. Cost needs a provider
that reports it (OpenRouter does).

## Templates

Ready-made agents to start from: a scripted hello agent, a web researcher, a
writer, an editorial team that delegates to both, support triage, a pull
request reviewer, and operations agents (fleet steward, cost watchdog,
approval triage). **Use** opens a form for the name, model and instructions,
sets the secrets the template needs and deploys it. See
[Templates](templates.md).

## Assistant

The assistant is an agent that works for the person chatting with it: it
answers questions about the fleet and makes changes, through the Hub's MCP
tools, with that person's permissions. Before a change it checks it
(`validateOnly` where there is one), says what it will do and waits for a yes.

An admin sets it up from **Templates → Console assistant**. That deploys the
`assistant` template and gives it a token created with `--on-behalf`: such a
token only acts for the submitter of the task it works on, never has more
scopes or namespaces than that person, never has admin rights and can never
decide approvals. Each answer links to its trace.

Drag the panel's left edge to widen it. **History** lists your past chats in
this browser; opening one continues that conversation, since the assistant
remembers each by its conversation key.

## Settings

Your token's identity and scopes; for admins, API tokens, platform secrets
and [notification targets](notifications.md).
