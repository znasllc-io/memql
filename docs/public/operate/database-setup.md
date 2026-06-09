---
title: Database Setup
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Database Setup

**Last Updated**: February 21, 2026

## Overview

memQL uses **separate databases** for each environment to ensure complete isolation and safety.

---

## Database Instances

| Environment | Database | Service ID | Region | Resources | Purpose |
|-------------|----------|------------|--------|-----------|---------|
| **Development** | Docker PostgreSQL + TimescaleDB | N/A | localhost | 2Gi | Development |
| **Staging** | Tiger Cloud: `db-99414` | `rt1dn6vj9g` | us-east-1 | 1 CPU, 4GB | QA & Testing |
| **Production** | [PENDING] **Not created yet** (deferred to avoid costs) | TBD | TBD | TBD | Live system |

---

## AUTH Secrets Configuration

### Google Cloud Secret Manager

**Note:** Development environment variables follow the bootstrap-envelope-plus-concept-storage model -- run `make bootstrap` for the small required env set, then `make secrets-init` + `make secrets-seed` to populate the rest in memQL's concept storage. See [docs/public/operate/env-vars.md](env-vars.md).

| Secret Name | Environment | Database | Used By |
|-------------|-------------|----------|---------|
| `MEMORY_NODES_DATABASE_DSN` | WARNING: Legacy | Staging (original) | Legacy/backup |
| `MEMORY_NODES_DATABASE_DSN_LAB` | Staging | `db-99414` | Staging deployment |
| `MEMORY_NODES_DATABASE_DSN_PROD` | [PENDING] **Not created** | TBD | Production (when ready) |

### Environment Variable Mapping

Both deployments use the **same environment variable name** in the container:
```bash
MEMORY_NODES_DATABASE_DSN
```

But they reference **different secrets**:

**Staging deployment:**
```bash
--set-secrets "MEMORY_NODES_DATABASE_DSN=MEMORY_NODES_DATABASE_DSN_LAB:latest"
#              ↑ Container env var         ↑ Secret in Secret Manager
```

**Production deployment:**
```bash
--set-secrets "MEMORY_NODES_DATABASE_DSN=MEMORY_NODES_DATABASE_DSN_PROD:latest"
#              ↑ Container env var         ↑ Secret in Secret Manager
```

**Result**: Your code always reads `MEMORY_NODES_DATABASE_DSN`, but each environment connects to its own database.

---

## START Database Management with Tiger CLI

### Installation

```bash
brew install timescaledb/tap/tiger-cli
```

### Authentication

```bash
tiger auth login
# Opens browser for OAuth authentication
```

### List Databases

```bash
tiger service list
```

### Create New Database

```bash
tiger service create \
  --name my-database \
  --environment PROD \
  --region us-east-1 \
  --cpu 1000 \
  --memory 4 \
  --addons time-series \
  --with-password \
  --output env
```

### Connect to Database

```bash
# Connect to default service
tiger db connect

# Connect to specific service
tiger db connect --service-id xdejfkq0s1

# Get connection string
tiger service info --service-id xdejfkq0s1 --output env
```

### Common Commands

```bash
# View service details
tiger service info

# Scale resources
tiger service update --cpu 2000 --memory 8

# Delete service (DANGEROUS!)
tiger service delete --service-id [ID]

# View metrics
tiger service metrics

# View logs
tiger service logs
```

---

## INFO Database Connections

### Staging Database

```
Host: rt1dn6vj9g.wb2g0uu9oq.tsdb.cloud.timescale.com
Port: 39610
Database: tsdb
User: tsdbadmin
Password: [stored in MEMORY_NODES_DATABASE_DSN_LAB secret]
```

**Access:**
```bash
# Via Tiger CLI
tiger db connect --service-id rt1dn6vj9g

# Via psql (get connection string from secret)
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_LAB')"
```

### Production Database

```
Host: xdejfkq0s1.wb2g0uu9oq.tsdb.cloud.timescale.com
Port: 36157
Database: tsdb
User: tsdbadmin
Password: [stored in MEMORY_NODES_DATABASE_DSN_PROD secret]
```

**Access:**
```bash
# Via Tiger CLI
tiger db connect --service-id xdejfkq0s1

# Via psql (get connection string from secret)
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD')"
```

---

## [REFRESH] Database Migrations

### Automatic Migrations

Both staging and production databases have **automatic migrations enabled** via:
- `MEMORY_NODES_DATABASE_AUTO_MIGRATE=true`
- `MEMORY_NODES_DATABASE_MIGRATE_ON_START=true`

### Migration Flow

