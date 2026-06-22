# Docker dev stack (parity cluster)

The 2-replica staging-parity cluster (`docker-compose.cluster.yml`) is
the ONLY supported local run path (memql#1304). The single-node
`docker-compose.full.yml` + `docker-compose.nemoclaw.yml` stacks were
retired in memql#1311; the cluster reproduces the cross-node /
replica-fan-out class of bugs a single-node stack structurally cannot.

Front door (memql#1313): the cluster serves at the TLS `*.local.znas.io`
subdomains via nginx (parity with staging's per-subdomain ingress).
`*.local.znas.io` resolves to 127.0.0.1 via real DNS (no `/etc/hosts`).

---

## Quick start

```bash
# Generate the wildcard mkcert dev cert, decrypt genesis, wipe the DB,
# rebuild, restart, wait healthy, reseed -- one command:
make dev-cluster-refresh

# Or boot the cluster in the background without the genesis wipe/seed:
make dev-cluster-up

# Foreground (build + up):
make dev-cluster

# Stop (keeps volumes) / view logs / parity litmus:
make dev-cluster-down
make dev-cluster-logs
make dev-cluster-status
```

Export `MEMQL_PACKAGES_TOKEN` (read:packages for the @visionarys-io +
@znasllc-io scopes) before building -- the copresent SPA image needs it.

---

## What's included

| Service | Replicas | Internal port(s) | Purpose |
|---------|----------|------------------|---------|
| **postgres** (`memql-db`) | 1 | 5432 | PostgreSQL + TimescaleDB |
| **nginx** (`memql-lb`) | 1 | 80, 443 | TLS subdomain front door |
| **bff** | 2 | 8088 (HTTP), 50051 (gRPC) | Backend-for-frontend / API surface |
| **cognition** | 2 | 8085 | Conversation intelligence |
| **voice** | 2 | 8085 | ASR/TTS pipeline (`/memql/audio`) |
| **agent** | 2 | 8085 (HTTP), 50051 (gRPC) | Task execution / AI work + WorkerService |
| **planner** | 2 | 8085 | Task planning + orchestration |
| **workbench** | 2 | 8085 | Per-Plan sandboxed exec environment |
| **identity** | 2 | 8081 | Magic-link auth, JWKS, admin UI |
| **copresent** | 2 | 8080 | CoPresent SPA |
| **livekit** | 1 | 7880-7882 | Self-hosted WebRTC SFU (voice/video) |
| **azurite** | 1 | 10000 | Azure Blob emulator |
| **pgadmin** (`memql-dbadmin`) | 1 | 5050 | DB management (optional, `--profile tools`) |

The replicated mesh nodes set no `container_name`/`hostname` so Compose
assigns each replica a unique `<project>-<service>-N` id (the compose
equivalent of staging's `fieldRef: metadata.name`). The compose project
is named `memql-cluster-multinode`.

---

## Front-door URLs (TLS, :443)

| Subdomain | Backend |
|-----------|---------|
| `https://app.local.znas.io` | copresent SPA |
| `https://identity.local.znas.io` | identity (auth, /admin, /setup, JWKS) |
| `https://bff.local.znas.io` | BFF gRPC + HTTP (`/memql/ws`, attachments, healthz); `/memql/audio` -> voice |
| `https://agent.local.znas.io` | agent gRPC (WorkerService.Stream) + HTTP |
| `https://livekit.local.znas.io` | LiveKit signaling |

Routing map: `docker/nginx/templates/default.conf.template`. The wildcard
mkcert cert lives in `docker/nginx/certs/dev.{crt,key}` (gitignored;
`make setup-tls` or `make dev-cluster-refresh` generates it).

---

## Verify

```bash
# Identity JWKS (TLS via nginx)
curl -v https://identity.local.znas.io/.well-known/jwks.json

# Front-door health
curl -v https://bff.local.znas.io/healthz

# Database (direct, not via nginx)
psql postgres://memql:memql_dev@localhost:5432/memql -c "SELECT version();"

# Per-replica node ids (parity litmus)
make dev-cluster-status
```

---

## Files in this directory

| File | Purpose |
|------|---------|
| `docker-compose.cluster.yml` | The 2-replica staging-parity cluster (the supported dev stack) |
| `docker-compose.cluster.ci.yml` | CI / co-tenant override: drops colliding host publishes + swaps the TLS front door for a plain-HTTP single-origin one (`nginx.ci.conf`) so the unattended cross-replica delivery gate runs without mkcert |
| `docker-compose.polyphon.yml` | Voice/avatar overlay (LiveKit + voice-agent) -- pending re-home onto the cluster (memql#1310) |
| `nginx/templates/default.conf.template` | The TLS subdomain front-door config (envsubst on `${DOMAIN}`) |
| `nginx/nginx.ci.conf` | Plain-HTTP single-origin front door used ONLY by the CI override |
| `nginx/certs/` | mkcert dev cert (`dev.{crt,key}`, gitignored) |
| `memql.Dockerfile` | The engine image (voice + identity nodes) |

See the full runbook + divergence audit:
[docs/public/operate/reproduce-staging-locally.md](../docs/public/operate/reproduce-staging-locally.md).
