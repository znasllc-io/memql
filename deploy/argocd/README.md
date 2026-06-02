# Argo CD — the GitOps reconciler (deployment-v2 Phase 2, #700)

Git is the single source of truth; **Argo CD** continuously reconciles the
`memql` namespace to the committed digest-pinned overlay (`deploy/k8s/overlays/<env>`,
Phase 1 #699). **Deploys become merges.** Nobody runs `kubectl set image` /
`rollout undo` / ad-hoc applies against the mesh anymore — Argo's `selfHeal`
reverts any out-of-band change. Rollback = `git revert` the overlay; Argo syncs
back automatically.

```
   PR merges digest change to deploy/k8s/overlays/staging  (on main)
                         │
              ┌──────────▼───────────┐
              │  Argo CD (argocd ns)  │  app `memql` (project `memql`)
              │  watches main         │  source = overlays/staging
              └──────────┬───────────┘
                  sync (auto, prune+selfHeal)
                         ▼
                   memql namespace
```

## Layout

| Path | What |
|---|---|
| `bootstrap/` | The one-time Argo CD install (pinned upstream v2.13.3) + `argocd` namespace. NOT GitOps-managed (chicken/egg). |
| `apps/project.yaml` | `AppProject memql` — restricts the reconciler to THIS repo + the `memql`/`argocd` namespaces. |
| `apps/root.yaml` | App-of-apps: renders everything under `apps/`, so adding an app is a PR. |
| `apps/memql.yaml` | The mesh + CoPresent Application; source = `deploy/k8s/overlays/staging`. |

## Bootstrap (one-time, per cluster)

Prereqs: `kubectl` context on the target cluster; the `memql` namespace +
`memql-secrets` already exist (they do on staging).

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

# 3. Apply the app-of-apps. This brings up the AppProject + the memql Application.
kubectl apply -f deploy/argocd/apps/root.yaml
```

### First sync = the watched tag→digest cutover

The live Deployments today run `:tag` image refs; the overlay pins the **same
image content** by `@sha256:` digest. So the memql app starts **OutOfSync**, and
the first sync rolls every Deployment **once** (content-identical) to move
tag→digest. It is protected by the #615 graceful drain (`maxUnavailable=0` +
SIGTERM drain) and the live autoscaler (#614), but **watch it**:

```bash
# Hold a sustained authenticated WS client against app.staging.copresent.ai here
# (the zero-dropped-streams check) BEFORE triggering the sync.
argocd app sync memql            # or: kubectl -n argocd patch app memql --type merge -p '{"operation":{"sync":{}}}'
argocd app wait memql --health
kubectl -n memql rollout status deploy/identity   # then the rest
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

## Break-glass (incident procedure)

When you MUST change the cluster faster than a PR (Argo would otherwise revert
you via `selfHeal`):

```bash
# 1. Suspend auto-sync on the affected app so selfHeal stops fighting you:
argocd app set memql --sync-policy none
#    (or: kubectl -n argocd patch app memql --type merge \
#         -p '{"spec":{"syncPolicy":{"automated":null}}}')

# 2. Do the emergency change (e.g. scale, hotfix image) directly:
kubectl -n memql <your emergency kubectl>

# 3. RECONCILE GIT TO MATCH within the same incident — commit the change to the
#    overlay so git stays the source of truth (otherwise the next re-enable
#    reverts your fix):
#       edit deploy/k8s/overlays/staging + PR + merge

# 4. Re-enable auto-sync:
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
stops reconciliation. The overlay + `aks-deploy.sh`/`aks-apply.sh` remain a
working manual path.
