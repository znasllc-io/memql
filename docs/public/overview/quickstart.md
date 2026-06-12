---
title: memQL Quick Start
audience: public
status: stable
area: overview
sinceVersion: 0.9.0
owner: znas
---

# memQL Quick Start
## Get Running in 5 Minutes

---

## Prerequisites

- **Docker** installed and running
- **Go 1.26.1+** (for development outside Docker)
- **GitHub Packages access** for the in-container CoPresent frontend.
  The `app` container `npm install`s two private SDK packages
  (`@visionarys-io/copresent-sdk`, `@znasllc-io/memql-sdk-core`) from
  GitHub Packages, so it needs a token at startup:

  ```bash
  # a GitHub token with read:packages, SSO-authorized for BOTH the
  # znasllc-io and visionarys-io orgs. A classic PAT works, or reuse
  # your gh login if it has the scope:
  export MEMQL_PACKAGES_TOKEN=$(gh auth token)
  ```

  Export it before `make dev-cluster-up` / `make dev-cluster-refresh`. Without it
  the `app` container's install fails (the backend still comes up). Full
  details + how to mint the token: `docs/sdk-dependency.md` in the
  CoPresent repo.
  No Node install on the host is needed -- the frontend runs in-container.

---

## Quick Start

### 1. Bootstrap (one-time per machine)

```bash
make bootstrap          # writes .env.local with master key + DB DSN
make secrets-init       # interactively populate ~/.memql/dev-secrets.yaml from manifest
make setup-tls          # generate locally-trusted TLS certs (mkcert)
```

