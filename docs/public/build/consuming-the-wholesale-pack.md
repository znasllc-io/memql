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
application, its decisions and access, published terms and price sheets,
private links, acceptance evidence, and email attempts. MemQL OS supplies the
store's Wholesale section. Your package supplies the storefront, extra form
fields and the automations that connect the store's approval process to these
builtins.

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

Eight concepts record the program:

| Concept | What it is | Who writes it |
|---|---|---|
| `v1:wholesale:application` | Somebody asking one store for trade terms | The shopper, through the pack's declared form |
| `v1:wholesale:applicationDecision` | One append-only decision: approve, reject or revoke | You, through `wholesaleRecordDecision` |
| `v1:wholesale:entitlement` | What was granted, to whom, through which adapter | The pack, through `wholesaleProvisionEntitlement` |
| `v1:wholesale:wholesaleSettings` | Per-store settings, documents policy, templates and sender | An operator sets up; the store's people change permitted settings |
| `v1:wholesale:documentVersion` | An immutable published terms, MAP-policy or price-sheet version | `wholesalePublishDocument` |
| `v1:wholesale:privateLink` | A token hash and the application/purpose it opens | The pack when handing over an email |
| `v1:wholesale:acceptance` | The accepted versions, hashes, statement and submitted evidence | `wholesaleAcceptTerms` |
| `v1:wholesale:emailSend` | One applicant email attempt and its outcome | The pack's send and resend builtins |

