---
title: memQL + CoPresent Deployment Strategy (AKS)
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# memQL + CoPresent Deployment Strategy (AKS)

Authoritative deploy + operations reference for the memQL node mesh and the
CoPresent product on **Azure Kubernetes Service**. This supersedes the former
Google Cloud Run and Azure Container Apps strategies, which are retired and
removed.

- Staging: cluster `aks-memql-staging` (rg `rg-memql-staging`), namespace
  `memql`. Hosts `app.staging.copresent.ai`, `identity.staging.copresent.ai`.
- Registry: `acrmemql.azurecr.io` (ACR). Database: managed **Tiger Cloud**
  (Timescale). Secrets: **genesis A2** sealed envelope. Per-env config in
  **Key Vault** (`kv-memql-<env>`).

---

## 1. Topology (live)

Namespace `memql` runs **8 Deployments** (engine mesh + carrier + SPA):

| Deployment | Image | Replicas | Role |
|---|---|---|---|
| `identity` | `memql-identity` | 2 (HA, envMode) | magic-link auth, JWT issuance, JWKS, admin UI. Owns the one-time DB migration. |
| `bff` | `memql-bff-copresent` (carrier) | 2 | backend-for-frontend; mesh hub; `/memql/ws`. |
| `cognition` | `memql-cognition` | 2 | routing + conductor + Polyphon. |
| `voice` | `memql-voice` | 2 | voice transport; `/memql/audio`. |
| `agent` | `memql-agent` | 2 | task execution, SI work, tool calling. |
| `planner` | `memql-planner` | 2 | plan orchestration. |
| `workbench` | `memql-workbench` | 2 | sandboxed headless work surface. |
| `copresent` | `copresent` (SPA) | 2 | the web app (static SPA served by nginx). |

Networking: a single `bff-external` LoadBalancer plus **3 Ingress** objects on
the ingress-nginx controller (cert-manager-issued certs):

- `app-main` + `app-identity-proxy` → `app.staging.copresent.ai` (SPA, `/memql/ws`,
  `/memql/audio`, and the same-origin `/.well-known/jwks.json` proxy).
- `staging-identity` → `identity.staging.copresent.ai` (identity service over TLS).

Internal mesh: each node verifies identity-issued JWTs via its per-node verifier
against `https://identity:8085` (internal cluster CA, `deploy/k8s/tls/`).
**Convention: the issuer/host is `identity.<env>.copresent.ai`. There is no
`auth.*` host or issuer anywhere.**

Engine images are built from the memQL repo (root `Dockerfile`, per-node build
tags). The `bff` carrier (`memql-bff-copresent`) and `copresent` SPA are built
+ version-pinned from their own repos and referenced in
`deploy/k8s/{bff,copresent}.yaml`.

```
                          Internet
                             |
              ingress-nginx (cert-manager TLS)
            /            |                     \
 app.staging.*      app.staging.*         identity.staging.*
 (app-main)     (app-identity-proxy)     (staging-identity)
      |                  |                      |
   copresent SPA    bff (/memql/ws,        identity (2, HA)
      |             /memql/audio,            |  JWKS @ :8085
      |              jwks proxy)             |
      \-------- bff-external LB -------------/
                       |
   bff <-> cognition / voice / agent / planner / workbench   (NodeService mesh, mTLS)
                       |
              managed Tiger Cloud (Postgres + TimescaleDB)
```

---

## 2. Deploy: `make deploy VERSION=X` (`scripts/deploy/aks-deploy.sh`)

One command takes the cluster from source to a rolled-out, gated deploy:

1. **Build + push** the 6 engine images via `az acr build` (root `Dockerfile`,
   one per build tag; `voice` is the CGO exception). Engine tags are
   **immutable** — a build that would overwrite an existing
   `memql-<nt>:<tag>` is refused (see §6). `--skip-build` deploys already-pushed
   tags.
2. **Ensure secrets**: internal TLS CA + identity cert
   (`deploy/k8s/tls/gen-internal-ca.sh`); warns if `memql-secrets` is absent.
3. **Migration gate** (pre-rollout): a Job pinned to `VERSION` runs the DB
   migrations once and **aborts the deploy on failure** — the mesh never rolls
   onto a half-migrated schema.
