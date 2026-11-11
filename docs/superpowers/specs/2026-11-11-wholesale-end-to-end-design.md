---
title: Wholesale from application to trade prices, end to end
audience: internal
status: draft
area: build
sinceVersion: 0.31.33
owner: znas
---

# Wholesale, end to end: from an application to a buyer paying trade prices

A client storefront hosted on this engine runs a wholesale program. A shopper
applies on the storefront, accepts the terms on a private link, a person on the
client's staff approves, the engine tags the buyer in Shopify, and the buyer then
sees trade prices in the cart. The client-side design was approved on 2026-11-04
and seven engine issues were filed from it (memql#6444 to memql#6450, called E1
to E7 below). Nothing was built since. An audit on 2026-11-11 found more gaps,
and the owner then added four asks.

This record is the engine's own statement of everything the engine must provide
so that path works end to end. It is written so an implementation plan can be
cut from it unit by unit (section 9).

Terms used throughout:

- **The applicant** is the person who filled the form. They are data on a row,
  never a principal: a shopper has no user, no session and no actor
  (`packs/wholesalepack/dsl/concepts.memql:75`).
- **The buyer** is an applicant whose application was approved.
- **The client's staff** are people in the client's organization who hold the
  wholesale part (section 5) and decide applications in the OS.
- **The route** is the entitlement adapter a store's settings name:
  `customerTag` (one Shopify customer tag) or `shopifyB2B`.
- **A private link** is a URL on the store's own site that carries a random
  token; the engine stores only the token's SHA-256.
- **The lane** is the transactional email lane of section 6: one message to one
  person a pack row names, with no unsubscribe.

**Approvals.** The W decisions in section 4 are the owner's, given with the brief
for this record on 2026-11-11. The decisions marked "(this record)" were made
while writing it and wait for the owner's review.

## 1. What the owner asked for

| # | Requirement, in the owner's terms |
| --- | --- |
| R1 | One application goes from the storefront form to an approved, tagged buyer who pays trade prices, with no step a person on the engine team has to do. |
| R2 | The applicant is told by email at each point: received (with the terms link), approved (with the price sheet), rejected, removed. Each email is a switch the client holds. |
| R3 | The client's staff are told by email when an application is ready to review and of each decision, at one address per store. |
| R4 | The client's staff read and decide applications themselves, scoped to their own storefront, in the OS. |
| R5 | Approved buyers are listed as a group the staff can add to and remove from. |
| R6 | Nothing in it decides anything: no model, no agent. A person decides every application. |
| R7 | Generic: any storefront with a wholesale program, a development store now and a merchant's live store later. |

## 2. What is wrong at v0.31.33

Checked in the code at `3d4fe13e9` (v0.31.33) in this worktree, building on the
audit of the same commit. E ids map to memql#6444 to memql#6450, N ids to the
audit's, W ids to section 4. G ids are new here.

| # | Finding | Where |
| --- | --- | --- |
| E1 | No lane reaches a person who is not a subscriber without an unsubscribe. The outbox needs every target on an environment allowlist; `row_address` enrols the address into a marketing audience first; order mail needs an order, consults the whole suppression list (including a newsletter unsubscribe) and attaches the one-click unsubscribe. Every render refuses a message without an unsubscribe link. | `component/outbound/config.go:61,107-109,160`; `component/emailrules/generate.go:82-101`, `fire.go:437-447`; `component/campaigns/send_order.go:34-42,53-55`; `send_to_recipient.go:154,288,303`; `render.go:555,570-571` |
| E2 | No document, link, acceptance, form or rule. The pack declares one shopper form and no read. Settings have four fields. | `packs/wholesalepack/shopper.go:46-81`; `dsl/concepts.memql:115-121` |
| E3 | No suspend or reinstate; revoked is final; the entitlement has one direction flag. | `packs/wholesalepack/application.go:89-101`; `entitlement.go:225` |
| E4 | The tag route refuses when no Shopify customer has the email, and the refusal is a plain error, so the row is `pending` (the guide says `failed`). `write_customers` is in neither scope list. | `integrations/shopify/wholesale.go:307-322,359-369`; `packs/wholesalepack/entitlement.go:340-344`; `integrations/shopify/connect.go:109-119`; `connections.go:27-30`; `docs/public/build/consuming-the-wholesale-pack.md:209` |
| E5 | No wholesale screen, no part, no account field on any wholesale row, no capability on any wholesale builtin. The organization gate judges builtins and mutations, never queries. | `clients/os/src/apps/deployables/` (no `wholesale/`); `component/auth/rbac_model.go:295`; `component/memql/organization_action_capabilities.go:13-19,57-63,79-82`; `packs/wholesalepack/dsl/builtins.memql` |
| E6 | Nothing a client DSL can call to send the applicant's emails, and nothing to make a second `granted` version not send a second approval email. | `packs/wholesalepack/dsl/builtins.memql` |
| E7 | No live test of the tag route; the pack's e2e runs with no connector and no client automation. | `integrations/shopify/wholesale_test.go:12`; `wholesale.go:49`; `packs/wholesalepack/live_e2e_test.go:354` |
| N1 | Provisioning from a client automation reads the application under the automation's actor, a synthetic reader that owns nothing, so it answers "not readable by this caller" and writes no row. The pack has no `IsInternal` check and no system actor. Read from the code; not yet run. | `component/automations/executor.go:68-107,584`; `packs/wholesalepack/entitlement.go:230`; `application.go:382-391`; compare `packs/reviewspack/notify.go:35,42` |
| N2 | A decision row is owned by whoever decided, so the site owner and the staff do not read the same log. | `packs/wholesalepack/dsl/concepts.memql:86` |
| N3 | An approval on a store with no route returns an error before any write, so no entitlement row says why nothing happened. | `packs/wholesalepack/entitlement.go:257-260,358-379` |
| N4 | Nothing takes the tag off on a revoke decision: the client wires approve only, and the engine offers no client-callable way that works from an automation (N1). | the client package's grant automation (its own repository); `packs/wholesalepack/entitlement.go:218-221` |
| N5 | The four Shopify wholesale builtins carry no capability and no origin check, by stated design, and resolve the store from the connector's registry. Their handler checks neither. Whether some other gate stops a signed-in caller from tagging any email on any store is not established. | `dsl/shopify/overlay/builtins.memql:112-118,141-155`; `integrations/shopify/wholesale_capabilities.go:171-181,200-210`; `capabilities.go:482-505` |
| G1 | Every wholesale mutation is client-callable. An authenticated caller who can write an application row can call `appendApplicationDecision` directly and record any transition, past the legality fold and past any future acceptance rule. | `packs/wholesalepack/dsl/mutations.memql:27-32,71-86` (no `@serverOnly` in the pack) |
| G2 | The pack is not on the internal-origin allowlist, so its writes cannot be made `@serverOnly` without an entry there. | `call_origin_conformance_test.go:97,284` (reviews is; wholesale is not) |
| G3 | A system actor is synthetic: a row written under it is owned by nobody. So the N1 fix cannot simply write as the system actor; it must read as the system and write as the merchant, as the reviews daily ask does. | `component/auth/system_actor.go:87-113`; `packs/reviewspack/due.go:143` |
| G4 | `ShopperSite` has no account, and the handler stamps only `storeId`, `siteId` and `submissionId` into the pack's and the extension's arguments. `accountId` is not a reserved shopper name. | `component/server/shopper_handler.go:122-127,300-302,373-375`; `app/transport_shopper_surface.go:84-90`; `component/memql/shopper_surface.go:157-173` |
| G5 | The shopper handler stamps nothing about the request itself, so an acceptance cannot record the address and browser it came from. The edge already computes a client address for its rate limit and sets only the three stamp headers. | `component/edge/shopper.go:261-283,408-413` |
| G6 | A deferred single send is replayed by recipient id alone. An order send's recipient exists only for its send, so a replay of a rate-limited order send may not find it. Read from the code; not yet run. A row send would have the same gap. | `component/campaigns/deferred_send.go:35-41`; `send_to_recipient.go:181-188` |
| G7 | The suppression read returns the cluster-wide row first and stops, so a lane that must skip an unsubscribe but honour a bounce cannot use it as it is. | `component/campaigns/suppression.go:19-34` |
| G8 | A template has no purpose, so nothing tells a template written for one person's request from a newsletter. | `dsl/campaigns/concepts.memql:138-150` |
| G9 | The organization gate keeps an untied row (no `accountId`) on the global capability contract, so until a backfill the client's staff are refused on every existing wholesale row while operators are admitted. | `component/memql/organization_action_capabilities.go:147-153` |

**On N5.** Verified by reading: neither the DSL nor the handler checks origin or
capability. Not verified: whether an engine-wide rule refuses a client call to an
`@sdk` builtin. U4's live test settles it (section 11). The fix is X22, built in U3.

**On N1 and G6.** Both come from reading the code. Each gets a failing test before
its fix, and is dropped from this record if the test passes.

## 3. Shape of the solution

Five moves, in the order they are built.

**Make the grant work from the client's automation, and say why when it does
not.** The pack's automation-called builtins require internal origin, read as a
named system actor and write as the store's merchant. Every write goes through a
`@serverOnly` mutation, so the rules the builtins hold cannot be walked around.
An approval with no route, a missing customer or a missing scope leaves an
entitlement row that names the cause and the one act that fixes it.

**Tie every wholesale row to the client's organization.** Rows carry the
`accountId` of the site they arrived through, as reviews' do, and an organization
part `app:deployables/wholesale` lets the client's staff read and decide their own
store's applications and nothing else.

