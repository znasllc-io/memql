# memQL Deployment Strategy

**Last Updated**: February 21, 2026

## Overview

memQL uses **manual deployments** with automatic database migrations. The automatic deployment trigger has been **disabled** to ensure controlled, coordinated deployments of both code and database changes.

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
