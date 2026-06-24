---
title: Staging Teardown + Rebuild Runbook
audience: internal
status: stable
area: ops
owner: znas
---

# Staging Teardown + Rebuild Runbook

How to shut the **staging** environment all the way down to stop Azure +
Tiger Cloud charges, and how to bring it back. First authored 2026-06-24
(the first staging teardown).

Teardown is scripted: [`scripts/staging/teardown-staging.sh`](../../../scripts/staging/teardown-staging.sh)
(tiered, dry-run-first, typed-confirmation).

## What bills in staging

Subscription `Pay-As-You-Go` (`19f85801-...`), region `eastus`, RG
`rg-memql-staging`:

| Resource | Kind | Cost | Teardown tier |
|----------|------|------|---------------|
| `aks-memql-staging` | AKS, node pool 4× `Standard_B2s` (autoscale 2–5) | **dominant compute** | `--compute` |
| 5× Standard public IPs (in node RG `MC_rg-memql-staging_...`) | networking | ~$18/mo | deleted with the cluster |
| `xahn9ru4v6` (`memql-staging`) | Tiger Cloud TimescaleDB | **dominant data** | `--database` |
| `stmemqlstaging` | Storage (Azure Blob) | small | `--ancillary` |
| `workspace-rgmemqlstagingm904` | Log Analytics | small | `--ancillary` |
| `cae-memql-staging` | Container Apps env (empty) | ~none | `--ancillary` |
| `acrmemql` | ACR (all release images) | small | `--registry` (KEEP by default) |
| `kv-memql-staging` | Key Vault (master key + genesis envelope) | ~none | `--keyvault` (KEEP by default) |
| `id-eso-memql` | Managed identity (ESO) | free | n/a |

**Default teardown (`--all`) keeps ACR + Key Vault** — they are
rebuild-critical (images + sealed secrets) and cost little. Add
`--registry` / `--keyvault` only for a permanent decommission.

## Teardown

```bash
# Dry-run first (prints every command, executes nothing):
bash scripts/staging/teardown-staging.sh --all

# Execute the full nuke (keeps ACR + Key Vault), interactive confirm:
bash scripts/staging/teardown-staging.sh --all --confirm

# Non-interactive (CI / owner one-shot):
bash scripts/staging/teardown-staging.sh --all --confirm --yes
```

What `--all` does, in order:
1. `az aks delete` — removes the cluster and its node RG (all node VMs,
   the 5 public IPs, LoadBalancers, the `identity-keys` PVC disk, the
   ArgoCD install, and ESO).
2. `tiger service delete xahn9ru4v6` — deprovisions the DB. **All data is
   purged.**
3. Deletes the storage account, the empty Container Apps env, and the Log
   Analytics workspace.

Verify zero charges remain:
```bash
az aks list -o table                       # empty
tiger service list                         # xahn9ru4v6 gone
az resource list -g rg-memql-staging -o table   # only acrmemql + kv-memql-staging (+ id-eso)
```

## Rebuild

Staging is GitOps-reconciled, so rebuild is: recreate the cluster shell,
re-provision the DB, re-seed the bootstrap secret, point ArgoCD at the
overlay, sync.

1. **Tiger Cloud DB** — `tiger service create --name memql-staging --region az-eastus2 ...`
   (or the Tiger console). Capture the new pooler + direct connection
   strings; the service id changes, so update the `base/kustomization.yaml`
   provisioning comment if you keep it as the reference.
2. **AKS cluster** — recreate `aks-memql-staging` in `rg-memql-staging`
   (node pool `Standard_B2s`, autoscale 2–5). Re-attach ACR pull
   (`az aks update --attach-acr acrmemql`).
3. **Bootstrap secret `memql-secrets`** — recreate from Key Vault
   (`kv-memql-staging` still holds the genesis envelope `MEMQL_GENESIS_B64`
   + `MEMQL_MASTER_KEY`) plus the new DB connection strings. The current
   live secret uses LEGACY DSN keys (`MEMORY_NODES_DATABASE_DSN` /
   `_DIRECT_DSN`); the boot-time alias shim bridges them to
   `MEMQL_DATABASE_DSN`, OR seed the new key names directly (preferred).
   See `deploy/k8s/base/kustomization.yaml` (the `kubectl create secret`
   recipe) and `docs/public/operate/env-vars.md`.
4. **ArgoCD + ESO** — install ArgoCD, apply the root app
   (`deploy/argocd/apps`), install ESO. The `memql` app reconciles
   `deploy/k8s/overlays/staging` (currently pinned to the 0.11.2 digest
   set in `releases/0.11.2.yaml`).
5. **Sync** — `argocd` hard-refresh + sync; migrations run on first boot
   and recreate the (additive) schema. No data is restored — staging
   comes back empty.

Note: the `#2115` identity automation-driven-deploy flag and the livekit
`NODE_IP` patch live in the staging overlay and come back with the sync;
`NODE_IP` (a public IP) will be a NEW address after rebuild — update the
overlay patch to the new livekit IP.
