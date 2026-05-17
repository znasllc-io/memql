# memQL

**Time-series memory graph database with MemQL DSL query language**

---

## START Quick Start

```bash
# Start development environment (Docker)
docker compose -f docker/docker-compose.full.yml up --build

# Run tests
go test ./...

# Deploy to staging (Google Cloud Run)
gcloud run deploy
```

**Full setup guide:** [QUICKSTART.md](QUICKSTART.md)

---

## DOCS Documentation

- **[CLAUDE.md](CLAUDE.md)** - Project overview and architecture
- **[QUICKSTART.md](QUICKSTART.md)** - 5-minute setup guide
- **[GLOSSARY.md](GLOSSARY.md)** - Complete documentation index
- **[TECH_STACK_AND_PRACTICES.md](TECH_STACK_AND_PRACTICES.md)** - Tech stack and deployment practices

---

## [BUILD] Tech Stack

### Backend
- **Language:** Go 1.26.1+
- **Database:** PostgreSQL 16 + TimescaleDB
- **API:** gRPC (primary) + WebSocket bridge for browsers + HTTP for OAuth callbacks / health / file uploads
- **SI:** Centralized provider system (OpenAI, Anthropic) on `MemqlService.Stream`
- **Auth:** in-house identity service (magic-link + JWT, JWKS-published)

### Query Language
- **MemQL DSL:** Custom query language for time-series graphs
- **Automations:** Event-driven workflows (.memql files)
- **Functions:** Reusable query functions (.memql files)

---

## Environments

### Development (Docker)
- **Database:** Local PostgreSQL + TimescaleDB container
- **Service:** Local memQL container
- **Access:** All developers
- **Command:** `docker compose -f docker/docker-compose.full.yml up --build`

### Staging (Cloud)
- **Database:** TimescaleDB Cloud (Tiger Cloud)
- **Service:** Google Cloud Run (us-central1)
- **Access:** All developers
- **Command:** `gcloud run deploy`

### Production (Cloud)
- **Database:** TimescaleDB Cloud (Tiger Cloud) - separate instance
- **Service:** Google Cloud Run (production)
- **Access:** Senior/Lead developers only
- **Deploy:** Automatic via CI/CD on merge to `main`

**Full details:** [TECH_STACK_AND_PRACTICES.md](TECH_STACK_AND_PRACTICES.md)

---

## TOOLS Development

### Prerequisites

**Hardware:**
- macOS with Apple Silicon (M1/M2/M3)
- MacBook Pro or MacBook Air
- 16GB RAM minimum (32GB recommended)

**Software:**
- Go 1.26.1+ (ARM64 build)
- Docker Desktop for Mac (Apple Silicon)
- gcloud CLI
- git

### Local Development Workflow

1. **Clone repository**
   ```bash
   git clone https://github.com/znasllc-io/memql.git
   cd memql
   ```

2. **Start development environment**
   ```bash
   docker compose -f docker/docker-compose.full.yml up --build
   ```

3. **Make changes and test**
   ```bash
   # Edit code
   # ...

   # Run tests
   go test ./...

   # View logs
   docker compose -f docker/docker-compose.full.yml logs -f
   ```

4. **Deploy to staging for integration testing**
   ```bash
   gcloud run deploy
   ```

5. **Commit to `main`** (focused commits) or open a feature branch + PR
   when review is genuinely useful. Stage by explicit path:
   ```bash
   git add path/to/changed.file
   git commit -m "domain: imperative subject"
   git push origin main
   ```

---

## Project Structure

```
memQL/
├── main.go                 # Entry point (thin orchestrator)
├── app/                    # Phased service bootstrap
│   ├── app.go             # Build() orchestrator
│   ├── config.go          # Config + auth
│   ├── database.go        # Database + concepts
│   ├── engine.go          # Engine + bus + automations
│   ├── integrations.go    # Integration providers
│   ├── transport.go       # gRPC + HTTP + WebSocket
│   ├── cluster.go         # Distributed node bootstrap
│   └── adapters.go        # Engine adapter types
├── component/              # Go service components
│   ├── memql/             # Core query engine
│   ├── database/          # Database providers
│   ├── server/            # HTTP/WebSocket servers
│   └── auth/              # Authentication
├── integrations/          # External service integrations
│   ├── cognition/         # SI collaboration
│   └── audio/             # Audio streaming
├── automations/           # MemQL DSL automations (.memql)
├── queries/               # MemQL DSL query functions (.memql)
├── mutations/             # MemQL DSL mutation functions (.memql)
├── specs/                 # MemQL DSL specification predicates (.memql)
├── tools/                 # MemQL DSL SI tool definitions (.memql)
├── docs/                  # Documentation
│   ├── core/              # Architecture, language
│   ├── api/               # API references
│   ├── guides/            # How-to guides
│   └── auth/              # Authentication
├── docker/                # Docker configuration
└── .claude/               # Configuration
```

