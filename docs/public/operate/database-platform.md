---
title: Database platform (CloudNativePG) -- operator guide
audience: ops
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Database platform (CloudNativePG)

memQL runs its own PostgreSQL 16 + TimescaleDB Community + pgvector, managed by
**CloudNativePG**, in every environment — local k3d, staging, and production.
One operator, one operand image, one set of manifests; what differs per
environment is numbers and endpoints.

Epic memql#3842.

## The pieces

| Layer | Where | What it is |
|---|---|---|
| Operator stack | [`deploy/cnpg/`](../../../deploy/cnpg/README.md) | CloudNativePG + the Barman Cloud plugin + cert-manager, reconciled by ArgoCD |
| Operand image | [`deploy/db-image/`](../../../deploy/db-image/README.md) | PG16 + TimescaleDB Community + pgvector, built on the GitHub build server |
| The database itself | [`deploy/k8s/components/cnpg-db/`](../../../deploy/k8s/components/cnpg-db/README.md) | a kustomize component: Cluster + Database + ObjectStore + ScheduledBackup |
| Monitoring | [`deploy/k8s/monitoring/`](../../../deploy/k8s/monitoring/README.md) | PodMonitor on :9187 + the alerts below |

Local dev, including taking a backup and restoring into a scratch cluster:
[reproduce-staging-locally.md](reproduce-staging-locally.md#database-backups-and-a-restore-drill-local).

---

## What an operator must provision (per cloud environment)

None of this can live in a manifest, and each item is load-bearing.

### 1. A backup storage account — **in its own resource group**

| Setting | Value | Why |
|---|---|---|
| Redundancy | **ZRS** | zone-redundant: the backups must survive the zone the database is in |
| Access tier | **Cool** | backups are written once and read almost never. Retrieval is ~$0.01/GB, which is worth stating explicitly so nobody economises on restore drills |
| Blob versioning | **on** | an overwrite is recoverable |
| Soft delete (blobs + containers) | **on**, ≥ 30 days | a delete is recoverable, and matches the PITR window |
| Container | `memql-staging-db-backups` / `memql-prod-db-backups` | one per environment; the overlays name these |

**The cluster identity gets write, not purge.** Grant the federated managed
identity `Storage Blob Data Contributor` on this account and **do not** grant
delete/purge rights at the account level, and put the account in a resource
group the cluster's identity cannot manage.

The reasoning is the one that makes backups worth having: the identity that
writes the backups is exactly the credential an attacker who owns the cluster
holds. A backup that credential can delete is not a backup. Soft delete plus a
separate resource group is what turns "we have backups" into "we have backups
an intruder cannot remove".

### 2. A Premium SSD v2 StorageClass named `managed-csi-premium-v2`

AKS ships no PSSDv2 class by default; the staging and production overlays name
this one for both the data and WAL volumes.

**PSSDv2 is LRS/zonal-only, and that is correct here rather than a compromise.**
Zone redundancy comes from Postgres replication across zones — the instances are
spread by the preset's anti-affinity — so ZRS disks underneath would buy the same
property a second time and pay for it twice.

### 3. A federated managed identity for the database ServiceAccount

The instance pods authenticate to Blob as a workload identity, so **no storage
key exists in the cluster** to leak or rotate. Fill its client id into the
`azure.workload.identity/client-id` annotation in the environment's overlay —
both currently carry a `REPLACE-WITH-…` placeholder.

### 4. The operand image, built and pinned

The overlays pin the database image by **tag**, not digest, because the image
has never been cut on the build server. Before the first deploy:

1. run `.github/workflows/build-db-image.yml` (`workflow_dispatch`);
2. replace the tag with the `@sha256:` its run summary prints —
   `scripts/deploy/pin-overlay-digests.sh` does it mechanically.

Until then `scripts/deploy/drift-check.sh --rendered --env=staging` **fails**,
which is the correct answer to *"is this deployable?"* — and it can only give
that answer because memql#3847 taught the checker to read `imageName:` as well
as `image:`. Before that, the database image was the one image in the estate
outside the digest gate.

---

## What is deployed, per environment

| | local | staging | production |
|---|---|---|---|
| Preset | *(none — see below)* | `mid` | `top` |
| Instances | 1 | 2 | 3, one per zone |
| Resources | 100m / 512Mi | 2 vCPU / 8 GiB | 4 vCPU / 16 GiB |
| Storage (data + WAL) | 10 + 2 GiB | 128 + 32 GiB | 256 + 64 GiB |
| `max_connections` | 200 | 400 | 400 |
| Backups | Azurite, 7d | Azure Blob, 30d | Azure Blob, 30d |
| HA / PDB | off | on | on |

Local composes the component but **no preset**: the presets describe what a
customer buys, and a developer laptop is not a tier.

`max_connections` is ours to set now. Tiger Cloud's per-tier ceiling (~59 usable
slots) is what the [connection-budget standard](db-connection-budget.md) was
written around and what produced the 53300 storm; that constraint is gone.

