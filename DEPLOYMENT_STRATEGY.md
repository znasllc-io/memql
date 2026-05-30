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
# Local image, version = the VERSION file's semver prefix:
make release

# Explicit version, build + push the pinnable tag to the shared ACR:
make release VERSION=2.4.0 ACR=acrmemql PUSH=1

# Plan only (build/push nothing):
make release VERSION=2.4.0 ACR=acrmemql PUSH=1 DRY_RUN=1
```

The target is a one-liner over
[`scripts/release/release.sh`](scripts/release/release.sh) (per the
function-based shell-script convention in CLAUDE.md). It:

- Resolves the version from `--version` or the clean **semver
  prefix** of the `VERSION` file (the part before the first `-`; the
  file's epoch suffix `2.3.0-<epoch>` is a dev stamp and is dropped
  for a release tag). The version must be strict `X.Y.Z`.
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

The `VERSION` file currently reads `2.3.0-<epoch>` (a dev stamp), and
the only git tag on the repo is `v0.1.0`. The first clean release tag
should reconcile the `2.3.x` lineage in `VERSION` rather than continue
the orphaned `v0.1.x` line — i.e. `v2.4.0` (semver minor bump over the
`2.3.x` working line, signalling the first deployable Azure cut). The
actual tag is the architect's call; this document describes the
mechanism, not the number.

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
