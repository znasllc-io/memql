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

- **Docker** installed and running (k3d runs Kubernetes inside Docker)
- **k3d + kubectl** (`brew install k3d kubectl`)
- **Go 1.26.1+** (for building the node images locally)
- **GitHub Packages access** for the CoPresent SPA image build.
  The SPA image `npm install`s two private SDK packages
  (`@visionarys-io/copresent-sdk`, `@znasllc-io/memql-sdk-core`) from
  GitHub Packages, so the build needs a token:

  ```bash
  # a GitHub token with read:packages, SSO-authorized for BOTH the
  # znasllc-io and visionarys-io orgs. A classic PAT works, or reuse
  # your gh login if it has the scope:
  export MEMQL_PACKAGES_TOKEN=$(gh auth token)
  ```

  Export it before `make up` / `make dev` so the SPA image build can
  resolve those packages (the backend nodes come up regardless). Full
  details + how to mint the token: `docs/sdk-dependency.md` in the
  CoPresent repo.

---

## Quick Start

### 1. Start the local cluster

```bash
make up                  # k3d cluster + ArgoCD + local overlay + seeded secrets
make k3d-status          # parity litmus: distinct per-pod MEMQL_NODE_ID
```

`make up` creates a k3d cluster, installs ArgoCD, applies the local
overlay at `deploy/k8s/overlays/local`, and seeds the k8s Secrets from
the genesis envelope. The same k8s manifests and ArgoCD reconciliation
path run locally and on AKS staging, so local IS staging parity.

For full multi-node mesh testing (2 replicas per mesh node), bring the
cluster up multi-node and scale the Deployments:

```bash
make up SERVERS=2 AGENTS=1
make k3d-scale N=2
```

This brings up Postgres + TimescaleDB, the
bff/cognition/agent/planner/voice/workbench mesh nodes, identity, the
copresent SPA, and LiveKit -- each pod carrying a unique `MEMQL_NODE_ID`
via `fieldRef: metadata.name`, exactly as in staging. The 2-replica
topology is the only one that reproduces cluster-only bugs (cross-node
delivery, replica fan-out, node lifecycle).

> Runbook + port-forward reference:
> [docs/public/operate/reproduce-staging-locally.md](../operate/reproduce-staging-locally.md).

### 2. Secrets

`make up` seeds the k8s Secrets (OpenAI / Anthropic keys, identity
service signing-key encryption secret, DB DSN, etc.) from the genesis
envelope via `scripts/k3d/seed-secrets.sh`. If you change a secret,
re-seed with:

```bash
make k3d-secrets
```

See [docs/public/operate/env-vars.md](../operate/env-vars.md) for the full
bootstrap-envelope-vs-concept-storage breakdown.

### 3. Watch logs

```bash
kubectl logs -n memql deploy/bff -f
```

---

## Verify

The cluster is reached via kubectl port-forwards. Start the ones you
need, then probe them:

```bash
# Identity service health
kubectl port-forward -n memql svc/identity 8085:8085 &
curl -v http://localhost:8085/.well-known/jwks.json

# Database (via the postgres port-forward; `make db` opens a psql shell)
kubectl port-forward -n memql svc/postgres 5432:5432 &
psql postgres://memql:memql_dev@localhost:5432/memql -c "SELECT version();"
```

---

## What's running?

There is no nginx front door and no `*.local.znas.io` subdomains in the
k3d world -- each service is reached with its own port-forward:

| Service | Port-forward | Notes |
|---------|--------------|-------|
| **App SPA** | `svc/copresent 8080:8080` | Built bundle (needs `MEMQL_PACKAGES_TOKEN` at image-build time) |
| **Identity service** | `svc/identity 8085:8085` | Magic-link auth, OAuth, JWKS, /admin, /pair/* |
| **BFF (gRPC)** | `svc/bff 50051:50051` | gRPC for cockpit / SDKs; HTTP/WS browser bridge |
| **LiveKit** | `svc/livekit 7880:7880` | Voice/video transport |
| **PostgreSQL** | `svc/postgres 5432:5432` | `make db` opens a psql shell |

**Database credentials:** `memql / memql_dev` on database `memql`.

---

## Run tests

```bash
go test ./...
```

---

## Stop / reset

```bash
# Tear down the cluster
make down

# Also drop the kubeconfig context
make down PURGE=1
```

After a code change, the inner loop is `make dev [NODE=<type>]`, which
rebuilds the image, imports it into k3d, and rolls the Deployment. For a
clean slate (fresh DB + fresh secrets), `make down && make up` recreates
the cluster from scratch.

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
make up
```

Operators who set SOME but not all of the required envs go
through the interactive wizard; their pre-set values prefill the
form. This means a placeholder `IDENTITY_BOOTSTRAP_DOMAIN` in the
local overlay default is fine -- the rest of the wizard fields
remain blank and the operator fills them in interactively the
first time.

---

## Cluster mode

```bash
make dev                  # rebuild + import + roll all app nodes
make dev NODE=bff         # rebuild + roll a single node (faster)
```

Each node runs a build-tagged binary (`-tags voice`, `-tags cognition`,
etc.) in its own Deployment, all sharing one PostgreSQL database. The
BFF dials the workers via `WorkerDialer`, seeded by `MEMQL_WORKER_PEERS`
and reconciled against `v1:cluster:node`.

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
lsof -i :50051  # BFF gRPC port-forward
lsof -i :8085   # identity port-forward
lsof -i :5432   # PostgreSQL port-forward
```

### Pods not starting

```bash
kubectl get pods -n memql                                            # are pods Running?
kubectl logs -n memql deploy/bff -f                                  # check node logs
```

### Database connection errors

```bash
kubectl exec -n memql deploy/postgres -- pg_isready -U memql
psql postgres://memql:memql_dev@localhost:5432/memql   # needs the postgres port-forward (make db)
```

### Concepts not loading

If `bff` logs say "no concepts loaded" or refuse to start, check for
schema-validation errors: a concept declaring a reserved payload field
(`createdBy`, `partition`, `id`, ...) bricks the whole loader. See
[memql-authoring-rules.md #19](../language/authoring-rules.md#19-reserved-intrinsics-do-not-redeclare-id--createdby--createdat--partition).

---

## Tips

- The local k3d cluster runs voice via the in-cluster LiveKit Deployment
  (`deploy/k8s/base/livekit.yaml`); the in-app voice path works locally,
  while avatar VIDEO remains staging-only.
- For ad-hoc DB inspection use `make db` (opens a psql shell over the
  postgres port-forward), or point any client at the forwarded `:5432`.
