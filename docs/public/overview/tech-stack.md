---
title: memQL Tech Stack & Deployment Practices
audience: public
status: stable
area: overview
sinceVersion: 0.9.0
owner: znas
---

# memQL Tech Stack & Deployment Practices

**Version:** 1.0
**Date:** February 8, 2026
**Audience:** Backend and Frontend Development Teams

---

## [TASKS] Purpose

This document establishes the **opinionated technologies and practices** for memQL development and deployment. These standards ensure consistency, effectiveness, and clear separation of environments across all teams.

---

## [BUILD] Technology Stack

### Backend

| Component | Technology | Version | Purpose |
|-----------|-----------|---------|---------|
| **Language** | Go | 1.26.1+ | Primary backend language |
| **Database** | PostgreSQL + TimescaleDB | 16 + latest | Time-series memory graph |
| **API** | HTTP + gRPC | - | REST and real-time communication |
| **WebSocket** | Native Go | - | Real-time collaboration |
| **SI** | Multi-provider (OpenAI, Anthropic) | latest | All SI text/chat/vision/speech goes through gRPC on `MemqlService.Stream` (`AiChatMsg`, `AiSpeechMsg`, `AiTranscribeMsg`, `AiSuggestMsg`). The legacy SI HTTP path is gone. |
| **Auth** | In-house identity service | - | Magic-link login, JWT, JWKS-published; PAT for CLI |
| **Container** | Docker | latest | Local development |
| **Orchestration** | Docker Compose | 3.8+ | Multi-container management |
| **Coding Agent** | NemoClaw (NVIDIA) | latest | Enterprise coding/automation agent (Apache 2.0) |

### Query Language

| Component | Technology | Purpose |
|-----------|-----------|---------|
| **MemQL DSL** | Custom language | Query language for time-series graphs |
| **Automations** | MemQL DSL | Event-driven workflows |
| **Functions** | MemQL DSL | Reusable query functions |

### Frontend (for reference)

| Component | Technology | Notes |
|-----------|-----------|-------|
| **Framework** | TBD | Coordinate with frontend team |
| **API Client** | HTTP + WebSocket | Connect to memQL service |
| **Auth** | In-house identity service (magic-link JWT) | Same auth provider as backend |

---

## Environment Architecture

### 1. Development Environment

**Purpose:** Isolated development and testing on developer machines

| Component | Technology | Location |
|-----------|-----------|----------|
| **Database** | PostgreSQL + TimescaleDB | Docker container (local) |
| **Service** | memQL | Docker container (local) |
| **Ports** | 5432, 8088 (BFF HTTP), 50051 (BFF gRPC), 50059 (Voice), 18789 (NemoClaw) | localhost |
| **Data** | Ephemeral | Docker volumes (can be reset) |

**Commands:**
```bash
# Start development environment
docker compose -f docker/docker-compose.full.yml up --build

# Stop (preserves data)
docker compose -f docker/docker-compose.full.yml down

# View logs
docker compose -f docker/docker-compose.full.yml logs -f

# Access database
psql postgres://memql:memql_dev@localhost:5432/memql
```

**Developer Access:** [x] All developers

---

### 2. Staging Environment

**Purpose:** Shared testing and staging environment for integration testing

| Component | Technology | Location |
|-----------|-----------|----------|
| **Database** | TimescaleDB Cloud (Tiger Cloud) | Managed Tiger Cloud instance |
| **Service** | memQL | Azure Kubernetes Service (cluster `aks-memql-staging`, namespace `memql`) |
| **URL** | HTTPS | https://app.staging.copresent.ai, https://identity.staging.copresent.ai |
| **Data** | Persistent | Managed by Tiger Cloud |

**Commands:**
```bash
# Deploy to staging (Azure AKS)
make deploy VERSION=X

# View logs
kubectl logs -n memql deployment/bff -f
```

See [DEPLOYMENT_STRATEGY.md](../operate/deployment-strategy.md) for the full deploy
flow and topology.

