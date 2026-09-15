---
title: Deploy-bundle runbook -- the retired imperative deploy path
audience: public
status: historical
area: operate
sinceVersion: 0.12.0
owner: znas
---

# Deploy-Bundle Runbook -- the retired imperative deploy path

> Historical: retired in memql#4550; kept for rationale.

**The path this runbook describes no longer exists.** It ran a deployment of
the engine mesh through the DSL deploy bundle (`deployEngineCluster`,
dsl/deployment/) from the Cockpit, and the Cockpit's `deploy` subcommand was
deleted along with its TUI and embedded runtime. The `deploy` make target and
`scripts/deploy/cockpit.sh` went with it.

**What to do instead.** The deploy is a git merge, and always was: build the
engine images on the build server, pin `{engine version, bundle digest, client
digest}` in ONE overlay under `deploy/k8s/overlays/<instance>`, and merge --
ArgoCD reconciles. Rollback is `git revert` on that overlay. Locally, `make up`
brings up k3d + ArgoCD against `deploy/k8s/overlays/local`.

**Why removing it cost nothing.** This path could never complete a real
deploy: the Cockpit's in-process engine carried no database, so every DB-backed
step reported `BLOCKED (owner-gated)`. It was proven live once, on 2026-07-04
(memql#2380), and owner-gated back to dry-run afterwards.

The rest of this document is kept because the bundle it drove --
`deployEngineCluster` and its phases -- still describes how a deploy is
modelled in the graph, and the `v1:cluster:deployment` timeline is still the
record of evidence. Read it for that; do not read it for commands to run. The
DSL lifecycle automations (memql#4490) are what replace the design.

## Upgrading to an engine that reads the language line

This section is current, unlike the rest of this document. It applies to any
product DSL a node mounts at `MEMQL_DSL_PATH`, whether a bundle image or a
package delivers it.

Every domain directory of a product's DSL declares the language it is written
in, in its own `<domain>/memql.toml`
([the language line](../language/memql.md#the-language-line)): two lines,
`memql = "1.0"` and `edition = "2026"`. An engine that reads the language line
refuses to boot on a domain without one, and strict boot refuses the node
rather than skipping the domain. An engine from before the language line
ignores the file.

`memqlmigrate` writes the file into every domain that has none. Point it at the
directory that holds the domains:

```bash
memqlmigrate --rewrite=language-line -w <root>
```

Or write the two lines by hand in each `<domain>/memql.toml`.

Do it in this order:

1. Add `memql.toml` to every domain, in the repository.
2. Ship the copy the nodes read. A node reads the DSL it was delivered, never
   the repository: the tree a bundle image copies into `MEMQL_DSL_PATH`, or a
   package's staged copy. For a bundle, build the image and pin its new digest
   in the overlay. The image carries the file when it copies each domain
   directory whole, as the fleet bundle's `COPY dsl/ /bundle/` does, and the
   init container copies the image's tree whole. For a package, redeploy it
   ([packages.md](packages.md#upgrading-to-an-engine-that-reads-the-language-line)).
   The engine still running ignores the file.
3. Roll the engine.

If the engine rolled first and the nodes crash-loop on
`language_line_missing`, set the break-glass, `MEMQL_DSL_ALLOW_SKIPS=1`. It is
a bootstrap variable, a key on the `memql-secrets` Secret
([env-vars.md](env-vars.md)), so the pods see it when they restart; the nodes
then boot and skip each refused domain whole. Ship the copy that carries the
file, then remove the break-glass and restart the nodes: while it is set, a
construct that fails to load is logged and dropped instead of refusing boot.

## What the bundle did

Every phase -- authorize, record, clone, build, place, gate, outcome, finalize,
rollback -- ran as the automation's statements, each one a journaled step, and
the `v1:cluster:deployment` timeline
in the target database was the record of evidence.

Scope: engine mesh ONLY (identity / agent / planner / workbench / mcp /
edge). A downstream product stack (carrier bff + SPA) deploys from its own
repo's release track -- see
[downstream-stacks.md](downstream-stacks.md).

## Prerequisites

1. **A cockpit binary** with a `deploy` subcommand -- no such build exists any
   more (memql#4550).
2. **The target database DSN** in `MEMQL_DATABASE_DSN` -- the deploy
   runtime boots DB-backed and persists the deployment timeline there.
   For development this is the k3d postgres
   (`kubectl port-forward -n memql svc/memql-db-rw 5432:5432`, then
   `postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable`).
3. **A workdir that is a git clone** of the memql repo. The clone step
   fetches + checks out the requested ref IN this tree; it does not
   create it. For development a local-path clone avoids any credential
   concerns: `git clone /path/to/memql /tmp/deploy-workdir`.
4. **`MEMQL_DEPLOY_REPO_ROOT`** pointing at a memql checkout (the
   capability-script runner resolves the allowlisted scripts/* backends
   from it when the cockpit runs outside the repo).
5. For the **`azure` provider** (the cloud target): an `az` session that can
   read the ACR (`acrmemql` by default; override with `MEMQL_DEPLOY_ACR`). The
   emitter resolves `payload.digests` from ACR by tag (memql#2381).

## The deploy target: `provider`, not `env`

MemQL ships one installation shape (epic memql#3943); there is no
staging-versus-production dimension for a command to select. What still
varies is the deploy TARGET, which `deployEngineCluster`
(`dsl/deployment/automations.memql`) switches on via its `provider` field --
`docker-local` (a k3d cluster, images imported locally) or `azure` (AKS,
digest-pinned overlay + ArgoCD sync). `MEMQL_DEPLOY_PROVIDER` sets the
default provider server-side when a caller does not supply one explicitly.
Pass it through the event payload (the `--input` JSON below) rather than an
`--env` flag -- the automation's `args {}` block has no `env` field any
more.

## The dry-run ladder (memql#2378)

| Invocation        | What happens |
|-------------------|--------------|
| `--dry-run`       | Resolve + preview only. Nothing executes. |
| *(default)*       | The automation EXECUTES; every capability script runs in its own dry-run mode and reports its intended work. Timeline rows ARE written. |
| `--apply`         | Scripts perform their work. The real deploy. |

An explicit `--input '{"dryRun": ...}'` wins over the flags.

## Development deploy (the k3d cluster)

The development flow builds images locally, imports them into k3d, and
gates on the mesh litmus (deployments healthy + unique per-pod
MEMQL_NODE_ID -- the `make status` check, selected by env inside
`scripts/deploy/deploy-gate.sh`).

IMPORTANT: pass `version=local` -- the local overlay pins
`memql-<type>:local` (`deploy/k8s/overlays/local/kustomization.yaml`),
so images built under any other tag import fine but never get pulled.
The gate catching exactly this mismatch is what it is for.

> **REMOVED (memql#4550).** This invocation is kept as a record of what the
> imperative path was; **the command no longer exists.** `memql-cockpit
> deploy` was deleted with the Cockpit's TUI and embedded runtime, and
> the `deploy` make target and `scripts/deploy/cockpit.sh` went with it. Nothing
> replaces it, because nothing needed to: this path could never complete a
> real deploy -- the cockpit's in-process engine carried no database, so
> every DB-backed step reported `BLOCKED (owner-gated)`.
>
> The local deploy is `make up` (k3d + ArgoCD applying
> `deploy/k8s/overlays/local`); the cloud deploy is a digest bump in one
> overlay plus a merge.

```bash
# NO LONGER RUNNABLE -- recorded for reference only.
MEMQL_DEPLOY_REPO_ROOT=/path/to/memql \
MEMQL_DATABASE_DSN="postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable" \
memql-cockpit deploy --ref main --role owner --actor "$USER" \
  --apply --input '{"workdir":"/tmp/deploy-workdir","version":"local","provider":"docker-local"}'
```

## Azure (cloud) deploy target

> **The cockpit path here is REMOVED too (memql#4550)**, and its removal
> costs nothing: the cloud deploy was always the GitOps one. Build the engine
> images on the build server, pin `{engine version, bundle digest, client
> digest}` in ONE overlay, merge -- ArgoCD reconciles. Rollback is `git
> revert` on the overlay.
>
> An entry instance is `cloud-entry`, not `cloud`: its overlay is
> `deploy/k8s/overlays/cloud-entry`, and both bring-up and later rolls go
> through Argo on that overlay -- see
> [azure-entry-install.md](azure-entry-install.md).
>
> What the deleted path did that the GitOps one still needs somewhere is
> resolving real image digests from ACR (memql#2381). That is a
> pin-the-overlay step, not a deploy runner.

## Reading the evidence

```sql
SELECT payload->>'deploymentId', payload->>'status', "createdAt"
FROM "MemoryNodes" WHERE concept = 'v1:cluster:deployment'
ORDER BY "createdAt" DESC;
```

One timeline per deploymentId: `in_progress` -> `succeeded` | `failed`.
The cockpit also emits an AUDIT line per invocation.

## Known gaps

- A mid-flight ACTION failure aborts the automation before finalize, so
  the timeline strands at `in_progress` (no terminal write). Re-running
  appends a fresh deploymentId.
- The `deploy` make target mangled JSON quoting on `ARGS='--input {...}'`; the
  workaround was to invoke the cockpit binary directly. Both are gone.
