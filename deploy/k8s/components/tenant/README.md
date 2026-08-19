# `tenant` — one memQL Cloud tenant, as values

The seam a per-tenant deployment composes its **shape** from. Its sibling
[`cnpg-db`](../cnpg-db/README.md) already does this for the database and says
why: *one shape, values per overlay*. This component does the same for the mesh.

Epic memql#3852, task memql#3853.

## The claim this component exists to make true

A memQL Cloud tenant is **not a new kind of deployment**. It is the same base,
the same engine images and the same ArgoCD reconciliation as our own cloud
install — differing only in namespace, domain, replica counts and database
preset. That is the environment-parity standard
([environment-parity.md](../../../../docs/public/operate/environment-parity.md))
applied to a paying customer, and it is what makes a tenant something we already
know how to operate rather than a second system.

Diff a rendered tenant overlay against `overlays/cloud`. What differs is a
namespace, a domain, some integers and an object-store path. Nothing else,
because there is nothing else to differ.

## Composing it

A per-tenant overlay is rendered by
[`scripts/fleet/tenant-provision.sh`](../../../../scripts/fleet/tenant-provision.sh)
from [`template/`](template/), and looks like this:

```yaml
namespace: acme
resources:
  - ../../base
  - domain.yaml                 # MEMQL_DOMAIN — the tenant's own hostname
components:
  - ../../components/engine-bff
  - ../../components/tenant/presets/solo         # the instance profile
  - ../../components/tenant/optional/ha          # only when HA is bought
  - ../../components/cnpg-db
  - ../../components/cnpg-db/presets/entry       # the database preset
```

## Presets are keyed by PROFILE, and the tier maps onto them

| Tier | Profile preset | Database preset | HA |
|---|---|---|---|
| Trial | `solo` | `entry` | never — nothing to fail over to |
| Node | `solo` | `entry` | `optional/ha` when the add-on is bought |
| Graph | `standard` | `mid` | always |
| Mesh | `dedicated` | `top` | always |
| Enterprise | `dedicated` | `top` | always |

Keyed by profile rather than by tier because the *tier* is a price and the
*profile* is a shape, and the two are not one-to-one: Trial and Node are priced
completely differently and run identically. The mapping above is not folklore —
it is the `instanceProfile` and `dbPreset` fields on each
`v1:fleet:tierSpec` seed row, which is where the provisioning automation reads
it from, and `component_test.go` asserts this table against those seeds.

### What each profile is

| Preset | Mesh nodes | Voice lane | Intended for |
|---|---|---|---|
| `solo` | 1 replica each | scaled to zero | Trial, Node |
| `standard` | 2 replicas each | 1 replica | Graph |
| `dedicated` | 2 replicas each | 2 replicas | Mesh, Enterprise |

**`solo` scales the voice lane to zero, and that is a pricing fact rather than a
technical one.** Voice is not in Node's base price (a voice minute costs roughly
two orders of magnitude more than a text turn), so a Node tenant that has not
bought the add-on should not be paying for three idle voice pods — and the
binaries fail-fast without LiveKit credentials by design, so running them
unconfigured is a guaranteed crash-loop rather than a harmless idle.

> `solo` is today the **replica-count** half of the condensed instance profile.
> The deeper condensation — collapsing the mesh into fewer processes, which is
> what takes entry cost of goods from roughly $143 to roughly $90 — is task
> memql#3856. This preset is the seam that work lands in; nothing above it has
> to change when it does.

**`standard` and `dedicated` carry identical replica counts.** They are separate
presets because they differ in *where* they run — `dedicated` is a cluster of
the tenant's own — and because they compose different database presets. Two
presets whose integers happen to match today is the same reasoning that has
`overlays/cloud` restate staging's counts rather than inherit them: a value that
is inherited in one place and stated in another is a structural difference
wearing a number's clothes.

## The HA toggle is two values, and the second is the one that gets forgotten

`optional/ha` raises every mesh Deployment to 2. It is composed **after** the
profile preset, so it wins.

It is only ever composed on top of `solo`. From Graph up, HA is included in the
price and the profile preset already carries it — composing `optional/ha` there
would be a no-op that implies the tier's replication is optional.

The database's own HA is the `cnpg-db` preset's business, and its README
documents the trap in full: raising `instances` while inheriting the
single-instance `enablePDB: false` lets a node drain take the primary and its
only replica together.

## Related

- [`../cnpg-db/README.md`](../cnpg-db/README.md) — the database half of a tenant
- [`../dsl-bundle/kustomization.yaml`](../dsl-bundle/kustomization.yaml) — how a
  tenant's product DSL arrives at runtime
- [`../../../../docs/public/operate/memql-cloud.md`](../../../../docs/public/operate/memql-cloud.md)
  — the fleet control plane this component is the placement half of