**Developer Access:** [x] All developers

---

### 3. Production Environment

**Purpose:** Live production system serving real users

| Component | Technology | Location |
|-----------|-----------|----------|
| **Database** | TimescaleDB Cloud (Tiger Cloud) | Production instance (separate from staging) |
| **Service** | memQL | Azure Kubernetes Service (production) |
| **URL** | HTTPS | Production domain |
| **Data** | Persistent | Managed by Tiger Cloud (with backups) |

**Deployment:**
- Automatic via CI/CD pipeline when code is merged to `main` branch
- Manual deploys require production access permissions

**Developer Access:** WARNING: Limited - only developers with production permissions

---

## AUTH Authentication & Authorization

### In-house identity service

All environments use the in-house identity service
(`component/identity`, `make identity`) for authentication:
- Magic-link login as the primary path
- Personal Access Tokens (PATs) for CLI clients
- Per-node JWT verifier (`component/identity/verifier`) on
  bff/voice/cognition/agent/planner; JWKS-published
- Role-based access control (owner / admin / writer / reader)
- Centralized user + invitation management via the admin web app
  at `/admin/*`

See [docs/public/operate/auth/identity-service.md](../operate/auth/identity-service.md)
for the operator-side narrative.

### Developer Access Levels

| Environment | Access Level | Permissions |
|-------------|-------------|-------------|
| **Development** | All Developers | Full access (own machine) |
| **Staging** | All Developers | Deploy, view logs, test |
| **Production** | Senior/Lead Developers | Deploy, configure, manage |

### Service Accounts

- AKS pulls images from ACR (`acrmemql.azurecr.io`)
- Secrets managed via the genesis A2 sealed envelope + Azure Key Vault
  (`kv-memql-<env>`); see DEPLOYMENT_STRATEGY.md
- Environment variables injected at runtime

---

## Deployment Practices

### Development Workflow

**Workflow:**
1. Pull latest from `main`
2. Start development Docker environment: `docker compose -f docker/docker-compose.full.yml up --build`
3. Make code changes
4. Test locally: `go test ./...`
5. View logs: `docker compose -f docker/docker-compose.full.yml logs -f`
6. Commit (directly to `main` for focused changes; feature branch + PR when review is useful)

**Best Practices:**
- Always use development Docker database (not staging database)
- Reset development database if migrations conflict: stop services, remove Docker volumes, then restart
- Use debug logging (enabled by default in Docker)
- Test automations and functions in development before deploying
- Generate `.env.local` (master key + bootstrap envelope) with `make bootstrap`, populate memQL config via `make secrets-init` + `make secrets-seed`

---

### Staging Deployment

**Workflow:**
1. Ensure all tests pass: `go test ./...`
2. Push feature branch to GitHub
3. Deploy to staging: `make deploy VERSION=X`
4. Verify deployment via logs and health endpoint
5. Test integration with frontend (if applicable)
6. Create pull request when ready

**Best Practices:**
- Always test in development before deploying to staging
- Staging shares database - be careful with schema changes
- Coordinate with team if making breaking changes
- Use staging for integration testing, not development

---

### Production Deployment

**Workflow:**
1. Feature branch reviewed and approved via PR
2. Merge PR to `main` branch
3. CI/CD pipeline automatically deploys to production
4. Monitor deployment logs for errors
5. Verify production health endpoint
6. Monitor for 24 hours after deployment

**Best Practices:**
- Never deploy untested code to production
- Always run full test suite before merging
- Schema migrations run automatically (use caution)
- Coordinate with team for major changes
- Have rollback plan ready
- Test thoroughly in staging before production deployment

---

## CONFIG Development Tools

### Hardware Requirements

**Standardized Platform:** macOS with Apple Silicon

| Component | Requirement | Notes |
|-----------|------------|-------|
| **Operating System** | macOS | Monterey (12.0) or later |
| **Processor** | Apple Silicon (M1/M2/M3) | ARM64 architecture |
| **Devices** | MacBook Pro or MacBook Air | With M-series chips |
| **RAM** | 16GB minimum, 32GB recommended | For Docker + IDE + services |
| **Storage** | 50GB free minimum | For Docker images, dependencies |

