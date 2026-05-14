# Infrastructure Management Guide

## Overview

This document describes the infrastructure setup and management tools for memQL.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     ENVIRONMENTS                             │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  DEVELOPMENT              STAGING               PRODUCTION   │
│  ───────────              ───────               ──────────   │
│  Docker Compose           Google Cloud Run      Google Cloud Run │
│  PostgreSQL+TimescaleDB   + Tiger Cloud                     │
│  localhost:5432           anequim-memql-staging               │
│                           (Cloud Run)                        │
│                           + postgres DB                       │
│                           (Tiger Cloud)                      │
└─────────────────────────────────────────────────────────────┘
```

## Environment Details

### Development Environment
- **Application**: Docker container (memQL service)
- **Database**: Docker container with TimescaleDB
- **Ports**: 5432 (PostgreSQL), 8088 (BFF HTTP -- behind nginx 8080 LB in compose), 50051 (BFF gRPC -- behind nginx 50050 LB), 50059 (Voice gRPC), 18789 (NemoClaw, optional)
- **NemoClaw**: Optional Docker overlay (`--nemoclaw` flag), shared instance with per-agent workspaces
- **Management**: `docker compose -f docker/docker-compose.full.yml up --build`

### Staging Environment
- **Application**: Google Cloud Run (`anequim-memql-staging`)
- **Database**: Tiger Cloud (managed TimescaleDB)
- **Region**: us-central1 (Google Cloud Run)
- **Database Host**: `rt1dn6vj9g.wb2g0uu9oq.tsdb.cloud.timescale.com`
- **NemoClaw**: Optional Cloud Run sidecar (`--nemoclaw` flag), multi-container deployment via `cloudbuild.nemoclaw.yaml`

### Production Environment
- **Application**: Google Cloud Run (`anequim-memql-production`)
- **Database**: Tiger Cloud (separate production instance)
- **Region**: us-west1 (Google Cloud Run)
- **NemoClaw**: Optional Cloud Run sidecar (`--nemoclaw` flag)

### Cluster Mode (Distributed Nodes)

memQL supports running as a distributed cluster of specialized nodes. Each node type
runs as a separate container with only the components relevant to its purpose.

**Local Development (Docker Compose):**
```bash
docker-compose -f docker/docker-compose.cluster.yml up -d
```

Starts: BFF, Cognition, Agent, Planner nodes + shared PostgreSQL.

**Cloud Deployment (per-node Cloud Run services):**

Service configs are in `infra/cluster/`:
- `service.bff.yaml` -- Backend for frontend (always-on CPU, min 1 instance)
- `service.cognition.yaml` -- Voice/conversation (always-on CPU, min 1 instance)
- `service.agent.yaml` -- Task execution (scales to zero, max 20)
- `service.planner.yaml` -- Planning (scales to zero, max 5)

Long-lived nodes (BFF, Cognition) require always-on CPU for persistent gRPC streams.
Ephemeral nodes (Agent, Planner) can scale to zero when idle.

All nodes share a single PostgreSQL + TimescaleDB database and communicate via the
`NodeService` gRPC bidirectional stream.

See [component/node/CLAUDE.md](component/node/CLAUDE.md) for architecture details.

---

## Infrastructure as Code (IaC) Options

### Recommended: Terraform (Open Source)

**Why Terraform:**
- [x] Open source (Mozilla Public License 2.0)
- [x] Supports Google Cloud, Timescale Cloud, and 1000+ providers
- [x] Industry standard, huge community
- [x] Single source of truth for all infrastructure
- [x] State management built-in
- [x] Free for all features

**Providers Needed:**
- `google` - Google Cloud Run, Cloud Build, etc.
- `timescale` - Timescale Cloud database management (if available)
- `postgresql` - Database schema management

**Example Terraform Structure:**
```
terraform/
├── main.tf                 # Main configuration
├── variables.tf            # Variables
├── outputs.tf              # Outputs
├── environments/
│   ├── local/             # Local dev overrides
│   ├── staging/           # Staging environment
│   └── production/        # Production (future)
├── modules/
│   ├── cloud-run/         # Cloud Run service
│   ├── database/          # Database configuration
│   └── secrets/           # Secret management
└── terraform.tfvars       # Variable values (gitignored)
```

### Alternative: Pulumi (Open Source)

**Why Pulumi:**
- [x] Open source (Apache 2.0)
- [x] Use real programming languages (Go, TypeScript, Python)
- [x] Type safety and IDE support
- [x] Supports all major cloud providers

**Structure:**
```
pulumi/
├── index.ts               # Main program
├── Pulumi.yaml            # Project config
├── Pulumi.local.yaml      # Local stack
└── Pulumi.staging.yaml    # Staging stack
```

### Alternative: Ansible (Open Source)

**Why Ansible:**
- [x] Open source (GPL)
- [x] Agentless, uses SSH
- [x] Good for configuration management
- [ ] Less ideal for cloud infrastructure provisioning

---

## Timescale Cloud Management

### Tiger CLI (Timescale Cloud Rebranded 2026)

**Note:** Timescale Cloud rebranded to "Tiger Cloud" in 2026. The CLI is now called `tiger`.

**Installation:**
```bash
# macOS/Linux/WSL
curl -fsSL https://cli.tigerdata.com | sh

