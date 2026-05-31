# memQL backend cluster on AKS

Kubernetes manifests for the memQL multi-node mesh, deployed to AKS
(epic [znasllc-io/memql#522](https://github.com/znasllc-io/memql/issues/522)
-- pivot from ACA, which can't host the per-node multi-port mesh).

## No database pod

Staging/prod use the managed **Tiger Cloud** DB (`xahn9ru4v6`, Azure East
US 2). There is **no postgres/pgadmin/nginx/livekit** in this directory --
only the 7 memQL node-types. Every node connects to Tiger via the
`MEMORY_NODES_DATABASE_DSN` key in the `memql-secrets` Secret.

## Mesh = cluster DNS

Each node's `Service` is named after its node-type short name (`bff`,
`cognition`, `voice`, `agent`, `planner`, `identity`, `workbench`) in the
`memql` namespace. Same-namespace cluster DNS resolves the compose mesh
values (`bff:50058`, `agent:50055`, ...) **unchanged**, so
`MEMQL_NODE_ADDRESS` / `MEMQL_PARENT_ADDRESS` / `MEMQL_WORKER_PEERS` are the
exact same strings as `docker/docker-compose.cluster.yml`.

| Node | Image | NodeService port | NODE_ADDRESS | PARENT | WORKER_PEERS |
|------|-------|------------------|--------------|--------|--------------|
| bff | memql-bff-copresent:0.9.0 | 50058 | bff:50058 | -- | voice=voice:50059,agent=agent:50055,cognition=cognition:50054,planner=planner:50056,workbench=workbench:50060 |
| cognition | memql:0.9.0 | 50054 | cognition:50054 | bff:50058 | agent=agent:50055 |
| voice | memql:0.9.0 | 50059 | voice:50059 | bff:50058 | -- |
| agent | memql:0.9.0 | 50055 | agent:50055 | bff:50058 | workbench=workbench:50060 |
| planner | memql:0.9.0 | 50056 | planner:50056 | bff:50058 | -- |
| workbench | memql:0.9.0 | 50060 | workbench:50060 | bff:50058 | -- |
| identity | memql:0.9.0 | 50061 | identity:50061 | -- | -- |

Every ClusterIP Service exposes the node's NodeService port (5005x) + `8085`
(http) + `50051` (grpc). `bff` also gets a `LoadBalancer` Service
(`bff-external`) on 8085 -- the external entry point (maps to
`app.copresent.ai` later).

## Migrations run once

The shared Tiger DB must not be migrated by 7 racing nodes. Only the
**identity** node sets `MEMORY_NODES_DATABASE_MIGRATE_ON_START=true` +
`MEMORY_NODES_DATABASE_AUTO_MIGRATE=true`; every other node has both
`false`.

## Secrets (genesis A2)

Three keys in `memql-secrets`: `MEMQL_MASTER_KEY`, `MEMQL_GENESIS_B64`,
`MEMORY_NODES_DATABASE_DSN`. With `MEMQL_GENESIS_AUTOLOAD=true`, each pod
decrypts the sealed envelope in-process at boot and applies ~150 vars
set-if-absent; the per-pod overrides (node type, mesh addresses, DSN) win.
See `secret.example.yaml` for the imperative `kubectl create secret`
recipe and the Azure Key Vault CSI alternative.

## Apply order

```bash
# 1. Namespace
kubectl apply -f deploy/k8s/namespace.yaml

# 2. Secret (real values -- created out-of-band, NOT in kustomize)
kubectl create secret generic memql-secrets -n memql \
  --from-literal=MEMQL_MASTER_KEY="$MEMQL_MASTER_KEY" \
  --from-literal=MEMQL_GENESIS_B64="$(base64 < ~/.memql/genesis.znas)" \
  --from-literal=MEMORY_NODES_DATABASE_DSN="$(tiger db connection-string xahn9ru4v6 --with-password)"

# 3. All node Deployments + Services
kubectl apply -k deploy/k8s/
```

Or, from the repo root: `make deploy-aks ENV=staging` (runs the namespace +
kustomize apply; the Secret step is a one-time prerequisite). `identity`
comes up first to run the one-time migration and serve JWKS; the other
nodes' verifiers retry JWKS non-fatally until it is ready.

## Validate

```bash
kubectl kustomize deploy/k8s/ | kubeconform -strict -summary -kubernetes-version 1.30.0
```