```
1. Create migration files:
   component/database/memory-nodes/migrations/
   ├── 20260209120000_feature.up.sql    (forward)
   └── 20260209120000_feature.down.sql  (rollback)

2. Test locally:
   docker compose -f docker/docker-compose.full.yml up --build
   # Migrations run automatically

3. Test in staging:
   gcloud run deploy  # staging
   # Migrations run on staging database

4. Deploy to production:
   gcloud run deploy  # production
   # Migrations run on production database
```

### Check Migration Status

```bash
# Staging database
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_LAB')" \
  -c "SELECT * FROM bun_migrations ORDER BY group_id DESC LIMIT 10;"

# Production database
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD')" \
  -c "SELECT * FROM bun_migrations ORDER BY group_id DESC LIMIT 10;"
```

### Manual Migration Rollback

```bash
# Connect to database
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD')"

# Apply .down.sql file
\i component/database/memory-nodes/migrations/[TIMESTAMP]_[NAME].down.sql
```

---

## Security Best Practices

### Secret Management

1. **Never commit database credentials** to git
2. **Rotate passwords regularly**:
   ```bash
   # Rotate in Tiger Cloud dashboard
   # Then update secret:
   echo -n "new-connection-string" | gcloud secrets versions add MEMORY_NODES_DATABASE_DSN_PROD --data-file=-

   # Redeploy to pick up new secret
   gcloud run deploy  # production
   ```

3. **Limit access** to production secrets:
   ```bash
   # Only Senior/Lead developers should have access to:
   # - MEMORY_NODES_DATABASE_DSN_PROD secret
   # - Production database in Tiger Cloud
   ```

### Database Access

- **Staging**: All developers can access for testing
- **Production**: Senior/Lead developers only
- **Development**: Each developer has isolated Docker database

### Backup Strategy

Tiger Cloud provides automatic backups:
- **Point-in-time recovery** (PITR)
- **Daily snapshots**
- **Retention**: Check your plan

Access backups in Tiger Cloud dashboard:
```bash
# Or via CLI
tiger service backups --service-id xdejfkq0s1
```

---

## Emergency Procedures

### Production Database Issues

1. **Check Tiger Cloud status**: https://status.timescale.com/
2. **View database logs**:
   ```bash
   tiger service logs --service-id xdejfkq0s1
   ```
3. **Check connections**:
   ```bash
   psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD')" -c "SELECT count(*) FROM pg_stat_activity;"
   ```
4. **Restore from backup** (if needed):
   - Go to Tiger Cloud dashboard
   - Select service → Backups
   - Choose restore point

### Connection Issues

```bash
# Test connection
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD')" -c "SELECT 1;"

# Check if secret is correct
gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD'

# Verify Cloud Run can access secret
gcloud run services describe memql-service --region us-west1 --format="value(spec.template.spec.containers[0].env)"
```

---

## INFO Monitoring

### Tiger Cloud Dashboard

- **URL**: https://console.cloud.timescale.com/
- **Metrics**: CPU, memory, storage, connections
- **Alerts**: Set up alerts for high usage

### Query Logs

```bash
# View slow queries
tiger service logs --service-id xdejfkq0s1 | grep "duration:"
```

### Connection Pooling

Consider adding connection pooling for production:
- **PgBouncer** via Tiger Cloud
- Or application-level pooling

---

## Cost Management

### Current Setup

- **Staging**: 1 CPU, 4GB RAM
- **Production**: 1 CPU, 4GB RAM
- **Region**: us-east-1 (both)

### Optimization

```bash
# Scale down staging when not in use
tiger service update --service-id rt1dn6vj9g --cpu 500 --memory 2

# Scale up production if needed
tiger service update --service-id xdejfkq0s1 --cpu 2000 --memory 8
```

---

## [REFRESH] Migration from Shared to Separate Databases

**Completed**: February 9, 2026

### What Changed

**Before:**
- Single database used by both staging and production
- Risk of data conflicts and corruption
- No clear separation

**After:**
- **Staging**: `db-99414` (rt1dn6vj9g)
- **Production**: `memql-production` (xdejfkq0s1)
- Complete isolation
- Safe to test in staging

### Data Migration

WARNING: **Note**: The new production database starts empty. If you need to migrate data from staging:

```bash
# Export from staging
pg_dump "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_LAB')" > staging_backup.sql

# Import to production
psql "$(gcloud secrets versions access latest --secret='MEMORY_NODES_DATABASE_DSN_PROD')" < staging_backup.sql
```

**Or** let migrations rebuild the schema and start fresh in production.

---

## [NOTE] See Also

- [DEPLOYMENT_STRATEGY.md](deployment-strategy.md) - Deployment procedures
- [Tiger Cloud CLI Docs](https://docs.timescale.com/use-timescale/latest/services/cli/)
- Tiger Cloud Dashboard: https://console.cloud.timescale.com/

---

**Important**: Always test database changes in staging before deploying to production!
