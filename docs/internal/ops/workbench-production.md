---
title: Workbench Production Deployment
audience: ops
status: stable
area: ops
sinceVersion: 0.9.0
owner: znas
---

# Workbench Production Deployment

**Status:** Cluster mode is the deployed default -- `deploy/k8s/base/agent.yaml`
sets `MEMQL_WORKBENCH_REMOTE=1` unconditionally, and `deploy/k8s/base/workbench.yaml`
runs 2 replicas by default in the base manifest (not merely an overlay). This
document covers the storage model and environment topology for that
deployment, and the remaining production-hardening items in section 7.

This doc catalogs the workbench's distributed split from the in-process MVP
(documented in [runbook.md](../../public/operate/workbench-runbook.md)) to a
dedicated cluster node with durable storage. None of it changes the
agent-facing tool surface or the workspace semantics — `workbenchHost` and
per-Plan workspaces behave identically in both modes; only where the work
executes and where bytes land at rest changes.

## 1. What's in the repo

The distributed split landed alongside the MVP and is now the default
runtime path, exercised by tests:

| Layer | What | Files |
|-------|------|-------|
| Build target | `workbench` node-type binary (`make workbench`, ~84 MB) | `app/build_workbench.go`, `app/integrations_workbench.go`, `app/cluster_workbench.go`, `component/node/{bootstrap,compiled}_workbench.go` |
| gRPC envelopes | `WorkbenchForwardRequest` / `Response` / `Cancel` on `NodeService.Stream` | `component/node/node.proto` + regenerated `gen/` |
| Stream wiring | Inbound dispatch + outbound response routing on NodeServer, ParentConnector, WorkerDialer | `component/node/{server,stream_handler,parent_connector,worker_dialer}.go` |
| Agent-side router | `workbench.ForwardRouter` -- prefers the replica holding this plan's workspace (memql#4354), else any healthy peer; sends, awaits response, and refuses on `ErrNoWorkbenchPeer` (memql#3506) | `integrations/workbench/forward_router.go` |
| Workbench-side handler | `workbench.ForwardHandler` -- unmarshals envelope, calls the local integration's dispatch, sends the response | `integrations/workbench/forward_handler.go` |
| AKS manifest stub | `workbench` Deployment placeholder (AKS-native; Cloud Run manifest removed) | `deploy/k8s/` |
| Local cluster | `workbench` Deployment + agent flipped to remote mode | `deploy/k8s/overlays/local` |

The toggle is one env var: `MEMQL_WORKBENCH_REMOTE=1` on the agent
node + a workbench address in `MEMQL_WORKER_PEERS`. With both set,
the integration's dispatch delegates to the remote node; without them
it stays local.

> Until memql#3450, setting both did nothing: `workbench` was in
> neither of `worker_dialer.go`'s dial predicates, so the seed and the
> `v1:cluster:node` row were both filtered out and the agent's
> `ForwardRouter` never found a peer. Because a peerless router returns
> `ErrNoWorkbenchPeer` and the integration read that as "dispatch
> locally," the tool call still succeeded -- on the agent pod's disk.
> If you tested the remote path on an engine older than that fix, the
> result you observed was the local path.
>
> memql#3506 closed the silence, which is the half that made the above
> undetectable. Remote mode is now an assertion: an unreachable
> workbench REFUSES the call (`no_workbench_peer`, naming the peer and
> `MEMQL_WORKER_PEERS`) instead of quietly running it on the agent. If
> you want the old degrade -- reasonable in development -- ask for it by
> name with `MEMQL_WORKBENCH_LOCAL_FALLBACK=1`. The live check that the
> hop actually lands on the workbench node is
> `test/clustere2e/workbench_remote_hop_test.go`, which asserts on the
> executing pod's `MEMQL_NODE_ID` rather than on command output.

## 1a. Replica affinity: a workspace lives on ONE replica

