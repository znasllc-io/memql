# Platform consolidation: a product-agnostic engine + DSL-bundle products

**Status:** proposed — gated on spike [#2473](https://github.com/znasllc-io/memql/issues/2473) · **Epic:** [#2472](https://github.com/znasllc-io/memql/issues/2472) · **Issue:** [#2474](https://github.com/znasllc-io/memql/issues/2474)

This ADR records the target architecture the owner decided on 2026-07-08 to
collapse the per-product complexity of the memQL ecosystem. It is **proposed**:
the load-bearing assumption (runtime DSL delivery on the AI nodes) is validated
by spike #2473 before this flips to *accepted* or drives a revision.

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
(`component/memql/dslfs`). Delivering product DSL at runtime returns
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
(`cockpit.<domain>`), no product DSL bundle. The bespoke `cockpit-bff` module
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

1. **Spike (#2473):** prove runtime DSL on the AI nodes (single-node + 2-replica);
   document the pure-DSL vs needs-product-Go boundary against the reference product's
   constructs. **Gates the rest.**
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