**Call the builtins, not the mutations.** The builtins check the caller,
store, current state and version before writing. The pack's concepts are
`@serverWritten` and its mutations are `@serverOnly`; a client writes through
the declared shopper forms or the admitted builtins.

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
@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")
@belongsTo(account, stated)
concept northwindTaxDetail {
  ownerUserId    string!
  applicationId  string!
  storeId        string!
  accountId      string
  ein            string!

  @relationship(type="references", field="applicationId", target=application, direction="outgoing")
}
```

Declare `accountId` on the extension's concept and mutation. The shopper
handler supplies it from the admitted site's organization; never accept a
shopper-selected organization. Keep the extension related to `applicationId`
and pass the server-stamped account into its write.

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
@filter(row => row.transition == "approve" && args.firstVersion == true)
automation grantOnApproval {
  args {
    applicationId  any
    transition     any
    firstVersion   bool
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

### The emails and notices run on the first version

The pack has ten sends and notices for your automations to call: the four
applicant emails (`wholesaleSendTermsEmail`, and `wholesaleSendBuyerEmail` for
`approved`, `rejected` and `revoked`) and the six staff notices of
`wholesaleNotifyStaff`. For three of them -- the approval email and the
`accessNotGranted` and `accessNotRemoved` notices -- the pack itself refuses to
mail for an access row written before it sent email. The other seven -- the
terms email, the rejection and removal emails, and the `readyToReview`,
`approved`, `rejected` and `revoked` notices -- are sent whenever they are
called for an application in the right state. The engine publishes
`node.created` for every version of a row, and the pack writes new versions of
old rows (`wholesaleBackfillAccounts`), so an automation that calls any of
those seven triggers on the application, acceptance or decision row and filters
on `args.firstVersion == true`, as the pack's own test clients do.

### Recording a decision

```
wholesaleRecordDecision(
  applicationId: "...",
  transition: "approve",       // approve | reject | revoke
  note: "vouched for by the rep"
)
```

The stored `decidedBy` is the signed-in caller. The older `principalKind`
and `decidedBy` arguments are ignored. Cluster operators cannot approve,
reject, add or remove the store's buyers; the store's own people decide.

Five things the pack guarantees here, so you do not have to:

- **A cluster operator is refused.** The caller must be one of the store's people with the wholesale part.
- **`storeId` is copied off the application**, never supplied — and the read
  runs under *your* actor, so a caller who cannot read an application cannot
  decide it.
- **Illegal transitions are refused by name.** `approve` and `reject` are legal
  from `submitted`; `revoke` only from `approved`. Nothing leads out of
  `rejected` or `revoked` — a rejected applicant applies again, which is a new
  application with its own log.
- **Nothing is overwritten.** A decision is a new row, so the reason for the
  first one survives the second.
- **One approved application per address on a store.** An `approve` is
  refused while another application of the same address (compared trimmed and
  lower-cased) on the same store is approved, with the sentence
  "Application `<id>` of this address is already approved on this store.
  Remove that buyer before approving this application, or reject this one."
  Two posts in one instant can both be written; this is where the second
  approval is stopped, so a revoke of one never takes the buyer's tag off
  while the other still reads granted. A `reject` or a `revoke` is never
  refused by it.

## 3. Choose an entitlement adapter

Approval asks the chosen route to grant trade prices. An operator sets up
the store and chooses its route in **Wholesale > Settings**, or through:

```
wholesaleSetSettings(
  storeId: "<store>",
  siteId: "<site serving this store>",
  applicationsOpen: true,
  entitlementAdapter: "customerTag",   // or "shopifyB2B"
  introText: "Trade accounts are reviewed within two working days."
)
```

An unknown adapter name is refused **here**, when you write the setting, rather
than at approval time — the moment you are least able to act on it.

### `shopifyB2B` — Shopify's own B2B objects

Creates a company stamped with the MemQL application id, its location, the
buyer as a company contact, and an ACTIVE catalog with a price list. A retry
finds an existing company by application id. A partial failure can still leave
catalog objects requiring operator inspection; this is not an atomic Shopify
transaction.

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

Finds the customer by the applicant's email, or creates the missing customer,
then adds one tag (`memql-wholesale` by default). Revocation removes the tag
from the customer recorded by the grant, even if their email changed.
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
| `pending` | The push has not landed and may yet: `shopify_error` when the store, Shopify or the connector failed in a way that can pass (an outage, a timeout, an answer nobody could read), or `no_route` when a revoke must go through an adapter this engine no longer has | Press Retry access on the application in the OS (`wholesaleRetryAccess`) |
| `failed` | Nothing was pushed, or the adapter **refused**, and the row names why: `no_settings` (the store has no wholesale settings), `no_route` (its settings name no adapter, or one this engine does not have), `plan_ceiling` (a catalog ceiling, or a plan the B2B adapter cannot read), `customer_refused` (the store refused to create the buyer's customer), `scope_missing` (the store refused access to its customers: the permissions are not approved, or the app is not approved for protected customer data), `shopify_error` (the connector is not configured, or a B2B revoke has no reference to act on). A buyer with no customer is not a refusal: the customer-tag route creates one and tags it | Fix the cause; retrying alone will not help |
| `revoked` | A revoke decision was recorded and acted on | Nothing |

**A failed push never un-approves an application.** An approval is a decision
somebody made; a push that failed is a fact about somebody else's API.
Collapsing the two would let an outage silently un-approve your customers.

## Operating the program

The Wholesale section has **Applications**, **Buyers**, **Documents** and
**Settings**. Grant `execute app:deployables/wholesale` to the store's group
in **Users > Groups**. The engine checks the site's organization and store on
every call. Cluster operators can inspect the program, set it up, choose its
route and sender, and retry access; they cannot decide for the store. The
engine's `operator`, `may.publish`, `may.export` and action lists are the
screen's authority, including whether a document can be published or records
exported.

Applications are shown as **Waiting for terms**, **Ready to review**,
**Approved**, **Rejected** or **Removed**. These stages combine the decision
log with the terms rule; they do not replace the entitlement's separate
`granted`, `pending`, `failed` or `revoked` state. Add buyer records a new
application and follows the same terms and approval path; it does not grant
trade prices immediately. Remove records a revoke decision. A client's revoke
automation must call `wholesaleRevokeEntitlement`; Retry access can also apply
the recorded decision.

### Terms and links

Publish versions of **Terms**, **MAP policy** and **Price sheet** in Documents.
A version has a label, Markdown body and content hash; publishing another never
rewrites what someone accepted. When terms are required, all three documents
must exist before applications open, and approval waits for acceptance. When
terms are not required, an application is ready to review immediately. Switching
the rule does not fabricate or erase an earlier acceptance.

A terms email opens `/wholesale/terms?t=<token>`. Its link lasts 30 days by
default (Settings permits 1 to 365), ends when replaced, accepted or when the
application is decided, and shows the current terms and MAP versions. The
acceptance records their ids and hashes, the statement, typed name, time and
edge-observed address/browser. The pack stores only a hash of the token.
An approval email opens `/wholesale/price-sheet?t=<token>`; that link has no
fixed expiry and opens only while the application is approved and access is
granted. Re-sending replaces the corresponding link. Links are minted when
the row lane hands over the message, including a rate-limited replay.

### Applicant emails and staff notices

Choose published **transactional** templates for terms, approval, rejection
and removal, and an organization sender. Each template may use
`{{fields.companyName}}`, `{{fields.applicantName}}`, `{{fields.signInEmail}}`,
`{{fields.storeName}}`, `{{fields.termsUrl}}`, `{{fields.termsExpiresOn}}` and
`{{fields.priceSheetUrl}}`. Only the relevant link is populated for each kind.
The current owner of the site serving the application is the sending actor;
if that site no longer serves the store, the pack resolves a current serving
site. A missing site, an ambiguous owner or missing organization write
permission is reported as not sent, with a remedy. Changing the sending identity
is an operator act. Settings explains what would be sent now and why not.

An automatic send is attempted once per application and kind. The application's
Emails section lists attempts and the lane's current delivery evidence. A
provider hand-over with no answer is **uncertain**, not proof that nothing was
sent; a resend warns that a second copy may arrive. Terms and approval emails
can be sent again; rejection and removal can be resent when they did not reach
the recipient. A rate-limited send remains deferred while the lane holds it.
The offered resend acts and the resend call use the same checks.

Staff notices use the transactional outbox and the store's `staffEmail`. Its
address must be admitted by the installation's outbound email allowlist;
Settings shows whether it is. Supported events are `readyToReview`, `approved`,
`rejected`, `revoked`, `accessNotGranted` and `accessNotRemoved`. Custom subjects
and bodies accept `{{companyName}}`, `{{applicantName}}`, `{{applicantEmail}}`,
`{{state}}`, `{{decidedBy}}`, `{{note}}` and `{{adminUrl}}`; other placeholders
are left as written. `adminUrl` opens the application's page in MemQL OS.

### Complete event wiring

Use one first-version filter on each application, acceptance and decision
trigger. Entitlement notifications must also see later attempts, so they do
not use that filter. The engine emits `node.created` for each written version;
these examples therefore cover both initial and later access outcomes without
separate `node.updated` subscriptions.

| Trigger | Filter in addition to first-version where indicated | Calls |
|---|---|---|
| Application, first version | none | `wholesaleSendTermsEmail`; `wholesaleNotifyStaff(event: "readyToReview")` for a no-terms program |
| Acceptance, first version | none | `wholesaleNotifyStaff(event: "readyToReview")` |
| Decision, first version | `transition == "approve"` | `wholesaleProvisionEntitlement`; `wholesaleNotifyStaff(event: "approved", decisionId: args.id)` |
| Decision, first version | `transition == "reject"` | `wholesaleSendBuyerEmail(kind: "rejected")`; `wholesaleNotifyStaff(event: "rejected", decisionId: args.id)` |
| Decision, first version | `transition == "revoke"` | `wholesaleRevokeEntitlement`; `wholesaleSendBuyerEmail(kind: "revoked")`; `wholesaleNotifyStaff(event: "revoked", decisionId: args.id)` |
| Entitlement, every version | `state == "granted" && attempt > 0` | `wholesaleSendBuyerEmail(kind: "approved")` |
| Entitlement, every version | `direction == "grant" && (state == "pending" || state == "failed")` | `wholesaleNotifyStaff(event: "accessNotGranted")` |
| Entitlement, every version | `direction == "revoke" && state != "revoked"` | `wholesaleNotifyStaff(event: "accessNotRemoved")` |

Every call takes `applicationId`; on an application trigger use `args.id`, on
an acceptance, decision or entitlement use `args.applicationId`. The builtins
check that each event is appropriate before sending. For example, removal
wiring is:

```memql
use wholesale.builtins.{ wholesaleRevokeEntitlement, wholesaleSendBuyerEmail, wholesaleNotifyStaff }

