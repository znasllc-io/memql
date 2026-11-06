---
title: Database platform (CloudNativePG) -- operator guide
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Database platform (CloudNativePG)

MemQL runs its own PostgreSQL 16 + TimescaleDB Community + pgvector, managed by
**CloudNativePG**, on every deploy target — local k3d and the cloud alike.
One operator, one operand image, one set of manifests; what differs per
environment is numbers and endpoints.

Epic memql#3842.

## The pieces

| Layer | Where | What it is |
|---|---|---|
| Operator stack | [`deploy/cnpg/`](../../../deploy/cnpg/README.md) | CloudNativePG + the Barman Cloud plugin + cert-manager, reconciled by ArgoCD |
| Operand image | [`deploy/db-image/`](../../../deploy/db-image/README.md) | PG16 + TimescaleDB Community + pgvector, built by the MemQL native pipeline |
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

**Put the account name into the overlay's `destinationPath`, in the HOST.** Both
cloud overlays ship
`https://REPLACE-WITH-STORAGE-ACCOUNT.blob.core.windows.net/memql-db-backups/`;
substitute the account you created above. This is not cosmetic and it is not
optional — barman decides which code path a destination takes from the host
alone, before it reads any credential, and a host that is a bare container name
is read as an **emulator** which then demands a connection string that workload
identity deliberately does not provide. The overlays carried the bare-container
form until memql#4460 and archived nothing for an instance's entire lifetime,
with a Ready `Cluster` and healthy pods throughout.
`TestBackupDestinationSurvivesBarmansEmulatorBranch` refuses the bare form now.

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

Build the operand through the native `tests.database-image` step, which selects
`deploy/db-image/Dockerfile`'s `verified` target and runs its smoke checks before
exporting an unpublished OCI archive. Prepare and approve a release candidate,
publish the exact archive, then pin the verified tag and digest in the instance
overlay. The tag must begin with the PostgreSQL major; the independent operand
version comes from `deploy/db-image/VERSION`.

`scripts/deploy/pin-overlay-digests.sh` resolves the image pins, and
`scripts/deploy/drift-check.sh --rendered` rejects unpinned database images as
well as service images. Publication does not roll the database or upgrade its
extensions: those remain reviewed instance changes.

---

## What is deployed, per target

| | local | cloud |
|---|---|---|
| Preset | *(none — see below)* | `top` |
| Instances | 1 | 3, one per zone |
| Resources | 100m / 512Mi | 4 vCPU / 16 GiB |
| Storage (data + WAL) | 10 + 2 GiB | 256 + 64 GiB |
| `max_connections` | 400 | 400 |
| Backups | Azurite, 7d | Azure Blob, 30d |
| HA / PDB | off | on |

