---
title: Environments — staging and production in one installation
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# Environments: staging and production in one installation

Staging and production are **two namespaces in one cluster**, sharing one
ArgoCD, one front door and one database server, separated by nothing but
configuration.

This page is the operator's model. It exists to let you answer one question
correctly without reading any code: **where does this write land?** The design
rationale lives in
[the virtual-environments design](../../superpowers/specs/2026-08-14-virtual-environments-design.md)
and is not repeated here.

---

## The shape

```
ONE cluster - ONE ArgoCD - ONE front door - ONE database server

  ns/memql-prod                    ns/memql-staging
    9 node types x 2 replicas        9 node types x 2 (-> 0 idle)
    search_path=memql_prod           search_path=memql_staging
                    \                /
              one Postgres/TimescaleDB service
                 schema memql_prod
                 schema memql_staging
                 schema public        <- deliberately EMPTY of tables
```

Both environments run the **same images** from the **same base**, reconciled by
the **same ArgoCD**. Diff `deploy/k8s/overlays/prod/kustomization.yaml` against
`deploy/k8s/overlays/staging/kustomization.yaml` and the only differences are
the namespace, two ConfigMap entries, the replica counts and the image digests.

---

## Where a write lands

**The connection decides. Nothing else does.**

Each namespace's pods carry the same database DSN with a different
`MEMQL_DB_SEARCH_PATH`, applied to every connection the driver opens. A pod in
`memql-staging` writes to `memql_staging` because that is what its connection
resolves an unqualified table name to. No query names a schema, no filter
mentions an environment, and no code path can forget which environment it is
in -- there is nothing to forget.

So the operator's rule is short:

> **A write lands in the environment whose namespace the pod that served it
> runs in.**

That covers every write, including the ones that are easy to forget are writes:
an automation firing, an agent replying, a background worker draining a queue.
Exercising a staging SPA performs mutations, and they land in staging because
the pod serving it is in staging.

### Why the boundary is the connection and not a filter

