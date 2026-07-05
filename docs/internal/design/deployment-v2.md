---
title: Deployment v2 — GitOps + progressive delivery (RFC)
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Deployment v2 — GitOps + progressive delivery (RFC)

**Status:** DRAFT — pending owner sign-off (epic znasllc-io/memql#697, Phase 0 #698)
**Author:** Platform/Staff (ZNAS LLC)
**Date:** 2026-06-02
**Supersedes:** the imperative `scripts/deploy/aks-deploy.sh` flow (kept until Phase 3 cuts over)
**Scope:** memQL engine mesh (6 node-types) + the downstream product carrier + the product SPA, on AKS `aks-memql-staging`, proven in staging ahead of a prod cutover.

---

## 0. TL;DR

We replace a workstation-run shell script that *imperatively mutates the cluster*
with a **declarative, git-reconciled, digest-pinned, analysis-gated** pipeline:

1. **Git is the only source of truth.** A reconciler (**Argo CD**) converges the
   cluster to the committed state. Humans and scripts never `kubectl set image`,
   never `rollout undo`, never `kubectl patch secret`.
2. **Every image is pinned by `@sha256:` digest** in a per-env kustomize `images:`
   overlay. There is exactly one place a deployed artifact is named.
3. **Artifacts are built once in CI** and **promoted by digest** — staging→prod
   copies the *same* validated digests; no rebuild, environments differ only by
   config.
4. **Rollouts are progressive** (Argo Rollouts): **blue/green for the BFF**
   (reusing #616/#675), **canary for engine node-types**. The gate is a set of
   **in-cluster, convergence-safe `AnalysisTemplate`s** that **auto-abort and
   auto-rollback**.
5. **Synthetic checks run inside the mesh** against service DNS, authenticated by
   a **designed `class="service_account"` JWT** (resolves #691).
6. **External Secrets Operator** pulls cluster config/secrets from **Key Vault**.
7. **Rollback = `git revert`**; the reconciler + Rollout abort do the rest.

Everything below justifies each choice, maps it to the concrete failure modes we
hit shipping 0.9.9, and lays out the migration path and rollback story.

---

## 1. Why the current process is structurally fragile

`scripts/deploy/aks-deploy.sh` (≈33 KB) is a competent script, but its
*architecture* makes a class of failures **possible by construction**:

| Property today | Consequence |
|---|---|
| Image authority is **split**: base manifests carry a mutable tag (`memql-*:0.9.9`), and the *live* tag is set at deploy time by `kubectl set image` (`aks-deploy.sh:468`) **after** `kubectl apply -k` (:501). | The manifest and the cluster can disagree. `--skip-build` returns before the pin → the manifest tag silently wins. (**#684**) |
| Rollback is `kubectl rollout undo` (`:567`). | Undo targets the ReplicaSet the `apply -k` created = the **manifest tag**, not the pre-deploy version. A failed deploy "rolls back" to the wrong thing. (**#684**) |
| Tags are **mutable** (ACR Basic); immutability is enforced only in the script. | A re-cut tag changes what a name points to; "0.9.6" meant two different image sets. |
| The gate is a **bespoke shell script** (`staging-smoke-test.sh`, ≈25 KB) that curls **public hosts** from a workstation. | It is coupled to ingress routing (hit the SPA catch-all instead of identity — **#680**) and to rollout timing (raced mixed-version convergence — **#682**), and it depends on the firewall letting the workstation reach the front door. |
| Automation auth was **never designed**. The per-node verifier is built with a **nil PATVerifier** (`app/config.go:97`); PATs verify only on the identity node (`app/integrations_identity.go:315`). | The deep authenticated check can't actually authenticate on the BFF surface. (**#691**) The mint tooling grew bugs (**#686**). |
| The whole flow is **imperative and runs from a laptop**. | No continuous reconciliation, no drift correction, releases aren't atomic across the 3 repos, and "what's deployed" lives in a human's shell history. |

The fix is not to patch each symptom — it's to remove the architecture that lets
them exist. §7 is the per-failure-mode "why this is now impossible" table.

What already works and is **reused as an input, not re-implemented**: the
migrate-fatal gate (**#671**), the `/readyz` schema probe (**#657**), the live
cluster autoscaler (**#614**), graceful gRPC drain (**#615/#552**), and the
blue/green BFF code (**#616/#675**, merged).

---

## 2. Target architecture

```
 Developer merges a digest-pinned overlay change to git
                         │
              ┌──────────▼───────────┐
              │      Argo CD          │   (reconciler; the ONLY actor that
              │   app-of-apps         │    mutates the cluster)
              └──────────┬───────────┘
                         │ renders kustomize, applies
        ┌────────────────▼─────────────────┐
        │  Argo Rollouts                    │
        │   • BFF: blue/green (#616)        │
        │   • engine node-types: canary     │
        └───────┬───────────────┬───────────┘
                │ each step gated by
        ┌───────▼───────┐  ┌────▼─────────────────────┐
        │ AnalysisTemplate │ in-cluster synthetic checks:
        │  (auto-abort +   │  • /readyz schema assert (#657)
        │   auto-rollback) │  • authenticated query via
        └──────────────────┘    class="service_account" JWT (#691)
                                • SLO metrics: err-rate, p95,
                                  active-stream drop (#616 counter)

 Secrets:  Key Vault ──(External Secrets Operator)──► k8s Secret
 Releases: CI emits per-component @sha256 ──► releases/<v>.yaml lockfile
           staging→prod = copy validated digests (no rebuild)
 Rollback: git revert  ──► reconciler converges back  (+ Rollout abort)
```

### Eight components, two repos, one release

| Component | Repo | Build |
|---|---|---|
| `memql-identity`, `memql-agent`, `memql-cognition`, `memql-planner`, `memql-voice`, `memql-workbench` | `znasllc-io/memql` | per-node `BUILD_TAGS` off the root `Dockerfile` (voice is the CGO exception) |
| the product carrier (bff) | the product org's carrier repo | built against a pinned engine version |
| the product SPA | the product org's SPA repo | `docker buildx` with `node_auth_token` + `VITE_*` bootstrap args |

A **release** is the set of 8 digests that were validated together. The lockfile
(§5) is the atomic unit; promotion moves a lockfile, never a tag.

---

## 3. Tooling decisions (with justification)

### 3.1 Reconciler: **Argo CD** (vs Flux)

**Decision: Argo CD.**

| Criterion | Argo CD | Flux |
|---|---|---|
| Progressive delivery | **First-party Argo Rollouts** — same project, native `Rollout` CRD, the analysis/abort model we need for blue-green + canary. | Flagger (separate project) layered on Deployments; works, but a second integration seam. |
| Operator visibility | Web UI + CLI showing live-vs-desired diff, sync status, and **drift** at a glance — directly useful for an "is staging what git says?" answer and for break-glass. | CLI/GitOps-Toolkit centric; less of a single operator pane. |
| App-of-apps | Native, well-trodden for "one namespace, many resources." | Kustomization CRDs compose similarly. |
| Footprint on a 4×B2s pool | Heavier control plane than Flux. **Accepted** — staging has the autoscaler (#614) and the operability win dominates. | Lighter. |

The deciding factor is **Rollouts**: our hardest requirement (zero dropped
streams via blue/green BFF + analysis-gated canary) is an Argo-native capability,
and #616/#675 already speak the blue/green vocabulary. Choosing Argo CD keeps the
reconciler and the progressive-delivery engine in one ecosystem. Flux+Flagger
would work but adds a cross-project seam for no benefit we need.

**Trade-off accepted:** Argo CD's control plane is heavier than Flux on a small
pool. Mitigation: the cluster autoscaler is live (min 2 / max 5), and Argo CD runs
comfortably within that envelope; we right-size its requests in Phase 2.

### 3.2 Progressive delivery: **Argo Rollouts**

- **BFF → blue/green.** Reuse the #616/#675 design: connected users stay on their
  color until they disconnect; new logins land on the new color; old color drains
  to 0 `activeStreams` then tears down. We port the existing
  `deploy/k8s/bff-bluegreen.yaml` + cutover logic into a `Rollout` with a
  blue/green strategy whose `prePromotionAnalysis` is the gate.
- **Engine node-types → canary.** Stateless behind the BFF; a canary step
  (e.g. 25%→50%→100% with analysis between steps) is the right shape and bounds
  blast radius on the B2s pool.
- **Gate = `AnalysisTemplate`.** Auto-abort on a failed analysis = automatic
  rollback to the stable ReplicaSet — no `rollout undo`, no script.

### 3.3 Secrets: **External Secrets Operator ← Key Vault**

ESO syncs from `kv-memql-<env>` into k8s Secrets declaratively. This retires the
hand-edited / `kubectl patch secret` flow for **cluster-facing** material. The
**genesis A2 envelope keeps its role** as the app-internal shared-secret bootstrap
(it is an application concern — NaCl secretbox under `MEMQL_MASTER_KEY`, autoloaded
at boot); ESO owns the *k8s Secret* that carries `MEMQL_MASTER_KEY`,
`MEMQL_GENESIS_B64`, and the DSN, so the cluster's secret state becomes
declarative and reconciled instead of hand-applied. (Detailed in Phase 5 #703.)

### 3.4 Automation auth (resolves #691): **`class="service_account"` JWT**

**Decision (owner-approved, auth authority Jose): a scoped, short-lived
`class="service_account"` JWT — NOT wiring the PATVerifier on API nodes.**

Why this over wiring a `pat.Verifier` into the BFF:

- The per-node verifier **already** validates identity-issued JWTs via JWKS with
  **no DB lookup** (`verifier.New(cfg, cache, /*PAT*/nil, logger)` —
  `app/config.go:97`). The `nil` is deliberate: it keeps token verification
  decoupled from a DB round-trip on every API node. A service-account JWT rides
  this existing path with **zero new code on the verify side**.
- There is direct precedent: **`class="voice_agent"`** JWTs are minted by
  `JWTIssuer.IssueVoiceAgentAccessToken` (`component/identity/jwt.go:386`,
  `ClassVoiceAgent = "voice_agent"` at :96) and **pinned to a message surface by
  an interceptor** (`component/grpc/voice_agent_stream_interceptor.go`). We add a
  sibling `ClassServiceAccount = "service_account"` + `IssueServiceAccountAccessToken`
  + a surface-pinning interceptor.
- A **machine identity** is the correct primitive for a synthetic check (and for
  future automation). It is short-lived, rotatable, and scoped to exactly the
  query surface the gate exercises. **PATs stay the human-CLI credential**, and we
  do **not** broaden where a long-lived static user credential is honored across
  the mesh.

**Design sketch (implemented in Phase 3 #701):**

```
identity service                       BFF (and mesh) — UNCHANGED verify path
  IssueServiceAccountAccessToken  ──►   verifier.New(cfg, cache, nil, log)
   class="service_account"               JWKS validates the JWT (no DB)
   scope: smoke query surface            service_account_stream_interceptor
   ttl: short (e.g. 15m), rotatable        pins class=service_account to the
                                            authenticated-query message set

In-cluster AnalysisTemplate (Phase 3):
  env: MEMQL_SVC_JWT  ← k8s Secret (ESO-synced or minted by an identity Job)
  step: open authenticated WS/gRPC to bff:50058 → cognition/agent, assert result
```

Identity backing: the `v1:identity:identity` concept already models a
**service-account** credential kind, so issuing/auditing a service-account token
fits the existing identity model. The token is delivered to the gate as a k8s
Secret (minted by a short-lived identity-side mechanism; rotation cadence and
exact mint surface are finalized in Phase 3, with #686's `pat` CLI fixes informing
a clean mint command).

**Trade-off:** we add a new token class + interceptor + a mint/rotation path.
Accepted — it is small, mirrors a shipped pattern, and is the only option that
keeps the hot verify path DB-free and the human/machine credential split clean.

---

## 4. CI-built artifacts + digest discipline

Today images are built by `az acr build` *from the deploy script on a laptop*.
v2 moves the build into **CI in each repo** and makes the digest the currency:

- Each repo's CI builds its image(s), pushes to ACR, and **emits the resulting
  `@sha256:` digest** as a build output/artifact.
- **Tags become immutable** at the registry level (ACR **Premium** immutability
  policy) — the script-level `ensure_tag_immutable` guard becomes a registry
  invariant. A tag is a human label; the **digest** is the contract.
- No deploy step ever builds. Deploy = reference an already-built digest.

This is the precondition for atomic, rebuild-free promotion (§5) and for the
drift detector (Phase 1): "is the cluster running the digest git says?" is only a
meaningful question once the digest is the authority.

---

## 5. Release lockfile + staging→prod promotion

**`releases/<version>.yaml`** pins all 8 components by digest:

```yaml
# releases/0.10.0.yaml  (illustrative)
version: 0.10.0
validatedAt: 2026-06-10T00:00:00Z
gate: deep            # the analysis suite that validated this set
components:
  memql-identity:   { repo: memql,            digest: sha256:... }
  memql-agent:      { repo: memql,            digest: sha256:... }
  memql-cognition:  { repo: memql,            digest: sha256:... }
  memql-planner:    { repo: memql,            digest: sha256:... }
  memql-voice:      { repo: memql,            digest: sha256:... }
  memql-workbench:  { repo: memql,            digest: sha256:... }
  product-carrier:  { repo: carrier,  digest: sha256:..., builtAgainstEngine: 0.10.0 }
  product-spa:      { repo: spa,      digest: sha256:..., builtAgainstEngine: 0.10.0 }
```

- **Assembly:** each repo's CI publishes its digest; an assembly workflow opens
  the lockfile PR. Merging the lockfile (into the staging overlay) is the deploy.
- **Promotion is a digest copy.** staging→prod copies the validated digests into
  the prod overlay. **No rebuild** — prod runs the exact bytes staging validated.
- **Cross-repo coherence is enforced in CI:** the carrier and SPA each record the
  engine version they were built against; the assembly step **fails** an
  incoherent set (carrier built against a different engine than the lockfile
  declares). This replaces the implicit "remember to rebuild the BFF too."
- This **supersedes `deploy/validated-versions.json`** (which carried the same
  intent — validated version + per-engine digests — but was written by the
  imperative script). The ledger's semantics live in the lockfile + git history.

(Built in Phase 4 #702.)

---

## 6. Migration path from today's script

Incremental, each step staging-proven, no flag day:

1. **Phase 1 (#699):** introduce the per-env kustomize `images:` digest overlay;
   delete `set image` / `rollout undo` from `aks-deploy.sh`; add the drift
   detector. The script still *applies*, but now from a single digest source.
   Rollback becomes `git revert` immediately. *(This alone kills #684.)*
2. **Phase 2 (#700):** install Argo CD; point it at the overlay. Deploys become
   merges. `aks-deploy.sh` is now redundant for applying and is reduced to (at
   most) a thin build/mint helper, or retired.
3. **Phase 3 (#701):** wrap BFF in a blue/green `Rollout` (port #616), engine in
   canary; replace `staging-smoke-test.sh` with `AnalysisTemplate`s; wire the
   service-account JWT. The gate is now in-cluster + convergence-safe.
4. **Phase 4 (#702):** lockfile + digest promotion + coherence CI.
5. **Phase 5 (#703):** ESO ← Key Vault; rehearse rollback + DB PITR.

The legacy script is removed when Phase 3 lands (the reconciler + Rollouts own
the deploy). Until then it co-exists, reading the same digest overlay so the two
paths can't disagree.

**Break-glass (documented in Phase 2):** suspend Argo auto-sync, perform a manual
sync or a direct `kubectl apply` of the committed overlay, then re-enable sync.
The cluster still converges to git afterward, so break-glass can't cause silent
drift.

---

## 7. Failure modes → why each is now structurally impossible

| Failure mode | Root architecture cause | v2 structural fix | Phase |
|---|---|---|---|
| **#684** manifest tag ≠ live image; `--skip-build` skips pin; rollback reverts to manifest tag | Split, mutable image authority + imperative pin + `rollout undo` | Single **digest** authority in the overlay; **no** runtime `set image`; **no** `rollout undo` (rollback = git revert); reconciler self-heals drift; registry-level tag immutability | 1, 2, 4 |
| **#680** smoke hit the wrong host (SPA catch-all) | Gate curls **public hosts**, coupled to ingress routing | Checks run **in-cluster** against service DNS — no public-host routing involved | 3 |
| **#682** smoke raced rolling-update convergence | Gate polls the **front door** during a mixed-version window | Gate is a Rollout **`AnalysisTemplate`** — convergence-native, evaluated per Rollout step, not against a load-balanced public endpoint | 3 |
| **#691** PAT rejected on BFF (nil PATVerifier) | Automation auth never designed; verify path is DB-decoupled by intent | Designed **`class="service_account"` JWT** validated by the existing JWKS verifier; surface-pinned by interceptor; PAT stays human-only | 0 (design), 3 (impl) |
| **#686** bespoke `pat` CLI bugs | Hand-rolled credential tooling on the critical path | Gate uses the designed service identity (not the `pat` CLI); the credential path is a tested k8s artifact, not a one-off command | 3 |
| Bespoke untested gate; firewall-coupled | A 25 KB shell script outside the cluster | Gate is **declarative, tested** k8s `AnalysisTemplate`s running inside the mesh | 3 |
| Imperative drift; non-atomic cross-repo release | Laptop-run script; 3 independent artifacts | **Reconciled** from git; **atomic digest lockfile** across all 8 components with coherence enforcement | 2, 4 |

---

## 8. Definition of Done (epic-level, restated)

- Zero dropped user streams across any rollout — **proven in staging** with a
  sustained WS client held through a full deploy (the #616 `activeStreams`
  counter is the evidence).
- Every failure mode in §7 structurally impossible (per-phase explanation above).
- Declarative + reconciled delivery (Argo CD); digest-based staging→prod
  promotion (no rebuild); analysis-gated auto-rollback; rehearsed DR.
- Then: a validated **prod cutover**.

---

## 9. Open items for owner sign-off

These are the calls that should be explicitly confirmed before Phase 1 starts:

1. **Reconciler = Argo CD** (vs Flux) — §3.1. Heavier control plane accepted for
   the Rollouts win + operator visibility.
2. **Auth = `class="service_account"` JWT** — §3.4. **Already approved** in the
   Phase 0 discussion; recorded here + on #691.
3. **ACR Premium** for registry-level tag immutability (§4) — a cost decision
   (ACR tier upgrade). Recommended before prod; can stay script-enforced on
   staging in the interim.
4. **Argo CD scope** — staging-only first, prod added at cutover (recommended), vs
   one Argo managing both from day one.
5. **Genesis envelope boundary** (§3.3) — confirm ESO owns the *k8s Secret*
   material while the A2 envelope stays the *app-internal* shared-secret bootstrap
   (no change to `component/secret/` / `component/genesis/`).

On approval, this RFC merges and Phase 1 (#699) begins.