Local composes the component but **no preset**: the presets describe what a
customer buys, and a developer laptop is not a tier. It sets its own values over
the component, `max_connections` among them: 400, so that two replicas of every
node (`make scale N=2`) fit. The component's own 200 is the entry preset's.

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
| `MemqlMigrationLockStuck` | a migrating node has found the same row in `bun_migration_locks` for 15m | no later migration or post-migration hook runs while every pod stays Ready ([recovery](env-vars.md#database-tuning)) |

A stalled archiver becomes a full WAL volume within hours: WAL is *retained*
rather than recycled while archiving is broken. Fix the archiver before resizing.
That sequence is not hypothetical — it is memql#4459 exactly, and it is why the
`entry` tier's WAL volume is 32 GiB rather than 16.

**A full WAL volume does not degrade the database, it prevents it from
starting.** CNPG has a free-space guard that refuses to launch Postgres at all
rather than start an instance that will wedge:

```
Not enough WAL disk space, avoid starting PostgreSQL
error: no free disk space for WALs
```

So the recovery is not "restart it" — the pod will `CrashLoopBackOff`
indefinitely, and on memql#4459 it did so 225 times over ~18 hours. Expand the
PVC first (online for both storage classes in use), confirm the reported
capacity has actually grown, and only then delete the pod. Everything that
depends on the database crashloops behind it, so the visible symptom is the
front door serving 503s rather than anything naming Postgres.

### Verifying a backup actually landed

**No alert here covers backup age, deliberately.** The CNPG metrics that would
carry one (`cnpg_collector_last_available_backup_timestamp` and its two
siblings) were deprecated in v1.26 and **do not update at all under
plugin-based backups**, which is the mode MemQL runs in everywhere — so they sit
at zero forever and a rule built on one fires permanently or never.

An `ObjectStore` reports whether it *parsed*, never whether a byte was written.
The check that means something reads the container:

```bash
make db-backup-verify ACCOUNT=<storage-account>
make db-backup-verify ACCOUNT=<storage-account> MAX_AGE_HOURS=48
```

It reports base-backup and WAL-archive ages separately (they fail
independently), and it distinguishes *"nothing has ever been written"* from
*"I was not allowed to look"* — Azure storage RBAC is data-plane, so
subscription **Owner grants no read on blobs**, and a refused listing must
never be reported as a missing backup.

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

## MemoryNodes compression

`MemoryNodes` is meant to compress its cold tail. The document version history
migration (`20260609000000`) asks for compression segmented by `concept`,
ordered by `"createdAt" DESC`, and a policy that compresses a chunk once it is
90 days old. **No instance has ever had it** (memql#5421), for three independent
reasons, each measured on TimescaleDB 2.29:

- On a fresh install the table is not yet a hypertable when that migration
  runs, so the migration skipped it.
- On an older install, where it was, the migration's policy call named the
  table unquoted, failed, and rolled the whole block back with a notice.
- Since the work spine's recovery heads (`20260926020000_work_run_heads`), the
  table carries `work_run_head_delete`: an `AFTER DELETE` trigger with a
  transition table. TimescaleDB refuses compression on a hypertable with a
  DELETE trigger of that kind, in both directions — compression cannot be
  enabled while it exists, and it cannot be created on a compressed table.

**What a node does about it.** On every boot, after the post-migration hook
makes `MemoryNodes` a hypertable, the node applies exactly those settings and
that policy — but only when the table has no compression settings and carries
no DELETE trigger with a transition table. It records what it decided on the
hook's status row, as `payload.compression.MemoryNodes`: `enabled`,
`already enabled`, `blocked: DELETE trigger with a transition table: <names>`,
`skipped: ...`, or `failed: ...` (a failure is retried by the next boot). It
waits at most two seconds for the table lock and gives up rather than hold
other queries behind it.

**Today every instance reports `blocked`**, and nothing about it changes. The
block lifts when the work spine captures deletes some other way: a row-level
`AFTER DELETE` trigger coexists with compression, and fires for rows deleted
out of a compressed chunk (measured). `SecretMemoryNodes` is not compressed, and
nothing intends it to be.

### Checking an instance

```sql
-- What the node decided at its last boot
SELECT payload->'compression'->>'MemoryNodes' AS compression
  FROM "MemoryNodes"
 WHERE id = 'system.timescaledb.status'
 ORDER BY "createdAt" DESC
 LIMIT 1;

-- The catalog's own answer
SELECT hypertable_name, compression_enabled
  FROM timescaledb_information.hypertables
 WHERE hypertable_schema = 'public'
   AND hypertable_name IN ('MemoryNodes', 'SecretMemoryNodes');

-- The policy, and whether it has run
SELECT j.job_id, j.schedule_interval, j.config->>'compress_after' AS compress_after,
       s.last_run_status, s.last_successful_finish, s.next_start
  FROM timescaledb_information.jobs AS j
  LEFT JOIN timescaledb_information.job_stats AS s ON s.job_id = j.job_id
 WHERE j.hypertable_schema = 'public'
   AND j.hypertable_name = 'MemoryNodes'
   AND j.config->>'compress_after' IS NOT NULL;

-- What blocks it, if anything
SELECT tgname
  FROM pg_trigger
 WHERE tgrelid = '"MemoryNodes"'::regclass
   AND NOT tgisinternal
   AND (tgtype & 8) <> 0
   AND tgoldtable IS NOT NULL;

-- How much of the table is compressed
SELECT count(*) FILTER (WHERE is_compressed) AS compressed_chunks, count(*) AS chunks
  FROM timescaledb_information.chunks
 WHERE hypertable_schema = 'public'
   AND hypertable_name = 'MemoryNodes';
```

### When the block lifts: plan that first boot

The first boot after the block lifts enables compression on every instance,
fresh or years old, with no migration. The policy job then runs **within
seconds**, not at its 12-hour interval, and compresses every chunk more than 90
days old. On an instance with a long history that is one background job over
the whole cold tail — CPU and I/O on the database for as long as it takes — so
treat that release like scheduled maintenance.

- **Reads** keep their meaning: a compressed chunk is decompressed
  transparently. Reads that reach the cold tail are slower, which is the trade
  the migration chose.
- **Ordinary writes** are appends: a mutation writes a new version and never
  rewrites an old one.
- **Anything that `UPDATE`s or `DELETE`s old rows decompresses what it
  touches**, and TimescaleDB caps that per transaction
  (`timescaledb.max_tuples_decompressed_per_dml_transaction`, 100000 by
  default; measured to refuse a 150000-row statement outright). The migration
  runner's connections lift the cap, because a concept-scoped repair migration
  is exactly that shape. Request-serving connections keep it, and the runtime
  paths that delete or rewrite old rows run there: the retention sweeps
  (authActivity, the work journal), the readiness purge, knowledge seed
  cleanup, and the Shopify compliance jobs. A batched sweep stays under the
  cap; a single statement over a large, cold, compressed set does not, and has
  to batch or lift the cap for its own transaction
  (`SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0`).
- A later migration that adds a DELETE trigger with a transition table to
  `MemoryNodes` **fails** on an instance where compression is on.

### Stopping it

Remove the policy and keep the settings:

```sql
SELECT remove_compression_policy('"MemoryNodes"'::regclass);
```

A node leaves a table that has compression settings alone, so the policy stays
removed across boots. Do **not** turn compression itself off
(`ALTER TABLE ... SET (timescaledb.compress = false)`): the next boot would put
it back.

## Closed-chunk compaction

`MemoryNodes` and `SecretMemoryNodes` are append-only hypertables in 7-day
chunks on `"createdAt"`, and inserts only ever reach the newest chunk. Space a
`DELETE` frees in a **closed** chunk is therefore reused by nothing and never
returned to the volume: every retention whose window is longer than a chunk
(declared `@retention` TTLs, the work journal's 30 days, pipeline runs) deletes
rows without giving back disk, and so did the October 14 schema conversion,
whose updates shrank rows. Production measured five closed chunks holding about
7.1 GB free, one of them 7.7 GB with 3.3 GB live.

**Deletes in closed chunks return space only through this job.** The nightly
`reclaimClosedChunkSpace` automation (04:20 UTC, on the cron leader, after the
03:20 log and 03:40 work-journal retention) calls the cluster-owner builtin
`closedChunkCompaction`, which rewrites **at most one** closed chunk per night
with TimescaleDB's `reorder_chunk` and then runs `VACUUM (ANALYZE)` on it (a
rewrite does not set the visibility map index-only reads need). No row is
changed: `reorder_chunk` swaps a finished copy in or discards it.

### How a pass decides

1. **Closed chunks only:** the chunk's range ended at least a day ago, and it is
   not compressed.
2. **Measure, once per change.** A pass scans the heap of a chunk that has no
   current measurement, summing `pg_column_size` of every column (a TOASTed
   value's stored size is read off its pointer, not fetched) plus 44 bytes a
   row. At most 4 chunks a pass, none started after 15 minutes, chunks never
   measured first and then the oldest measurement, so none is starved. Measurements
   live in `chunk_compaction_progress` and are reused while the chunk's size and
   its insert/update/delete counter (`pg_stat_user_tables`) still read what they
   read then, for at most 30 days; a retention delete moves the counter, so the
   next pass measures that chunk again.
3. **Worth it:** the chunk would give back at least 256 MiB **and** 30% of its
   size (the automation's arguments; the handler's floors are 16 MiB and 5%).
4. **Room, twice over.** The pass needs room for the chunk's live data plus its
   indexes, twice (the second copy covers a sort spilling to disk). Capacity
   comes from the `database-health` report (the one `clusterStorageStatus`
   reads) when it was observed within 15 minutes; usage is the larger of the
   report's and the live sum of `pg_database_size`. Without a fresh report the
   pass uses `MEMQL_CHUNK_COMPACTION_VOLUME_BYTES`, if set, against the live
   sizes; with neither it rewrites nothing (`room_unknown`), and a real pass
   stops before measuring anything. 5% of the volume is
   treated as unusable (ext4's root reserve). With a fresh report the WAL volume
   must also hold the rewrite's WAL once. Of the chunks that are worth it, the
   pass takes the one giving back most **that fits**: a smaller chunk rewritten
   tonight is room for a larger one later.
5. **Owner:** `reorder_chunk` requires the chunk's owner. The engine's role
   creates `MemoryNodes` in its migrations, so it owns the hypertable and every
   chunk; a pass that finds otherwise answers `not_owner`, naming both roles.
6. **WAL archive keeping up.** A rewrite writes WAL roughly the size of what it
   rewrites, and `database-health` fails above 64 segments awaiting archiving.
   The pass reads `pg_stat_archiver` and the current WAL position (the engine's
   role cannot call `pg_ls_archive_statusdir()`, which needs `pg_monitor`) and
   refuses when more than 4 segments wait, when the archiver's latest attempt
   failed, when segments wait and nothing was archived for 10 minutes, or when
   the backlog cannot be read; a fresh report naming an `archive-*` problem
   refuses too.

### What a rewrite does to readers and writers

`reorder_chunk` copies the chunk while reads carry on, holding an
`ExclusiveLock` that makes a `DELETE` into that chunk wait. The swap at the end
needs an `AccessExclusive` lock; the pass runs with `lock_timeout = 1s`, so a
reader in the way makes the rewrite fail within a second, with the chunk intact,
instead of queueing every later reader behind the swap. A lock timeout is
retried up to three times in a pass, 30 s apart, re-reading the WAL guard
first. A rewrite is bounded at 40 minutes and a pass at 55; anything that ends
a rewrite leaves the chunk as it was, and the chunk then waits a day before the
next attempt, doubling to at most 16 days while it keeps failing. The hourly
declared-retention sweep at `:40` can see one batch end at its own statement
timeout while a long rewrite holds the chunk; it retries the next hour.

A pass takes one extra database connection for its duration, cluster-wide: an
advisory lock admits one pass at a time, and the session is opened on the
direct endpoint without the request pool's statement timeout
([connection budget](db-connection-budget.md)).

### Reading what it did

The builtin answers `{ outcome, reason, detail, chunk, measured, eligible,
room, wal }`. `outcome` is `compacted`, `planned` (a dry run that would
rewrite), `skipped` or `failed`; `reason` names why nothing was rewritten
(`disabled`, `already_running`, `reorder_unavailable`, `no_candidate`,
`room_unknown`, `no_room`, `not_owner`, `wal_backlog`, `wal_unknown`,
`no_reorder_index`, `out_of_time`) or why a rewrite failed (`lock_timeout`,
`statement_timeout`, `cancelled`, `error`). A failure is logged at `WARN`
naming the chunk; a pass that rewrites nothing logs its reason at `INFO`.
`memql_database_chunk_compaction_runs_total{outcome}` and
`memql_database_chunk_compaction_reclaimed_bytes_total` count passes and bytes
given back.

```sql
-- What the passes measured and did, newest first
SELECT chunk, live_bytes, measured_total_bytes, measured_at,
       outcome, failures, attempted_at, compacted_at, detail
  FROM chunk_compaction_progress
 ORDER BY coalesce(attempted_at, measured_at) DESC;
```

### Running it, and stopping it

- **Preview** as a cluster owner: `closedChunkCompaction(dryRun: true)`
  measures, judges room, ownership and WAL, and reports what a pass would do,
  writing nothing (no rewrite, no progress row).
- **Run now:** `closedChunkCompaction()` takes the nightly thresholds by default
  and holds the call until the pass ends. A run started from the automation's
  Run button is bounded at five minutes at most, and a rewrite is only started
  with five minutes of the pass left, so such a run measures and answers
  `out_of_time`; call the builtin instead.
- **Stop the nightly rewrites:** set `MEMQL_CHUNK_COMPACTION_ENABLED=false` in
  the installation's overlay and roll the pods. Each pass then answers
  `disabled`; dry runs still report.
- **Stop a rewrite in progress:** cancel its backend. The statement rolls back
  and the chunk stays as it was:

  ```sql
  SELECT pg_cancel_backend(pid)
    FROM pg_stat_activity
   WHERE query LIKE '%reorder_chunk(%' AND pid <> pg_backend_pid();
  ```

  Restarting the pod running the pass has the same effect within ten seconds
  (the session sets `client_connection_check_interval`).

---

## Related

- [Operator stack + upgrade procedure](../../../deploy/cnpg/README.md) — an
  operator upgrade rolls every database pod; treat it as scheduled maintenance
- [Operand image](../../../deploy/db-image/README.md) — the two-`.so` TimescaleDB
  upgrade choreography and the tag constraint
- [DR runbook](../../internal/ops/dr-runbook.md) — recovery from the object store
- [TimescaleDB licence compliance](../../internal/ops/timescaledb-license-compliance.md)
  — why Community edition, and the positioning it obliges
