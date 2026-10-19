# Troubleshooting

Start with these three commands; most problems show up in one of them:

```sh
agen ps --all          # instances, their state and how many tools each loaded
agen logs my-agent     # instance starts and exits, runs, task retries
agen trace <task-id>   # every model call, tool call and delegation of a task
```

`agen whoami` shows which token you are using and its scopes.

## Installing and starting

**`agen: command not found`** (or `not recognized` on Windows) right after
installing. The terminal predates the `PATH` change: open a new one. If it
persists, `~/.agen/bin` is not on `PATH` (you set `AGEN_NO_MODIFY_PATH=1`, or
your shell reads another profile); add it yourself.

**`unsupported OS` / `unsupported CPU` / `there is no release for Intel Macs`.**
There is no build for that machine yet; [build from source](development.md).

**`checksum mismatch`.** The download was corrupted or altered; run the
installer again. It never installs an archive that fails the check.

**`agen up` fails to listen** (`address already in use`; on Windows `Only one
usage of each socket address`). Another `agen up` (or something else) holds
port 7070 or 7071. Stop it with `agen down`, or start
on other ports: `agen up --listen 127.0.0.1:7170 --gateway-listen 127.0.0.1:7171`.

**`no local agen found`.** A fleet command found no running `agen up`. Start
one, or point the CLI at a Hub with `--hub` and `--token` (or `AGEN_HUB`,
`AGEN_TOKEN`).

**`hello` always answers the same thing.** Its model is scripted (`"provider":
"fake"`); it never calls a real model. Use `agen init --template researcher` for
a real one.

**Start over.** `agen down`, then delete `~/.agen` (keep `~/.agen/bin` if you
want the programs).

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
- `agen ps --all` shows the tool count per instance; 0 means the bundle has
  no `mcp.json` servers. A server that fails to start fails the whole
  instance, and `agen logs` says why.
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

**`no_answer: the model returned no text and no tool calls`.** The model,
asked twice, and every model in `fallbackModels` came back empty. With
`finish_reason "length"`, reasoning likely used the whole output limit: raise
`maxOutputTokens` in `x-agen/harness.json` or lower `reasoningEffort`. Adding
a fallback model also helps; see [Models](guides/models.md#fallback-models).

**`provider account error: ...`, or a deployment shows `provider held`.** The
provider refused the API key (401, 402 or 403: invalid or expired key, no
credit, a key limit). Agen does not retry or fall back on these, and the Hub
holds the account: every deployment using the same provider and key secret
takes no new tasks, and one that is asleep is not woken for A2A calls (they
fail with `... is stopped: provider account ... is held ...`). Queued tasks
wait. Fix the key or add credit, then end the hold at
once instead of waiting for the next probe:

```sh
agen providers
agen providers resume default/openrouter:OPENROUTER_API_KEY
```

See [Models and providers](guides/models.md#account-errors-and-provider-holds).

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
of daily budget, held after a provider account error (`agen providers`), at
`scale.max` with every instance busy, or waiting for an earlier task of the
same conversation key. A task submitted with `--at` or `--in` waits for its
start time (the console shows it as `scheduled`). `agen ps` and `agen tasks`
show which.

**Queued tasks wait behind an approval.** When every run of a deployment
waits for an approval, nothing else of it moves: its instances are busy with
those runs. The console's Overview warns about it ("tasks queued behind a
pending approval"). Decide the approval (`agen approvals`), allow the tool in
`permissions`, or raise `scale.max` or `scale.maxConcurrency`.

**Waiting for room on a full Nest.** A deployment that wants more instances
than its Nests have capacity for waits; the console's Overview lists it. Idle
deployments on a full Nest give their capacity back after 1 minute (instead of
their `idleTimeout`) while another deployment waits. Add a Nest, raise
`--capacity`, or lower `scale.min` of deployments that hold instances they do
not use.

**`had no free instance within 10m0s (asleep, starting, busy or waiting for
Nest capacity)`.** An A2A call (or `call_agent`) waited 10 minutes for an
instance of the target and gave up. The target may be at `scale.max` with
long runs, stuck waiting for Nest capacity (above), paused, out of budget, or
failing to start (`agen ps --all`). Give it more instances, or use
`start_agent` for long work so callers do not hold a call open.

**`task ... still running after 10m0s; it keeps running`.** `agen run` stopped
waiting, not the task. Wait for it again with `agen wait <task-id>`, or pass
`--timeout 0` to `agen run` to wait without a limit.

**A task was retried.** If an instance dies mid-task, the task is released and
the next attempt resumes the same run on another instance (logged as
`returned to the queue` / `requeued`). After 5 expired leases it fails instead.

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
