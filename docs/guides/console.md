# The console

The Hub serves a web console at its address (`http://127.0.0.1:7070/` with
`agen up`). `agen ui` prints a sign-in link; any API token works, and the
console shows and allows what the token's scopes and namespaces allow.

```sh
agen ui
```

The console updates live: the Hub streams changes (tasks, runs, model and
tool calls, approvals, instances, deployments) to it, so lists, traces and the
fleet map change as work happens.
Press `Ctrl+K` anywhere to jump to a page or a deployment, or paste a trace or
task id.

## Overview

The first page answers "what is happening, and does anything need me?":

- **Needs attention**: counts by kind and the three most serious items, each
  with what to do about it. Kinds include approvals waiting for a decision,
  deployments with failed runs in the last 24 hours (delegated calls
  included), daily budgets that are nearly or fully used, failed instances or
  tool servers, deployments waiting for room because every Nest is full, and
  live warnings: runs near their turn limit or run budget, runs that have been
  quiet for 5 minutes, long approval waits, queues waiting or blocked behind a
  pending approval, bursts of tasks in one conversation, model error rates,
  held provider accounts, missed schedules and webhooks refusing calls. The
  full list, paged, is in the [Inbox](#inbox).
- **Fleet map**: who calls whom in the last 24 hours.
  - Where work enters (API and CLI, schedules, webhooks, A2A callers outside
    the fleet) is drawn on the left.
  - Calls are solid lines, work started with `start_agent` is dashed, and
    results going back are dotted; an agent calling itself is a loop. Line
    width follows the number of calls and colour the share that failed.
  - Dots travel along a line while a call is in flight (a red diamond when it
    failed), and an agent pulses while it waits for its model.
  - Select an agent or a line for details: what an agent runs now and its
    backlog, or a line's calls, errors, p95 latency and the target's latest
    failures.
  - Above 12 agents the cards get compact and tall columns wrap. Drag to pan,
    `Ctrl` + wheel or the buttons to zoom, and **Fit** to see everything.
    **Find an agent** highlights it and its direct neighbours; agents with
    nothing in 24 hours can be hidden.
- **Now**: every run in progress, nested under the run waiting for it, with
  what it is doing (model, tool, waiting for an agent or an approval) and
  for how long.
- **Activity**: tasks as they are queued, run and finish, grouped by
  conversation.
- **Timeline**: runs per agent over time, to spot overlaps, bursts and long
  runs.
- **Latest traces**.

## Inbox

Approvals that agents wait for, with the tool, its arguments, who asked and
what the agent was doing (from its transcript), and tasks that failed since
your last visit. The person whose work asked for an approval cannot decide it
(see [Permissions](permissions.md)). The inbox also has the full list of
everything that needs attention, paged.

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
cost), where it stands against its limits (spend today, queued tasks with how
many are scheduled for later and how long the oldest has waited, instances),
its instances with their tool servers, and tabs for runs, tasks, logs (live),
the bundle's files and triggers. A deployment whose provider account is held
says **provider held** (see [Models](models.md#account-errors-and-provider-holds)).

- **Run a task** submits one, with an optional conversation key, labels, a
  start time (the task waits until then) and a model tier (for agents on a
  routing model).
- A task's details show its start time, model tier (and whether it ran near
  the daily budget), the task that started it and where an async result
  goes. **Cancel task** cancels one that has not finished.
- The **Bundle** tab lists earlier versions of the bundle. **View** shows a
  version's files; **Roll back to this** deploys that version's files again,
  and instances restart on it.

## Schedules

Everything that starts work on its own: cron triggers with their next and
last run (**Run now**), one-off tasks waiting for their start time
(**Cancel**), webhooks (**New secret**), and missed and refused trigger
events. See [Triggers](triggers.md#the-schedules-page).

## Costs

Spend per deployment over 24 hours, 7 or 30 days, with runs, failures,
duration percentiles and tokens, and spend over time. Cost needs a provider
that reports it (OpenRouter does).

- **Models**: calls by the model that served them (a routing model or a
  fallback may serve another model than the one configured), with failures,
  fallbacks, latency, tokens and cost.
- **Provider accounts**: accounts held after an account error, with the
  reason and the next try, and **Resume** (admin). See
  [Models and providers](models.md#account-errors-and-provider-holds).

## Templates

Ready-made agents to start from: a scripted hello agent, a web researcher, a
writer, an editorial team that delegates to both, a pull request reviewer,
coding, CI, support, incident and data agents, and operations agents (fleet
steward, approval triage). **Use** opens a form for the name, model and
instructions, sets the secrets the template needs and deploys it. See
[Templates](templates.md).

## Fleet

The Nests that run instances, with their capacity and instances. For admins,
**Add a nest** shows the `agen nest run` command to run on the new machine,
with a fresh join token (it works once, for an hour) and, when the Hub uses
TLS, the CA hash to pin; **Revoke** removes a Nest, and its instances move to
other Nests.

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

While it works, the chat lists each step (the tools it calls, with their
arguments); a finished answer keeps them under **Steps**. Drag the panel's
left edge to widen it. **History** lists your past chats in
this browser; opening one continues that conversation, since the assistant
remembers each by its conversation key.

## Settings

Your token's identity and scopes; for admins, API tokens, platform secrets
and [notification targets](notifications.md).
