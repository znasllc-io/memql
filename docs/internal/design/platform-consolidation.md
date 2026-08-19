---
title: Platform consolidation: a product-agnostic engine + DSL-bundle products
audience: internal
status: historical
area: design
sinceVersion: 0.12.0
owner: znas
---

# Platform consolidation: a product-agnostic engine + DSL-bundle products

**Status:** accepted — validated by spike [#2473](https://github.com/znasllc-io/memql/issues/2473) (2026-07-08) · **Epic:** [#2472](https://github.com/znasllc-io/memql/issues/2472) · **Issue:** [#2474](https://github.com/znasllc-io/memql/issues/2474)

This ADR records the target architecture the owner decided on 2026-07-08 to
collapse the per-product complexity of the memQL ecosystem. The load-bearing
assumption — that a mesh node with **no compiled-in product DSL**, loading it at
boot from `MEMQL_DSL_PATH`, routes and runs a turn identically to a
statically-linked node — was **validated by spike #2473** on the live local
cluster (single-node and 2-replica), so this is now **accepted**. See the spike
for the go/no-go note, the runtime-mount mechanism, and the pure-DSL vs
needs-product-Go boundary mapped against the reference product's constructs.

### Spike outcome (what the platform work must carry forward)

- **Runtime delivery reuses the existing `dsl.RegisterTree` overlay** — the plain
  engine loaded the full reference-product bundle from a runtime volume with zero
  skipped constructs, and a bad bundle fails loud under the strict-boot gate. The
  only new code is a boot hook that mounts each `MEMQL_DSL_PATH/<domain>` via
  `RegisterTree(os.DirFS(...))` *before the first `dsl.Tree()` walk*. Today the
  `core/dslfs` `MEMQL_DSL_PATH` override is per-construct-type and is
  **not** wired into the real load path (`unified_loader.go` walks `dsl.Tree()`);
  the engine-platform step must add the per-domain boot mount.
- **The AI critical path invokes zero product-Go executors.** Its one pack
  dependency is the agent-reply *prompt DSL* (pure-DSL, core provider), which
  runtime delivery supplies. Every product-Go integration the product DSL
  references (chat / daily-space / avatar / training) is **call-time only** and
  off the basic route+turn path — clean "absorb into the engine" work (Decision 2),
  no blocker.
- **Guardrail:** an unregistered `integration.*` executor is silent at load and
  invisible to strict-boot; it errors only at call time. A *partial* Go absorption
  will boot green and fail on the un-absorbed tool. The platform must either absorb
  all referenced integrations or add a boot completeness check mapping every
  mounted-pack `@executor("integration.*")` to a registered integration.

## Context — what feels too complicated today

A running product is a constellation of repos + images:

- **Two repos per product**: a Go **carrier/BFF** (e.g. a `<product>-carrier` repo)
  that owns the product DSL + Go integrations + the whole deploy/release estate,
  and a separate **client** SPA (e.g. a Vite/React SPA, not a Go module).
  Plus the shared engine (`memql`) and the cockpit — four checkouts, tied by a
  workspace-level `go.work`.
- **~9 images per release**: 3 engine-built (identity, mcp, voice) + **5
  carrier-built (bff, cognition, agent, planner, workbench)** + the SPA.

The **root cause of the deploy/topology weight is one fact**: product DSL is
**statically linked into the engine at compile time** (`RegisterTree`). The five
carrier images are compiled from *one identical* product-DSL tree and differ
only by a build tag. That single fact cascades into a carrier-base Docker stage,
a 5-way build matrix, three build workflows across three repos that must run at a
coordinated version, an `assemble-lockfile` step reconciling 9 digests with a
`builtAgainstEngine` coherence check, **~79 hand-tracked release lockfiles**, a
strict **engine → carrier → SPA three-repo landing order** for any wire change,
and staging overlays that compose the engine base *remotely* and restore
hostnames through a brittle index-pinned JSON patch ladder.

So the pain is not "two repos" — it is **a fleet of product node-images and a
cross-repo release dance**, both downstream of static DSL linking.

## Target architecture

- **The engine is the whole platform.** Every node type (identity, bff,
  cognition, agent, planner, voice, workbench, mcp) ships as a
  **product-agnostic engine image**. Every reusable capability (avatar/LiveKit,
  chat delivery, training, daily-space, …) lives in the engine as a **generic,
  DSL-configurable feature** — never product-specific code. The engine stays one
  separate, public, product-agnostic repo.
- **A product = a DSL bundle + a client.** No carrier repo, no product Go, and
  no per-product node-images in the common case.
- **DSL is delivered at runtime.** The product DSL ships as a **tiny data-only
  bundle image**; an **init-container** copies it into a shared volume the mesh
  nodes read via `MEMQL_DSL_PATH` at boot.
- **A "BFF" is an engine `bff` node** fronting a product (mounting its bundle).
  A deploy concern, not code. Genuinely-bespoke product Go (rare) becomes a thin
  optional `bff/` plugin module in the product repo.
- **A release is `{engine version, bundle digest, client digest}`** pinned in one
  overlay in one repo. No cross-repo assembly, no coherence check, no lockfile
  fleet.

```
  product-agnostic engine mesh (all engine-built images)
        ▲                    ▲
        │ mounts             │ mounts
   DSL bundle (product A)   DSL bundle (product B)   ← tiny data images
        │                    │
     bff node (A)          bff node (B)              ← plain engine bff + bundle
        │                    │
     client A (SPA)        client B (SPA)            ← the only per-product code
```

## The six decisions

### 1. DSL delivery → runtime bundle (not compile-time linking)
The only reason the mesh isn't product-agnostic today is compile-time DSL
linking. The engine already ships a runtime `MEMQL_DSL_PATH` override
(`core/dslfs`). Delivering product DSL at runtime returns
cognition/agent/planner/workbench to product-agnostic engine images and collapses
the product's node-image count toward one. *Alternative rejected:* keep
compile-time linking — leaves the 5-image fleet + release choreography intact.

### 2. Product Go → absorb generic capabilities into the engine
Go can't be runtime-delivered like DSL; it compiles into the node that runs it.
But most "product Go" (avatar, chat, training, daily-space) isn't product-*
specific* — it's generic capability registered under a product namespace, and
some is node-bound (the avatar runs on the *voice* node's media plane and can't
move to a BFF). So these are **re-homed into the engine as generic,
DSL-configurable features** — literally "collapse everything into the engine."
A product then has *no* Go in the common case; only one-of-a-kind Go remains, as
a thin BFF plugin. *Guardrail:* they must land as **generic** capabilities any
product configures, never product-specific, to keep the engine agnostic.
*Alternatives rejected:* concentrate all product Go in a "fat BFF" (node-bound Go
like the avatar doesn't fit); keep Go per-node (no collapse).

### 3. Product repo → DSL-first
Because a product is mostly DSL (1) with its generic Go in the engine (2), the
common product has **no Go at all** — so **no `go.work` and no Go module**. The
repo is the DSL bundle + the client + deploy config; the "BFF" is a deploy
concern. The `require`/`replace`/`go.work` engine-dependency footgun simply does
not exist for a pure-DSL product. A `go.work` + a thin `bff/` module appears only
when a product invents bespoke Go. *Alternative rejected:* always carry a Go BFF
module + engine dependency per repo, even when nothing product-specific compiles.

### 4. Bundle delivery → data-only image + init-container
The product DSL is packaged as a tiny data-only image (`FROM scratch` + the
`.memql` files); an init-container copies it into a shared volume the engine
nodes read at `MEMQL_DSL_PATH`. The product's entire "build" becomes *packaging
text* (seconds, no Go, no carrier-base, no matrix), digest-pinnable, so a release
is `{engine version, bundle digest, client digest}` in one overlay/repo. This is
what dissolves the ~79 lockfiles + cross-repo coherence. *Alternatives rejected:*
a ConfigMap (caps at ~1 MB; a real DSL tree exceeds it); a bundle service fetched
at boot (a runtime dependency on node startup). *Not chosen:* collapsing the
engine into one runtime-node-type-selectable mega-image — bigger, riskier, and it
loses the build-tag binary-size wins.

### 5. Cockpit → ops-console variant; drop the module
The cockpit is a thick Go TUI and is product-neutral (reads only engine/cluster
concepts). Its "BFF" is just a plain engine `bff` image deployed as its edge
(`api.<domain>`), no product DSL bundle. The bespoke `cockpit-bff` module
([memql-cockpit#289](https://github.com/znasllc-io/memql-cockpit/issues/289)) —
a workaround for the engine not shipping a `bff` image (#2204) — is **deleted**
once the engine ships a product-neutral `bff` again. The connection flip (#291)
and deploy-controls rewire (#292) remain valid.

### 6. Sequencing → de-risking spike first
Everything above is known-doable engineering *except* one load-bearing
assumption: that a mesh node with no compiled-in product DSL, loading it from
`MEMQL_DSL_PATH` at boot, behaves identically to a statically-linked one —
specifically on the AI nodes (cognition/agent). Spike #2473 validates that on a
local cluster **before** any platform work. *Alternatives rejected:* build the
engine platform first (bets the biggest work on the unvalidated assumption);
cockpit-first pilot (proves plumbing, not the linchpin — the cockpit is
product-neutral).

## Migration sequence (gated on the spike)

1. **Spike (#2473): DONE — validated 2026-07-08.** Runtime DSL proved on the AI
   nodes (single-node + 2-replica); the pure-DSL vs needs-product-Go boundary is
   documented against the reference product's constructs. Gated the rest; gate open.
2. **Engine platform:** ship every node type as a product-agnostic image (re-add
   `bff`); wire the init-container/bundle mechanism into `deploy/k8s/base`; make
   the mesh load runtime DSL, fail-loud on a bad bundle.
3. **Absorb generic capabilities** (incremental): re-home the reference product's Go
   integrations as generic engine features, each removing product Go.
4. **Template redesign** (`memql-project`): stamp a DSL-first product repo.
5. **Reference product 2→1:** consolidate the product's carrier + client repos into one
   DSL-first repo — the end-to-end proof.
6. **Cockpit cleanup:** delete `cockpit-bff`; deploy the engine bff.

## Non-goals / guardrails

- Engine stays one separate, public, product-agnostic repo (the "second product
  boots with zero engine edits" bar); not vendored into product repos.
- Absorbed capabilities are generic (DSL-configured), never product-specific.
- No change to the build-server → OIDC → ACR image rule; bundle/client images are
  just cheaper artifacts.
- Node-type images stay build-tag-separated (smaller binaries); the engine is not
  collapsed into one runtime-selectable mega-image.

## Supersedes

The centerpiece of the per-client-BFF epic
([memql-cockpit#288](https://github.com/znasllc-io/memql-cockpit/issues/288)):
the `cockpit-bff` module (#289) is deleted under Decision 5. The connection flip
(#291) and deploy-controls rewire (#292) stand.

---

## Amendment (memql#3721): the per-customer cluster

**Added 2026-08-13.** This ADR settles how a *product* relates to the engine. It
says nothing about how a *customer* does, and the front-door design
([memql#3700](https://github.com/znasllc-io/memql/issues/3700)) had to decide
that. Two decisions came out of it. Both are applications of the target
architecture above rather than changes to it, so they are recorded here rather
than in a new ADR.

The decisions and their rejected alternatives live in
`docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`, D1 and D12.
This section records what they mean for the repo topology this ADR governs, and
does not restate their arguments.

### Isolation is the cluster: one memQL cluster per customer

A customer gets their own cluster, one domain, and as many sites, apps and
products as they want inside it. Not a shared cluster with account-scoped rows.

D1 argues this from two places. The measured half is
[`account-isolation-model.md`](account-isolation-model.md) (ACCEPTED,
memql#3321): §5.2 records that `actor.*` carries **no tenancy dimension**, so
every account-scoped filter can only compare a payload field against a
caller-supplied arg, and §6(b) — a resolved account set on `AccessContext` — is
named the load-bearing item and is not built.

The decisive half is independent of all of that: **you can hand over a cluster;
you cannot hand over a tenant.** "We build it, then give it to them if they want
to run it themselves" is a product promise no shared-tenancy design can keep,
and no amount of §6(b) work changes that.

`v1:identity:account` is therefore **parked, not removed** (D12). Leaving it is
safe as a measured property rather than a hope: §3.4 of that document records
that the credential *"authorizes nothing today, and that is a checked property
rather than an aspiration"* — no verifier resolves the `mql_acct_` prefix, no
interceptor admits it, `dsl/identity` declares no by-`keyHash` lookup for the
family, and both absences are pinned by tests in
`component/identity/accounttoken`. Removal is a separate decision available
later at no penalty, precisely because the thing is inert.

> D12 cites that passage as §3.3, which is "Why an account has no login". The
> sentence is in §3.4, "What an 'account token' therefore is". Same document,
> same claim, one section along.

The accepted cost is fleet-management complexity across many clusters instead of
tenancy-isolation complexity inside one. Deliberate, and named in D1 as a real
cost rather than a free win.

### A customer repo does not fork the engine

`memql-<customer>` holds the DSL bundle, the SPAs, the websites and the deploy
overlay, and consumes the engine as **pinned images**. One repo per customer,
everything of theirs inside it, fully handoverable.

**What it must not be is a copy of the platform.** That is the carrier model
this ADR already supersedes, and the Context section above records what it cost:
~9 images per release, a 5-way build matrix, three build workflows across three
repos that must run at a coordinated version, an `assemble-lockfile` step
reconciling 9 digests with a `builtAgainstEngine` coherence check, ~79
hand-tracked release lockfiles, and a strict engine → carrier → SPA three-repo
landing order for any wire change.

Per customer that is strictly worse than it was per product, and it acquires a
failure mode the per-product version did not have: **a CVE in the engine means
patching N forks.** Every one of them diverges from the day it is cut, which is
the point at which "patch them all" stops being a batch operation.

The line worth writing down:

> **The repo holds the cluster's definition, not the cluster's code.**

A release stays `{engine version, bundle digest, client digest}` pinned in one
overlay in one repo — the same release shape Decision 4 establishes. A customer
repo adds websites and possibly several SPAs to what it pins; it does not add
node images.

### Why this needed recording

"A monorepo per customer that is a copy of the platform" is the natural thing to
reach for, and it was reached for during the front-door design conversation. The
Context and Target-architecture sections above supersede it *in the abstract*,
but neither mentions the per-customer case — so a reader can arrive at the fork
honestly, from this document, and be wrong. That is what this amendment is for,
and it is why it is an amendment rather than an ADR of its own: nothing above it
changes.

**Out of scope here:** the `memql-project` template lives outside this
repository and needs its own update to stamp a customer-shaped repo (the DSL
bundle plus one or more clients plus the deploy overlay, and no engine fork).
That is tracked with the template, not here.