# Or via Homebrew (macOS)
brew install --cask timescale/tap/tiger-cli

# Or via Go
go install github.com/timescale/tiger-cli/cmd/tiger@latest

# Quick setup script
./scripts/tiger-setup.sh
```

**Authentication:**
```bash
# Login to Tiger Cloud
tiger login

# Check auth status
tiger auth status

# Logout
tiger logout
```

**Service Management:**
```bash
# List all services
tiger service list

# Get service details
tiger service info <service-id>

# View logs
tiger service logs <service-id>

# Create new service
tiger service create \
  --name memql-dev \
  --region us-east-1 \
  --plan dev

# Start/stop service
tiger service start <service-id>
tiger service stop <service-id>

# Resize service
tiger service resize <service-id>

# Delete service
tiger service delete <service-id>
```

**Database Operations:**
```bash
# Get connection string
tiger connection-string <service-id>

# Connect with psql
tiger connect <service-id>

# Test connection
tiger test-connection <service-id>
```

**Configuration:**
```bash
# View config
tiger config

# Set output format (json, yaml, table)
tiger config set output json

# List services as JSON
tiger service list --output json
```

### Timescale Cloud Terraform Provider

Unfortunately, Timescale doesn't have an official Terraform provider yet. Workarounds:

1. **Use Terraform + Manual Timescale Setup**
   - Provision Cloud Run with Terraform
   - Store Timescale connection string as Terraform variable
   - Manage Timescale via their web console/CLI

2. **Use `null_resource` with Timescale CLI**
   ```hcl
   resource "null_resource" "timescale_db" {
     provisioner "local-exec" {
       command = "tscloud service create ..."
     }
   }
   ```

3. **Use `postgresql` Provider for Schema Management**
   ```hcl
   provider "postgresql" {
     host     = var.timescale_host
     username = var.timescale_user
     password = var.timescale_password
   }

   resource "postgresql_database" "memql" {
     name = "memql"
   }
   ```

---

## Recommended Setup

### Phase 1: Local Development [x] (Current)
- [x] Docker Compose for local PostgreSQL + TimescaleDB
- [x] Helper scripts for database management
- [x] Separate `.env.local` for local config

### Phase 2: Terraform for Infrastructure
```bash
terraform/
├── main.tf
├── google-cloud-run.tf    # Cloud Run service
├── timescale.tf           # Timescale connection config
├── secrets.tf             # Secret Manager
└── variables.tf
```

### Phase 3: CI/CD Integration
```bash
.github/workflows/
├── deploy-staging.yml     # Deploy to staging
└── terraform-plan.yml     # Terraform validation
```

---

## Migration Strategy

### Current State
- [x] Local: Docker PostgreSQL + TimescaleDB
- [x] Staging: Google Cloud Run + Tiger Cloud
- [ ] No IaC - manual management

### Target State
- [x] Local: Docker (unchanged)
- [x] Staging: Terraform-managed
- [x] Production: Terraform-managed (when ready)
- [x] Single source of truth in Git

### Steps
1. Document current staging infrastructure
2. Write Terraform to recreate staging
3. Test Terraform apply (dry-run)
4. Migrate secrets to Terraform
5. Add CI/CD for Terraform changes

---

## Tools Summary

| Tool | Use Case | Cost | License |
|------|----------|------|---------|
| **Docker Compose** | Local dev database | Free | Apache 2.0 |
| **Terraform** | Infrastructure as Code | Free | MPL 2.0 |
| **Timescale CLI** | Database management | Free | Proprietary |
| **Google Cloud SDK** | GCP management | Free | Apache 2.0 |
| **Pulumi** | Alternative to Terraform | Free | Apache 2.0 |

---

## Next Steps

1. [x] Set up local Docker database
2. [x] Create environment separation (.env vs .env.local)
3. [PENDING] Install Timescale CLI
4. [PENDING] Document staging infrastructure in Terraform
5. [PENDING] Set up Terraform state backend (Google Cloud Storage)
6. [PENDING] Migrate staging to Terraform management

---

## Useful Commands

### Local Docker Database
```bash
# Start
./scripts/docker-dev.sh start

# Stop
./scripts/docker-dev.sh stop

# Connect
./scripts/docker-dev.sh psql

# Reset (delete all data)
./scripts/docker-dev.sh reset
```

### Run memQL Locally
```bash
# With local Docker database
./scripts/run-local.sh --local-db

# With staging database (Tiger Cloud)
./scripts/run-local.sh
```

### Timescale Cloud
```bash
# Login
tscloud login

# List services
tscloud services list

# Get connection string
tscloud service connection <service-id>
```

### Google Cloud
```bash
# List Cloud Run services
gcloud run services list

# Deploy
gcloud builds submit --config cloudbuild.yaml

# View logs
gcloud logging read "resource.type=cloud_run_revision"
```
