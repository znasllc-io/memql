# Docker Full Stack Setup

Complete memQL environment in Docker (database + service).

---

## START Quick Start

### Start Everything
```bash
# From project root
docker-compose -f docker/docker-compose.full.yml up -d
```

### Stop Everything
```bash
docker-compose -f docker/docker-compose.full.yml down
```

### Reset (Delete All Data)
```bash
docker-compose -f docker/docker-compose.full.yml down -v
```

---

## What's Included

| Service | Container | Port | Purpose |
|---------|-----------|------|---------|
| **PostgreSQL + TimescaleDB** | `memql-db` | 5432 | Database |
| **Nginx load balancer** | `memql-lb` | 8085, 50050 | Single entry point for clients |
| **BFF node** | `memql-bff` | 8088 | Backend-for-frontend / API surface |
| **Cognition node** | `memql-cognition` | 8086 | Conversation intelligence |
| **Voice node** | `memql-voice` | 8085 (in-cluster) | ASR/TTS pipeline |
| **Agent node** | `memql-agent` | 8089 | Task execution / SI work |
| **Planner node** | `memql-planner` | 8087 | Task planning + orchestration |
| **Identity node** | `memql-identity` | 8081 | Magic-link auth, JWKS, admin UI |
| **pgAdmin** | `memql-dbadmin` | 5050 | DB Management (optional, `--profile tools`) |

The compose project is named `memql-cluster` (full topology) or
`memql-cluster-multinode` (cluster.yml topology) so the stack is
identifiable at a glance via `docker compose ls`.

---

## CONFIG Configuration

### Environment Variables

Create `.env.docker` in project root with secrets:

```bash
# SI
MEMQL_SI_OPENAI_API_KEY=sk-...

# Identity service (per-node verifier)
IDENTITY_VERIFIER_BASE_URL=http://identity:8081
IDENTITY_VERIFIER_EXPECTED_ISSUER=http://localhost:8081

# Optional: Discord, etc.
```

### Load Environment
```bash
# Export all vars
export $(grep -v '^#' .env.docker | xargs)

# Then start
docker-compose -f docker/docker-compose.full.yml up -d
```

---

## CHECK Verification

### Check Services
```bash
docker-compose -f docker/docker-compose.full.yml ps
```

### Check Logs
```bash
# All services
docker-compose -f docker/docker-compose.full.yml logs -f

# Just memQL
docker-compose -f docker/docker-compose.full.yml logs -f memql

# Just database
docker-compose -f docker/docker-compose.full.yml logs -f postgres
```

### Test HTTP API
```bash
curl http://localhost:8088/healthz
```

### Test Database
```bash
psql postgres://memql:memql_dev@localhost:5432/memql -c "SELECT version();"
```

---

## TOOLS Development Workflow

### Make Code Changes

1. **Edit code** in your IDE
2. **Rebuild service:**
   ```bash
   docker-compose -f docker/docker-compose.full.yml up -d --build memql
   ```
3. **Watch logs:**
   ```bash
   docker-compose -f docker/docker-compose.full.yml logs -f memql
   ```

### Hot Reload (Alternative)

Mount source code as volume (for development):
```yaml
# Add to docker-compose.full.yml memql service:
volumes:
  - ..:/app:ro  # Mount entire project (read-only)
```

Then use `air` or `CompileDaemon` for hot reload.

---

## INFO Database Management

### Connect with psql
```bash
docker-compose -f docker/docker-compose.full.yml exec postgres psql -U memql -d memql
```

### Use pgAdmin
```bash
# Start pgAdmin
docker-compose -f docker/docker-compose.full.yml --profile tools up -d pgadmin

# Open: http://localhost:5050
# Email: admin@memql.local
# Password: admin
```

**Add Server in pgAdmin:**
- Host: `postgres` (Docker network name)
- Port: `5432`
- Database: `memql`
- Username: `memql`
- Password: `memql_dev`

---

## Troubleshooting

### Containers Won't Start

```bash
# Check Docker is running
docker ps

# View detailed logs
docker-compose -f docker/docker-compose.full.yml logs
```

### Port Conflicts

