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
| Cache marked `binds-on-first-use` | yes | no | no |
| Steps at once | 4 | 2 | 1 |
| Container limit | 2 CPU / 4Gi | 2 CPU / 4Gi | 2 CPU / 4Gi |
| Container request | 250m / 512Mi | 250m / 512Mi | 250m / 512Mi |

- **The cloud numbers are sized for the default pool** `azure-provision.sh`
  creates — 2 x Standard_D2as_v4, about 3.8 allocatable CPU — with the
  overlay's own mesh already on it (cloud requests 2.4 CPU / 3Gi, entry
  250m / 640Mi, measured from the renders). A step's request multiplies with
  its containers (below), so two plain steps fit beside the cloud mesh; at the
  12 Jobs x 1 CPU / 2Gi first proposed, not one did. **A CI-heavy instance
  raises the ceiling and the requests in its own overlay, together with a
  dedicated node pool for the steps**; raising them alone gives steps that sit
  Pending until their scheduling timeout. `render_pipelines_test.go` pins all
  three overlays' numbers, so that decision shows in review.

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
- **The local cache is marked `memql.io/binds-on-first-use: "true"`.**
  `local-path` binds on first consumer, so the claim stays Pending until a step
  mounts it, and Argo CD reads a Pending claim as Progressing and an
  Application as its worst resource: without the mark every dev cluster's
  Application reported Progressing until a pipeline had run, and anything
  waiting for Healthy waited forever. The ArgoCD bootstrap carries a
  PersistentVolumeClaim health customization
  ([`deploy/argocd/bootstrap/pvc-health.yaml`](../../../argocd/bootstrap/pvc-health.yaml))
  that reads a marked Pending claim as Healthy and gives every other claim Argo
  CD's own answer. The cloud claims are not marked: there a Pending cache can
  mean its class does not exist, which must keep reading Progressing.
  `make up` gives every dev cluster the customization: a fresh one through the
  bootstrap's `kubectl apply -k`, an existing one -- where up.sh skips that
  apply rather than re-fetch the upstream install -- by merging
  `pvc-health.yaml` into the live `argocd-cm` with a JSON merge patch, which
  leaves every other key and label as it is.
- **The ceiling is a count of Jobs**, enforced atomically by the API server
  across every workbench replica. A step that meets a full quota is refused
  with `exceeded quota` and the runner waits, bounded by the run's wall-clock
  ceiling, rather than failing it. **It counts Jobs and nothing else**: a
  compute quota is enforced when the Job's pod is created, after the Job
  exists, so a step it refused would wait inside its own
  `activeDeadlineSeconds` and time out having never run.
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
| `secrets` | create, get, list, patch, delete, deletecollection | one Secret per Job: the clone token and the step's resolved secrets; listed (by label, metadata alone) by the sweep that deletes a Secret no Job ever came to own |

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
  internet minus `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `169.254.0.0/16` and `168.63.129.16/32`: not the mesh, the database, another
  pod, a node, the cloud's instance-metadata endpoint or Azure's WireServer
  (a public address that, on AKS, serves a node's provisioning material).

**A NetworkPolicy is enforced by the cluster, not by the object.** k3s, the
local cluster, enforces it out of the box. An AKS cluster enforces it only when
it runs a network policy engine (`az aks create --network-policy ...`), and
`azure-provision.sh` does not set one today; on such a cluster the policy is
accepted and changes nothing — so the workbench's runner, which proves the
namespace isolated before it creates a step, refuses every step there
(`pipeline_isolation_unenforced`) until an engine is enabled
([the isolation proof](../../../../docs/public/operate/pipelines-substrate.md#the-isolation-proof)).

**So the workbench proves it before it starts a step.** Before the first step
it creates -- and again once a pass is an hour old -- each workbench replica
runs a probe here: one Indexed Job (`memql.io/probe=isolation`) whose pods each
listen on a port, one of which tries to reach the other while it reaches the
cluster's DNS, which the policy allows. Reaching DNS but not the listener is a
pass. Reaching the listener refuses every step `pipeline_isolation_unenforced`,
naming the fix, and so does a probe that cannot decide (DNS unreachable, the
listener gone), until a later proof passes. The probe is one Job, so it waits
for one slot under the ceiling as a step does, and it deletes its Job and its
Secret before it answers. It runs under the runner's grant above and needs
nothing more (`integrations/pipelinesteps/isolation.go`).

## The clone image

`MEMQL_PIPELINES_CLONE_IMAGE` is the image of each Job's `clone` init container,
which runs `git` and is handed the repository token. The isolation probe runs
on it too, and needs `bash` (its `/dev/tcp`) and `perl` (the listener): an
image without them leaves the probe unable to decide, and every step refused.
It is pinned by digest —
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
3. **A network policy engine.** Without one the workbench starts no step: its
   proof finds the namespace open and refuses each one
   `pipeline_isolation_unenforced` (see above).

## Gates

- [`../../overlays/render_pipelines_test.go`](../../overlays/render_pipelines_test.go)
  renders all three overlays: placement, the grant verb for verb, the binding,
  the step identity, the network policy, the ceiling and limits as stated and
  pinned values (Jobs only in the quota), the cache per overlay, the
  binds-on-first-use mark (local only), and the workbench's env. It also
  renders the ArgoCD bootstrap, with the remote upstream install swapped for a
  local stand-in, and asserts `argocd-cm` carries the claim health script that
  reads the mark.
- `../../overlays/render_cloud_test.go` and `render_cloud_entry_test.go`: the
  one-namespace gates, which accept this component's objects in
  `memql-pipelines` and nothing else.
- [`../../overlays/substrate_overlay_coupling_test.go`](../../overlays/substrate_overlay_coupling_test.go):
  an `azureblob-*` cache class requires the script's `--enable-blob-driver`.
- `component_test.go` beside this file: the component's own files, read
  without a renderer.