@trigger(event="node.created", concept="v1:wholesale:applicationDecision")
@filter(row => row.transition == "revoke" && args.firstVersion == true)
automation revokeAfterRemoval {
  args {
    id any
    applicationId any
    transition any
    firstVersion bool
  }
  removed := builtin wholesaleRevokeEntitlement(applicationId: args.applicationId)
  sent := builtin wholesaleSendBuyerEmail(applicationId: args.applicationId, kind: "revoked")
  told := builtin wholesaleNotifyStaff(applicationId: args.applicationId, event: "revoked", decisionId: args.id)
}
```

The independent access-outcome triggers still run if a provision or revoke
records failure. Do not put those notices only after a successful adapter call.

### Builtin reach

| Reach | Builtins |
|---|---|
| Shopper routes under the serving site's owner | `wholesaleSubmitApplication`, `wholesaleTermsForLink`, `wholesaleAcceptTerms`, `wholesalePriceSheetForLink` |
| Store's wholesale part, with the call's organization and actor checks | `wholesaleList`, `wholesaleGet`, `wholesaleBuyers`, `wholesaleDecisionLog`, `wholesaleStoreSettings`, `wholesaleUpdateSettings`, `wholesaleRecordDecision`, `wholesaleRetryAccess`, `wholesaleDocuments`, `wholesalePublishDocument`, `wholesaleExportStore`, `wholesaleAddBuyer`, `wholesaleRemoveBuyer`, `wholesaleResendEmail` |
| Trusted automation | `wholesaleProvisionEntitlement`, `wholesaleRevokeEntitlement`, `wholesaleSendTermsEmail`, `wholesaleSendBuyerEmail`, `wholesaleNotifyStaff` |
| Cluster operator or trusted maintenance call | `wholesaleSetSettings`, `wholesaleBackfillAccounts` |
| The caller's readable application | `wholesaleApplicationState` |

### Upgrading existing records

Deploy the client's first-version filters before calling
`wholesaleBackfillAccounts`. The backfill ties the pack's old rows to the
organization of their admitted store/site and fills missing event-time fields
in the same fenced write. It preserves `submittedAt`, `decidedAt` and the
entitlement's existing times, so rewriting a row cannot reorder decisions or
make a removed buyer look approved. A stopped run can be repeated.

Read its report before granting the wholesale part. Rows that cannot be tied
remain listed with a reason; a second settings row for one store must be
resolved by an operator rather than silently choosing between organizations.
Export in Settings > Records includes the program's settings, applications,
decisions, access, documents, acceptance evidence and email attempts. Keep the
export private: these are business contacts and decision records.

## 4. The form contract

The application form is one of the pack's four shopper routes:

```
POST /_memql/forms/wholesale/application
```

on the site's own origin, as a plain HTML form post with **no JavaScript**.
It accepts exactly three fields — `companyName`, `applicantName`,
`applicantEmail` — and answers `303` to `/wholesale/thank-you` or, on refusal,
to `/wholesale/problem?reason=...`. **Both pages are yours**: a refusal renders
in your design and your language rather than ours.

`storeId`, `siteId`, `ownerUserId`, `accountId` and `id` are **stamped server-side and
refused as form fields**. `storeId` comes from the binding the post arrived
through — the serving binding, or the *preview* binding when the submission was
made while exercising a candidate version — which is what keeps an application
made under preview out of the live store's queue.

Your own fields — the EIN — do **not** go in this form. They go in your own
write, against your own concept, from your own route. See
[the shopper write path](building-a-pack.md) for what declaring one costs.

Submissions are refused as `applications_closed` when this store's
`applicationsOpen` is false, and **absent settings are closed**: a merchant who
has never touched their wholesale settings has not asked the internet for their
customers' business details. Every refusal code each route can answer is in
[What each refusal means](#6-what-each-refusal-means).

**One open application per address on a store.** A post from an address that
already has a `submitted` or `approved` application on the store — compared
trimmed and lower-cased, the same rule a staff member's *Add buyer* is refused
by — writes nothing, your extension's row included, and is answered exactly as
an accepted post: the same `303` to `/wholesale/thank-you`. Telling the poster
"this address already applied" would tell anybody typing somebody else's
address who your wholesale buyers are. The instance's log holds one line naming
the store and the open application, never the address; a store's own people
may not be able to read that log. A `rejected` or `revoked`
application blocks nothing: the business applies again.

## 5. There is no public read of an application

The pack exposes the application form, the terms read and acceptance form,
and the price-sheet read. The reads require a private token and return only
the documents and limited context that purpose needs. They do not return the
application, its contact details, decision notes or email history. A shopper
may submit an application but cannot query applications, including their own.
Reading a buyer's own account would require a separate verified customer
principal and an explicitly admitted route.

## 6. What each refusal means

A storefront is a static site: all it knows about a refusal is the `reason` a
form redirects with (`?reason=` on its error page) or the `error` a read
answers (`422` with `{"error": "<code>"}`), and it words each code in its own
design and language. Each code below means one thing, so a page can word it
truthfully. A route forwards only the codes it lists; any other error reaches
the page as the surface's `failed`.

The surface answers four codes itself, before the pack is reached, on every
route: `invalid` (a declared field broke its own rule: a required field is
empty, a value is too long, a box is not `on`, a fingerprint is not 64 hex
characters), `too_large`, `unavailable` and `failed`. A route may not declare
one of these four; the surface refuses such a declaration when the pack
registers.

### The application form

`POST /_memql/forms/wholesale/application`; error page `/wholesale/problem`.

| `reason` | What happened | What the page should say |
|---|---|---|
| `applications_closed` | The store takes no application through this site now: its switch is off, it is not set up, the site is not one the store admits, or it requires its terms and a document is not published. The four are answered alike, so a visitor cannot tell one from another | The program is not accepting applications at the moment |

A post from an address with an open application is not a refusal: it is
answered as an accepted post (see [the form contract](#4-the-form-contract)).

### The terms read

`GET /_memql/reads/wholesale/terms?requestToken=<token>`, the read a terms link's
page makes. On success it answers the company name, the current terms and MAP
policy (`kind`, `label`, `body`, `sha256` each), the acceptance `statement` and
its `statementSha256`, and the link's `expiresAt`.

| `error` | What happened | What the page should say |
|---|---|---|
| `link_used` | The terms were accepted through this application's link. Checked first, so it stays the answer whatever happens to the application after the acceptance: approved, rejected or revoked. It says nothing about a decision | The terms are already accepted; nothing more to do here |
| `link_closed` | The link is real, and its application is no longer waiting for its terms: rejected, approved without an acceptance, or removed | This link no longer applies |
| `link_expired` | The link is past its expiry, and its application is still waiting for its terms | Ask the store for a new link |
| `link_invalid` | The token is no link of this store: unknown, malformed, replaced by a newer link, or another store's, never told apart. Also a link that resolves but whose application cannot be read on this store | This link is not valid |
| `documents_unavailable` | The store has no current terms or MAP version to show. A new link would not help | The terms are not available at the moment |

A terms link is judged in the order `link_used`, `link_closed`, `link_expired`.

### The acceptance form

`POST /_memql/forms/wholesale/acceptance`; success page `/wholesale/accepted`,
error page `/wholesale/link-problem`. The page posts the `requestToken`, both
boxes as `on` (`acceptTerms`, `acceptMap`), the `signedName`, and **the three
fingerprints the terms read answered**: `termsSha256`, `mapSha256` and
`statementSha256`. The acceptance records the versions and the sentence those
fingerprints name, so what is recorded is what the applicant was shown.

| `reason` | What happened | What the page should say |
|---|---|---|
| `link_used`, `link_closed`, `link_expired`, `link_invalid`, `documents_unavailable` | As on the terms read. `link_used` is also the answer to the second of two acceptances posted at once | As on the terms read |
| `terms_changed` | A document or the acceptance sentence changed between the read and the post. Nothing is written and the link stays usable | The terms have changed; read them again and accept the current ones |
| `invalid` | The surface's own code: a field broke its rule | Check the form and send it again |

### The price-sheet read

`GET /_memql/reads/wholesale/priceSheet?requestToken=<token>`, the read a
price-sheet link's page makes. On success it answers the company name and the
current price sheet (`label`, `body`, `sha256`, `publishedAt`). A price-sheet
link has no expiry and is never used up.

| `error` | What happened | What the page should say |
|---|---|---|
| `link_closed` | The link is real, and the buyer cannot read the sheet now: the application is not approved (submitted, rejected or revoked), or it is approved and the buyer's access is not in place: not granted, or no longer | The price sheet is not available on this link now, which can be for good or for the moment |
| `link_invalid` | As on the terms read | This link is not valid |
| `documents_unavailable` | The store has no current price sheet | The price sheet is not available at the moment |

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