`deploy/k8s/base/workbench.yaml` runs **2 replicas**, and a workspace is a
**filesystem**. A filesystem does not follow the request, so which replica
serves a call is not a load-balancing detail -- it decides whether the plan's
files are there.

Until memql#4354 the agent's peer picker was **any-fit**. A plan's first call
made a directory on one replica; its second call landed on the other with even
odds and found an empty tree. Both calls reported `ok=true` and neither result
named a node, so the failure read as the agent having imagined the write. The
`v1:workbench:workspace` concept existed in the DSL the whole time and was
written by nothing, so there was no record to disagree with.

**How it works now.** The node that creates the directory writes the row and
stamps its own `MEMQL_NODE_ID` on `workspace.nodeId`. Before forwarding, the
agent reads the plan's live workspace row and passes that node id to
`ForwardRouter.pickWorkbenchPeer`, which **prefers that replica whenever it is
healthy and connected** and falls back to any-fit only when it is not.

| Fact | Where |
|---|---|
| The pin, and the four lifecycle calls | `integrations/workbench/workspace_store.go` |
| Affinity in peer selection | `integrations/workbench/forward_router.go` (`selectWorkbenchPeer`) |
| Row bookkeeping on the serving node | `integrations/workbench/integration.go` (`recordWorkspace`) |
| The concept | `dsl/workbench/concepts.memql` (`nodeId`, `ownerUserId`) |

**The row is owner-tiered, and that is load-bearing.**
`v1:workbench:workspace` declares `@rowAuthz(owner="ownerUserId", clusterOwner)`.
The read gate has no internal-origin bypass, so a read with **no actor** returns
**zero rows and no error** -- which is indistinguishable from "this plan has no
workspace" and would make the integration provision a fresh directory on every
call. Every read and write therefore runs under
`auth.ContextWithUserActor(ctx, <the plan's requestedBy>)`.

INFO: `integrations/workbench` is deliberately **not** on the internal-origin
allowlist in the repo-root `call_origin_conformance_test.go`. It binds a user
actor instead, and none of the workbench DSL constructs are `@serverOnly`.

WARNING: A workbench call whose `planId` does not resolve to a readable
`v1:planner:plan` row is now **REFUSED** (`errorCode:
workspace_owner_unresolved`) rather than run. Writing the row under a blank
actor would stamp `ownerUserId: ""`, which hides it from the user whose files it
describes and from the operator; and a workspace keyed on a plan that does not
exist never reaches the `releaseWorkspaceOnPlanTerminal` automation, so its
directory is never reclaimed.

### What happens when a replica is lost

A workbench replica leaving the mesh takes its `emptyDir` with it. There is
nothing to migrate -- **files are NOT copied to the new replica**, because a
file tree cannot be recovered from a node that is no longer there. The design
accepts a fresh empty directory and **records why**:

1. The pinned replica is gone, so the picker returns a healthy substitute and
   the agent logs a WARNING naming **both** node ids and the plan.
2. The substitute creates the directory, sees a live row naming a different
   node, and marks that row `status=released`,
   `releasedReason=node_lost` (`dsl/workbench/mutations.memql`).
3. It inserts a successor row naming itself. The successor carries a different
   id, derived from `(planId, nodeId)` -- one row cannot be both released and
   provisioned.
4. The plan continues on an **empty** workspace.

The re-provision happens **exactly once**: subsequent calls find a live row
naming the serving node and adopt it. Anything else would hand the plan a new
empty directory per call, which is worse than the split it replaces.

INFO: Operator answer to "where did my file go" -- query the plan's workspace
rows and look for `releasedReason=node_lost`. A released row with that reason
IS the answer; without the row there is no record that anything moved.

[ ] Not implemented: no notice reaches the user's canvas. The canvas-state
    concept and `mutationCreateCanvasState` are **pack-only** constructs the
    engine core does not load (see the note in
    `integrations/planner/plan_execution.go`: a planner-side notifier for the
    same reason failed at runtime with `function "mutationCreateCanvasState" not
    found`, and the card now lands via a product-pack automation). A node-loss
    card would have to arrive the same way -- an automation in the product
    bundle fired off the `node_lost` release -- and nothing in this repository
    can emit one.

