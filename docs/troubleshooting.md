# Troubleshooting

Start with these three commands; most problems show up in one of them:

```sh
agen ps --all          # instances, their state and how many tools each loaded
agen logs my-agent     # instance starts and exits, runs, task retries
agen trace <task-id>   # every model call, tool call and delegation of a task
```

`agen whoami` shows which token you are using and its scopes.

## Errors

**`... requires the operator scope; this token has: viewer`.** The token lacks
a scope. Create one with it: `agen token create --name ci --scope operator`.
Scopes: `viewer` reads, `operator` deploys and runs tasks, `approver` decides
approvals, `admin` does everything.

**`namespace X is not allowed for this token`.** The token is limited to other
namespaces; pass `-n` with one it allows, or use another token.

**`approval ... was requested by work this token asked for`.** Nobody approves
their own request. Decide it with a different token that has the `approver`
scope.

**`approval ... is no longer pending`.** Someone decided it already, or it
expired (`approvalTimeout`, default 1 h).

**`invalid bundle: ...`.** Every problem is listed with its file. Run
`agen deploy <dir> --validate` while you fix them; the JSON Schemas are in
`spec/bundle/` and in `GetBundleGuide`.

**The agent says it has no tools, or never uses them.**
- `agen ps --all` shows the tool count per instance; 0 means `mcp.json` is
  missing or its servers failed to start (the instance logs say why).
- `--validate` warns about an agent with no tools and about permission rules
  that name servers the bundle does not have.
- `permissions.default` is `ask` when unset: every call waits for an approval
  (`agen approvals`). Allow the tools it may use freely.

**The agent never calls a real model.** `x-agen/harness.json` says
`"provider": "fake"` (scripted replies) or `"replay"`. `--validate` warns
about it.

**`secret OPENROUTER_API_KEY is not set (source env)`.** The instance could
not resolve a declared secret. With `"source": "env"` it must be in the environment of
`agen up` or the Nest (or `--host-env`); with `"source": "platform"` set it
with `agen secret set NAME --for <deployment>`.

**`budget_exceeded`.** The run hit `maxTokensPerRun` or `maxUsdPerRun`; see
[Budgets](guides/budgets.md). A deployment that used its `maxUsdPerDay` takes
no new work until the next UTC day; `agen ps` shows it.

**`max_turns (N) exceeded`.** The agent kept calling tools. Raise `maxTurns` in
`x-agen/agent.md`, or make the instructions say when to stop.

**`effect_unknown`.** A side-effecting tool call was interrupted (timeout,
crash) and may or may not have happened, so Agen did not repeat it. Mark tools
that are safe to repeat as idempotent; see
[Giving agents tools](guides/tools.md#side-effects-and-retries).

**`conversation already has an unfinished run`.** Another run of the same
conversation is still going: tasks with one conversation key run one at a
time and wait for each other. Over A2A, two messages of one `contextId` sent
to different Gateways at once can meet this; send them one at a time, or
resend the earlier message (same `messageId`) to let it finish.

**A task stays `queued`.** The deployment may be paused (`agen start`), out
of daily budget, at `scale.max` with every instance busy, or waiting for an
earlier task of the same conversation key. `agen ps` and `agen tasks` show
which.

**A task was retried.** If an instance dies mid-task, the task is released and
the next attempt resumes the same run on another instance (logged as
`returned to the queue` / `requeued`). After `max_task_attempts` (default 5)
expired leases it fails instead.

**`webhook is misconfigured`.** The trigger's settings are wrong (for example
its signing secret is not set); the reason is in `agen logs <name>`.
**`invalid signature`** or **`invalid or missing webhook secret`**: the caller
signed with another secret or sent the wrong token; rejected calls are listed
by `agen triggers <name>`.

**`get_task` returns before the task finished.** `waitSeconds` waits at most
300 s; call it again.

**Text from a tool server is garbled.** Agen gives tool servers
`PYTHONUTF8=1` and `PYTHONIOENCODING=utf-8`; a server in another language
must write UTF-8 to stdout itself.

## Where things are

- Local fleet: `~/.agen` (`AGEN_HOME`), with the SQLite store, the admin token
  in `local.json` and unpacked bundles under `nest/`.
- Traces, logs and run history are in the Store; nothing is written to log
  files unless you redirect the processes' output.
