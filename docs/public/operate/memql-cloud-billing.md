---
title: memQL Cloud billing -- Stripe, metering, and the spend ceiling
audience: public
status: stable
area: operate
sinceVersion: 0.13.0
owner: znas
---

# memQL Cloud billing

**Audience:** operators running memQL Cloud's billing.
**Epic:** memql#3852. **This task:** memql#3854.
**Companion:** [the fleet control plane](memql-cloud.md), which this pays for.

## The shape

```
Stripe  ──webhook──▶  POST /inbound/stripe  ──▶  v1:platform:inboundRequest  ──▶  fleet automations
tenant  ──webhook──▶  POST /inbound/usage   ──▶  v1:platform:inboundRequest  ──▶  v1:fleet:usageMeter
                                                                                        │
                                                                            Stripe usage records
```

Both flows ride the engine's existing [inbound receiver](inbound-delivery.md).
There is no new HTTP endpoint, which was the epic's architecture decision.

## Configuring the Stripe source

```bash
MEMQL_INBOUND_SOURCE_ALLOWLIST=stripe,usage

MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_SCHEME=hmac-sha256-hex
MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_HEADER=Stripe-Signature
MEMQL_INBOUND_SOURCE_STRIPE_ELEMENT_SEPARATOR=,
MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_ELEMENT=v1
MEMQL_INBOUND_SOURCE_STRIPE_TIMESTAMP_ELEMENT=t
MEMQL_INBOUND_SOURCE_STRIPE_SECRET=whsec_...        # secret store, never an overlay
```

### The three `ELEMENT` vars, and why they had to exist

Stripe sends **one** header carrying **both** values:

```
Stripe-Signature: t=1614556800,v1=5257a869e7ecebeda32...
```

`v1=` is not a leading *prefix*, because `t=<unix>,` comes first. So
`SIGNATURE_PREFIX` cannot reach the digest, and `TIMESTAMP_HEADER` has no
separate header to read. Before memql#3854 a Stripe source configured as
carefully as this page allows still refused every delivery with a flat `401`.

`ELEMENT_SEPARATOR` / `SIGNATURE_ELEMENT` / `TIMESTAMP_ELEMENT` describe *how
the bytes are laid out* — which is the same kind of fact the `SCHEME` constants
describe, so the receiver still carries **no vendor table** and the next sender
that packs its signature into one header is configured rather than coded.

**They are all-or-nothing.** A separator with no element key, or an element key
with no separator, is refused at boot. Both partial states fail the same way at
request time — a flat `401` — from a configuration that looks *more* complete
than the one that would work.

**The timestamp lives inside the signed payload**, so a replayed delivery cannot
be forward-dated to escape the window: moving `t` invalidates the digest. That
property is why `TIMESTAMP_ELEMENT` reads from the signature header and pointing
it at a different header is refused.

## Two layers of allowance, and why one is not enough

| | Where it lives | What it answers |
|---|---|---|
| **The meter** | `v1:fleet:usageMeter`, on the mothership | What was consumed this period; what Stripe is told |
| **The ceiling** | `memql-allowance` ConfigMap, **inside the tenant** | What this instance can spend before it stops, no matter what |

The meter is the accounting truth. It is reconstructed from reports a tenant
sends over the outbound→inbound path, so it **lags**, and a lost report
under-bills by that report.

The ceiling is the structural backstop: the tier's limit, read by the
[LLM guard](../ai/llm-cost-control.md) in-process, since boot, with no network
and no graph read. A tenant cannot exceed it if the mothership is unreachable,
if metering is broken, or if the fleet bundle has a bug in it.

**The second one is what makes "no unbounded-spend path exists" a claim about
structure rather than a hope about our own correctness.** Every metering design
has a failure mode where the meter stops and the model keeps being called; the
only defence that survives that is a limit on the other side of the boundary.

### Zero means unlimited, and that is the trap

`MEMQL_LLM_MAX_TOTAL_CALLS=0` does not mean "no calls". It means **unlimited**.

So the one value that silently disables the whole mechanism is also the one you
reach by leaving a field unset, mis-computing a tier, or coercing an empty
string — on a healthy-looking pod, in the trial tier, where it costs the most.

`tenant-provision.sh` therefore **requires** both ceilings and **refuses a
zero**, and `TestEveryTenantRendersASpendCeiling` asserts every rendered tenant
carries one and mounts it.

### Why the trial is the case this exists for

A trial's card has been authorized and never charged. An unbounded trial is an
unbounded gift, available to anyone who can complete a checkout form.

That is why trial is the one tier whose `overagePolicy` is `throttle` rather
than `meter`. A paid tier's ceiling sits well above its allowance — the customer
has a card we charge, overage is billable and profitable, and cutting off a
paying customer mid-sentence to save money we would have made is the wrong
trade. A trial's ceiling *is* its allowance.

### The ceiling is per process and since boot

Deliberately, and worth understanding rather than "fixing":

- It is a **backstop, not an accountant.** The meter does periods; this does
  runaways.
- A since-boot latch **resets on a rollout**, which is the right failure
  direction here: the worst case is a runaway getting a second chance, rather
  than a paying customer permanently locked out by a counter nobody can see.
- It is **per process**, so on the `solo` profile (one replica of each node —
  trial and Node) it is close to per-tenant. On wider profiles it is per-replica
  and looser, which is fine: those tiers have a card behind them.

