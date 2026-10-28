# Docker assets

The local dev cluster runs on **k3d + ArgoCD** (Argo parity, memql#2061),
not Docker Compose. The Docker Compose stack was retired in memql#2068 /
#2088 -- the same `deploy/k8s` overlays + ArgoCD reconciliation path now run
locally and in the cloud. See the k3d runbook:
[docs/public/operate/reproduce-the-cloud-locally.md](../docs/public/operate/reproduce-the-cloud-locally.md)
and the quick start in [CLAUDE.md](../CLAUDE.md) (`make up` / `make dev` /
`make status` / `make down`).

## Files in this directory

| File | Purpose |
|------|---------|
| `init-db.sql` | The PostgreSQL extension bootstrap the migrations expect (TimescaleDB, etc.). Referenced by `.github/workflows/ci.yml` and applied by the local k3d postgres on first boot. |

## The engine image is not here

Every node type (identity / bff / agent / planner / workbench / mcp / edge)
builds from the repository's root [`Dockerfile`](../Dockerfile), selected by
`--build-arg BUILD_TAGS=<type>` and a target stage
(`scripts/lib/engine_build_args.sh` maps one to the other):

- `make dev` (`scripts/k3d/dev.sh`) builds node images and imports them into
  k3d.
- `make release` (`scripts/release/release.sh`) builds the bff image locally.
- The native release build in [`memql-package.yaml`](../memql-package.yaml)
  builds every node type for deploys. Deployable images are never hand-pushed;
  see [release candidates](../docs/public/operate/release-candidates.md).

This directory used to hold `memql.Dockerfile`, a second engine Dockerfile
that only `make release` built. It drifted from the root one until it could
not build at all, and was retired. Carrier images (product-DSL-bearing node
types) build from the product pack repo's Dockerfile via the carrier hook
([downstream-stacks.md](../docs/public/operate/downstream-stacks.md)).
