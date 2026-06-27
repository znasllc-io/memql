# Docker assets

The local dev cluster runs on **k3d + ArgoCD** (Argo parity, memql#2061),
not Docker Compose. The Docker Compose stack was retired in memql#2068 /
#2088 -- the same `deploy/k8s` overlays + ArgoCD reconciliation path now run
locally and in staging. See the k3d runbook:
[docs/public/operate/reproduce-staging-locally.md](../docs/public/operate/reproduce-staging-locally.md)
and the quick start in [CLAUDE.md](../CLAUDE.md) (`make up` / `make dev` /
`make status` / `make down`).

## Files in this directory

| File | Purpose |
|------|---------|
| `memql.Dockerfile` | The engine image (`--target <type>-runtime`): standalone + the engine node-type variants (identity / voice / mcp). Used by `make dev` (`scripts/k3d/dev.sh`, local k3d import) and `scripts/release/release.sh` (`make release`). The carrier images (bff / cognition / agent / planner / workbench) build from `memql-bff-copresent/Dockerfile`. |
| `init-db.sql` | The PostgreSQL extension bootstrap the migrations expect (TimescaleDB, etc.). Referenced by `.github/workflows/ci.yml` and applied by the local k3d postgres on first boot. |

Local image builds (engine + carrier) are imported into k3d by
`make dev` (`scripts/k3d/dev.sh`). Deployable images are built on the
GitHub build server (OIDC -> ACR), never hand-pushed -- see
[docs/public/operate/deployment-strategy.md](../docs/public/operate/deployment-strategy.md).
