# Workbench Production Deployment (Future Work)

**Status:** Deferred. The code is committed and tested; deployment
is gated on the broader production rollout. Cross this bridge when
you're ready to deploy memQL services to GCP Cloud Run in a
multi-node topology.

This doc catalogs what needs to happen to move the workbench from
the in-process MVP (documented in [runbook.md](runbook.md)) to a
dedicated cluster node with durable storage. None of it changes
the agent-facing tool surface or the workspace semantics --
`workbenchHost` and per-Plan workspaces behave identically in
both modes; only where the work executes changes.

## 1. What's already in the repo

The distributed split landed alongside the MVP so the architecture
is committed and exercised by tests, just not yet in active use:

| Layer | What | Files |
|-------|------|-------|
| Build target | `workbench` node-type binary (`make workbench`, ~84 MB) | `app/build_workbench.go`, `app/integrations_workbench.go`, `app/cluster_workbench.go`, `component/node/{bootstrap,compiled}_workbench.go` |
| gRPC envelopes | `WorkbenchForwardRequest` / `Response` / `Cancel` on `NodeService.Stream` | `component/node/node.proto` + regenerated `gen/` |
| Stream wiring | Inbound dispatch + outbound response routing on NodeServer, ParentConnector, WorkerDialer | `component/node/{server,stream_handler,parent_connector,worker_dialer}.go` |
| Agent-side router | `workbench.ForwardRouter` -- finds healthy workbench peer, sends, awaits response, falls back to local on `ErrNoWorkbenchPeer` | `integrations/workbench/forward_router.go` |
| Workbench-side handler | `workbench.ForwardHandler` -- unmarshals envelope, calls the local Integration's dispatch, sends the response | `integrations/workbench/forward_handler.go` |
| Cloud Run config | gen2 service with GCS-FUSE volume at `/var/lib/memql/workbenches` | `infra/cluster/service.workbench.yaml` |
| Docker compose | `workbench` service + named volume + agent flipped to remote mode | `docker/docker-compose.cluster.yml` |

The toggle is one env var: `MEMQL_WORKBENCH_REMOTE=1` on the agent
node + a workbench address in `MEMQL_WORKER_PEERS`. With both set,
the integration's dispatch delegates to the remote node; without
them it stays local.

## 2. Cluster mode -- local validation

Before the Cloud Run cutover, exercise the cluster path locally:

```bash
docker compose -f docker/docker-compose.cluster.yml up --build
```

This spins up bff + cognition + agent + planner + voice + identity
+ workbench. The agent service is pre-configured with
`MEMQL_WORKBENCH_REMOTE=1` and a `workbench=workbench:50060` seed
so dispatches route over `NodeService.Stream` to the dedicated
binary. Workspaces persist in the `workbench_workspaces` Docker
named volume.

Smoke test: same procedure as the runbook -- ask an agent to write
a file -- then verify it landed on the workbench container:

```bash
docker compose -f docker/docker-compose.cluster.yml exec workbench \
  ls /var/lib/memql/workbenches/
```

## 3. Cloud Run cutover

The Cloud Run service is defined in
`infra/cluster/service.workbench.yaml`. It is a gen2 deployment
(required for GCS-FUSE) with the workbench bucket mounted at
`/var/lib/memql/workbenches`.

### 3.1 Provision the backing bucket (one-time)

```bash
PROJECT=fast-fire-486523-f3
BUCKET=memql-workbench-data

gcloud storage buckets create gs://${BUCKET} \
  --project=${PROJECT} \
  --location=us-central1 \
  --uniform-bucket-level-access \
  --soft-delete-duration=0d

gcloud storage buckets add-iam-policy-binding gs://${BUCKET} \
  --member="serviceAccount:memql-runtime@${PROJECT}.iam.gserviceaccount.com" \
  --role=roles/storage.objectUser \
  --project=${PROJECT}
```

### 3.2 Build + push the image

