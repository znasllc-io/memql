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

## Organization permissions

Select an organization when creating campaign work or a group. Operators who
manage several organizations are asked to choose; MemQL never silently assigns
new work to the installation owner's organization. A member with access to just
one organization sees that organization selected.

A send requires access to the Campaigns app and permission to update data in
its organization. Reading a shared campaign or recipient list does not grant
permission to send. Campaigns, audiences, templates and sending identities
must belong to the same organization. The worker checks the campaign owner's
current authority again before each delivery, so revoked membership also
stops queued work. Test messages and single-recipient sends use the same
organization permission check.

Sending readiness is checked for the selected organization and sender. The
operator organization's mailbox status does not enable or block client mail.
Use **Check again** after changing a connection; the backend also repeats the
check before scheduling and sending.

Delivery records preserve the organization and authenticated owning user.
Editing a shared resource preserves its existing owner; each new version
separately records the user who made that change.
Rule-triggered deliveries name their email rule; they do not invent a campaign
relationship. Historical records are attributed only when an existing parent
proves which organization owns them.

## Templates in Productivity Tools

Create a named template in **Campaigns → Templates**, choose its organization,
then open it in the editor. File content lives in MemQL Productivity Tools:
**Source**, **Preview**, and **Split** use the same editable document. The OS
keeps the template list, organization, status, and archive controls. Files
settings select browser VS Code (the default), desktop VS Code, or Cursor.

**Create from examples** starts a Materializer draft from an image, individual
files, or an explicitly selected ZIP of resources plus your brief. MemQL reads
the authorized bytes and produces editable HTML, plain text, and a subject in
an `.email.json` file. A resource ZIP is inspected only for that explicit
composition; opening a ZIP in Files still downloads it intact. Private images
currently inform the design; this flow does not publish private image URLs or
attach those images to outgoing mail. Review placeholders before publishing.

Use **Use in Campaigns** to save a draft or publish the reviewed template for
its organization. Publishing makes it available to send; it does not send
mail. Editing a published template returns it to draft. Concurrent edits fail
with a comparison prompt, and publishing requires the exact saved revision.
An existing template cannot be moved to another organization by editing its
file. Create a separate copy instead.

Newly queued and scheduled sends keep a frozen copy of the ready template
reviewed at preflight. Later edits do not change those messages. The worker
still rechecks the caller's authority and the source template's readability.
Jobs queued before this change have no captured copy and retain their previous
behavior; pause and recreate those jobs before editing their shared template.

The client write surface is `campaignSaveTemplate`, with `templateId`,
`accountId`, `name`, `content`, `expectedRevision`, and `action` (`save`,
`publish`, or `archive`). Its receipt includes the saved revision and status.
Raw template writes and the underlying create/update mutations are internal.

## Connect an organization's Azure email

In **Campaigns → Senders → Connect email domain**, or **Settings → Connections →
Email**, choose the client organization first. These are two entrances to the
same connection. An owner or developer with write access to that organization
can configure it. Other campaign authors select its saved sending identities.

1. **Sign in with Microsoft.** Enter the displayed code on Microsoft's page.
   MemQL requests delegated Azure Resource Manager access under your Microsoft
   account. Your tenant may require an administrator's consent. No password,
   client secret, or subscription key is entered into the campaign form.
2. **Choose the subscription and resource group.** For accounts with access to
   multiple Microsoft directories, switch directories before selecting the
   subscription. You can select an existing resource group or name a new one
   and choose its region. Enter the client's email domain and select the email
   data location. The suggested service names can be changed.
3. **Review, then create.** MemQL registers the Communication resource provider
   if needed and creates the email service, delivery service, and custom-domain
   resource. Azure usage belongs to the selected subscription. MemQL does not
   convert the subscription, buy support, or send a message during setup.
   Repeating an interrupted step checks the same resources. A conflicting
   service owned outside this organization's connection is refused. Service names
   are reserved across cluster replicas before Azure creation, and remain assigned
   to that organization after disconnecting. If another operator changes the plan,
   review the new plan before creating its resources.