**Add the terms step.** Three documents per store, versioned. A private link
mailed to the applicant opens a page that shows the current terms and MAP policy;
accepting them writes an acceptance record. Approval is refused without one, and
applications cannot open until the three documents exist.

**Add one narrow email lane, and builtins for the client's DSL to call it.** The
lane sends one message to the address on one registered pack row, from a
published transactional template, with no unsubscribe, skipping an address that
bounced or complained. The pack's builtins mint the links and choose the template
per kind; the client's DSL decides when each runs. Staff notices ride the existing
outbox to one address per store.

**Give the client's staff a Wholesale section** on the storefront deployable's
page, with Applications, Buyers, Documents and Settings.

Suspend and reinstate come last, as their own unit.

## 4. Decisions

### 4.1 The owner's decisions (they bind)

The client design's D1 to D13 stand. On top of them:

| # | Decision | Why |
| --- | --- | --- |
| W1 | Pricing stays the tag route: the engine adds and removes one customer tag. Discounts and the segment are made by hand in the store's admin; the engine never creates or checks them. | A price change made on a merchant's behalf from an approval is a larger act than the approval (`integrations/shopify/wholesale.go:301-306`). |
| W2 | No model or agent anywhere. Every automatic step is an ordinary programmed step. A person decides every application. | R6. |
| W3 | Emails to the applicant: (1) received, with the private terms link; (3) approved, with a private link to the price sheet, sent only once the tag is on; (4) rejected; (5) removed. Each exists only if its template is published ("no template, no send"). | R2. Emails 4 and 5 were off in the client design; the owner turned them on. A switch the client holds keeps wording the client's. |
| W4 | Emails to the client's staff: one when an application is ready to review (at acceptance, not at submission), and a copy of each decision. They go to one address per store, held as a setting, so a test store can use a test address. The engine provides the setting and a way to send to it; the client's DSL decides the wording. | R3. Before acceptance there is nothing to act on. |
| W5 | The terms stay as designed: a private link that lasts 30 days, one checkbox per document plus a typed name, an acceptance record, approval refused when no acceptance exists, applications not openable until the three documents exist. | The client's contract requires formal acceptance before review. |
| W6 | The client's staff work in a Wholesale section on the storefront deployable's page: Applications, Buyers, Documents, Settings. Approved buyers are listed on the Buyers tab of that section, not in the Users app (confirmed by the owner, 2026-11-11). The list must be showable elsewhere later without changing the server side. | R4, R5. A group member must be a principal who signs in, and a buyer is a store customer, so a buyer cannot be a group member (`dsl/identity/concepts.memql:777-790`). |
| W7 | Adding a buyer by hand does not skip the terms: it creates the application on the buyer's behalf, and email 1 follows as for anyone. Removing a buyer records a revoke and takes the tag off. | Every buyer then has an acceptance record; otherwise W5 has a hole. |
| W8 | Access follows the reviews pattern: an `accountId` on every wholesale row, stamped from the site; a part `app:deployables/wholesale` granted to the client's group; capability checks on the read and decide builtins, scoped to that client's site. Decision rows are tied to the account and owned by the merchant. Rows written before this version get their account by a backfill. | R4. `packs/reviewspack/dsl/concepts.memql:31,68`; `organization_action_capabilities.go:70-82`. |
| W9 | N1 to N4 are fixed first: provisioning from an automation works (system actor, internal origin); decisions visible to the site owner; an approval with no route leaves a visible record; the engine offers what the client's DSL needs to take the tag off on a revoke. | Nothing reaches a store without N1. |
| W10 | The engine creates the Shopify customer at approval when none exists (D4) and shows each buyer's access with a retry. | Otherwise every approval needs a second manual step. |
| W11 | The price sheet is a document reached by a private link, not an attachment. | Only inline images are attachable (`integrations/email/acs.go:141-145`), and a link follows document versions. |
| W12 | Suspend and reinstate (E3) are the last unit and may ship in a second release; removal covers the end state first. | E3 is independent of the path. |
| W13 | The managed connection's scope set, including `read_customers` and `write_customers`, arrives with the store-mirror branch, which lands first. This record assumes it. | One approval by the store instead of two. |

### 4.2 Decisions made in this record

Each is marked so the owner can spot it.

| # | Decision (this record) | Why |
| --- | --- | --- |
| X1 | Every wholesale mutation becomes `@serverOnly`, and the pack joins the internal-origin allowlist with a stated reason, exactly as reviews did. The existing mutation names and arguments stay; arguments are only added. | G1. W5's "approval is refused without an acceptance" is not true while a caller can append a decision around the builtin that refuses it. |
| X2 | `wholesaleProvisionEntitlement` and `wholesaleRevokeEntitlement` become automation-only (internal origin). A new gated builtin, `wholesaleRetryAccess`, is the OS's retry. | One builtin cannot be both a trusted job's and a client's: the capability gate would judge the automation, and an ungated client path would skip the organization. Reviews splits them the same way (`reviewsNotifyModerator` versus `reviewsDecide`). |
| X3 | Automation-called builtins read as a named system actor and write as the application's merchant (its `ownerUserId`), the same identity the shopper path borrows. | G3. A row written as the system actor is owned by nobody. |
| X4 | The cluster's operators read every store's applications and decide none, as in reviews (`packs/reviewspack/decide.go:139-146`). A decision needs a person who is not a cluster operator. | W8 says "the reviews pattern", and the client design says the client's staff decide. |
| X5 | Change `applicationDecision`'s row tier to `owner="ownerUserId", clusterOwner, account="accountId"` and add those two fields, rather than adding a second decision concept as reviews did. Existing rows are rewritten by the backfill with every field they held plus the two they lacked. | There are only a handful of decision rows in the field. A second concept would double every fold and every read for ever. The append-only property lives in the write path (no caller chooses an id), and the backfill changes no decision field. |
| X6 | A document version holds its text (Markdown, at most 200 KB) in the row, fingerprinted over those bytes. No file upload. | The page and the record must show exactly what was agreed to; a fingerprint over the shown text proves it. An attachment would go through the file processor, which summarises with a model (`dsl/common/concepts.memql:13-14`), against W2. |
| X7 | One private-link row per application and purpose, at a derived id. Minting writes a new version with a new hash, which ends the old link (its hash no longer matches). A link to the terms ends at expiry, at acceptance, or when the application is no longer submitted. A link to the price sheet has no expiry; it works while the application is approved and its access granted, and always shows the current price sheet. | One row is one answer to "what link does this person hold". A price sheet a buyer rereads in month three must still open; removal ends it. |
| X8 | `termsRequired` absent means false. Turning it on while applications are open, or opening while it is on, is refused when any of the three documents has no version. | A second client without terms keeps today's behaviour; the client turns it on once its documents are published. |
| X9 | The lane skips an address whose suppression reason is `hard_bounce`, `complaint` or `manual`, cluster-wide or in the organization, and ignores `unsubscribed`. | E1: a newsletter unsubscribe must not block a person's own application mail; a dead address, a complaint and an operator's block must. |
| X10 | A template gains `purpose` (`marketing` or `transactional`; absent is marketing). The lane sends only a published transactional template. A campaign, a journey step and an event email rule (`component/emailrules`, when armed and when it fires) each refuse a transactional template. | G8. The lane has no unsubscribe, so its templates must not be reachable from the bulk path, and the bulk path's must not be reachable from it. |
| X11 | The template for each email kind is a settings field the client's staff set, from the organization's published transactional templates. The sender identity is a settings field the cluster's operators set; empty means the organization's configured sender. | Same split as reviews (`reviewspack/dsl/builtins.memql:56-72`): wording is the store's, the mailbox the mail leaves from is an operator's. |
| X12 | The staff notice is staged by the pack through `stageServerOutboundRequest` to the settings' `staffEmail`, which must be on the outbound allowlist. The client's DSL passes subject and body with a closed set of placeholders the pack fills. | W4 says the client writes the wording; the outbox is the operational lane and already has the allowlist control. |
| X13 | The acceptance sentence is a settings field (`acceptanceText`) with an engine default, shown by the terms page and copied into the record. | The record must hold the exact sentence shown, and that sentence is the client's. |
| X14 | The client's own fields on an application are rendered generically: the extension's row at the application's id, every declared field except stamped ones, labelled by the field name in plain words. The row on the list shows the extension concept's `@displayCard` primary and secondary. | E5: the engine never learns a client's field names. Field descriptions are written for developers and run to several sentences, so they are not labels. |
| X15 | Reject is offered while an application waits for terms, as well as once it is ready to review. | Staff must be able to reject an obvious spam application without waiting for it to accept terms. |
| X16 | Add buyer works whether applications are open or closed, refuses an email that already has a submitted or approved application on the store, and needs the three documents when terms are required. | Closed means the public form. A second application for one person would split their history. |
| X17 | A revoke when no customer exists is a success (there is no tag to remove). A revoke of an entitlement that never reached a store (its adapter is `none`) writes `revoked` without calling an adapter. | A revoke must never create a customer, and must not fail for ever on something that was never granted. |
| X18 | No automatic retry of a `pending` grant. Staff retry from the OS. A staff notice kind for a grant that did not land exists for the client to wire. | W2 keeps automation small; the notice makes the failure seen. |
| X19 | Accounts are backfilled by an operator-run builtin, run once at rollout and safe to run again, not at boot. | A boot pass would run on every node at once; an operator run is one pass with a report. |
| X20 | The address an acceptance records is the leftmost `X-Forwarded-For` entry the edge saw, stamped by the edge into a header the bff trusts, and recorded as "the address the request came from, as the edge saw it". | G5. It is spoofable by the sender, which the record says; it is evidence, not identity. |
| X21 | The staff-facing word for `revoked` is Removed, and for the email kind `revoked` it is "Removed". Stored values keep the transition's word. | W3 and W7 say removed; the enum says revoke; neither is renamed. |
| X22 | The four Shopify wholesale builtins (`shopifyTagWholesaleCustomer`, `shopifyUntagWholesaleCustomer`, `shopifyProvisionWholesale`, `shopifyRevokeWholesale`) refuse a call without internal origin, and the pack's `engineCaller` stamps internal origin inline on its one Execute, after the pack's own checks. Built in U3, proved by U4's step 7. | N5. A builtin that changes what a person pays must not be reachable by any signed-in caller; "the pack already checks" only holds while the pack is the only caller. Decided at review of this record, 2026-11-11. |
| X23 | The staff are told when access did not land: the client wires a staff notice on an entitlement version that is `pending` or `failed`, using the notice kind X18 names. It is one email on a failure, never a retry. | X18 leaves the retry to a person, so the person has to hear about it without opening every application. Decided at review of this record, 2026-11-11. |

