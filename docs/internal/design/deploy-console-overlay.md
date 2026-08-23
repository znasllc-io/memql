---
title: Where an in-cluster deploy console gets its overlay
audience: internal
status: draft
area: design
sinceVersion: 0.19.6
owner: znas
---

# Where an in-cluster deploy console gets its overlay

> **Status.** This is the design record epic memql#4275 asked for. Its central
> question — which of three substrates the deploy console should use — is
> **decided and implemented** (memql#4257), and the answer is a fourth option
> the epic did not list. What remains is the narrower question the fourth
> option does not answer: **how a checkout reaches the pod**, for the two verbs
> that genuinely need one. That part is **not done** and is scoped below.

## The problem as filed

`component/deploycontrol/service.go:405-409` reads
`<repoRoot>/deploy/k8s/overlays/cloud/kustomization.yaml`, and no node in any
cluster has one:

1. the overlay dir is a hardcoded literal (`service.go:62-66`);
2. `MEMQL_DEPLOY_REPO_ROOT` is set in no manifest under `deploy/`, so
   `service.go:111-117` falls back to `os.Getwd()` = `/app`;
3. `/app/deploy` is in no image and mounted by nothing — `.dockerignore:14`
   excludes `deploy/k8s` from the build context, and the runtime stage copies
   only `memql`, `healthcheck`, `portal`.

All four files are identical for local and cloud, **so a cloud identity pod
fails the same way.** This was never a k3d quirk.

memql#4257 added two facts the epic did not have: the image is **distroless**,
so there is no `kubectl` and no `git` to run even if a checkout existed; and
`deploy/k8s/base/identity.yaml` bound **no ServiceAccount with any permission
on `applications.argoproj.io`**, so a kubectl that existed would have been
refused by the API server.

## The decision

The epic offered three options. The chosen substrate is a **fourth**, and it
dominates all three for the effects actually required — which are: GET one
Application, PATCH it twice, LIST two collections.

| Option | Verdict |
|---|---|
| (a) give the identity pod a checkout (initContainer / git-sync + deploy key) | **Not for reads.** Adds a credential and a moving part to the most security-sensitive node in the mesh, and does not solve the missing binaries or the missing RBAC. Still the only answer for `git revert` — see below. |
| (b) derive reads from live cluster state | **Adopted, and generalised.** The epic recommended it for reads because it reports what IS running rather than what a file says should be. Writes turned out to need the same substrate, not a different one. |
| (c) take the console out of the cluster | **Rejected.** It would make the portal's cluster-operations surface read-only permanently, and the forwarding path (bff → identity over `NodeService.Stream` with `ForwardedAuthority`) already exists and works. |
| (d) **the Kubernetes REST API, in process** | **Implemented** (memql#4257). |

### Why (d) rather than client-go

`component/deploycontrol/k8sapi.go` is plain `net/http` against the API server
with the pod's projected ServiceAccount token — what `kubectl` does underneath,
minus `kubectl`. **Zero new dependencies.**

client-go is correct in shape and expensive in fact: no `k8s.io` dependency
exists anywhere in this workspace, and pulling client-go + apimachinery +
`k8s.io/api` into a module for two verbs is hundreds of packages and a
permanent upgrade obligation — for *typed* access to a CRD
(`argoproj.io/v1alpha1`) whose types are not in it anyway.

Three properties worth keeping:

- **Substrate selection is detected, not configured** — is a ServiceAccount
  token actually projected here. Not an `if env == "..."` branch: there is no
  environment to read, and `TestNoEnvironmentBranchingInEngineCode` forbids one
  with an empty exemption map. A k3d pod and an AKS pod take the same branch
  for the same reason; an operator machine takes the other.
- **The token is re-read per call.** Projected tokens rotate; a process caching
  the first read starts 401ing after its lifetime, which for a deploy console
  means "it worked on the day it was deployed".
- **Least privilege is expressible precisely** — get+patch on **one**
  Application pinned by `resourceNames`, get+list on Rollouts and AnalysisRuns.
  No create, delete, update, watch, wildcard, or ClusterRole.

### The RBAC placement, which is not where you would put it

The argocd-namespace grant is **not** in `deploy/k8s/base`. It was, and every
overlay rendered it into `memql`: kustomize's `namespace:` transformer rewrites
`metadata.namespace` on every namespaced resource it accumulates, **including
one that states `argocd` explicitly**. Measured on all three overlays.

A Role relocated that way applies cleanly, binds cleanly, and grants nothing.
The first symptom is a repair 403ing with every manifest in the repository
looking correct. It lives in `deploy/argocd/apps/deploy-console-rbac.yaml`,
rendered by the app-of-apps root with `directory:` (no transformer), and gated
by `deploy/k8s/overlays/render_deploy_rbac_test.go`.

## What still needs a checkout, and what it would take

Two Executor methods have no in-cluster form and now refuse **by name** rather
than as a generic kickoff failure:

- **`RunRollback` / `Git`** (`no_overlay_checkout`). Rollback is `git revert`
  of the overlay commit, and that is the point of it: reverting the one commit
  that changed the digests re-pins exactly the prior ones. There is no
  substitute that is not a second forward action.
- **`RunRolloutAction`** (`no_rollout_plugin`). `kubectl argo rollouts
  promote|abort` is an out-of-tree **plugin**. Its in-process form is not one
  API call — promote manipulates pause conditions whose representation is
  argo-rollouts-version-dependent, and a write-shaped guess against a live
  rollout is worse than an honest no.

Both were already impossible in a deployed cluster; what changed is that the
operator is told which prerequisite is absent.

**If a checkout is wanted, the shape is option (a) with the blast radius
narrowed:** a read-only git-sync sidecar with a deploy key scoped to one
repository, on the identity pod, mounting an `emptyDir` the main container
reads. The decisions it needs, none of which this record makes:

1. **Which repository.** The engine repo is public and needs no credential;
   an instance repo (the `memql-znas` shape) is private and does.
2. **Whether `git revert` may run in-cluster at all.** It writes a commit that
   must then be pushed, which turns a read-only sync into a write credential —
   a materially larger grant than everything else here combined. The
   alternative is that rollback becomes a *proposal* the console renders and a
   human merges, which is more honest about who owns `main`.
3. **`MEMQL_DEPLOY_REPO_ROOT` stays unset until one of those lands.** Setting
   it to a path no image contains would convert an honest refusal into a
   confusing one.

## Also relevant

`CutVersion` used to **swallow** the same read (`cutversion.go:216-226`
returned `""` on failure) and would have acted on an empty promoted version;
memql#4265 fixed that independently. The local overlay never sets
`MEMQL_DEPLOY_PROVIDER=docker-local`, so the deliberate refusal at
`driver.go:32` ("local clusters are operated via `make up`") was unreachable;
memql#4265 sets it.

Full evidence: `.claude/prds/portal-polish.md` item 2. Sibling epic: memql#4261.