**Why Apple Silicon?**
- Standardized development environment across team
- Native ARM64 Docker performance
- Consistent tooling and behavior
- Superior battery life for remote work

### Required Software

| Tool | Purpose | Installation |
|------|---------|--------------|
| **Go 1.26.1+** | Backend development | https://go.dev/dl/ (ARM64 build) |
| **Docker Desktop** | Local environment | https://docker.com/products/docker-desktop (Apple Silicon) |
| **Docker Compose** | Local container orchestration | Pre-installed with Docker Desktop |
| **Azure CLI (`az`)** | Cloud deployments (AKS, ACR) | https://learn.microsoft.com/cli/azure/install-azure-cli |
| **kubectl** | AKS cluster management | https://kubernetes.io/docs/tasks/tools/ |
| **git** | Version control | Pre-installed on macOS |

### Optional Tools

| Tool | Purpose | Installation |
|------|---------|--------------|
| **pgAdmin** | Database GUI | `docker-compose --profile tools up -d pgadmin` |
| **Tiger CLI** | Database management | `./scripts/tiger-setup.sh` |
| **Postman** | API testing | https://postman.com/downloads/ |

---

## DOCS Documentation Standards

### Documentation Structure

```
memQL/
├── CLAUDE.md              # Project overview (read first)
├── docs/public/overview/quickstart.md          # 5-minute setup guide
├── GLOSSARY.md            # Complete doc index
├── docs/
│   ├── core/              # Architecture, language
│   ├── api/               # API references
│   ├── guides/            # How-to guides
│   ├── auth/              # Authentication
│   └── planning/          # Historical planning
└── .claude/               # Claude Code CLI configuration
```

### Finding Documentation

1. **Project overview:** Start with `CLAUDE.md`
2. **Quick setup:** Read `docs/public/overview/quickstart.md`
3. **Find topics:** Use `GLOSSARY.md`
4. **Component details:** Check directory `CLAUDE.md` files
5. **Commands:** See [docs/public/overview/quickstart.md](quickstart.md) for common development commands

---

## START Quick Reference

### Common Commands

```bash
# Development Environment
docker compose -f docker/docker-compose.full.yml up --build    # Start Docker stack
docker compose -f docker/docker-compose.full.yml down           # Stop containers
docker compose -f docker/docker-compose.full.yml logs -f        # View logs
psql postgres://memql:memql_dev@localhost:5432/memql            # PostgreSQL shell

# Testing
go test ./...                    # Run Go test suite

# Deployment (Azure AKS) -- see DEPLOYMENT_STRATEGY.md
make deploy VERSION=X            # Deploy to staging (scripts/deploy/aks-deploy.sh)

# Dev secrets workflow
make bootstrap                                   # Generate .env.local with master key + bootstrap envelope
make secrets-init                                # Interactively populate ~/.memql/dev-secrets.yaml from manifest
make secrets-seed                                # Push the yaml into running memQL (encrypts secrets first)
make secrets-list                                # Diff manifest vs yaml vs running memQL

# AKS
kubectl get pods -n memql                                   # List pods
kubectl logs -n memql deployment/bff -f                     # View logs
kubectl get deployments -n memql                            # Deployment status (staging)
```

### Environment Variables

**Managed via memQL concept storage** (`v1:platform:globalVariable` and
`v1:platform:globalSecret`). Operators populate via Make targets backed by
the dev-secrets workflow; see [docs/public/operate/env-vars.md](../operate/env-vars.md)
for the full design.

**Development (Docker):**
- Generate `.env.local` with `make bootstrap` (writes master key + bootstrap envelope; idempotent)
- Populate `~/.memql/dev-secrets.yaml` interactively with `make secrets-init`
- Push to memQL with `make secrets-seed`
- Override with `.env.local` file (git-ignored) for the bootstrap envelope only
- Debug logging enabled by default

