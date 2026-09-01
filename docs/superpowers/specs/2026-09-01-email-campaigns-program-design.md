# Email Campaigns Program -- Design

- **Date:** 2026-09-01
- **Status:** approved (in-session Q&A with the owner; every fork below
  records the choice that was made and why)
- **Scope:** program umbrella for four sequenced sub-projects. **SP1** the
  campaigns backend (full spec:
  [2026-09-01-campaigns-backend-design.md](2026-09-01-campaigns-backend-design.md)),
  **SP4** integration configuration from OS Settings, **SP3** the Campaigns
  OS app, **SP2** event-triggered emails. This document holds the
  assessment, the program-level locked decisions, and the phase map. SP2 /
  SP3 / SP4 get their own specs written when each starts, against this
  document's decisions -- a session picking one of those up starts by
  writing that spec.
- **Sequencing:** SP1 -> SP4 -> SP3 -> SP2. Config before the app so the
  app ships with the configure-from-Settings story instead of retrofitting
  it; event emails last because they consume SP1's sender identities and
  SP4's status plumbing.
- **Follow-ups noted, not built in this program:** A/B testing;
  multi-step journeys/drips (an event rule is one trigger -> one email);
  client-tenant sending and any Azure Communication Services transport
  (deliberately future, slotting behind the senderIdentity record);
  per-account suppression (rejected -- see P8); signup forms / landing
  pages; text-part link tracking.

## Why

