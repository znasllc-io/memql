---
title: Staging and Production Inside One Installation
audience: internal
status: design approved; ready for implementation planning
area: design
date: 2026-08-14
owner: znas
surface: deploy/k8s + component/database + component/deploycontrol + dsl/platform
---

# Staging and Production Inside One Installation

Collapse two cluster installations into one, without collapsing the boundary
between them. One Kubernetes cluster, one ArgoCD, one front door, one database
server -- and two environments that cannot write to each other's data.

Sub-project **D** of the 2026-08-14 brainstorm. Tracked as epic memql#3748.

---

## 1. Problem

Staging and production are two complete installations: two clusters, two
ArgoCD estates, two of everything. `Promote` is copying image digests from the
staging overlay into the production overlay, and the script that did it
(`scripts/release/promote.sh`) was moved out of this repo entirely by
`992deb41`. Meanwhile the engine's own `deploycontrol` refuses local clusters
(`driver.go:35`), so the development environment is not on this path at all.

Three costs: double the infrastructure, a promote that is a text edit to a
manifest, and no way to stage a *site* -- `v1:platform:site` has no environment
axis, so an SPA either is live or is not.

---

## 2. The constraint that decides the design

**memQL has no tenancy dimension in the actor envelope.** That is a recorded
finding, not an oversight: the sites epic's D1 measures it (memql#3321), and
the partition dimension that once did this job was deliberately retired in #56.
Every account-scoped filter compares against a caller-supplied argument.

So an environment boundary cannot be a filter. There is nothing in `actor.*` to
filter on, and adding one means re-opening #56 and touching every query and
mutation in the tree, where a single missed conjunct leaks production data.

It also cannot be "shared data, separate artifacts". Exercising a staging SPA
performs **mutations**; automations fire; agents write. Those land wherever the
connection points.

The boundary therefore has to be **the connection itself**, which is the one
place no application code path can forget.

---

## 3. The shape

```
ONE k3d/AKS cluster · ONE ArgoCD · ONE front door · ONE database server

  ns/memql-prod                    ns/memql-staging
    9 node types × 2 replicas        9 node types × 1 (→0 idle)
    search_path=memql_prod           search_path=memql_staging
                    ╲                ╱
              one Postgres/TimescaleDB service
                 schema memql_prod
                 schema memql_staging
                 schema public        ← deliberately EMPTY
```

### 3.1 Two schemas, one database, one server

Each namespace's secret carries the same DSN with a different `search_path`.
memQL already sets per-connection parameters via `pgdriver.WithConnParams`
(`component/database/database.go:297`), so `search_path` slots in beside the
existing session safety-net parameters; the migration job picks up the same
value and `CREATE TABLE IF NOT EXISTS` lands in the first schema on the path.
TimescaleDB resolves `create_hypertable('MemoryNodes'::regclass, ...)` through
the search path, so hypertables and continuous aggregates need no special
handling.

**`public` is left empty, and that is load-bearing.** An unset or mistyped
`search_path` falls back to `"$user", public`. If production lived in `public`,
that slip would silently mean *production*. With both environments in named
schemas, the same slip resolves to a schema containing nothing and every query
fails loudly on the first statement. The safe failure is bought by giving
production a name too.

### 3.2 Why not two databases

Considered and rejected. It needs no engine change at all, which is its whole
appeal -- but a second database inside a managed Postgres service is a
plan-dependent privilege, and it puts the two environments on separate
connections, so promoting a site becomes two connections and a copy through the
application instead of one transaction. The `search_path` addition is small and
buys both back.

### 3.3 Hostnames

