---
title: Inference cost and electricity estimates
description: Read API-equivalent and local-energy estimates separately from actual billing, with source dates and uncertainty.
audience: public
status: stable
area: operate
sinceVersion: 0.28.0
owner: znas
---

# Inference cost and electricity estimates

Fleet routing history and Apps session details distinguish an **API-equivalent
estimate** from **actual billing**. A signed-in app can report usage without
reporting a charge. A zero legacy cost field, subscription, or local machine
therefore does not establish free work. Existing records without the necessary
receipts stay unavailable; they are not retroactively filled from requested
models or current rates.

## API-equivalent estimates

For Apps and federated OpenAI/Anthropic inference, the calculator uses the
reported served model and a versioned observation of official API text prices.
The quote travels with the estimate: vendor, exact model ID, currency, source
URLs, checked date, refresh deadline, effective date when established, rates
and context multipliers. Checking a page today does not establish the date its
price took effect. Unknown model aliases remain unmapped.

Expand an estimate's assumptions and sources to inspect its retained rates,
context multipliers and token categories. Unreported cache counts display as
unavailable even when another category explicitly reports zero. Electricity
details retain the utility, tariff, timezone, effective period and applicable
rate bands alongside the energy source and allocation basis.

The initial catalog was checked on 22 October 2026 against the official
[OpenAI pricing](https://developers.openai.com/api/docs/pricing), the linked
model pages, and [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing).
It covers only the exact IDs recorded in `core/costestimate/prices.json`.
This is a standard global text-token equivalent; service tiers, region
premiums, tools, storage, taxes and subscription allocation are excluded.
It does not claim those exclusions have zero cost.

Input is normalized to include uncached input, cache reads and cache writes.
Known categories use their own rates. Missing cache counts or durations produce
a bounded range instead of fabricated zeroes. Request context thresholds use
that request's input count. Aggregate app/session totals cannot recover context
brackets or each request's model; their estimate assumes the reported model for
the aggregate and explicitly records that attribution limit. Mixed-model totals
need per-model receipts before they can support a measured attribution claim.

Quotes become stale at their refresh deadline. A stale calculation retains its
original source and is labelled stale; it does not silently become current.
Refresh the reviewed catalog from official sources, preserve the checked and
unknown effective dates, and run the calculator tests. Refreshing the catalog
affects new calculations; a historical receipt keeps its original quote.
Automatic vendor-price fetching is not implemented in this slice.

## Machine electricity settings

Open **Fleet → Machines → your machine → Settings → Electricity**. Enter the
machine's physical service address, city, region, postcode, country code and
IANA timezone. The owner verifies the location and authorizes utility research.
The street address is sealed in a separate owner-scoped profile, not placed on
the shared machine registration. Reading or attesting this private input
requires the owner's resolved user or personal-token authority; app-session,
badge and delegated tool credentials cannot do it.

Verification starts an ordinary MemQL goal through available authorized
inference doors. It receives locality and an optional provider, never the street
address. It searches official service-area and tariff sources and submits
bounded candidate evidence. A city alone does not prove which utility serves a
property. When candidates are ambiguous, the owner chooses the applicable
utility and tariff. If research is unavailable, enter the known provider and
retry. This workflow does not authorize purchases or routing changes.

Research records source URLs, currency, timezone, effective dates, checked and
refresh dates, and all applicable time-of-use or tier bands. The owner confirms
the tariff. Changing location, provider or lookup authorization clears earlier
findings and tariff selection. Delayed research for an old location revision is
refused. Save failures retain the local draft; concurrent saves require reload.

## Energy and uncertainty

Enter an explicit estimate of whole-machine watts during inference and its
source, such as an owner estimate informed by a power-meter sample. For a local
call with a measured routed-attempt wall duration (including dispatch/wait
time), the calculator uses:

```text
estimated kWh = watts × seconds ÷ 3,600,000
estimated electricity cost = estimated kWh × applicable price per kWh
```

The calculation layer also accepts measured kWh with a source and allocation
basis. This slice does not install a meter or collect automatic energy telemetry.
No power evidence means unavailable electricity cost. Tokens do not determine
energy. The router can read the caller's own machine profile; a borrowed
machine without a readable owner-confirmed profile remains unavailable.

When time-of-use or household-tier allocation is unknown, the result spans the
retained applicable bands. A tariff outside its effective period is unavailable;
an overdue refresh is stale. Fixed/demand charges and taxes are excluded unless
the rate evidence explicitly includes them. Whole-machine watts multiplied by
several overlapping call durations must not be summed as measured consumption.
Hardware purchase, cooling and other operating costs are outside this estimate.

The [evaluation record protocol](../overview/evaluation-case-records.md) retains
measurement scope, source, uncertainty and quote snapshots while excluding
precise addresses. Public results require a reviewed export. Focused required-database checks cover sealed persistence, owner admission,
research/confirmation, calculation and concurrent revisions in an isolated
PostgreSQL/TimescaleDB instance. Native CI and a live owner-confirmation and
utility-research exercise remain required before end-to-end verification.
