# The Accounts App -- Design

- **Date:** 2026-09-01
- **Status:** approved (in-session Q&A with the owner; every fork below
  records the choice that was made and why)
- **Scope:** a new `dsl/accounts/` engine domain (`v1:accounts:account` +
  queries/mutations/shapes/seeds), four optional tie fields on existing
  concepts (`v1:platform:site`, the Library index, `v1:identity:invitation`,
  `v1:common:knowledgeDomain`), one optional field on `SendGuestInviteMsg`
  (wire-compatible), broadcast routing rules, and the Accounts app in
  `clients/os/` plus tie surfaces in Deployables, Files, Training and Users.
- **The wave this belongs to:** Epic B of three. Epic A is the
  packages-and-deployables design (2026-09-01, same directory); Epic C
  (custom domains, DNS verification + automatic certificates) follows and
  consumes exactly two things from here: `site.accountId` and
  `account.domain`.
- **Follow-ups noted, not built here:** a contact concept, client
  sign-in/guest access via accounts, folder-level file labels, agent routing
  off account-tagged knowledge domains.

## Why

Owner's brief, condensed: accounts are **clients** -- the companies this
instance's owner does work for. The instance hosts their storefronts and
sites (Epic A), stores files about them, trains knowledge for them, invites
their people as guests -- and today nothing ties any of that to who it is
for. The Accounts app is the client registry: your own company is set up on
first open (part prepopulated, minimal required), every other account is
created as needed, and ties stay lightweight -- "it doesn't have to be super
complex, but the UI needs to account for it." Personal surfaces (a user's
machines, their settings) deliberately get no account dimension.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Model | An account is a **record** -- who work is for. Never a login boundary, never a visibility scope; row authorization is untouched everywhere and every tie is an optional plain reference. Client *access*, when it is eventually wanted, rides the existing guest-invitation machinery pointed at an account -- accounts never become a wall |
| D2 | Concept | New engine domain `dsl/accounts/`; `v1:accounts:account` with the site-style composite tier and owner stamp (`@rowAuthz(owner="ownerUserId", clusterOwner)`; a cluster owner's creates land cluster-owned, users own theirs, "list every account" stays expressible) |
| D3 | Self | The owner's own company is the **literal-id singleton** `v1:accounts:account:self` -- seeded at boot **create-if-absent, never refreshed**. The cluster singletons refresh every boot because the system is their writer; this row's writer is a human, and a boot must never clobber their edits. Prepopulated at creation from `MEMQL_DOMAIN` (domain) and the owner's identity (contact); `name` gets a starter the first-run card asks to complete |
| D4 | Contacts | Fields on the row (`primaryContactName`, `primaryContactEmail`) in v1. A contact concept is future work nothing here needs yet |
| D5 | Ties | Four, all optional references with no read effect: (1) `accountId` on `v1:platform:site` (+ `references` relationship, `as="forAccount"`); (2) `accountIds` **list** on the Library index row ("one or two accounts" is the owner's own framing); (3) `accountId` on `v1:identity:invitation`, threaded `SendGuestInviteMsg -> createGuestInvitation` -- one new optional proto field, wire-compatible, called out for the frontend in the commit body; (4) `accountId` (single) on `v1:common:knowledgeDomain` -- a **tag** Training renders and filters by; agent routing and domain attachment are deliberately unaffected |
| D6 | The app | Ordinary OS app `accounts` (always-docked is the Bin's distinction, #4784): sections **Accounts** + **Settings**. Detail = profile facts + edit, then four live rollups (deployables, labeled files, tagged domains, guest invites), each an engine query filtered by account id through the normal authorized read |
| D7 | First run | Opening the app while the self account is unconfigured renders the setup card in place -- prepopulated fields, `name` the only must. "Unconfigured" is the absence of `configuredAt`, stamped by `updateAccount`. No modal ambush anywhere else in the OS |
| D8 | Lifecycle | `active` / `archived` with a visible Archived filter and an in-surface confirm. No `disabled` rung: accounts are not servable things, so the deployables three-step ladder does not apply |
| D9 | Epic C seam | `account.domain` exists so C's custom-domain flow has a suggestion source; C consumes it plus `site.accountId` and nothing else from here |
| D10 | Delivery | Four tasks, **two PRs**: PR 1 = engine (concept/seeds + the four ties), PR 2 = OS (the Accounts app + tie surfaces in the other apps). Stated on the epic and every task |

## A. What exists today (the ground this builds on)

- The guest write path is engine DSL split across two domains, all five
  mutations `@serverOnly` (memql#4258); `SendGuestInviteMsg` drives
  `createGuestInvitation`, so the invitation tie threads one optional gRPC
  field through an existing seam.
- The Library index is being reshaped by the files epic (#4721/#4781:
  folders, provenance). The `accountIds` field lands on the index row and is
  coordinated with that work -- field addition only, no schema fight.
- `v1:common:knowledgeDomain` is engine-core; Training lists domains today.
- `v1:platform:site` just gained `packageId`/`packageDeployableName` in the
  Epic A design; `accountId` is a third optional field of the same kind.
- The cluster singletons (`v1:cluster:database:primary` etc., memql#4766)
  prove the literal-id pattern; D3 deliberately inverts their refresh rule.

## B. The concept

| Field | Notes |
|---|---|
| `ownerUserId` | stamped from the actor; empty = cluster-owned (self is) |
| `name` | required; starter value at seed time |
| `domain` | the client's own domain; C's suggestion source (D9) |
| `primaryContactName`, `primaryContactEmail` | D4 |
| `notes` | free text |
| `status` | `active` \| `archived` (D8) |
| `configuredAt` | stamped by `updateAccount`; absence drives the first-run card (D7) |

Mutations `createAccount` / `updateAccount` / `archiveAccount`; queries
`accountsAll` (+ archived filter), `accountById`, and the per-tie rollups
(`sitesForAccount`, `libraryItemsForAccount`, `domainsForAccount`,
`invitationsForAccount`). Broadcast routing rules for
`graph.node.*.v1:accounts:account` so the app's lists are live.

## C. The tie surfaces

- **Deployables**: account chip + picker on the site detail (OS); shown
  beside kind/status. Epic A's detail work and this land independently --
  the field is optional either way.
- **Files**: inspector gains the account label picker (multiple); browse
  gains an account filter. Field addition coordinated with #4721's tree.
- **Users**: guest invites display their account where set; the send flow
  gains an optional account picker.
- **Training**: domain rows render their account tag; a filter by account.
  Nothing about routing, attachment, or agent behavior changes (D5).

## D. Testing

- Seed idempotency: boot twice, edit, boot again -- edits survive
  (create-if-absent proven, not assumed).
- Conformance: the new concept classifies; tie fields are declared on their
  concepts (a concept field without a shape is invisible to clients -- the
  shapes are part of the task, not an afterthought).
- The generated-artifact fan-out (SDK gen, arch model, env registry, embed
  counts, memqllint) is expected work.
- Rollup queries db-gated; OS vitest under the live-list retain()/arrival
  rules; the proto field addition gets a wire test on the guest path.

## E. Out of scope, and neighbors

Out of scope: client sign-in or any account-scoped authorization; a contact
concept; folder-level file labels; agent routing off account domains;
billing/invoicing; the DNS flow itself (Epic C).

Neighbors: Epic A (packages-and-deployables, same directory, 2026-09-01);
the files epic #4721 (Library index coordination); #4784 (Bin -- archived
accounts are another candidate for its cross-app open question); Epic C
(custom domains) consumes `site.accountId` + `account.domain`.
