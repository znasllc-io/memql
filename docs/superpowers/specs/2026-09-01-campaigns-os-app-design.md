# The Campaigns OS App (SP3) -- Design

- **Date:** 2026-09-01
- **Status:** approved (program decisions P2 / P3 / P7 in
  [2026-09-01-email-campaigns-program-design.md](2026-09-01-email-campaigns-program-design.md)
  are the authority this spec elaborates; every fork below records the
  choice that was made and why)
- **Scope:** a new `clients/os/src/apps/campaigns/` app, its tie surfaces
  in the Accounts app (a `campaignsForAccount` rollup band in
  `AccountDetail.tsx`), and the broadcast routing rules plus routing
  exclusions in `component/node/routing.go` and `routing_reach.go` that
  make the app's lists live. **No DSL** -- the schema, queries, shapes
  and builtins landed ahead of this. **No engine Go beyond the routing
  table**, which is a wire-contract change and is called out for the
  frontend in the commit body.
- **The wave this belongs to:** Phase 3 of the email-campaigns program.
  It consumes SP1's stats, import, test send and sender identities, and
  SP4's status plumbing for the "needs configuration" cue. Two tasks,
  two PRs -- **T8** (#4827, broadcast rules + app core) and **T9**
  (#4828, detail surfaces) -- stated on the epic and on both task
  issues. The Rules section arrives with SP2 (#4830).
- **Follow-ups noted, not built here:** a WYSIWYG template editor (the
  editor is a source editor with a preview); audience segmentation;
  per-recipient engagement history; a campaigns widget on the desktop;
  scheduling a send from a calendar surface.

## Why

The whole campaigns feature is reachable today only from a deprecated
surface. `clients/portal/src/integrations/CampaignsPage.tsx` is the one
authoring UI, the portal is deprecated in favour of MemQL OS, and nothing
is ported -- an app in the OS is how an operator gets to their campaigns
at all once the portal stops being where work happens.

Two things make this more than a port. The portal page counts campaign
outcomes in the browser (section G), which under-reports every campaign
past a page bound and does it silently; SP1's `campaignStats` replaces it
with exact server-side counts and this app is that builtin's only
consumer. And `v1:campaigns:*` has **no broadcast routing rule of any
kind**, so every campaigns row is dark to the mesh: a list would be
correct on load and frozen afterwards, which is the failure that looks
most like the app working.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| A1 | Sections | **Campaigns, Audiences, Templates, Senders, Settings** -- plus **Rules** when SP2 lands (#4830). Declared as `CAMPAIGNS_SECTIONS` in `apps/campaigns/settings.ts` rather than as a literal in the manifest, so the gear target and the section list cannot disagree |
| A2 | Senders is a section of THIS app | `v1:campaigns:senderIdentity` rows have to be creatable somewhere or the campaign identity picker is empty on every fresh cluster -- and an empty required picker reads as a broken form rather than as unconfigured. Settings/Integrations (SP4) is about CREDENTIALS; a sending identity carries **no secret material** (D3: it is the operator's declaration that a mailbox exists and may be used) and is an ordinary owned campaigns-domain record with an account tie. It belongs beside the campaigns it names. See A2 note below |
| A3 | No manifest role gate | The nine operator-facing campaigns concepts are composite tier, so row admission already decides what a person sees and a manifest `roles` entry would be presentation pretending to be authorization. Same argument the Accounts, Files and Deployables apps record |
| A4 | Live vs on-demand | **LIVE** (new broadcast rules): `campaign`, `audience`, `template`, `senderIdentity`, and `emailRule` when SP2 lands -- low-volume rows a person edits, where real liveness is the point. **ON-DEMAND** with a printed read time: `recipient`, `delivery`, `engagementEvent`, and the stats. `delivery` already carries a recorded routing EXCLUSION with its reason; `engagementEvent` gains one in the same table (A5) |
| A5 | The exclusions are declarations, not omissions | `component/node/routing_reach.go`'s `RoutingExclusions()` requires a REASON on every entry and is read by a gate. `v1:campaigns:delivery` is already there ("One row per recipient per send... the campaigns surface reports progress by counting rows rather than by watching them arrive"). `v1:campaigns:engagementEvent` is the same shape one order of magnitude worse -- one row per open and per click -- and gets its own entry rather than being left unmentioned, so a later reader can tell a decision from a gap. The precedent both cite is `v1:worker:invocation` |
| A6 | Campaign fingerprints EXCLUDE the send counters | `sentCount`, `failedCount`, `skippedCount` and `recipientCount` move on every worker tick for the whole duration of a send. Naming any of them in the arrival fingerprint turns a sending campaign into a strobe. The row still RE-RENDERS live -- that is the point of the counters being broadcast -- it just does not RING. Fingerprint what a person would call a change: `name`, `status`, `scheduledAt`, `templateId`, `audienceId`, `senderIdentityId`, `accountId`, `fromName`, `replyTo`, `trackOpens`, `trackClicks` |
| A7 | Progress is a number, arrival is a cue | Because A6 removes the counters from the fingerprint, the app needs a second, honest way to show a send moving: the campaign detail renders the counters continuously (they are on `campaignFull`) with the `status` pill as the state, and calls `campaignStats` on demand for the full breakdown. A live counter and an arrival ring answer different questions and conflating them loses both |
| A8 | The account tie is required at create, in the app only | `createCampaign` / `createAudience` / `createTemplate` / `createSenderIdentity` all keep `accountId` OPTIONAL in the schema, like every other tie in the tree (accounts D1). The app requires picking one -- the operator's own company is the seeded `v1:accounts:account:self` row, so "no client" is never the honest answer here even though the schema permits it. Enforcement is a required field on the create form, never a filter: **the tie is a record, never a visibility scope** |
| A9 | Nothing is inserted locally | A created campaign arrives on its own broadcast with the arrival cue, exactly like one somebody else created. The create form closes on the mutation returning, not on the row appearing; the list is the subscription's business |
| A10 | The portal page is reference-only | Nothing is ported. Its structure informs the section split and nothing else, and its client-side counting is the specific bug `campaignStats` exists to replace (G) |
| A11 | Unconfigured says so, and says where | When SP4's `integrationStatus` reports the email integration unconfigured, the Campaigns app renders a "needs configuration -- open Settings" cue in surface, beside the control that would have failed. It does not hide the app, and it does not attempt the send and render the refusal: the refusal is the engine's sentence about an attempt, and there is no reason to make one |

**A2, stated the other way round**, because the fork is real and the
wrong answer is defensible: Settings could own sender identities, on the
argument that "who we send as" is deployment configuration. It is not, for
three reasons. The row is per-USER and composite-tier, so it is one
operator's record rather than a cluster fact; it carries an `accountId`
tie, which is a campaigns-domain relationship Settings knows nothing
about; and the moment an operator is picking an identity for a campaign
they are in this app, so putting its creation two apps away turns the
common case -- a new client, a new mailbox, a first campaign -- into a
navigation exercise. What DOES belong in Settings is the Graph credential
the identity sends through, and that is exactly SP4's split.

## A. Sections

**Campaigns.** The list is a `LiveList` over the `campaigns` collection
with A6's fingerprint. Detail is the editor plus outcomes: the form
(name, audience, template, sender identity, account, `fromName`,
`replyTo`, `scheduledAt`, `trackOpens`, `trackClicks`), the send controls
(start / schedule / pause / resume / cancel), a **test send** field, the
live counters, the on-demand stats breakdown, and an on-demand delivery
ledger page. The identity picker prefills from the selected account's
identities via `senderIdentitiesForAccount` and offers the rest -- prefill
is UX, and the engine never infers an identity from `accountId`.

**Audiences.** List plus recipients. The recipient table is an on-demand
read with a printed read time, because an audience is the one campaigns
list that is genuinely large. Import is the section's own control: a
Library artifact picker, `hasHeader`, then `campaignImportRecipients`,
whose `{added, duplicates, invalid, total}` plus up to twenty sample
invalid lines render as a result panel in surface. An import that refuses
whole -- over `MEMQL_CAMPAIGNS_MAX_AUDIENCE` -- renders the engine's
sentence, because "it never silently truncates" is only useful if the
operator is told which of the two happened.

**Templates.** Source editor for subject, HTML and text parts, with the
closed merge-tag set documented beside the field and a preview rendered
against a synthetic recipient. Marking a template `ready` is a distinct
act and the campaign preflight refuses a draft, so the editor says which
state it is in rather than leaving the operator to discover it at send
time.

**Senders.** List, create, edit, and the `active`/`disabled` toggle
(A2). The list shows `address`, `fromName`, the account tie and the
status; disabling is presented as retiring a mailbox rather than deleting
one, because past campaigns name it and the reputation history is keyed
on its address.

**Settings.** Per-app preferences in the app's own versioned, sanitized
store -- never a corner of the desktop document, so an app learning a
checkbox cannot cost anyone their desks.

**Rules** arrives with SP2 and is specified in
[2026-09-01-event-emails-design.md](2026-09-01-event-emails-design.md).

## B. Which rows are live, and why the split is not an optimization

The mesh forwards a `graph.node.*` event only when a routing rule matches;
unmatched topics are default-deny. `v1:campaigns:*` matches nothing today,
so every campaigns list in this app would load correctly and then never
change -- including a campaign the operator just started, whose counters
are written by a worker on another replica. T8 adds `created` + `updated`
broadcast rules for `campaign`, `audience`, `template` and
`senderIdentity`; SP2 adds `emailRule`.

The four that stay dark stay dark for a stated reason:

| Concept | Why not live | What the app does instead |
|---|---|---|
| `delivery` | one row per recipient per send; a modest campaign is a larger burst than everything else in the routing table together. Already in `RoutingExclusions()` | on-demand ledger page + `campaignStats` for the counts |
| `engagementEvent` | one row per open and per click, on a list that is by definition larger than the roster. New exclusion entry (A5) | folded into `campaignStats` server-side |
| `recipient` | an audience is the large list; a roster page is a read, not a feed | on-demand table with a printed read time |
| `suppression`, `sendJob`, `reputationWindow`, `warmupState` | clusterOwner-tier engine rows a browser cannot read at all under an ordinary operator | not rendered here; the operator surface for them is the runbook's queries |

Row admission gates subscriptions on the same function that gates reads,
so the composite tier means a cluster owner's live list carries every
operator's campaigns and an ordinary operator's carries their own. Nothing
in the app filters by account, and nothing filters by owner: both are the
server's answer.

## C. The arrival cue, and the campaigns-specific trap

The OS rule is that a fingerprint decides what counts as news, and that
liveness fields must stay out of it or a list strobes. Campaigns carry
the sharpest instance of that rule in the tree, and it is not a
`lastSeenAt`-shaped field, which is why it is easy to miss.

**The send counters are the trap.** During a send, `sentCount`,
`failedCount` and `skippedCount` are rewritten by the drain worker on
every batch -- for minutes, for as long as the campaign takes. A
fingerprint naming any of them makes the campaign that is doing exactly
what the operator asked for the one row that will not stop ringing, and
makes the ring worthless everywhere else, because the operator learns to
ignore it. A6 is the rule; the test is a fixture that advances only
`sentCount` across two snapshots and asserts zero ticks.

The DSL says the same thing about SP2's rule row from the other end:
`emailRule.lastFiredAt` is documented in the concept as *"a LIVENESS
field... Display it; do not ring on it"*, and `firedCount` as *"shown,
never fingerprinted"*. The same discipline, one domain, three fields.

Two supporting rules the app inherits rather than reinvents. A seed or a
resync is not an arrival, so a reconnect does not ring the whole list. And
re-baselining on a filter change goes through the live view's `viewKey`,
not a React `key` prop, so switching the account filter does not replay
every row as new.

**One live-collection key rule matters here specifically:** a collection
key must encode everything the spec reads and nothing that merely arrives
late. The campaigns lists are keyed on constants (`"campaigns:list"` and
siblings) with filtering done client-side over the snapshot, so an
asynchronously-resolved account id or actor id can never restart a
collection from empty mid-render.

## D. The account tie

`campaign`, `audience`, `template` and `senderIdentity` each carry an
optional `accountId` with a `forAccount` relationship, and `emailRule`
does too. The app's treatment is the one the Accounts app already
established:

- **At create**, an `AccountPicker` (exported from
  `clients/os/src/apps/accounts/AccountPicker.tsx`) with the account
  required by the FORM (A8). The picker offers archived accounts suffixed
  and synthesizes an unresolvable held value rather than dropping it,
  which is what stops an edit silently re-tying a row to a different
  client.
- **On detail**, an `AccountChip` beside the name.
- **In the Accounts app**, a fifth rollup band in `AccountDetail.tsx`
  beside deployables, files, knowledge and guests, reading
  `campaignsForAccount` -- the rollup SP1 added to
  `dsl/accounts/queries.memql` copying `sitesForAccount` including its
  tier conjunct. It follows the band contract exactly: an independent
  `run()` rather than a shared `Promise.all`, a count, up to five named
  rows, "and N more", the read time printed, a Re-read button, and a
  server refusal rendered verbatim as the band's body rather than as an
  empty state.

The band is deliberately not live. The other four are not either, and the
whole ledger prints "These are not live -- re-read to see changes made
since", which is the honest presentation of an on-demand read and the one
the account detail already makes.

## E. Errors, refusals and the two honest absences

Every refusal renders in surface, beside the control that produced it,
in the engine's own words. There are no toasts in this shell, and a
refusal inside a dialog that then closes is a refusal nobody can re-read.

Three refusals are worth naming because the app must not paraphrase them:

- **Preflight.** `campaignStartSend` and `campaignScheduleSend` refuse on
  no sender, unconfigured one-click unsubscribe, an unready template, an
  empty or over-ceiling audience, or a missing/disabled sender identity.
  The app renders the sentence and, for the environment-kind refusals,
  says that the send waits and retries rather than implying it failed --
  the runbook's authoring-versus-environment split is a distinction the
  operator acts on.
- **The two absences in the stats.** `campaignStats` reports unique open
  and click counts as **unmeasured** rather than as a number when the
  bounded engagement read comes back at its bound, and reports **no soft
  bounce figure per campaign at all** because nothing measures one. The
  app renders "unmeasured" and omits the soft-bounce row entirely. It
  must not render either as `0`: an absent figure and a zero are different
  answers, and a zero here would be read as "no soft bounces", which is a
  claim nothing in the system can make.
- **Row-authz absence.** A referent the caller cannot read -- a template
  belonging to another operator, an account outside their view -- renders
  as its id in the data voice, never blank. The lookup batching in
  view-kit already behaves this way; the app must not add an empty-string
  fallback on top of it.

## F. A shape is what a client can read

Before rendering a field, read what the SHAPE projects rather than what
the concept declares -- a concept field with no shape entry is invisible
to every client, and the omission presents as "no data". The landed
shapes were written for this app, so the four fields most likely to be
assumed absent are present: `campaignFull` projects `accountId`,
`senderIdentityId`, `trackOpens` and `trackClicks` alongside the four
counters, `senderIdentityFull` and `emailRuleFull` project their full
field sets, and `engagementDeliveryRef` exists so the stats path can fold
engagement rows without pulling the whole event.

The converse rule applies to the recipient and delivery tables: both
shapes are `@pii`, so nothing about them is cached or exported casually
and the tables are read on demand under the caller's own actor.

## G. What the portal page got wrong, and why it is not portable

`clients/portal/src/integrations/CampaignsPage.tsx` and
`CampaignEditorPage.tsx` are reference-only. The portal is deprecated;
nothing is ported. One defect in them is worth recording because it is the
reason a builtin exists.

`useCampaigns.ts`'s `useCampaignDetail` computes the audience's sendable
count as `sendable.length` over the rows returned by
`sendableRecipientsForAudience`, and that query carries `paginate 100`.
So the number rendered as *"N subscribed of this audience would be
mailed"* saturates at 100 for every audience larger than 100, with no
indication that it did. The delivery outcomes on the same page are counted
the same way over `deliveriesForCampaign`, also `paginate 100`. A bounded
read of an unbounded set is a truncation, and presenting one as a total is
the failure mode the engine's own roster walk was rewritten to remove.

`campaignStats` replaces it with server-side aggregation whose count
buckets are exact at any audience size -- and, where a bucket genuinely
cannot be exact, says so (E) rather than rounding into a number. This app
must not reintroduce a client-side count anywhere: if a figure is not on
`campaignStats`, it is not rendered.

## H. Testing

- **Fingerprint test (A6):** two snapshots differing only in `sentCount`
  produce zero arrival ticks; two differing in `status` produce one. This
  is the test that fails if somebody "completes" the fingerprint by adding
  the counters.
- **Live-list contract:** the collection is retained at the app root and
  the sections consume the handle, so no section can render "Loading from
  the cluster" forever by subscribing without retaining.
- **Routing:** the new rules appear in `defaultRoutingRules()` and the two
  exclusions carry reasons; the existing routing gate reads the source and
  the exclusions table on every run, so an added concept with neither is a
  build failure rather than a silent dark list.
- **Stats rendering:** unmeasured renders as "unmeasured", the soft-bounce
  row is absent, and a `0` for either is a test failure.
- **Import result:** the refuse-whole path renders the engine's sentence;
  the per-row path renders the sample invalid lines with their line
  numbers.
- **Settings contract:** `settingsSection` names a section the manifest
  declares -- the existing registry test covers this once the app is in
  `OS_REGISTRY.apps`.
- **Account band:** a refused rollup renders the server's sentence in the
  band body, not an empty state.
- Verification is `make os-build` plus the OS vitest suite. A green
  typecheck is not evidence about the stylesheet: only the build parses
  it.

## I. A doc defect this phase inherits

`clients/os/README.md` states that `useAccountOptions` keys its collection
on one string so several surfaces mounting it share one subscription.
`clients/os/src/apps/accounts/tie.tsx` documents the opposite in detail
and is the accurate account: `live/useLiveCollection.ts` constructs a
`LiveCollection` per component memoised on `[connection, key]` and never
consults the SDK's shared registry, so four pickers mounted at once open
four subscriptions. The Campaigns app adds a fifth mounting surface, which
is what makes the claim worth correcting rather than leaving. The fix
belongs to the README's owner; this spec records which of the two is true
so the next reader does not have to measure it again.

## J. Delivery

- **PR 6 -- T8 (#4827):** the broadcast rules and exclusions, the app
  shell and manifest, and the four list sections with their live
  collections, fingerprints and create forms.
- **PR 7 -- T9 (#4828):** campaign detail (stats, delivery ledger, send
  controls, test send), audience recipients and CSV import, the template
  editor and preview, and the `campaignsForAccount` band in
  `AccountDetail.tsx`.

Each PR branches from and targets `main` -- never stacked -- and each
leaves the OS build and vitest green on its own. The routing-table change
in PR 6 alters what the mesh forwards and is called out explicitly in the
commit body.
