# Argo CD — the GitOps reconciler (deployment-v2 Phase 2, #700)

Git is the single source of truth; **Argo CD** continuously reconciles the
cluster's namespace to its committed digest-pinned overlay
(`deploy/k8s/overlays/cloud`, Phase 1 #699). **Deploys become merges.** Nobody
runs `kubectl set image` / `rollout undo` / ad-hoc applies against the mesh
anymore — Argo's `selfHeal` reverts any out-of-band change. Rollback =
`git revert` the overlay; Argo syncs back automatically.

```
   PR merges digest change to deploy/k8s/overlays/cloud  (on main)
                         │
              ┌──────────▼─────────────────────┐
              │  Argo CD (argocd ns)            │  project `memql`
              │  watches main                   │
              │   app memql  -> ns memql        │  (manual sync)
              └──────────┬─────────────────────┘
                         ▼
          ONE NAMESPACE, ONE CLUSTER, ONE BASE
```

## One installation shape (epic memql#3943)

MemQL has no staging-versus-production dimension. An operator who wants a second
environment installs a **second instance** — its own cluster or at least its own
Argo CD, its own domain, its own database — and that instance answers on its own
address. So there is one Application, one namespace and one overlay, and
everything in that overlay is a VALUE over `deploy/k8s/base`.

This reverses epic memql#3748, which put two environments in two namespaces of
one cluster and separated their DATA with a Postgres schema search path
(`memql_prod, public` / `memql_staging, public`). That search-path mechanism is
gone with the concept it bounded: with one installation there is one schema, and
the default path is correct.

Prerequisites worth knowing before operating this:

- **The namespace needs its own out-of-band prerequisites**: a `memql-secrets`
  Secret, and — because `SecretStore` is namespace-scoped — a copy of
  `deploy/external-secrets/secretstore.yaml` in it before base's
  `ExternalSecret`s can sync. The monitoring manifests under
  `deploy/k8s/monitoring/` declare their own `namespace: memql`.
- **`scripts/deploy/drift-check.sh`** compares the overlay's committed digests
  against running pods; `--namespace=` overrides the default.

## Layout

| Path | What |
|---|---|
| `bootstrap/` | The one-time Argo CD install (pinned upstream v2.13.3) + `argocd` namespace. NOT GitOps-managed (chicken/egg). |
| `apps/project.yaml` | `AppProject memql` — restricts the reconciler to THIS repo + the `memql` / `argocd` namespaces. An Application whose destination namespace is missing here is **rejected at sync**. |
| `apps/root.yaml` | App-of-apps: renders the manifests its `directory.include` brace list names. **That list is the registration** — a manifest dropped into `apps/` without being added to it is silently not rendered, and the only symptom is a cluster that is not running. |
| `apps/memql.yaml` | The cluster -> ns `memql`, source `overlays/cloud`, **manual sync**. |
| (moved) | The PRODUCT's own bff/SPA/rollouts Applications moved to the product pack repo at the P3 cutover (#2429); this dir keeps the engine estate. |

The Application excludes `/spec/replicas` from drift detection, which is what
makes `make scale N=<n>` a runtime override `selfHeal` will not revert — and
what makes a scale-to-zero survive without manual repair.

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

# 3. Apply the app-of-apps. This brings up the AppProject + the memql
#    Application.
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
argocd app sync memql    # or: kubectl -n argocd patch app memql --type merge -p '{"operation":{"sync":{}}}'
argocd app wait memql --health
kubectl -n memql rollout status deploy/identity   # then the rest
bash scripts/deploy/drift-check.sh --live        # must report converged
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

## The steady-state deploy

A deploy is a digest bump **merged** to `deploy/k8s/overlays/cloud`, not a
`kubectl set image` / `rollout undo`. `apps/memql.yaml` is on **manual sync**
(no `automated:` block) because this is the highest-stakes surface. An operator
triggers the sync explicitly for every deploy; promote to auto-sync
(`automated: {prune, selfHeal}`) later once a couple of clean GitOps deploys
have happened.

```
   PR merges digest change to deploy/k8s/overlays/cloud  (on main)
                         │
                         ▼  (no auto-sync — operator triggers it)
                  argocd app sync memql
                         ▼
                    memql namespace
```

```bash
# 1. Bump the {engine version, bundle digest, client digest} in the overlay,
#    commit that ONE file, open the PR, merge to main.
#    `scripts/deploy/pin-overlay-digests.sh` does the edit mechanically.
# 2. Confirm git is digest-pinned + the live cluster has no out-of-band drift:
bash scripts/deploy/drift-check.sh --live      # must report converged
# 3. Hold a sustained authenticated WS client against the entrypoint here
#    (the zero-dropped-streams check) BEFORE the first tag->digest cutover sync,
#    exactly as the "First sync" section above describes.
argocd app sync memql
argocd app wait memql --health
# 4. Validation authority — the existing post-deploy functional gate:
bash scripts/deploy/deploy-gate.sh        # the deploy is "good" only if this passes
```

Rollback = `git revert` the digest-bump commit + re-sync (`argocd app sync
memql`). That restores exactly the prior digests **because the bump is one
commit touching one file** — which is the property the one-tree layout buys and
the reason the console's rollback is a git operation rather than a second
forward deploy. On manual-sync there is no `selfHeal`, so the app's OutOfSync
status and `drift-check.sh --live` are the drift authority until auto-sync is
enabled.

> ### Trained constructs do NOT ride a deploy. Read this before filing a bug.
>
> A deploy moves **image digests**. A product's DSL bundle rides the same
> commit for the same reason — it is a data-only image mounted at
> `MEMQL_DSL_PATH`, so shipping it *is* pinning a digest.
>
> A **promoted construct** (memql#3746) is none of those things: it is a
> `v1:authoring:construct` **row** in this installation's database. Shipping an
> engine version does not create, move or delete one, and an installation you
> trained is the only installation that has it — MemQL ships one installation
> shape (epic memql#3943), so a second environment is a second install with its
> own database.
>
> Asserted on the mechanism by the deploy-pack effect tests, which hold that a
> deploy's entire effect is digest lines in one git-tracked file plus the ArgoCD
> reconciliation that follows.

### Prerequisites (one-time, owner-gated)

Before the `memql` app can sync:

1. Ensure the **`memql-secrets` Secret** exists in the `memql` namespace
   (out-of-band by design; see `deploy/k8s/base/secret.example.yaml`). The
   Namespace itself is reconciled: `base/namespace.yaml` declares it and the
   overlay's `namespace:` field names it.
2. Create a **`SecretStore` in `memql`** pointing at the cluster's Key Vault
   (copy `deploy/external-secrets/secretstore.yaml` and set the `vaultUrl`).
   `SecretStore` is namespace-scoped, so base's `ExternalSecret`s cannot resolve
   without one.
3. **Pin the placeholder values in `deploy/k8s/overlays/cloud`.** Two are
   deliberately unroutable rather than plausible-looking, so that reconciling
   before the cluster exists fails visibly: the `memql-mcp` all-zeros digest,
   and the livekit `NODE_IP` of `0.0.0.0`. Set them from real infrastructure and
   pin real digests for the rest.

Until those are done the app **fails closed**, which is the intended
not-yet-configured state.

The imperative `make deploy` path stays available as a documented **break-glass**
escape hatch (its full demotion is tracked by #2205) until a couple of clean
GitOps deploys have happened.

## Break-glass (incident procedure)

When you MUST change the cluster faster than a PR (Argo would otherwise revert
you via `selfHeal`):

```bash
# 1. Suspend auto-sync so selfHeal stops fighting you. The app ships on MANUAL
#    sync, so this is a no-op unless auto-sync was enabled:
argocd app set memql --sync-policy none
#    (or: kubectl -n argocd patch app memql --type merge \
#         -p '{"spec":{"syncPolicy":{"automated":null}}}')

# 2. Do the emergency change (e.g. hotfix image) directly:
kubectl -n memql <your emergency kubectl>
#    Scaling is NOT break-glass: /spec/replicas is excluded from drift detection,
#    so `make scale N=<n>` needs none of this.

# 3. RECONCILE GIT TO MATCH within the same incident — commit the change to the
#    overlay so git stays the source of truth (otherwise the next re-enable
#    reverts your fix):
#       edit deploy/k8s/overlays/cloud + PR + merge

# 4. Re-enable auto-sync (only if it was on):
argocd app set memql --sync-policy automated --self-heal --auto-prune
```

Break-glass can never cause silent drift: step 4 re-converges to git, and
`drift-check.sh --live` (or Argo's own status) flags any gap.

## Uninstall / rollback the reconciler itself

```bash
kubectl delete -f deploy/argocd/apps/root.yaml     # removes the apps (finalizers cascade)
kubectl delete -k deploy/argocd/bootstrap          # removes Argo CD
```
Removing Argo does NOT touch the running mesh (the Deployments stay); it only
stops reconciliation. Re-apply the overlay with `kubectl apply -k` only as
break-glass; GitOps is the sanctioned path. `aks-deploy.sh` is gone.
