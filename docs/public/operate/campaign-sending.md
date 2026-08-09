---
title: Campaign sending
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Campaign sending

The engine that actually moves campaign mail: what it does, the product
decisions baked into it, what an operator has to configure, and what it
deliberately does not do.

memql#3323 modelled campaigns as concepts and shipped the authoring UI. It
stopped before sending, because sending is a set of product decisions rather
than UI. memql#3348 made those decisions and built the engine. Each one is
recorded next to the code that implements it; this page is the operator-facing
summary.

Related: [Environment variables](env-vars.md) ·
[Outbound delivery](outbound-delivery.md) (the *transactional* outbox, which
campaigns deliberately do not use — see below)

---

## The shape of a send

```
  operator                engine                          recipient
  ────────                ──────                          ─────────
  campaignStartSend  ──▶  preflight (sender? unsubscribe?
                          template ready? audience sane?)
                            │ refuses here, or
                            ▼
                          v1:campaigns:sendJob   (queued)
                            │
       drain worker  ──────▶│  every 15s, one batch per job
                            │
                            ├── roster  ⨯ delivery ledger  = who is left
                            ├── cluster suppression list   = who may not
                            ├── token bucket               = how fast
                            └── send ─────────────────────────────────▶ inbox
                                  │                                     │
                            v1:campaigns:delivery (one row per          │
                            recipient, terminal)                        │
                                                                        │
  suppression list ◀── POST /unsubscribe ◀── one-click ◀────────────────┘
```

---

## Configuration

Minimum to send anything:

| Variable | Why |
|---|---|
| `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` | Signs the one-click unsubscribe link. **Required** — a send is refused without it. |
| `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` | Public origin the link points at, e.g. `https://cockpit.example.com`. **Required.** Must be externally reachable: the recipient's mail client POSTs to it. |
| An email sender | `MEMQL_EMAIL_AZURE_*` + `MEMQL_EMAIL_SENDER` (Microsoft Graph), or `SMTP_*`. With neither, the node runs the `LogSender` and a send is refused. |

Tuning (all optional, all documented in [env-vars.md](env-vars.md)):
`MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`, `_BATCH_SIZE`, `_MAX_ATTEMPTS`,
`_MAX_AUDIENCE`, `_POLL_SECONDS`, `_STARTUP_DELAY_SECONDS`,
`_CLAIM_TTL_SECONDS`, `_THROTTLE_SECONDS`, `_SEND_TIMEOUT_SECONDS`,
`_ENABLED`.

**Rotating the unsubscribe secret invalidates every unsubscribe link already
sitting in a recipient's inbox.** There is no key-id or previous-secret
fallback. Set it once per deployment and treat it as a key without a rotation
story until one exists.

---

## The decisions

### Suppression is cluster-wide, not per-account

An unsubscribe is a statement by a person to *the operator of this
deployment*. Every campaign leaves through the same authenticated mailbox,
under the same `From` address, signed by the same SPF and DKIM records. A
per-operator list would let one operator mail an address that unsubscribed
from another, in a message the recipient cannot tell apart — and the complaint
lands on the one shared domain reputation. Legally the obligation attaches to
the sender, and there is one sender.

**It costs nobody an address.** `v1:campaigns:suppression` stores no mailbox:
the row id is the SHA-256 digest of the normalized address, and the only
human-readable field is the *domain*. So "which domains are bouncing?" is
answerable to a deliverability review and "who unsubscribed?" is not. A caller
can ask about an address only if it already holds it.

The list is `clusterOwner`-tier. Writing it requires admin or the cluster
owner (`campaignSuppress`, `campaignRecordFeedback`), or is done by the engine
itself from a verified unsubscribe. An ordinary operator's view of who left is
their own recipient rows' `subscriptionStatus`, which the next send converges
onto the list's verdict.

### Suppression is enforced at the point of send

Not at audience-build time. The worker checks the cluster list for every
recipient immediately before mailing, **before** it looks at the recipient
row's own `subscriptionStatus`. That ordering is what "outranks every
audience" means: a re-imported CSV produces a recipient row saying
`subscribed`, and the cluster list still refuses it. An audience assembled
last month cannot know about last week's unsubscribe.

A suppressed recipient gets a `skipped` delivery row with the reason, not a
silence — the operator is owed the outcome.

If the suppression lookup *fails*, the batch stops. An unanswerable list is
never read as an empty one.

