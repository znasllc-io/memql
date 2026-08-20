# `fleet` — the MemQL Cloud control plane

MemQL Cloud's control plane, built on MemQL. Epic memql#3852, task memql#3853.

## The shape of it

```
a subscriber signs up          →  v1:fleet:subscriber
they check out                 →  v1:fleet:subscription   (Stripe is the source of truth)
the subscription buys a shape  →  v1:fleet:tierSpec       (the price list, seeded)
something has to run           →  v1:fleet:instance       ← DESIRED STATE
they use it                    →  v1:fleet:usageMeter
```

The **instance row is desired state**, and the automations in
[`dsl/fleet/automations.memql`](dsl/fleet/automations.memql) are a controller
reconciling the world to it. Writing `suspending` on a row is how you suspend a
tenant; the controller notices, runs the capability script, and settles the row
at `suspended`.

That choice is load-bearing three times over, and the automations file argues
each one at the point it matters: no cross-row lookups are needed, the design is
loop-free by construction, and Orbit gets an honest progress signal without ever
holding credentials for the cluster's control plane.

## Two halves, on opposite sides of a line

|  | Lives in | Why |
|---|---|---|
| The **fleet DSL** — subscribers, subscriptions, tiers, instances, meters, and the lifecycle automations | `deploy/fleet/dsl/` — a data-only bundle, **not embedded** | MemQL Cloud is a PRODUCT. The engine under it is product-neutral by doctrine, so our control plane reaches a node the same way any customer's product does: at runtime, through `MEMQL_DSL_PATH`. |
| The **tenant lifecycle scripts** — [`scripts/fleet/`](../../scripts/fleet/) — and the [`tenant`](../k8s/components/tenant/README.md) kustomize component | the engine repo, compiled/registered normally | These provision *a MemQL instance*. They take a tenant name, a profile and a domain, render an overlay, and talk to ArgoCD. They know nothing about subscriptions, money or trials. Any operator running MemQL for anyone would want them — that is what makes them platform rather than product. |

`dsl/embed.go` does not name `fleet`, and
[`bundle_test.go`](bundle_test.go) fails the build if it ever does.

## Running it

The control plane is an ordinary MemQL deployment with this bundle mounted:

```yaml
components:
  - ../../components/dsl-bundle          # the init-container + MEMQL_DSL_PATH wiring
images:
  - name: memql-dsl-bundle
    newName: acrmemql.azurecr.io/memql-fleet-bundle
    digest: sha256:...
```

Locally, point an engine at the tree directly — no image, no init-container:

```bash
MEMQL_DSL_PATH=$PWD/deploy/fleet/dsl make dev
```

## Checking it

```bash
go test ./deploy/fleet/                       # the bundle gates
go run ./cmd/memqllint deploy/fleet/dsl/      # the same pipeline, on demand
go test ./deploy/k8s/components/tenant/       # the tier presets + the scripts
```

`go test ./...` does **not** otherwise look at this tree —
`test/dslconformance` walks `dsl.Tree()`, which is the embedded tree plus
plugin-registered subtrees, and a directory under `deploy/` is in neither. That
is exactly why `bundle_test.go` exists, and its package comment records what
that pipeline catches and, measured rather than assumed, what it does not.

## Where the money is

Everything about pricing is in
[`dsl/fleet/seeds.memql`](dsl/fleet/seeds.memql) — one file, five rows, every
number the business runs on. Three surfaces read those rows rather than
restating them: the public pricing page, Orbit's upgrade picker, and the
allowance enforcement that decides whether a turn is billable.

There is deliberately **no mutation that writes a tier**. A price change is a
bundle change: edit the seed, review it, deploy it.
[`dsl/fleet/mutations.memql`](dsl/fleet/mutations.memql) records the reasoning,
which is shorter than it looks — a bundle has no Go, so an "owner/admin only"
tier mutation would in fact be reachable by any authenticated caller, on a
publicly-readable concept, whose `messageCredits` field is the allowance that
same caller would then be serving under.

## Related

- [`../k8s/components/tenant/README.md`](../k8s/components/tenant/README.md) — the tier presets and what each one buys
- [`../k8s/components/cnpg-db/README.md`](../k8s/components/cnpg-db/README.md) — the database half of a tenant
- [`../../docs/public/operate/memql-cloud.md`](../../docs/public/operate/memql-cloud.md) — the operator runbook
- [`../../docs/internal/design/capability-script-contract.md`](../../docs/internal/design/capability-script-contract.md) — what the four lifecycle scripts have to obey
