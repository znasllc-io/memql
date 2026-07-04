# Deploy-Bundle Runbook -- `make deploy` via deployEngineCluster

How to run a deployment of the PURE ENGINE mesh through the DSL deploy
bundle (`deployEngineCluster`, dsl/deployment/) from the cockpit, end to
end. This is the deploy-as-a-pack path proven live on 2026-07-04
(memql#2380): every phase -- authorize, record, clone, build, place,
gate, outcome, finalize, rollback -- runs as automation steps, and the
`v1:cluster:deployment` timeline in the target database is the record
of evidence.

Scope: engine mesh ONLY (identity / cognition / voice / agent / planner /
workbench / mcp / voice-agent). The CoPresent product stack (bff +
carriers + SPA) deploys via the release / promote.sh track -- see
[deployment-strategy.md](deployment-strategy.md).

## Prerequisites

1. **A cockpit binary** built from memql-cockpit main (`go build ./cmd/memql-cockpit/`).
2. **The target database DSN** in `MEMQL_DATABASE_DSN` -- the deploy
   runtime boots DB-backed and persists the deployment timeline there.
   For development this is the k3d postgres
   (`kubectl port-forward -n memql svc/postgres 5432:5432`, then
   `postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable`).
3. **A workdir that is a git clone** of the memql repo. The clone step
   fetches + checks out the requested ref IN this tree; it does not
   create it. For development a local-path clone avoids any credential
   concerns: `git clone /path/to/memql /tmp/deploy-workdir`.
4. **`MEMQL_DEPLOY_REPO_ROOT`** pointing at a memql checkout (the
   capability-script runner resolves the allowlisted scripts/* backends
   from it when the cockpit runs outside the repo).
5. For **staging/production**: an `az` session that can read the ACR
   (`acrmemql` by default; override with `MEMQL_DEPLOY_ACR`). The
   emitter resolves `payload.digests` from ACR by tag (memql#2381).

## The dry-run ladder (memql#2378)

| Invocation        | What happens |
|-------------------|--------------|
| `--dry-run`       | Resolve + preview only. Nothing executes. |
| *(default)*       | The automation EXECUTES; every capability script runs in its own dry-run mode and reports its intended work. Timeline rows ARE written. |
| `--apply`         | Scripts perform their work. The real deploy. |

An explicit `--input '{"dryRun": ...}'` wins over the flags.

## Development deploy (the k3d cluster)

The development flow builds images locally, imports them into k3d, and
gates on the mesh litmus (deployments healthy + unique per-pod
MEMQL_NODE_ID -- the `make status` check, selected by env inside
`scripts/deploy/deploy-gate.sh`).

IMPORTANT: pass `version=local` -- the local overlay pins
`memql-<type>:local` (`deploy/k8s/overlays/local/kustomization.yaml`),
so images built under any other tag import fine but never get pulled.
The gate catching exactly this mismatch is what it is for.

```bash
# one-time workdir
git clone /path/to/memql /tmp/deploy-workdir

MEMQL_DEPLOY_REPO_ROOT=/path/to/memql \
MEMQL_DATABASE_DSN="postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable" \
memql-cockpit deploy --env development --ref main --role owner --actor "$USER" \
  --apply --input '{"workdir":"/tmp/deploy-workdir","version":"local"}'
```

Or through make (quoting of --input JSON does not survive make ARGS;
pass simple inputs only, or invoke the binary directly as above):

```bash
make deploy ENV=development VERSION=main APPLY=1
```

## Staging / production (dry-run today; live is owner-gated)

```bash
memql-cockpit deploy --env staging --ref 0.11.2 --role owner --actor "$USER" \
  --input '{"workdir":"/tmp/deploy-workdir"}'
```

The emitter resolves real image digests from ACR (printed as INFO
lines), defaults `overlayPath=deploy/k8s/overlays/staging`, and the
bundle's GitOps branch (pinOverlayDigests + argoSync + post-deploy
gate) runs -- in dry-run reporting mode by default. The LIVE staging
exercise is deferred to the staging-rebuild decision (memql#2381).

## Reading the evidence

```sql
SELECT payload->>'deploymentId', payload->>'status', "createdAt"
FROM "MemoryNodes" WHERE concept = 'v1:cluster:deployment'
ORDER BY "createdAt" DESC;
```

One timeline per deploymentId: `in_progress` -> `succeeded` | `failed`.
The cockpit also emits an AUDIT line per invocation.

## Known gaps

- A mid-flight ACTION failure aborts the automation before finalize, so
  the timeline strands at `in_progress` (no terminal write). Re-running
  appends a fresh deploymentId.
- voice / voice-agent are GATED on LiveKit Cloud credentials locally
  (memql#2416): `make up` / `make secrets` scales them to 0 with a loud
  warning when `LIVEKIT_*` is not exported, and enables them when it is
  -- so a green gate never depends on an unprovisioned voice lane.
- `make deploy ARGS='--input {...}'` mangles JSON quoting; invoke the
  cockpit binary directly when passing structured input.