---

## Alerts

Three failures, all of which leave the database **serving traffic perfectly**
while they are true — which is exactly why they need to page rather than be
noticed:

| Alert | Fires when | Why it is silent otherwise |
|---|---|---|
| `MemqlDatabaseWALArchivingFailing` | the archiver failed more recently than it succeeded, 5m | this one loses **data**, not availability. The first sign without it is a restore that cannot go back far enough |
| `MemqlDatabaseWALNeverArchived` | a cluster 30m old has never archived | the day-one case: a wrong `destinationPath` produces exactly this, and the alert above cannot see it (neither timestamp is set) |
| `MemqlDatabaseVolumeFillingUp` / `AlmostFull` | < 20% / < 10% free | Postgres **stops** rather than degrades when a volume fills |
| `MemqlDatabaseReplicaLagging` | > 5m behind, 10m | your failover target is worse than you think |
| `MemqlDatabaseReplicaNotStreaming` | in recovery with the WAL receiver down, 10m | not lagging — not replicating. Lag would read flat, so the rule above cannot see it |

A stalled archiver becomes a full WAL volume within hours: WAL is *retained*
rather than recycled while archiving is broken. Fix the archiver before resizing.

### Grafana dashboard

CloudNativePG publishes and maintains a dashboard; it is **imported rather than
vendored**, because it is large, upstream-maintained, and re-fetching it is a
one-liner — a stale 100 KB copy in this repo would be worse than a URL:

```bash
curl -sSLO https://raw.githubusercontent.com/cloudnative-pg/grafana-dashboards/main/charts/cluster/grafana-dashboard.json

kubectl create configmap cnpg-grafana-dashboard \
  --from-file=grafana-dashboard.json -n monitoring \
  --dry-run=client -o yaml \
| kubectl label -f- --local -o yaml grafana_dashboard=1 \
| kubectl apply -f -
```

The `grafana_dashboard=1` label is what the kube-prometheus-stack sidecar
watches. Panels are fed by the same `cnpg_*` metrics the alerts use, scraped via
[`podmonitor-database.yaml`](../../../deploy/k8s/monitoring/podmonitor-database.yaml).

---

## Connection pooling

**Not deployed, deliberately.** A PgBouncer `Pooler` is ready as an opt-in
component ([`cnpg-db/optional/pooler`](../../../deploy/k8s/components/cnpg-db/optional/pooler/kustomization.yaml))
and no overlay composes it.

Self-hosting removed the reason a pooler was mandatory. Adding one "for safety"
against a ceiling that no longer exists would make the system *less* reliable: a
second network hop, a second thing to fail over, a second place a connection can
be held.

Reach for it when `cnpg_backends_total` approaches `max_connections` in steady
state — particularly during a rollout, when old and new pods overlap and the
count roughly doubles. Pool rather than raise the ceiling: every Postgres
backend is a process with its own memory.

If you do enable it, point `MEMQL_DATABASE_DSN` at the pooler and leave
`MEMORY_NODES_DATABASE_DIRECT_DSN` on `memql-db-rw`. **Transaction-mode pooling
must never carry the migrations or `MEMQL_DB_SEARCH_PATH`** — the search path is
a session GUC and *is* the environment boundary, so pooling it would mean a
staging query landing on a connection still set to production's schema. That is
the one failure this architecture has no second line of defence against.

---

## Related

- [Operator stack + upgrade procedure](../../../deploy/cnpg/README.md) — an
  operator upgrade rolls every database pod; treat it as scheduled maintenance
- [Operand image](../../../deploy/db-image/README.md) — the two-`.so` TimescaleDB
  upgrade choreography and the tag constraint
- [DR runbook](../../internal/ops/dr-runbook.md) — recovery from the object store
- [TimescaleDB licence compliance](../../internal/ops/timescaledb-license-compliance.md)
  — why Community edition, and the positioning it obliges
