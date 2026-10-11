# Running Agen: local and distributed

Local and distributed fleets use the **same two binaries** (`agen`,
`agen-host`) and the **same commands**; only the configuration differs
(where the Store is, whether the Hub uses TLS, how many Nests join).

Install Agen on every machine that runs a Hub or a Nest, and on clients:
see [Install](install.md). Agen registers no OS services; run Hubs and Nests
under your service manager (systemd, or the StatefulSets in `deploy/kube`).

## Local: one process

```sh
agen up # Hub + scheduler + one Nest, SQLite in ~/.agen; keeps running, so use a second terminal for the rest
agen init # writes the hello example to ./hello
agen deploy hello --replicas 1
agen run hello "hi"
agen ps --all # deployments and instances
agen ui # sign-in link for the web UI
agen down # stops the Hub, the Nest and every agent
```

## Distributed: Hubs, Nests, Postgres

Same commands, different flags:

```sh
# Hub (any number of replicas; one is leader, the rest stand by)
# AGEN_HUB_KEK (same on every Hub, never given to Nests) seals the Hub's
# CA and call-token keys in the Store.
AGEN_ADMIN_TOKEN=... AGEN_HUB_KEK=... agen hub serve --tls --listen 0.0.0.0:7070 \
    --tls-host hub.example --store postgres://agen@db/agen
# prints: CA sha256:<hash>

# A Store role for Nests and their hosts: run-data tables only (no access to
# the Hub's keys or tokens). Give Nests this role's URL, not the admin one.
AGEN_HOST_DB_PASSWORD=... agen store host-role --store postgres://admin@db/agen --role agen_host

# A join token per Nest (single use), with the CA hash to pin
agen join-token --hub https://hub.example:7070 --token $AGEN_ADMIN_TOKEN --ca-hash sha256:<hash>

# Each Nest (Manager + Gateway + agent processes)
agen nest run --hub https://hub.example:7070 --join-token <token> --ca-hash sha256:<hash> \
    --name nest1 --store postgres://agen_host@db/agen \
    --gateway-listen 0.0.0.0:7071 --gateway-url http://nest1:7071

# Clients: the same fleet commands, pointed at the Hub
export AGEN_HUB=https://hub.example:7070 AGEN_TOKEN=... AGEN_CA_HASH=sha256:<hash>
agen deploy hello --replicas 3 # a bundle directory, e.g. from agen init
agen ps --all
```

The leader Hub deletes spans, log lines and trigger events older than
`--retention` (default 30 days; `0` keeps them). See [Traces](guides/traces.md#retention).

Moving a local fleet to Postgres keeps its history:
`agen down && agen migrate --to postgres://…`, then start the Hub with
`--store postgres://…`.

`deploy/compose/cluster.yml` runs a complete distributed fleet in Docker
(Postgres, two Hubs behind a TLS-passthrough load balancer, three Nests);
`platform/e2e` tests it end to end, including a Nest dying mid-run, the
leader Hub failing over and both Hubs being down.

## Secrets

Bundles name their secrets in `x-agen/secrets.json`; values never go in the
bundle. With `"source": "platform"` the value comes from the fleet:

```sh
printf %s "$OPENROUTER_API_KEY" | agen secret set OPENROUTER_API_KEY --for my-agent -n default
agen secret ls -n default          # names only; values are never shown
agen secret rm OPENROUTER_API_KEY -n default
```

A Nest gets a value only for a deployment it runs that declares it and that the secret names (`--for`). With
`AGEN_HUB_KEK` set, values are sealed in the Store.

## Key rotation

See [Rotation](security.md#rotation): the KEK is rotated without downtime by
restarting the Hubs with both keys before re-sealing.

## Kubernetes

`deploy/kube` runs the Hub (two replicas, mTLS) in namespace `agen`, and a
Nest whose agents run as pods (`--backend kubernetes`) in namespace
`agen-nests`. They are separate because a Nest may create pods, and a pod
can mount any secret of its namespace: the Hub's admin token must not be
one of them. With kind:

```sh
docker build -f deploy/docker/Dockerfile -t agen:dev .
docker pull postgres:17-alpine
kind create cluster --config deploy/kube/kind.yaml
# Load the images. On Docker Desktop, `kind load docker-image` can fail for
# multi-platform images; importing into the node directly works:
docker save agen:dev | docker exec -i kind-control-plane ctr -n k8s.io images import --snapshotter=overlayfs -
docker save postgres:17-alpine | docker exec -i kind-control-plane ctr -n k8s.io images import --snapshotter=overlayfs -

kubectl apply -f deploy/kube/hub.yaml -f deploy/kube/postgres-dev.yaml   # postgres-dev: throwaway DB
kubectl -n agen create secret generic agen-admin --from-literal=token=<admin token>
kubectl -n agen create secret generic agen-store --from-literal=url=postgres://agen:agen@postgres.agen.svc:5432/agen
kubectl -n agen logs -l app=agen-hub | grep CA                             # the CA hash
agen join-token --hub https://127.0.0.1:17444 --token <admin token> --ca-hash sha256:<hash>

kubectl create namespace agen-nests
# Nests get a least-privilege Store role (run-data tables only), made from a Hub pod:
kubectl -n agen exec deploy/agen-hub -- env AGEN_HOST_DB_PASSWORD=<pw> agen store host-role --role agen_host
kubectl -n agen-nests create secret generic agen-store --from-literal=url=postgres://agen_host:<pw>@postgres.agen.svc:5432/agen
kubectl -n agen-nests create secret generic agen-join --from-literal=token=<join token> --from-literal=ca-hash=sha256:<hash>
# Optional: variables every agent instance gets, e.g. a provider key.
kubectl -n agen-nests create secret generic agen-host-env --from-literal=OPENROUTER_API_KEY=<key>
kubectl apply -f deploy/kube/nest.yaml
agen deploy hello --hub https://127.0.0.1:17444 --token <admin token> --ca-hash sha256:<hash>
```

On a host whose Docker uses cgroup v1 (older WSL2 kernels), recent kubelets
do not start; add this to the kind config:

```yaml
kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    failCgroupV1: false
```

`agen scale`, `agen run`, `agen ps` and the rest are unchanged; each
instance shows up as a pod labelled `agen.dev/deployment=<name>`, limited by
`--kube-cpu-limit` / `--kube-memory-limit` (1 CPU, 1Gi by default).
Instances only answer their Manager (a per-instance host token); the
NetworkPolicy in nest.yaml adds network isolation where the CNI enforces
it (kind's default CNI does not). Bundles must fit in a ConfigMap (700 KiB).
`platform/e2e` (TestKubernetesBackend) tests this on kind: scaling, a
50-task burst, scale to zero and wake on an A2A call, a deleted instance
pod, a deleted Nest pod, host isolation and provider keys.

| | Local (`agen up`) | Distributed |
|---|---|---|
| Binaries | `agen`, `agen-host` | same |
| Store | SQLite in `~/.agen` | Postgres |
| Hub ↔ Nest | HTTP on localhost, nest bearer token | HTTPS, Hub-issued Nest certificates (mTLS) |
| Hubs | 1 (in process) | 1+ (leader election in the Store) |
| Nests | 1 (in process) | any number, `agen nest run` |
| Commands | `agen deploy/scale/ps/run/logs/stop/rm/...` | same (+ `--hub/--token/--ca-hash`) |
