# Argo CD — the GitOps reconciler (deployment-v2 Phase 2, #700)

Git is the single source of truth; **Argo CD** continuously reconciles each
environment's namespace to its committed digest-pinned overlay
(`deploy/k8s/overlays/<env>`, Phase 1 #699). **Deploys become merges.** Nobody
runs `kubectl set image` / `rollout undo` / ad-hoc applies against the mesh
anymore — Argo's `selfHeal` reverts any out-of-band change. Rollback =
`git revert` the overlay; Argo syncs back automatically.

```
   PR merges digest change to deploy/k8s/overlays/{prod,staging}  (on main)
                         │
              ┌──────────▼──────────────────────┐
              │  Argo CD (argocd ns)            │  project `memql`
              │  watches main                   │
              │   app memql-staging  -> ns memql-staging  (auto-sync)
              │   app memql-prod     -> ns memql-prod     (manual sync)
              └──────────┬──────────────────────┘
                         ▼
        TWO NAMESPACES, ONE CLUSTER, ONE BASE
```

## Two environments, one installation (epic memql#3748 / #3766)

Staging and production used to be two complete installations — two clusters,
two Argo CD estates, two of everything — and "promote" was copying image digests
between them. They are now **two namespaces in one cluster**, reconciled by one
Argo CD from one repository and **one base**. The overlays differ only in
**values**: namespace, the `memql-environment` ConfigMap's two entries, replica
counts, image digests. Diff
`deploy/k8s/overlays/{prod,staging}/kustomization.yaml` — that is all there is.

**The isolation that matters did not move to Kubernetes; it moved to the
database connection.** memQL has no tenancy dimension in the actor envelope, so
an environment boundary cannot be a filter. Each namespace's pods are put on a
different Postgres schema search path — `memql_prod, public` and
`memql_staging, public` — on every connection the driver opens
(`component/database/search_path.go`). A staging automation that fires and
writes lands in `memql_staging` because that is where the connection points,
which is the one place no application code path can forget.

Consequences worth knowing before operating this:

- **`public` is empty by design.** An unset or mistyped search path falls back
  to `"$user", public`. If production lived there, that slip would silently
  mean production; with both environments named, the same slip resolves to a
  schema holding nothing and the first statement fails loudly. `public` stays
  **on** the path (TimescaleDB's functions live in it) and **empty** of tables.
- **Both namespaces need their own out-of-band prerequisites**: a
  `memql-secrets` Secret, and — because `SecretStore` is namespace-scoped — a
  copy of `deploy/external-secrets/secretstore.yaml` in each namespace before
  base's `ExternalSecret`s can sync. The monitoring manifests under
  `deploy/k8s/monitoring/` declare their own `namespace: memql` and are applied
  separately, so they cover neither new namespace until copied.
- **`scripts/deploy/drift-check.sh` derives the namespace from `--env`** now
  (`prod` -> `memql-prod`, `staging` -> `memql-staging`, `local` -> `memql`),
  with `--namespace=` to override. It had to: `--live` compares the overlay's
  committed digests against running pods, and a drift detector pointed at a
  namespace the overlay does not deploy to reports "converged" against somebody
  else's workloads.
- **The Deployments console has NOT been threaded through yet.**
  `component/deploycontrol` still runs `kubectl -n memql` and reads the Argo
  app named `memql`, which is the downstream product pack's surface rather than
  either of these. Pointing it at an environment is engine-promotion work
  (memql#3769).

## Layout

| Path | What |
|---|---|
| `bootstrap/` | The one-time Argo CD install (pinned upstream v2.13.3) + `argocd` namespace. NOT GitOps-managed (chicken/egg). |
| `apps/project.yaml` | `AppProject memql` — restricts the reconciler to THIS repo + the `memql` / `memql-prod` / `memql-staging` / `argocd` namespaces. An Application whose destination namespace is missing here is **rejected at sync**. |
| `apps/root.yaml` | App-of-apps: renders the manifests its `directory.include` brace list names. **That list is the registration** — a manifest dropped into `apps/` without being added to it is silently not rendered, and the only symptom is an environment that is not running. |
| `apps/memql-staging.yaml` | The staging environment -> ns `memql-staging`, source `overlays/staging`, auto-sync. |
| `apps/memql-prod.yaml` | The production environment -> ns `memql-prod`, source `overlays/prod`, **manual sync**. |
| (moved) | The PRODUCT's own bff/SPA/rollouts Applications moved to the product pack repo at the P3 cutover (#2429); this dir keeps the engine estate. |

Both Applications exclude `/spec/replicas` from drift detection, which is what
makes `make scale N=<n> ENV=<env>` a runtime override `selfHeal` will not
revert — and what makes staging's scale-to-zero survive without manual repair.

## Bootstrap (one-time, per cluster)

Prereqs: `kubectl` context on the target cluster, and a `memql-secrets` Secret
in **each environment's namespace**. The namespaces themselves are reconciled
(`base/namespace.yaml`, renamed by each overlay), but the Secret carries real
values and is created out of band, so it has to exist before the first sync of
that environment.

```bash
# 1. Install Argo CD (pinned). Idempotent.
kubectl apply -k deploy/argocd/bootstrap
kubectl -n argocd rollout status deploy/argocd-server --timeout=300s

# 2. Give Argo READ-ONLY access to this (private) repo. Use a GitHub
#    deploy key (read-only) or a fine-grained PAT. Deploy-key route:
ssh-keygen -t ed25519 -N '' -f /tmp/argocd_memql_ro -C 'argocd-memql-ro'
gh repo deploy-key add /tmp/argocd_memql_ro.pub --repo znasllc-io/memql --title argocd-memql-ro   # read-only
kubectl -n argocd create secret generic repo-memql \
  --from-literal=type=git \
  --from-literal=url=git@github.com:znasllc-io/memql.git \
  --from-file=sshPrivateKey=/tmp/argocd_memql_ro
kubectl -n argocd label secret repo-memql argocd.argoproj.io/secret-type=repository
shred -u /tmp/argocd_memql_ro /tmp/argocd_memql_ro.pub 2>/dev/null || rm -f /tmp/argocd_memql_ro*
#    NOTE: if using SSH, set the Application repoURL to the git@ form to match.
#    For an HTTPS PAT instead, store url=https://github.com/... + password=<PAT>.

# 3. Apply the app-of-apps. This brings up the AppProject + both environment
#    Applications (memql-staging, memql-prod).
kubectl apply -f deploy/argocd/apps/root.yaml
```

### First sync = the watched tag→digest cutover

The live Deployments today run `:tag` image refs; the overlay pins the **same
image content** by `@sha256:` digest. So the memql app starts **OutOfSync**, and
the first sync rolls every Deployment **once** (content-identical) to move
tag→digest. It is protected by the #615 graceful drain (`maxUnavailable=0` +
SIGTERM drain) and the live autoscaler (#614), but **watch it**:

```bash
# Hold a sustained authenticated WS client against the product front door here
# (the zero-dropped-streams check) BEFORE triggering the sync.
argocd app sync memql-staging    # or: kubectl -n argocd patch app memql-staging --type merge -p '{"operation":{"sync":{}}}'
argocd app wait memql-staging --health
kubectl -n memql-staging rollout status deploy/identity   # then the rest
bash scripts/deploy/drift-check.sh --live --env=staging   # must report converged
```

After the first sync, `selfHeal` keeps the cluster pinned; subsequent deploys are
just merges to the overlay.

## Access the UI / CLI

```bash
kubectl -n argocd port-forward svc/argocd-server 8080:443 &
# initial admin password:
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo
argocd login localhost:8080 --username admin --insecure
```

## Prod on ArgoCD (#2207, re-homed by #3766)

Prod reconciles the same way as staging — a deploy is a digest bump **merged**
to `deploy/k8s/overlays/prod`, not a `kubectl set image` / `rollout undo`. The
difference: `apps/memql-prod.yaml` is on **manual sync** (no `automated:`
block) because prod is the highest-stakes surface. An operator triggers the sync
explicitly for every deploy; promote to auto-sync (`automated: {prune, selfHeal}`)
later once a couple of clean GitOps prod deploys have happened.

```
   PR merges digest change to deploy/k8s/overlays/prod  (on main)
                         │
                         ▼  (no auto-sync — operator triggers it)
              argocd app sync memql-prod
                         ▼
                   memql-prod namespace (SAME cluster as staging)
```

### Prod deploy (the steady-state flow)

```bash
# 1. Pin the prod digests: edit deploy/k8s/overlays/prod -> PR -> merge to main.
# 2. Confirm git is digest-pinned + the live cluster has no out-of-band drift:
bash scripts/deploy/drift-check.sh --live --env=prod      # must report converged
# 3. Hold a sustained authenticated WS client against the prod entrypoint here
#    (the zero-dropped-streams check) BEFORE the first tag->digest cutover sync,
#    exactly as the staging "First sync" section above describes.
argocd app sync memql-prod
argocd app wait memql-prod --health
# 4. Validation authority — the existing post-deploy functional gate:
bash scripts/deploy/post-deploy-gate.sh                   # the deploy is "good" only if this passes
```

Rollback = `git revert` the overlay change + re-sync (`argocd app sync
memql-prod`). On manual-sync there is no `selfHeal`, so the app's OutOfSync
status and `drift-check.sh --live --env=prod` are the drift authority until
auto-sync is enabled.

### Prerequisites (one-time, owner-gated)

Prod is **the same cluster as staging** since #3766 — a second namespace, not a
second estate — so the cluster registration and API-server placeholder this
section used to describe are gone. What remains before `memql-prod` can sync:

1. Ensure the `memql-prod` **`memql-secrets` Secret** exists (same out-of-band
   prereq as staging, in the new namespace). The Namespace itself is reconciled:
   `base/namespace.yaml` declares it and the overlay's `namespace:` field names
   it.
2. Create a **`SecretStore` in `memql-prod`** pointing at the prod Key Vault
   (copy `deploy/external-secrets/secretstore.yaml`, change the namespace and
   the `vaultUrl`). `SecretStore` is namespace-scoped, so base's
   `ExternalSecret`s cannot resolve without one.
3. **Pin the placeholder values in `deploy/k8s/overlays/prod`.** Two are
   deliberately unroutable rather than plausible-looking, so that reconciling
   before prod exists fails visibly: the `memql-mcp` all-zeros digest, and the
   livekit `NODE_IP` of `0.0.0.0`. Set them from real prod infrastructure and
   pin real digests for the rest.

Until those are done the app **fails closed**, which is the intended
not-yet-configured state.

The imperative `make deploy ENV=production` path stays available as a documented
**break-glass** escape hatch (its full demotion is tracked by #2205) until a
couple of clean GitOps prod deploys have happened.

## Break-glass (incident procedure)

When you MUST change the cluster faster than a PR (Argo would otherwise revert
you via `selfHeal`):

```bash
# 1. Suspend auto-sync on the affected app so selfHeal stops fighting you:
argocd app set memql-staging --sync-policy none
#    (or: kubectl -n argocd patch app memql-staging --type merge \
#         -p '{"spec":{"syncPolicy":{"automated":null}}}')
#    memql-prod is manual-sync already, so there is nothing to suspend there.

# 2. Do the emergency change (e.g. hotfix image) directly, IN THE ENVIRONMENT'S
#    OWN NAMESPACE — the two are `memql-staging` and `memql-prod`:
kubectl -n memql-staging <your emergency kubectl>
#    Scaling is NOT break-glass: /spec/replicas is excluded from drift detection
#    on both apps, so `make scale N=<n> ENV=<env>` needs none of this.

# 3. RECONCILE GIT TO MATCH within the same incident — commit the change to the
#    overlay so git stays the source of truth (otherwise the next re-enable
#    reverts your fix):
#       edit deploy/k8s/overlays/staging + PR + merge

# 4. Re-enable auto-sync:
argocd app set memql-staging --sync-policy automated --self-heal --auto-prune
```

Break-glass can never cause silent drift: step 4 re-converges to git, and
`drift-check.sh --live` (or Argo's own status) flags any gap.

## Uninstall / rollback the reconciler itself

```bash
kubectl delete -f deploy/argocd/apps/root.yaml     # removes the apps (finalizers cascade)
kubectl delete -k deploy/argocd/bootstrap          # removes Argo CD
```
Removing Argo does NOT touch the running mesh (the Deployments stay); it only
stops reconciliation. The overlay + `aks-deploy.sh`/`aks-apply.sh` remain a
working manual path.