4. **Verify the domain.** Add the exact TXT/CNAME records Microsoft supplies at
   the domain's DNS provider, then choose **Check domain records**. Merge an SPF
   include into an existing SPF record instead of creating a second SPF record.
   Do not replace existing MX or website records. Domain, SPF, and both DKIM
   checks must succeed before the sender can be enabled. DNS propagation may
   require returning later; the connection plan is saved.
5. **Choose a sender.** For example, sender `news` on `client.com` sends from
   `news@client.com`. Set the client's display name and a reply mailbox that
   already receives mail, such as `help@client.com`. ACS is outbound email; it
   does not create a mailbox. `no-reply` is an optional name, not a requirement.
   The verified sender is saved in the ordinary Campaigns sender list.

The organization credential is encrypted on the cluster. Microsoft sign-in
sessions are tied to the exact MemQL user and organization, expire after one
hour once connected, and are shared across replicas. Closing a screen does not
lose the resource plan. A new Microsoft sign-in can resume it. Only the public
session identifier is retained by the browser; access tokens, refresh tokens,
and service keys are not returned to it.

A client send requires its own ready connection and a verified sender in that
connection. It never falls back to the operator's Graph/SMTP mailbox. ACS sets
its sender display name on the sender resource; a campaign override that does
not match is refused. Disconnecting clears the active connection credential and
stops future sends across replicas. It does not delete Azure resources, revoke
keys in Azure, or alter DNS. Historical encrypted record versions remain
subject to the installation's retention policy.

A successful ACS send currently means **accepted by Azure**, not delivered to
an inbox. An ambiguous submission response is not automatically submitted a
second time. Delivery feedback still needs the feedback pipeline described
below; the setup flow does not install an Event Grid delivery-feedback route.

