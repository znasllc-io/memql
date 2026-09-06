---
title: Orbit -- the MemQL Cloud customer console
audience: public
status: stable
area: operate
sinceVersion: 0.13.0
owner: znas
---

# Orbit

**Audience:** anyone building or deploying the MemQL Cloud customer console.
**Epic:** memql#3852. **This task:** memql#3855.
**Companions:** [the fleet control plane](memql-cloud.md), [billing](memql-cloud-billing.md).

Three consoles, and it is worth fixing which is which before anything else:

| | What it is | Scope |
|---|---|---|
| **Cockpit** | the terminal IDE | a developer's machine |
| **Portal** | the graphical ops console | one instance |
| **Orbit** | the customer's control app | the fleet above them |

## Orbit is a client, and a client cannot be the gate

Everything below follows from that.

Orbit reads and writes the fleet graph over the standard gRPC/WS surface, as
the signed-in subscriber, with no elevated credential of any kind. A subscriber
sees their own rows because **the server will not return anything else** — not
because Orbit filters.

That is enforced two ways, both server-side, and
[`deploy/fleet/authz_test.go`](https://github.com/znasllc-io/memql/blob/main/deploy/fleet/authz_test.go)
gates both:

1. Every customer concept declares `@rowAuthz(owner="ownerUserId")`, so the
   engine injects the ownership predicate into every read and refuses a write
   whose target belongs to somebody else. The owned tier has **no cluster-owner
   bypass**.
2. Every caller-scoped query additionally names `ownerUserId==actor.userId` as a
   **top-level** conjunct.

The second exists because of *where* the conjunct sits. Inside a `when()` guard
it vanishes when its argument is absent — that is what the guard is for — so a
query that reads as scoped returns the whole table the moment a caller omits an
optional argument. That is memql#2883, it has happened before, and it looks
entirely correct in review. The gate splits the filter on `&&` at depth zero and
requires the conjunct to be one of the terms.

### Orbit never talks to ArgoCD

Provisioning progress comes from `v1:fleet:instance.status` transitions, which
the lifecycle automations stamp.

A console that queried the cluster's control plane directly would need
credentials for the cluster's control plane — a far larger blast radius than
"can read my own rows", in a browser, for the sake of a progress bar.

## What Orbit reads

| Surface | Query |
|---|---|
| Tier picker (signed out) | `publicTiers`, `tierByName` |
| Tier picker (signed in) | `tierSpecByName` — adds the operational fields |
| Account | `mySubscriber` |
| Plan panel | `mySubscriptions`, `mySubscriptionById` |
| Dashboard | `myInstances`, `myInstanceById` |
| Usage strip | `myCurrentUsage` |
| Billing history | `myUsageHistory` |

`publicTiers` binds the **narrow** `tierPublic` projection. `tierSpecByName`
binds `tierFull`, which adds `instanceProfile` and `dbPreset` — operational
facts about what we run, not facts a prospective customer needs. Publishing our
infrastructure shape to the open internet is a free reconnaissance gift, and the
narrow shape is the enforcement rather than a note asking callers to be careful.

## What Orbit writes

| Action | Mutation |
|---|---|
| Sign up | `createSubscriber` |
| Update billing details | `updateSubscriberBilling` |
| Upgrade / downgrade tier | `requestTierChange`, then the two instance writes below |
| Toggle HA | `setSubscriptionAddOns`, then the two instance writes below |
| Cancel | `requestCancellation` |
| Suspend / resume | write the instance's status |

**There is deliberately no customer-callable mutation that sets a subscription
to `active`, moves a period boundary, or writes a Stripe id.** A customer cannot
pay themselves. Those are reached only from a signature-verified webhook.

### A tier change is two writes, and the second is the one that acts

```
requestTierChange(subscriptionId, tier)              # what they bought
tierSpecByName(tier)                                 # read the shape it buys
setInstanceShape(instanceId, tier, instanceProfile,  # what will run
                 dbPreset, haEnabled,
                 maxLlmCalls, maxLlmCostUsd)
markInstanceProvisioning(instanceId)                 # ← this is what reconciles
```

The last write is the command. The fleet's automations treat the transient
instance statuses as commands, so moving the row to `provisioning` is what
re-renders the tenant at its new shape — see
[the control plane](memql-cloud.md#the-one-idea).

**Why Orbit does this rather than an automation on the subscription.** An
automation's actor is not the row's owner, so a caller-scoped read from inside
one returns nothing *while looking entirely correct*. Orbit is signed in as the
subscriber and can read both rows; the automation cannot. Pushing the join to
the client is what keeps every server-side read in this domain trivially
correct.

The consequence to design for: **these writes are not atomic.** A tier change
that writes the subscription and then fails leaves a subscription on the new
tier and an instance on the old shape. That is a visible, recoverable state —
the dashboard shows the mismatch, and re-running the last two writes fixes it —
rather than a silent one. Show the user the instance's state, not the
subscription's, when they differ.

### The shape arguments are resolved, not chosen

`setInstanceShape` takes `instanceProfile`, `dbPreset` and the two ceilings as
arguments, so a caller could ask for `dedicated` on a Node subscription.

What stops that is not the mutation. It is that these values come off the
tier's own spec row, and a wrong shape is corrected on the next re-render from
the same source. Recorded here because the argument list looks like a choice and
is not one. A tighter binding — the engine resolving the tier server-side —
needs the same clusterOwner-tier read the billing gaps need.

## Deploying Orbit

Orbit is a MemQL product client: a DSL bundle plus an SPA, built from the
`memql-project` template, in **its own repository**. The engine's `clients/`
allowlist is untouched — a customer's SPA belongs in a product repo, and so does
ours.

It is served by the mothership's edge as a hosted site, like any other:

```
v1:platform:site
  hostname:  orbit.<domain>
  kind:      spa
  bundleRef: blob://sites/<id>/<version>/
  apiProxy:  true
  status:    live
```

`apiProxy: true` mounts `/_memql/*` on Orbit's own origin, forwarded to the bff,
which makes Orbit same-origin with its API — removing CORS and the
`SameSite=Lax` cookie problem entirely. See [site hosting](site-hosting.md) for
the publish flow.

**The site row is not seeded**, unlike the portal's. Its hostname is
`orbit.<domain>` and the domain is a per-deployment value, so the row is created
once by an operator against the mothership rather than baked into a bundle that
cannot know it.

## What is not built

Stated rather than implied.

- **The SPA itself.** This repository holds the server-side contract Orbit
  consumes and the gates that make it safe; the console lives in its own repo.
- **Team management.** The acceptance criterion asks for inviting additional
  admins. It is blocked by a design decision recorded in
  `deploy/fleet/dsl/fleet/concepts.memql`: the owned authz tier compares a field
  to `actor.userId`, so an account has exactly one owner today. A second admin
  is a tier change plus a grant concept — not a re-model, but not a mutation
  either. Cheaper to do once a customer is asking than to guess the shape now.
- **The Stripe billing-portal link.** It is a Stripe API call returning a
  short-lived URL, which needs the same server-side Stripe credential the
  [billing gaps](memql-cloud-billing.md#what-is-not-built-yet) need.

## Related

- [The fleet control plane](memql-cloud.md) · [Billing](memql-cloud-billing.md)
- [Site hosting](site-hosting.md) — how Orbit gets served
- [Per-row authz](auth/per-row-authz-audit.md) — the model these gates enforce
- [The Portal](portal.md) — the per-instance console Orbit deep-links into