---

## Common Commands

| Task | Command |
|------|---------|
| **Start Docker stack** | `docker compose -f docker/docker-compose.full.yml up --build` |
| **Stop Docker services** | `docker compose -f docker/docker-compose.full.yml down` |
| **Run Go test suite** | `go test ./...` |
| **Deploy to staging** | `gcloud run deploy` |
| **View container logs** | `docker compose -f docker/docker-compose.full.yml logs -f` |
| **Database shell** | `psql postgres://memql:memql_dev@localhost:5432/memql` |

---

## Authentication

Every environment authenticates against the in-house **identity
service** (`component/identity`):
- Magic-link sign-in (no passwords)
- OAuth-style code exchange for SPAs (`/oauth/token`)
- JWKS-published EdDSA signing keys (`/.well-known/jwks.json`)
- Role-based access control (RBAC) per `v1:identity:user.role`
- Centralized user / partition-access management at `/admin/`

**Developer access:**
- **Development:** All developers (own machine)
- **Staging:** All developers (shared testing)
- **Production:** Senior/Lead developers only (live system)

---

## Testing

```bash
# Run all tests
go test ./...

# Run specific package tests
go test -v ./component/memql/...

# Run with coverage
go test -cover ./...
```

---

## Docker

### Development Stack

Full stack with PostgreSQL + TimescaleDB + memQL service:

```bash
# Start everything
docker compose -f docker/docker-compose.full.yml up --build

# View logs
docker compose -f docker/docker-compose.full.yml logs -f

# Access database
psql postgres://memql:memql_dev@localhost:5432/memql

# Stop (preserves data)
docker compose -f docker/docker-compose.full.yml down
```

**Documentation:** [docker/README.md](docker/README.md)

---

## [DOCS] MemQL Language

MemQL DSL is a domain-specific query language for time-series memory graphs.

### Example Query
```memql
// Find recent utterances in a space
spaceUtterances({
  "spaceId": "space_123",
  "limit": 10
})
```

### Example Automation
```memql
@enabled
@trigger(event="graph.node.created.v1:cognition:participant")
@description("Auto-join SI agents to spaces")
func (Automation) autoJoinSI() {
  // Automation logic...
}
```

**Full reference:** [docs/core/memql.md](docs/core/memql.md)

---

## Deployment

### Staging Environment
```bash
# Deploy latest code to staging (Google Cloud Run)
gcloud run deploy

# View logs
gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=memql-anequim" --limit 50
```

### Production Environment
- Automatic deployment via CI/CD when code is merged to `main` branch
- Manual deployment requires production access permissions
- Always test in staging before deploying to production

---

## Contributing

1. Read [TECH_STACK_AND_PRACTICES.md](TECH_STACK_AND_PRACTICES.md)
2. Make changes and test in development environment (`go test ./...`)
3. Deploy to staging for integration testing
4. Commit directly to `main` for focused changes, or open a PR when review is useful
5. Stage files by explicit path (`git add <file>`)

**Git workflow:** Single long-lived `main` branch. Pre-release: no
backwards-compat shims; fix both memQL and the consumer at once.

---

## License

Proprietary - Znasllc.io

---

## [HELP] Need Help?

1. **Quick start:** [QUICKSTART.md](QUICKSTART.md)
2. **Find documentation:** [GLOSSARY.md](GLOSSARY.md)
3. **Tech stack details:** [TECH_STACK_AND_PRACTICES.md](TECH_STACK_AND_PRACTICES.md)
4. **Component docs:** Check directory `CLAUDE.md` files
5. **Issues:** Create GitHub issue

---

**memQL - Time-series memory graph database for SI-powered collaboration**
