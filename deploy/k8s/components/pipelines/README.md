# `pipelines` — where pipeline steps run

Every compiled pipeline step that names no need of its own runs as a Kubernetes
**Job** in the `memql-pipelines` namespace, created, watched and captured by the
**workbench** node. This component is that namespace and everything in it, plus
the one ConfigMap that points the workbench at it.

Epic memql#5478, task memql#5492.

## What it ships

| Object | Namespace | What it is for |
|---|---|---|
| `Namespace/memql-pipelines` | — | Where steps run. Pod Security `enforce: baseline`, `warn: restricted` |
| `Role` + `RoleBinding/memql-pipelines-runner` | `memql-pipelines` | The runner's grant, bound to `memql-engine` |
| `ServiceAccount/memql-pipelines-step` | `memql-pipelines` | The step pod's identity: no token mounted, nothing bound |
| `PersistentVolumeClaim/memql-pipelines-cache` | `memql-pipelines` | Go and npm caches shared across runs, mounted at `/cache` |
| `ResourceQuota/memql-pipelines-ceiling` | `memql-pipelines` | How many steps run at once (`count/jobs.batch`) |
| `LimitRange/memql-pipelines-limits` | `memql-pipelines` | Every step container's default requests and limits |
| `NetworkPolicy/memql-pipelines-isolate` | `memql-pipelines` | Nothing in; DNS and the public internet out |
| `ConfigMap/memql-pipelines` | the mesh's | `MEMQL_PIPELINES_NAMESPACE`, `MEMQL_PIPELINES_CLONE_IMAGE` |

It also appends that ConfigMap to the **workbench** Deployment's `envFrom`, and
to nothing else: the workbench is the one binary that contains the runner.

## Who composes it

The three instance overlays — `local`, `cloud` and `cloud-entry` — and nobody
else.

**Not tenants.** A tenant overlay renders `base` under `namespace: <tenant>`,
and `memql-pipelines` is one name per cluster: two tenants sharing a cluster
would share one namespace, one Role and one quota. The
[`tenant`](../tenant/README.md) template does not compose this component, and
`component_test.go` fails if it starts to.

**A consumer must set its own namespace with an `unsetOnly` transformer**, not
the plain `namespace:` field. The plain field rewrites every namespace it meets
— an explicit `memql-pipelines` included — so it moves these objects into the
mesh namespace, and with a second Namespace object in the stream it fails the
render outright:

```
error: namespace transformation produces ID conflict
```

The instance overlays carry `namespace-transformer.yaml`:

```yaml
apiVersion: builtin
kind: NamespaceTransformer
metadata:
  name: mesh-namespace
  namespace: memql
unsetOnly: true
```

It fills in only what states no namespace — the ConfigMap here, which belongs
beside the workbench — and keeps every stated one. Swapping it in for the plain
field rendered all three overlays byte-identical apart from this component's
objects (measured on 2026-10-03). The trade it makes: changing that value no
longer moves what `base` placed, because `base` states `memql` itself.

## Values

Each instance overlay states its values in `pipelines-values.yaml`, one
strategic-merge patch per object. The component's own numbers are what a laptop
can run.

| | `local` | `cloud` | `cloud-entry` |
|---|---|---|---|
| Cache class | cluster default (field absent) | `azureblob-nfs-premium` | `azureblob-nfs-premium` |
| Cache access mode | `ReadWriteOnce` | `ReadWriteMany` | `ReadWriteMany` |
| Cache size | 20Gi | 100Gi | 50Gi |
| Steps at once | 4 | 12 | 4 |
| Container limit | 2 CPU / 4Gi | 4 CPU / 8Gi | 2 CPU / 4Gi |
| Container request | 250m / 512Mi | 1 CPU / 2Gi | 250m / 512Mi |

- **The cache's access mode is not a preference.** The steps of one run land on
  whichever node has room, so on a multi-node cloud cluster the cache two step
  pods mount at once must be `ReadWriteMany`. Locally, k3d's `local-path` class
  offers `ReadWriteOnce` only — per node, not per pod — and a local-path volume
  pins the pods that mount it to its node, where they share it. "No class" is
  an **absent** `storageClassName`: an empty string means "bind only to a
  pre-made volume" and the claim sits Pending forever.
- **`azureblob-*` needs the Azure Blob CSI driver.** AKS creates those classes
  only on a cluster running it, which
  [`scripts/deploy/azure-provision.sh`](../../../../scripts/deploy/azure-provision.sh)
  turns on: `--enable-blob-driver` at create, and `az aks update
  --enable-blob-driver --yes` on a cluster that predates it (read first, so a
  converged cluster reports `changed: false`). Without the driver the claim
  sits Pending naming a class that does not exist.