The front door's rule -- the host count must not grow with customers, apps or
sites (memql#3700) -- is preserved. Environment is not one of those things: it
is a closed set of two.

The host set becomes a generated product of **role × environment**:

| Role | production | staging |
|---|---|---|
| api | `api.<domain>` | `api-staging.<domain>` |
| identity | `identity.<domain>` | `identity-staging.<domain>` |
| mcp | `mcp.<domain>` | `mcp-staging.<domain>` |
| sites | `*.<domain>`, apex | `*.staging.<domain>` |

Single-label roles keep the existing `*.<domain>` wildcard certificate. Sites
need a second wildcard, `*.staging.<domain>`, which cert-manager issues exactly
as it issues the first.

**A staging site always lives under the cluster's own domain.** The staging
host for `shop.acme.com` is `shop.staging.<clusterdomain>` -- never
`shop.staging.acme.com`. Staging is the operator's testing surface and must not
require DNS the operator may not control.

Generated, not authored: adding an environment is a value change, not a
manifest edit, in the same spirit as #3700 deriving `api.<domain>`'s path set.

### 3.4 ArgoCD

One ArgoCD instance, one repository, one base. Two Applications --
`memql-prod` and `memql-staging` -- with destination namespaces and overlays
that differ only in **values**: namespace, `MEMQL_ENVIRONMENT`, the DSN's
`search_path`, replica counts, image digests, and the host prefix.

That is precisely the base/overlay split CLAUDE.md's parity standard demands:
the *shape* of the system is identical in both, and only values move. The
standard is satisfied more completely after this change than before it, because
staging and production now reconcile from the same base rather than from two
overlay trees maintained in parallel.

`MEMQL_ENVIRONMENT` already exists and is already stamped onto
`v1:cluster:node.environment` at registration (#1873).

### 3.5 Scaling

Per-environment replica values in each overlay; `make scale N=2 ENV=staging`.
Staging scales to zero when idle, which is what makes the second environment
cost storage rather than compute.

---

## 4. Promotion

Two distinct things get promoted, and conflating them is the current confusion.

### 4.1 Engine promotion -- moving the mesh to a version

Unchanged in kind: pin the production overlay's image digests to the ones
staging is running. What changes is that both overlays live in one tree
reconciled by one ArgoCD, so a promote is one commit rather than a copy between
estates.

### 4.2 Artifact promotion -- moving a site to production

`v1:platform:site` gains no environment field. Instead, **each environment has
its own site rows**, in its own schema, because environments are already
separated by the connection.

The bundle itself is immutable and versioned by prefix in shared object storage
(`blob://sites/<id>/<version>/`), so it is written once and referenced by both.
Promotion is: read the staging site row's `bundleRef`, write it to the
production site row. Both schemas are reachable on one connection, so that is a
single transaction, and rollback is the same write with the previous value.

```
blob://sites/shop/v4.3.0/          uploaded once, immutable

  memql_staging.site  shop   → v4.3.0    tested
  memql_prod.site     shop   → v4.2.0

  promote → memql_prod.site shop = v4.3.0     one transaction
  rollback → the same write, previous value
```

This is what "promote the artifacts, not the cluster" means concretely, and it
is why the two-schema choice pays for itself beyond isolation.

### 4.3 DSL bundle promotion

A product's DSL bundle is delivered as a data-only image mounted at
`MEMQL_DSL_PATH`, so promoting one is pinning the production overlay to the
digest staging is running -- the same act as §4.1, in the same commit.

**Trained constructs are not promoted this way and deliberately do not cross.**
A promoted construct is a row in its schema, so staging and production are
trained separately (sub-project B, memql#3745). Training production is an
explicit act against production, not a side effect of promoting a bundle.

---

## 5. What must not regress

- **Local stays one environment.** `make up` brings up a single environment and
  does not grow a second namespace. Development is not staging, and giving the
  local cluster two environments would make the inner loop slower to prove
  nothing.
- **`deploycontrol` still refuses local.** Unchanged (`driver.go:35`).
- **No `if env == "..."` in engine code.** Environment reaches the engine as
  configuration -- a search path, a domain prefix, a replica count -- and never
  as a branch. The parity standard rejects a second way to deploy, and this
  design must not smuggle one in.
- **The five-host front-door discipline.** Rules stay generated from a closed
  set; nothing gains a hand-authored hostname.

---

## 6. Module layout

```
component/database/database.go        search_path via sessionConnParams
component/database/.../migrations     schema-aware migrate job
deploy/k8s/overlays/prod              namespace, values
deploy/k8s/overlays/staging           namespace, values
deploy/k8s/base                       host rules generated over role × environment
deploy/argocd/apps                    two Applications, one base
component/deploycontrol               promote targets an overlay in this tree
dsl/platform/mutations.memql          promoteSite: cross-schema bundleRef write
scripts/k3d, Makefile                 ENV= on scale; unchanged for `make up`
```

---

## 7. Testing

- **Schema isolation, for real**: a DB test that writes under
  `search_path=memql_staging` and asserts the row is invisible under
  `search_path=memql_prod`, and the reverse. This is the entire premise; it gets
  a test that would fail if the search path were ever ignored.
- **The empty-`public` guard**: with `search_path` unset, a query fails rather
  than reading either environment. Asserted, because it is the design's safety
  margin.
- **Render gates** over both overlays: every generated host rule present, both
  wildcard certificates requested, no hostname authored by hand -- extending
  the existing front-door render gate (memql#3701).
- **Migrations run per schema** and leave the other untouched.
- **Promote round-trip**: staging `bundleRef` reaches production in one
  transaction, and rollback restores the prior value.

Per the house rule, the `_db_test.go` suites run against a throwaway
TimescaleDB container, never a bootstrapped cluster database.

---

## 8. Relationship to the other sub-projects

Independent of **A** and **B**. Sub-project **C** (memql#3733) consumes the
result: a remote instance in the Deployments view gains an environment
dimension, and the "no deploy pipeline configured" state it renders is the one
this epic finally fills in for the engine's own estate.

Trained constructs (**B**) are per-schema by construction, so the two designs
need no coordination.
