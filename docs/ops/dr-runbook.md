# Disaster recovery runbook (deployment-v2 Phase 5, #703)

Rehearsed recovery for the memQL + CoPresent stack on AKS. Three independent
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
git log --oneline -- deploy/k8s/overlays/staging
# Revert the bad change and push; Argo reconciles (or apply manually pre-Argo):
scripts/deploy/aks-rollback.sh --to=<bad-commit>      # prints the exact steps
git revert --no-edit <bad-commit> && git push
# Verify convergence:
scripts/deploy/drift-check.sh --live --env=staging    # -> converged
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

## 3. Database PITR (managed Tiger Cloud)

The database is managed **Tiger Cloud** (Timescale) with point-in-time recovery.
It is firewalled to in-cluster egress, so recovery is driven from the Tiger
console / CLI, not a workstation.

**Procedure:**
1. In the Tiger console (or `tiger` CLI), **fork/restore** the service to a
   timestamp just before the incident → a new service id.
2. Update `memory-nodes-database-dsn` in **Key Vault** to the restored service's
   connection string; ESO propagates it (or DEPLOYMENT_STRATEGY §4 manual path).
3. Roll the mesh so pods pick up the new DSN:
   `kubectl rollout restart deploy -n memql` (or let Argo re-sync).
4. Run the readiness gate: `scripts/deploy/drift-check.sh --live` is image-only;
   for schema, hit `/readyz` (the #657 schema assertion) — a 200 `ready` proves
   the restored DB has the critical schema.

**Rehearsal (safe):** fork the staging DB to a recent timestamp into a throwaway
service, point a *scratch* deploy at it, confirm `/readyz` is green, then discard
the fork. Do **not** repoint live staging during a drill.

## Recovery objectives (staging targets; tighten for prod)

| Domain | Mechanism | RTO (target) | RPO |
|---|---|---|---|
| Deploy/config | git revert + reconcile / Rollout auto-abort | minutes | 0 (git) |
| Secrets | ESO re-sync from Key Vault | minutes | 0 (KV durable) |
| Database | Tiger Cloud PITR fork + DSN swap | depends on data size | per Tiger PITR window |

## Pre-prod checklist

- [ ] §1 git-revert rollback rehearsed on staging.
- [ ] §2 ESO secret re-sync rehearsed.
- [ ] §3 Tiger PITR fork rehearsed into a scratch service + `/readyz` green.
- [ ] Prod RTO/RPO targets agreed with the owner; prod Key Vault + Tiger PITR
      window confirmed.