4. **Apply (digest-pinned overlay)**: `kubectl apply -k deploy/k8s/overlays/<env>`
   — the committed overlay pins **every** image by `@sha256:` digest, so it is
   the single image authority. There is **no** runtime `kubectl set image`: the
   apply cannot leave a node on a stale manifest tag (the #613/#684 class is
   structurally gone — deployment-v2 Phase 1, #699). `identity` is waited Ready
   first (JWKS), then the rest.
5. **Health gate**: a **drift assertion** (`drift-check.sh --live`: every live
   pod runs the exact digest the overlay pins), then the **bff promote**
   (#1520) + the **functional post-deploy gate** (#1519, §7.1), then the
   front-door **smoke gate** (§7). On failure with the gate armed (default),
   the deploy stops and prints the **git-revert** rollback procedure (§8) — it
   does **not** imperatively revert.
6. **Record validated version** (§6) on a green *deep* gate.

> **Why a functional gate (#1519/#1520).** A digest match (`drift-check`) plus
> `kubectl rollout status` are **not** proof a release works. The 0.9.60 deploy
> went green on both while it was actually broken two ways: (a) the bff
> blue/green Rollout sat at `BlueGreenPause` and was never promoted, so
> `bff-active` kept selecting the **old** color (no agent peer →
> `siSuggest: no connected agent node available`); (b) identity JWKS diverged
> across replicas (per-pod keys) → ~50% of token verifications failed, silently.
> The functional gate (§7.1) catches both before stamping a release validated.

Carrier + SPA are built/pinned from their own repos (`memql-bff-copresent
make release`; copresent `docker buildx` with the `node_auth_token` BuildKit
secret + `VITE_*` build-args) — not by this script.

Flags: `--version`, `--env`, `--skip-build`, `--skip-tls`, `--skip-migrate`,
`--no-smoke`, `--no-gate`, `--allow-overwrite`, `--dry-run`. `--dry-run` prints
the full plan and mutates nothing.

---

## 3. Configuration precedence

Two layers, lowest to highest:

1. **Genesis envelope (base layer, set-if-absent)** — shared secrets + config
   sealed in the A2 envelope (`MEMQL_GENESIS_B64` in `memql-secrets`), autoloaded
   at boot when `MEMQL_GENESIS_AUTOLOAD=true`. Applied only for keys not already
   in the environment.
2. **Per-pod env / envFrom (override)** — values set explicitly in the k8s
   manifests (node type, mesh addresses, `IDENTITY_*` hosts, feature flags) win
   over the envelope.

Rule of thumb: **shared secrets/config → genesis envelope** (via the re-seal
flow in §4); **per-node, non-secret config → k8s manifest env**. Never set
shared secrets with ad-hoc `kubectl set env`.

---

## 4. Secrets: genesis A2 envelope

Shared secrets live in a single encrypted envelope sealed under
`MEMQL_MASTER_KEY` (NaCl secretbox; see `component/secret/`). The cluster carries
three keys in the `memql-secrets` Secret: `MEMQL_MASTER_KEY`,
`MEMQL_GENESIS_B64` (the sealed envelope), and `MEMORY_NODES_DATABASE_DSN`.
The manifest of expected keys is `scripts/secrets/manifest.yaml` (entries may be
`optional: true` — documented + sealed-when-present but not required, e.g. the
identity signing seed).

**To add/rotate a shared secret (the canonical flow — do NOT `kubectl set env`):**

Since Phase 5 (#703), External Secrets owns `memql-secrets` and reconciles it
from Key Vault (`creationPolicy: Merge`, 1h refresh), so **Key Vault is the
single source of truth** and the cluster Secret follows it. The #734 drift —
Key Vault and the cluster Secret silently diverging — came from updating the two
sides as independent manual steps. The re-seal is therefore scripted, and the
script **hard-verifies that the live Secret converged to what was pushed before
it rolls any pod** (drift can no longer be left behind):

```bash
# 1. Edit the per-env source of truth (real values, gitignored, NEVER committed):
#    ~/Downloads/staging.genesis.env   (staging)   |   prod equivalent
# 2. Re-seal + propagate + verify-convergence + roll, in one guarded step:
MEMQL_MASTER_KEY=... \
scripts/secrets/reseal-genesis.sh \
    --env=staging --env-file=~/Downloads/staging.genesis.env
```

The script (`scripts/secrets/reseal-genesis.sh`):
1. seals the env-file under `MEMQL_MASTER_KEY`;
2. writes the blob to Key Vault (`kv-memql-<env>/memql-genesis-b64`);
3. propagates to the cluster — when ESO is present it forces an `ExternalSecret`
   refresh and waits for `SecretSynced` (ESO is the sole writer of the managed
   keys, so a manual cluster patch would just be reverted); when ESO is absent
   (pre-bootstrap) it `kubectl patch`es the Secret directly **in the same run**;
4. **verifies the live `MEMQL_GENESIS_B64` hash equals what was pushed** — aborts
   non-zero on divergence (the #734 guardrail) instead of rolling onto drift;
5. rolls the consuming deployments (`--no-roll` / `--roll="deployment/identity …"`
   to scope).

Never hand-patch only Key Vault or only the cluster Secret — the convergence
check exists precisely so neither side is updated alone.

---

## 5. Identity HA + signing key

Identity runs **2 replicas in envMode**: the Ed25519 signing seed
(`IDENTITY_SIGNING_KEY_B64`, std-base64 of 32 bytes) rides the genesis envelope,
so every replica derives the **same** key + `kid` + JWKS. There is **no RWO key
PVC** (which would force a single writer), the strategy is RollingUpdate, and a
PodDisruptionBudget keeps ≥1 pod serving auth through disruptions.

- **Rotate the signing key**: re-seal a new seed into the envelope (§4) and roll
  identity. In envMode there is no previous-key overlap, so a rotation
  invalidates JWTs signed by the old key → clients re-authenticate (and mesh
  nodes re-bootstrap on restart). The signing key is **JWT-only**; it does **not**
  encrypt stored secrets (those use `MEMQL_MASTER_KEY`), so rotating it never
  risks stored data.
- **Disk-key mode (local dev only)**: when `IDENTITY_SIGNING_KEY_B64` is unset,
  identity falls back to an on-disk keypair under `IDENTITY_KEY_DIR`
  (single-writer; not for HA).

---

## 6. Promotion gate (staging → prod)

A version is promotable to prod **only after it passes the deep staging smoke**,
and a validated artifact is **immutable**.

1. **Immutable tags** — `aks-deploy.sh` refuses to overwrite an existing engine
   tag in ACR (`ensure_tag_immutable`). `--allow-overwrite` exists only to
   re-cut an *unvalidated* tag. (ACR Basic enforces this at the script layer; an
   ACR **Premium** upgrade adds a registry-level immutability policy — recommended
   before prod.)
2. **Deep gate** — `SMOKE_PROFILE=deep` (§7) must pass; a token-less baseline
   gate is not a basis for promotion.
3. **Release lockfile** (deployment-v2 Phase 4, #702) — on a green deep gate, the
   8 component digests are assembled into `releases/<version>.yaml`
   (`scripts/release/assemble-lockfile.sh`) and PR'd; the `release-lockfile` CI
   gate (`coherence-check.sh`) enforces 8-digest-pinned + carrier/SPA
   coherence. This **supersedes** `deploy/validated-versions.json`.
4. **Prod promotes by digest copy, no rebuild** — `scripts/release/promote.sh
   --version=X --env=prod` copies the validated lockfile's digests into
   `deploy/k8s/overlays/prod`; PR it and Argo CD reconciles prod. Prod runs the
   exact bytes staging validated. See `releases/README.md`.

---

## 7. Deep smoke gate (`scripts/deploy/staging-smoke-test.sh`)

`SMOKE_PROFILE`:

- **baseline** (default): front-door reachability — TLS+DNS, identity health +
  JWKS (direct + app proxy), **`/readyz` schema assertion**, login page,
  `/memql/ws` + `/memql/audio` wiring, SPA boot assets, identity styling. A SKIP
  never fails the run.
- **deep** (the gate): all baseline checks **plus** a real authenticated WS query
  that fans BFF → cognition/agent. Every deep check **must run and PASS** — a
  missing input (no `MEMQL_SMOKE_TOKEN` / ws client) is a **FAIL, not a SKIP**.
  This is what makes the gate conclusive (the 0.9.6 incident went
  8-PASS/0-FAIL/2-SKIP green while the authenticated app was broken).

**Server-side readiness (`/readyz`, #657).** Every memql binary exposes an
unauthenticated `GET /readyz` that asserts critical schema presence via
`to_regclass` (the core `"MemoryNodes"` table + `automation_execution_claims`)
and returns 503 when an invariant is missing. The smoke `check_readiness` probes
it on the **identity host** (`identity.<env>.copresent.ai/readyz`) — identity
routes `/` to the identity pod, which is built from this repo and connects to
the shared memory-nodes DB; the app host only proxies `/memql*`+`/.well-known`,
so `/readyz` there hits the SPA catch-all (#680). It runs in every profile (no
token needed); in the deep gate a non-200 — or a 404 from a version that
predates the probe — is a hard FAIL. This proves a migration actually applied
WITHOUT DB credentials (the DB is firewalled to AKS egress) and is the runtime
counterpart to the §2c migrate gate (#671) — together they close the gap that
let #624 ship a broken schema behind a green deploy.

```bash
SMOKE_PROFILE=deep MEMQL_SMOKE_TOKEN=<pat-or-jwt> bash scripts/deploy/staging-smoke-test.sh
```

`aks-deploy.sh` runs the deep profile automatically when `MEMQL_SMOKE_TOKEN` is
in the deploy environment, and flags a token-less deploy as not promotable.

Deeper tier tracked as a follow-up: a headless-browser walkthrough +
console-error tier (#658).

### 7.1 Functional post-deploy gate + bff auto-promote (`scripts/deploy/post-deploy-gate.sh`, #1519/#1520)

The **authority** that stamps a release validated — run by `aks-deploy.sh`'s
health gate (and re-runnable standalone). It does two things a digest-drift
check structurally cannot:

**Promote the bff Rollout (#1520).** The bff blue/green Rollout is
`autoPromotionEnabled:false`, so after the new color is Ready it parks at
`BlueGreenPause` until promoted. The gate verifies the
`prePromotionAnalysis` (the in-cluster `deploy-gate` AnalysisRun) is **green**,
then runs `kubectl argo rollouts promote bff` — retrying for the documented
"may need promoting twice" case — and asserts `bff-active` flips to the release
color. **A failed analysis does NOT promote**: the old color stays active and
the deploy fails loudly (traffic never flips onto an un-gated color).

**Validate across three conditions (#1519)** — a release is **not** validated
until ALL pass:

1. **bff promoted** — `kubectl argo rollouts status bff` == `Healthy` **AND**
   the `bff-active` Service's `rollouts-pod-template-hash` selector ==
   the Rollout's stable (release) color. Catches the false-green where the
   Service still points at the **old** color (the 0.9.60 incident) — invisible
   to `rollout status` + `drift-check`.
2. **JWKS coherent** — polls the LB `/.well-known/jwks.json` `JWKS_POLLS` times
   (each may hit a different replica) **and** every `identity` pod directly
   (`kubectl exec` → its own `:8085` JWKS); **FAILS** if the served `kid` set
   diverges across replicas. Catches the silent per-pod-key divergence that
   broke ~50% of token verifications.
3. **functional auth** — a real authenticated round-trip that exercises
   **BFF → agent** forwarding. Preferred path: the in-cluster
   `class="service_account"` deploy-gate query (`deploy-gate` image +
   `deploy-gate-jwt` Secret, §6.x / `deploy/rollouts/README.md`) run as a
   one-shot Job against `bff-active` — the operator/CI path that needs no
   interactive login. Fallback: an authed query via `MEMQL_SMOKE_TOKEN` through
   the deep smoke profile. With neither credential it **fails loudly** (a gate
   that can't authenticate proves nothing); set `GATE_FUNCTIONAL_OPTIONAL=1`
   (implied by `--no-gate`) to downgrade only this condition to a warning when
   an operator runs a manual login round-trip out of band.

```bash
# Promote bff, then run the full functional gate (what aks-deploy.sh does):
bash scripts/deploy/post-deploy-gate.sh --env=staging
# Re-validate an already-promoted release without re-promoting:
bash scripts/deploy/post-deploy-gate.sh --gate-only --env=staging
# Just promote the bff Rollout:
bash scripts/deploy/post-deploy-gate.sh --promote-only --env=staging
```

A failure blocks the release: under the armed gate `aks-deploy.sh` stops and
prints the git-revert rollback procedure (§8). Idempotent — a promote on an
already-promoted Rollout is a no-op and the checks are pure reads. Tuning knobs:
`JWKS_POLLS`, `APP_HOST`/`IDENTITY_HOST`, `DEPLOY_GATE_IMAGE`,
`DEPLOY_GATE_JWT_SECRET`, `MEMQL_SMOKE_TOKEN`.

---

## 8. Zero-downtime + recovery

- **Rollout**: stateless nodes roll with RollingUpdate + graceful gRPC drain;
  identity is HA (§5) so auth stays up across a roll. The node lifecycle state
  machine, the SIGTERM/operator-triggered graceful drain
  (`MEMQL_SHUTDOWN_DRAIN_DELAY` / `MEMQL_SHUTDOWN_GRACE_PERIOD`), readiness≠liveness
  (`/livez` vs `/healthz`+`/readyz`), the ordered-rollout driver
  (`scripts/cluster/rolling-drain*`), and the green-before-staging-deploy parity
  gate are detailed in the [lifecycle runbook](./lifecycle-runbook.md).
- **Rollback = `git revert`** (deployment-v2 Phase 1, #699). The committed
  digest overlay is the only image authority, so a rollback reverts the bad
  overlay commit and reconciles: `make deploy-rollback ARGS=--to=<commit>`
  (`scripts/deploy/aks-rollback.sh`) prints the exact `git revert` + re-converge
  steps (`--apply` re-applies the overlay). Under Argo CD (Phase 2, #700) the
  revert push reconciles automatically. The old `kubectl rollout undo` path is
  retired — it reverted to the manifest tag, not the prior digest (#684).
- **Secret recovery**: the sealed envelope is in Key Vault (`kv-memql-<env>/
  memql-genesis-b64`); re-store it into `memql-secrets` and roll (§4).
- **DB**: managed Tiger Cloud (point-in-time recovery via Tiger). The DSN lives
  in `memql-secrets` / Key Vault.

---

## 9. Capacity (#614)

Staging runs nodepool `nodepool1` = 4× `Standard_B2s`. The pool handles the
current mesh (16 pods) but has thin headroom for rolling-update surge. Enable the
cluster autoscaler so a roll can surge without a scheduling deadlock.

The chosen sizing is **codified as IaC** in `scripts/deploy/aks-autoscaler.sh`
(`make deploy-autoscaler`) — an idempotent, declarative converge to the floor
below. It supports `--dry-run` (prints the plan, no Azure writes) and `--show`
(read-only state). The **live enable is owner-gated** (enabling the autoscaler
on shared cluster infra is a persistent cost decision), so the script defaults
to a plan-and-stop posture; the exact live command it converges to is:

```bash
az aks nodepool update -g rg-memql-staging --cluster-name aks-memql-staging \
    -n nodepool1 --enable-cluster-autoscaler --min-count 2 --max-count 5
```

> Sizing (min/max, SKU) is a cost decision. **Codified floor: min 2, max 5 on
> B2s** (the committed default in `aks-autoscaler.sh`; override per-call with
> `--min`/`--max`/`--nodepool` if a future right-sizing changes it).

A complementary **pre-deploy headroom guard** in `scripts/deploy/aks-deploy.sh`
(`check_nodepool_headroom`) runs before the mesh rolls: it sums the
rolling-update surge CPU (one maxSurge pod per Deployment × the per-node CPU
request) and compares it to the cluster's free allocatable CPU. It **warns** by
default (and points at `aks-autoscaler.sh`), or **fails** the deploy with
`--gate-headroom`; `--skip-headroom` opts out. Until the autoscaler is live this
is the belt-and-suspenders signal against the surge-deadlock that stalled 0.9.6.

---

## 10. Prerequisite (one-time, out-of-band)

`memql-secrets` carries real values and is created out of band — never committed:

```bash
kubectl create secret generic memql-secrets -n memql \
  --from-literal=MEMQL_MASTER_KEY="$MEMQL_MASTER_KEY" \
  --from-literal=MEMQL_GENESIS_B64="$(base64 < /tmp/<env>.genesis.znas)" \
  --from-literal=MEMORY_NODES_DATABASE_DSN="$(tiger db connection-string <id> --with-password)"
```

See also `deploy/k8s/README.md` (manifest-level reference) and
`deploy/k8s/README-public-entry.md` (ingress-nginx + cert-manager + internal TLS).
