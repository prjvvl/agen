# Permissions and approvals

Every tool call is checked against the agent's permissions before it runs:
allowed, sent to a person for approval, or denied.

## Rules

```json
{
  "permissions": {
    "default": "ask",
    "rules": [
      { "tool": "fetch.*", "action": "allow" },
      { "tool": "crm.update_*", "action": "ask" },
      { "tool": "*.delete_*", "action": "deny" }
    ],
    "approvalTimeout": "30m"
  }
}
```

- `tool` is an exact name or a glob (`*` matches anything, dots included).
  Names may be written as the agent registers them (`crm.update_contact`) or
  as the model sees them (`crm_update_contact`).
- Rules are evaluated deny first, then ask, then allow; the first match in
  that order wins, regardless of the order they are written in. Nothing
  matches: `default` applies (`ask` if unset).
- A denied call returns an error to the model, which can try something else.
- `agen deploy --validate` warns about a rule that names a server the bundle
  does not have.

## Approvals

An `ask` creates a durable approval and the run waits (status
`waiting_approval`) until someone decides, or `approvalTimeout` (default 1 h)
passes, which counts as denied. Approvals survive restarts: a run resumed on
another instance waits for the same approval.

```sh
agen approvals
agen approve <id>
agen deny <id>
```

Each approval shows the tool, its arguments (secrets redacted), the task and
run it belongs to, who asked and when it expires; a decided one shows who
decided and when. The web UI has the same list, and MCP clients use
`list_approvals` and `decide_approval`.

**Nobody approves their own request.** The principal whose work asked (the
token that submitted the task, or the A2A caller) cannot decide its
approvals. Give the approver a separate token:

```sh
agen token create --name alice-approver --scope approver
```

A delegated run's approvals name the person behind the root task, so
delegation cannot be used to approve your own request either.

## Notifications

To hear about an approval as soon as it is raised, give the deployment a
URL:

```json
{
  "permissions": {
    "default": "ask",
    "notify": { "url": "https://hooks.example/agen-approvals", "secret": "APPROVAL_HOOK_KEY" }
  }
}
```

The Hub POSTs each new pending approval as JSON
(`{"type": "approval.pending", "approval": {...}}`, header
`X-Agen-Event: approval.pending`), retrying a few times on failure. With
`secret`, the body is signed with that platform secret:
`X-Agen-Signature: sha256=<hex HMAC-SHA256 of the body>`. Set the secret for
the deployment first:

```sh
printf %s "$HOOK_KEY" | agen secret set APPROVAL_HOOK_KEY --for my-agent
```

Failed notifications are written to the deployment's logs (`agen logs`).
The Hub posts to whatever URL the bundle names, including internal
addresses, so only trusted operators should deploy bundles.

## Embedded agents

An agent embedded with an SDK decides approvals in your code: pass an
approval callback when you create the agent (see the SDK README). Without
one, `ask` is treated as deny.