## 5. Data

Fields are added. None is removed or renamed (removing a field bricks stored
rows, memql#5199). Every new or changed mutation below is `@serverOnly` (X1).
Every derived row id is a bare slug with no colon. `slug(x)` is the pack's
existing `slugOf`.

### 5.1 Concepts in `packs/wholesalepack/dsl/concepts.memql`

**`application`** (`@version("1.1.0")`), row tier becomes
`@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`.

| Field | Type | Meaning |
| --- | --- | --- |
| `accountId` | string | The site's organization, stamped at submission (U5). Empty on a row from before this version until the backfill. |
| `source` | enum("storefront", "staff") | How it arrived. Absent reads as storefront. |
| `addedBy` | string | The person who added it, for `source: staff`. |

**`applicationDecision`** (`@version("1.1.0")`), row tier becomes
`@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")` (X5).

| Field | Type | Meaning |
| --- | --- | --- |
| `ownerUserId` | string | The application's merchant, copied off it. |
| `accountId` | string | The application's organization, copied off it. |
| `transition` | enum gains `"suspend"`, `"reinstate"` (U13 only) | |

**`entitlement`** (`@version("1.1.0")`), row tier gains `account="accountId"`.

| Field | Type | Meaning |
| --- | --- | --- |
| `accountId` | string | Copied off the application. |
| `errorCode` | string | One of `no_settings`, `no_route`, `scope_missing`, `plan_ceiling`, `customer_refused`, `shopify_error`. Empty on success. The OS turns it into one sentence and one act (section 8). |
| `shopifyCustomerId` | string | The customer the tag is on. |
| `customerCreated` | bool | True on the version that created it. |
| `state` | enum gains `"suspended"` (U13 only) | |

`adapter` stays required; a row written when no route is set carries `none` (N3).

**`wholesaleSettings`** (`@version("1.1.0")`), row tier gains `account="accountId"`.

| Field | Type | Meaning |
| --- | --- | --- |
| `accountId` | string | The site's organization. |
| `siteId` | string | The site the settings were made for, which the organization gate judges. |
| `termsRequired` | bool | X8. Absent is false. |
| `linkLifetimeDays` | int | Terms link lifetime, 1 to 365. Absent is 30. |
| `staffEmail` | string | Where staff notices go (W4). |
| `acceptanceText` | string | X13. Absent is "I have read and accept the Wholesale Terms and Conditions and the MAP Policy shown above." |
| `senderIdentityId` | string | X11. Operators only. |
| `termsTemplateId` | string | Email 1. Empty: email 1 is off. |
| `approvedTemplateId` | string | Email 3. |
| `rejectedTemplateId` | string | Email 4. |
| `revokedTemplateId` | string | Email 5. |
| `suspendedTemplateId`, `reinstatedTemplateId` | string | U13 only. |

**`documentVersion`** (new). One published version of one document. Append-only:
created by the engine with a generated id, never updated. The newest per
`(storeId, kind)` is current.
`@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`.

| Field | Type |
| --- | --- |
| `ownerUserId` | string!, the store's merchant |
| `storeId` | string! |
| `accountId` | string |
| `kind` | enum("terms", "map", "priceSheet")! |
| `label` | string!, the version label the staff type, at most 120 characters |
| `body` | string!, Markdown, at most 200 KB |
| `sha256` | string!, hex digest of `body`'s UTF-8 bytes |
| `publishedBy` | string!, the person, read from the caller |
| `publishedAt` | datetime!, the engine clock |

**`privateLink`** (new). One per application and purpose, at
`wholesale-link-<purpose>-<slug(applicationId)>` (X7). Tier as above.

| Field | Type |
| --- | --- |
| `ownerUserId`, `storeId`, `accountId`, `siteId` | string (`ownerUserId`, `storeId` required) |
| `applicationId` | string! |
| `purpose` | enum("terms", "priceSheet")! |
| `tokenHash` | string!, SHA-256 hex of the token; the token is never stored |
| `issuedAt` | datetime! |
| `expiresAt` | datetime, empty for `priceSheet` |
| `usedAt` | datetime, set when a terms link is accepted |

**`acceptance`** (new). One per application, at `wholesale-acceptance-<slug(applicationId)>`,
A second acceptance of one application is refused: the construct reads the acceptance first and answers `link_used` when one exists, and the write carries a row-version fence that requires the row to be absent (`ContextWithRowVersionFence` with a zero version), so of two posts at once one writes and the other answers `link_used`. Tier as above.

| Field | Type |
| --- | --- |
| `ownerUserId`, `storeId`, `accountId` | string |
| `applicationId` | string! |
| `applicantEmail` | string!, copied from the application |
| `signedName` | string!, as typed, at most 120 characters |
| `acceptedAt` | datetime!, the engine clock |
| `termsVersionId`, `termsSha256` | string! |
| `mapVersionId`, `mapSha256` | string! |
| `statement` | string!, the exact sentence shown |
| `clientAddress` | string, X20 |
| `userAgent` | string, at most 512 characters |
| `linkIssuedAt` | datetime, the link's own issue time |

**`emailSend`** (new). One per email to the applicant, at
`wholesale-email-<kind>-<slug(applicationId)>-<attempt>`. Tier as above.

| Field | Type |
| --- | --- |
| `ownerUserId`, `storeId`, `accountId` | string |
| `applicationId` | string! |
| `kind` | enum("terms", "approved", "rejected", "revoked", "suspended", "reinstated")! |
| `attempt` | int!, 1 for the automatic send, then one more for each resend |
| `requestId` | string!, the lane's request id (section 6) |
| `templateId` | string |
| `state` | enum("sent", "skipped", "failed", "pending")! |
| `reason` | string, the lane's own words for skipped or failed |
| `sentAt` | datetime |
| `requestedBy` | string, the person who pressed Resend; empty for the automatic send |

Delivery after acceptance (delivered, bounced) is not copied here; the read builtin
asks the lane's ledger (section 6).

### 5.2 Mutations in `packs/wholesalepack/dsl/mutations.memql`

All `@serverOnly`. Existing ones keep their names and gain arguments.

| Mutation | Change |
| --- | --- |
| `createApplicationRow` | `@serverOnly`; gains `accountId`, `source`, `addedBy`, and `ownerUserId` as an argument (the Go supplies the site owner; X3). |
| `appendApplicationDecision` | `@serverOnly`; gains `ownerUserId`, `accountId`; `transition` enum gains suspend and reinstate in U13. `decidedBy` stays stamped from the actor. |
| `writeEntitlementRow` | `@serverOnly`; gains `ownerUserId` (argument, replacing the actor stamp), `accountId`, `errorCode`, `shopifyCustomerId`, `customerCreated`. |
| `setWholesaleSettingsRow` | `@serverOnly`, kept for the old builtin. |
| `putWholesaleSettingsRow` (new) | The whole merged settings row at `SettingsRowID(storeId)`, every field of 5.1, as `putReviewSettings` does. |
| `appendDocumentVersion` (new) | Insert with a generated id. |
| `putPrivateLinkRow` (new) | Insert at the derived id. |
| `putAcceptanceRow` (new) | Insert at the derived id; the caller fences it to an absent row (5.1). |
| `putEmailSendRow` (new) | Insert at the derived id. |
| `backfillWholesaleRow` (new) | One per concept (`backfillApplicationRow`, `backfillDecisionRow`, `backfillEntitlementRow`, `backfillSettingsRow`): writes the row at its own id with every field it held plus `accountId` (and `ownerUserId` for a decision). |

### 5.3 Queries in `packs/wholesalepack/dsl/queries.memql`

Added. Row tier decides what each answers, as today.

| Query | Args | Notes |
| --- | --- | --- |
| `documentVersionsForStore` | storeId, kind | Newest first. |
| `privateLinkByTokenHash` | storeId, tokenHash | Read on the shopper path under the borrowed merchant, as `reviewRequestByTokenHash` is. Projects no hash. |
| `privateLinkForApplication` | applicationId, purpose | |
| `acceptanceForApplication` | applicationId | |
| `emailSendsForApplication` | applicationId | Newest first. |
| `decisionsForStore` | storeId | Newest first, paginated; the decision log. |
| `applicationsWithoutAccount` | (none) | `@serverOnly`; the backfill. |

### 5.4 Builtins in `packs/wholesalepack/dsl/builtins.memql`

"Gated" means `@requiresCapability("execute", "app:deployables/wholesale")`,
judged by the organization gate against the target named. "Internal" means the
Go refuses any call without internal origin before reading anything. "Shopper"
means it is a declared shopper construct, run under the site owner's borrowed
authority with client origin.

| Builtin | Reach | Target | Arguments | Answers |
| --- | --- | --- | --- | --- |
| `wholesaleSubmitApplication` | shopper (unchanged) | | gains stamped `accountId` | receipt |
| `wholesaleRecordDecision` | gated | application, `applicationId` | unchanged; refuses a cluster operator (X4); `approve` refused without an acceptance when terms are required | receipt |
| `wholesaleApplicationState` | unchanged | | | gains `stage`: `waitingForTerms` or `readyToReview` while submitted |
| `wholesaleSetSettings` | cluster owners and internal only | | gains optional `siteId`; enforces X8 | receipt |
| `wholesaleProvisionEntitlement` | internal (X2) | | unchanged | entitlement receipt |
| `wholesaleRevokeEntitlement` | internal (X2) | | unchanged | entitlement receipt |
| `wholesaleRetryAccess` (new) | gated | application | applicationId | provisions when approved, revokes when revoked and the row is not revoked; otherwise refused by name |
| `wholesaleList` (new) | gated | site, `siteId` | siteId, storeId, stage (`waitingForTerms`, `readyToReview`, `approved`, `rejected`, `revoked`, `suspended`, `all`; default `readyToReview`), search, limit, after | rows with company, applicant, email, stage, the extension's two display facts, createdAt; counts per stage; next |
| `wholesaleGet` (new) | gated | application | applicationId | the application, the extension's fields (X14), the acceptance, the decision log, the entitlement, the email sends with delivery status, the link's expiry, and `acts`: the acts the engine would accept now from this caller |
| `wholesaleDecisionLog` (new) | gated | site | siteId, storeId, search, limit, after | each decision with who, when, note, and its application's company |
| `wholesaleBuyers` (new) | gated | site | siteId, storeId, search, limit, after | every application whose state is approved (or suspended), with its access state and the customer |
| `wholesaleStoreSettings` (new) | gated | site | siteId, storeId | effective settings with defaults; `exists`; `staffEmailAdmitted` (whether the outbox's allowlist admits it, U9); the documents' current versions; the organization's transactional templates to choose from; what the caller may change |
| `wholesaleUpdateSettings` (new) | gated | site | siteId, storeId, and any of: applicationsOpen, termsRequired, linkLifetimeDays, staffEmail, acceptanceText, entitlementAdapter, introText, the template ids; `senderIdentityId` from operators only | merges; refuses a store with no settings (operators set it up, as reviews); refuses X8 |
| `wholesaleDocuments` (new) | gated | site | siteId, storeId | per kind: current version and history (label, when, who, fingerprint) |
| `wholesalePublishDocument` (new) | gated | site | siteId, storeId, kind, label, body | the new version's id and fingerprint |
| `wholesaleAddBuyer` (new) | gated | site | siteId, storeId, companyName, applicantName, applicantEmail | the new application's id (X16) |
| `wholesaleRemoveBuyer` (new) | gated | application | applicationId, note | records a revoke decision; the tag comes off through the client's automation (N4), and `wholesaleRetryAccess` takes it off if that automation is not wired |
| `wholesaleResendEmail` (new) | gated | application | applicationId, kind (`terms` or `approved`) | the new `emailSend` row |
| `wholesaleExportStore` (new) | gated | site | siteId, storeId | `{fileName, json}`: applications, the extension's fields, acceptances, decisions, entitlements, email sends, document versions, settings; never a token hash |
| `wholesaleSendTermsEmail` (new) | internal | | applicationId | sends email 1 through the lane, whose resolver mints the terms link at hand-over; skipped with a reason when terms are not required, the template is unset, or the application is not submitted |
| `wholesaleSendBuyerEmail` (new) | internal | | applicationId, kind | sends email 3, 4 or 5; `approved` needs state approved and the entitlement granted (W3), and the resolver mints the price-sheet link at hand-over; `rejected` and `revoked` need that state |
| `wholesaleNotifyStaff` (new) | internal | | applicationId, event, subject, body, decisionId | stages one staff notice (section 7.3) |
| `wholesaleTermsForLink` (new) | shopper read | | requestToken | the company, the current terms and MAP policy (kind, label, body, fingerprint), the acceptance sentence, the expiry; or a named refusal |
| `wholesaleAcceptTerms` (new) | shopper form | | requestToken, acceptTerms, acceptMap, signedName, plus the evidence the handler puts on the request's context | writes the acceptance; or a named refusal |
| `wholesalePriceSheetForLink` (new) | shopper read | | requestToken | the company and the current price sheet; or a named refusal |
| `wholesaleBackfillAccounts` (new) | cluster owners and internal only | | storeId (optional) | counts: tied, already tied, left untied with why (X19) |

Refusal codes for the three link constructs: `link_invalid`, `link_expired`,
`link_used`, the same words reviews uses (`packs/reviewspack/request.go:41-47`).

### 5.5 Shopper surface (`packs/wholesalepack/shopper.go`)

| Route | Construct | Fields | Redirects |
| --- | --- | --- | --- |
| form `wholesale/application` | unchanged | unchanged | unchanged |
| form `wholesale/acceptance` (new) | `wholesaleAcceptTerms` | `requestToken` (required, 200), `acceptTerms` (required, 8), `acceptMap` (required, 8), `signedName` (required, 120); `Evidence: true` | OK `/wholesale/accepted`, error `/wholesale/link-problem`, with the three link refusals declared |
| read `wholesale/terms` (new) | `wholesaleTermsForLink` | `requestToken` (required) | `Private: true` |
| read `wholesale/priceSheet` (new) | `wholesalePriceSheetForLink` | `requestToken` (required) | `Private: true` |

The links in emails are `https://<site host>/wholesale/terms?t=<token>` and
`https://<site host>/wholesale/price-sheet?t=<token>`. The storefront owns those
pages; they call the reads.

### 5.6 Engine-wide changes

| Where | Change |
| --- | --- |
| `component/memql/shopper_surface.go` | `accountId`, `clientAddress` and `userAgent` join the reserved names. `ShopperForm` gains `Evidence bool`. |
| `component/memql/shopper_extension.go` | `ShopperExtension` gains `AcceptsAccount bool`, set by the loader when the extension's mutation declares an `accountId` argument. |
| `component/server/shopper_handler.go` | `ShopperSite` gains `AccountID`. The handler stamps `accountId` into the pack's arguments, and into the extension's when `AcceptsAccount`. For an `Evidence` form it puts `clientAddress` from the edge's header and `userAgent` from the request (cut to 512) on the context it executes the construct with, never in the arguments, so a direct call of the construct carries no evidence. |
| `app/transport_shopper_surface.go` | Reads `accountId` off the site row into `ShopperSite`. |
| `component/edge/shopper.go` | Sets `X-Memql-Shopper-Client` to `shopperClientAddress(r)` on the proxied request, after deleting any inbound copy, beside the three stamp headers. |
| `component/auth/rbac_model.go`, `dsl/rbac/seeds.memql` | `"app:deployables/wholesale": {RoleOwner, RoleDeveloper}` and its two seeds. |
| `component/memql/organization_action_capabilities.go` | The part joins `organizationDeployableCapability`. `wholesaleRecordDecision`, `wholesaleGet`, `wholesaleRetryAccess`, `wholesaleRemoveBuyer`, `wholesaleResendEmail` target `v1:wholesale:application` by `applicationId`; the site-wide builtins of 5.4 target `v1:platform:site` by `siteId`. |
| `clients/os/src/apps/deployables/parts.ts` | `wholesale` joins `DEPLOYABLE_PARTS`. |
| `call_origin_conformance_test.go` | An allowlist entry for `packs/wholesalepack` with its reason (U1). |
| `dsl/campaigns`, `component/campaigns` | Section 6. |
| `dsl/shopify/overlay/builtins.memql`, `integrations/shopify` | `shopifyTagWholesaleCustomer` gains optional `buyerName`; find-or-create (U3). |

### 5.7 Events a client DSL triggers on

No new event kind. Every step the client wires is a graph event on a pack row:

| Moment | Trigger | Filter |
| --- | --- | --- |
| A new application (form or staff) | `node.created` on `v1:wholesale:application` | |
| Terms accepted | `node.created` on `v1:wholesale:acceptance` | |
| A decision | `node.created` on `v1:wholesale:applicationDecision` | `row.transition == "approve"` and so on |
| Access granted | `node.created` and `node.updated` on `v1:wholesale:entitlement` | `row.state == "granted"` |
| Access did not land | the same two | `row.state == "pending" \|\| row.state == "failed"` |

The automations themselves are the client's (section 13).

## 6. The transactional email lane (E1)

One builtin in the campaigns component, a sibling of `campaignSendOrderMessage`:

```
@executor("integration.campaigns.sendRowMessage")
builtin campaignSendRowMessage {
  rowConcept        string!  // a concept registered for the lane
  rowId             string!  // the row whose address is mailed
  accountId         string!  // the row's organization; template and sender must be its
  templateId        string!  // a published transactional template
  requestId         string!  // one message; a retry returns its receipt
  kind              string!  // which message about the row; handed to the row's resolver
  senderIdentityId  string   // omit for the organization's sender
}
```

**One person, never an address.** No argument names a recipient or carries the
message's fields. A pack registers, from Go, a resolver for its row concept:
`campaigns.RegisterRowAddress(RowAddress{Concept: "v1:wholesale:application",
Resolve: ...})`. A DSL file cannot register one. The lane calls the resolver only
when it is about to hand a message to the provider, with the concept, the row id,
the account, the `kind` and the request id; the resolver answers the recipient's
address, the row's account and the `{{fields.<key>}}` values for that kind, or
withdraws the message with a reason (ledgered `skipped`). The lane refuses when
the resolved account is not `accountId`. The wholesale pack registers it when its
provider is built, so a cluster where the pack is disabled registers nothing.

**For trusted jobs only.** A call without internal origin is refused before
anything is read, as order mail does (`send_order.go:53-55`). The wholesale pack
calls it with internal origin stamped inline on that one call, after its own gate
(the capability for a resend, internal origin for an automation's send).

**The send.** It runs through `sendToRecipientOnce` with a row recipient that
exists only for this send (`Recipient{ID: "row-" + slug(concept) + "-" + slug(rowId)}`),
the way the order route does, so the receipt, the lock, pacing, the sender
resolution and the delivery ledger are the existing ones. What differs:

- **No unsubscribe.** A new `RenderOptions.Transactional` makes `renderMessage`
  skip the `errNoUnsubscribeLink` refusal, the List-Unsubscribe header pair and the
  appended footer (`render.go:563-580`, the footer at `:575-578`). `RequireUnsubscribe` is not consulted for
  this path. Nothing else is appended. Whether a given message counts as
  transactional where the client trades is the client's judgment of its own
  template; the guide says so beside the purpose field.
- **Bounce and complaint stop it; an unsubscribe does not.** A new
  `Store.BlockingSuppression(accountId, digest)` reads the cluster row and the
  organization row and answers the first whose reason is `hard_bounce`,
  `complaint` or `manual` (X9, G7). A match is ledgered `skipped` with that reason,
  as today. Bounces and complaints already become suppressions from the provider's
  signed reports (`feedback_ingest.go:165-197`, `azure_feedback.go:17-60`), so an
  address that bounced once is skipped from then on with no new code.
- **Transactional templates only.** `tmpl.Status == "ready"` and
  `tmpl.Purpose == "transactional"`, else the send is held with the template named
  (`templateHeld`), which the pack records as `skipped` with that sentence.
- **Replay knows the row, and asks again.** `v1:campaigns:delivery` gains
  `rowConcept`, `rowId` and `rowKind` (string), and never the fields. The
  deferred-send replay rebuilds a row send from them instead of reading a recipient
  row (G6), and the replayed send calls the resolver again, so a rate-limited
  message goes out later with fresh fields (a link minted then) and no token is
  ever stored. The receipt check comes before the resolver: a second call with the
  same request id sends nothing and mints nothing. U7's first test settles whether order sends share
  the gap; if they do, the same two fields are set for them (`rowConcept:
  "v1:shopify:order"`) and that is recorded in the plan as a fix.

**Templates and who edits them.** A template is an ordinary `v1:campaigns:template`
of the store's organization, with `purpose: "transactional"`. It is written and
published in the Email app by anyone that app lets edit the organization's
templates; the client's staff can when the organization grants them the Email app.
`campaignSaveTemplate` gains an optional `purpose`; the Email app's template page
shows it as a choice ("Marketing" / "About one person's own request: no unsubscribe").
The campaign, journey and event-rule pickers leave transactional templates out,
and starting a campaign, saving a journey step, or arming or firing an event email
rule with one is refused (X10). Each template uses
`{{fields.<key>}}` tags from a closed set the pack fills:
`companyName`, `applicantName`, `signInEmail`, `termsUrl`, `termsExpiresOn`,
`priceSheetUrl`, `storeName`.

**Delivery status.** The ledger row is the existing `v1:campaigns:delivery`
(`status`, `skipReason`, `providerStatus`, `sentAt`), keyed by `requestId`. A
second builtin, `campaignRowMessageStatus(requestIds)` (internal), answers
`{requestId, status, skipReason, providerStatus, sentAt}` for each, read under the
system actor. `wholesaleGet` calls it after the gate and projects only those
fields.

**Why it cannot be used for marketing.** No argument can name an address. Only Go
can register a row, and the wholesale pack registers one concept. Only trusted
callers reach it, and the pack's own builtins bound its request ids to
`(kind, application, attempt)`, with each kind tied to the application's state
(5.4). A message goes to one person about their own application; there is no list,
no audience and no campaign behind it. The residual risk is a template whose
words are marketing; the engine cannot read intent, so the purpose field, the
closed tag set and the guide carry that rule, and the client's staff own it.

**Request ids.** `wholesale-<kind>-<slug(applicationId)>-<attempt>`, which
satisfies `singleSendRequestID` (`single_send.go:19`). A second automatic call for
one kind finds attempt 1's receipt and sends nothing (E6's idempotency). A resend
is the next attempt.

## 7. The terms, the links and the staff notices

### 7.1 Private links (E2)

- **Minted** by the pack's row resolver (section 6) at the moment the lane hands
  the email to the provider: the terms link for a `terms` send
  (`wholesaleSendTermsEmail`, `wholesaleResendEmail(kind: terms)`), the price-sheet
  link for an `approved` send (`wholesaleSendBuyerEmail(kind: approved)` and its
  resend). The send builtins themselves never mint, and a send whose request id
  already has a receipt reaches no resolver, so a repeated event mints nothing.
  A deferred email that is replayed mints a fresh link then. The token is 32 random bytes, base64url (`packs/reviewspack/request.go:52-59`
  is the code to copy). It is in the email only.
- **Stored** as a new version of the one `privateLink` row for that application
  and purpose: the hash, `issuedAt`, and for terms `expiresAt = issuedAt +
  linkLifetimeDays` (absent is 30). The old token stops matching at that moment, so
  a resend ends the old link (X7).
- **Verified** by the shopper constructs, under the site owner's borrowed authority
  and scoped to the store the request arrived through: the hash must match a link
  of this store (`link_invalid` otherwise, which also covers a replaced link); for
  terms, `usedAt` must be empty and the application still submitted (`link_used`),
  and the clock before `expiresAt` (`link_expired`); for the price sheet, the
  application approved and its entitlement granted (`link_used` otherwise).
- **The public page may read** the company name, the current terms and MAP policy
  (label, body, fingerprint), the acceptance sentence and the link's expiry, or the
  current price sheet. Never the applicant's email, never an id, never a hash.
- **The public page may write** one acceptance, once. The form's construct
  re-verifies the link, checks both boxes arrived as `on`, trims the typed name and
  refuses an empty one (`link_invalid` is not used for that: an empty name answers
  `invalid` from the handler's own required-field check), reads the current versions
  of both documents again, and writes the acceptance and the link's `usedAt` in that
  order. It reads the acceptance first and answers `link_used` when one exists, and
  the acceptance write is fenced to an absent row (`ContextWithRowVersionFence`
  with a zero version), so of two posts at once one writes and the other answers
  `link_used`. A crash between the two writes leaves an acceptance and a link that
  still opens; the next post finds the acceptance and answers `link_used`.

Both reads are `Private` (`component/memql/shopper_surface.go:284-289`): never
cached, and the edge's existing rules keep the token out of a Referer.

### 7.2 The two rules (W5)

- `wholesaleRecordDecision` with `approve` reads the settings; when
  `termsRequired` is true and `acceptanceForApplication` answers nothing, it refuses:
  "This application has not accepted the terms yet."
- `wholesaleUpdateSettings` and `wholesaleSetSettings` refuse a write that leaves
  `applicationsOpen` and `termsRequired` both true while any kind has no
  `documentVersion`: "Publish the terms, the MAP policy and the price sheet first."
- `wholesaleSubmitApplication` and `wholesaleAddBuyer` re-check the second rule at
  write time, so a document cannot be the reason a link opens onto nothing.

### 7.3 Staff notices (W4)

`wholesaleNotifyStaff(applicationId, event, subject, body, decisionId)`, internal.

- `event` is one of `readyToReview`, `approved`, `rejected`, `revoked`,
  `accessNotGranted`, and in U13 `suspended`, `reinstated`.
- The target is the settings' `staffEmail`. Empty: skipped, "the store names no
  address", as reviews (`notify.go:52-55`).
- `subject` and `body` are the client's text. The pack replaces a closed set of
  placeholders: `{{companyName}}`, `{{applicantName}}`, `{{applicantEmail}}`,
  `{{state}}`, `{{decidedBy}}` (the person's name), `{{note}}`, `{{adminUrl}}` (the
  OS address, built as reviews builds it; there is no deep link to one application). Nothing from the client's extension row is
  reachable, so a tax number cannot travel by accident.
- Staged with `stageServerOutboundRequest`, request id
  `wholesale-staff-<event>-<slug(applicationId)>[-<slug(decisionId)>]`, so a
  re-delivered event stages nothing new.
- The outbox delivers only to an allowlisted address (`component/outbound/config.go:107-109,160`).
  A new exported `outbound.EmailAdmitted(target string) bool`, reading the same
  configuration, lets the settings read say whether the address is admitted.

## 8. Operator surface

All of it is in the Deployables app, on a storefront deployable that fronts a
store, for a person holding `execute app:deployables/wholesale` for its
organization. It mounts as Reviews does (`DeployablePage.tsx:449-459,552`): an
icon button "Wholesale" in the deployable's Head, absent without the part or for a
deployable that fronts no store, and a section that takes the pane, opening with
its Head and then `LocalTabs`: **Applications, Buyers, Documents, Settings**
(DESIGN.md "A tab is a different kind of thing"). One application is a page
opened from the tab it was found in, under its trail.

Rules that hold everywhere (DESIGN.md): kit components only; no dividers between
rows (rule 1); facts in plain text, no pill, chip or badge in a row ("Record
lists"); `Notice` for every refusal and failure, never a toast (rule 1); filters
behind `Refine` (rule 2); acts in the one `ActionBar`, at most three, primary last,
an act that is not legal is absent (rule 12); a question in place uses
`InPlaceAsk` (rule 12); loading is the shape of the content (`RecordListSkeleton`,
`ContentSkeleton`), a real empty state only after a successful empty read, a failed
read is a `Notice` with Try again ("Loading is the shape of the content"); a
control whose part is not held is absent (rule 12; `parts.ts`).

**For a cluster operator** every tab reads, and a `Caption` says once, on the list:
"You can read this store's applications. Its own people decide them." Decide acts
are absent for them (X4); the engine's `acts` list in `wholesaleGet` already leaves
them out.

### Applications

- **List.** `wholesaleList`. `Refine` asks the stage: Waiting for terms, Ready to
  review (default, oldest first), Approved, Rejected, Removed, All (newest first).
  Search: company, applicant, email.
- **Row.** `RecordRow`: the company as the name, the applicant and email as the
  secondary line, the extension's two display facts as plain text, and "waited 3
  days" or the date as the trailing caption. The stage word only under All.
- **Empty.** "Nothing ready to review" (or the stage's words), and under Ready to
  review, when terms are required, one sentence: "Applications appear here once the
  applicant has accepted the terms."
- **Closed store.** When applications are closed, the Head's meta says "Closed to
  new applications".

**One application** (`wholesaleGet`), `Details` with `DetailsSection` and `Facts`:

1. *Application*: company, applicant, email, submitted, how it arrived (the
   storefront, or added by a named person).
2. *Their details*: the extension's fields (X14), or "No further details" for a
   staff-added application, or "These details are not readable to you" when the
   client's extension row is not yet tied to the organization (section 13).
3. *Terms*: accepted by the typed name, when, the two versions with their labels
   and the first eight characters of each fingerprint, the address and browser; or
   "Not accepted yet. The link expires on 12 December." or "The link has expired."
4. *Access*: Trade prices on (on the customer), Not granted yet, or Failed, each with the
   sentence for its `errorCode`: `no_route` "No access route is chosen in
   Settings."; `no_settings` "This store has no wholesale settings."; `scope_missing`
   "The store has not allowed MemQL to change customers. Its owner approves the new
   permissions from the store's admin."; `plan_ceiling` "The store's plan allows no
   more catalogs."; `customer_refused` and `shopify_error` "Shopify refused:" and its
   words.
5. *Emails*: one line per kind sent: Sent on, Delivered, Bounced ("the address does
   not accept mail"), Not sent with the reason (no template published, the address
   bounced before, the sender is not set up).
6. *Decisions*: who, what, when, the note.

The bar, by stage (rule 12):

| Stage | State words | Acts (primary last) | Calls |
| --- | --- | --- | --- |
| Waiting for terms | Waiting for terms | Reject; Resend terms email | `wholesaleRecordDecision(reject)`; `wholesaleResendEmail(terms)` |
| Ready to review | Ready to review | Reject; Approve | `wholesaleRecordDecision` |
| Approved | Approved, and the access state | Remove; Resend approval email (when sent once); Retry access (when not granted) | `wholesaleRemoveBuyer`; `wholesaleResendEmail(approved)`; `wholesaleRetryAccess` |
| Removed | Removed | Retry access (only while the tag is still on) | `wholesaleRetryAccess` |
| Rejected | Rejected | none | |

Reject and Remove ask in place with an optional internal note (`TextArea`) and
"Reject it / Keep it". After each write the page re-reads; a refusal (for example
a second person decided first) stands as a `Notice` above the bar with the
engine's sentence.

### Buyers

- `wholesaleBuyers`: every approved application. A row names the company, the buyer
  email, and the access in words ("Trade prices on since 3 December", "Not on yet:
  no access route is chosen"). Opening a row opens the application's page, where
  Remove and Retry access live.
- The Head's `AddButton` "Add buyer" opens a page: Company, Name, Email, and a
  sentence: "They will be emailed the terms. They can buy at trade prices once they
  accept and someone approves." The bar: Cancel; Add and send the terms
  (`wholesaleAddBuyer`). A refusal (X16) stands as a `Notice`.
- Empty: "No buyers yet."
- The list is one builtin with no OS-only logic, so another app could show it later
  (W6).

### Documents

- `wholesaleDocuments`. Three sections, one per kind (Wholesale terms, MAP policy,
  Price sheet), each with its `Subhead` and an Add control "Publish a new version".
  Facts: the current label, published on and by, the fingerprint's first eight
  characters. Earlier versions are a `RecordList` beneath, newest first; opening one
  shows its text read-only.
- A kind with no version reads "Not published. Applications cannot open while terms
  are required."
- **Publish page**: Label (`Input`), Text (`TextArea`, Markdown), "Load from a file"
  for a `.md` or `.txt` read in the browser, a preview of the rendered text, and the
  bar: Cancel; Publish (`wholesalePublishDocument`). Publishing never changes an
  acceptance already recorded, which keeps its own fingerprints.

### Settings

`wholesaleStoreSettings`, written by `wholesaleUpdateSettings`, as reviews' settings
page is. Groups (rule 8):

- *Applications*: Open or closed (a choice); Terms required (a checkbox, since this
  is a form, rule 10); Link lifetime in days.
- *Staff*: Staff address, and beneath it "The outbox delivers to this address." or
  "The outbox does not deliver to this address yet. The cluster's operators add it."
- *Access*: the route (customer tag, Shopify B2B, none).
- *Emails*: for each of the four emails, the template (a `Select` of the
  organization's published transactional templates, with "Off" first), and the
  sender as a read-only fact ("Sent as …", set by the cluster's operators).
- *Acceptance*: the sentence (`TextArea`).
- *Records*: "Download every record" (`wholesaleExportStore`).

A store with no settings reads "This store's wholesale program is not set up yet.
The cluster's operators set it up." with no controls.

**No permission.** A person without the part sees no Wholesale button. A call that
reaches the engine anyway is refused with `capability_not_held`, whose copy
`packages/refusals.ts` already carries.

## 9. Runtime changes, by unit

Each unit has one job, its own tests, and is one or two plan tasks. **U1 to U4 are
the first milestone**: one application from the form to an approved, tagged buyer on
a development store, approved by a direct builtin call as the merchant, with the
client's grant and revoke automations wired.

| Unit | What changes | Files | Interfaces | Depends on | Proved by |
| --- | --- | --- | --- | --- | --- |
| U1 Automation-safe provisioning (N1, X1 to X3, G1 to G3) | Provision and revoke require internal origin, read as system actor `wholesale-provision`, write as the application's merchant. Every pack mutation `@serverOnly`; the writer stamps internal origin inline on its one Execute; allowlist entry. Entitlement and decision writes carry `ownerUserId` from the application. | `packs/wholesalepack/{entitlement,application,reader}.go`, `dsl/{mutations,builtins}.memql`, `call_origin_conformance_test.go` | `engineWriter.Write(ctx, mutation, args)` as reviews'; `const provisionActor = "wholesale-provision"` | none | A unit test with a client-origin context is refused before any read; a database test runs the client's grant automation on an approve decision and finds a `granted` row owned by the merchant; a direct client call to `appendApplicationDecision` is refused. |
| U2 Visible failures (N3) | `applyEntitlement` writes `failed` with `errorCode` for no settings, no route, plan ceiling; `pending` with `shopify_error` for a transient error; `scope_missing` when Shopify answers with an access-denied for customers. Revoke rules of X17. | `packs/wholesalepack/entitlement.go`, `adapter_customer_tag.go`, `dsl/concepts.memql` | `errorCodeOf(err) string` | U1 | Unit tests per code; an approval on a store with no route leaves a `failed` row naming `no_route`. |
| U3 Customer find-or-create, scopes and the origin refusal (E4, W10, W13, X22) | The four Shopify wholesale builtins refuse a call without internal origin and the pack's `engineCaller` stamps it (X22). `TagWholesaleCustomer` finds by email, else creates (email, split name, no marketing consent), then tags; a duplicate-email refusal from `customerCreate` re-reads and continues; returns whether it created. Untag of a missing customer succeeds. `write_customers` joins `ConnectScopes`. The guide's `failed` claim is corrected. | `integrations/shopify/{wholesale,wholesale_calls,wholesale_capabilities,connect}.go`, `dsl/shopify/overlay/builtins.memql`, `packs/wholesalepack/adapter_customer_tag.go`, `docs/public/build/consuming-the-wholesale-pack.md` | `TagWholesaleCustomer(ctx, store, email, name, tag) (gid string, created bool, err error)` | mirror branch landed | Unit tests with a fake Admin transport: missing customer created once; second run creates nothing and adds no second tag; a client-origin call to each of the four builtins is refused before any Admin call. |
| U4 Live test (E7) | `integrations/shopify/wholesale_live_test.go`, skipped without development-store credentials: create a missing customer, tag, untag, re-tag, re-provision with no change, report a grant without `write_customers`, and call `shopifyTagWholesaleCustomer` as a signed-in non-owner to settle N5. A pack e2e that runs a client automation on an approve decision against the same store. | `integrations/shopify/wholesale_live_test.go`, `packs/wholesalepack/live_e2e_test.go` | the mirror plan's live harness where it exists | U1, U3 | Passes against a development store; the default suite skips cleanly. |
| U5 Account stamping and backfill (W8 data half) | `accountId` on the four existing concepts; reserved names; `ShopperSite.AccountID`; handler stamps it into the pack's and the extension's arguments; `AcceptsAccount`; `wholesaleBackfillAccounts`. | `component/memql/{shopper_surface,shopper_extension}.go`, `component/server/shopper_handler.go`, `app/transport_shopper_surface.go`, `packs/wholesalepack/*` | section 5.6 | U1 | A database test: a form post on a site of organization A writes both rows with A; an extension without an `accountId` argument still writes; the backfill ties a pre-version row, its decisions and its entitlement, and reports a row with no resolvable site. |
| U6 The part and the gated builtins (W8 access half, N2, X4, X5) | The part, its seeds, the organization gate cases; `applicationDecision`'s new tier; the read builtins, `wholesaleRetryAccess`, `wholesaleUpdateSettings`, `wholesaleStoreSettings`, `wholesaleBuyers`, `wholesaleDecisionLog`, `wholesaleExportStore`; operator refusal on decisions. | `component/auth/rbac_model.go`, `dsl/rbac/seeds.memql`, `component/memql/organization_action_capabilities.go`, `packs/wholesalepack/{reads,settings,export}.go` (new), `dsl/*.memql`, `clients/os/src/apps/deployables/parts.ts` | section 5.4 | U5 | A database test: a person in organization A holding the part lists and approves A's application and is refused on B's; a cluster operator reads and cannot decide; the site owner reads the moderator's decision. The part is pinned as reviews' is, by `component/memql/wholesale_capability_test.go` and the pack's own gate test; `TestTheDeployablesPartsAreDeclaredOnTheirConstructs` loads only the platform tree and cannot see the pack. |
| U7 The lane (E1, X9, X10) | `campaignSendRowMessage`, `RegisterRowAddress`, `RenderOptions.Transactional`, `BlockingSuppression`, template `purpose`, the pickers' filter, delivery `rowConcept` and `rowId`, the replay, `campaignRowMessageStatus`. | `component/campaigns/{send_row,render,suppression,deferred_send,template}.go`, `dsl/campaigns/{concepts,builtins}.memql`, `clients/os/src/apps/email` (purpose choice, picker filter) | section 6 | none (parallel from the start) | Unit: no List-Unsubscribe header and no footer; a client-origin call refused; an unregistered concept refused; a marketing template held. Database: an address with an organization `unsubscribed` row is sent; one with `hard_bounce` is skipped; a rate-limited send is replayed with its row recipient; a second call with the same request id sends nothing. |
| U8 Terms (E2, W5, X6 to X8, X13, X20) | The three new concepts; `wholesalePublishDocument`, `wholesaleDocuments`; the link mint and verify; the form and two reads; the evidence stamp in the edge and the handler; the two rules. | `packs/wholesalepack/{documents,links,acceptance}.go` (new), `shopper.go`, `dsl/*.memql`, `component/edge/shopper.go`, `component/server/shopper_handler.go` | sections 5.1, 5.5, 7.1, 7.2 | U5 | Unit: the token is never stored; expired, used, replaced and other-store tokens refused by name; a resend ends the old token; the acceptance holds every field; approve refused without one; opening refused without three documents; versions append-only, newest current. |
| U9 Emails and staff notices (E6, W3, W4, W11, X11, X12) | `emailSend`; `wholesaleSendTermsEmail`, `wholesaleSendBuyerEmail`, `wholesaleResendEmail`, `wholesaleNotifyStaff`; the price-sheet link; `outbound.EmailAdmitted`; delivery status in `wholesaleGet`. | `packs/wholesalepack/{email,notify}.go` (new), `dsl/*.memql`, `component/outbound/config.go` | sections 6, 7.3 | U6, U7, U8 | Unit and database: no template, no send; approved refused while the entitlement is pending; a second granted version sends nothing; a resend of terms ends the old link and is attempt 2; a staff notice with no address is skipped; placeholders filled, extension fields unreachable. |
| U10 Add and remove a buyer (W7, X16) | `wholesaleAddBuyer`, `wholesaleRemoveBuyer`. | `packs/wholesalepack/buyers.go` (new), `dsl/builtins.memql` | section 5.4 | U6, U8 | Database: an added buyer is a submitted application owned by the merchant, tied to the organization, `source: staff`; a duplicate refused; remove records a revoke by the caller. |
| U11 OS, slice one | The Wholesale section with Applications (list, one application, approve, reject, remove, retry) and Settings (open, route, staff address). | `clients/os/src/apps/deployables/wholesale/*` (new), `page/DeployablePage.tsx`, `page/DeployableWorkspace.tsx` | `useWholesaleList`, `useWholesaleApplication`, `useWholesaleWrites`, as `reviews/useReviews.ts` | U6 | Component tests for each stage's bar; a person without the part sees no button; both themes, both widths. |
| U12 OS, slice two | Buyers, Documents, the rest of Settings (terms, link lifetime, emails, acceptance sentence, records), the Terms and Emails sections of one application, Resend, the generic extension renderer. | `clients/os/src/apps/deployables/wholesale/*` | as U11 | U8, U9, U10, U11 | As U11, plus: no act is drawn in a state that refuses it. |
| U13 Suspend and reinstate (E3, W12) | The two transitions; `suspended` entitlement; `customerTag` untags and re-tags; `shopifyB2B` sets its catalog to draft and back to active (a new `catalogUpdate` to ACTIVE); emails and notices for both; OS acts Suspend and Reinstate. | `packs/wholesalepack/{application,entitlement,adapter_*}.go`, `integrations/shopify/{wholesale,wholesale_capabilities}.go`, `dsl/shopify/overlay/builtins.memql`, the OS section | `applyEntitlement(ctx, args, mode)` with mode grant, revoke, suspend, reinstate | U4, U6, U12 | Unit: reinstate from revoked or rejected refused by name; live: suspend untags, reinstate re-tags, revoke from suspended works. |

**In parallel.** Three tracks can start at once: the pack (U1, U2, U5, U6, U8, U9,
U10, in that order, because they share `concepts.memql`, `mutations.memql` and
`builtins.memql`), the Shopify integration (U3, then U4 once U1 is in), and the lane
(U7). The OS starts once U6 has landed (U11), and U12 follows U9 and U10. U13 is last.

## 10. What can go wrong, step by step

| Step | When it fails | What a person sees | How it is retried |
| --- | --- | --- | --- |
| Apply | Applications closed, a field invalid, the site not bound | The storefront's problem page with `?reason=` | The applicant tries again |
| Apply | The client's extension row fails | The application stands with its details blank (existing D5); the OS says "No further details" | Staff contact the applicant |
| Email 1 | No template, template not published, no sender, a bounced address | The application's Emails line: Not sent, with the reason | Fix the cause; Resend terms email |
| Email 1 | The provider is rate limited | Not sent yet; the lane's replay sends it | Nothing |
| Terms page | Expired, used, replaced or broken link | The storefront's link-problem page | Staff press Resend terms email |
| Accept | Both boxes not ticked, no name | The problem page (`invalid`) | The applicant goes back |
| Staff notice | No staff address, or not on the allowlist | The Settings line says so; the outbox row stays undelivered | The operators add the address; the next event notifies |
| Approve | No acceptance, a second person decided first, an operator | A `Notice` with the engine's sentence | Nothing, or read the application again |
| Grant | No settings, no route, scope missing, plan ceiling | Access: Failed with the sentence and its one act | Fix the cause; Retry access |
| Grant | Shopify error or timeout | Access: Not granted yet with Shopify's words; email 3 waits | Retry access |
| Grant | The client's grant automation is not wired | Access: none recorded, approved | Retry access |
| Email 3 | Access not granted | No email; nothing to do until access lands | It sends on the granted version |
| Email 3 | Template off, address bounced | Emails: Not sent, with the reason | Fix; Resend approval email |
| Price sheet page | The buyer was removed, or access not granted | The link-problem page | Staff resend after granting |
| Remove | The revoke automation is not wired, or Shopify fails | Removed, and Access: the tag is still on | Retry access |
| Backfill | A row's site cannot be found, or two sites serve its store | The builtin's report names each row | The operator fixes the site and runs it again |

## 11. Testing

**Unit and database tests** are listed per unit in section 9. Every database test
uses the engine test database the repository's tests already require.

**N5 and N1 first.** U1 opens with a failing database test of the client's grant
automation (N1). U4 includes the direct call that proves N5 closed (X22). G6 is settled by U7's
first test. A suspicion whose test passes is struck from section 2 in the plan.

**Live, against a development store** (E7), skipped without credentials:

1. With a grant lacking `write_customers`, a provision answers `scope_missing`.
2. With the grant complete and no customer for a fresh address, a provision creates
   one customer and tags it; the entitlement records `customerCreated`.
3. A second provision creates nothing and adds no second tag.
4. Revoke removes the tag; a second revoke succeeds with nothing to do.
5. Provision again re-tags.
6. The automation path: an approve decision written through the pack, with the
   client's grant automation loaded, leaves a `granted` row and the tag on.
7. A signed-in non-owner calls `shopifyTagWholesaleCustomer` directly and is
   refused before any Admin call (X22), which closes N5.
8. U13: suspend untags, reinstate re-tags, revoke from suspended untags.

**The walk, on the owner's instance**, against a development store, after the
rollout (section 15). A person follows it in the UI. "Staff" is a non-operator
person in the client's organization holding the wholesale part.

| # | Step | Passes when |
| --- | --- | --- |
| 1 | As the operator, set up the store's wholesale settings with its site; add the staff address to the outbox allowlist. | Settings opens for staff; "The outbox delivers to this address." |
| 2 | As staff, publish the terms, MAP policy and price sheet in Documents. | Each shows its label and fingerprint. |
| 3 | In Email, publish four transactional templates; choose them in Settings; set the route to customer tag; turn on Terms required; open applications. | Saved; opening was refused before step 2 and is accepted now. |
| 4 | In the store's admin, the segment and the automatic discounts exist (made by hand). | As the client's setup list. |
| 5 | On the storefront, apply with a fresh test address. | The thank-you page. Applications > Waiting for terms lists it. |
| 6 | Read the test inbox. | Email 1 arrives with no unsubscribe link and a terms link. |
| 7 | In the OS, Resend terms email; open the first link. | The first link reaches the link-problem page; the second opens the terms. |
| 8 | Tick both boxes, type the name, accept. | The accepted page. The staff inbox receives "ready to review". The application shows the acceptance with both versions. |
| 9 | Open the terms link again. | The link-problem page. |
| 10 | As the operator, open the application. | It reads; there is no Approve. |
| 11 | As staff, Approve. | The staff inbox receives the approval copy. Access reads Trade prices on, and the customer exists in the store's admin with the tag. |
| 12 | Read the test inbox. | Email 3 arrives only after step 11's access, with the price-sheet link and the sign-in address. |
| 13 | Open the price-sheet link. | The current price sheet. |
| 14 | Publish a new price sheet; open the same link. | The new version. |
| 15 | Sign in on the storefront as the buyer; fill the cart across a tier boundary. | Trade prices in the cart on the right side of each boundary; retail below it. |
| 16 | Check out with the development store's test payment, or an admin order marked paid. | The order is placed at trade prices. |
| 17 | Buyers lists the buyer; Remove, with a note. | Removed; access reads the tag is off; the staff inbox gets the removal copy; email 5 arrives. |
| 18 | Reload the storefront signed in. | Retail prices. The price-sheet link reaches the link-problem page. |
| 19 | Apply again with a second address; Reject it while it waits for terms. | Email 4 arrives; the staff copy arrives; the terms link no longer opens. |
| 20 | Buyers > Add buyer with a third address. | Email 1 arrives; the application is waiting for terms, added by staff. |
| 21 | Set the route to none; accept and approve the third. | Access: Failed, "No access route is chosen in Settings."; no email 3. |
| 22 | Set the route back; Retry access. | Trade prices on; email 3 arrives. |
| 23 | Unpublish (archive) the rejected-email template; reject a fourth application. | No email 4; the application's Emails line says the template is not published. |
| 24 | Download every record from Settings. | One JSON file with the applications, details, acceptances, decisions, access, emails, documents and settings, and no token hash. |
| 25 | U13 only: suspend and reinstate the buyer from step 22. | Retail while suspended, trade after reinstating; both emails and copies. |

## 12. Several engine nodes

| Step | Behaviour with several nodes |
| --- | --- |
| A client automation on a pack row's event | Runs once per event as every automation does today. Every step it calls is idempotent: sends by request id (the campaigns receipt lock, `single_send.go:132-160`), staff notices by outbox request id, grants because a second `tagsAdd` changes nothing. |
| Two grants of one application at once | Both find no customer and one `customerCreate` is refused for a duplicate email; that path re-reads and tags the one customer (U3). Both write a version of the one entitlement row; the newest wins, and both say granted. |
| Two decisions at once | The legality fold runs in each; both may read submitted and both append. Reviews has the same window. The OS re-reads and the second person's page shows the log. Closing the window needs a lock per application; carried as a risk (section 14). |
| The lane's deferred replay | The existing campaigns drain, under the receipt lock, which survives node changes (`deferred_send.go:12-14,46-52`). |
| The outbox | The existing cross-replica claim (`component/outbound/config.go`, `ClaimTTL`). |
| Link expiry | Computed at read time against the engine clock; no job. |
| The backfill | One operator call on one node. Run twice at once, both write identical versions; it only touches rows still untied. |

No new scheduled job is added.

## 13. What the client's package does (outside this repository)

Named here so the engine side is complete; the client builds it.

- Automations, each a `@trigger` with the filter of 5.7: on an application, call
  `wholesaleSendTermsEmail`; on an acceptance, `wholesaleNotifyStaff(event:
  readyToReview)`; on an approve decision, `wholesaleProvisionEntitlement` (exists)
  and the staff copy; on a reject, `wholesaleSendBuyerEmail(kind: rejected)` and the
  copy; on a revoke, `wholesaleRevokeEntitlement` (N4),
  `wholesaleSendBuyerEmail(kind: revoked)` and the copy; on a granted entitlement,
  `wholesaleSendBuyerEmail(kind: approved)`; on an entitlement version that is
  `pending` or `failed`, `wholesaleNotifyStaff(event: accessNotGranted)` (X23).
- Remove the desk email at submission once the acceptance notice is wired.
- Add `accountId` (stamped) and `account="accountId"` to its extension concept and
  mutation, so its staff read its own fields.
- The storefront pages `/wholesale/terms`, `/wholesale/accepted`,
  `/wholesale/link-problem`, `/wholesale/price-sheet`.
- Move its pinned engine commit to the release that carries this.

## 14. Risks carried

| Risk | Test or step that settles it |
| --- | --- |
| N1 is not the runtime failure the code suggests | U1's first database test |
| N5: any signed-in caller can tag any email | Closed by X22 in U3; U4 step 7 proves the refusal |
| G6: deferred order and row sends are not replayable | U7's first database test |
| Changing `applicationDecision`'s row tier on stored rows (X5) | A database test with a decision written by the old mutation, read before and after the backfill |
| Making the mutations `@serverOnly` breaks a caller that wrote them directly (an import, `twoclients_test.go`) | The full pack suite in U1; any import path moves to a builtin |
| A client's package automations do not run with internal origin | U4 step 6 (the automation path); reviews' moderator notice relies on the same (`automations/executor.go:868-873`) |
| Two decisions at once both pass the fold | Accepted for this version, as in reviews; a per-application lock is a later change |
| A store connected through its own app lacks `write_customers` | Its own app's scopes must include `write_customers` and its owner must approve again; store health's `scopesMissing` names it, and the live test step 1 proves the answer |
| Shopify's protected customer data | Reading and creating customers by email needs the app's protected-data declaration; a development store needs no review, a live store does. The store-mirror record's D3 carries the declaration; this record adds no field to it |

## 15. Documentation

- `docs/public/build/consuming-the-wholesale-pack.md`: the new states and error
  codes (and the `failed` correction), the terms step, the link rules, every
  builtin with its reach, the events of 5.7 and a worked set of client automations,
  the staff placeholders, the account field on an extension.
- `docs/public/operate/campaign-sending.md`: the lane, the template purpose, what
  stops a send and what does not, and why it is not for marketing.
- `docs/public/operate/deployables.md`: the Wholesale section and the part.
- `docs/public/operate/shopify-connector.md`: `write_customers` on the custom-app
  path and what it is used for.
- `docs/public/operate/outbound-delivery.md`: the staff address on the allowlist.
- `clients/os/DESIGN.md`: nothing, unless a kit piece is added. Rule 1 already has
  no row dividers, which E5 asked for.

## 16. Rollout

1. **The store-mirror branch lands** (W13). The development store approves the new
   scopes.
2. **Release one: U1 to U6 and U11.** The operator runs `wholesaleBackfillAccounts`
   once and reads its report; grants the wholesale part to the client's
   organization group on the Users > Groups page; creates the store's settings with
   its site (from the Wholesale section's Settings tab, or `wholesaleSetSettings`).
   The client wires its grant and revoke automations, adds `accountId` (stamped) and
   `account="accountId"` to its extension concept and mutation so its staff read
   its own fields (section 13), and moves its pin.
   From here the client's staff approve in the OS and buyers are tagged.
3. **Release two: U7 to U10 and U12.** Staff publish the documents; templates are
   published as transactional and chosen; the staff address is allowlisted; the
   client wires the email automations and removes the submission desk email; then
   Terms required is turned on. The walk (section 11) runs on the development store.
4. **Release three, which may be later: U13** (W12). Until it ships, removal is how
   access ends.
5. **A merchant's live store**, once Shopify has reviewed the app's protected
   customer data declaration (store-mirror record, section 12), with one test
   application approved by the client's own staff.

## 17. Proposed additions (not decided)

- **A lock per application for decisions**, closing the window in section 12 (two
  decisions at once both pass the fold). Left for a later change, as in reviews.

Two additions proposed in the draft of this record were decided at its review and
are now X22 (the Shopify wholesale builtins refuse a call without internal origin)
and X23 (a staff notice when access did not land).

## 18. Not in this version

- Shopify B2B beyond suspend and reinstate, including its catalog-per-buyer rework.
- Creating, checking or changing discounts or the segment (W1, D13).
- Reminder emails, and any automatic retry of a grant.
- File uploads for documents, and attachments of any kind (X6, W11).
- Buyers in the Users app (W6).
- Enforcing the terms' order minimums at checkout.
- An applicant reading their own application, or changing it.
- More than one staff address per store.
- Any model or agent (W2).