- **The ceiling is a count of Jobs**, enforced atomically by the API server
  across every workbench replica. A step that meets a full quota is refused
  with `exceeded quota` and the runner waits, bounded by the run's wall-clock
  ceiling, rather than failing it.
- **The limits are a `LimitRange` default**, so the runner sets no resources on
  a step and this is the one place a step's size is decided. It covers every
  container in the pod — the clone init container and each service sidecar as
  well as the step. A default request above its default limit is refused by
  the API server.

## The grant

`memql-pipelines-runner` holds exactly what the runner calls:

| Resource | Verbs | Why |
|---|---|---|
| `batch/jobs` | create, get, list, watch, patch, delete, deletecollection | one Job per step attempt; its heartbeat and outcome annotations; deleted once the agent has the result; a cancelled run swept by label |
| `pods` | get, list, watch | the Job's pod classifies a failure (image pull, clone) |
| `pods/log` | get | the step's output, followed while it runs |
| `secrets` | create, get, patch, delete, deletecollection | one Secret per Job: the clone token and the step's resolved secrets |

It is bound to **`memql-engine`**, the ServiceAccount every engine Deployment
runs as, not to a workbench-only account: the workbench also makes model calls,
and both vendors' workload identity federation trusts `memql-engine` by name.
That is the precedent [`custom-domain-rbac.yaml`](../../base/custom-domain-rbac.yaml)
set. What confines the grant is the namespace — it holds only in
`memql-pipelines`, where the mesh runs nothing — and the binary: only the
workbench contains the runner. There is no ClusterRole.

## The step

A step is code from a repository, in an image the platform did not build:

- It runs as `memql-pipelines-step`, which has no token mounted and nothing
  bound to it, so it holds no Kubernetes credential at all.
- Pod Security `baseline` refuses a privileged, host-path or host-network pod
  in the namespace at admission, whatever spec reaches the API server.
- The network policy admits nothing in and lets out only cluster DNS and the
  internet minus `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` and
  `169.254.0.0/16`: not the mesh, the database, another pod, a node or the
  cloud's instance-metadata endpoint.

**A NetworkPolicy is enforced by the cluster, not by the object.** k3s, the
local cluster, enforces it out of the box. An AKS cluster enforces it only when
it runs a network policy engine (`az aks create --network-policy ...`), and
`azure-provision.sh` does not set one today; on such a cluster the policy is
accepted and changes nothing.

## The clone image

`MEMQL_PIPELINES_CLONE_IMAGE` is the image of each Job's `clone` init container,
which runs `git` and is handed the repository token. It is pinned by digest —
whoever controls a mutable tag would control the token — and the digest is the
multi-arch index, measured, never typed:

```bash
docker buildx imagetools inspect docker.io/library/buildpack-deps:bookworm-scm
# Digest:    sha256:b42f74a50a22540b839042134919504ff7c51e7cf8d751b79d0f94c56566cb92   (2026-10-03)
```

## Bringing an existing cluster onto it

1. **The AppProject.** The `memql` AppProject
   ([`deploy/argocd/apps/project.yaml`](../../../argocd/apps/project.yaml))
   permits `memql-pipelines` as a destination. An install whose Argo runs its
   own AppProject must add the same destination before it syncs a revision
   carrying this component, or the sync is refused for every object here.
2. **On AKS, the Blob CSI driver.** Re-run `azure-provision.sh` against the
   cluster; it converges an existing one with the update above.
3. **Network policy enforcement**, if the isolation is to mean anything on
   that cluster (see above).

## Gates

- [`../../overlays/render_pipelines_test.go`](../../overlays/render_pipelines_test.go)
  renders all three overlays: placement, the grant verb for verb, the binding,
  the step identity, the network policy, the ceiling and limits as stated
  values, the cache per overlay, and the workbench's env.
- `../../overlays/render_cloud_test.go` and `render_cloud_entry_test.go`: the
  one-namespace gates, which accept this component's objects in
  `memql-pipelines` and nothing else.
- [`../../overlays/substrate_overlay_coupling_test.go`](../../overlays/substrate_overlay_coupling_test.go):
  an `azureblob-*` cache class requires the script's `--enable-blob-driver`.
- `component_test.go` beside this file: the component's own files, read
  without a renderer.
