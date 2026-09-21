---
title: Consuming the wholesale pack from a product repository
audience: public
status: stable
area: build
sinceVersion: 0.22.8
owner: znas
---

# Consuming the wholesale pack from a product repository

The **wholesale pack** (`packs/wholesalepack`, domain `wholesale`) owns an
application, a decision and an entitlement. It owns nothing else on purpose:
not who approves, not in what order, not what is notified, not any form field
past the minimum, and not any surface. Those are **yours**, and this page is
what you write to supply them.

If you have not read [Building a pack](building-a-pack.md), read it first. This
page assumes it, and assumes you know that a **pack** is a client-agnostic
product feature — the third of the [three extension
words](../concepts/component-integration-pack.md).

> **The pack ships disabled.** Enabling it publishes a write endpoint on every
> deployable whose `shopperForms` is on, and what that endpoint collects is a
> named person's business contact details. Turn it on deliberately, in
> **Cluster → Modules**, per cluster. There is no `v1:platform:packState` row
> until somebody writes one, and absence means the pack's declared default,
> which is off.

## What you are adopting

Four concepts, and you will write against three of them:

| Concept | What it is | Who writes it |
|---|---|---|
| `v1:wholesale:application` | Somebody asking one store for trade terms | The shopper, through the pack's declared form |
| `v1:wholesale:applicationDecision` | One append-only decision: approve, reject or revoke | You, through `wholesaleRecordDecision` |
| `v1:wholesale:entitlement` | What was granted, to whom, through which adapter | The pack, through `wholesaleProvisionEntitlement` |
| `v1:wholesale:wholesaleSettings` | Per-store: applications open, adapter, intro copy | You, through `wholesaleSetSettings` |

**An application has no state field.** Its state — `submitted`, `approved`,
`rejected`, `revoked` — is folded from its decision log by
`wholesaleApplicationState`. That is deliberate: an application whose state was
overwritten would lose *why* it was refused the moment somebody approved it.

## 1. Your own fields are your own concept

The first client collects an EIN. The next will not. So the pack collects the
**minimum an application needs to be one** — `companyName`, `applicantName`,
`applicantEmail`, plus the server-stamped `storeId` — and your fields live in
**your** domain, related to the pack's application:

```memql
use wholesale.concepts.{ application }

@version("1.0.0")
@description("Our own field on a wholesale application: the federal tax identifier we are required to collect.")
@rowAuthz(owner="ownerUserId", clusterOwner)
concept northwindTaxDetail {
  ownerUserId    string!
  applicationId  string!
  storeId        string!
  ein            string!

  @relationship(type="references", field="applicationId", target=application, direction="outgoing")
}
```

**Not a `metadata` blob on the pack's application.** A blob would take your
fields, then the next client's, and the pack's schema would drift toward
whoever asked first — untyped and unqueryable for everybody. A related concept
is typed, queryable, and invisible to every other client.

## 2. Your process is automations over the pack's concepts

The pack ships **no automations and no logic**. That is what leaves room for
yours. Trigger on the pack's concepts and call the pack's builtins; both are
public surface.

The simplest process — one owner decides, and that is that:

```memql
use wholesale.builtins.{ wholesaleProvisionEntitlement }

/// When the owner approves, entitle the buyer.
@trigger(event="node.created", concept="v1:wholesale:applicationDecision")
@filter(row => row.transition == "approve")
automation grantOnApproval {
  args {
    applicationId  any
    transition     any
  }
  granted := builtin wholesaleProvisionEntitlement(applicationId: args.applicationId)
}
```

A process with a review step, a threshold and somebody to tell looks different
and needs nothing new from the pack — open your own review row on
`v1:wholesale:application`, route it through
[`v1:commerce:approvalChain`](../../../dsl/commerce), and grant on the decision
exactly as above.

> **Reach for `dsl/commerce` directly.** It already ships `approvalChain`,
> `salesRep`, `territory`, `creditLimit`, `quote` and `reorderList`. The
> wholesale pack deliberately carries no copy of any of them: a pack with its
> own idea of an approval chain would be one you had to work around, and a
> client that wants no chain at all would be carrying it for nothing.

### Recording a decision

```
wholesaleRecordDecision(
  applicationId: "...",
  transition: "approve",       // approve | reject | revoke
  principalKind: "client",     // the only expressible kind
  decidedBy: "<user id>",
  note: "vouched for by the rep"
)
```

Four things the pack guarantees here, so you do not have to:

- **A Provider operator is refused.** `principalKind` admits only `client`.
- **`storeId` is copied off the application**, never supplied — and the read
  runs under *your* actor, so a caller who cannot read an application cannot
  decide it.
- **Illegal transitions are refused by name.** `approve` and `reject` are legal
  from `submitted`; `revoke` only from `approved`. Nothing leads out of
  `rejected` or `revoked` — a rejected applicant applies again, which is a new
  application with its own log.
- **Nothing is overwritten.** A decision is a new row, so the reason for the
  first one survives the second.

## 3. Choose an entitlement adapter

Approval grants wholesale prices; **how** is your choice, named in settings:

```
wholesaleSetSettings(
  storeId: "<store>",
  applicationsOpen: true,
  entitlementAdapter: "customerTag",   // or "shopifyB2B"
  introText: "Trade accounts are reviewed within two working days."
)
```

