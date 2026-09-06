---
title: Reproduce the cloud locally (k3d + ArgoCD)
audience: public
status: stable
area: operate
sinceVersion: 0.9.36
owner: znas
---

# Reproduce the cloud locally (k3d + ArgoCD)

The k3d + ArgoCD cluster is the **blessed local dev topology** (memql#2061,
Epic 0 -- Argo parity): boot it with `make up`. It mirrors the cloud cluster
(AKS, `deploy/k8s/`) end to end -- the same Kustomize base, the same
ArgoCD-reconciled manifests, the same `ignoreDifferences`/selfHeal config -- so
the full class of GitOps + cross-node mesh bugs reproduces locally instead of
only after a deploy.

> **MemQL ships ONE installation shape** (epic memql#3943). There is no
> staging-versus-production dimension inside the product: an operator who wants
> a second environment installs a second instance, with its own domain and its
> own ArgoCD. Local and cloud are two INSTALLS of the same system, not two
> environments of one, which is why the only differences enumerated below are
> resource sizes and the sources of DNS, TLS and secrets.

> **Development principle: multi-node is the default.** Every feature
> runs across the 2-replica mesh locally and in the cloud -- never
> assume a single process. State/context/events that cross a node
> boundary need explicit plumbing (proxied/forwarded requests don't
> carry another node's session state; cross-node events need a routing
> rule). Implement AND test for the hop: a green single-node unit test
> is a false signal -- exercise the proxied/cross-node path
> (`test/clustere2e/`, `component/grpc/ai_forward_test.go`) and verify
> on this cluster. See the "Multi-node is the DEFAULT" rule in the
> root `CLAUDE.md`. (Bugs this would have caught: memql#1448, #1412,
> #1388.)

## What "parity" means here

| Aspect | Local cluster | Cloud cluster | Parity |
|---|---|---|---|
| Orchestrator | ArgoCD (k3d) | ArgoCD (AKS) | identical |
| Manifests | `deploy/k8s/overlays/local/` | `deploy/k8s/overlays/cloud/` | same base, values differ |
| Node-type split | identity / mcp / agent / planner / workbench / edge | same | identical (the product `bff` head is pack-owned, #2204) |
| Build model | engine (`Dockerfile`) for ALL node types (`memql-<type>:local`), product-agnostic | same product-agnostic engine images | identical -- a product's DSL mounts at runtime via `MEMQL_DSL_PATH` (the `dsl-bundle` component), not a per-product image; see [downstream-stacks.md](downstream-stacks.md) |
| Replicas per mesh node (default) | **1** (scale to 2 with `make scale N=2`) | **2**; scaled to 0 when idle | equivalent once scaled -- the saving is the idle time, not the width |
| Per-replica node id | `fieldRef: metadata.name` (downward API, same as the cloud) | `fieldRef: metadata.name` | **identical** |
| `MEMQL_NODE_ID` uniqueness | enforced by fieldRef -- unique per pod | enforced by fieldRef | identical |
| ArgoCD `ignoreDifferences` | `/spec/replicas` excluded | same | identical |
| Database | CloudNativePG Cluster (`memql-db`), 1 instance, WAL + base backups to Azurite | CloudNativePG Cluster (`memql-db`), 3 instances across 3 zones, WAL + base backups to Azure Blob | config only -- **same operator + manifests since memql#3846**; only instances/sizes/backup destination differ |
| Connection pooler | not present locally (single db-pool pod, direct) | not present -- both DSNs point at `memql-db-rw` directly; a PgBouncer `Pooler` ships ready but not enabled (`cnpg-db/optional/pooler`) | config only |
| Blob storage | Azurite emulator | Azure Blob | config only |
| Secrets / keys | dev defaults (seeded by `make secrets`) | Key Vault via ESO | config only |
| ExternalSecrets | deleted by `$patch: delete` in local overlay | present | config only |
| Ingress | k3s-bundled traefik front door (`identity.memql.localhost`, mkcert TLS) + port-forwards for the gRPC heads | ingress-nginx | divergent -- traefik vs nginx |
| Digest-pinning gate | skipped for the `local` overlay in `scripts/deploy/drift-check.sh` (`check_rendered`'s `ENV=local` branch, which still asserts the overlay renders) | enforced | divergent -- justified |

## Prerequisites

- **docker** -- Docker Desktop or Colima (must be running).
- **k3d** -- `brew install k3d`.
- **kubectl** -- `brew install kubectl`.
- **mkcert** -- `brew install mkcert`. The front door terminates TLS with a
  browser-trusted `*.memql.localhost` wildcard; `make secrets` issues that pair
  for you (see [Front-door TLS](#front-door-tls) below), but it needs mkcert
  and a root CA on the machine.
- **certutil** -- `sudo apt-get install -y libnss3-tools` on Debian/Ubuntu, `brew install nss` on macOS (Firefox's trust store is NSS on both; Safari and Chrome read the system keychain and need nothing extra). `scripts/install/nss-tools.sh` installs it for you and picks the right package manager.
  (`nss-tools` on Fedora/RHEL, `nss` on Arch). Firefox and Chrome on Linux read
  their own NSS trust store rather than the system one, and mkcert can only
  write it through certutil. Without it mkcert warns and exits 0, so the pair
  is issued, the cluster comes up, and every browser refuses the front door --
  which is why it is now a hard prerequisite (memql#3560). On macOS the system
  keychain covers Safari and Chrome; only Firefox needs NSS, so it is not
  required there.

  Every `make secrets` checks for it, not only the first (memql#3730), because
  every run now re-checks that the front-door pair covers the domain. On a
  machine with **no browser at all** -- a headless box, a CI runner -- waive
  browser trust instead of installing it:

  ```bash
  MEMQL_LOCAL_TLS_ALLOW_MISSING_CERTUTIL=1 make secrets
  ```

  The front door then works for `curl`, the SDKs and the Cockpit, and stays
  untrusted in Firefox and Chrome. `--allow-missing-certutil` is the same waiver
  when calling `scripts/k3d/seed-secrets.sh` directly.
- **git** -- the cluster's ArgoCD Application points at the current git branch;
  you must push your branch before ArgoCD can sync it.

### Front-door TLS

One-time, per machine -- creating a root CA writes to the system trust store,
so it stays a deliberate step and never a prompt:

```bash
bash scripts/install/mkcert-setup.sh --confirm=install-memql-ca
```

The confirmation phrase is only needed if you have no mkcert CA yet. An
existing CA is never regenerated -- it may be signing certificates for your
other local stacks -- but `mkcert -install` IS run against it, because a CA on
disk is not evidence that anything trusts it. mkcert creates the CA first and
trusts it second, so an interrupted first run leaves a real CA that no browser
accepts; the run that follows completes the trust rather than reading the
file's presence as proof the work is done (memql#3560).

Trusting a CA writes to the system trust store, which needs a password. Run
this in a terminal: from a process with no terminal attached -- a VS Code
extension host, a CI step -- sudo has nowhere to prompt, and the capability
refuses with exit 4 and the exact command to run instead.

After that, `make up` / `make secrets` issue the `*.memql.localhost`
pair at `~/.memql/certs/dev.{crt,key}` when it is absent, reuse it when it is
present **and covers the domain being served**, and load it into the cluster as
the `memql-front-door-tls` Secret that both front-door ingresses reference.
Override the location with `MEMQL_LOCAL_TLS_CERT` / `MEMQL_LOCAL_TLS_KEY` (or
`--tls-cert` / `--tls-key`).

A pair that does **not** cover the domain is REISSUED -- with a warning naming
both the names it carried and the names it needed, and
`frontDoorTlsSource: reissued` in the result envelope (memql#3730). Reuse was
previously conditional on the file merely EXISTING, so a machine that ran the
local stack before the domain rename (memql#3593) kept seeding a valid
certificate for a domain that no longer exists: traefik had nothing matching the
requested SNI, served its own default certificate instead, and `make secrets`
reported `ok: true` over it -- permanently, because re-running it changed
nothing. A reissue overwrites the pair in place, so a hand-made certificate you
want kept belongs somewhere `MEMQL_LOCAL_TLS_CERT` points at, and must cover
`*.<domain>` and `<domain>`. A re-run over a pair that already covers them never
rotates it; deliberate reissue of a matching pair is `mkcert-setup.sh --force`.

Which domain it is checked against is, in order: `--domain` / `make secrets
DOMAIN=...`, then **the domain this cluster already serves** (the `memql-domain`
ConfigMap), then `memql.localhost`. The cluster tier matters on a custom-domain
cluster: without it, a plain `make secrets` would check the pair against
`memql.localhost` and reissue over your `lab.example.com` certificate.

`frontDoorTlsCoverageVerified` in the envelope says whether the names were
actually read. Without openssl they cannot be, and the pair is then kept
untouched and reported as **unverified** rather than as covering -- the one
outcome where a run cannot rule out serving the wrong certificate.

The bring-up asserts that `memql-front-door-tls` exists AND that the certificate
inside it covers the front-door hostnames, and FAILS on either (memql#3384,
memql#3730). It has to: traefik answers a missing referenced secret -- or one
holding a certificate for names it was not asked for -- by silently serving its
own `TRAEFIK DEFAULT CERT`, so every TLS client sees an untrusted edge while the
whole mesh reports Available. Without openssl the certificate cannot be read;
the check then says the names were not verified rather than claiming coverage it
did not establish.

Non-browser clients need the CA too. Node (the VS Code extension host, npm
tooling) does **not** read the OS trust store: point it at mkcert's root with

```bash
export NODE_EXTRA_CA_CERTS="$(mkcert -CAROOT)/rootCA.pem"
```

otherwise the connection fails with `unable to verify the first certificate`.

No product sibling repo is required: the engine cluster builds every node
image from this repo's Dockerfile, product-agnostic. A product layers in at
runtime by mounting its DSL bundle at `MEMQL_DSL_PATH` (the `dsl-bundle`
component) -- there are no per-product node images (see
[downstream-stacks.md](downstream-stacks.md)).

No env file is required for `make up`; dev secrets are hardcoded
in `scripts/k3d/seed-secrets.sh` (Azurite well-known key, `memql_dev` Postgres
password).

## Bring it up

```bash
# Single-node (fast, default):
make up

# Multi-node (2 servers + 1 agent, for cross-node mesh testing):
make up SERVERS=2 AGENTS=1
make scale N=2

# Clean slate (nuke + repave -- wipes the in-cluster DB by construction):
make up-refresh
```

`make up` is the full fresh bring-up (`scripts/k3d/bringup.sh`); it does the
following in order:

1. Creates a k3d cluster (default name `memql`).
2. Installs ArgoCD v2.13.3 (same version as the cloud) via
   `kubectl apply -k deploy/argocd/bootstrap`.
3. Seeds k8s Secrets (`memql-secrets`, `memql-db-app-creds`) via
   `scripts/k3d/seed-secrets.sh`.
4. Applies the ArgoCD Application `memql-local` pointing at
   `deploy/k8s/overlays/local` on the current git branch.
5. Waits for ArgoCD to sync and pods to become Ready (configurable via
   `MEMQL_K3D_ARGOCD_TIMEOUT`, default 300s).
6. Builds + imports the engine images (same as `make dev`) so no pod sits in
   ImagePullBackOff, then waits for every Deployment to become Available.

`make up-refresh` runs the same bring-up preceded by a purge teardown
(`make down PURGE=1`), so the in-cluster Postgres is wiped and the
environment repaves from scratch. It is idempotent and honors the same
`SERVERS`/`AGENTS`/`REVISION` overrides as `make up`.

## Choosing a domain

The cluster is served at `memql.localhost` by default -- an RFC 6761 loopback
name that needs no domain ownership, no DNS provider and no third party. Bring
your own instead with `DOMAIN=`:

    make up DOMAIN=lab.example.com

Everything domain-shaped follows from that one value. It is seeded into the
cluster as the single `MEMQL_DOMAIN` key of the `memql-domain` ConfigMap, and
every node derives its identity base URL, expected issuer, discovery endpoint,
CORS origins and OAuth redirect URIs from it at boot
(`component/envregistry/domain.go`). When the domain differs from the overlay's
committed default, `k3d.up` also emits two `spec.source.kustomize.patches`
entries on the ArgoCD Application to repoint the two front-door Ingress hosts.
No file under `deploy/` names a domain.

### The hosts file is only written when it is needed

The installer points the front-door hostnames at 127.0.0.1 -- unless they
already resolve there. If your own DNS answers 127.0.0.1 for
`api.<domain>`, `identity.<domain>`, `mcp.<domain>`, `portal.<domain>`, `os.<domain>` and the
apex, the hosts block is skipped entirely and no elevation prompt appears. A
hostname resolving to some OTHER address is refused rather than shadowed,
naming the address it answered.

Note that a resolver honouring RFC 6761 -- systemd-resolved does -- already
answers every `*.localhost` name on loopback, so on those machines the default
domain needs no hosts entry at all.

### The domain is chosen once

It is baked into every passkey (the WebAuthn RP id is derived from it), every
session and node token (the issuer is `https://identity.<domain>`), the
certificate's SANs and the hosts block. `make up DOMAIN=...` refuses a value
that differs from the one the cluster is already serving; rebuild with
`make up-refresh DOMAIN=...`.

### If a node will not start

`CreateContainerConfigError` naming `memql-domain` means the ConfigMap is
missing. Re-run `make secrets`.

The reference is deliberately not optional. A node with no domain would fall
back to the base manifests' placeholder issuer, boot, form a mesh, and
reject every token with an error naming neither the domain nor the ConfigMap --
so refusing to start is the honest failure.

## Port reference

After `make up`, these host ports are mapped to the k3d cluster:

| Port | Service |
|------|---------|
| `443` | the front door (traefik: TLS terminated here, routed by hostname) |
| `80` | the front door (redirects) |
| `5432` | Postgres -- debug only, never a connection path |

The identity (`8085`) mapping is gone: it was a second entrance to a service
the front door already serves, which is what
[environment-parity.md](environment-parity.md) forbids.

The product SPA (`:8080`) and the product `bff` gRPC head (`:50051`) -- a
plain engine `bff` node fronting the product's DSL bundle -- are NOT part of
the engine repo's local overlay (#2204); they are wired from the product's own
overlay (the client image + the `dsl-bundle` component). Clients (the Cockpit,
SDKs) connect to the product-neutral `bff` node **exactly as in the cloud** --
through the `api.memql.localhost` traefik front door (TLS on 443, mkcert
`*.memql.localhost` wildcard, h2c gRPC to `svc/bff:50051`); no port-forward is in
the connection path (see [environment-parity.md](environment-parity.md)). For
low-level gRPC debugging only, a raw port-forward is still available:

```bash
kubectl port-forward -n memql svc/bff 50051:50051   # debug only; not the connection path
```

Access identity: `https://identity.memql.localhost` (front-door TLS -- needs a
`*.memql.localhost` hosts entry; the cert is issued and seeded by `make up` /
`make secrets`, see [Front-door TLS](#front-door-tls)). The front door is the
ONLY entrance; for low-level debugging a raw
`kubectl port-forward -n memql svc/identity 8085:8085` still works.
Access the engine gRPC head: `localhost:50051` (after the port-forward above)

## Inner-loop dev

The workflow after a code change:

```bash
# Rebuild ALL nodes and restart pods:
make dev

# Rebuild a single node type (faster):
make dev NODE=bff
make dev NODE=identity
make dev NODE=agent

# Pull and import upstream infra images (postgres/azurite):
make dev PULL_INFRA=1
```

`make dev` does:

1. `docker build` the node image from this repo's Dockerfile (`BUILD_TAGS=<type>`), product-agnostic; a product delivers its DSL at runtime via `MEMQL_DSL_PATH` (the `dsl-bundle` component), not by rebuilding the image.
2. `k3d image import` -- loads the image into the cluster's containerd.
3. `kubectl rollout restart deployment/<node>` -- triggers a pod roll so
   the new image is used. ArgoCD's `ignoreDifferences` does not cover
   the `restartedAt` annotation so selfHeal won't revert this.

The inner loop is **pure-Argo**: no manifest files are applied directly. The
pod restart is purely at the pod level; ArgoCD still owns the Deployment spec.

### Rebuilding a wizard-installed cluster

A cluster the install wizard created runs RELEASED images: `k3d.up
--image-registry/--image-tag` writes `spec.source.kustomize.images` overrides
onto the ArgoCD Application, pinning every node to a registry tag. So a plain
rebuild there builds and imports images nothing references, and the checkout
the install cloned stays inert. Two params on the same `k3d.dev` capability
close that gap:

| Flag | What it names |
|---|---|
| `--repo-root=<checkout>` | the checkout the images are **built from**. Mirrors `k3d.up --repo-root`, and exists for the same reason: the packaged editor extension runs a staged copy of `scripts/` with no Go source beside it, so "this script's own repository" is not a MemQL tree there. |
| `--image-source=checkout` | after the images are imported, **remove the image override of every node this run built** so the overlay's own `:local` references apply to them. ArgoCD's resulting sync is what rolls the pods. |

The extension's **Rebuild from checkout** is exactly
`k3d.dev --repo-root=<checkout> --image-source=checkout`, driven by the one-step
graph `scripts/install/graph/rebuild.json`.

The **database operand's override is kept**. `memql-db` is not a node: it is
versioned on the PostgreSQL axis, and CloudNativePG refuses an `imageName` whose
tag it cannot parse (memql#4063), so the rebuild leaves it pinned and skips
building it.

**So does every node you did not rebuild.** With `NODE=bff` only bff's override
is dropped; the other eight keep pointing at their released images, which are
the images actually in the cluster. Dropping theirs would aim them at a
`memql-<node>:local` this run never imported -- and under
`imagePullPolicy: IfNotPresent` that is `ImagePullBackOff`, on nodes you never
asked to touch.

**It fails rather than flattering you.** An `--app-name` no Application answers
to is refused (exit 4) instead of being read as "there were no overrides to
drop", and after patching, the run waits for every Deployment to actually name
`memql-<node>:local` (exit 5 if it never does) -- because ArgoCD's `Synced` is
bookkeeping about a comparison it has already made, and can be a stale read
taken before the refresh landed.

The change is not permanent. An install, upgrade or repair rewrites those
overrides, which returns the cluster to released images -- rebuild again to go
back to the checkout's. Without `--image-source=checkout` nothing is patched, so
`make dev` behaves exactly as it always has.

### ...and bringing that checkout up to date first

**Update from origin and rebuild** sits beside it and is the same crossing with
a fetch in front: `install.updateStack` over the recorded checkout, then the
identical rebuild step, driven by `scripts/install/graph/update-rebuild.json`.
Both buttons stay, because they answer two different questions -- "test just
what I have" and "test the latest with what I have" -- and neither is a mode of
the other.

**A refusal changes nothing on disk.** That is the property the whole thing is
arranged around, because the person who presses this is a developer holding
uncommitted work:

| What it finds | What it does |
|---|---|
| behind, and your edits do not touch what is arriving | fast-forwards; your edits come along |
| your edits touch files the update also changes | stops, names the files, changes nothing |
| commits here the branch does not have | stops and says so -- or combines them, if you chose that |
| conflicts while combining | stops, names the paths, and **leaves the conflict in the editor to resolve** |
| a merge or rebase already under way | stops before it fetches |

The safety is git's rather than ours: `git merge --ff-only` and `git checkout
--detach` both carry uncommitted edits across and both refuse atomically. What
the script adds is computing the overlap first, so the refusal names files and
the checklist can predict it instead of reporting it afterwards.

**A wizard-installed checkout is cloned `--depth 1`, and the first update
deepens it.** At depth 1 the local commit and a freshly fetched tip share no
ancestry in the object store, so git cannot see a fast-forward and every update
would report a divergence that is not real. It happens once per checkout and
the checklist says so before you start.

**A release install's checkout is detached at a tag and has no branch to
update to.** That is the honest answer rather than a gap: the checklist says so
and offers no branch of its own, because substituting `main` would quietly
offer to move a pinned cluster onto the tip of a branch nobody named. A `main`
install repaired since is detached too, and there the branch the install
recorded is used.

## Multi-node mesh testing

```bash
# Scale to 2 replicas per Deployment:
make scale N=2

# Litmus: unique MEMQL_NODE_ID per pod + one shared identity signing keyset:
make status

# Scale back to single-node:
make scale N=1
```

Because `deploy/k8s/base/` sets `MEMQL_NODE_ID` via `fieldRef: metadata.name`,
each pod automatically gets a unique node id matching its pod name. No overlay
changes are needed to enable multi-node.

`make status` asserts two properties, both of which only exist once there is
more than one replica:

1. **Unique `MEMQL_NODE_ID` per pod.** Shared ids are the root cause of the
   #1042 class of mesh bugs.
2. **One shared identity signing keyset.** It reads
   `/.well-known/jwks.json` from every identity replica (through the apiserver
   pod proxy -- engine images are `FROM scratch`, so there is no shell to exec
   into) and compares the `kid` sets. Divergent keysets mean each replica has
   minted its OWN Ed25519 key, so a token issued by one is unverifiable by any
   node that fetched JWKS from the other: roughly half of all auth fails, and
   the rejection reads as an authentication error rather than a key problem.
   That is memql#3400 -- `make scale N=2` walked straight into it before
   `make secrets` started seeding a shared `MEMQL_IDENTITY_SIGNING_KEY_B64`.

A replica the litmus could not read is reported `UNKNOWN`, never as a pass.

## Re-seed secrets

```bash
make secrets
```

This re-runs `scripts/k3d/seed-secrets.sh` and is idempotent -- including for
the two values a re-run must never silently rotate: `MEMQL_MASTER_KEY`
(memql#2958) and the identity signing seed `MEMQL_IDENTITY_SIGNING_KEY_B64`
(memql#3400, whose rotation would invalidate every live session and every
minted mesh node token). Both are read back from the cluster and preserved.
Use it if you've
torn down and recreated the cluster, or if you've rotated the dev secret values.

## Database: backups and a restore drill (local)

The local database is a **CloudNativePG Cluster** (`memql-db`), not a bare
`postgres` pod — the same operator, the same operand image and the same four
resource kinds the cloud runs, with smaller numbers in them
(memql#3846). It archives WAL and takes base backups to the in-cluster
**Azurite**, so the backup path is exercised locally rather than only in the
cloud.

```bash
make status          # includes the database litmus: phase, extensions, archiving
```

The litmus reports three things a pod listing cannot distinguish:

| Line | Why it is separate |
|---|---|
| `cluster:` / `phase:` | is Postgres serving, and which pod is primary |
| `extensions:` | a Cluster reports **Ready** as soon as Postgres accepts connections — *before* the `Database` CR reconciles `timescaledb` and `vector`. A Ready cluster with no timescaledb looks healthy and fails the first `create_hypertable`. |
| `archiving:` | the one that fails **silently** — the database serves traffic perfectly with no backups behind it |

### Take a backup

```bash
kubectl apply -n memql -f - <<'YAML'
apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata: { name: manual-backup }
spec:
  cluster: { name: memql-db }
  method: plugin
  pluginConfiguration:
    name: barman-cloud.cloudnative-pg.io
    parameters: { barmanObjectName: memql-db-backup }
YAML

kubectl get backup manual-backup -n memql -w   # -> phase: completed
```

A nightly `ScheduledBackup` runs the same path unattended.

### Restore into a scratch Cluster

Recovery is a **new Cluster** bootstrapped from the object store — never an
in-place operation on the running one:

```bash
kubectl apply -n memql -f - <<'YAML'
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: memql-db-scratch }
spec:
  instances: 1
  imageName: memql-db:16-dev
  imagePullPolicy: IfNotPresent
  enablePDB: false
  postgresql:
    shared_preload_libraries: [timescaledb]
  storage: { size: 2Gi }
  bootstrap:
    recovery:
      source: origin
  externalClusters:
    - name: origin
      plugin:
        name: barman-cloud.cloudnative-pg.io
        parameters:
          barmanObjectName: memql-db-backup
          serverName: memql-db
YAML

kubectl wait --for=condition=Ready cluster/memql-db-scratch -n memql --timeout=480s
kubectl exec -n memql memql-db-scratch-1 -c postgres -- \
  psql -U postgres -d memql -c "SELECT count(*) FROM timescaledb_information.hypertables;"
kubectl delete cluster memql-db-scratch -n memql       # tear the scratch copy down
```

Add `bootstrap.recovery.recoveryTarget.targetTime: "<RFC3339>"` to restore to a
point in time rather than to the latest backup.

> **Two local-only settings make this work, and both look like noise.** The
> `ObjectStore`'s `destinationPath` carries the account as the first path
> segment (`http://azurite:10000/devstoreaccount1/...`), where the cloud form
> carries it in the hostname
> (`https://<account>.blob.core.windows.net/<container>/`) — host first in
> both, never a container-only `azure://<container>/` shape (memql#4496).
> And the azurite Deployment passes `--skipApiVersionCheck`,
> because Azurite refuses the `x-ms-version` barman's SDK sends. Without either
> one there are simply no backups, and **the Cluster still reports Ready** —
> which is why `TestLocalBackupsCanActuallyReachAzurite` asserts both.

## Tear down

```bash
make down           # delete cluster (keeps kubeconfig context)
make down PURGE=1   # also remove the kubeconfig context
```

## Config-vs-topology audit

The governing invariant: **along the mesh-delivery path, only config may differ
from the cloud -- never topology or build.** Every divergence below is enumerated
with its justification.

### Invariants -- MUST stay identical

- **Service set:** identity / mcp / agent / planner / workbench / edge (the
  product `bff` head and SPA are pack-owned, #2204).
- **Build source per node:** every node is the same **product-agnostic engine
  image** (built here from this repo's Dockerfile; digest-pinned in the cloud
  overlay) -- local and cloud never diverge on build. Only the **DSL bundle** mounted at
  runtime (`MEMQL_DSL_PATH`) differs per product (the #1053 rule, revised under
  platform consolidation #2472).
- **`fieldRef: metadata.name`** for `MEMQL_NODE_ID` on every Deployment in
  `deploy/k8s/base/` -- identical to the cloud.
- **ArgoCD `ignoreDifferences`** on `/spec/replicas` -- identical to the cloud.
- **Inter-node addressing** (`MEMQL_NODE_ADDRESS` / `MEMQL_PARENT_ADDRESS` /
  `MEMQL_WORKER_PEERS` / `MEMQL_WORKBENCH_REMOTE`) -- via k8s Service DNS
  (same as the cloud, just cluster-local).

### Divergences -- justified

| # | Divergence | Local | Cloud | Why acceptable |
|---|---|---|---|---|
| 1 | **Replicas (default)** | 1 per Deployment | 2 per Deployment (0 when idle) | Resource-constrained laptops locally; cost in the cloud, which parks at zero between uses. Multi-node is opt-in in BOTH via `make scale N=2`. The fieldRef mechanism is identical everywhere, so the multi-node path fully reproduces wherever you scale it up. |
| 2 | **Ingress** | k3s-bundled **traefik** front door for `identity.memql.localhost` (mkcert TLS); gRPC heads via port-forward | ingress-nginx on AKS | Same ingress *topology* as cloud (an HTTPS front door for identity); traefik ships with k3s so there's no extra install. gRPC heads (`mcp:50051`) stay on port-forward -- they're not fronted locally. |
| 3 | **Digest-pinning gate** | skipped for `ENV=local` in `scripts/deploy/drift-check.sh` | enforced | Local images are built by `make dev` with a stable `:local` tag; they have no ACR digest. `check_rendered` special-cases `ENV=local`: it skips the digest-pin assertion but still fails if the overlay does not render. As of this writing no Go test asserts this exemption specifically -- `scripts/deploy/drift_check_test.go` covers image-ref normalization, not the local-skip branch -- so the behaviour is enforced by the script alone. |
| 4 | **ExternalSecrets / Key Vault** | deleted by `$patch: delete` in local overlay | ESO syncs from Key Vault | Dev secrets are seeded directly by `make secrets`. |
| 5 | **Connection pooler** | direct Postgres connection | direct Postgres connection (a PgBouncer `Pooler` ships ready but not enabled, `cnpg-db/optional/pooler`) | Single-node dev without a pool is safe; the cloud runs without one today too, and the optional pooler can be enabled on either side if a connection-count ceiling ever demands it. |

### Config-only -- EXPECTED to differ

- `MEMQL_DATABASE_DSN` (local `memql-db-rw` vs cloud `memql-db-rw`, self-hosted CloudNativePG on both sides).
- Blob backend (Azurite connection string vs Azure Blob).
- Bootstrap/dev escape hatches (`MEMQL_IDENTITY_ALLOW_INSECURE_*`).
- `MEMQL_IDENTITY_BASE_URL` / `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` (local port-forward
  vs AKS ingress hostname).

## Worked example: reproduce a cross-node mesh bug

1. `make up SERVERS=2 AGENTS=1 && make scale N=2`.
2. `make status` -- verify all pods show distinct `MEMQL_NODE_ID` values.
   If any share an id, stop: the mesh cannot reproduce cross-node bugs.
3. Reproduce the scenario (e.g. send a chat message that triggers an assistant
   reply). Watch logs:
   ```bash
   kubectl logs -n memql -l app=agent --all-containers -f | grep -Ei 'node_id|EventForward|dedup'
   ```
4. Root-cause against the cross-node path (event routing rules, session state
   on the wrong node, missing proxy forward).
5. Fix and `make dev` to rebuild + restart. ArgoCD reconciles; the pod roll
   picks up the new image.

## The previous Compose-based local stack is retired

The k3d + ArgoCD topology is the **only** supported local path as of
memql#2061 (Epic 0). The earlier Compose-based local stack is fully retired
(memql#2068 / #2088) -- the old compose files and their `make` targets are
deleted, replaced by `make up` / `make dev` / `make down`. k3d + ArgoCD wins
because:

- It uses the same manifests and reconciliation path as the cloud (Kustomize +
  ArgoCD), so GitOps bugs reproduce locally.
- `fieldRef: metadata.name` for `MEMQL_NODE_ID` is identical to the cloud.
- The ArgoCD `ignoreDifferences` and selfHeal behavior matches the cloud exactly.

There is no *nginx* front door locally, but the local overlay DOES ship a
k3s-bundled **traefik** front door on 443 for `https://identity.memql.localhost`
(`deploy/k8s/overlays/local/front-door.yaml`), terminating TLS with a
browser-trusted mkcert `*.memql.localhost` wildcard (`memql-front-door-tls`, issued and
seeded by `make secrets` at `~/.memql/certs/dev.{crt,key}` -- see
[Front-door TLS](#front-door-tls); its absence fails the bring-up rather than
degrading silently). This mirrors the cloud ingress topology. The **gRPC** heads
are still reached via kubectl port-forward -- identity is not exposed on gRPC
externally, and the `mcp` engine gRPC head `:50051` is forwarded on demand.
Postgres `:5432` is likewise port-forwarded; the product SPA + the product
`bff` (a plain engine node fronting the product's DSL bundle) are
engine-external and absent locally (#2204).
