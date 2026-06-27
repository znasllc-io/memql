---
title: Workbench Production Deployment
audience: ops
status: stable
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Workbench Production Deployment

**Status:** Cluster-mode deployment is deferred — gated on the broader
production rollout. The code is committed and tested. This document
covers the storage model and environment topology that apply when the
workbench node is promoted to production.

This doc catalogs what needs to happen to move the workbench from the
in-process MVP (documented in [runbook.md](../../public/operate/workbench-runbook.md)) to a dedicated
cluster node with durable storage. None of it changes the agent-facing
tool surface or the workspace semantics — `workbenchHost` and per-Plan
workspaces behave identically in both modes; only where the work
executes and where bytes land at rest changes.

## 1. What's already in the repo

The distributed split landed alongside the MVP so the architecture is
committed and exercised by tests, just not yet in active use:

| Layer | What | Files |
|-------|------|-------|
| Build target | `workbench` node-type binary (`make workbench`, ~84 MB) | `app/build_workbench.go`, `app/integrations_workbench.go`, `app/cluster_workbench.go`, `component/node/{bootstrap,compiled}_workbench.go` |
| gRPC envelopes | `WorkbenchForwardRequest` / `Response` / `Cancel` on `NodeService.Stream` | `component/node/node.proto` + regenerated `gen/` |
| Stream wiring | Inbound dispatch + outbound response routing on NodeServer, ParentConnector, WorkerDialer | `component/node/{server,stream_handler,parent_connector,worker_dialer}.go` |
| Agent-side router | `workbench.ForwardRouter` -- finds healthy workbench peer, sends, awaits response, falls back to local on `ErrNoWorkbenchPeer` | `integrations/workbench/forward_router.go` |
| Workbench-side handler | `workbench.ForwardHandler` -- unmarshals envelope, calls the local integration's dispatch, sends the response | `integrations/workbench/forward_handler.go` |
| AKS manifest stub | `workbench` Deployment placeholder (AKS-native; Cloud Run manifest removed) | `deploy/k8s/` |
| Local cluster | `workbench` Deployment + agent flipped to remote mode | `deploy/k8s/overlays/local` |

The toggle is one env var: `MEMQL_WORKBENCH_REMOTE=1` on the agent
node + a workbench address in `MEMQL_WORKER_PEERS`. With both set,
the integration's dispatch delegates to the remote node; without them
it stays local.

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

## 3. Environment topology (decisions locked #805)

memQL uses a **separate Azure Storage account per environment** —
strong blast-radius isolation with no cross-env data bleed.

| Environment | Storage backend | Account | Container | Status |
|-------------|----------------|---------|-----------|--------|
| **Local dev** | Azurite emulator (Docker) | `devstoreaccount1` (well-known) | `attachments` (created by init script) | Tracked: #806 |
| **Staging** | Azure Blob Storage | `stmemqlstaging` in `rg-memql-staging` | `attachments` | Tracked: #807 |
| **Prod** | Azure Blob Storage | Dedicated prod account | `attachments` | DEFERRED — blocked on prod go-live (#809) |

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

The in-cluster hostname is `azurite` (the k8s Service name). This value
is stamped into `~/Downloads/local.genesis.env` so `make genesis-seal`
bundles it into the sealed envelope and the seeded `memql-secrets` Secret
(`make up` / `make secrets`) lands it onto the agent pod. See
#806 for the full wiring.

### Staging

Staging uses the real `stmemqlstaging` storage account in
`rg-memql-staging`. The connection string is provisioned by the
`scripts/deploy/provision-blob-staging.sh` script (see #807). It is
not shared with local dev — each env carries its own sealed envelope.

### Prod (deferred)

Prod uses a separate account in the production resource group,
provisioned when prod goes live. The provisioning script and genesis
runbook from staging (#807) apply directly; only the account name and
resource group change. Tracked in #809.

## 4. Delivering the connection string (genesis envelope)

The connection string is a **shared cluster secret** — it follows the
genesis envelope process, not ad-hoc `kubectl set env`.

Canonical steps (staging example; local dev mirrors this with
`local.genesis.env`):

1. Edit `~/Downloads/staging.genesis.env` — add or update:
   ```
   MEMQL_AZURE_STORAGE_CONNECTION_STRING=<connection string>
   MEMQL_AZURE_BLOB_CONTAINER=attachments
   ```
2. Seal the envelope:
   ```bash
   make genesis-seal ENV_FILE=~/Downloads/staging.genesis.env
   ```
3. Re-store the sealed `genesis-b64` value in:
   - Key Vault (the canonical source of record), and
   - The `genesis` Kubernetes secret on the agent node.
4. Roll the agent pods so they pick up the new secret:
   ```bash
   kubectl rollout restart deployment/memql-agent -n memql
   ```

The vars must reach the **agent node** — that is the binary that runs
the workbench integration, the attachment upload handler, and
computer-use uploads. Verify injection via `app/transport_agent.go`.

Do NOT use `kubectl set env` directly — that creates config drift
outside the genesis envelope and breaks the sealed-secret source of
truth.

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
   and `MEMQL_AZURE_BLOB_CONTAINER` from the genesis secret (step 4
   above).

## 7. What's not yet implemented

Items deliberately deferred; revisit at production cutover:

- **Per-instance workspace isolation.** Today every workbench
  instance in a Plan sees the same directory tree. Multiple concurrent
  Tasks writing the same path use last-writer-wins. Fine for v1; add
  advisory locking if it bites.
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
- **Frontend visibility.** The cockpit and CoPresent UIs do not
  surface workbench contents. Add a Plan-level "Workbench outputs"
  panel when there is user demand. (Workspaces are agent-private by
  design; surfacing is optional.)

## 8. Rollback plan

If a cluster cutover causes issues, the rollback is one env var:
remove `MEMQL_WORKBENCH_REMOTE` from the agent Deployment and
re-deploy. The agent integration's dispatch falls back to local
in-process mode immediately. The workbench Deployment can stay
running idle (or be scaled to zero) without affecting agent
operation.

The MVP path is the default; production cutover is purely additive.
