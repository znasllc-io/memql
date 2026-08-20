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
  campaignScheduleSend    template ready? audience sane?)
                            │ refuses here, or
                            ▼
                          v1:campaigns:sendJob
                          (queued — or `scheduled`, which
                           the worker promotes to queued
                           once the campaign's scheduledAt
                           has passed)
                            │
       drain worker  ──────▶│  every 15s, one batch per job
                            │
                            ├── roster page ⨯ its ledger   = who is left
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
| `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` | Signs the one-click unsubscribe link. **Required** — a send is refused without it. Rotating it needs the variable below; see [Rotating the unsubscribe signing key](#rotating-the-unsubscribe-signing-key). |
| `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS` | The previous signing key: **verified against, never signed with**. Optional, and unset only on a deployment that has never rotated. |
| `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` | Public origin the link points at, e.g. `https://api.example.com`. **Required.** Must be externally reachable: the recipient's mail client POSTs to it. |
| An email sender | `MEMQL_EMAIL_AZURE_*` + `MEMQL_EMAIL_SENDER` (Microsoft Graph), or `SMTP_*`. With neither, the node runs the `LogSender` and a send is refused. |

Tuning (all optional, all documented in [env-vars.md](env-vars.md)):
`MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`, `_BATCH_SIZE`, `_MAX_ATTEMPTS`,
`_MAX_AUDIENCE`, `_POLL_SECONDS`, `_STARTUP_DELAY_SECONDS`,
`_CLAIM_TTL_SECONDS`, `_THROTTLE_SECONDS`, `_SEND_TIMEOUT_SECONDS`,
`_ENABLED`.

Feedback and warming (both opt-in, both off with no value):
`MEMQL_CAMPAIGNS_FEEDBACK_SOURCES` ([feeding bounces back
in](#feeding-bounces-back-in)), `_WARMUP_ENABLED` and the `_WARMUP_*` ladder
and thresholds ([warming](#warming-a-new-sending-domain)),
`_SENDING_IDENTITY`.

Rotating the unsubscribe signing key is a two-variable operation, and doing it
with one variable breaks every link already sent. The procedure is
[below](#rotating-the-unsubscribe-signing-key).

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

### The audience is walked, not held

The send reads the roster **one page at a time**, through the engine's keyset
cursor, and reads the delivery ledger **for that page** rather than for the
campaign. Both halves matter: the ledger for a large campaign is exactly as
large as its roster, so cursoring one and not the other would move the ceiling
rather than remove it.

Before this, both reads were whole and bounded at 5000 rows — and a bounded
read of an unbounded set is a truncation, so a larger audience would have been
mailed as a silent prefix. The send refused instead, and "5000 is the largest
mailing list this can send to" was the price.

**Why ascending order is safe while the roster is being edited.** Reads
collapse to the latest version per id, so editing a recipient mid-send — which
this worker does, converging a suppressed address — moves it to a *newer*
timestamp, i.e. forward in an ascending walk, never backward past the cursor.
A row can therefore be seen twice and can never be missed. A duplicate costs a
comparison; a skip costs somebody their mail. Duplicates inside one batch are
de-duplicated, because those would be mailed before either send was recorded.

**The cursor is an optimization, not the idempotency mechanism.** It lives on
the send job so a tick resumes instead of re-scanning from the start. Losing
it, or having the engine refuse a stale one, costs one re-scan. What decides
who gets mailed is the ledger, below.

**`MEMQL_CAMPAIGNS_MAX_AUDIENCE` still exists, and now means something
different.** It was a correctness guard at 5000. It is now a deliberate
refusal at 250,000: no size is unsafe, but a send that large is more often a
mis-scoped audience or a bad import than an intent, and it cannot be recalled.
Raising it is a decision about how large a send you mean to make — not
something you have to do in three places to avoid silent truncation.

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
List-Unsubscribe: <https://api.example.com/unsubscribe?token=...>
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

### Rotating the unsubscribe signing key

The token names the key that signed it, and the node verifies against a ring of
two — `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` and the optional
`MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS`. Only the first ever signs.

```
u2.<keyId>.<owner>.<recipient>.<campaign>.<tag>
   ^^^^^^^ first 4 bytes of HMAC-SHA256(secret, "memql/campaigns/unsubscribe/key-id"), hex
```

The key id is a digest **of the key**, not a slot number or a counter. That is
deliberate: a link minted today is clicked on a node where that same secret has
since become the *previous* one, so a label like "current" would be wrong
exactly when it is needed, and a counter is one more value an operator can set
inconsistently across replicas. A digest of the key is true wherever the key is.
It leaks nothing — anyone holding a token already holds a 128-bit MAC over known
plaintext under the same secret.

**The procedure**

1. Set `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS` to the **current** value of
   `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET`.
2. Set `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` to the new value.
3. Apply **both in the same change** and roll every node that mints or serves
   unsubscribe links (the campaign worker and the bff).
4. **Leave `_PREVIOUS` set. Do not remove it later.**

Step 4 is where this differs from every other key rotation in the system, and it
is the whole reason this page has a section.

**How long does an old link keep working? Forever — until a second rotation.**

The window is counted in *rotations*, not in days. There is no time-based expiry
anywhere in the token, and adding one would defeat the point.

The usual advice — "drop the previous value once no unsent mail references it" —
has no true form here. An unsubscribe link does not expire when the send
finishes; it lives in the recipient's mailbox for as long as they keep the
message, which for a mailing list is indefinitely. So there is no date after
which retiring the old key is safe, and `_PREVIOUS` is not a migration window
that closes. It is a permanent second **reader** key.

What that buys, stated plainly:

| | Links signed by |
|---|---|
| Current key | keep working |
| Previous key | keep working |
| Any key retired by an earlier rotation | **dead, permanently** |

So the standing rule is **rotate at most once for any reason short of
compromise.** A second rotation retires the oldest key and does break every link
it signed — which is acceptable in exactly one case: the key leaked, and killing
those links is the point. If a routine second rotation is unavoidable, treat the
messages signed by the retiring key as messages whose opt-out is now
mailto-only, and say so in the next send's footer.

**What the engine tells you**

- At boot, the campaign worker warns once when this deployment holds only one
  key *and* has already sent campaign mail:

  ```
  WARN campaigns: this cluster has already sent campaign mail and holds only ONE
  unsubscribe signing key. MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS is unset,
  so changing MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET would invalidate every
  unsubscribe link already sitting in a recipient's mailbox ...
  ```

  It is silent before the first send (setting `_PREVIOUS` then costs nothing)
  and silent once `_PREVIOUS` is set.

- When a link arrives signed by a key nobody holds, the endpoint renders the
  ordinary "This link is not valid" page — never a 500 — and logs a warning
  carrying the token's `keyId` (which is not secret). One of those lines means a
  rotation dropped a key that had already signed live mail.

### SPF / DKIM alignment is structural

MemQL does not sign DKIM — the relay does, with the keys published for the
mailbox it authenticated as. What MemQL guarantees is that it never sets a
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

### Scheduling one

```
builtin campaignScheduleSend(campaignId: "<id>", scheduledAt: "2026-09-01T09:00:00Z")
```

or the **Schedule send** button in the editor. The send then starts on its own;
nothing else has to be pressed.

`scheduledAt` is an RFC 3339 instant. A value with no offset is read as **UTC** —
guessing a local zone from a node's `TZ` would make one string mean different
moments on different replicas.

Four things are worth knowing about it:

**The preflight runs when you schedule, and again when it fires.** Both, and
that is the point of scheduling being a builtin rather than a stored date: a
campaign whose template is still a draft is refused while you are looking at
the screen, not at 3am.

**A time already past is refused.** Use `campaignStartSend` to send now. A
backdated schedule is far more often a typo in the year or the offset than a
request to send immediately.

**The campaign row decides when it fires.** The send job carries a copy of the
time for you to look at; the worker compares the clock against
`v1:campaigns:campaign.scheduledAt`. So moving the date — with the editor, or
with `updateCampaign` — moves the send, and a schedule you postponed does not
go out on its original time.

**A campaign whose time passed while the cluster was down still sends**, when
the cluster comes back. There is no window past which a late send is dropped,
deliberately: a silently-skipped campaign is the failure this replaced. If you
no longer want it, cancel it — a campaign sitting in `scheduled` is a campaign
that is going to send.

If the fire-time preflight refuses, the reason lands on the **campaign row's
`lastError`**, where the operator who scheduled it is looking, and the two
kinds of refusal are treated differently by who can fix them:

| Refusal | Kind | What happens |
|---|---|---|
| Template un-readied, audience emptied or over the ceiling | authoring | campaign goes `failed` with the reason; fix and schedule again |
| No sender registered on the node, one-click unsubscribe unconfigured | environment | the send **waits** and retries each tick, reason stamped on the row |

The split is by who can fix it, not by severity. Failing a campaign because a
node booted without its mail credentials would make an operator re-author a
schedule to recover from a bad deploy.

Cross-replica, a due campaign fires once — and the claim is the least of the
three reasons. The send job's id **is** the campaign's id, so two replicas
promoting it write one row; the drain claim admits one replica per (job,
progress); and the delivery ledger is per (campaign, recipient) underneath both.

**Re-sending a campaign is not a thing.** The ledger is per (campaign,
recipient), so a second run finds every recipient terminal and mails nobody.
Author a new campaign.

---

## Feeding bounces back in

Bounces and complaints arrive out of band — Microsoft Graph's `sendMail`
returns `202 Accepted` and the bounce comes back to the sending mailbox
later. **Two formats parse out of the box** (memql#3461):

| Format | What it covers | Who produces it |
|---|---|---|
| `rfc3464` | Bounces, hard and soft | The standard delivery status notification. Microsoft Graph / Exchange, Postfix, essentially every SMTP-era relay. Cannot express a complaint — nothing bounced, so there is no DSN. |
| `ses` | Bounces **and complaints** | Amazon SES feedback over SNS. Carries the complaint feedback loop a DSN structurally cannot. |

Anything else needs a parser, and the honest consequence of not writing one
is that its bounces never reach the suppression list.

### Wiring a feed

1. Point the provider's feedback webhook at `POST /inbound/{source}`
   ([inbound delivery](inbound-delivery.md)) and configure that source's
   allowlist entry and signing secret there.
2. Name it as a feedback feed:

   ```bash
   MEMQL_CAMPAIGNS_FEEDBACK_SOURCES=postmaster=rfc3464,ses-feedback=ses
   ```

That is the whole wiring. A shipped automation
(`ingestCampaignFeedback`) fires on every staged inbound row and offers it
to `campaignIngestFeedback`, which parses and applies it.

**Listing the source here is the authorization**, and there is deliberately
no role check on top of it. The call asserts nothing: every address and every
verdict comes out of a body the provider signed with a secret you configured,
arriving at a source you allowlisted, in a format you named — three
deployment-level decisions, all made by whoever holds the env. A payload from
a source configured `scheme=none` is refused, because unauthenticated input
must not write a cluster-wide list. The most anyone can do by calling the
builtin directly is re-process a webhook you already trusted, which is
idempotent.

### Hard vs soft is the provider's word, not ours

| Provider says | Result |
|---|---|
| DSN `Status: 5.x.x`, or `Action: failed` | hard bounce → **suppressed** |
| DSN `Status: 4.x.x`, or `Action: delayed` | soft bounce → recorded, not suppressed |
| SES `bounceType: Permanent` | hard bounce → **suppressed** |
| SES `bounceType: Transient` or `Undetermined` | soft bounce → not suppressed |
| SES complaint | **suppressed** |

`Undetermined` counts as transient on purpose: SES is saying it could not
tell, and a permanent suppression on "could not tell" costs a real
subscriber, while the other error costs at most a few more attempts at a dead
address. Nothing anywhere classifies by reading the diagnostic **text** — a
parser that looked for "user unknown" would be inventing a verdict.

A bounce is attributed to its send through the `X-Campaign-Id` header the
sender stamps, which a DSN quotes back and SES echoes in `mail.headers`.
Best-effort: attribution improves a review and is never a precondition for
suppressing.

### What happens to a payload we cannot read

It is **not dropped**. The inbound row is stamped `failed` with the reason and
the call errors, so:

```
query inboundRequestsByStatus(status: "failed")
```

is the list of feedback this deployment could not understand. A payload that
reads fine and carries no bounce — a delivery receipt, an SNS subscription
confirmation — is stamped `processed` with what it was, because recording
that as a failure would teach you to ignore failures. An SNS
`SubscriptionConfirmation` says to visit its `SubscribeURL`, which is the one
message that means the wiring is working.

### By hand

An operator can still enter a report or suppress directly:

```
builtin campaignRecordFeedback(email: "...", kind: "hard_bounce", campaignId: "...")
builtin campaignSuppress(email: "...", reason: "manual", note: "...")
```

Both are admin-or-cluster-owner, because there the caller **is** asserting a
verdict.

## Reputation telemetry

Every send and every piece of provider feedback is counted per **(sending
identity, recipient domain, day)** on `v1:campaigns:reputationWindow`
(memql#3462). That is the breakdown that matters: mailbox providers judge a
sender independently, so "our bounce rate is 1%" is not a number any of them
acts on, while "our complaint rate at gmail.com is 0.4% this week" is.

```
query reputationWindowsSince(since: "2026-08-01")
```

Cluster-owner gated, and it carries a domain and four integers — no address
anywhere, the same property that makes the suppression list safe to keep
cluster-wide.

Two things it deliberately does **not** claim:

- **`accepted` is not `delivered`.** It counts what the transport took;
  Graph answers `202` and the bounce arrives hours later through the feedback
  path. Delivery is inferred as accepted minus bounces, and there is no
  `delivered` column because nothing on this side could honestly produce one.
- **Unsubscribes are not counted.** The one-click endpoint is a separate
  handler with its own store, so a column for them would be a column nothing
  writes. The ramp thresholds on bounces and complaints, which is what the
  providers themselves act on.

There is one counter row **per replica** (`nodeId`), written as an absolute
running total rather than an increment. Two replicas incrementing one shared
row would need a read-modify-write on the send path and would lose counts to
the interleaving; one row each, summed at read time, has neither problem.

---

## Warming a new sending domain

Warming is raising volume gradually on a new sending identity, so a provider
sees a growing trickle rather than a cold blast.

**There is now an automated ramp, and it is off by default.**

```bash
MEMQL_CAMPAIGNS_WARMUP_ENABLED=true
MEMQL_CAMPAIGNS_WARMUP_STEPS=5,10,25,50,100,200
```

An established sending domain does not want its rate re-derived by a control
loop that has never seen it, which is why you have to ask for it.

**It can only ever slow you down.** The effective rate is min(the current
step, `MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`). The ramp holds your configured
rate down while the evidence is thin; it never raises it past what you
allowed.

### It advances on evidence, not on a clock

A fixed-schedule ramp is a guess with a schedule attached, and it produces
the worst kind of false confidence — an operator believing they are warming
safely while the system has no idea whether it is working. This one advances
a step only when **all** of these hold:

| Condition | Knob | Why |
|---|---|---|
| The step has run long enough | `_WARMUP_MIN_HOURS_PER_STEP` (24) | Providers judge over time. A step they have not seen through a daily cycle has not been judged, however clean it looks. |
| It has sent enough to judge | `_WARMUP_MIN_VOLUME_PER_STEP` (200) | Four messages with no bounces is a hard bounce rate of 0.0 and no evidence at all. This is the condition that stops an empty numerator reading as a clean bill of health. |
| Every measurable domain is inside its thresholds | `_WARMUP_MAX_HARD_BOUNCE_RATE` (2%), `_WARMUP_MAX_COMPLAINT_RATE` (0.1%) | **Per domain, never in aggregate.** One bad domain inside a large healthy total is exactly what an aggregate hides, and it is the shape that gets a sending domain blocked. |

A domain over threshold **reduces** the ramp a step rather than merely
holding it. Holding at a rate that is already producing complaints is not a
neutral act.

A domain with less than `_WARMUP_MIN_DOMAIN_VOLUME` (50) of its own is
ignored, or one bounce at a domain we sent three messages to would read as
33% and pin the ramp forever. The cost — a genuinely bad small domain is
invisible to the ramp until it grows — is a real gap rather than a hidden
one.

### Reading what it decided

```
query warmupStateForIdentity(sendingIdentity: "sender@example.com")
```

carries the current `step`, its `ratePerMinute`, the last `decision`
(`started` / `held` / `advanced` / `reduced`) and a `reason` in words. `held`
is the common and healthy state, so the reason is written on every
evaluation: an operator looking at a step that has not moved for two days
should read why rather than guess.

A malformed `_WARMUP_STEPS` leaves the ramp **disabled** with a boot warning,
rather than being sorted into a ladder you did not write.

### Doing it by hand instead

Still perfectly reasonable, and unchanged:

1. Start `MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE` low (a few per minute).
2. Send to your most engaged recipients first — split the audience rather
   than sampling it, so the send order is deliberate.
3. Raise the rate over days, watching `reputationWindowsSince` and the
   `domain` breakdown on the suppression list.
4. Back off on any rise in complaints.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `startSend` refuses: "no one-click unsubscribe" | `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` / `_BASE_URL` unset. |
| `startSend` refuses: "no email sender registered" | Neither the Graph nor the SMTP credentials resolved on this node; the `LogSender` is not a sender for this purpose. |
| `startSend` refuses: template not ready | Mark the template `ready`. |
| `startSend` refuses: "over the ceiling" | The audience is larger than `MEMQL_CAMPAIGNS_MAX_AUDIENCE` (default 250,000). Nothing is technically wrong with a send that size — raise it if you mean it, or check the audience is scoped the way you think. |
| Scheduled campaign did not send | Read the campaign row's `lastError` — the fire-time preflight records its reason there. An environment refusal (no sender, no unsubscribe config) retries; an authoring one leaves the campaign `failed`. |
| Scheduled campaign sent later than its time | Expected after downtime: a late send is sent rather than dropped. The log line carries `lateBy`. |
| Campaign stuck at `sending`, counters not moving | Check the job's `throttledUntil` — the provider parked it. Otherwise check that a node with a configured sender is running the worker. |
| The warming ramp never advances | Read `warmupStateForIdentity`'s `reason` — it names which condition is unmet. Most often the step has not sent `_WARMUP_MIN_VOLUME_PER_STEP` messages yet, which is the ramp refusing to treat a clean rate over four messages as evidence. |
| The warming ramp is enabled and nothing paces | Check the boot log for a malformed `MEMQL_CAMPAIGNS_WARMUP_STEPS`; the ramp disables itself rather than inventing a ladder. |
| Bounces never appear on the suppression list | The source is not in `MEMQL_CAMPAIGNS_FEEDBACK_SOURCES`, or its format is misspelled (a misspelling is dropped, not defaulted). Check `inboundRequestsByStatus(status: "failed")` for payloads that arrived and could not be read. |
| Everything `skipped` | The cluster suppression list matched. Browse `v1:campaigns:suppression` as the cluster owner. |
| Recipient says the unsubscribe link does not work | `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` is not externally reachable, or the key that signed it has been retired by two rotations. Check the node log for `refused an unsubscribe link signed by a key this node no longer holds`; if the retired value can still be recovered, putting it back in `_PREVIOUS` revives those links. See [Rotating the unsubscribe signing key](#rotating-the-unsubscribe-signing-key). |
| Boot warns "holds only ONE unsubscribe signing key" | This deployment has sent campaign mail and has no `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS`. Nothing is broken yet; the next rotation of the secret alone would break every link already sent. |