### The operator surface for all of this

`/fleet/workbenches` in the MemQL Portal (memql#4356) is where the above is
read rather than queried: the workbench replicas the cluster knows about, and
the per-plan workspaces living on each -- live and released, with the release
reason spelled out, so a `node_lost` row is legible without going to the
source. A cluster owner can widen the scope from their own workspaces to every
workspace in the cluster; that is the read that answers "why is this workbench
node full".

Two reads back it, both caller-scoped in the ENGINE rather than in the page
(`dsl/workbench/queries.memql`):

| Query | Who | Scope |
|---|---|---|
| `myWorkspaces` | anyone signed in | `ownerUserId==actor.userId`, live and released, newest first |
| `allWorkspaces` | cluster owner (`actor.isClusterOwner==true`) | every workspace, optionally narrowed to one `status` |

The existing `provisionedWorkspaces` inventory read is unchanged, and is now
row-gated like everything else.

WARNING: the page cannot show a **fill level**. `v1:cluster:node` declares no
capacity field -- no disk figure, no workspace cap, no quota -- so the per-node
number is the count of workspaces the page has LOADED that name that node, and
it is captioned as exactly that. Any node label shaped like a capacity is
rendered beside it, because an operator who set one meant it to be read.

## 2. Storage model: ephemeral scratch + durable blob

### Durability design

The workbench directory (`MEMQL_WORKBENCH_ROOT/{planId}/`) is
**ephemeral per-Plan scratch**: it exists only while the workbench Pod
is alive. On AKS staging the workbench directory is backed by an
`emptyDir` volume — correct by design provided the blob path is
configured.

Durability comes from the blob store. When an agent calls `fs_write`,
the workbench integration uploads the bytes to the configured Azure
Blob container via `integrations/azureblob` and records a
`v1:common:attachment` node (type `workbench/output`) and a
`v1:library:generatedOutput` node. The durable copy lives in the blob
container; the `emptyDir` is just a staging area for the current
execution.

The two env vars that wire durability:

| Variable | Purpose |
|----------|---------|
| `MEMQL_AZURE_STORAGE_CONNECTION_STRING` | Connection-string auth to the Azure Storage account (or Azurite emulator). Used by `integrations/azureblob.New()`. |
| `MEMQL_AZURE_BLOB_CONTAINER` | Container name to upload into. Read via `integrations/azureblob.ContainerFromEnv()`. |

When either variable is absent the uploader returns an error and
storage degrades to a `local://` placeholder — no bytes leave the Pod.
This is the same fail-open posture as before the Azure Blob migration
(#801 / PR #803).

The app does **not** create the container at runtime. Container
creation is owned by the provisioning step for each environment (see
sections 3 and 4 below).

### Blob URL shape

A successful upload returns a URL of the form:

```
https://<account>.blob.core.windows.net/<container>/<objectName>
```

This URL is stored on the `v1:common:attachment.blobUrl` field. The
attachment download endpoint (`GET /spaces/{spaceId}/attachments/{attachmentId}`,
shipped in #804) serves bytes back by calling
`integrations/azureblob.DownloadURL()` against this stored URL.

## 3. Storage topology, by deploy target (decisions locked #805)

Each INSTALLATION carries its own Azure Storage account — strong
blast-radius isolation with no data bleed between installs.

This table used to have a **Staging** row and a **Prod** row, and that was a
claim about a dimension the product does not have. MemQL ships ONE
INSTALLATION SHAPE (epic memql#3943): an operator who wants a second
environment installs a second instance, with its own domain, its own ArgoCD
and — by the rule above — its own storage account. What genuinely varies is
the **deploy target**, which is a different axis and carries its own field,
`provider`.

| Deploy target | `provider` | Storage backend | Account | Container | Status |
|---|---|---|---|---|---|
| **Local** | `docker-local` | Azurite emulator (Docker) | `devstoreaccount1` (well-known) | `attachments` (created by init script) | Tracked: #806 |
| **Cloud** | `azure` | Azure Blob Storage | `st<install>` in `rg-<install>` | `attachments` | Tracked: #807 |

### Local dev (Azurite)

Local dev uses the **Azurite** official Azure Storage emulator (Docker
image `mcr.microsoft.com/azure-storage/azurite`). This keeps local
dev zero-cost, offline-capable, and on the identical connection-string
code path as cloud.

Azurite exposes a well-known development connection string
(`DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;...;BlobEndpoint=http://azurite:10000/devstoreaccount1;`).
The exact value is documented in the
[Azurite README](https://github.com/Azure/Azurite#connection-string)
and the Microsoft Azure Storage SDK docs — it is public, hardcoded in
Azurite itself, and safe to use locally.

The in-cluster hostname is `azurite` (the k8s Service name).
`scripts/k3d/seed-secrets.sh` writes it onto the `memql-secrets` Secret
(`make up` / `make secrets`), which the agent pod reads via `envFrom`.
See #806 for the full wiring.

### Cloud install

A cloud install uses a real Azure Storage account, `st<install>` in
`rg-<install>` — placeholders, not names: the account and resource group are
the operator's, supplied at install time, and no file in this repository may
name one (memql#4286).

It is not shared with local dev, and not shared with any other install: each
one carries its own secret material. A second cloud install is a second
account by the same rule, which is what the deleted "Prod (deferred)" section
was reaching for — there is no promotion step between two installs, because
there is no tier for one to be promoted from.

**The provisioning script does not exist.** `scripts/deploy/` carries no
`provision-blob-*.sh`; #807 tracks writing one. Until it does, the connection
string is set by hand as a key on `memql-secrets` (section 4 below), which is
the path every other config value takes.

## 4. Delivering the connection string

The connection string is a **shared cluster secret**, and it reaches the
pods by the one path every other config value does (epic memql#3958): as a
KEY on the `memql-secrets` Secret. Not ad-hoc `kubectl set env`.

> **This section used to describe re-sealing a genesis envelope** — edit a
> plaintext `.env`, run a `genesis-seal` make target, re-store the sealed
> `genesis-b64` blob in Key Vault and in a `genesis` k8s secret. That
> mechanism no longer exists.

Canonical steps (cloud example; local dev writes the same keys through
`scripts/k3d/seed-secrets.sh`):

1. Put each value in Key Vault:
   ```bash
   az keyvault secret set --vault-name kv-<install> \
     --name memql-azure-storage-connection-string --value "<connection string>"
   az keyvault secret set --vault-name kv-<install> \
     --name memql-azure-blob-container --value "attachments"
   ```
2. Declare each as a `remoteRef` in
   `deploy/external-secrets/externalsecret-memql.yaml` so ESO reconciles it
   onto `memql-secrets`. **An undeclared key is a key nothing reconciles** —
   that is exactly what made the identity signing seed a latent outage
   (memql#3960).
3. Roll the agent pods so they pick up the new value:
   ```bash
   kubectl rollout restart deployment/memql-agent -n memql
   ```

The vars must reach the **agent node** — that is the binary that runs
the workbench integration, the attachment upload handler, and
computer-use uploads. Verify injection via `app/transport_agent.go`.

Do NOT use `kubectl set env` directly — that creates config drift outside
Key Vault and breaks the declared source of truth.

## 5. Cluster mode -- local validation

Before the AKS cutover, exercise the cluster path locally on k3d. Bring
the cluster up (`make up`) with the `workbench` Deployment enabled and
the agent flipped to remote mode in `deploy/k8s/overlays/local`
(`MEMQL_WORKBENCH_REMOTE=1` + a `workbench=workbench:50060` seed in
`MEMQL_WORKER_PEERS`), then roll the changed nodes:

```bash
make dev NODE=agent
make dev NODE=workbench
```

With remote mode set, dispatches route over `NodeService.Stream` to the
dedicated binary. The workbench directory is local to the workbench pod;
durable output requires Azurite to be running and both vars set (see #806).

Smoke test: ask an agent to write a file, then verify the workbench
pod received it:

```bash
kubectl exec -n memql deploy/workbench -- ls /var/lib/memql/workbenches/
```

## 6. AKS cutover

When the workbench node is promoted to a dedicated AKS Deployment:

1. Build and push the `workbench` binary image (same pipeline that
   produces `memql-agent` / `memql-cognition` / `memql-planner`):
   ```bash
   make workbench
   # image build + push wired via the existing image-build pipeline
   ```

2. Apply the AKS Deployment manifest (`deploy/k8s/`) with the
   workbench service as its own `Deployment` + `Service`.

3. Add to the agent Deployment manifest:
   ```yaml
   - name: MEMQL_WORKBENCH_REMOTE
     value: "1"
   - name: MEMQL_WORKER_PEERS
     value: "workbench=memql-workbench:50060"
   ```
   Adjust the address to match the Kubernetes internal DNS for the
   workbench Service.

4. Ensure the agent Deployment receives `MEMQL_AZURE_STORAGE_CONNECTION_STRING`
   and `MEMQL_AZURE_BLOB_CONTAINER` from `memql-secrets` (step 4
   above).

## 7. What's not yet implemented

Items deliberately deferred; revisit at production cutover:

- **Per-instance workspace isolation.** Every workbench call in a Plan now
  reaches the same directory tree on the same replica (section 1a); concurrent
  Tasks writing the same path still use last-writer-wins. Add advisory locking
  if it bites.
- **Workspace durability across replica loss.** A lost replica costs the plan
  its files (section 1a). The row records that it happened; nothing restores
  them. A durable substrate (a shared volume, or rehydrating from blob) would
  close it.
- **Resource quotas per Plan.** Global size + timeout caps exist but
  no per-Plan disk quota or per-Plan blob size limit. Add
  `Plan.workbenchQuotaBytes` and a pre-write check if a misbehaving
  agent is seen filling the container.
- **Network egress policy.** `http_fetch` currently allows arbitrary
  outbound HTTP. Add an allowlist (or deny-by-default + agent opt-in)
  before going production.
- **Image hardening.** The workbench binary's base image needs a
  decision (Alpine vs. distroless + busybox vs. Ubuntu-minimal) and
  pre-installed runtimes (`curl`, `git`, Python, Node) that the
  prompt's `workbench:environment` chunk promises agents will find.
- **Audit telemetry.** No `v1:worker:invocation` equivalent for
  workbench calls yet. Decide whether high-volume telemetry is needed
  for the sandboxed path.
- **Frontend visibility.** The cockpit and the product frontend do not
  surface workbench contents. Add a Plan-level "Workbench outputs"
  panel when there is user demand. (Workspaces are agent-private by
  design; surfacing is optional.)

## 8. Rollback plan

If a cluster cutover causes issues, the rollback is one env var:
remove `MEMQL_WORKBENCH_REMOTE` from the agent Deployment and
re-deploy. The agent integration's dispatch returns to local
in-process mode immediately.

Removing the flag is the correct rollback, and since memql#3506 it is
the ONLY one that works: leaving the flag set while the workbench is
unreachable now refuses every call rather than silently running it on
the agent. Setting `MEMQL_WORKBENCH_LOCAL_FALLBACK=1` would also
restore service, but it restores it by putting the work back on the
agent's disk while the manifest still claims otherwise -- which is the
state memql#3450 shipped in. Prefer removing the assertion you are no
longer honouring. The workbench Deployment can stay
running idle (or be scaled to zero) without affecting agent
operation.

The MVP path is the default; production cutover is purely additive.
