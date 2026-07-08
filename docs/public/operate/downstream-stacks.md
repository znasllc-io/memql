---
audience: public
status: current
area: operate
sinceVersion: 0.12.0
owner: platform
---

# Downstream product stacks (the carrier contract)

> **Superseded by platform consolidation (#2472).** The carrier contract
> below (a `<product>-carrier` Go repo that compile-time-links the product DSL
> and builds the mesh node images) is being replaced by the **runtime
> DSL-bundle** model: the engine ships every node as a product-agnostic image
> and loads product DSL at boot from `MEMQL_DSL_PATH`; a product is a DSL
> bundle + a client, with no carrier repo and no product node-images. New
> products stamp the DSL-first template (`memql-project`, default). This page
> documents the carrier hook that remains valid only while an existing product
> finishes migrating. See
> [../../internal/design/platform-consolidation.md](../../internal/design/platform-consolidation.md)
> and the topology in the engine `CLAUDE.md`.

The memQL engine repo is **product-agnostic**: it builds, runs, and tests
the engine mesh only, and it contains no product names. A product built on
memQL is structured as a small constellation of repos consolidated by a
workspace (`go.work`) checkout:

```
<workspace>/
├── go.work              # consolidates the Go modules below
├── memql/               # THIS repo -- the shared engine (product-agnostic)
├── <product>-carrier/   # product DSL + integrations; Go module depending
│                        # on the engine; its Dockerfile builds CARRIER
│                        # images (engine + product pack compiled together)
└── <product>-client/    # the product frontend (SPA or otherwise)
```

The engine's local-cluster tooling (`scripts/k3d/*.sh`, `make up` /
`make up-refresh` / `make dev` / `make scale` / `make status` / `make down`)
is the single implementation for every stack. Product repos do NOT copy
these scripts -- they invoke the engine's, passing the overrides below.
This is the contract a project-template repo stamps out for each new
product.

## What `make up` does here (engine-only)

In this repo, `make up` brings up a fully-running **engine** cluster: every
app node type (identity, voice, mcp, cognition, agent, planner, workbench)
builds from this repo's Dockerfile and is tagged `memql-<type>:local`. No
product code is involved; a node that would execute product DSL simply
serves the engine's own tree.

## The overrides (stable interface)

All are available as flags on the capability scripts and as pass-through
variables on the make targets. Environment-variable defaults exist for
`direnv`-style workspace setups.

