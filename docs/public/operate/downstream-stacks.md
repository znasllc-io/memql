---
audience: public
status: current
area: operate
sinceVersion: 0.12.0
owner: platform
---

# Downstream product stacks (the DSL-bundle contract)

The memQL engine repo is **product-agnostic**: it builds, runs, and tests
the engine mesh only, and it contains no product names. A product built on
memQL is **not** a Go constellation -- it is a **DSL bundle plus a client**,
deployed as one overlay over the product-agnostic engine:

```
<product>/                    # ONE repo -- no product Go, no go.work
├── dsl/                      # the product's .memql tree (data only),
│                             #   packaged as a tiny data-only BUNDLE image
├── client/                   # the product frontend (SPA or otherwise)
└── deploy/k8s/overlays/…     # pins {engine version, bundle digest,
                              #   client digest} + adds the dsl-bundle component
```

The engine ships **every** node type (identity, bff, cognition, agent,
planner, voice, workbench, mcp) as a **product-agnostic image** from this
repo's Dockerfile; nothing product-specific is compiled in. Product DSL is
delivered at **runtime**: the data-only bundle image is run as an
init-container -- the `deploy/k8s/components/dsl-bundle` kustomize component --
that copies the `.memql` tree into a shared volume; each node reads it at
`MEMQL_DSL_PATH`, and `dsl.MountRuntimeDomainsFromEnv` registers every
product domain via `RegisterTree(os.DirFS(…))` before the first tree walk.
So a plain engine image runs any product's DSL with zero product code. A
"bff" is just an engine `bff` node fronting a product's bundle -- a deploy
concern, not a repo. Genuinely-bespoke product Go (rare) becomes a thin
optional `bff/` plugin module in the product repo.

A **release** is `{engine version, bundle digest, client digest}` pinned in
that one overlay in that one repo -- no cross-repo assembly, no per-node
product images, no release lockfiles. Full rationale + the migration
sequence:
[../../internal/design/platform-consolidation.md](../../internal/design/platform-consolidation.md).

The engine's local-cluster tooling (`scripts/k3d/*.sh`, `make up` /
`make up-refresh` / `make dev` / `make scale` / `make status` / `make down`)
is the single implementation for every stack. Product repos do NOT copy
these scripts -- they invoke the engine's, passing the overrides below.
This is the contract a project-template repo stamps out for each new
product.

## What `make up` does here (engine-only)

In this repo, `make up` brings up a fully-running **engine** cluster: every
app node type (identity, voice, mcp, cognition, agent, planner, workbench)
builds from this repo's Dockerfile as a product-agnostic image tagged
`memql-<type>:local`. No product bundle is mounted, so each node runs only
the engine's own embedded DSL tree. Point the same image at a product's
bundle -- via the `dsl-bundle` component and `MEMQL_DSL_PATH` -- and it runs
that product's DSL at boot with no rebuild.

## The overrides (stable interface)

All are available as flags on the capability scripts and as pass-through
variables on the make targets. Environment-variable defaults exist for
`direnv`-style workspace setups.

| Make var / flag | Env default | Meaning |
|---|---|---|
| `APP_NAME` / `--app-name` | `MEMQL_K3D_APP_NAME` | ArgoCD Application name the bring-up registers (default `memql-local`) |
| `OVERLAY_PATH` / `--overlay-path` | `MEMQL_K3D_OVERLAY_PATH` | Kustomize overlay path within the Application's repo (default `deploy/k8s/overlays/local`) |
| `REPO_URL` / `--repo-url` | `MEMQL_K3D_REPO_URL` | Git repo URL the ArgoCD Application tracks (default this repo) |
| `EXTRA_PORTS` / `--extra-ports` | `MEMQL_K3D_EXTRA_PORTS` | Additional `host:container` k3d LoadBalancer mappings, comma-separated (e.g. `50051:50051,8080:8080` for a product gRPC head + frontend). k3d LB ports are fixed at cluster-create time, so pass this on the FIRST `make up` |
| `APP_PROJECT` / `--app-project` | `MEMQL_K3D_APP_PROJECT` | ArgoCD AppProject the Application belongs to (default `memql`). The engine AppProject allowlists only the engine repo in `sourceRepos`, so a downstream stack needs its own project |
| `PROJECT_MANIFEST` / `--project-manifest` | `MEMQL_K3D_PROJECT_MANIFEST` | Path to the AppProject manifest the bring-up applies (default: the engine's). A downstream stack passes its own, allowlisting its repo |
| (env only) | `MEMQL_K3D_REPO_TOKEN` + `MEMQL_K3D_REPO_USERNAME` | ArgoCD repository credential for a PRIVATE downstream repo. Env-only so tokens never appear in argv. Username defaults to `x-access-token` (GitHub token auth); token can be `$(gh auth token)` |

A product does not build node images. Its DSL reaches the cluster as a
**data-only bundle image**: build it from the product's `.memql` tree
(`FROM scratch` + the files), import it into k3d like any other local image,
and let the product overlay's `deploy/k8s/components/dsl-bundle` component
mount it onto the mesh nodes (those labelled `memql/product-dsl: "true"`) at
`MEMQL_DSL_PATH`. The overlay pins the `{engine, bundle, client}` digests;
the engine node images are the plain `memql-<type>:local` images this repo
already builds, so swapping a product in is invisible to the manifests.

## What a product repo's Makefile looks like

The engine bring-up registers ONE Application per invocation, so a product
`make up` is two engine calls: first the engine bring-up itself (cluster +
ArgoCD + secrets + the engine Application), then a second, idempotent
`up.sh` invocation that registers the product's own Application (its bff
head + the dsl-bundle component + the client). The second call MUST pass
`--revision` -- the default tracks the ENGINE checkout's branch, which is
meaningless for the product repo.

