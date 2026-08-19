# CloudNativePG operator stack

The operator every environment's database runs on. Local k3d and the cloud
install reconcile **this one directory** through ArgoCD; what differs per
environment is the `Cluster` CR in that environment's overlay — instances,
resources, storage, backup destination — which is values, not shape.

Epic memql#3842, task memql#3845.

| Component | Pin | Namespace |
|---|---|---|
| CloudNativePG operator | `v1.30.0` | `cnpg-system` |
| Barman Cloud plugin (CNPG-I) | `v0.14.0` | `cnpg-system` |
| cert-manager (prerequisite) | `v1.21.1` | `cert-manager` — see [`../cert-manager/install`](../cert-manager/install/kustomization.yaml) |

## How it is reconciled

```
deploy/argocd/apps/cert-manager.yaml    (sync-wave -2)  -> deploy/cert-manager/install
deploy/argocd/apps/cnpg-operator.yaml   (sync-wave -1)  -> deploy/cnpg/install
deploy/argocd/apps/memql.yaml                           -> the overlay's Cluster CR
```

Both Applications are registered in `deploy/argocd/apps/root.yaml`'s
`directory.include` brace list. **That list is the registration** — a manifest
dropped into that directory is not rendered until its filename appears there,
and the failure is silent: the Application simply does not exist.

Locally, `scripts/k3d/up.sh` applies the same two manifests directly (ArgoCD
reconciles them either way) and waits for each before registering the mesh
Application.

### The ordering is load-bearing

The Barman plugin's manifest declares `Certificate` and `Issuer` objects for its
mTLS with the operator. On a cluster without cert-manager's CRDs served, that
Application does not degrade — **it fails to apply**. Hence the sync-waves in the
cloud and an explicit wait locally.

### Why `project: default`

The `memql` AppProject exists to bound what the *mesh* reconciler may do: three
namespaces and a near-empty `clusterResourceWhitelist`. An operator installs
CRDs, ClusterRoles, and webhook configurations by nature, so it cannot live
inside that boundary without widening it until it bounds nothing. `root.yaml`
itself runs on `default` for the same reason. A third AppProject with a wide-open
whitelist would grant identical access while implying it were tighter.

### Why `prune: false` on both

Auto-pruning an operator that owns CRDs deletes the CRDs, and with them every
object of those kinds. For CNPG that is `clusters.postgresql.cnpg.io` — pruning
it removes every `Cluster` in the estate, and the operator then tears down the
StatefulSets and PVCs behind them. Removing an operator is a deliberate act, not
something a reconcile should be able to do because a manifest momentarily failed
to render. `selfHeal` stays **on**: it reverts drift in the controller
Deployment, which is not the same risk.

## Operator upgrades are scheduled database maintenance

**An operator upgrade rolls every database pod in the cluster.** Treat it as a
maintenance window, not a routine dependency bump.

CNPG supports roughly a **six-month window per minor**, and staying patch-current
is not optional — CVE-2026-44477 is the precedent. Falling behind means an
emergency upgrade under time pressure, which is the worst moment to be rolling
every database pod for the first time in months.

### Procedure

1. **Read the upstream release notes** for every minor between the current pin
   and the target, specifically for CRD changes and for a stated minimum
   Kubernetes version.
2. **Bump the pin** in `install/kustomization.yaml` (both the tag in the URL and
   the filename, which carries the version). One PR, operator only — do not
   combine it with a TimescaleDB bump or a Cluster CR change.
3. **Local first.** `make up-refresh` on a clean k3d cluster, then confirm
   `kubectl -n cnpg-system get deploy` is available and an existing local
   `Cluster` reaches `Cluster in healthy state`.
4. **The cloud install, in a maintenance window.** Watch for the rolling
   restart to complete on every instance, confirm the three alerts from
   memql#3847 stay quiet, and run the failover litmus afterwards
   (memql#3850) — an operator upgrade is exactly when you want promotion
   proven rather than assumed.

Upgrading the **Barman Cloud plugin** is the same shape but cheaper: it is a
sidecar, so the blast radius is backups rather than availability. Verify a backup
completes and a restore drill passes (memql#3849) after bumping it.

## PodDisruptionBudgets

CNPG creates a PDB per Cluster by default, which is correct for a
multi-instance cluster: it stops a node drain from taking the primary and its
only replica at once.

**Set `enablePDB: false` for a single-instance Cluster.** With `instances: 1`
the default PDB permits zero disruptions, so a node drain — `kubectl drain`, a
k3d node restart, an AKS node-pool upgrade — **blocks forever** on a pod nothing
can safely evict. The local overlay runs one instance and therefore disables it;
the cloud overlay runs two or three and leaves it on.

This is the single most common way a CNPG install becomes mysteriously
un-drainable, and the symptom (a drain that hangs with no error) points nowhere
near the cause.

## What is deliberately not used

**The in-tree `barmanObjectStore` stanza.** It is deprecated in CNPG; backups go
through the Barman Cloud **plugin** instead. That choice propagates to the
operand image: it is built on CNPG's `standard` base rather than the deprecated
`system` base, which is the one carrying in-tree Barman binaries. Picking the
plugin with a `system` image, or the in-tree stanza with a `standard` image, are
both half-configurations that fail at the first backup rather than at apply time.
See [`../db-image/README.md`](../db-image/README.md).
