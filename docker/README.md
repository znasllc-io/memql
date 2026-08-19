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
| `memql.Dockerfile` | The engine image. Most node types (bff / cognition / agent / planner / workbench / mcp / edge) are selected via `--build-arg BUILD_TAGS=<type>` against the single default runtime stage; `--target voice-runtime` is the one CGO-specific exception (CGO_ENABLED=1, for the voice-agent's RobotGo-adjacent deps). Used by `make dev` (`scripts/k3d/dev.sh`, local k3d import) and `.github/workflows/build-engine-images.yml` for cloud deploys. Carrier images (product-DSL-bearing node types) build from the product pack repo's Dockerfile via the carrier hook (docs/public/operate/downstream-stacks.md). |
| `init-db.sql` | The PostgreSQL extension bootstrap the migrations expect (TimescaleDB, etc.). Referenced by `.github/workflows/ci.yml` and applied by the local k3d postgres on first boot. |

Local image builds (engine + carrier) are imported into k3d by
`make dev` (`scripts/k3d/dev.sh`). Deployable images are built on the
GitHub build server (OIDC -> ACR), never hand-pushed -- see
docs/public/operate/deployment-strategy.md (see the product pack repo's docs/operate/deployment-strategy.md).