```makefile
ENGINE := ../memql

up:
	# 1. Engine bring-up: cluster + ArgoCD + secrets + engine Application,
	#    with this stack's LB ports.
	$(MAKE) -C $(ENGINE) up EXTRA_PORTS=50051:50051
	# 2. Package + import the product's data-only DSL bundle image (no product
	#    node images -- the engine images are product-agnostic).
	docker build -f dsl.Dockerfile -t <product>-dsl-bundle:local .
	bash $(ENGINE)/scripts/k3d/import-image.sh --image=<product>-dsl-bundle:local --dryRun=false
	# 3. Register the product Application (idempotent on the running cluster).
	#    Its overlay adds the dsl-bundle component (mounting the bundle at
	#    MEMQL_DSL_PATH) and pins the {engine, bundle, client} digests.
	MEMQL_K3D_REPO_TOKEN=$$(gh auth token) bash $(ENGINE)/scripts/k3d/up.sh \
	    --app-name=<product>-local \
	    --app-project=<product> \
	    --project-manifest=$(CURDIR)/deploy/argocd/project.yaml \
	    --repo-url=https://github.com/<org>/<product>.git \
	    --revision=$$(git rev-parse --abbrev-ref HEAD) \
	    --overlay-path=deploy/k8s/overlays/local \
	    --no-secrets

dev:
	# Re-package the bundle and roll the mesh so each init-container
	# re-copies the updated .memql tree.
	docker build -f dsl.Dockerfile -t <product>-dsl-bundle:local .
	bash $(ENGINE)/scripts/k3d/import-image.sh --image=<product>-dsl-bundle:local --dryRun=false
	kubectl rollout restart -n memql deploy -l memql/product-dsl=true
```

A private downstream repo needs the ArgoCD repository credential
(`MEMQL_K3D_REPO_TOKEN`); the engine repo itself is public and syncs
anonymously. A frontend/client repo builds its own image and imports it
with the same generic seam:

```bash
docker build -t <product>-frontend:local .
bash ../memql/scripts/k3d/import-image.sh --image=<product>-frontend:local --dryRun=false
```

## Composition model: one Application per repo

A product stack is composed of one ArgoCD Application per repo, all in the
same namespace -- NOT a single overlay that cross-references repos:

1. **Engine app** (`memql-local`): the engine's own overlay, from this
   (public) repo. Registered by the engine `make up` the product target
   delegates to.
2. **Product app** (`<product>-local`): the product repo's overlay carrying
   ONLY the product-owned resources -- its bff head Deployment/Service (a
   plain engine `bff` image), the `dsl-bundle` component that mounts the
   product bundle at `MEMQL_DSL_PATH`, and its client Deployment. Registered
   by a second, idempotent `up.sh` call with the product's
   `--app-name/--app-project/--project-manifest/--repo-url/--overlay-path`.

Why not a kustomize remote base? ArgoCD's repo-server resolves remote
bases outside the Application's credential -- a private product repo (the
normal case) cannot be fetched that way, and a private base would also
couple manifest revisions across repos. Independent Applications keep each
repo's resources owned, pruned, and versioned by that repo.

The engine bring-up is idempotent, so the product repo's `make up` simply
runs it twice: once as the engine bring-up (with the stack's `EXTRA_PORTS`),
then once more to register the product Application (`--no-secrets`, since
secrets were already seeded) after packaging and importing the product's
data-only DSL bundle image and client image.

## Design rules for new seams

1. The engine never names a product. Product identifiers (concept ids,
   skill slugs, knowledge domains) live in the product's DSL and reach the
   engine at **runtime** through the DSL bundle (`MEMQL_DSL_PATH`), never
   compiled in. The rare bespoke-Go plugin still registers through the
   narrow seams (`memql.RegisterPlugin`, `node.RegisterRoutingRule`,
   suggest-domain registration); image
   names and hostnames come through the override flags above.
2. Every new engine ↔ product seam must pass the template test: *could a
   second product plug in without editing the engine repo?*
3. Overrides are flags/vars with env defaults -- never hardcoded paths to
   sibling checkouts.
