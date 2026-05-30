# memQL Deployment Strategy

**Last Updated**: May 30, 2026

## Overview

memQL uses **manual deployments** with automatic database migrations. The automatic deployment trigger has been **disabled** to ensure controlled, coordinated deployments of both code and database changes.

> **Platform migration in progress (epic #491).** The staging/production
> sections below describe the current **Google Cloud Run** deployment.
> A migration to **Azure Container Apps + Tiger Cloud** is underway; the
> first piece — an idempotent `make deploy-setup` bootstrap — has landed
> (see [Azure deploy bootstrap](#azure-deploy-bootstrap-make-deploy-setup)).
> The GCP docs are retained until the migration completes (later issues
> in the epic handle the cutover); do not treat them as removed yet.

---

## Azure deploy bootstrap (`make deploy-setup`)

`make deploy-setup` is the re-runnable foundation for the Azure
deployment (epic #491, issue #492). It is **idempotent**: it installs/
verifies + authenticates the toolchain and creates-or-converges the
core Azure resources, so re-running converges to correct state with no
duplicates and no drift. The implementation is function-based bash at
[`.claude/scripts/deploy-setup.sh`](.claude/scripts/deploy-setup.sh),
per the Skills+Scripts architecture.

### Usage

```bash
make deploy-setup                          # bootstrap staging (default)
make deploy-setup DRY_RUN=1                # print the plan, mutate nothing
make deploy-setup ENV=production DRY_RUN=1 # production path (parameterized stub)
make deploy-setup ARGS=--secrets-file=~/.memql/deploy.staging.env
make deploy-setup ARGS=--help              # full flag reference
```

`ENV` selects the environment (`staging` default, or `production`).
`DRY_RUN=1` forwards `--dry-run`, which prints the full plan and a
state report without touching Azure. `ARGS=...` forwards extra flags
to the script.

### What it does

1. **Toolchain** — install-if-missing-else-verify + authenticate:
   `az` (+ the `containerapp` extension), `gh`, the **Tiger Data CLI**
   `tiger`, `docker`, `jq`, `psql`. On macOS, installs go through
   Homebrew; on other platforms the script verifies and prints an
   install hint rather than running `sudo` from a make target. It
   never triggers an interactive login automatically — it detects
   whether `az` / `gh` / `tiger` sessions exist and tells you exactly
   which `… login` command to run if not.
2. **Azure resources** (create-or-converge, region **East US**), with
   an existence check before every create so nothing is duplicated:
   - `rg-memql-<env>` — resource group
   - `acrmemql` — container registry (Basic SKU, **shared** across
     envs; anchored to the staging resource group)
   - `kv-memql-<env>` — Key Vault
   - `cae-memql-<env>` — Container Apps environment
3. **Secrets → Key Vault** — refreshes the deployment secrets
   (`MEMORY_NODES_DATABASE_DSN`, content-ID salt,
   `MEMQL_SI_OPENAI_API_KEY`, `MEMQL_SI_OPENAI_PROJECT_ID`, and the
   Discord webhook URLs). The secret **names** are authoritative from
   [`service.yaml`](service.yaml); the **values** are pulled from a
   gitignored env file (`--secrets-file`, default `.env.deploy.<env>`)
   or prompted interactively — **never** hardcoded in the repo. A
   value is only written when it differs from what's already stored,
   so re-runs are a true no-op.
4. **State report** — prints what already existed, what was created,
   what was reconciled, and what was skipped (missing tool / auth /
   value).

### Secret values file

Provide secret values via a gitignored file (matched by the repo's
`.env.*` ignore rules), one `KEY=VALUE` per line, using the env-var
names from `service.yaml`. Example `.env.deploy.staging`:

```
MEMORY_NODES_DATABASE_DSN=postgres://...
MEMORY_NODES_ZNASLLC_LAB_CONTENTID_SALT=...
MEMQL_SI_OPENAI_API_KEY=sk-...
MEMQL_SI_OPENAI_PROJECT_ID=proj_...
MEMQL_SECRET_VARIABLE_DISCORD_DEVAUTO_WEBHOOK_URL=https://discord.com/api/webhooks/...
MEMQL_SECRET_VARIABLE_DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/...
```

Already-exported environment variables take precedence over the file.

### Status / remaining work

The script is correct, `shellcheck`-clean, and fully dry-run-able, and
is covered by Go tests under [`scripts/deploy/`](scripts/deploy/) that
run in CI (`go test ./...`). It has **not** yet been run against a live
Azure subscription — that validation is the remaining step and is
gated on the operator's `az login` access (epic #491 external
prerequisite). The `production` path is wired and parameterized but
stubbed pending that same validation.

---

## Tiger Cloud DB provisioning (`make db-provision`)

`make db-provision` provisions the managed **Tiger Cloud** (Timescale
Community + pgvector) database for the environment and wires its
connection DSN into the per-env Key Vault (epic #491, issue #494). It is
**idempotent**: a re-run detects the existing service and only rewrites
the DSN secret when it actually rotated. The implementation is
function-based bash at
[`scripts/deploy/tiger-provision.sh`](scripts/deploy/tiger-provision.sh),
per the Skills+Scripts architecture.

### Usage

```bash
make db-provision                          # provision staging DB (default)
make db-provision DRY_RUN=1                # print the plan, mutate nothing
make db-provision ENV=production DRY_RUN=1 # production path (parameterized stub)
make db-provision ARGS=--help              # full flag reference
```

`ENV` selects the environment (`staging` default, or `production`).
`DRY_RUN=1` forwards `--dry-run`, which prints the full plan and a
state report without touching Tiger Cloud or Azure.

### What it does

1. **Tiger Cloud service** — via the **Tiger Data CLI** (`tiger`),
   create-or-verify the `memql-<env>` service in **Azure East US**
   (region slug `azure-eastus`). The service is existence-checked
   before create (matched on the service name in `tiger service list`),
   so it is never duplicated.
2. **Extensions** — confirm the two extensions memQL needs:
   `timescaledb` (Timescale Community, pre-enabled on Tiger Cloud) and
   `vector` (pgvector). The check runs an idempotent
   `CREATE EXTENSION IF NOT EXISTS` against the service, which is a
   no-op when the extension is already enabled.
3. **DSN → Key Vault** — read the service's libpq connection DSN from
   the `tiger` CLI and store it as the Key Vault secret
   `memory-nodes-database-dsn` in `kv-memql-<env>`. The secret name is
   the **same** one [`deploy-setup.sh`](.claude/scripts/deploy-setup.sh)
   uses for `MEMORY_NODES_DATABASE_DSN`, so the two scripts converge on
   one secret. The value is only written when it differs from what's
   already stored — so a re-run that didn't rotate the DSN is a true
   no-op. An exported `MEMORY_NODES_DATABASE_DSN` overrides the read
   (lets an operator wire a manually-rotated DSN).
4. **Auto-migrations** — memQL runs migrations on backend start
   (`MEMORY_NODES_DATABASE_AUTO_MIGRATE=true`,
   `MEMORY_NODES_DATABASE_MIGRATE_ON_START=true` from
   [`service.yaml`](service.yaml)), so the schema converges against
   this DB on the first deploy. No separate migration step is needed.
5. **State report** — prints what already existed, what was created,
   what was rotated, and what was skipped (missing tool / auth / value).

On a host without the `tiger` CLI (or where it isn't authenticated),
the script verifies-and-instructs rather than auto-installing — it
prints the exact `tiger auth login` / install commands and points at
`make deploy-setup` for the full toolchain bootstrap.

### Status / remaining work

The script is `shellcheck`-clean and fully dry-run-able, and is covered
by Go tests under [`scripts/deploy/`](scripts/deploy/) that run in CI
(`go test ./...`). It has **not** yet been run against a live Tiger
Cloud account — that validation is the remaining step, gated on the
operator's Tiger Cloud account (epic #491 external prerequisite). The
exact `tiger` subcommand surface (`service create`,
`get-connection-string`, `service exec`) is pinned to the documented
CLI and may need a small adjustment once exercised live. The
`production` path is wired and parameterized but stubbed.

---

## Cluster deploy to Container Apps (`make deploy`)

`make deploy ENV=staging|production` builds + pushes the cluster images
and deploys the **cluster of worker nodes** to Azure Container Apps
(epic #491, issue #495). The backend is **not** a single binary: it is a
set of node-type workers sharing the one Tiger Cloud DB. The
implementation is function-based bash at
[`.claude/scripts/deploy.sh`](.claude/scripts/deploy.sh), per the
Skills+Scripts architecture, mirroring `deploy-setup.sh`.

### Cluster shape (what gets deployed)

Two image families land in the one ACA environment
(`cae-memql-<env>`):

| App | Image | `MEMQL_NODE_TYPE` | Ingress | Port |
|-----|-------|-------------------|---------|------|
| `ca-memql-cognition-<env>` | `acrmemql.azurecr.io/memql:<ver>` | `cognition` | internal | 8085 |
| `ca-memql-voice-<env>`     | `…/memql:<ver>` | `voice`     | internal | 8085 |
| `ca-memql-agent-<env>`     | `…/memql:<ver>` | `agent`     | internal | 8085 |
| `ca-memql-planner-<env>`   | `…/memql:<ver>` | `planner`   | internal | 8085 |
| `ca-memql-identity-<env>`  | `…/memql:<ver>` | `identity`  | internal | 8085 |
| `ca-memql-workbench-<env>` | `…/memql:<ver>` | `workbench` | internal | 8085 |
| `ca-copresent-bff-<env>`   | `…/memql-bff-copresent:<carrier-ver>` | `bff` | **external** | 8085 |

- The **engine node-types** run the `memql` image, selected by
  `MEMQL_NODE_TYPE`, and take **internal** ingress — they are workers
  inside the ACA environment and never face the public internet.
  (Node-type list + ports match
  [`docker/docker-compose.cluster.yml`](docker/docker-compose.cluster.yml).)
- The **CoPresent BFF carrier** runs the `memql-bff-copresent` carrier
  image (`MEMQL_NODE_TYPE=bff`) and takes **external** ingress — it is
  the node the frontend hits, mapping to `api.<env>.copresent.ai`
  later. Its image is built from the **sibling**
  [`../memql-bff-copresent`](../memql-bff-copresent) repo, whose
  `make release` build context spans both repos (its `go.mod` has
  `replace ../memql`). CoPresent **pins** this carrier at a specific
  version; `--carrier-version` / `CARRIER_VERSION` targets that pinned
  tag.

### Usage

```bash
make deploy                                          # staging, VERSION from the VERSION file
make deploy DRY_RUN=1                                # print the full plan, mutate nothing
make deploy ENV=staging CARRIER_VERSION=0.9.0        # pin the BFF carrier tag
make deploy SKIP_BUILD=1 VERSION=0.9.0 CARRIER_VERSION=0.9.0  # deploy already-pushed tags
make deploy ENV=production DRY_RUN=1                 # production path (parameterized stub)
make deploy ARGS=--help                              # full flag reference
```

`ENV` selects the environment. `VERSION` is the engine (`memql`) image
tag (default: the [`VERSION`](VERSION) file's semver). `CARRIER_VERSION`
is the BFF carrier tag (default: the sibling repo's `VERSION`, else
`VERSION`). `SKIP_BUILD=1` deploys already-pushed tags without
rebuilding. `DRY_RUN=1` forwards `--dry-run`.

### What it does

1. **Build + push** the two images to the shared ACR (`acrmemql`):
   the engine image via this repo's
   [`scripts/release/release.sh`](scripts/release/release.sh)
   (`make release` equivalent), and the carrier image via the sibling
   repo's `make release` (so the cross-repo build context is owned by
   the repo that defines it). Skippable with `SKIP_BUILD=1`. A pre-step
   runs `az acr login` so the release scripts' `docker push` can
   publish.
2. **Deploy** each Container App, **create-or-update** (idempotent): an
   existence check (`az containerapp show`) picks `create` vs `update`,
   so re-running converges with no duplicates. Ingress is converged
   separately so a flip (internal ↔ external) is reconciled on re-run.
   Each app gets `--min-replicas 1` and `--system-assigned` managed
   identity.
3. **Secrets + env** — the DSN rides a **Key Vault secret reference**
   (`keyvaultref:…/secrets/memory-nodes-database-dsn` +
   `identityref:system`) resolved at runtime by the app's managed
   identity, exposed to the container as
   `MEMORY_NODES_DATABASE_DSN=secretref:…`. Non-secret env mirrors
   [`service.yaml`](service.yaml) (`SERVER_ADDRESS=0.0.0.0:8085`,
   `MEMQL_GRPC_ADDRESS=:50051`, the auto-migrate flags).
4. **State report** — prints which apps were created vs updated, with
   which image tags + ingress, and what was skipped.

### Status / remaining work

The script is `shellcheck`-clean and fully dry-run-able, and is covered
by Go tests under [`scripts/deploy/`](scripts/deploy/) that run in CI
(`go test ./...`). It has **not** yet been run against a live Azure
subscription — that validation (plus the live image build/push, which
needs Docker + the sibling carrier checkout) is the remaining step,
gated on the same `az login` access and on `make deploy-setup` +
`make db-provision` having created the resources. The exact
`az containerapp` flag surface (secret/Key-Vault-ref syntax, ingress
flip) may need a small adjustment once exercised live. The `production`
path is wired and parameterized but stubbed.

---

## Genesis envelope in cloud — the A2 secrets model

The cloud cluster's config is the **genesis envelope**: ~150 vars
(OpenAI / Anthropic / Deepgram / JumpCloud / avatar keys, identity
URLs, …). Locally, `make dev-refresh` decrypts `genesis.znas`
host-side into a plaintext `env_file` that docker-compose mounts. For
cloud we keep the envelope **sealed** and decrypt it **in-process at
boot** — it never lands on disk decrypted in Azure. This is the
architect-chosen **A2** model (issue #518, epic #491).

### How it works

A boot hook in `component/genesis` (`AutoloadFromEnv`, called from
`main.go` before any config is read) is gated by one env var:

| Env var | Role |
|---------|------|
| `MEMQL_GENESIS_AUTOLOAD` | Master switch. Set to `true` to enable in-process decrypt. **Unset/anything else = no-op** — local dev's `env_file` path is completely untouched. |
| `MEMQL_GENESIS_B64` | The **encrypted** envelope, base64-encoded, carried directly in an env var. **Preferred for cloud**: decoded and decrypted in memory, never written to disk. |
| `MEMQL_GENESIS_PATH` | Path to a sealed envelope file (default `~/.memql/genesis.znas`). Used when `MEMQL_GENESIS_B64` is empty. |
| `MEMQL_MASTER_KEY` | The 32-byte (64 hex chars) key that decrypts the envelope. Already read from the process env by the `secret`/`genesis` packages. |

When `MEMQL_GENESIS_AUTOLOAD=true`, boot:

1. Sources the encrypted envelope bytes from `MEMQL_GENESIS_B64`
   (preferred) or, if absent, from the `MEMQL_GENESIS_PATH` file.
2. Decrypts in-process under `MEMQL_MASTER_KEY` via the existing
   `secret.OpenBlob` path (`genesis.OpenBytes` for the B64 case —
   the in-memory twin of `OpenFile`, so the ciphertext never touches
   a temp file).
3. Applies each entry to the process environment **set-if-absent** —
   it never overwrites a var that is already set.

### Overrides win (set-if-absent)

The set-if-absent rule is the crux of the model. Container App
overrides — the Tiger `MEMORY_NODES_DATABASE_DSN` (a Key Vault secret
reference), `MEMQL_NODE_TYPE`, identity host URLs,
`SERVER_ALLOWED_ORIGINS` — are set in the container's environment
**before** the process starts. Because auto-load only fills in vars
that are *absent*, those per-deploy overrides always win over the
envelope's local-dev defaults. The envelope is the **base layer**;
the Container App env is the override layer on top.

(Locally the layering is the same shape: genesis envelope = base, the
repo-root `.env` override = top. The difference is local dev decrypts
host-side into `env_file` and does not set `MEMQL_GENESIS_AUTOLOAD`,
so this in-process path is dormant.)

### Fail-closed

If `MEMQL_GENESIS_AUTOLOAD=true` but the envelope or master key is
missing or undecryptable, boot **fails with a clear fatal error** —
it does not silently come up mis-configured:

- `MEMQL_MASTER_KEY` unset → fatal.
- No envelope source (`MEMQL_GENESIS_B64` empty **and** the path
  doesn't exist) → fatal.
- Bad base64, truncated/tampered ciphertext, or wrong master key →
  fatal.

### Deploy wiring (next step)

The deploy script passes the sealed envelope as `MEMQL_GENESIS_B64`
and `MEMQL_MASTER_KEY` (the latter as a Key Vault secret reference,
like the DSN) to each Container App, alongside the per-node overrides.
That deploy-script rework is tracked as the follow-up to #518 and
replaces the earlier "6 individual secrets" approach.

---

## Release & versioning (semver tag -> immutable image)

This section establishes memQL's release convention for the Azure
deployment foundation (znasllc-io/memql#493, epic #491): a memQL
**semver tag** maps to a single **immutable container image**, and
that image tag is the one number CoPresent pins.

### Dependency direction (why memQL carries no BFF require)

memQL is the **upstream** module. The CoPresent BFF
(`github.com/visionarys-io/memql-bff-copresent`) imports memQL's Go
packages (`app`, `server`, `genesis`, `core/...`) and mounts its own
`copresent/` DSL subtree into memQL's engine at boot via
`dsl.RegisterTree` (see [`dsl/embed.go`](dsl/embed.go)). The import
graph therefore points **BFF -> memQL**, not the other way:

```
  memql-bff-copresent  (require memql + replace ../memql for dev)
            │  imports app/server/genesis, calls dsl.RegisterTree
            ▼
        memql            (no source import of the BFF)
```

Because memQL imports **zero** BFF packages, a `require
github.com/visionarys-io/memql-bff-copresent` line in memQL's
`go.mod` does not survive `go mod tidy` — Go strips any required
module that nothing in the build graph imports. (Verified: a manual
`go get ...@v0.2.0` adds an `// indirect` line that the next
`go mod tidy` removes, and `GOWORK=off go build ./...` still
succeeds because there was nothing BFF-shaped to resolve.) Forcing
the require via a blank import is impossible anyway — the BFF imports
memQL, so memQL importing the BFF would be a compile-time import
cycle.

The pin that actually matters flows the other way and is an **image
tag**, not a module version: memQL releases `memql:X.Y.Z`, and
CoPresent's `deploy/backend-version` file pins that tag
(visionarys-io/copresent#140). The `go.work` workspace at the repo
parent (`use ./memql ./memql-bff-copresent ./memql-cockpit`) is what
gives local cross-repo dev its edit-and-rebuild loop; it is unchanged
by this convention. CI / release builds run with `GOWORK=off` so they
resolve purely from `go.mod` + `go.sum`.

A CI-style guard for this lives in
[`scripts/release/release_test.go`](scripts/release/release_test.go)
(`TestStandaloneBuildResolves`): it runs `GOWORK=off go mod verify`
so a regression that made the standalone build need the workspace
fails `go test ./...`.

### `make release` — cut an immutable image

```bash
# Local image, version = the VERSION file's semver (0.9.0):
make release

# Explicit version, build + push the pinnable tag to the shared ACR:
make release VERSION=0.9.0 ACR=acrmemql PUSH=1

# Plan only (build/push nothing):
make release VERSION=0.9.0 ACR=acrmemql PUSH=1 DRY_RUN=1
```

The target is a one-liner over
[`scripts/release/release.sh`](scripts/release/release.sh) (per the
function-based shell-script convention in CLAUDE.md). It:

- Resolves the version from `--version` or the `VERSION` file (now a
  plain `X.Y.Z` with no suffix; the script still strips any legacy
  `-<epoch>` dev stamp before the first `-` for safety). The version
  must be strict `X.Y.Z`.
- Resolves the short git SHA and stamps it onto the image as
  `org.opencontainers.image.revision` (plus
  `org.opencontainers.image.version`), so the immutable `X.Y.Z` tag
  is always traceable back to an exact commit. A dirty tree marks the
  revision `<sha>-dirty`.
- Builds from [`docker/memql.Dockerfile`](docker/memql.Dockerfile)
  and tags `<registry/>memql:X.Y.Z` (registry derived from `ACR`
  -> `<acr>.azurecr.io`, or set directly via `REGISTRY`; empty =
  local-only).
- Treats the tag as **write-once**: with `PUSH=1` it refuses to
  overwrite an existing `X.Y.Z` tag in the registry unless
  `ALLOW_OVERWRITE=1` is passed. That immutability is what makes the
  tag a trustworthy pin for CoPresent.

### Promotion flow (memQL tag -> image -> CoPresent pin)

```
   git tag vX.Y.Z on memql main        (architect cuts the tag)
            │
            ▼
   make release VERSION=X.Y.Z ACR=acrmemql PUSH=1
            │   builds + pushes acrmemql.azurecr.io/memql:X.Y.Z (immutable)
            ▼
   CoPresent deploy/backend-version = X.Y.Z      (reviewed PR; copresent#140)
            │   make check-backend-pin verifies the image exists in ACR
            ▼
   CoPresent staging/prod deploy runs against the pinned backend image
```

The backend lane (`ca-memql-*` tracking memQL `main`) moves
continuously; the **pinned** lane (`ca-memql-pinned` behind
`api.staging.copresent.ai`) only moves when someone bumps
CoPresent's `backend-version` to a new `memql:X.Y.Z` in a reviewed
PR. One memQL tag transitively fixes one BFF-compatible engine build.

### Versioning lineage

As of the 2026-05-30 platform versioning reset (epic
[znasllc-io/memql#501](https://github.com/znasllc-io/memql/issues/501)),
memQL is on a clean **`0.9.0`** baseline. The old `2.3.0-<epoch>` dev
stamp and the orphaned `v0.1.0` tag are both retired; **git tag is the
single source of truth** and there are no epoch suffixes. The first
clean release tag is `v0.9.0`, cut on `main`.

See [VERSIONING.md](VERSIONING.md) for the full policy (semver,
pre-1.0 rules, `1.0.0` cut at the invite-only beta) and
[COMPATIBILITY.md](COMPATIBILITY.md) for the platform pin chain
(copresent → memql-bff-copresent carrier → memQL; memql-cockpit
declares a minimum memQL/protocol).

---

## Environments

| Environment | Region | Database | Purpose | Access |
|-------------|--------|----------|---------|--------|
| **Development** | Docker | Local PostgreSQL + TimescaleDB | Development | All developers |
| **Staging** | us-central1 | Tiger Cloud (staging instance) | QA & Testing | All developers |
| **Production** | us-west1 | Tiger Cloud (production instance) | Live system | Senior/Lead only |

---

## START Deployment Flow

```
┌─────────────────┐
│ 1. DEVELOPMENT  │  docker compose up --build
│                 │  - Docker PostgreSQL
│                 │  - Isolated database
└────────┬────────┘
         │
         │ Test locally, run tests
         │
         ▼
┌─────────────────┐
│ 2. STAGING ENV  │  gcloud run deploy
│ (us-central1)   │  - Cloud Run
│                 │  - Tiger Cloud (staging)
│                 │  - Auto migrations
└────────┬────────┘
         │
         │ QA, integration testing
         │
         ▼
┌─────────────────┐
│ 3. PRODUCTION   │  gcloud run deploy (production)
│ (us-west1)      │  - Cloud Run
│                 │  - Tiger Cloud (production)
│                 │  - Auto migrations
│                 │  - [WARNING] Requires confirmation
└─────────────────┘
```

---

## [LIST] Available Commands

### Development

```bash
# Start local environment
docker compose -f docker/docker-compose.full.yml up --build

# Run tests
go test ./...

# View logs
docker compose -f docker/docker-compose.full.yml logs -f

# Database shell
psql postgres://memql:memql_dev@localhost:5432/memql

# Stop services
docker compose -f docker/docker-compose.full.yml down
```

### Staging Deployment

```bash
# Deploy to staging (Google Cloud Run)
gcloud run deploy

# Check logs
gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=anequim-memql-staging AND resource.labels.location=us-central1" --limit 50

# Rollback if needed
gcloud run services update-traffic anequim-memql-staging \
  --region us-central1 \
  --to-revisions [PREVIOUS-REVISION]=100
```

### Production Deployment

```bash
# Deploy to production (Google Cloud Run, requires confirmation)
gcloud run deploy

# Check logs
gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=anequim-memql-production AND resource.labels.location=us-west1" --limit 50

# Rollback (DANGEROUS - use with caution)
gcloud run services update-traffic anequim-memql-production \
  --region us-west1 \
  --to-revisions [PREVIOUS-REVISION]=100
```

---

## Database Migrations

### How It Works

**Automatic on deployment:**
- Environment variables enable auto-migrations:
  - `MEMORY_NODES_DATABASE_AUTO_MIGRATE=true`
  - `MEMORY_NODES_DATABASE_MIGRATE_ON_START=true`
- Migration files: `component/database/memory-nodes/migrations/`
- Format: `YYYYMMDDHHMMSS_description.{up,down}.sql`

### Creating Migrations

1. **Create migration files:**
   ```bash
   # Create new migration
   touch component/database/memory-nodes/migrations/20260209120000_add_feature.up.sql
   touch component/database/memory-nodes/migrations/20260209120000_add_feature.down.sql
   ```

2. **Write SQL:**
   ```sql
   -- .up.sql (forward migration)
   ALTER TABLE memory_nodes ADD COLUMN new_field TEXT;

   -- .down.sql (rollback migration)
   ALTER TABLE memory_nodes DROP COLUMN new_field;
   ```

3. **Test locally:**
   ```bash
   docker compose -f docker/docker-compose.full.yml up --build
   # Check logs for migration success
   ```

4. **Deploy to staging:**
   ```bash
   gcloud run deploy
   # Verify migration in staging database
   ```

5. **Deploy to production:**
   ```bash
   gcloud run deploy  # production
   # Monitor migration logs carefully
   ```

### Rollback Migrations

**Code rollback** (automatic via Cloud Run):
```bash
gcloud run services update-traffic anequim-memql-staging --region us-central1 --to-revisions [PREVIOUS-REVISION]=100  # or rollback production revision
```

**Database rollback** (manual):
```bash
# Connect to database
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN')"

# Check current migrations
SELECT * FROM bun_migrations ORDER BY group_id DESC LIMIT 10;

# Apply .down.sql file manually
psql "CONNECTION_STRING" < component/database/memory-nodes/migrations/[MIGRATION].down.sql
```

---

## Configuration

### Staging Environment (us-central1)

- **Service**: anequim-memql-staging
- **Region**: us-central1
- **URL**: https://anequim-memql-staging-439288787761.us-central1.run.app
- **Resources**: 2Gi RAM, 2 CPU
- **Scaling**: 0-10 instances (scale to zero)
- **Database**: Tiger Cloud (staging instance)
- **Secrets**: Staging-specific secrets
- **Cost**: ~$5-20/month

### Production Environment (us-west1)

- **Service**: anequim-memql-production
- **Region**: us-west1
- **URL**: https://anequim-memql-production-439288787761.us-west1.run.app
- **Resources**: 4Gi RAM, 4 CPU
- **Scaling**: 1-20 instances (always running)
- **Database**: Tiger Cloud (production instance)
- **Secrets**: Production-specific secrets
- **Cost**: ~$50-200/month

---

## Security & Secrets

All secrets stored in **Google Cloud Secret Manager**:

**Note:** Development environment variables follow the bootstrap-envelope-plus-concept-storage model: a tiny set of bootstrap vars in `.env.local` (generated by `make bootstrap`), with everything else in memQL's `v1:platform:globalVariable` and `v1:platform:globalSecret` concepts populated via `make secrets-init` + `make secrets-seed`. See [docs/guides/env-vars.md](docs/guides/env-vars.md).

```bash
# List secrets
gcloud secrets list

# View secret value (requires permission)
gcloud secrets versions access latest --secret="SECRET_NAME"

# Update secret
echo -n "new-value" | gcloud secrets versions add SECRET_NAME --data-file=-

# Trigger redeployment to pick up new secret
gcloud run services update anequim-memql-staging --region us-central1  # or anequim-memql-production --region us-west1
```

### Required Secrets

| Secret | Staging | Production | Purpose |
|--------|---------|------------|---------|
| `MEMORY_NODES_DATABASE_DSN` | [OK] | [OK] (separate) | Database connection |
| `MEMQL_SI_OPENAI_API_KEY` | [OK] | [OK] | OpenAI API |
| `JUMPCLOUD_CLIENT_ID` | [OK] | [OK] | Auth |
| `JUMPCLOUD_CLIENT_SECRET` | [OK] | [OK] | Auth |
| `BFF_ANEQUIM_*_CLIENT_ID` | Staging | Prod | Client config |
| `BFF_ANEQUIM_*_CLIENT_SECRET` | Staging | Prod | Client config |

---

## Emergency Procedures

### Staging Deployment Failed

1. **Check build logs**:
   ```bash
   gcloud builds list --limit 5
   gcloud builds log [BUILD_ID]
   ```

2. **Check service logs**:
   ```bash
   gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=anequim-memql-staging AND resource.labels.location=us-central1 AND severity>=ERROR" --limit 50
   ```

3. **Rollback if needed**:
   ```bash
   gcloud run services update-traffic anequim-memql-staging --region us-central1 --to-revisions [PREVIOUS-REVISION]=100
   ```

### Production Deployment Failed

[WARNING] **CRITICAL - Follow these steps immediately:**

1. **Rollback code immediately**:
   ```bash
   # List revisions
   gcloud run revisions list --service anequim-memql-production --region us-west1

   # Rollback to previous
   gcloud run services update-traffic anequim-memql-production \
     --region us-west1 \
     --to-revisions [PREVIOUS-REVISION]=100
   ```

2. **Check if migration failed**:
   ```bash
   gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=anequim-memql-production AND resource.labels.location=us-west1 AND jsonPayload.msg:migration" --limit 50
   ```

3. **Rollback database if needed** (get DBA help):
   ```bash
   # Connect to production database
   psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD')"

   # Apply .down.sql migration
   ```

4. **Notify team** and escalate to Senior/Lead developer

---

## INFO Monitoring

### Check Service Health

```bash
# Staging
gcloud run services describe anequim-memql-staging --region us-central1

# Production
gcloud run services describe anequim-memql-production --region us-west1
```

### View Logs

```bash
# Staging - recent logs
gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=anequim-memql-staging AND resource.labels.location=us-central1" --limit 100

# Production - errors only
gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=anequim-memql-production AND resource.labels.location=us-west1 AND severity>=ERROR" --freshness=1h

# Migration logs (staging)
gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=anequim-memql-staging AND jsonPayload.msg:migration" --limit 50
```

---

## [REFRESH] Deployment Checklist

### Staging Deployment

- [ ] Code changes tested locally (`docker compose -f docker/docker-compose.full.yml up --build`)
- [ ] Tests passing (`go test ./...`)
- [ ] Code reviewed
- [ ] Deploy to staging: `gcloud run deploy`
- [ ] Verify deployment health
- [ ] Test key functionality

### Production Deployment

- [ ] [OK] All staging deployment checks passed
- [ ] [OK] Staging deployment tested and verified
- [ ] [OK] Database migrations tested in staging
- [ ] [OK] Code review completed
- [ ] [OK] Senior/Lead developer approval
- [ ] [OK] Rollback plan prepared
- [ ] [OK] Team notified of deployment
- [ ] [OK] Deploy to production: `gcloud run deploy`
- [ ] [OK] Monitor logs for 15 minutes
- [ ] [OK] Test critical functionality
- [ ] [OK] Notify team of completion

---

## NOTE Change Log

### February 9, 2026
- **Disabled automatic deployment trigger** (was deploying on push to `develop`)
- **Created manual deployment process** for staging and production
- **Established three-environment strategy** (development → staging → production)
- **Configured automatic migrations** for all environments
- **Documented rollback procedures** for both code and database

### Previous State
- Automatic deployment via Cloud Build trigger on push to `develop`
- Deployments to us-west1 only
- No clear separation between staging and production
- Manual migration management

---

## [HELP] Support

- **Staging issues**: All developers can troubleshoot
- **Production issues**: Escalate to Senior/Lead developers
- **Database issues**: Check Tiger Cloud dashboard
- **Secrets issues**: Verify in Google Cloud Secret Manager

---

**Remember**: Always test in staging before deploying to production!