`make setup-tls` requires
[mkcert](https://github.com/FiloSottile/mkcert) -- on macOS:
`brew install mkcert nss`. It installs a local root CA into your
system trust store and produces `docker/nginx/certs/dev.{crt,key}`
with one wildcard SAN: `*.${IDENTITY_BOOTSTRAP_DOMAIN}` (default
`*.local.znas.io`). The cluster's nginx terminates TLS for every
service's subdomain; browsers, Go clients, and the cockpit-worker
all verify against the system trust store so no warnings appear.

### Prereq -- DNS records

The dev cluster reaches the nginx LB through a single wildcard
hostname pattern: `*.${IDENTITY_BOOTSTRAP_DOMAIN}` (default
`*.local.znas.io`) must resolve to `127.0.0.1`. One DNS record
covers every service slot (`app.<domain>`, `identity.<domain>`,
`bff.<domain>`, `agent.<domain>`):

| Domain        | Type | Name      | Value     |
|---------------|------|-----------|-----------|
| znas.io       | A    | `*.local` | 127.0.0.1 |

Verify with `dig +short bff.local.znas.io` -- expect `127.0.0.1`.

To run against a different domain (e.g. for an enterprise install
with a different parent domain), export
`IDENTITY_BOOTSTRAP_DOMAIN=<your-domain>` before
`make setup-tls` and `docker compose up`. The mkcert script and
nginx template both pick it up; certs are issued for `*.<your-
domain>`. You also need the matching DNS record for that domain.

### 2. Start the stack

```bash
make dev-cluster-up       # 2 replicas/mesh node; front door https://app.local.znas.io
make dev-cluster-status   # parity litmus: distinct per-replica node ids
```

This brings up the **2-replica staging-parity cluster** -- the blessed
and only supported local topology (memql#1304): Postgres + TimescaleDB,
the bff/cognition/agent/planner/voice/workbench mesh nodes (2 replicas
each), identity, the copresent SPA, and LiveKit, all behind the nginx
TLS front door on the `*.local.znas.io` subdomains. It is the only local
topology that reproduces cluster-only bugs (cross-node delivery, replica
fan-out, node lifecycle).

> Runbook + divergence audit:
> [docs/public/operate/reproduce-staging-locally.md](../operate/reproduce-staging-locally.md).

### 3. Seed concept-stored config

```bash
make secrets-seed       # encrypt + push the yaml into the running memQL
```

After this the system has the OpenAI / Anthropic keys, identity
service signing-key encryption secret, etc. live in
`v1:platform:globalSecret`. See
[docs/public/operate/env-vars.md](../operate/env-vars.md) for the full
bootstrap-envelope-vs-concept-storage breakdown.

### 4. Watch logs

```bash
make dev-cluster-logs
```

---

## Verify

```bash
# Identity service health (TLS via nginx)
curl -v https://identity.local.znas.io/.well-known/jwks.json

# Database (direct, not via nginx)
psql postgres://memql:memql_dev@localhost:5432/memql -c "SELECT version();"
```

---

## What's running?

| Service | Public URL | Notes |
|---------|------------|-------|
| **App SPA** | https://app.local.znas.io | Proxied by nginx to the `copresent` container (built bundle; needs `MEMQL_PACKAGES_TOKEN`) |
| **Identity service** | https://identity.local.znas.io | Magic-link auth, OAuth, JWKS, /admin, /pair/* |
| **BFF (gRPC + WS)** | https://bff.local.znas.io | gRPC for cockpit / SDKs; HTTP/WS for browser bridge |
| **Agent (gRPC)** | https://agent.local.znas.io | WorkerService.Stream lives here; cockpit-workers attach |
| **PostgreSQL** | localhost:5432 | Internal only; nginx doesn't proxy DB |

The browser only ever sees ports `:443` (HTTPS) and `:80` (redirect).
Internal ports are not exposed to the host.

**Database credentials:** `memql / memql_dev` on database `memql`.

---

## Run tests

```bash
go test ./...
```

---

## Stop / reset

```bash
# Stop (preserves data)
make dev-cluster-down

# Full reset (drops volumes) + fresh rebuild
make dev-cluster-restart-purge
```

The repo includes a one-shot `make dev-cluster-refresh` that exports
concept state to yaml, wipes the DB, restarts, and re-seeds on the
staging-parity cluster -- use it when you're iterating on schema
changes. (memql#1304: the single-node `make dev-refresh` was removed;
the parity cluster is the only supported local run path.)

---

## First-run setup -- two paths

The cluster has one identity owner. They get cluster-wide admin
rights and the operator-side keys to /admin. There are two ways
to claim that role on a fresh deployment:

### A) Interactive (default) -- visit `/setup`

Bring the stack up with no `IDENTITY_BOOTSTRAP_OWNER_*` env vars
set. The identity service notices it isn't bootstrapped yet and
gates `/login` until someone fills out the wizard at
`https://identity.<domain>/setup`. The wizard captures domain,
owner profile, registration mode, and notification recipients,
then emails a magic link to the owner address. Click the link,
land on `/admin`, you're the cluster owner.

### B) Unattended -- env vars on first boot

Set the full `IDENTITY_BOOTSTRAP_*` envelope on the identity
service before first boot. When all required values
(`DOMAIN`, `OWNER_EMAIL`, `OWNER_FIRST_NAME`, `OWNER_LAST_NAME`,
`REGISTRATION_MODE`) are present and the cluster hasn't been
bootstrapped yet, identity stamps `clusterSettings` and emails
the owner magic link automatically -- no `/setup` visit needed.

```bash
export IDENTITY_BOOTSTRAP_DOMAIN=staging.example.com
export IDENTITY_BOOTSTRAP_OWNER_EMAIL=alex@example.com
export IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME=Alex
export IDENTITY_BOOTSTRAP_OWNER_LAST_NAME=Stone
export IDENTITY_BOOTSTRAP_REGISTRATION_MODE=waitlist
# optional: phone, primary_role, gender, birthdate, org_name,
# registration_domains, internal_domains, internal_default_role,
# notify_emails -- all envs at IDENTITY_BOOTSTRAP_<NAME>
make dev-cluster-up
```

Operators who set SOME but not all of the required envs go
through the interactive wizard; their pre-set values prefill the
form. This means `IDENTITY_BOOTSTRAP_DOMAIN=local.znas.io` on
the dev compose default is fine -- the rest of the wizard fields
remain blank and the operator fills them in interactively the
first time.

---

## Cluster mode

```bash
make dev-cluster-restart           # bff + voice + cognition + agent + planner
make dev-cluster-restart-purge     # same, with DB volume wipe
```

Each node runs a build-tagged binary (`-tags voice`, `-tags cognition`,
etc.), all sharing one PostgreSQL database. The BFF dials the workers
via `WorkerDialer`, seeded by `MEMQL_WORKER_PEERS` and reconciled
against `v1:cluster:node`.

See [component/node/CLAUDE.md](../../../component/node/CLAUDE.md) for the full
node architecture.

---

## Cockpit (terminal IDE + ops console)

The Cockpit ships from its own repo,
[`github.com/znasllc-io/memql-cockpit`](https://github.com/znasllc-io/memql-cockpit);
build and product docs live there. It connects to this cluster over
gRPC (`bff.<domain>`).

---

## Next steps

- **Read the project overview:** [CLAUDE.md](../../../CLAUDE.md)
- **Find any doc:** [GLOSSARY.md](../../../GLOSSARY.md)
- **Architecture:** [docs/public/concepts/architecture.md](../concepts/architecture.md)
- **Write your first automation:** [docs/public/language/memql.md](../language/memql.md) -- the DSL reference; real examples live in `dsl/<namespace>/automations.memql`
- **MemQL gotchas:** [docs/public/language/authoring-rules.md](../language/authoring-rules.md) -- read before authoring `.memql` files

---

## Troubleshooting

### Port already in use

```bash
lsof -i :8088   # BFF HTTP
lsof -i :50050  # gRPC LB
lsof -i :5432   # PostgreSQL
```

### Docker not starting

```bash
docker ps                                                             # is docker up?
make dev-cluster-logs                                                 # check container logs
```

### Database connection errors

```bash
docker compose -f docker/docker-compose.cluster.yml exec postgres pg_isready -U memql
psql postgres://memql:memql_dev@localhost:5432/memql
```

### Concepts not loading

If `bff` logs say "no concepts loaded" or refuse to start, check for
schema-validation errors: a concept declaring a reserved payload field
(`createdBy`, `partition`, `id`, ...) bricks the whole loader. See
[memql-authoring-rules.md #19](../language/authoring-rules.md#19-reserved-intrinsics-do-not-redeclare-id--createdby--createdat--partition).

---

## Tips

- The voice/avatar overlay (`docker-compose.polyphon.yml`) is pending
  re-home onto the parity cluster (memql#1310); the cluster already ships
  a `livekit` service for the in-app voice path.
- pgAdmin is available at http://localhost:5050 with `--profile tools`.