memQL has **no tenancy dimension in the actor envelope**. That is a recorded
finding rather than an oversight (memql#3321), and the partition dimension that
once did this job was deliberately retired (#56). So an environment boundary
cannot be a `WHERE` clause: there is nothing in `actor.*` to filter on, and one
missed conjunct leaks production data.

It also cannot be "shared data, separate artifacts", because a staging SPA
*writes*.

### Why `public` is empty, and why that is load-bearing

An unset or mistyped search path falls back to `"$user", public`. If production
lived in `public`, that slip would **silently mean production**.

With both environments in *named* schemas, the same slip resolves to a schema
holding no application tables, and the first statement fails loudly. Production
is given a name precisely so that a mistake becomes an error instead of a
disclosure.

`public` does stay on the path -- TimescaleDB's functions live there -- but it
holds no application tables. The migrate job checks that the extension is
actually reachable on the configured path, so a wrong path fails there, naming
the path, rather than forty migrations later as
`function create_hypertable does not exist`.

---

## Hostnames

The front-door host set is the **product of role and environment**, generated
rather than authored (memql#3767):

| Role | production | staging |
|---|---|---|
| api | `api.<domain>` | `api-staging.<domain>` |
| identity | `identity.<domain>` | `identity-staging.<domain>` |
| mcp | `mcp.<domain>` | `mcp-staging.<domain>` |
| sites | `*.<domain>`, apex | `*.staging.<domain>` |

**Role hosts hyphenate; site hosts nest.** The asymmetry is a TLS fact, not a
style choice: a wildcard certificate matches exactly **one** label, so
`*.<domain>` covers `api-staging.<domain>` and does **not** cover
`api.staging.<domain>`. Keeping role hosts single-label means the certificate
the cluster already has covers every environment's.

Sites cannot avoid nesting, because a site's own name occupies that label. A
labelled environment therefore requests exactly one more wildcard,
`*.staging.<domain>` -- one certificate for all of its sites, not one per site.

**A staging site always lives under the CLUSTER's domain.** The staging host
for `shop.acme.com` is `shop.staging.<clusterdomain>`, never
`shop.staging.acme.com`. Staging is the operator's testing surface and must not
require DNS the operator may not control.

Adding an environment is a **value change** -- a new overlay directory carrying
a new label -- and needs no generator edit.

---

## What promotion moves

Three things get promoted, and they are not the same act.

| What | How it moves | Result |
|---|---|---|
| **Engine version** | pin production's image digests to the ones staging runs | one commit, one ArgoCD reconcile |
| **DSL bundle** | pin production to the bundle digest staging runs | the same commit |
| **A site** | write staging's `bundleRef` onto production's site row | one transaction, no upload |

Engine and bundle promotion are one commit in one tree, reconciled by one
ArgoCD -- not a copy between estates.

Site promotion moves **only the reference**. The bundle is immutable and
versioned by prefix in shared object storage (`blob://sites/<id>/<version>/`),
written once by the publish path and referenced by both environments:

```
blob://sites/shop/v4.3.0/          uploaded once, immutable

  memql_staging.site  shop -> v4.3.0    tested
  memql_prod.site     shop -> v4.2.0

  promote  -> memql_prod.site shop = v4.3.0    one transaction
  rollback -> the same write, previous value
```

Because both schemas are reachable on one connection, that promote is a single
transaction. **Rollback is the same write with the previous value**, not a
distinct code path.

Two consequences worth knowing before you need them:

- Promoting a site production does not have yet **creates** it. First publish
  and promote are the same act. You must supply the production hostname, because
  it cannot be derived from the staging one -- staging lives under the cluster's
  domain and production lives wherever the customer's DNS points.
- Promoting the version production is already pinned to is a **no-op**. No new
  row version is written, because the graph's row history *is* a site's version
  list and an appended no-op would show a deploy that never happened.

### Trained constructs do not cross environments

**A promoted construct does not travel with an engine or bundle promotion, and
this is deliberate.**

A promoted construct is a row in its own schema. Rows are separated by the
connection, so staging and production are trained **separately** (memql#3745).
Training production is an explicit act against production, never a side effect
of promoting a bundle.

This is the single most likely wrong assumption about the combined system. The
symptom is "I promoted the bundle -- why is my promoted construct missing?", and
the answer is that nothing was lost: the construct is still in staging, where it
was trained, and production was never trained.

---

## Running a promotion

```bash
# promote a site from staging to production
kubectl exec -n memql-prod deploy/bff -- /app/memql promote-site \
  --site-id v1:platform:site:shop --from staging --to prod

# ... which prints the previous reference; roll back with it
kubectl exec -n memql-prod deploy/bff -- /app/memql promote-site \
  --site-id v1:platform:site:shop --to prod --bundle-ref blob://sites/shop/v4.2.0/

# first trip to production: the hostname cannot be derived, so name it
kubectl exec -n memql-prod deploy/bff -- /app/memql promote-site \
  --site-id v1:platform:site:shop --from staging --to prod --hostname shop.acme.com
```

Clients drive the same operation over `PromoteSiteMsg` on
`MemqlService.Stream`, which is owner-gated. The subcommand exists for the case
where no client is to hand -- including a cluster whose bff is the thing being
fixed.

Scaling names its environment, and defaults to local so the inner-loop command
typed from memory cannot point at production:

```bash
make scale N=2 ENV=staging
make scale N=0 ENV=staging      # staging spends most of its life here
```

---

## What does not change

- **Local stays one environment.** `make up` grows no second namespace, and the
  local overlay supplies no environment ConfigMap. Development is not staging,
  and a second local environment would make the inner loop slower to prove
  nothing. See [reproduce-staging-locally.md](reproduce-staging-locally.md).
- **`deploycontrol` still refuses local.** Development is not a deploy target.
- **No `if env == "..."` in engine code.** Environment reaches the engine as a
  search path, a domain prefix and a replica count -- never as a branch.
  `TestNoEnvironmentBranchingInEngineCode` fails the build on engine code so
  much as *naming* a deployment environment, because a name is what a branch is
  built out of.

---

## See also

- [environment-parity.md](environment-parity.md) -- the standard this satisfies
- [reproduce-staging-locally.md](reproduce-staging-locally.md) -- the local cluster
- [front-door.md](front-door.md) -- the generated host and path sets
- [the design](../../superpowers/specs/2026-08-14-virtual-environments-design.md) -- why, at length
