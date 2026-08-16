---
title: memQL Cloud -- the fleet control plane
audience: public
status: stable
area: operate
sinceVersion: 0.13.0
owner: znas
---

# memQL Cloud: the fleet control plane

**Audience:** operators running memQL Cloud, and anyone running a fleet of
memQL instances for other people.
**Epic:** memql#3852. **This task:** memql#3853.

memQL Cloud sells private memQL instances by subscription. This page is how the
fleet that provisions them works, and how to drive it by hand when you need to.

## The one idea

**The instance row is desired state.** The control plane is a controller
reconciling the cluster to it.

```
v1:fleet:instance.status = "provisioning"   →  render the overlay, adopt it in ArgoCD  →  "running"
                           "suspending"     →  scale to zero, data intact              →  "suspended"
                           "resuming"       →  scale back up                           →  "running"
                           "tearing_down"   →  final backup, then destroy              →  "torn_down"
```

The four transient statuses on the left are **commands**. The four settled
statuses on the right are what the controller writes when it is done, and no
automation triggers an action on any of them — which is what makes the whole
design loop-free without a generation counter, a reconcile lock or a debounce.

The practical consequence for an operator: **to act on a tenant, write its
status.** There is no separate command surface, no queue, and nothing to drain.

## A tenant is not a new kind of deployment

It is the same base, the same engine images and the same ArgoCD reconciliation
as staging and production. Diff a rendered tenant overlay against
`deploy/k8s/overlays/prod` and what differs is a namespace, a domain, some
integers and an object-store path.

That is [environment parity](environment-parity.md) applied to a paying
customer, and it is the reason a tenant is something you already know how to
operate. `argocd app get <tenant>` is the honest answer to "what is that
customer running" — which is the answer you want when the control plane is the
thing that is broken.

## What a tier buys

| Tier | Price | Message credits | Voice minutes | Profile | Database | HA |
|---|---|---|---|---|---|---|
| Trial | $0, 14 days | 500 (throttled) | 60 (throttled) | `solo` | `entry` | never |
| Node | $199/mo | 2,000 | 0 (add-on) | `solo` | `entry` | add-on |
| Graph | $949/mo | 10,000 | 1,000 | `standard` | `mid` | included |
| Mesh | $2,999/mo | 50,000 | 5,000 | `dedicated` | `top` | included |
| Enterprise | quoted | per contract | per contract | `dedicated` | `top` | included |

Overage is $30 per 1,000 message credits and $0.15 per voice minute. Annual
billing is ten months' list price for twelve months of service, computed at
checkout from the monthly price so the discount cannot drift.

**Trial throttles where every other tier meters**, and that is the single most
important row in the table. A trial with metered overage is an unbounded spend
path attached to a card that has been authorized and never charged.

Every number above lives in exactly one place —
`deploy/fleet/dsl/fleet/seeds.memql` — and three surfaces read it from there
rather than restating it: the pricing page, Orbit's upgrade picker, and the
allowance enforcement.

### There is no way to change a price from a console

Deliberately. A price change is a bundle change: edit the seed, review it,
deploy it. The full reasoning is in `deploy/fleet/dsl/fleet/mutations.memql`,
and the short version is that a bundle has no Go, so a mutation documented as
"owner/admin only" would in fact be reachable by any authenticated caller — on
a publicly-readable concept whose `messageCredits` field is the allowance that
same caller would then be serving under.

A price **rise** publishes a new tier and marks the old one `grandfathered`.
Editing a tier row in place re-prices every subscription that names it,
retroactively and silently.

## Driving it by hand

The four lifecycle capability scripts are the deterministic backends the
automations call. They are also perfectly good to run yourself — that is the
[capability-script contract](../../internal/design/capability-script-contract.md)
working as intended: same behaviour whether a human or an automation invokes
them.

**Every one defaults to a dry run.** Pass `--dryRun=false` to act.

```bash
# What would provisioning acme look like?
scripts/fleet/tenant-provision.sh \
  --tenant=acme --domain=acme.memql.cloud \
  --profile=solo --dbPreset=entry --ha=false

# Do it.
scripts/fleet/tenant-provision.sh ... --dryRun=false

# Scale a tenant to zero, data intact.
scripts/fleet/tenant-suspend.sh --tenant=acme --dryRun=false

# Bring it back at the counts its overlay declares.
scripts/fleet/tenant-resume.sh --tenant=acme --dbInstances=1 --dryRun=false

# Destroy it. Final backup first; the phrase names the tenant.
scripts/fleet/tenant-teardown.sh --tenant=acme --confirm='teardown acme' --dryRun=false
```

`--print-spec` on any of them prints its parameters as JSON, without running
anything.

### Reading the exit codes

| Code | Meaning |
|---|---|
| 0 | done; the result envelope is on stdout |
| 2 | bad parameter — an unusable tenant slug, an unknown profile, a non-numeric count |
| 3 | **refused** — the teardown confirmation phrase did not match |
| 4 | prerequisite missing — no `kubectl` on the runner |
| 5 | the operation failed — the envelope's `error.message` says how |

`changed` in the envelope is honest in both directions. Re-provisioning
unchanged inputs reports `changed: false`, which is what makes a redelivered
provisioning event harmless.

## Things that will bite you

**Suspend turns automated sync off before it scales.** In that order, always.
Scaling a self-healing ArgoCD Application down without disabling sync first is a
suspend that silently comes back up on the next reconcile — and it looks exactly
like a successful suspend until somebody reads a bill.

**Resume scales the database first.** A mesh that comes up against a
zero-instance database crash-loops on connect: noisy, alarming, and entirely
self-inflicted.

**Teardown aborts if the final backup fails.** By design. A teardown whose
backup failed and proceeded anyway is indistinguishable, an hour later, from one
that worked. `--skipBackup=true` exists for a tenant that never provisioned, and
it is a separate flag rather than a fallback for exactly that reason.

**Suspended and torn down are different states.** Suspended keeps every volume,
backup and namespace; a resume is a scale-up. Torn down has no data and a resume
is impossible. Conflating them is how a resume gets attempted against deleted
volumes, and the failure arrives as a confusing reconciliation error rather than
as a refusal.

**A Node tenant buying HA needs both halves.** The `optional/ha` component
doubles the mesh; the database has to move from the `entry` preset to `mid`
separately. Shipping only the first gives you a mesh that survives a node drain
and then has nothing to talk to.

## Where things live

| | |
|---|---|
| The fleet DSL bundle | [`deploy/fleet/`](https://github.com/znasllc-io/memql/tree/main/deploy/fleet) |
| The lifecycle scripts | `scripts/fleet/` |
| Tier presets (the mesh half) | [`deploy/k8s/components/tenant/`](https://github.com/znasllc-io/memql/tree/main/deploy/k8s/components/tenant) |
| Database presets | [`deploy/k8s/components/cnpg-db/`](https://github.com/znasllc-io/memql/tree/main/deploy/k8s/components/cnpg-db) |
| Rendered tenant overlays | `deploy/k8s/tenants/<tenant>/` |
| Tenant ArgoCD Applications | `deploy/argocd/tenants/<tenant>.yaml` |

## Related

- [Environment parity](environment-parity.md) — the standard a tenant satisfies
- [The database platform](database-platform.md) — what a tenant's database is
- [Inbound delivery](inbound-delivery.md) — how Stripe webhooks reach the fleet
- [Outbound delivery](outbound-delivery.md) — how the fleet sends mail without Go
