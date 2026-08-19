---
title: Database platform (CloudNativePG) -- operator guide
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Database platform (CloudNativePG)

memQL runs its own PostgreSQL 16 + TimescaleDB Community + pgvector, managed by
**CloudNativePG**, on every deploy target — local k3d and the cloud alike.
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
[reproduce-the-cloud-locally.md](reproduce-the-cloud-locally.md#database-backups-and-a-restore-drill-local).

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
| Container | `memql-db-backups` | the overlay names it |

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

AKS ships no PSSDv2 class by default; the cloud overlay names
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

Until then `scripts/deploy/drift-check.sh --rendered ` **fails**,
which is the correct answer to *"is this deployable?"* — and it can only give
that answer because memql#3847 taught the checker to read `imageName:` as well
as `image:`. Before that, the database image was the one image in the estate
outside the digest gate.

---

## What is deployed, per target

| | local | cloud |
|---|---|---|
| Preset | *(none — see below)* | `top` |
| Instances | 1 | 3, one per zone |
| Resources | 100m / 512Mi | 4 vCPU / 16 GiB |
| Storage (data + WAL) | 10 + 2 GiB | 256 + 64 GiB |
| `max_connections` | 200 | 400 |
| Backups | Azurite, 7d | Azure Blob, 30d |
| HA / PDB | off | on |

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

## Cutover from Tiger Cloud

The one-time migration off the managed service. Written to be executed rather
than read: every step has a check, and the rollback is one command until the
soak ends.

**Why a maintenance-window dump/restore rather than replication.** The cluster held
~78k `MemoryNodes` rows at the last DR rehearsal and production does not exist
yet, so the data is small enough that a window costs less than the machinery of
logical replication — and a dump/restore has a rollback that is *doing nothing*,
which replication does not.

### Before the window

1. **The new cluster is running and healthy.** `make status` shows the database
   litmus green: phase, extensions applied, archiving OK.
2. **A backup has completed** on the new cluster, and
   `make db-restore-drill` passes. Do not migrate onto a database
   whose recovery path is unproven.
3. **Record the source counts** — these are what step 6 compares against:

   ```bash
   psql "$TIGER_DSN" -c 'SELECT count(*) FROM "MemoryNodes";'
   psql "$TIGER_DSN" -c 'SELECT count(*) FROM timescaledb_information.continuous_aggregates;'
   psql "$TIGER_DSN" -c 'SELECT count(*) FROM timescaledb_information.hypertables;'
   ```

### The window

4. **Quiesce.** `make scale N=0`. Nothing should be writing while
   the dump runs; a dump taken under writes is a dump whose row counts will not
   match and whose consistency you then have to reason about.

5. **Dump and restore.**

   ```bash
   pg_dump --format=custom --no-owner --no-privileges "$TIGER_DSN" > staging.dump

   # TimescaleDB REQUIRES the pre/post restore bracket. Without it the restore
   # replays the extension's own catalog rows as ordinary data and the
   # hypertables come back as plain tables -- which restores "successfully" and
   # silently loses every hypertable, chunk and continuous aggregate.
   kubectl exec -n memql memql-db-1 -c postgres -- \
     psql -U postgres -d memql -c "SELECT timescaledb_pre_restore();"

   pg_restore --no-owner --no-privileges --dbname "$CNPG_DSN" staging.dump

   kubectl exec -n memql memql-db-1 -c postgres -- \
     psql -U postgres -d memql -c "SELECT timescaledb_post_restore();"
   ```

6. **Spot-check against step 3.** Row count, hypertable count, and continuous
   aggregate count must all match. A mismatch in the *third* number with the
   first two correct is the pre/post bracket having been skipped.

7. **Swap the DSNs in Key Vault** — `memory-nodes-database-dsn` and the direct
   DSN — to `memql-db-rw`. ESO propagates on its refresh interval; force it
   with `kubectl -n memql annotate externalsecret memql-secrets
   force-sync=$(date +%s) --overwrite`.

8. **Roll the mesh, identity first.** Pods read their environment once, at
   start: `kubectl rollout restart deploy/identity -n memql`, wait for
   it, then the rest. Mind that ArgoCD's image sync re-applies replicas —
   `make scale N=2` brings the mesh back to its committed width.

9. **Confirm.** `/readyz` green across the mesh; the three database alerts
   quiet; `make status` litmus green.

### Rollback, until the soak ends

**Swap the Key Vault DSNs back and roll identity first.** That is the whole
procedure. Tiger stays running and untouched through the soak precisely so this
stays a one-command answer — which is why deprovisioning is a *separate,
later* step rather than part of the cutover.

### Ending it

Soak **1–2 weeks** with the alerts quiet. Then deprovision the Tiger service
(`xahn9ru4v6`) and accept the data purge. Note that `teardown-staging.sh` no
longer has a `--database` tier: there is nothing left for it to deprovision.

## Proving HA: the failover litmus

**HA is a property you either measure or merely believe.** A CNPG cluster
reports `instances: 3` and *"Cluster in healthy state"* whether or not a
promotion would actually work — a replica in recovery with a dead WAL receiver
counts toward that number. The only way to know is to take the primary away and
watch.

```bash
make db-failover-litmus CONFIRM=kill-the-primary
```

`ENV` defaults to **local** for the same reason it does on `make scale`: this
target deletes a pod, and a remote default would aim a habit at production. The
phrase is typed at the make level *and* passed through to the capability script,
so neither layer is the one that made a destructive act easy.

It asserts three separate things:

| # | Assertion | Why it is separate |
|---|---|---|
| 1 | a **new primary** is promoted within the timeout | the property HA is bought for. Timed, because "eventually" is not failover |
| 2 | a row **committed before** the kill is present after it | a replica far enough behind is promoted just as readily, and the cluster looks equally healthy afterwards. This is the outcome that would make the promotion worthless |
| 3 | every instance returns to ready, streaming again | a cluster that promoted but never rebuilt its replica has spent its redundancy and now **reports HA while having none** |

It **refuses** a single-instance cluster with exit 4 rather than failing it:
there is nothing to promote to, so killing the primary is not a failover test —
it is an outage with a script watching.

**Run it on production bring-up acceptance, and after every operator upgrade.**
An operator upgrade rolls every database pod, which is exactly when you want
promotion proven rather than assumed.

## Tabletop: losing a whole availability zone

Production runs three instances, one per zone. **No restore is needed** — this
is the case the third instance exists for.

**Expected behaviour.** The zone's instance disappears. If it held the primary,
CNPG promotes one of the two survivors; writes pause for the promotion window
and resume. If it held a replica, nothing pauses. The cluster runs degraded
(2/3) until the zone returns or Kubernetes reschedules onto a surviving zone —
and it can reschedule, because the storage is per-instance rather than shared.

**Verification steps.**

1. `kubectl get cluster memql-db -n memql` — expect `2/3 ready` and a
   `currentPrimary` in a surviving zone.
2. Confirm the application recovered: `/readyz` green, no sustained error rate.
3. `MemqlDatabaseReplicaNotStreaming` should be **quiet** — the two survivors
   must still be replicating to each other. If it fires, the cluster has one
   working instance and no redundancy.
4. Confirm WAL archiving is unaffected (`MemqlDatabaseWALArchivingFailing`
   quiet). The backup account is ZRS, so it survives the zone independently.
5. When the zone returns, the third instance rejoins and rebuilds. Watch
   `readyInstances` return to 3 before treating the incident as closed.

**What would make this go wrong.** The `top` preset sets
`whenUnsatisfiable: DoNotSchedule` on the zone spread deliberately: it is better
for the third instance to sit `Pending` than for two instances to land in one
zone while the cluster reports three. A cluster that quietly co-located two
instances would fail this tabletop *at the moment of the outage*, having looked
correct until then.

## Cost

Recorded here because "cheaper" was the premise of the whole epic and a premise
nobody measures stops being true quietly.

| | Tiger Cloud (list) | Self-hosted (list) |
|---|---|---|
| Production, 4 CPU HA | ~$1,606/mo | ~$322–454/mo |
| Per-client instance | ~$393/mo | ~$65–91/mo |

Verified against live Tiger and Azure pricing on 2026-08-14; the epic's target
is **≤ ~$460/mo list** for production (~$330 on a savings plan) and **≤ ~$40/mo**
for the cluster.

Two things to note when checking the actual bill:

- **The savings-plan purchase for the database node pool is a billing action,
  not a manifest change.** Nothing in this repository can make it happen.
- **Storage grows forever.** `MemoryNodes` never evicts, and the storage line is
  the one that moves without anybody changing anything. That was true on Tiger
  too — at $0.883/GB-month, 11× Azure disk — which is part of why this epic
  exists.

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
must never carry the migrations** — they take advisory locks, and a lock
acquired in one transaction and released in another is, on a pooled connection,
released by whoever holds that backend next.

---

## Related

- [Operator stack + upgrade procedure](../../../deploy/cnpg/README.md) — an
  operator upgrade rolls every database pod; treat it as scheduled maintenance
- [Operand image](../../../deploy/db-image/README.md) — the two-`.so` TimescaleDB
  upgrade choreography and the tag constraint
- [DR runbook](../../internal/ops/dr-runbook.md) — recovery from the object store
- [TimescaleDB licence compliance](../../internal/ops/timescaledb-license-compliance.md)
  — why Community edition, and the positioning it obliges
