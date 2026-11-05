# Notifications

The Hub can tell another system when something happens: a chat webhook, an
incident tool, or your own service. A **notification target** is a URL per
namespace that receives events from the [event log](events.md) as signed
JSON. Without `--event` a target gets the events that usually need a person:

| Event | When |
|---|---|
| `approval.pending` | An agent waits for an approval. |
| `task.failed` | A task ended with an error. |
| `budget.exhausted` | A deployment used its daily budget (once per UTC day). |
| `provider.held` | A provider account was held after an account error ([Models](models.md#account-errors-and-provider-holds)). |
| `provider.resumed` | A held provider account works again (a run succeeded, or an operator resumed it). |

`--event` can also name any [event type](events.md) (`task.finished`,
`task.needs_input`, `budget.threshold`, `activity.warning`, ...) or a prefix
(`task.`), and `--deployment`, `--label k=v` and `--severity` narrow what a
target gets:

```sh
agen notify set ops-chat https://hooks.example.com/agen --event approval.pending --event task.failed
agen notify set apollo https://hooks.example.com/apollo --event task.finished --label project=apollo
agen notify set pager https://hooks.example.com/pager --event activity.warning --severity critical
agen notify ls
agen notify rm ops-chat
```

`agen notify set` prints the target's signing secret once; setting a target
again replaces it and its secret. A new target gets the events after it was
set. Targets need an admin token; the console has them under **Settings →
Notifications**.

## What arrives

A `POST` with `Content-Type: application/json`, the event name in
`X-Agen-Event`, the log event, and the object it is about:

```json
{"type": "task.failed",
 "event": {"seq": "1041", "type": "task.finished", "deployment": "researcher", "taskId": "01J...", "data": {"state": "failed", "error": "..."}},
 "task": {"id": "01J...", "namespace": "default", "deployment": "researcher", "state": "TASK_STATE_FAILED", "error": "..."}}
```

Task events carry `task`, approval events `approval`, budget and deployment
events `deployment`, and `provider.held` and `provider.resumed` carry `hold`
(a `ProviderHold`), in the same shape as the API returns them.

`X-Agen-Signature: sha256=<hex>` is the HMAC-SHA256 of the raw body with the
target's secret. Check it before trusting the body, for example in Python:

```python
import hashlib, hmac

def valid(body: bytes, header: str, secret: str) -> bool:
    want = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, header)
```

Events are delivered in log order, one at a time per target. When the
receiver fails (a network error or any non-2xx answer), the same event is
tried again after a pause that grows up to 5 minutes, and later events wait
behind it, so nothing arrives out of order. `agen notify ls` (and
`ListNotificationTargets`) shows since when a target fails and why. An event
still failing after an hour is dropped: the target counts it (`dropped`), the
deployment's log says which one, and delivery moves on. The leader Hub
delivers, so a Hub restart picks up where it stopped. A deployment can also
send its own approvals to a URL with `permissions.notify` in its bundle (see
[Permissions](permissions.md)).