```bash
make workbench
docker build --build-arg BUILD_TAGS=workbench \
  -t us-central1-docker.pkg.dev/${PROJECT}/memql/memql-workbench:latest .
docker push us-central1-docker.pkg.dev/${PROJECT}/memql/memql-workbench:latest
```

(Wire this into the same image-build pipeline that produces the
existing memql-agent / memql-cognition / memql-planner images.)

### 3.3 Deploy

```bash
gcloud run services replace infra/cluster/service.workbench.yaml \
  --region=us-central1 \
  --project=${PROJECT}
```

### 3.4 Flip the agent service

Add to `infra/cluster/service.agent.yaml`:

```yaml
- name: MEMQL_WORKBENCH_REMOTE
  value: "1"
- name: MEMQL_WORKER_PEERS
  value: "workbench=memql-workbench:50058"
```

(Adjust the workbench address to whatever the Cloud Run internal
DNS resolves to. The existing AI / voice forwarding patterns in
the cluster do the same dance.)

## 4. Storage substrate considerations

The Cloud Run config uses **GCS-FUSE** because:

- Cloud Run instances are ephemeral; local disk doesn't survive
  revision deploys or instance restarts. Per-Plan workspaces must
  outlive both.
- GCS gives durable per-object storage; FUSE makes it look like
  POSIX. Latency for small reads/writes is fine for the workbench's
  workload (notes, scratch files, command outputs).
- Costs scale linearly with stored data, not provisioned
  capacity. No "we paid for 1 TiB but use 12 GiB" failure mode.

Alternative we considered:

- **Filestore (managed NFS).** Strong POSIX semantics. Rejected
  for v1 because the minimum size is 1 TiB and the floor cost is
  ~$200/month -- overkill for the workload.
- **Cloud Run instance-local disk.** Free + fast. Rejected
  because workspaces would die on every revision deploy and any
  autoscale-down event, violating the "workspace lives as long as
  the parent Plan" agreement.

If POSIX edge cases bite (atomic rename, hard links, etc.), the
Filestore path stays open.

## 5. What's NOT in the committed code

A few production-deployment-adjacent items deliberately not yet
implemented; revisit when the cutover lands:

- **Per-instance workspace isolation.** Today every workbench
  instance reads the same GCS bucket; multiple instances reading
  the same Plan see the same files. That's deliberate for
  multi-Task collaboration but means we don't yet have a story
  for two concurrent Tasks writing the same path (last-writer
  wins). Likely fine for v1; add advisory locking if it bites.
- **Resource quotas per Plan.** Today the workbench has global
  size + timeout caps but no per-Plan disk quota. A misbehaving
  agent could fill the bucket. Add `Plan.workbenchQuotaBytes` +
  a sweep that checks usage before allowing further writes.
- **Network egress policy.** `http_fetch` currently allows
  arbitrary outbound HTTP. Add an allowlist (or deny-by-default
  + agent opt-in) before going production.
- **Image hardening.** The default base image is whatever the
  Dockerfile picks. Decide on Alpine vs distroless+busybox vs
  Ubuntu-minimal; preinstall the runtimes (`curl`, `git`,
  Python, Node) the prompt's `workbench:environment` chunk
  promises agents will find.
- **Audit telemetry.** No equivalent of `v1:worker:invocation`
  for workbench calls yet. Decide whether high-volume telemetry
  is needed at all for the sandboxed path -- it's a different
  threat model than the user's machine.
- **Frontend visibility.** The cockpit / copresent UIs don't
  surface the workbench at all. Add a Plan-level "Workbench
  contents" panel when there's user demand. (Optional;
  workspaces are agent-private by design.)

## 6. Rollback plan

If the cluster cutover causes issues, the rollback is one env
var: remove `MEMQL_WORKBENCH_REMOTE` from the agent service
config and re-deploy. The agent integration's dispatch will fall
back to local in-process mode immediately. The workbench Cloud
Run service can stay running idle (or be deleted) without
affecting agent operation.

The MVP path is still the default; production cutover is purely
additive.