```bash
# Check what's using ports
lsof -i :8088  # memQL HTTP
lsof -i :5432  # PostgreSQL
lsof -i :50051 # memQL gRPC

# Stop conflicting services or change ports in docker-compose.full.yml
```

### Database Connection Errors

```bash
# Check database health
docker-compose -f docker/docker-compose.full.yml exec postgres pg_isready -U memql

# Check memQL can reach database
docker-compose -f docker/docker-compose.full.yml exec memql sh -c 'nc -zv postgres 5432'
```

### Migrations Failing

```bash
# Check migration logs
docker-compose -f docker/docker-compose.full.yml logs memql | grep migration

# Reset database
docker-compose -f docker/docker-compose.full.yml down -v
docker-compose -f docker/docker-compose.full.yml up -d
```

### Build Failures

```bash
# Clean build
docker-compose -f docker/docker-compose.full.yml build --no-cache memql

# Check Go modules
docker-compose -f docker/docker-compose.full.yml run --rm memql go mod download
```

---

## Security Notes

### Production Use

**DO NOT use this setup for production!** This is for local development only.

For production:
1. Use secrets management (not environment variables)
2. Enable SSL/TLS for database
3. Use strong passwords
4. Restrict network access
5. Run as non-root user (already configured)
6. Use read-only volumes where possible

---

## Performance

### Resource Limits

Add to `docker-compose.full.yml`:

```yaml
services:
  memql:
    deploy:
      resources:
        limits:
          cpus: '2'
          memory: 2G
        reservations:
          cpus: '1'
          memory: 1G
```

### Build Cache

Speed up builds:
```bash
# Use BuildKit
DOCKER_BUILDKIT=1 docker-compose -f docker/docker-compose.full.yml build
```

---

## Cleanup

### Remove Everything
```bash
# Stop and remove containers, networks
docker-compose -f docker/docker-compose.full.yml down

# Also remove volumes (deletes data!)
docker-compose -f docker/docker-compose.full.yml down -v

# Also remove images
docker-compose -f docker/docker-compose.full.yml down --rmi all -v
```

### Clean Docker System
```bash
# Remove unused containers, networks, images
docker system prune -a

# Remove unused volumes
docker volume prune
```

---

## Compose File Variants

| File | Purpose | Usage |
|------|---------|-------|
| `docker-compose.full.yml` | Standard single-node development | `docker-compose -f docker/docker-compose.full.yml up -d` |
| `docker-compose.polyphon.yml` | Overlay: adds LiveKit, Redis, Bridge Agent | `docker-compose -f full.yml -f polyphon.yml up -d` |
| `docker-compose.nemoclaw.yml` | Overlay: adds NemoClaw coding agent | `docker-compose -f full.yml -f nemoclaw.yml up -d` |
| `docker-compose.cluster.yml` | Multi-node cluster (bff, cognition, agent) | `docker-compose -f docker/docker-compose.cluster.yml up -d` |

Overlays are combined with the full stack: `docker-compose -f full.yml -f polyphon.yml up -d`

The cluster variant runs separate containers for each node type, all sharing one PostgreSQL database.
See [component/node/CLAUDE.md](../component/node/CLAUDE.md) for distributed architecture details.

---

## DOCS See Also

- [docs/public/overview/quickstart.md](../docs/public/overview/quickstart.md) - Quick start guide
- [Local Development](../docs/guides/local-development.md) - Development guide
- [CLAUDE.md](../CLAUDE.md) - Project overview

---

## TASKS Commands Reference

| Task | Command |
|------|---------|
| **Start** | `docker-compose -f docker/docker-compose.full.yml up -d` |
| **Stop** | `docker-compose -f docker/docker-compose.full.yml down` |
| **Logs** | `docker-compose -f docker/docker-compose.full.yml logs -f` |
| **Rebuild** | `docker-compose -f docker/docker-compose.full.yml up -d --build` |
| **Reset** | `docker-compose -f docker/docker-compose.full.yml down -v && up -d` |
| **psql** | `docker-compose -f docker/docker-compose.full.yml exec postgres psql -U memql` |
| **Status** | `docker-compose -f docker/docker-compose.full.yml ps` |
| **Exec Shell** | `docker-compose -f docker/docker-compose.full.yml exec memql sh` |

---

**Ready to develop!**