Owner's brief, condensed: MemQL runs email campaigns for the operator's
own company AND for their client accounts (the accounts app's rows -- one
account, many campaigns, many templates). Core Mailchimp, not the kitchen
sink: audiences, templates, scheduled sends, stats, open/click tracking,
CSV import -- plus **event-triggered emails** ("when X happens on concept
Y, send template Z", e.g. "when a new admin user is added, email the
owner") authored in the app and materialized through the DSL, because the
DSL's constructs (automations, logic, builtins over integrations) are the
platform's composition surface. All of it surfaced as a Campaigns app in
MemQL OS. And cross-cutting: integrations must be configurable from OS
Settings by owner/developer roles, so a fresh cluster boots clean with
nothing configured and features refuse with stated reasons instead of
breaking.

## Where the tree already was (assessed 2026-09-01)

The sending engine is production-shaped and deeper than the top-level
docs said: `component/campaigns` (~7.5k lines, 122 unit tests) has the
full campaign lifecycle including scheduling with fire-time preflight,
a drain worker (token bucket, provider 429/`Retry-After` parking, retry
backoff, cross-replica claims, keyset roster walk, crash-resume via the
delivery ledger), RFC 8058 one-click unsubscribe with a rotation-safe key
ring, cluster-wide digest-keyed suppression enforced at point of send,
bounce/complaint webhook ingestion (RFC 3464 + SES parsers behind
`POST /inbound/{source}`), per-domain reputation telemetry and an
evidence-driven warmup ramp. Ten concepts under `dsl/campaigns/`; the
whole query/mutation/builtin surface is already generated into both SDKs.

What the assessment found missing, and this program builds:

| Layer | State found | Program answer |
|---|---|---|
| Account/client dimension | none -- `ownerUserId` only; "shared / team-owned campaigns are a deliberate non-goal" (`dsl/campaigns/concepts.memql:29`) | SP1: account tie + composite tier + sender identities |
| Recipient management | one-at-a-time `addRecipient`; no import path anywhere; `source:"import"` never written | SP1: CSV import + `fields` map |
| Templates | one merge tag (`{{displayName}}`); no test send | SP1: closed merge-tag set + `campaignTestSend` |
| Outcomes visibility | no stats query; `skippedCount` on a row browsers cannot read; no open/click tracking (SES open/click events explicitly discarded, `feedback_ses.go:123`) | SP1: `campaignStats` + tracking endpoints + `engagementEvent` |
| Bounce loop on Graph | webhook ingestion exists but nothing feeds it -- DSNs land in the sending mailbox and nothing reads them | SP1: Graph NDR reader |
| Event-triggered email | no user-facing rules feature; the runtime authoring pipeline exists and is live-capable | SP2 |
| OS surface | none; authoring UI exists only in the deprecated portal (`clients/portal/src/integrations/CampaignsPage.tsx` -- reference, not portable); `v1:campaigns:*` has no broadcast routing rule | SP3 |
| Integration config from OS | `integrations/email/lazy.go` already resolves credentials from `globalVariable`/`globalSecret` rows as a dev fallback; `integration.email.status` exists; no Settings surface, no general model | SP4 |

**Transport truth, settled:** Microsoft Graph `sendMail` from one mailbox
per cluster (SMTP fallback; `LogSender` local-only, memql#4477). Azure
Communication Services was evaluated and explicitly rejected --
memql#4218, recorded at `docs/public/operate/azure-entry-install.md`
("Graph is the ONLY path... no SMTP, no Azure Communication Services" on
that install) -- and appears nowhere else in code or docs. The
one-mailbox assumption is load-bearing in three places: the cluster-wide
suppression rationale, the reputation/warmup `sendingIdentity` key, and
the structural SPF/DKIM stance (`From` is not caller-settable,
`integrations/email/mime_test.go`). P1 below is designed to respect all
three.

## Locked decisions (program level)

| # | Decision | Choice (owner-approved) |
|---|---|---|
| P1 | Sender model | **Per-account sender identity, mailboxes in the operator's tenant.** New `v1:campaigns:senderIdentity` record; a campaign carries an optional `senderIdentityId` (the app prefills from the selected account); absent means the env-configured cluster default, so today's behavior is the default case. Client-owned domains / client tenants are a later transport addition behind the same record. Reputation and warmup already key per sending identity (`component/campaigns/config.go` `derivedSendingIdentity`), so they simply start seeing plurality |
| P2 | Team access | **Composite tier** `@rowAuthz(owner="ownerUserId", clusterOwner)` on the operator-facing campaign concepts (the tier `v1:accounts:account` already uses). Each user's campaigns stay theirs; the cluster owner gains read + builtin-control oversight; direct row writes stay owner-scoped (the write guard ignores the second argument, memql#4312). Full team sharing (granted tier) deliberately deferred |
| P3 | v1 scope | CSV import, server-computed stats, test-send + widened merge tags, **and open/click tracking** -- all four, owner's call. Tracking's two new public HTTP endpoints are hereby owner-approved as documented exceptions (the mail client dictates the wire -- the `/unsubscribe` category) |
| P4 | Event-email execution | **Each rule materializes as a real authored automation construct** through the existing runtime authoring pipeline (`v1:authoring:*` bundle/construct rows -> validate -> activate -> `AuthoredRuntimeRegistry` + `AuthoredScheduler`), generated **deterministically from the form -- no LLM**. Reason: an automation's `@trigger` names ONE concept at load time, so arbitrary user-chosen trigger concepts cannot be pre-shipped; and the governance rails (per-rule pause, circuit breaker, cluster kill switch `authoredAutomationsEnabled`, boot re-arm, author-scoped actor) come free. Known gap to close in SP2: `ActivateApprovedBundle` has no production caller (`component/grpc/authoring_handlers.go:23`), so activation today takes effect at next boot -- SP2 wires it so a rule goes live immediately |
| P5 | Event-email recipients | **Two lanes by recipient kind.** Recipients that are cluster users/roles ("email the owner") ride the transactional outbox (`stageOutboundRequest` -> `v1:platform:outboundRequest`): allowlist enforced, no unsubscribe footer, no suppression. Recipients that are audience members or addresses read off the triggering row ride the campaign machinery via a new single-recipient send primitive: suppression checked, unsubscribe attached, sender identity applied, outcome ledgered. Mirrors the engine's own campaigns-vs-outbox split (`docs/public/operate/campaign-sending.md`, "Campaigns do not use the transactional outbox") and keeps the shared suppression list away from operational mail |
| P6 | Integration config | Graph-backed runtime config: values in the existing `v1:platform:globalSecret` / `globalVariable` rows, resolved at use time (the `integrations/email/lazy.go` pattern, generalized), **gated owner-or-developer** (`owner/admin/developer/writer/reader` -- `developer` is first-class, `component/auth/rbac.go`; explicitly NOT admin). Boot-envelope variables (DSN, master key, identity URLs) are excluded -- the env-vars doc's bootstrap-vs-concept-stored line is the boundary, and Settings says so rather than offering fields that cannot work. Per-integration status (needs configuration / configured / unhealthy) via status capabilities; `integration.email.status` is the prototype. Unconfigured never breaks boot: features refuse with the stated reason, exactly like the campaigns preflight today |
| P7 | Account tie | Optional `accountId string` + `@relationship(type="references", as="forAccount", field="accountId", target=account, direction="outgoing")` on `campaign`, `audience`, `template`; recipients inherit through their audience; a `campaignsForAccount` rollup joins the four existing ones in `dsl/accounts/queries.memql`. **The tie is a record, never a visibility scope** (`dsl/accounts/concepts.memql` D1 -- the house rule). The app requires picking an account at create; the operator's own company is the existing `v1:accounts:account:self` row |
| P8 | Suppression | **Stays cluster-wide.** Several sending identities inside one operator tenant are still one legal sender org; an unsubscribe is a statement to this deployment's operator. Per-account suppression is rejected -- it would let one account mail an address that unsubscribed from another |
| P9 | Delivery | One epic (owner's call), four sequential phases, eleven tasks, nine PRs -- grouping stated on the epic and on every task. Each phase is independently shippable |

## The four sub-projects

### SP1 -- Campaigns backend (phase 1; spec: [2026-09-01-campaigns-backend-design.md](2026-09-01-campaigns-backend-design.md))

Account tie, composite tier, sender identities + Graph send-as-identity,
CSV import + `recipient.fields`, merge tags + test send, stats +
open/click tracking, the Graph NDR reader, and the hardening/defect
ledger below. Five tasks, three PRs. All detail in its spec.

### SP4 -- Integration config in OS Settings (phase 2; spec written at start)

What the spec-writer inherits from this session's research:

- The runtime-resolution precedent is `integrations/email/lazy.go`:
  env first, then `globalVariable`/`globalSecret` rows by the SAME key
  names, then the disabled state -- resolved on first use, no restart.
  Generalize that tiering into a declared per-integration config manifest
  (key, secret-vs-variable, what functionality each key unlocks), rather
  than each integration hand-rolling it.
- The env registry (`component/envregistry/manifest.yaml`) already
  carries per-var metadata and is gated in CI; the boot-envelope vs
  runtime-configurable boundary must be declared there, not inferred.
- Status: `integration.email.status` (`dsl/integrations/builtins.memql`)
  is the prototype status capability; every configurable integration
  grows one, and the Settings cards render from them.
- Surface: the OS Settings app already has role-gated sections (Cluster
  is admin+); the new **Integrations** section gates owner-or-developer.
  Per-app config lives here, not in the app windows; the app windows may
  show a "needs configuration -- open Settings" cue when their
  integration reports unconfigured.
- First consumer: the campaigns/email stack (Graph credentials, sender
  identity defaults, unsubscribe secrets -- minus anything
  boot-envelope).

### SP3 -- The Campaigns OS app (phase 3; spec written at start)

What the spec-writer inherits:

- `clients/os/README.md` is required reading before any of it (the
  live-collection contract, the arrival-cue rule, in-surface errors, no
  local inserts, shapes-not-concepts, `settingsSection` required).
- `v1:campaigns:*` has **no broadcast routing rule** today
  (`component/node/routing.go` -- grep is empty). The app phase adds
  rules for `campaign` / `audience` / `template` / `senderIdentity`
  (low-volume rows, real liveness); `delivery` and `engagementEvent`
  stay on-demand reads that print when they were read (volume -- the
  `v1:worker:invocation` precedent).
- Arrival-cue detail decided here: campaign fingerprints EXCLUDE the send
  counters (`sentCount` etc. move every worker tick mid-send; the row
  re-renders live but must not ring) -- fingerprint what a person would
  call a change: name, status, `scheduledAt`, template/audience/identity
  ties.
- Account integration: `AccountPicker` / `AccountChip`
  (`clients/os/src/apps/accounts/AccountPicker.tsx`) at create (required
  in-app) and on detail; a campaigns rollup band in `AccountDetail.tsx`
  beside sites/domains/library/invitations.
- The portal surface (`clients/portal/src/integrations/CampaignsPage.tsx`,
  `CampaignEditorPage.tsx`, `useCampaigns.ts`) is reference-only -- the
  portal is deprecated; nothing is ported, and its client-side stat
  counting (page-capped at 100) is the bug the `campaignStats` builtin
  replaces.
- Sections: Campaigns (list + detail: stats, deliveries, controls),
  Audiences (recipients, subscription states, import), Templates (editor,
  merge-tag help, test send), Rules (arrives with SP2).
- Small doc defect to fix in passing: `clients/os/README.md` claims
  `useAccountOptions` shares one subscription across apps; `tie.tsx`
  documents the opposite and is the accurate account.

### SP2 -- Event-triggered emails (phase 4; spec written at start)

What the spec-writer inherits:

- Storage/UX row: `v1:campaigns:emailRule` (form state: trigger concept +
  event kind, condition, template, lane, recipients, account tie, status,
  generated bundle/construct refs). The rule's EXECUTABLE form is the
  authored construct P4 describes; the row is what the app lists, edits,
  pauses.
- The generator is a deterministic template producing an automation (+
  logic where needed) from the form -- the LLM `authoringEmit` path stays
  off. Trigger concepts come from the live concepts registry
  (`ConceptsListMsg`).
- The pipeline map: `component/memql/authoring_runtime.go`,
  `component/automations/authored_scheduler.go`,
  `app/engine_authored.go` (`wireAuthoredRuntime`, boot re-arm),
  activation orchestrator `component/memql/authoring_activation_engine.go`
  -- and the gap: wire a production caller for `ActivateApprovedBundle`
  so activation is immediate (today: next-boot re-arm only).
- The actor trap, documented twice in-tree (`component/campaigns/schedule.go`
  top comment; `deploy/fleet/dsl/fleet/billing.memql`): an automation's
  actor is not the row's owner, and a caller-scoped read from inside one
  returns nothing while looking correct. Authored automations run under
  the AUTHOR's envelope (`AuthorContext`, role writer) -- the spec must
  verify both lanes' send paths are reachable from that envelope, with
  the fleet pack's recipient-denormalization trick as the documented
  fallback.
- Lane primitives: operational lane calls the existing
  `stageOutboundRequest` mutation (worked examples:
  `deploy/fleet/dsl/fleet/automations.memql` `welcomeOnInstanceRunning`,
  `trial.memql` `trialNudgeDay7` for the cron + `forEach` shape).
  Marketing lane needs the new single-recipient send builtin SP1 does NOT
  build -- it belongs here, over SP1's identity + suppression + ledger
  machinery. There is deliberately no DSL-callable free-form
  `sendEmail` builtin today (`dsl/identity/automations.memql` records
  that); the lanes are the two sanctioned shapes.

## Defect ledger (independent of the features; fixed where noted)

1. **Preflight bypass:** `startCampaign`, `pauseCampaign`,
   `resumeCampaign`, `scheduleCampaign`, `updateCampaignProgress`,
   `recordCampaignDelivery` are ordinary owned-tier mutations a browser
   can call directly, desyncing campaign state from the engine
   (`dsl/campaigns/mutations.memql` header says "only ever reached
   through the builtins" -- nothing enforces it). Fix: `@serverOnly` +
   internal-origin stamping at the Go call sites (SP1/T1).
2. **`campaign.fromName` never reaches the wire** -- used only in the
   unsubscribe footer (`component/campaigns/render.go`), while the field
   doc promises a per-campaign From display name. Fixed by the identity
   work (SP1/T2).
3. **The Graph bounce loop is open:** DSNs return to the sending mailbox;
   the ingestion path waits behind `POST /inbound/{source}` and nothing
   feeds it on a Graph deployment. Fix: the NDR reader (SP1/T5).
4. **`consentEvent` has zero writers** -- a complete, generated,
   never-written compliance surface (`component/campaigns/consent.go`
   helpers are test-only). Fix: unsubscribe + feedback ingest + import +
   operator suppress start emitting (SP1/T1, T3).
5. **Stale docs:** root `CLAUDE.md` campaigns note said the scheduler and
   warmup ramp were "not built" (both shipped) and counted seven concepts
   (ten) -- fixed in the PR landing this spec.
   `dsl/campaigns/concepts.memql:12` says "EIGHT CONCEPTS" (SP1/T1).
   The OS README `useAccountOptions` claim (SP3).

## Delivery map

| Phase | Tasks | PRs |
|---|---|---|
| 1 -- SP1 backend | T1 tie/tier/hardening/consent, T2 sender identity + transport, T3 import/merge/test-send, T4 stats/tracking, T5 NDR reader | PR 1 = T1+T2, PR 2 = T3, PR 3 = T4+T5 |
| 2 -- SP4 config | T6 engine (manifest, resolution, status caps), T7 OS Settings Integrations section | PR 4 = T6, PR 5 = T7 |
| 3 -- SP3 app | T8 broadcast rules + app core, T9 detail surfaces (stats, deliveries, import, test send) | PR 6 = T8, PR 7 = T9 |
| 4 -- SP2 event emails | T10 spec + engine (rule, generator, activation, single-send), T11 Rules section in the app | PR 8 = T10, PR 9 = T11 |

Every PR branches from and targets `main` (never stacked -- a stacked
base gets no CI here). Each phase leaves `main` shippable.
