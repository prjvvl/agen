# Templates

Agen ships ready-made agent bundles to start from. They are ordinary bundles:
once deployed, change them like any other.

```sh
agen init --list
agen init my-researcher --template researcher
agen deploy my-researcher --replicas 1
```

The console's **Templates** page deploys them directly, asks for the name,
model and instructions, and sets the platform secrets they need.

| Template | What it does | Needs |
|---|---|---|
| `hello` | A scripted agent that never calls a model. | nothing |
| `researcher` | Answers questions by reading web pages, with sources. | `OPENROUTER_API_KEY`, `uv` for its fetch tool |
| `writer` | Writes drafts from notes and facts. | `OPENROUTER_API_KEY` |
| `editor` | Plans a piece, delegates to `researcher` and `writer`, checks the draft. | both deployed in the same namespace |
| `support-triage` | A webhook receives customer messages; it classifies each and drafts a reply as JSON. | `OPENROUTER_API_KEY` |
| `pr-reviewer` | GitHub pull request events arrive on a signed webhook; it reads the change and comments. | `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET` (platform secrets) |
| `fleet-steward` | Every 30 minutes, reports new failed tasks and their likely cause. Read-only. | a `viewer` token |
| `cost-watchdog` | A daily spend report that flags jumps and budgets running out. Read-only. | a `viewer` token |
| `approval-triage` | Summarizes pending approvals with a recommendation; never decides. | an `approver` token |
| `assistant` | The console's chat assistant. | an `--on-behalf` token (see [The console](console.md#assistant)) |

The operations templates talk to the Hub through its MCP endpoint. They read
two platform secrets: `AGEN_HUB_URL` (the Hub's address as the Nest sees it)
and `AGEN_TOKEN` (a token with the scopes in the table). The console sets
both when it deploys them; from the CLI:

```sh
agen token create --name fleet-steward-agent --scope viewer
echo "<the token>" | agen secret set AGEN_TOKEN --for fleet-steward
echo "http://127.0.0.1:7070" | agen secret set AGEN_HUB_URL --for fleet-steward
```
