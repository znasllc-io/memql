# Campaigns Backend (SP1) -- Design

- **Date:** 2026-09-01
- **Status:** approved (in-session Q&A with the owner; program decisions
  P1-P9 in
  [2026-09-01-email-campaigns-program-design.md](2026-09-01-email-campaigns-program-design.md)
  are the authority this spec elaborates)
- **Scope:** `dsl/campaigns/` (schema, queries, mutations, builtins),
  `dsl/accounts/queries.memql` (one rollup), `component/campaigns/`
  (worker, render, stats, tracking, tokens), `integrations/email/`
  (multi-identity Graph send + the NDR reader), `component/server` (the
  two tracking paths) + front-door regeneration, `component/envregistry`
  (new vars), runbook updates (`docs/public/operate/campaign-sending.md`,
  `azure-entry-install.md` pointer). **No OS work here** -- that is SP3.
- **The wave this belongs to:** Phase 1 of the email-campaigns program;
  five tasks, three PRs (grouping at the bottom and on every task issue).
- **Follow-ups noted, not built here:** the SP2 single-recipient send
  builtin (belongs to the event-email spec); client-tenant identities /
  ACS; engagement-row retention sweep; text-part link tracking;
  per-recipient timezone sends.

## Why

The engine sends one operator's campaigns from one mailbox. The program
makes campaigns a multi-client product: campaigns/audiences/templates tie
to accounts, each account can send as its own identity (mailboxes in the
operator's tenant, P1), teams get owner oversight (P2), and the v1
feature cut (P3) adds the four things an operator actually needs to run a
real list: import, stats, test send, open/click tracking -- plus closing
the Graph bounce loop, without which suppression quietly starves on the
chosen transport.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| D1 | Account tie | `accountId string` (optional) + `@relationship(type="references", as="forAccount", field="accountId", target=account, direction="outgoing")` on `campaign`, `audience`, `template`. Recipients inherit through `audienceId` -- no direct field. New `campaignsForAccount` rollup in `dsl/accounts/queries.memql` beside the four existing ones (`sitesForAccount` is the exact model to copy, including the tier conjunct). The tie is a record, never a read filter (accounts D1). Requiring an account is APP behavior (SP3); the schema keeps the field optional like every other tie in the tree |
| D2 | Tier migration | The six operator-facing concepts (`audience`, `recipient`, `template`, `campaign`, `delivery`, `consentEvent`) move from `@rowAuthz(owner="ownerUserId")` to the composite `@rowAuthz(owner="ownerUserId", clusterOwner)`. Effect: cluster owner gains read + the builtin controls (their gate is "can the caller read the campaign row"); direct row writes stay owner-scoped (the write guard ignores the second argument, memql#4312). The four engine concepts stay `clusterOwner`. Every authored query keeps its tier predicate as a top-level conjunct -- the conformance land-gate requires it |
| D3 | `senderIdentity` concept | New `v1:campaigns:senderIdentity`, composite tier: `ownerUserId string!`, `address string!` (the mailbox UPN the Graph app may send as), `fromName string!`, `replyTo string` (default for campaigns that set none), `accountId string` (+ `forAccount` relationship), `status enum("active","disabled") @default("active")`, `notes string`. **No secret material** -- authentication stays the cluster's one Graph credential; an identity row is the operator's declaration that this mailbox exists and may be used. CRUD mutations (`createSenderIdentity`, `updateSenderIdentity`, `setSenderIdentityStatus`) + `senderIdentities` / `senderIdentitiesForAccount` queries + a full shape. Address is validated for form and header-safety (no CR/LF -- it becomes an RFC 5322 header) |
| D4 | Resolution order | `campaign.senderIdentityId` (new optional field) -> else the env-configured default sender. The app prefills the picker from the account's identities; the ENGINE never infers an identity from `accountId` -- prefill is UX, resolution is explicit. Preflight (both start and schedule, at both authoring and fire time) refuses a campaign naming a missing or `disabled` identity, with the reason on `lastError` per the existing environment-vs-authoring refusal split |
| D5 | Transport: send-as-identity | `integrations/email.Message` stays From-less (the `mime_test.go` stance -- caller-supplied From breaks SPF/DKIM alignment -- survives verbatim). The Sender interface gains an explicit identity parameter: `Send(ctx, msg, SendAs{Address, FromName})` where `SendAs` zero-value means "the configured default". `GraphSender` builds `/users/{sendAs.Address}/sendMail` and stamps From from it; the campaigns worker is the ONLY caller passing a non-zero `SendAs`, resolved from the registry -- never free-form. The SMTP path stays single-identity: a campaign naming a non-default identity on an SMTP-only node is an environment refusal (wait-and-retry, reason stamped), because SMTP AUTH is bound to one mailbox |
| D6 | `fromName` on the wire (defect fix) | Display name finally reaches the From header: identity `fromName`, overridden by `campaign.fromName` when set, else the env default -- rendered by the sender (Graph structured payload / MIME `From:` phrase, header-escaped). The field's DSL doc already promises exactly this; the code catches up |
| D7 | Entra scoping (ops) | Each identity mailbox must be a member of the mail-enabled security group behind the Exchange `ApplicationAccessPolicy` that scopes `Mail.Send` (and now `Mail.Read`, D13). Runbook gains a "adding a sending identity" section in `campaign-sending.md`, pointing at the existing `azure-entry-install.md` policy walkthrough. No engine preflight can verify tenant policy -- the honest check is the send refusal surfacing Graph's 403 on `lastError` |
| D8 | Identity keying downstream | `derivedSendingIdentity` (`component/campaigns/config.go`) becomes per-send: the resolved identity's normalized address, falling back to the env-derived value. Reputation rows and warmup state key on it exactly as today -- `warmupStateForIdentity` and the per-identity ramp were built for plurality and start receiving it. Suppression stays cluster-wide (P8) and the unsubscribe token format is untouched |
| D9 | CSV import | New builtin `campaignImportRecipients(audienceId, artifactId, hasHeader)` (`@sdk`): the file arrives through the existing Library artifact upload (bytes on `v1:library:file`, read server-side under the caller's actor); Go streams `encoding/csv`. Column mapping v1: a header row is required to carry `email` (case-insensitive; `displayName`/`name` recognized); every OTHER column lands verbatim in `recipient.fields` (new `fields object` on the concept). Per row: `NormalizeEmail`, RFC-shape validation, dedup against the audience's existing rows AND within the file (first occurrence wins), refuse the whole import only when the resulting roster would exceed `MEMQL_CAMPAIGNS_MAX_AUDIENCE`. Writes `source:"import"` (the enum value finally gets its writer) and a `consentEvent(kind:"grant", source:"import")` per added recipient. Returns `{added, duplicates, invalid, total}` with up to 20 sample invalid lines. The builtin renders batched mutation calls -- and its rendered MemQL is parse-tested (D15) |
| D10 | Merge tags | The replacer set widens to a CLOSED list: `{{displayName}}`, `{{email}}`, `{{campaignName}}`, `{{accountName}}` (empty when untied), `{{fields.<key>}}` for the recipient's `fields` map. Still `strings.NewReplacer` construction per recipient over enumerable keys -- NOT a template engine; the injection stance at the top of `render.go` is preserved verbatim, and the HTML/text escaping split (HTML path `html.EscapeString`s every substituted value, text path does not) applies to every new tag. Unknown tags stay literal in the body; `campaignTestSend` reports them (D11) rather than a hard preflight gate |
| D11 | Test send | New builtin `campaignTestSend(campaignId, to)` (`@sdk`): renders the campaign's template against a synthetic recipient (displayName "Test Recipient", the CALLER's `to` address, `fields` from the audience's first recipient when one exists so `{{fields.*}}` show real shape), subject prefixed `[Test] `, sent through the campaign's resolved identity, unsubscribe footer rendered with an obviously-inert token. Writes NO delivery row, touches NO counters, consumes the ordinary token bucket. Returns the list of unresolved merge tags it found. Gated by campaign read (owner or cluster owner); `to` is required and validated -- it does not default to the caller's email to keep the builtin honest about where mail goes |
| D12 | Stats | The worker starts writing `campaign.skippedCount` (new field; the job already computes it). New builtin `campaignStats(campaignId)` (`@sdk`, Go aggregation -- the DSL deliberately has no group-by): `{recipients, pending, sent, failed, skipped: {suppressed, unsubscribed, other}, bounces: {hard, soft}, complaints, unsubscribed, opens: {unique, total}, clicks: {unique, total}}`, computed from delivery rows (status + `skipReason`), per-campaign `consentEvent` rows (bounce/complaint/withdraw), and `engagementEvent` (D13). Replaces the portal's client-side page-capped counting outright |
| D13 | Open/click tracking | Per-campaign `trackOpens` / `trackClicks` booleans, default true. At render, the HTML part only: every `http(s)` href is rewritten to `<base>/t/c/<token>` and a `<img src="<base>/t/o/<token>" width="1" height="1" alt="">` pixel is appended; `<base>` is the existing `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` (same public origin; no new var). The token is HMAC-signed by the existing unsubscribe key ring under a NEW context string (`memql/campaigns/track/v1` -- same two-key rotation semantics, same `keyId` derivation), payload `{deliveryId, campaignId, kind, url}` base64url -- stateless, and the signature makes the redirect open-redirect-proof (an unsigned or tampered URL never redirects). `GET /t/o/{token}`: always answers the 1x1 GIF, records on valid signature. `GET /t/c/{token}`: 302 to the signed URL; invalid token renders the plain "link not valid" page, never a 500. Both endpoints live on the bff exactly like `/unsubscribe`: a `TrackingPaths()` declaration feeding `HandlerAuthorizedPaths` + `SelfAuthenticatedPaths`, `make frontdoor` regenerated in the same PR. Events land on new `v1:campaigns:engagementEvent` (composite tier): `ownerUserId!`, `campaignId!`, `deliveryId!`, `kind enum("open","click")!`, `url string`, `occurredAt datetime!` -- written via a `@serverOnly` mutation under internal origin. Raw events stored; `campaignStats` computes unique by (delivery, kind). Unbounded in v1; the retention sweep is a noted follow-up |
| D14 | The Graph NDR reader | `integrations/email` gains a poller, active only when the resolved sender is Graph AND campaigns are enabled: every `MEMQL_EMAIL_NDR_POLL_SECONDS` (default 300; 0 disables) it lists the DEFAULT sending mailbox's inbox for unread `multipart/report` messages, fetches each as MIME (`$value`), and stages it as a `v1:platform:inboundRequest` with `source:"graph-mailbox"`, `signatureVerified:true`, dedupe key = the Graph message id (redelivery collapses). Marking `signatureVerified` is honest here because provenance IS the verification: the payload was read from our own mailbox over our own authenticated credential -- there is no third-party signature to check, and the field is what gates `campaignIngestFeedback`. The operator lists `graph-mailbox=rfc3464` in `MEMQL_CAMPAIGNS_FEEDBACK_SOURCES` (documented; the existing automation + DSN parser take over from there). Processed messages are marked read + categorized `memql-processed`; the poller reads identity mailboxes too once plural (per active identity, same policy group -- D7's `Mail.Read` note). New env vars land in `component/envregistry/manifest.yaml` |
| D15 | Hardening + consent writers | `@serverOnly` on `startCampaign`, `pauseCampaign`, `resumeCampaign`, `scheduleCampaign`, `updateCampaignProgress`, `recordCampaignDelivery`; their Go call sites (worker, capabilities, schedule) stamp internal origin -- the write guard refuses an unstamped `@serverOnly` write with only a WARN, so the stamping is load-bearing, not hygiene. Client SDKs regenerate without them; nothing breaks (the portal drives the `@sdk` builtins plus `create*`/`updateCampaign`/`cancelCampaign`, all untouched). Consent writers: the unsubscribe POST emits `consentEvent(kind:"withdraw", source:"one_click")`, feedback ingest emits `bounce`/`complaint` (`source:"provider"`), `campaignSuppress` emits `suppress` (`source:"operator"`), import emits `grant` (D9). The "EIGHT CONCEPTS" header comment gets the real count |

## Schema delta (exact)

- `campaign`: + `accountId string`, `senderIdentityId string`,
  `skippedCount int`, `trackOpens bool @default(true)`, `trackClicks bool
  @default(true)`; tier -> composite; + `forAccount` and
  `sendsAs` (references `senderIdentity`) relationships.
- `audience`: + `accountId string` (+ relationship); tier -> composite.
- `template`: + `accountId string` (+ relationship); tier -> composite.
- `recipient`: + `fields object`; tier -> composite.
- `delivery`, `consentEvent`: tier -> composite.
- New: `senderIdentity` (D3), `engagementEvent` (D13).
- `dsl/accounts/queries.memql`: + `campaignsForAccount`.

A new-construct change fans out: memqllint, both SDK generations, the
env-registry gate (new vars), the arch model, and the embed count all
move in the same PR as the schema they gate.

## Error handling (the spec's standing rules)

- Every refusal names its reason where the operator looks: authoring
  refusals fail the campaign, environment refusals wait-and-retry with
  the reason on `lastError` -- the existing split, extended to identity
  resolution (D4) and SMTP single-identity (D5).
- The tracking endpoints never surface an error to a human as a broken
  image or a 500 (D13): the pixel always answers, the redirect renders
  the same page an invalid unsubscribe link gets.
- The NDR poller treats an unparseable mailbox message exactly as the
  webhook path treats one: staged, stamped failed with the reason,
  visible in `inboundRequestsByStatus(status:"failed")` -- never dropped.
- The import refuses whole (over-cap) or reports per-row (invalid lines
  in the result); it never silently truncates -- the engine's own
  "bounded read of an unbounded set is a truncation" rule.

## Testing

- **Db-gated suites** (`MEMQL_REQUIRE_DB=1` lane) for: import
  (dedup, fields, cap, consent rows), stats aggregation, identity
  resolution + preflight refusals, tier migration (cluster owner reads
  another owner's campaign; a plain user does not).
- **Rendered-call parse tests** for every builtin that writes
  (`campaignImportRecipients`, `campaignTestSend`, the engagement
  mutation): the campaigns test suite drives a fake engine that records
  call STRINGS, which hides render bugs -- each new rendered call is also
  parsed by the real parser in a test.
- **Token tests:** tracking-token round-trip, tamper, wrong-context
  (an unsubscribe token must not verify as a tracking token and vice
  versa), key-rotation ring behavior mirroring
  `token_rotation_test.go`.
- **Transport tests:** `SendAs` reaches the Graph URL and From header;
  zero-value falls back; SMTP + non-default identity refuses; From
  display-name header escaping.
- **NDR fixtures:** real `multipart/report` DSN MIME through poller ->
  staged row -> existing parser -> suppression, plus the
  benign-payload path.
- **Front door:** `make frontdoor-paths-check` green with the new
  `TrackingPaths()`; the paths appear in the generated block.
- **Conformance:** new concepts/queries/mutations classify under the
  authz gates; relationship targets resolve; `make test` (the module
  path form) is the verification command, never bare `./...`.

## Delivery (five tasks, three PRs)

- **PR 1 -- schema + identity (T1 + T2):** D1, D2, D3-D8, D15. The
  fan-out artifacts regenerate here.
- **PR 2 -- list management (T3):** D9, D10, D11.
- **PR 3 -- outcomes (T4 + T5):** D12, D13, D14.

Each PR branches from and targets `main`; each leaves `make test` and
the db-gated lane green on its own.
