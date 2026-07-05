# Platform Compatibility

This is the **hub document** for how the memQL platform repos fit
together at a given point in time. memQL is the upstream engine, so
this doc lives here; downstream repos link to it.

It answers one question: **given a version of one repo, what versions
of the others does it work with?** The answer is a **pin chain**, not
a single global version number — repos version independently
(see [VERSIONING.md](VERSIONING.md)) and coherence is maintained by
explicit pins rather than lockstep.

## The repo shape

Every product built on memQL follows the same constellation (see
[docs/public/operate/downstream-stacks.md](docs/public/operate/downstream-stacks.md)):

| Repo | Role |
|---|---|
| `znasllc-io/memql` (this repo) | The engine + node-type binaries (bff / voice / cognition / agent / planner / workbench / mcp / identity) and the wire protocol. **Upstream.** |
| the product's **carrier** repo | Imports memQL's Go packages, registers its DSL subtree + product integrations through the plugin-SDK seams, and builds the deployable backend (carrier) images. Owns the product's deploy/release estate. |
| the product's **frontend** repo | The SPA and its client-side deploy config. |
| `znasllc-io/memql-cockpit` | The **CLI / ops console** (display name "memQL Cockpit"), and the worker run-mode binary. A gRPC client of memQL. |

The concrete repo names, versions, and pin values for a given product
live in that product's carrier repo (its VERSIONING/COMPATIBILITY
docs), not here.

## The pin chain

The dependency graph points **downward toward memQL**, and so does the
pinning:

```
  product frontend
      │  deploy/backend-version pins ──►
      ▼
  product carrier  (BFF carrier)
      │  go.mod require + image built from ──►
      ▼
  memql  (engine + protocol)          ◄── memql-cockpit declares the
                                          min memQL / protocol it speaks
```

1. **frontend → carrier.** The frontend's `deploy/backend-version`
   file pins the **carrier** version it deploys against. This is the
   one number the product team bumps to move to a newer backend.

2. **carrier → memql.** The carrier imports memQL's Go packages
   (`app`, `server`, `genesis`, `core/...`) and mounts its own DSL
   subtree via `dsl.RegisterTree`. It pins the memQL version it
   builds against and bakes it into the immutable backend image.
   Because the import graph is **carrier → memQL** (never the
   reverse), memQL carries no require on any carrier — see the product
   pack repo's docs/operate/deployment-strategy.md.

3. **memql-cockpit → memql (declared minimum).** Cockpit is a gRPC
   client, not part of the backend image, so it is not in the build
   pin chain above. Instead it **declares the minimum memQL engine /
   wire-protocol version it speaks** and checks the connected
   cluster's reported version against that minimum at connect time.
   Cockpit and the engine can therefore advance on independent
   release cadences as long as the cluster meets Cockpit's declared
   floor.

### How a deploy resolves

A running product deployment is fully determined by walking the
chain once:

```
  frontend @ deploy/backend-version = carrier vA.B.C
        └─► product carrier vA.B.C  (built against memql vX.Y.Z)
                  └─► memql vX.Y.Z  (immutable memql:X.Y.Z image)
```

One product bump (the `backend-version` pin) transitively fixes one
carrier build, which transitively fixes one memQL engine build. The
image tag `memql:X.Y.Z` is **write-once / immutable**, which is what
makes each link in the chain a trustworthy pin.

## The platform train

Repos version **independently** by default — memQL may be at `0.14`
while a carrier is at `0.11` and Cockpit at `0.10`. The pins above
keep any deployed combination coherent without forcing the numbers to
match.

The **exception** is the `1.0.0` cut at the invite-only beta
(~Aug 2026). At the beta, all platform repos are tagged `1.0.0`
**together as a coordinated train** — a single coherent
"first real users" release across the engine, the product's carrier +
frontend repos, and memql-cockpit. After the train departs, repos
resume **independent semver** and the pin chain again does the work of
keeping combinations coherent.

So:

- **Normally:** independent semver + the pin chain. Numbers diverge;
  pins guarantee coherence.
- **At the 1.0 beta:** one coordinated train — everyone tags `1.0.0`
  at once.

See each repo's `VERSIONING.md` for its own baseline and policy; this
repo's is [VERSIONING.md](VERSIONING.md).