The installation must have a registered first-party MemQL Microsoft application
before sign-in is available. This is installation/publisher configuration,
separate from each organization's connection. The flow uses Microsoft's device
code protocol and the existing MemQL stream; it adds no OAuth callback endpoint.
See Microsoft's [device authorization flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-device-code),
[email domain setup](https://learn.microsoft.com/en-us/azure/communication-services/quickstarts/email/add-custom-verified-domains),
and [email sending API](https://learn.microsoft.com/en-us/rest/api/communication/email/email/send?view=rest-communication-email-2025-09-01).

## Local test inbox

The local k3d overlay selects the capture email transport automatically. Open
**Email → Inbox** in MemQL OS as an owner or developer to inspect messages
generated by sign-in, invitations, campaigns and email rules. Messages are stored
in the installation's shared storage, encrypted with its existing master key;
capturing on one node and reading on another works without a separate mail server.

The inbox shows the latest 100 messages from the past seven days. Refresh it after
triggering mail, or return focus to the app. Open a message to inspect its sender,
recipient and body, and follow its verification link. Previews remove scripts,
remote images and styles, so opening test mail does not load tracking pixels.
Older encrypted rows are not deleted by the inbox's seven-day display window.

Capture sends no Internet mail, even when external provider credentials exist.
It is selected by the installation value `MEMQL_EMAIL_TRANSPORT=capture`, never
by a person's role or an email address. Storage or encryption failures fail the
send instead of claiming success. The ordinary external transport remains
`auto`; its Email app states that receiving has not been connected. Neither mode
changes bootstrap's email-validation exception or connects Outlook.

This is an operator test inbox and can contain other test users' sign-in links.
Ordinary users and admins cannot read it. It is not a connected personal mailbox.

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
| `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` | Public origin the link points at, e.g. `https://api.example.com`. **Required.** Must be externally reachable: the recipient's mail client POSTs to it. It is also the origin the open pixel and click redirects are built from — [tracking](#open-and-click-tracking) introduces no second variable, because two origins that can disagree is two ways for a message to carry a URL nothing serves. |
| An email sender | A ready Azure connection and verified sender for the campaign organization. Explicit local capture is also supported. Graph/SMTP remain available for the operator organization; a client campaign cannot borrow them. |

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

On a Microsoft Graph deployment there is one more, and without it the
suppression list quietly starves on the transport you actually send on:
`MEMQL_EMAIL_NDR_POLL_SECONDS` (default 300; `0` disables) runs the
[Graph mailbox reader](#the-graph-mailbox-reader), and the rows it stages
mean nothing until `graph-mailbox=rfc3464` is listed in
`MEMQL_CAMPAIGNS_FEEDBACK_SOURCES`.

Rotating the unsubscribe signing key is a two-variable operation, and doing it
with one variable breaks every link already sent. The procedure is
[below](#rotating-the-unsubscribe-signing-key).

---

## The decisions

### A campaign is one operator's, and the cluster owner can see it

The nine operator-facing concepts — `audience`, `recipient`, `template`,
`senderIdentity`, `campaign`, `delivery`, `consentEvent`, `engagementEvent`
and `emailRule` — carry the **composite** tier,
`@rowAuthz(owner="ownerUserId", clusterOwner)`. Every row's `ownerUserId` is
stamped from the caller's own actor rather than from an argument, so a
campaign belongs to whoever made it.

**What the second term buys, and what it deliberately does not.** A cluster
owner READS every operator's campaigns, and can drive the builtins on them —
`campaignStartSend`, `campaignPauseSend`, `campaignResumeSend`,
`campaignScheduleSend`, `campaignStats`, `campaignTestSend` — because each of
those is gated on "can the caller read the campaign row" and nothing else.
A cluster owner **cannot** rewrite another operator's campaign directly: the
write guard ignores the second argument, so `updateCampaign`,
`createTemplate` and every other row write stays owner-scoped.

That asymmetry is the point. The person who has to answer "why did this
client's send stop" needs to see the row and to be able to stop it; nobody
needs to be able to edit somebody else's copy behind their back. Without the
second term the oversight would not merely be unimplemented — a plain owner
tier has no cluster-owner escape on the READ path at all, so it would not be
*expressible*, and every fleet-wide campaigns view would silently render one
person's subset while looking complete.

Full team sharing — several people editing one campaign — is a deliberate
non-goal. This is oversight, not sharing.

The four engine concepts (`sendJob`, `suppression`, `reputationWindow`,
`warmupState`) stay `clusterOwner`-tier and are not readable by an ordinary
operator at all.

### Client opt-outs belong to the organization

An unsubscribe blocks marketing mail from the organization that sent it,
across that organization's audiences, campaigns, and sending identities.
It does not remove the same address from an unrelated client's list.
Both queued campaigns and event-triggered sends check this at send time,
even if a CSV import has recreated a subscribed membership.

The engine-owned `v1:campaigns:suppression` rows store an organization ID and
an email digest, never a plaintext mailbox. The composite row ID includes
both, preventing clients from overwriting each other's opt-outs. Reads bypass
per-node caches so a click handled by one replica is visible to another sender.

Existing rows without an organization remain cluster-wide safety blocks.
They are not silently narrowed or removed. `campaignSuppress` and provider
hard-bounce/complaint feedback continue to create these administrator-level
blocks. A matching block in either scope prevents delivery.

### Suppression is enforced at the point of send

Not at audience-build time. The worker checks both suppression scopes for every
recipient immediately before mailing, **before** it looks at the recipient
row's own `subscriptionStatus`. That ordering is what "outranks every
audience" means: a re-imported CSV produces a recipient row saying
`subscribed`, and the applicable suppression still refuses it. An audience assembled
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

New tokens bind owner, recipient, campaign, organization, and the original
normalized email digest with an HMAC. Moving, renaming, or deleting a recipient
cannot redirect the opt-out. Already-mailed `u2` links remain valid and derive
the organization from the recipient they originally named. An authoritative
suppression-write failure returns a retryable 503, never a success page.

The token is not stored:
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
u3.<keyId>.<owner>.<recipient>.<campaign>.<organization>.<emailDigest>.<tag>
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

- the From **address** is never a free parameter of a send. It is either the
  configured sender credential's own mailbox, or the `address` of a
  [sending identity](#sending-identities) an operator declared as a row —
  a closed registry, resolved server-side from the campaign, never a string
  the caller supplies;
- `v1:campaigns:campaign` still has no from-address field. It has
  `senderIdentityId`, which names a row, and `fromName`, which is a display
  name only;
- `replyTo` is per-campaign and is the correct escape valve: it steers replies
  without touching the authenticated identity.

**Your side of it is DNS, and it is now per identity.** Publish SPF and DKIM
records covering **every mailbox any identity sends as**, plus a DMARC policy
for each of those domains. Adding a sending identity for a client's domain
without their DNS records is the one way to make deliverability worse by
using this feature. There is no preflight for it, because any check the engine
could run would be a guess about records it cannot see from inside the cluster.

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

## Sending identities

One deployment, several clients, several mailboxes. A **sending identity**
(`v1:campaigns:senderIdentity`) is the operator's declaration that a mailbox
exists in this tenant and that campaigns may send as it:

| Field | Notes |
|---|---|
| `address` | the verified address the organization sends as. Normalized lowercase, validated for RFC 5322 shape **and** header safety — a CR or LF here would be header injection into every message the identity sends. It is also the reputation and warmup key, so two spellings of one mailbox would split its ramp in half |
| `fromName` | the From display name, e.g. `Acme News`. Required: a From with no phrase shows the raw mailbox to every recipient |
| `replyTo` | default Reply-To for campaigns that set none of their own. A campaign's own always wins |
| `accountId` | the organization this sender belongs to; must match its campaign and transport |
| `status` | `active` or `disabled` |
| `notes` | operator provenance. Never a credential |

**There is no secret material on the row, and that is the design.**
Azure credentials live in encrypted organization connection storage. The identity
row records the address and presentation; the transport still requires that
address to be in the organization's verified sender set. Graph/SMTP credentials
remain installation configuration for the operator's own mail.

### Resolution order, and why there is no fallback

```
campaign.senderIdentityId  →  the operator SAID which mailbox
(empty, operator org)      →  the operator organization’s configured default
(empty, client org)         →  refused: choose that client’s sending identity
```

That is the whole ladder. **The engine never infers an identity from a
campaign's `accountId`.** The authoring UI prefills the picker from the
selected account's identities, and prefill is UX while resolution is explicit
— an engine that guessed would mail a client's list from a mailbox nobody
chose, and would be right often enough that the wrong case went unnoticed.

Client campaigns require an explicit identity. Legacy campaigns belonging to the
operator organization can use its configured default.

**A missing or `disabled` identity is refused, never silently defaulted.**
Falling back to the cluster default would mail a client's audience from the
wrong mailbox, under the wrong From, against the wrong SPF and DKIM records,
and nothing would say so — the send would look completely successful. The
refusal is classified the same way every other preflight refusal is:

| What happened | Kind | What happens |
|---|---|---|
| the identity row is missing, or its `status` is `disabled` | authoring | the campaign goes `failed` with the reason on `lastError`; fix the campaign or re-enable the mailbox and start again |
| this node's sender is SMTP and the campaign names a non-default identity | environment | the send **waits** and retries each tick, reason stamped |

The second is an environment refusal because SMTP AUTH binds to one mailbox:
a node with the SMTP sender genuinely cannot send as anything else, and a
node that can may be running elsewhere in the cluster. Failing the campaign
for a deploy-shaped reason would make an operator re-author a schedule to
recover from it. The preflight runs at both authoring time and fire time, so
an identity disabled between the two is caught at the second.

### `disabled` is how you retire a mailbox

Not deletion. Past campaigns name the row, and the reputation history is keyed
on its address — `derivedSendingIdentity` resolves per send to the identity's
normalized address, so `reputationWindowsSince` and `warmupStateForIdentity`
break down per mailbox with no change on their side. They were built for
plurality and simply start receiving it.

Organization opt-outs apply across that organization's sending identities.
Cluster-wide safety blocks still apply across all identities.

### Organization controls access and sending

New campaigns, audiences, templates, and sender identities select an
organization. Read and write access follows the cluster's organization
authorization rules; the worker checks current send permission, and the
campaign, audience, template, recipient, and sender must agree on organization.
A client campaign cannot fall back to the operator's sender identity.


### Adding a sending identity

For ACS, use the guided connection above. It verifies the domain, creates the
Azure sender username, and saves the corresponding Campaigns identity. Creating
an identity manually does not prove that its address is verified.

The following instructions apply only to an existing **Microsoft Graph operator
mailbox** configuration. They do not configure a client's ACS connection.

1. **Create the mailbox** in the Microsoft 365 tenant that hosts your sending
   domain — the tenant `MEMQL_EMAIL_AZURE_TENANT_ID` names, which is not
   necessarily the tenant your Azure subscription lives in. A token issued for
   the wrong tenant is a 404 on a mailbox that plainly exists.

2. **Add it to the mail-enabled security group behind your
   `ApplicationAccessPolicy`.** This is the step that is easy to miss and
   silent when missed, because everything else works: the row saves, the
   picker offers the mailbox, the campaign starts, and every message fails at
   the provider.

   ```powershell
   Connect-ExchangeOnline -Organization <mailbox-tenant-domain>
   Add-DistributionGroupMember -Identity "memql-mail-senders@<domain>" `
     -Member "news@<domain>"

   # Verify BOTH directions. A policy that grants is not evidence it denies,
   # and replication can take ~30 minutes.
   Test-ApplicationAccessPolicy -Identity "news@<domain>"          -AppId <client-id>
   Test-ApplicationAccessPolicy -Identity "<a-real-person>@<domain>" -AppId <client-id>
   # want: Granted, then Denied.
   ```

   If you have no such group yet, the app is still scoped **tenant-wide** —
   `Mail.Send` (Application) is literally "Send mail as any user". Create the
   group and the policy first:
   [`Mail.Send` is tenant-wide until you scope
   it](azure-entry-install.md#mailsend-is-tenant-wide-until-you-scope-it).

3. **Publish SPF, DKIM and DMARC for the new mailbox's domain**, if it is a
   domain this deployment has not sent from before. A new identity on an
   already-authenticated domain inherits its records and needs nothing here.
   A new *domain* is a new reputation: see [SPF / DKIM alignment is
   structural](#spf--dkim-alignment-is-structural), and expect to
   [warm it](#warming-a-new-sending-domain) — the ramp keys on the identity's
   normalized address, so a new mailbox starts its own ramp rather than
   inheriting an established one's rate.

4. **Create the row.**

   ```
   mutation createSenderIdentity(
     address:   "news@<domain>",
     fromName:  "Acme News",
     replyTo:   "hello@<domain>",
     accountId: "<account id>"
   )
   ```

   `address` is normalized lowercase and is the reputation and warmup key, so
   two spellings of one mailbox would split its ramp in half. `fromName` is
   required — a From with no phrase shows the raw mailbox to every recipient.

**`Mail.Read` matters too, and only for the default mailbox.** [The Graph
mailbox reader](#the-graph-mailbox-reader) reads `MEMQL_EMAIL_SENDER`'s inbox
for delivery reports, so that mailbox needs `Mail.Read` under the *same*
policy group. Identity mailboxes do not need it yet: bounces for a send from
`news@` come back to `news@`, and the reader does not walk them, so a
non-default identity's hard bounces are not yet feeding suppression. Plan
around that until it does — the group membership is the only thing to change
when it lands.

**Retiring one is `status: "disabled"`, never a delete.** See
[`disabled` is how you retire a mailbox](#disabled-is-how-you-retire-a-mailbox).

---

## Importing recipients

```
builtin campaignImportRecipients(audienceId: "<id>", artifactId: "<id>", hasHeader: true)
```

The file is a CSV already uploaded to the Library. It is read **server-side
under the caller's own actor**, so a file the caller cannot read is a file
this cannot import — the artifact id is not a capability.

**A header row is required and the column mapping IS the header.** `email` is
required (case-insensitive); `displayName` and `name` are recognized; **every
other column lands verbatim in the recipient's `fields` map**, reachable from
a template as `{{fields.<key>}}`. There is no positional import, deliberately:
guessing which column holds addresses is how an import mails the wrong list.

Per row the address is normalized and shape-validated, then de-duplicated
against the audience's existing recipients **and** against earlier rows of the
same file — first occurrence wins, so a file listing somebody twice with two
different names does not produce two recipients.

**The import refuses whole rather than truncating.** If the resulting roster
would exceed `MEMQL_CAMPAIGNS_MAX_AUDIENCE`, nothing is written. A partially
imported list is one nobody knows is partial, and the send that follows is a
silent prefix of the audience the operator meant.

It returns `{added, duplicates, invalid, total}` plus up to twenty sample
invalid lines **with their line numbers**, so the next action is fixing the
file rather than guessing at it. Each added recipient gets a consent event
(`kind: "grant"`, `source: "import"`) — which is what finally gives
`source: "import"` a writer, and gives the compliance export a record of where
an address came from.

---

## Merge tags

The replacer set is **closed**, and is a `strings.NewReplacer` over enumerable
keys rather than a template engine — there is no expression evaluation in a
campaign body and there is not going to be:

| Tag | Value |
|---|---|
| `{{displayName}}` | the recipient's name, with a sensible fallback |
| `{{email}}` | the recipient's address |
| `{{campaignName}}` | the campaign's name |
| `{{accountName}}` | the tied account's name; **empty when untied**, which is the ordinary case |
| `{{fields.<key>}}` | one key of the recipient's `fields` map, as imported |

**The HTML and text parts escape differently, on purpose.** Every substituted
value is HTML-escaped on the HTML path and not on the text path, because the
text part is not markup and escaping it would show `&amp;` to a reader.
`displayName` and every `fields.*` value are recipient-supplied — an imported
CSV is untrusted input — so that split is a security property, not a
formatting one.

**An unknown tag stays literal in the body.** It is not an error and it is not
blanked, because blanking it would delete the evidence of the typo. The way to
catch one before the whole audience does is the test send, which reports every
tag it could not resolve.

---

## Test send

```
builtin campaignTestSend(campaignId: "<id>", to: "you@example.com")
```

Renders the campaign's template against a synthetic recipient — display name
`Test Recipient`, the address you name, and the `fields` of the audience's
first real recipient when one exists, so `{{fields.*}}` show the shape they
will actually have rather than blanks. The subject is prefixed `[Test] `, the
message goes through the campaign's **resolved sending identity**, and the
unsubscribe footer carries an obviously-inert token.

It writes **no delivery row** and touches **no counter**, so a test can never
make a campaign look partly sent. It does consume the ordinary send-rate token
bucket, because a test is a real message to a real mailbox.

It returns the list of merge tags it could not resolve. That is the check that
catches a typo'd `{{fields.compnay}}` while it is still cheap.

`to` is **required and never defaults to the caller's own address.** A builtin
that mails somewhere you did not name is one whose default you have to
remember.

---

## Reading what a campaign did

```
builtin campaignStats(campaignId: "<id>")
```

Every bucket that can be an exact count is one, at any audience size:

| Group | Buckets |
|---|---|
| roster | `recipients`, `pending` |
| transport | `sent`, `failed` |
| `skipped` | `suppressed`, `unsubscribed`, `other` |
| `bounces` | `hard` |
| feedback | `complaints`, `unsubscribed` |
| `opens` / `clicks` | `total`, `unique` |

They are computed server-side from the delivery ledger (status plus
`skipReason`), the campaign's own consent events, and its engagement events.
This replaces counting a page of rows in a browser, which under-reported every
campaign past the page bound and did so silently.

`skippedCount` also now lands on the campaign row itself, where it always
should have been: the worker has computed it per job since the sending engine
shipped and had nowhere on the campaign to put it, so the number an operator
most needs — the gap between the audience and what actually left — was visible
only on a row a browser cannot read.
`recipientCount - sentCount - skippedCount - failedCount` is what is still
outstanding.

### Two figures are honestly absent rather than rounded

**Unique opens and clicks can come back `unmeasured`.** They are folded from a
bounded read of the engagement rows, and a read that comes back *at* its bound
is a truncation. Reporting the folded number anyway would print a unique count
that is quietly a floor, on the one campaign large enough that the figure
mattered. So at the bound the answer is `unmeasured`, and the totals — which
are exact counts — are still there.

**There is no soft-bounce figure per campaign at all.** Not a zero: the field
is absent. Soft bounces are recorded and deliberately do not suppress, but
nothing attributes one to a campaign, so a `0` here would be read as "no soft
bounces" — a claim nothing in the system can make. An absent figure and a zero
are different answers, and a surface rendering this must not turn one into the
other.

---

## Open and click tracking

Two booleans on the campaign, both defaulting **true**: `trackOpens` and
`trackClicks`. An operator sending to a privacy-sensitive list turns them off
per campaign.

**The HTML part only.** At render time every `http(s)` href is rewritten to a
signed redirect and a 1x1 pixel is appended. The text part is untouched:
there is no honest way to count an open in plain text, and rewriting a URL a
reader can *see* would be visible mangling for a number nobody asked for.

The token is HMAC-signed by the same key ring the unsubscribe link uses, under
a **different context string**, so an unsubscribe token can never verify as a
tracking token or the reverse. Its payload carries the delivery, the campaign,
the kind and — for a click — the destination URL. **The destination is inside
the signed payload rather than in a query parameter**, which is what makes the
redirect open-redirect-proof: there is no unsigned input for an attacker to
aim.

| Endpoint | Behaviour |
|---|---|
| `GET /t/o/<token>` | **always** answers the 1x1 GIF; records the open only on a valid signature |
| `GET /t/c/<token>` | 302 to the signed URL; an invalid or tampered token renders the same "link is not valid" page an expired unsubscribe link gets — never a 500, never a redirect |

Both are served by the bff on the same public origin as `/unsubscribe`
(`MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL`), and both are HTTP for the reason
`/unsubscribe` is: the caller is the recipient's mail client, and there is no
gRPC version of that conversation. The token has to be a single path segment —
the self-authenticated exemption these routes rely on is bounded to exactly
one segment under the mount, so a token containing a `/` is not merely
mis-parsed, it is 401'd before the handler runs.

Hits land on `v1:campaigns:engagementEvent`, one row per hit, written under
internal origin. Uniqueness is by **delivery**, not by address: one address in
two audiences is two people as far as a campaign is concerned.

**What tracking does not tell you.** An open is a mail client fetching an
image — clients that block remote images never report one, and clients that
prefetch report one nobody read. It is a floor on engagement and a comparison
between sends, not a readership figure. There is no `delivered` event for the
same reason there is no `delivered` column on the reputation window: the
transport says accepted and a bounce arrives later.

---

## Running a send

1. Author the audience, template and campaign in the Campaigns app
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

   From a client, the same call is a generated SDK method — the builtin is
   marked `@sdk` (memql#4239), which is what puts it on that surface:
   `qc.CampaignStartSend(ctx, client.CampaignStartSendArgs{CampaignId: id})`
   in Go, `query.campaignStartSend({ campaignId })` in TypeScript. The pause,
   resume and schedule calls below have theirs too (`campaignPauseSend`,
   `campaignResumeSend`, `campaignScheduleSend`), and the app's buttons go
   through those methods rather than composing the call string by hand.
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

### The Graph mailbox reader

**On Microsoft Graph nobody dials you.** A failed delivery comes back as a
`multipart/report` DSN mailed to the sending mailbox, so the webhook path
above — which is the whole ingestion story — had nothing feeding it on the
transport this deployment actually sends on. Bounces landed in a mailbox and
were read by no one; the suppression list looked healthy because it was empty.

The reader closes that loop. Active only when the resolved sender is Graph
**and** campaigns are enabled, it lists the sending mailbox's inbox for unread
`multipart/report` messages every `MEMQL_EMAIL_NDR_POLL_SECONDS` (default
300; `0` disables, and a non-integer value falls back to the default rather
than to off), fetches each as MIME, and stages it as an ordinary
`v1:platform:inboundRequest` with `source: "graph-mailbox"`. Processed
messages are marked read and categorized, so a restart does not re-read the
mailbox from the beginning; the Graph message id is the dedupe key
underneath, so a redelivery collapses onto the same row.

Two variables and one Entra fact make it work:

```bash
MEMQL_EMAIL_NDR_POLL_SECONDS=300
MEMQL_CAMPAIGNS_FEEDBACK_SOURCES=graph-mailbox=rfc3464
```

**Staging a row is not acting on one.** Until `graph-mailbox=rfc3464` is
listed, the reader files DSNs the shipped automation then declines to
process — the same "listing the source here is the authorization" rule as
every other feed, applied to a feed the engine happens to fill itself. And
the mailbox must be readable: the Graph application needs `Mail.Read` under
the **same** `ApplicationAccessPolicy` that scopes `Mail.Send`
([azure-entry-install.md](azure-entry-install.md)), or the poll fails with a
403 the log names and the loop stays open.

**`signatureVerified` is stamped true on these rows, and that is honest
rather than a shortcut.** Provenance IS the verification here: the payload
was read out of our own mailbox over our own authenticated credential, and
there is no third-party signature to check. That field is what gates
`campaignIngestFeedback`, so a row that could not honestly carry it would be
staged and never acted on.

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
| Bounces never appear, and this is a Graph deployment | Nothing dials you on Graph. Check `MEMQL_EMAIL_NDR_POLL_SECONDS` is not `0`, that `graph-mailbox=rfc3464` is in `MEMQL_CAMPAIGNS_FEEDBACK_SOURCES`, and that the Graph app holds `Mail.Read` under the same `ApplicationAccessPolicy` as `Mail.Send` — a 403 on the poll is the usual answer. See [the Graph mailbox reader](#the-graph-mailbox-reader). |
| `startSend` refuses: the sending identity is missing or disabled | An authoring refusal, so the campaign goes `failed`. Re-enable the mailbox (`status: "active"`) or point the campaign at one that exists. It is deliberately not defaulted — see [resolution order](#resolution-order-and-why-there-is-no-fallback). |
| A campaign naming an identity waits forever on an SMTP node | An environment refusal: SMTP AUTH binds to one mailbox, so that node cannot send as anything else. The send retries each tick. Either run the send where the Graph sender is configured, or clear `senderIdentityId`. |
| Import reports `invalid` for every row | The header is missing or does not carry an `email` column. There is no positional import: the column mapping IS the header. |
| Import refused and nothing was written | The resulting roster would exceed `MEMQL_CAMPAIGNS_MAX_AUDIENCE`. It refuses whole rather than truncating; split the file or raise the ceiling if you mean it. |
| A merge tag appears literally in the delivered mail | It is not in the closed set, or `{{fields.<key>}}` names a column the import did not create. Run `campaignTestSend` — it returns every tag it could not resolve. |
| `campaignStats` says opens are `unmeasured` | The bounded engagement read came back at its bound, so a unique count would be a floor presented as a total. The `total` figures beside it are still exact counts. |
| Opens and clicks are all zero | `trackOpens` / `trackClicks` are off on that campaign, or the message went out as text only. Tracking is the HTML part only, by design. Also check `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` is externally reachable — it is the origin the pixel and redirects are built from. |
| Everything `skipped` | The cluster suppression list matched. Browse `v1:campaigns:suppression` as the cluster owner. |
| Recipient says the unsubscribe link does not work | `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` is not externally reachable, or the key that signed it has been retired by two rotations. Check the node log for `refused an unsubscribe link signed by a key this node no longer holds`; if the retired value can still be recovered, putting it back in `_PREVIOUS` revives those links. See [Rotating the unsubscribe signing key](#rotating-the-unsubscribe-signing-key). |
| Boot warns "holds only ONE unsubscribe signing key" | This deployment has sent campaign mail and has no `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS`. Nothing is broken yet; the next rotation of the secret alone would break every link already sent. |
