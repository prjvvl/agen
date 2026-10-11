# Notifications

The Hub can tell another system when something needs a person: a chat
webhook, an incident tool, or your own service. A **notification target** is a
URL per namespace that receives these events as signed JSON:

| Event | When |
|---|---|
| `approval.pending` | An agent waits for an approval. |
| `task.failed` | A task ended with an error. |
| `budget.exhausted` | A deployment used its daily budget (once per UTC day). |

```sh
agen notify set ops-chat https://hooks.example.com/agen --event approval.pending --event task.failed
agen notify ls
agen notify rm ops-chat
```

Without `--event` a target gets every event. `agen notify set` prints the
target's signing secret once; setting a target again replaces it and its
secret. Targets need an admin token; the console has them under **Settings →
Notifications**.

## What arrives

A `POST` with `Content-Type: application/json`, the event type in
`X-Agen-Event`, and the object it is about:

```json
{"type": "task.failed", "task": {"id": "01J...", "namespace": "default", "deployment": "researcher", "state": "TASK_STATE_FAILED", "error": "..."}}
```

`approval.pending` carries `approval` and `budget.exhausted` carries
`deployment`, in the same shape as the API returns them.

`X-Agen-Signature: sha256=<hex>` is the HMAC-SHA256 of the raw body with the
target's secret. Check it before trusting the body, for example in Python:

```python
import hashlib, hmac

def valid(body: bytes, header: str, secret: str) -> bool:
    want = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, header)
```

Delivery is tried three times (network errors and 5xx answers are retried);
failures are logged on the deployment (`agen logs`). A deployment can also
send its own approvals to a URL with `permissions.notify` in its bundle (see
[Permissions](permissions.md)).