## Dunning: a failed payment starts a clock

```
invoice.payment_failed  →  subscription past_due  →  subscriber delinquent  →  email
                                                                    │
                                         grace expires  →  suspend (data intact)
                                         payment recovers  →  active, grace cleared
```

**Service is not cut when a payment fails.** Most failed payments are an expired
card and someone who has not read their email yet. Cutting the customer off
loses the customer *and* the invoice — the two things dunning exists to protect.

The dunning email is keyed on `graceEndsAt`, not on the account, so it fires
**once per cycle**. Keyed on the account it would fire once *ever*: outbound
staging is idempotent by `requestId`, so the customer's second late payment
months later would land on a row already marked `sent` and no mail would go out.

## Metering is eventually consistent, on purpose

A tenant that cannot reach the mothership **keeps serving** and reports later.
The alternative is a customer's instance going down because our billing system
is having a bad afternoon.

Two properties make that safe:

- **The ceiling** bounds spend locally while reports are missing (above).
- **Reports are absolute, not deltas.** One meter row per
  `(subscription, period, metric)` at a derived id, holding a total the tenant
  counted. Re-applying the same report writes the same number, so the
  at-least-once delivery the inbound receiver guarantees cannot double-bill.

That last point is the reason `recordUsage` takes totals. A redelivery lands on
the same `v1:platform:inboundRequest` row, but staging it is still a write — so
the automation **fires again**. An additive meter would double the customer's
bill; a set-valued one is a no-op.

## Provider costs feed the margins

`inputCostPerMillion` / `outputCostPerMillion` on each provider in
`dsl/providers/providers.memql` are the AI cost of goods every tier margin is
computed against.

**An understated cost does not present as an error. It presents as a business
that looks profitable.** `chat54Mini` carried $0.15/$0.60 — the gpt-4o-mini
rates — through two model renames, against an official gpt-5.4-mini list of
$0.75/$4.50. Five times understated on input, seven on output.

`TestProvidersForOneModelAgreeOnItsPrice` now gates the case that matters: two
providers naming the same `@model` must report the same cost. That is what a
half-applied correction looks like, and it is what happened — the streaming
sibling carried an identical stale copy.

A deliberate exception is declared, not silenced: `chat54Pro` / `stream54Pro`
price as the pro tier while aliasing to the flagship `@model` (the pro model is
responses-API only). The entry **goes stale** when the alias does, and a stale
entry fails.

## What is not built yet

Stated rather than implied.

- **Stripe products and prices are not provisioned by anything here.** They need
  a live Stripe account. The tier catalog
  (`deploy/fleet/dsl/fleet/seeds.memql`) is the source of truth for the numbers;
  creating the matching Stripe objects is a manual step until a catalog-sync
  capability lands.
- **The Stripe event → fleet row resolution is not wired.**
  `claimStripeDelivery` stamps the delivery **`failed`, with the reason on the
  row**, and stops.

  `failed` rather than `processing` is deliberate: `processing` means "a product
  automation has this in hand", so every Stripe delivery would sit there forever
  and `inboundRequestsByStatus("processing")` would show a pile nothing was
  working. **A status that claims a worker which does not exist is worse than no
  handler at all — it is a backlog that reads as progress.** `failed` is
  queryable, carries `lastError` naming the missing piece, and stays
  re-drainable, since the engine never retries product-side handling itself.

  The reason is worth stating precisely, because the obvious guess about it is
  wrong.

  It is **not** the lookup. `subscriptionByStripeId` resolves a `sub_...` to a
  fleet row perfectly well — `@serverOnly` + `@unbounded` is exactly the shape an
  internal sweep or webhook handler takes, and a logic reaches it with
  `OriginInternal` stamped by the step executor.

  It is that **the DSL cannot read the event.**
  `v1:platform:inboundRequest.body` is the verified raw body as a *string*, and
  the automation evaluator has no JSON parsing — no field extraction, no path
  read, nothing that turns those bytes into `data.object.subscription`. A
  handler can see that a Stripe delivery arrived and cannot see what it says.

  That is a real limit on the inbound surface's "no product Go, no sidecar"
  promise, and this is the **second** thing to hit it: campaigns needed a Go
  builtin (`integration.campaigns.ingestFeedback`) for the identical reason.
  The fix is generic and belongs in the engine — **one JSON-extract builtin
  would serve every product draining `POST /inbound/{source}`**, which is the
  entire point of that surface existing.

- **Stripe usage records are not pushed.** `usageMeter.reportedToStripe` exists
  and the arithmetic is defined; the API call needs a server-side Stripe
  credential and an outbound path for it.

The through-line: what is missing is **not** graph access. The sweeps
(`subscriptionsByStatus`, `instancesByStatus`, `subscriptionByStripeId`) are
built and reach every row an internal handler needs. What is missing is the
ability to read a third party's payload, and one generic engine builtin closes
it for every product at once.

## Related

- [The fleet control plane](memql-cloud.md)
- [Inbound delivery](inbound-delivery.md) · [Outbound delivery](outbound-delivery.md)
- [LLM cost control](../ai/llm-cost-control.md) — the guard this ceiling configures
- [Campaign sending](campaign-sending.md) — the precedent for borrowing an owner's actor
