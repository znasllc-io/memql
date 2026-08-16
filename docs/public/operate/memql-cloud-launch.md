---
title: memQL Cloud -- the launch checklist
audience: public
status: stable
area: operate
sinceVersion: 0.13.0
owner: znas
---

# The memQL Cloud launch checklist

**Audience:** whoever signs off the first paying customer.
**Epic:** memql#3852. **This task:** memql#3857.

## The one hard gate

> **No public launch until metering is enforced end to end. An unmetered tenant
> is unbounded AI spend.**

Everything else on this page is a judgement call. This one is not, and it is the
reason the epic put it in writing before any of the work started.

**What satisfies it is not the meter.** The meter lags — usage is reported from
a tenant's cluster to the mothership over a network, at-least-once, eventually —
so a launch gated on metering alone is gated on a mechanism with a documented
failure mode where the meter stops and the model keeps being called.

What satisfies it is the **spend ceiling inside the tenant**: the tier's limit,
read by the [LLM guard](../ai/llm-cost-control.md) in-process, since boot, with
no network and no graph read.
[`TestEveryTenantRendersASpendCeiling`](https://github.com/znasllc-io/memql/blob/main/deploy/k8s/components/tenant/render_test.go)
asserts every rendered tenant carries one and mounts it, and
`tenant-provision.sh` refuses a zero — because
`MEMQL_LLM_MAX_TOTAL_CALLS=0` means *unlimited*, so the value that disables the
whole mechanism is the one an unset field produces.

## Items a test decides, not a person

Three checklist items are claims about the repository, and a claim that can be
checked and is not is one somebody ticks from memory at the end of a long day.
These are checked:

| Claim | Gate |
|---|---|
| A trial cannot meter overage | `TestTheTrialCannotMeterOverage` |
| Every metering tier prices its overage above zero | `TestEveryTierAllowanceIsBoundedAndPriced` |
| Every tenant renders and mounts a spend ceiling | `TestEveryTenantRendersASpendCeiling` |
| The runbook's price table matches the price list | `TestThePublishedPriceTableMatchesTheSeeds` |
| No public copy calls memQL a database | `TestNoDatabaseProductClaims` |
| The teardown sweep cannot act on the whole fleet | `TestEverySweepNarrowsItsCandidates` |
| Cross-tenant reads are impossible | `deploy/fleet/authz_test.go` |

```bash
go test ./deploy/... ./test/dslconformance/ .
```

## The checklist

### Money

- [ ] Stripe account out of test mode; tax and invoice settings configured.
- [ ] Products and prices created to match `deploy/fleet/dsl/fleet/seeds.memql`.
      **Not automated** — see [billing](memql-cloud-billing.md#what-is-not-built-yet).
- [ ] `MEMQL_INBOUND_SOURCE_STRIPE_*` configured, secret in the secret store.
- [ ] A test-mode checkout completes and the delivery is `signatureVerified`.
- [ ] Dunning exercised on Stripe's test clock: fail → email → grace → suspend →
      recover.

### Spend

- [ ] **The hard gate above.** Every tier's ceiling set and rendered.
- [ ] A trial exhausts its allowance and *throttles* rather than billing.
- [ ] The kill switch (`MEMQL_LLM_KILL_SWITCH_ENABLED`) verified on one tenant.

### Tenants

- [ ] `make up SERVERS=2` + a full provision → suspend → resume → teardown, unattended.
- [ ] A tenant's backups land in a **per-tenant** object-store prefix, and a
      restore drill runs from one.
- [ ] Wildcard DNS and TLS cover `*.<domain>` for tenant hostnames.
- [ ] The teardown path proven to take a final backup, and to **abort** when it fails.

### Copy

- [ ] Pricing page reads `publicTiers` live. **No number is copied into markup** —
      the query is `@public` for exactly this reason, and it is what makes "one
      place" true rather than aspirational.
- [ ] memQL described as an AI platform throughout. Zero "is a database" claims —
      this is a [licence-compliance](../../internal/ops/timescaledb-license-compliance.md)
      condition, not a style preference.
- [ ] Small print present: overage rates, trial terms, annual = two months free.
- [ ] ToS with the TSL notice and the DDL-prohibition clause; privacy policy.

### Operations

- [ ] Support channel and per-tier SLA wording agreed and published.
- [ ] Status/incident comms decision made and the page live.
- [ ] Someone on call, and they have read [the control plane runbook](memql-cloud.md).
- [ ] Analytics on the pricing page: page → trial start → conversion.
      Privacy-respecting; no third-party tracker on a page that will be read by
      people evaluating us on data handling.

## What is deliberately not on this list

**"Every automation exercised in production."** The lifecycle automations and
the five trial sweeps are authored, load through the engine's own `Init`, and
are gated — but their *step-result semantics at runtime* are proven by running
them, not by loading them. The parity-cluster run is the first item under
Tenants and it is the one that closes this.

**A dollar figure for the entry tier.** The epic models ~$143 → ~$90 for the
condensed profile. What is measured is the pod count: 8 against 13
([trials](memql-cloud-trials.md#what-solo-therefore-is-today)). Confirming the
money needs a running cluster and a month of billing, and signing off a modelled
number as measured is how the [stale provider costs](memql-cloud-billing.md#provider-costs-feed-the-margins)
this epic had to fix got there in the first place.

## Related

- [The control plane](memql-cloud.md) · [Billing](memql-cloud-billing.md) ·
  [Trials](memql-cloud-trials.md) · [Orbit](memql-cloud-orbit.md)
- [LLM cost control](../ai/llm-cost-control.md) — the guard behind the hard gate
- [Site hosting](site-hosting.md) — publishing the pricing page
