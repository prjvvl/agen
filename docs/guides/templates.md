# Templates

Agen ships ready-made agent bundles to start from. They are ordinary bundles:
once deployed, change them like any other.

```sh
agen init --list
agen init my-researcher --template researcher
agen deploy my-researcher --replicas 1
```

To start on another model, pass it to `agen init`. A routing model such as
`typesafe/jev-router` picks the model for each request within a cost tier
(see [Models](models.md#routing-models-and-tiers)):

```sh
agen init my-coder --template coder --tier low
agen init my-writer --template writer --model typesafe/jev-router --tier low --max-tier high
agen init my-agent --template hello --model deepseek/deepseek-v4-flash --fallback none
```

`--model` on `hello` swaps its scripted replies for OpenRouter and reads
`OPENROUTER_API_KEY` from the environment. `--fallback` takes
comma-separated models, or `none`. `agen init` checks the result and writes
nothing if the settings are invalid (for example `--tier` on a model that is
not a routing model).

The console's **Templates** page deploys them directly, asks for the name,
model (and the cost tier for a routing model) and instructions, and sets the
platform secrets they need. **New agent** in the console offers a routing
model too.

A template brings the tools and skills it uses: tool servers in its
`mcp.json` (the Hub's own tools, GitHub's MCP server, a web fetcher, Agen's
[built-in workspace and SQL servers](tools.md#built-in-tool-servers)) and
skills in `skills/` (`hello` greets with one, `pr-reviewer` loads a review
checklist, `coder` and `ci-fixer` preload a coding workflow). `agen init
--list` and the Templates page show them. To give an agent more tools, add
servers to its `mcp.json` and allow their tools in `permissions`; see
[Tools](tools.md).

| Template | What it does | Needs |
|---|---|---|
| `hello` | A scripted agent that never calls a model. | nothing |
| `researcher` | Answers questions by reading web pages, with sources. | `OPENROUTER_API_KEY`, `uv` for its fetch tool |
| `writer` | Writes drafts from notes and facts. | `OPENROUTER_API_KEY` |
| `editor` | Plans a piece, delegates to `researcher` and `writer`, checks the draft. | both deployed in the same namespace |
| `pr-reviewer` | GitHub pull request events arrive on a signed webhook; it reads the change and comments. | `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET` (platform secrets) |
| `coder` | Makes code changes in one project folder on the machine it runs on: reads, edits, and runs the project's tests (each command asks first). | `OPENROUTER_API_KEY`, the folder in `mcp.json` |
| `ci-fixer` | A failing CI job arrives on a signed webhook; it reproduces the failure in a local clone, finds the cause and fixes it there. Never pushes. | `OPENROUTER_API_KEY`, `CI_WEBHOOK_SECRET` (platform secret), a clone in `mcp.json` |
| `support` | A webhook receives customer messages; it answers from your documentation folder (read only: read, list, grep), with sources, and flags what a person must handle. | `OPENROUTER_API_KEY`, the docs folder in `mcp.json` |
| `incident-responder` | An alert arrives on a webhook; it reads your runbooks and the fleet and writes the impact, likely cause and next steps. Read-only. | `OPENROUTER_API_KEY`, the runbooks folder in `mcp.json`, a `viewer` token |
| `data-analyst` | Answers questions about a Postgres or SQLite database with read-only SQL and shows the queries it ran. | `OPENROUTER_API_KEY`, `DATABASE_URL` (platform secret) |
| `fleet-steward` | Every 30 minutes, reports new failed tasks and live warnings (bursts, quiet runs, blocked queues, held providers); every morning, a cost report. Read-only. | a `viewer` token |
| `approval-triage` | Summarizes pending approvals with a recommendation; never decides. | an `approver` token |
| `assistant` | The console's chat assistant. | an `--on-behalf` token (see [The console](console.md#assistant)) |

`coder`, `ci-fixer` and `data-analyst` use a [routing model](models.md) with
a fallback model. The folder templates (`coder`, `ci-fixer`, `support`,
`incident-responder`) read a path on the machine their instance runs on: set
`--root` in `mcp.json` before deploying, and deploy them where that folder is.

The operations templates (`fleet-steward`, `approval-triage`,
`incident-responder`, `assistant`) talk to the Hub through its MCP endpoint.
They read two platform secrets: `AGEN_HUB_URL` (the Hub's address as the Nest
sees it, shared by all of them) and their own token, stored as
`AGEN_TOKEN.<agent>` (`key` in `x-agen/secrets.json`) because each needs
different scopes. The console sets both when it deploys them; from the CLI:

```sh
agen token create --name fleet-steward-agent --scope viewer
echo "<the token>" | agen secret set AGEN_TOKEN.fleet-steward --for fleet-steward
echo "http://127.0.0.1:7070" | agen secret set AGEN_HUB_URL --for fleet-steward --for incident-responder
```

A secret names every deployment that may read it: setting it again replaces
that list, so name them all.
