# Operating Agen securely

The design is in [architecture.md](architecture.md) §10. This page lists what
to set up, and what each control does and does not cover.

## Checklist for a distributed fleet

1. **Hubs serve TLS.**
   - Start them with `agen hub serve --tls`.
   - A Hub refuses to serve bearer tokens over plain HTTP beyond loopback.
2. **Give every Hub the same `AGEN_HUB_KEK`** (32 or more random bytes; never
   give it to Nests). It seals the Hub's CA key, its call-token keys and
   platform secrets in the Store. A TLS Hub without one logs a warning.
3. **Give Nests a least-privilege Store role, not the admin URL:**

   ```sh
   AGEN_HOST_DB_PASSWORD=... agen store host-role --store postgres://admin@db/agen --role agen_host
   ```

   - The role can use the run-data tables only. It cannot read the Hub's
     keys, API or join tokens, or tasks.
   - Add `--namespace NS` (repeatable) to limit the role to those namespaces'
     rows with Postgres row-level security.
   - A Nest serving several namespaces maps each to its role with
     `agen nest run --store-for NS=URL`. Add `--store-for-only` so no
     namespace falls back to `--store`.
4. **Isolate agents from each other with the Kubernetes backend.**
   - Each instance runs in its own pod, with CPU and memory limits, seccomp
     and no service-account token. It uses a namespace separate from the
     Hub's.
   - Native Nests run agents as the Nest's OS user. They are for trusted
     agents, with no sandbox and no CPU limit. `--host-memory-limit` caps
     memory per process:
     - Windows: committed memory, via a Job Object.
     - Linux: address space (`RLIMIT_AS`), which counts reserved virtual
       memory, so leave headroom (2 GiB is tested).
     - Each stdio MCP server the agent starts has its own cap.
   - Docker Nests (the compose cluster) are native Nests in containers: the
     container is the boundary between Nests, not between the agents of one
     Nest.
5. **Keep credentials off command lines.**
   - Pass them in the environment: `AGEN_STORE`, `AGEN_JOIN_TOKEN`,
     `AGEN_TOKEN`, `AGEN_HUB_KEK`.
   - Platform secrets are set from stdin and granted to named deployments
     (`--for`). A bundle that declares a secret's name gets nothing unless
     the secret names its deployment, so an operator cannot deploy their way
     to an admin's secret:

     ```sh
     printf %s "$OPENROUTER_API_KEY" | agen secret set OPENROUTER_API_KEY --for my-agent
     ```

## What protects what

| Control | Covers | Limits |
|---|---|---|
| API tokens with scopes (`viewer`, `operator`, `approver`, `admin`) and namespaces | HubService, REST and MCP alike (MCP tools need the same scopes as their API calls) | |
| Nest mTLS certificates (30 days, renewed automatically; `agen nests revoke ID`) | NestService | Revocation is immediate; renewal swaps certificates atomically |
| A2A call tokens (Hub-signed: users 10 min, agents 1 h; bound to one deployment) | Gateway calls, including while Hubs are down | A token is a bearer credential until it expires |
| Per-instance host token | An instance's control API answers only its Manager | |
| Approvals (`ask`) | The requester can never approve their own ask, whether submitted through the Hub or over A2A with a user token | Hosts of a namespace could tamper with that namespace's run records |
| Host role and row-level security | The Store DSN given to Nests | Namespace isolation is real only with separate pods or OS users |
| `AGEN_HUB_KEK` sealing | Hub keys and platform secrets at rest | Copies from before sealing may remain in backups: rotate after enabling |

## Rotation

**KEK, without downtime:**

1. Restart every Hub with `AGEN_HUB_KEK=<new>` and
   `AGEN_HUB_KEK_PREVIOUS=<old>`. They open values sealed with either key
   and seal new ones with the new key.
2. Re-seal everything with the new key:

   ```sh
   AGEN_HUB_KEK=<old> AGEN_HUB_KEK_NEW=<new> agen hub rotate-kek --store postgres://admin@db/agen
   ```

3. Restart the Hubs without `AGEN_HUB_KEK_PREVIOUS`.

**Call-token key:**

```sh
AGEN_HUB_KEK=<kek> agen hub rotate-token-key --store postgres://admin@db/agen
```

- **Call-token keys:**
  - A new key is published to Nests at once and used for signing after 5
    minutes (Hubs re-read keys every minute, and Managers refresh them every
    minute).
  - The previous key verifies live tokens until the next rotation.
  - Rotation is refused while the current key is younger than an agent
    token's lifetime plus the grace (about 1 h 10 min), so no live token
    stops verifying. `--force` overrides this.
  - Retired keys are kept, only to verify stored requester records.
- **Nest certificates:** they renew themselves (`--cert-renew-before`, 10 days).
- **CA:** a new CA means re-enrolling Nests (not automated).
- **API tokens:** revoke them with the API (`RevokeApiToken`) and create new
  ones.

## Local mode

`agen up` serves plain HTTP on localhost and keeps SQLite in `~/.agen`
(0600; ACLs on Windows). Its Gateway accepts anonymous calls from the local
machine only. There are no roles, and keys are unsealed unless
`AGEN_HUB_KEK` is set.