| Make var / flag | Env default | Meaning |
|---|---|---|
| `CARRIER_REPO` / `--carrier-repo` | `MEMQL_CARRIER_REPO` | Path to the product carrier repo; its `Dockerfile` builds the carrier nodes |
| `CARRIER_NODES` / `--carrier-nodes` | `MEMQL_CARRIER_NODES` | Comma-separated node types built from the carrier Dockerfile (e.g. `bff,cognition,agent,planner,workbench`) |
| `CARRIER_CONTEXT` / `--carrier-context` | `MEMQL_CARRIER_CONTEXT` | Docker build context for carrier builds (default: the carrier repo's parent, i.e. the workspace root, so the Dockerfile can mount both source trees) |
| `APP_NAME` / `--app-name` | `MEMQL_K3D_APP_NAME` | ArgoCD Application name the bring-up registers (default `memql-local`) |
| `OVERLAY_PATH` / `--overlay-path` | `MEMQL_K3D_OVERLAY_PATH` | Kustomize overlay path within the Application's repo (default `deploy/k8s/overlays/local`) |
| `REPO_URL` / `--repo-url` | `MEMQL_K3D_REPO_URL` | Git repo URL the ArgoCD Application tracks (default this repo) |
| `EXTRA_PORTS` / `--extra-ports` | `MEMQL_K3D_EXTRA_PORTS` | Additional `host:container` k3d LoadBalancer mappings, comma-separated (e.g. `50051:50051,8080:8080` for a product gRPC head + frontend). k3d LB ports are fixed at cluster-create time, so pass this on the FIRST `make up` |
| `APP_PROJECT` / `--app-project` | `MEMQL_K3D_APP_PROJECT` | ArgoCD AppProject the Application belongs to (default `memql`). The engine AppProject allowlists only the engine repo in `sourceRepos`, so a downstream stack needs its own project |
| `PROJECT_MANIFEST` / `--project-manifest` | `MEMQL_K3D_PROJECT_MANIFEST` | Path to the AppProject manifest the bring-up applies (default: the engine's). A downstream stack passes its own, allowlisting its repo |
| (env only) | `MEMQL_K3D_REPO_TOKEN` + `MEMQL_K3D_REPO_USERNAME` | ArgoCD repository credential for a PRIVATE downstream repo. Env-only so tokens never appear in argv. Username defaults to `x-access-token` (GitHub token auth); token can be `$(gh auth token)` |

Carrier builds run `docker build --build-arg BUILD_TAGS=<type>` against
`${CARRIER_REPO}/Dockerfile` and tag the result `memql-<type>:local` -- the
same name the overlay pins -- so overriding a node is invisible to the
manifests. The build context defaults to the workspace root because a
carrier Dockerfile conventionally compiles against the sibling engine
checkout (`replace ../memql` in its `go.mod`).

## What a product repo's Makefile looks like

The engine bring-up registers ONE Application per invocation, so a product
`make up` is two engine calls: first the engine bring-up itself (cluster +
ArgoCD + secrets + the engine Application + carrier-built images), then a
second, idempotent `up.sh` invocation that registers the product's own
Application. The second call MUST pass `--revision` -- the default tracks
the ENGINE checkout's branch, which is meaningless for the product repo.

```makefile
ENGINE := ../memql

# Carrier build flags: which node images build from THIS repo's Dockerfile.
CARRIER_FLAGS := \
    CARRIER_REPO=$(CURDIR) \
    CARRIER_NODES=bff,cognition,agent,planner,workbench

up:
	# 1. Engine bring-up: cluster + ArgoCD + secrets + engine Application,
	#    with this stack's LB ports and carrier-built node images.
	$(MAKE) -C $(ENGINE) up $(CARRIER_FLAGS) EXTRA_PORTS=50051:50051
	# 2. Register the product Application (idempotent on the running cluster).
	MEMQL_K3D_REPO_TOKEN=$$(gh auth token) bash $(ENGINE)/scripts/k3d/up.sh \
	    --app-name=<product>-local \
	    --app-project=<product> \
	    --project-manifest=$(CURDIR)/deploy/argocd/project.yaml \
	    --repo-url=https://github.com/<org>/<product>-carrier.git \
	    --revision=$$(git rev-parse --abbrev-ref HEAD) \
	    --overlay-path=deploy/k8s/overlays/local \
	    --no-secrets

dev:
	$(MAKE) -C $(ENGINE) dev $(CARRIER_FLAGS) $(if $(NODE),NODE=$(NODE))
```

A private downstream repo needs the ArgoCD repository credential
(`MEMQL_K3D_REPO_TOKEN`); the engine repo itself is public and syncs
anonymously. A frontend/client repo builds its own image and imports it
with the generic seam:

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
   ONLY the product-owned resources (its bff head Deployment/Service, its
   frontend Deployment). Registered by a second, idempotent `up.sh` call
   with the product's `--app-name/--app-project/--project-manifest/
   --repo-url/--overlay-path`.

Why not a kustomize remote base? ArgoCD's repo-server resolves remote
bases outside the Application's credential -- a private product repo (the
normal case) cannot be fetched that way, and a private base would also
couple manifest revisions across repos. Independent Applications keep each
repo's resources owned, pruned, and versioned by that repo.

The engine bring-up is idempotent, so the product repo's `make up` simply
runs it twice: once as the engine bring-up (with the stack's
`EXTRA_PORTS` and carrier build flags), then once more to register the
product Application (`--no-secrets`, since secrets were already seeded).
The client repo's `make up` delegates to the carrier repo's and layers its
frontend the same way.

## Design rules for new seams

1. The engine never names a product. Product identifiers (concept ids,
   skill slugs, knowledge domains, image names, hostnames) live in the
   product repos and reach the engine through registration seams
   (`memql.RegisterPlugin`, `node.RegisterRoutingRule`,
   `node.RegisterConceptOwnership`, suggest-domain registration) or
   through the override flags above.
2. Every new engine ↔ product seam must pass the template test: *could a
   second product plug in without editing the engine repo?*
3. Overrides are flags/vars with env defaults -- never hardcoded paths to
   sibling checkouts.