An unknown adapter name is refused **here**, when you write the setting, rather
than at approval time — the moment you are least able to act on it.

### `shopifyB2B` — Shopify's own B2B objects

Creates a company stamped with the MemQL application id, its location, the
buyer as a company contact, and an ACTIVE catalog with a price list. Idempotent:
a retry finds the first company rather than making a twin.

**It needs to read `v1:shopify:store.plan`**, which is `@rowAuthz(clusterOwner)`
and stays that way. Not because of the tier — since 2026-04-02 companies,
payment terms, volume pricing and up to three catalogs are available *below*
Plus — but because **below Plus there are at most three company-location
catalogs**, and the adapter reports that ceiling before it creates anything
rather than discovering it at the fourth grant, inside a live store. If the
caller cannot read the plan, this adapter refuses and says so.

**Revoking never deletes.** The catalog goes to `DRAFT`; the company, its
location, its contact, the price list and every order placed under them are
untouched. Revoking trade terms is ending a discount, not erasing a customer.

### `customerTag` — plan-independent

Adds one tag (`memql-wholesale` by default) to the buyer's customer record.
Works on any tier, and needs no plan at all — that is the whole point of it.

> **The tag entitles nobody on its own, and this is the one constraint the pack
> cannot enforce for you.** Key an **automatic discount, conditioned on the
> customer**, on that tag. **Never a discount code.** A shopper cannot assign
> themselves a tag — MemQL writes it through the push channel under the store's
> own credentials — but a code travels, and a code-based discount makes the tag
> decorative. The pack deliberately does not create the discount for you:
> creating a price rule on your behalf from an approval is a far larger act
> than the approval was.

**Draft orders are not an adapter**, and the reason is worth stating because it
comes up. An adapter is called at approval time and must report success or
failure then; a draft-order scheme provisions nothing until an order happens, so
the entitlement would sit `pending` for ever. Draft orders are an
order-*placement* mechanism, they belong in your process, and
`v1:commerce:quote` plus the connector's `CreateDraftOrderFromQuote` already
carry them end to end.

### What a failure means

| Row state | What happened | What to do |
|---|---|---|
| `granted` | The adapter reported success | Nothing |
| `pending` | The push failed transiently | Call `wholesaleProvisionEntitlement` again |
| `failed` | The adapter **refused** — a catalog ceiling, a buyer with no customer account | Fix the cause; retrying alone will not help |
| `revoked` | A revoke decision was recorded and acted on | Nothing |

**A failed push never un-approves an application.** An approval is a decision
somebody made; a push that failed is a fact about somebody else's API.
Collapsing the two would let an outage silently un-approve your customers.

## 4. The form contract

The pack declares **one shopper route** and no shopper read:

```
POST /_memql/forms/wholesale/application
```

on the site's own origin, as a plain HTML form post with **no JavaScript**.
It accepts exactly three fields — `companyName`, `applicantName`,
`applicantEmail` — and answers `303` to `/wholesale/thank-you` or, on refusal,
to `/wholesale/problem?reason=...`. **Both pages are yours**: a refusal renders
in your design and your language rather than ours.

`storeId`, `siteId`, `ownerUserId` and `id` are **stamped server-side and
refused as form fields**. `storeId` comes from the binding the post arrived
through — the serving binding, or the *preview* binding when the submission was
made while exercising a candidate version — which is what keeps an application
made under preview out of the live store's queue.

Your own fields — the EIN — do **not** go in this form. They go in your own
write, against your own concept, from your own route. See
[the shopper write path](building-a-pack.md) for what declaring one costs.

Submissions are refused when this store's `applicationsOpen` is false, and
**absent settings are closed**: a merchant who has never touched their wholesale
settings has not asked the internet for their customers' business details.

## 5. There is no public read of an application

`reviewspack` declares a form *and* a read, because a storefront renders
reviews. This pack declares a form and **no read**. An application is a named
person's business contact details against a named company; a shopper may submit
one and may not read one back, **not even their own**. Reading their own needs
the verified Shopify customer principal that memql#5550 recorded as the answer
for a buyer's own data and deliberately did not build.

Nothing forecloses it — reach is declared per route, so a second kind of caller
on a second route is an addition rather than a rewrite.

## The test your adoption is held to

`packs/wholesalepack/twoclients_test.go` runs the design's own question as a
test:

> Could two clients with different approval processes both express theirs
> without editing the pack?

Two fixture client domains live in
`packs/wholesalepack/testdata/clients/` — one approving on a single owner's say
and collecting an EIN as its own concept, one routing through an
`approvalChain` with a threshold and notifying a `salesRep`. The test
fingerprints the pack's own tree before and after both are mounted and
validated, and **fails if either fixture needed a change under the pack's
directory**.

Those two fixtures are the worked examples on this page, and they are executed
by CI rather than merely written down. If your adoption needs something they did
not, that is worth an issue against the pack — it may be a missing seam rather
than a missing field.

## See also

- [Building a pack](building-a-pack.md) — the pack contract, and the shopper write path
- [Component vs integration vs pack](../concepts/component-integration-pack.md) — the three words
- [Shopify storefront checklist](../operate/shopify-storefront-checklist.md) — what an operator does per store