### A hard bounce suppresses; it does not delete the membership

`campaignRecordFeedback(kind: "hard_bounce")` adds the address to the cluster
list and the next send marks that operator's recipient row `bounced`. The
audience membership stays.

Deleting it looks tidier and is wrong twice. It destroys the audit trail — an
operator whose delivery rate fell finds a smaller audience and no explanation.
And it makes the address *resurrectable*: the next import re-adds it as a
fresh `subscribed` row, and mailing a known-dead address is exactly what a
reputation system punishes. Keeping the row makes the sendable count drop
visibly with the reason attached; keeping the cluster suppression means a
re-import cannot undo it, because the list is consulted at send time rather
than at import time.

A **soft** bounce does neither. It is transient — a full mailbox, a greylisting
relay — and suppressing on one loses a real subscriber to a bad afternoon. The
per-recipient retry budget (`MEMQL_CAMPAIGNS_MAX_ATTEMPTS`) is what bounds
those.

### Idempotency is the ledger, not a retry flag

`v1:campaigns:delivery` has one row per (campaign, recipient), at a
deterministically derived id. The worker's batch is *"roster members with no
delivery row, plus rows still `pending` whose retry is due"*. A recipient that
reached `sent`, `skipped` or terminal `failed` is simply not in the next
batch.

So a resumed send, a restarted process, a paused-and-resumed campaign and a
claim lost to a dead replica all converge on the same behaviour, and none of
them needs anything to remember what it did. **The absence of the row is the
work queue.**

Cross-replica, a Postgres claim (`campaignSend`, keyed per job and progress
count, leased for `MEMQL_CAMPAIGNS_CLAIM_TTL_SECONDS`) means one replica
drains a given campaign at a time.

### Two rate limits, and both are needed

**Ours** is a token bucket, `MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`, per
process. It exists so the provider's limit is not *discovered* by exceeding it
on every send. Per-process rather than cluster-wide is correct for what it
defends: only one replica drains a given campaign, so a campaign is paced by
exactly one bucket.

**Theirs** is the 429. Microsoft Graph throttles a mailbox and answers `429`
with a `Retry-After`; the sender surfaces that as a typed error, and the
worker parks the whole job until the stated deadline and ends the batch. A 429
with no `Retry-After` is still a throttle and parks for
`MEMQL_CAMPAIGNS_THROTTLE_SECONDS`.

Ignoring the second is the failure the issue names: the loop keeps hammering,
the provider keeps refusing, and the retries eventually arrive as duplicates
in the recipient's mailbox.

### Unsubscribe: RFC 8058 one-click, plus a visible link

Every campaign message carries both halves:

```
List-Unsubscribe: <https://cockpit.example.com/unsubscribe?token=...>
List-Unsubscribe-Post: List-Unsubscribe=One-Click
```

and a link in the body. The header serves the mailbox provider's own
"unsubscribe" button; the link serves a person reading in a client that does
not surface it.

`GET /unsubscribe` renders a confirmation page. `POST /unsubscribe` performs
the opt-out. **A GET never unsubscribes anyone** — mail clients, link scanners
and security appliances prefetch URLs found in messages, which is precisely
why RFC 8058 specifies a POST.

The token is an HMAC over (owner, recipient, campaign). It is not stored:
storing one would mean a row per recipient per campaign, minted at send time
and never prunable, because a link in somebody's inbox has to keep working for
years. It does not expire, for the same reason — a stale unsubscribe link is a
compliance failure dressed as hygiene.

The endpoint is HTTP, which is a documented exception to the gRPC-first rule
for the same reason `POST /inbound/{source}` is: the caller is the recipient's
mail client, and there is no gRPC version of that conversation.

### SPF / DKIM alignment is structural

memQL does not sign DKIM — the relay does, with the keys published for the
mailbox it authenticated as. What memQL guarantees is that it never sets a
`From` address the relay did not authenticate as:

- the From **address** comes from the configured sender credential, inside the
  sender, and is not a parameter of the campaign path;
- `v1:campaigns:campaign` has no from-address field at all. It has `fromName`,
  a display name only;
- `replyTo` is per-campaign and is the correct escape valve: it steers replies
  without touching the authenticated identity.

**Your side of it is DNS.** Publish SPF and DKIM records covering the
configured mailbox, and a DMARC policy. There is no preflight for this because
any check the engine could run would be a guess about records it cannot see
from inside the cluster.

