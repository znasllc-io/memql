---
title: Disaster recovery runbook (deployment-v2 Phase 5, #703)
audience: ops
status: stable
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Disaster recovery runbook (deployment-v2 Phase 5, #703)

Rehearsed recovery for the memQL + product stack on AKS. Three independent
recovery domains — **deploy/config**, **secrets**, **database** — each with a
rehearsal you can run on staging without risk.

> Convention: rollback is **`git revert` + reconcile**, never imperative
> `kubectl rollout undo` (deployment-v2 Phase 1, #699). Argo CD (Phase 2)
> reconciles; the digest overlay is the source of truth.

## 1. Deploy / config rollback (git revert)

A bad release (regression, failed gate) is recovered by reverting the overlay
commit; Argo CD reconciles the previous digests back.

**Rehearsal (staging, safe):**
```bash
# Pick the previous good overlay commit:
git log --oneline -- deploy/k8s/overlays/cloud
# Revert the bad change and push; Argo reconciles (or apply manually pre-Argo):
scripts/deploy/aks-rollback.sh --to=<bad-commit>      # prints the exact steps
git revert --no-edit <bad-commit> && git push
# Verify convergence:
scripts/deploy/drift-check.sh --live     # -> converged
```
Recovery time = one reconcile cycle. No image rebuild, no `rollout undo`.

Under a progressive Rollout (Phase 3), a failed `AnalysisRun` **auto-aborts** to
the stable ReplicaSet — recovery is automatic and needs no human step.

## 2. Secret recovery (Key Vault → ESO)

The sealed genesis envelope + master key + DSN live in **Key Vault**
(`kv-memql-staging`); External Secrets (Phase 5) reconciles them into
`memql-secrets`. Losing the in-cluster Secret is recovered by re-sync, not by
hunting for the values.

**Rehearsal (staging — do in a window; pods re-read on restart):**
```bash
# Simulate loss of the in-cluster Secret material (ESO restores it):
kubectl -n memql annotate externalsecret memql-secrets force-sync=$(date +%s) --overwrite
kubectl -n memql get externalsecret memql-secrets -w        # -> SecretSynced
# Confirm the keys are present (not the values):
kubectl -n memql get secret memql-secrets -o jsonpath='{.data}' | tr ',' '\n' | sed 's/:.*//'
```
If ESO is unavailable, the manual fallback is DEPLOYMENT_STRATEGY §4 (re-seal →
`az keyvault secret set` → `kubectl patch`). **Key Vault is the durable source**;
the cluster Secret is a cache.

> Signing-key compromise: rotate the Ed25519 seed (re-seal into the envelope,
> push to Key Vault, roll identity). This invalidates every outstanding JWT of
> every class — clients re-auth. See identity-service.md + service-account-jwt.md.

## 3. Database recovery (self-hosted CloudNativePG)

The database is **self-hosted CloudNativePG** (epic memql#3842), with continuous
WAL archiving and nightly base backups to Azure Blob. Recovery is a **new
Cluster bootstrapped from the object store** — never an in-place operation on
the damaged one, which is what makes it safe to attempt while the incident is
still being understood.

> This section replaced a Tiger Cloud console/CLI procedure. The mechanism is
> now entirely in-cluster and declarative, so the recovery path and the drill
> are the same manifests with different names.

### What recovery looks like

```yaml
# A NEW Cluster. The damaged one is left alone -- it is evidence, and it may
# still be serving reads.
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: memql-db-recovered
  namespace: memql
spec:
  instances: 3
  imageName: <the SAME image the source ran>
  postgresql:
    shared_preload_libraries: [timescaledb]
  storage: { size: 256Gi }
  # NO WAL ARCHIVER while recovering -- see the warning below.
  plugins: []
  bootstrap:
    recovery:
      source: origin
      # Omit recoveryTarget to restore to the LATEST available point.
      recoveryTarget:
        targetTime: "2026-08-16T04:30:00Z"
  externalClusters:
    - name: origin
      plugin:
        name: barman-cloud.cloudnative-pg.io
        parameters:
          barmanObjectName: memql-db-backup
          serverName: memql-db          # the SOURCE cluster's name
```

> **The recovered cluster must not archive into the store it was restored
> from.** Given the Barman plugin as its WAL archiver it will write its own
> timeline into the same object store, and the corruption is invisible until the
> *next* recovery picks the wrong one. Bring it up with `plugins: []`, confirm
> the data, and only then attach an archiver — pointed at a fresh
> `serverName`, or after the old cluster is retired.

Then:

1. **Verify before you switch.** `psql` into the recovered primary and confirm
   the critical schema (`"MemoryNodes"`, `automation_execution_claims` — the
   #657 `/readyz` assertion) and the continuous aggregates
   (`timescaledb_information.continuous_aggregates`), plus a row count that
   matches expectations. A database that accepts connections is not a restored
   database.
2. **Swap the DSN** in Key Vault (`memory-nodes-database-dsn`) to the recovered
   cluster's `-rw` Service; ESO propagates it.
3. **Roll the mesh, identity first** — pods read their environment once, at
   start: `kubectl rollout restart deploy/identity -n <ns>` then the rest.
4. **Confirm** `/readyz` is green across the mesh.

### Point-in-time recovery

`recoveryTarget.targetTime` restores to any moment inside the retention window
(**30 days** in staging and production). Continuous WAL archiving is what makes
that continuous rather than a nightly granularity.

**Retrieval from Cool-tier Blob is ~$0.01/GB**, which is stated here so nobody
economises on drills. The cost of a rehearsal is rounding error against the cost
of discovering the restore path is broken during an incident.

### Rehearsal — scripted, and safe by construction

```bash
make db-restore-drill
```

`scripts/deploy/db-restore-drill.sh` restores the latest backup into a **scratch
namespace**, asserts the restored data is genuinely usable, reports the measured
RTO, and tears the namespace down — including on failure, unless `--keep` is
passed for a post-mortem. It never touches the live cluster and never repoints a
DSN.

It asserts four things, and the last two are the ones a naive check misses:

| Assertion | What it catches |
|---|---|
| the cluster reaches Ready from a recovery bootstrap | the restore path itself. **This is the measured RTO.** |
| the critical schema is present | a database that answers connections but would fail `/readyz` |
| the continuous aggregates survived | TSL-only objects — exactly what a restore onto the wrong image loses while everything else looks fine |
| row counts are non-zero, and reported | an **empty-but-valid** restore, which a schema check alone waves through |

The drill cluster carries `plugins: []` for the reason in the warning above: a
routine drill must not be able to pollute the backups it exists to validate.

### Schedule it

**Quarterly, minimum**, and after any change to the backup path (a new storage
account, an operator upgrade, a credential rotation). A backup's existence
proves nothing; its restore proves everything — and the interval between drills
is exactly how stale your belief about recoverability is allowed to get.

The drill emits a single JSON envelope on stdout, so a CI schedule or a cron
automation can consume it directly and alert on `ok: false`.

## Recovery objectives (staging targets; tighten for prod)

| Domain | Mechanism | RTO (target) | RPO |
|---|---|---|---|
| Deploy/config | git revert + reconcile / Rollout auto-abort | minutes | 0 (git) |
| Secrets | ESO re-sync from Key Vault | minutes | 0 (KV durable) |
| Database — **instance loss** | CNPG promotes a replica | **seconds**, measured by `make db-failover-litmus` | 0 for committed writes |
| Database — **zone loss** | promotion within the surviving zones; no restore | seconds | 0 for committed writes |
| Database — **data loss / corruption** | new Cluster from the object store + PITR | measured by the restore drill; scales with data size | **≤ 5 min** (continuous WAL archiving; tighter with `archive_timeout`) |

The database now has **three** rows rather than one because it now has three
distinct failure modes with three different answers. Conflating them is how an
instance loss gets treated as a restore — turning a seconds-long promotion into
an hours-long recovery for no reason.

## Pre-prod checklist

- [x] §1 git-revert rollback rehearsed on staging (#712). `aks-rollback.sh --to`
      prints the revert; a scratch revert restored prior pinned digests
      (`drift-check --rendered` OK). The progressive-Rollout **auto-abort** path
      was also proven live: a failed deploy-gate auto-rolled-back to the stable
      color with zero human step and zero dropped streams.
- [x] §2 ESO secret re-sync rehearsed (#712). Forced `force-sync` on the
      `memql-secrets` ExternalSecret -> `SecretSynced`; the Secret recovered
      **byte-identical** from Key Vault (sha256 match) -- KV is the durable source.
- [x] §3 Tiger PITR fork rehearsed (#712) -- **superseded**. Kept as the record
      of the last rehearsal on the managed service: the fork came up `READY`
      with both critical-schema tables present and 77,945 `MemoryNodes` rows
      intact. That mechanism no longer exists; the rows below replace it.
- [x] §3 CNPG restore drill SCRIPTED and proven (memql#3849). The drill restores
      the latest backup into a scratch namespace, asserts the critical schema,
      the continuous aggregates and a non-zero row count, reports the measured
      RTO, and tears down. Exercised end to end against a real cluster with real
      backups; the source cluster's archiving was confirmed unaffected
      afterwards, which is the property `plugins: []` on the drill cluster
      exists to protect.
- [x] §3 CNPG failover proven (memql#3850). `make db-failover-litmus` killed the
      primary of a two-instance cluster: promoted in **2s**, the write committed
      before the kill survived, and both instances were back at **17s**.
- [ ] The restore drill run against **real staging Blob backups**, and its
      timings recorded here. Needs a live staging cluster; the script is ready
      and takes no arguments beyond `ENV=staging`.
- [ ] Quarterly drill scheduled (CI schedule or cron automation consuming the
      drill's JSON envelope and alerting on `ok: false`).
- [ ] Prod RTO/RPO targets agreed with the owner; prod Key Vault + the 30-day
      Blob retention window confirmed against the table above.