**Partition Configuration:**
- No env var. Partition defaults to `"default"` and is set per-request via
  the gRPC envelope (`MemqlClientMessage.partition`).
- Auto-injected into every query / mutation / event topic by the engine.
- The CLI manages partitions interactively in the Clusters tab and persists
  the per-cluster selection in `~/.memql/clusters.yaml`.

**Staging/Production (AKS):**
- Shared secrets (DSN, master key, identity signing seed) ride the
  genesis A2 sealed envelope (`MEMQL_GENESIS_B64` in the `memql-secrets`
  Secret), backed up in Azure Key Vault (`kv-memql-<env>`). Per-node,
  non-secret config lives in the k8s manifest env. See
  DEPLOYMENT_STRATEGY.md for the canonical add/rotate flow.
- Everything else lives in memQL's `v1:platform:globalSecret` /
  `v1:platform:globalVariable` concepts
- Never commit secrets to git

---

## Opinionated Practices

### Code Standards

1. **Go formatting:** Use `gofmt` and `goimports`
2. **Testing:** Write tests for all new features
3. **Error handling:** Always handle errors explicitly
4. **Logging:** Use structured logging (slog)
5. **Comments:** Document why, not what

### Git Workflow

1. **Single long-lived branch:** `main`. Commit directly when PR review
   isn't useful; use a short-lived feature branch only when it is.
2. **Stage by explicit path** (`git add <file>`) -- never `git add -A` /
   `.`. Multiple Claude sessions may share a worktree.
3. **Pre-release; no backwards-compat shims.** When a contract changes,
   fix both memQL and the consumer (typically CoPresent) at once and
   delete what's no longer needed.
4. **Commit messages:** Clear, imperative mood. Subject under ~70
   chars. Body explains the why.
5. **Co-authoring:** Include SI contributions
   (`Co-Authored-By: Claude ... <noreply@anthropic.com>`).

### Docker Practices

1. **Development isolation:** Always use development Docker for local work
2. **Volume mounts:** Use for live code changes
3. **Health checks:** All containers must have health checks
4. **Multi-stage builds:** Optimize image sizes
5. **Non-root users:** Security best practice
6. **Environment variables:** Generate `.env.local` with `make bootstrap`, then populate memQL config via `make secrets-init` + `make secrets-seed`

### Database Practices

1. **Migrations:** Automatic on startup (use carefully in production)
2. **Seeding:** Use concept seeding for test data
3. **Backups:** Managed by Tiger Cloud (production and staging)
4. **Reset:** `docker compose -f docker/docker-compose.full.yml down -v` then restart for fresh development database
5. **Schema changes:** Coordinate with team

---

## Team Coordination

### Before Major Changes

Notify team if changes affect:
- Database schema
- API contracts
- Authentication flow
- Environment variables
- Deployment process

### Communication Channels

- **Code reviews:** GitHub pull requests
- **Questions:** Team chat or documentation
- **Issues:** GitHub issues
- **Architecture:** Architecture decision records (ADRs)

---

## INFO Success Metrics

### Development Velocity

- Local setup time: < 5 minutes
- Test execution time: < 2 minutes
- Deploy to staging time: < 5 minutes
- Deploy to production time: < 10 minutes

### Code Quality

- Test coverage: > 70%
- Build success rate: > 95%
- Production incidents: Minimize
- Documentation completeness: 100%

---

## [REFRESH] Continuous Improvement

This document is a living standard. Update it when:
- New technologies are adopted
- Practices are refined based on experience
- Team feedback suggests improvements
- Industry best practices evolve

**Last Updated:** April 29, 2026

---

## Support & Questions

- **Documentation:** Check `GLOSSARY.md` first
- **Quick start:** See `docs/public/overview/quickstart.md`
- **Team:** Ask in team chat or create GitHub issue

---

**This document establishes our opinionated tech stack and practices for effective, standardized development across all teams.**