### Campaigns do not use the transactional outbox

`v1:platform:outboundRequest` and its worker deliver transactional mail.
Campaigns send directly instead, for three reasons:

1. the campaign worker has to **see** the provider's 429 and `Retry-After` to
   pace itself; a queue drained by another worker turns that into an opaque
   retry;
2. a **terminal per-recipient outcome** is the point of the delivery row, and
   an async seam leaves every row `pending` until somebody writes a
   reconciler;
3. `MEMQL_OUTBOUND_EMAIL_ALLOWLIST` is a domain allowlist for transactional
   mail. A marketing audience is by definition arbitrary external domains, so
   routing campaigns through it would either refuse every send or force that
   allowlist wide open — weakening the egress control it exists to provide.

`delivery.outboundRequestId` stays on the schema for a future queued
transport.

---

## Running a send

1. Author the audience, template and campaign in the portal
   (Integrations → Campaigns).
2. Mark the template **ready**. A draft is refused: half-written copy
   delivered to an audience is unrecallable, and this is the cheapest guard
   against it.
3. Press **Start sending**, or call the builtin:

   ```
   builtin campaignStartSend(campaignId: "<id>")
   ```

   The preflight refuses — with the reason — if no sender is registered, if
   one-click unsubscribe is unconfigured, if the template is not ready, or if
   the audience is empty or at the ceiling.
4. Watch the campaign row's counters, or the delivery ledger
   (`deliveriesForCampaign`).

Pause and resume:

```
builtin campaignPauseSend(campaignId: "<id>")
builtin campaignResumeSend(campaignId: "<id>")
```

Pausing leaves the ledger alone, so resuming continues exactly where it
stopped.

**Re-sending a campaign is not a thing.** The ledger is per (campaign,
recipient), so a second run finds every recipient terminal and mails nobody.
Author a new campaign.

---

## Feeding bounces back in

Bounces and complaints arrive out of band — Microsoft Graph's `sendMail`
returns `202 Accepted` and the bounce comes back to the sending mailbox
later. Wire the provider's feedback webhook to `POST /inbound/{source}`
([inbound delivery](inbound-delivery.md)) and have a DSL automation over
`v1:platform:inboundRequest` call:

```
builtin campaignRecordFeedback(email: "...", kind: "hard_bounce", campaignId: "...")
```

The automation has to present an **admin service-account credential**
([service-account JWT](auth/service-account-jwt.md)) — the suppression list is
cluster-wide, so writing it is a deployment-level action and the gate is a
role rather than a row predicate.

An operator can also suppress by hand:

```
builtin campaignSuppress(email: "...", reason: "manual", note: "...")
```

---

## Warming a new sending domain

There is **no automated warming ramp**, and that is a deliberate omission
rather than an oversight: a correct ramp is driven by reputation telemetry —
per-domain deliverability, complaint rate, throttle frequency over time —
which this deployment does not collect. A ramp built on a fixed schedule
instead is a guess with a schedule attached.

The manual procedure works today and uses the same knob:

1. Start `MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE` low (a few per minute) on a
   new domain.
2. Send to your most engaged recipients first — split the audience rather than
   sampling it, so the send order is deliberate.
3. Raise the rate over days, watching the skipped/failed counts on the
   campaign rows and the `domain` breakdown on the suppression list.
4. Back off on any rise in complaints.

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| `startSend` refuses: "no one-click unsubscribe" | `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` / `_BASE_URL` unset. |
| `startSend` refuses: "no email sender registered" | Neither the Graph nor the SMTP credentials resolved on this node; the `LogSender` is not a sender for this purpose. |
| `startSend` refuses: template not ready | Mark the template `ready`. |
| Job `failed`, "audience has reached the ceiling" | Split the audience, or raise `MEMQL_CAMPAIGNS_MAX_AUDIENCE` **and** the `paginate` bound on `audienceRosterForSend` **and** `MEMORY_ENGINE_MAX_WINDOW`. |
| Campaign stuck at `sending`, counters not moving | Check the job's `throttledUntil` — the provider parked it. Otherwise check that a node with a configured sender is running the worker. |
| Everything `skipped` | The cluster suppression list matched. Browse `v1:campaigns:suppression` as the cluster owner. |
| Recipient says the unsubscribe link does not work | `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` is not externally reachable, or the secret was rotated after the message was sent. |
