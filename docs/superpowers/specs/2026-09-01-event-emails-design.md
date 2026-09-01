# Event-Triggered Emails (SP2) -- Design

- **Date:** 2026-09-01
- **Status:** approved (program decisions P4 / P5 in
  [2026-09-01-email-campaigns-program-design.md](2026-09-01-email-campaigns-program-design.md)
  are the authority this spec elaborates; every fork below records the
  choice that was made and why)
- **Scope:** `dsl/campaigns/` (the `emailRule` concept + its queries,
  mutations and the three builtins, all landed with the schema commit),
  a deterministic construct generator in `component/emailrules`, a
  production caller for `ActivateApprovedBundle`, the marketing lane's
  single-recipient send primitive over SP1's identity + suppression +
  ledger machinery, and the Rules section in the Campaigns OS app.
  Runbook updates to
  [campaign-sending.md](../../public/operate/campaign-sending.md).
- **The wave this belongs to:** Phase 4 of the email-campaigns program,
  and the last. It consumes SP1's sender identities, suppression and
  delivery ledger, and SP3's app shell. Two tasks, two PRs -- **T10**
  (#4829, engine) and **T11** (#4830, the Rules section) -- stated on
  the epic and on both task issues.
- **Follow-ups noted, not built here:** multi-step journeys and drips (a
  rule is one trigger to one email, deliberately); delete-triggered
  rules; per-rule rate limiting beyond the circuit breaker; a digest
  mode that batches N firings into one message; rules that write rows
  rather than send mail (the generator emits a send, and widening it is
  a different feature with a different blast radius).

## Why

An operator wants "when a new admin user is added, email the owner", and
wants to say it in a form rather than in a pull request. Everything the
sentence needs already exists in the tree and none of it is joined up:
the runtime authoring pipeline can compile and arm a user-authored
automation, the transactional outbox delivers operational mail, and SP1
gave the campaign machinery a per-recipient send with suppression,
identity resolution and a ledger. What is missing is the FORM that
produces one, the GENERATOR that turns the form into a real construct,
and the single wire that makes activation take effect while the operator
is still looking at the screen.

The two hard parts are not the UI. The first is that an automation's
`@trigger` names ONE concept at load time, so "any concept the operator
picks" cannot be served by anything pre-shipped. The second is that an
authored automation runs under an envelope that is neither the row
owner's nor the system actor's, and every read from inside one is
scoped by that envelope -- which is why the two most obvious ways to
resolve recipients both return nothing while looking entirely correct.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| E1 | Form vs mechanism | `v1:campaigns:emailRule` is the FORM: what the app lists, edits, pauses and explains. What RUNS is a real authored automation construct generated from it. The row records `bundleId`, `constructName`, `lastError`, `lastFiredAt` and `firedCount` so the two never have to be reconciled by a human reading logs. Composite tier, like every other operator-facing campaigns concept |
| E2 | Why a generated construct | An automation's `@trigger` names one concept **at load time**. There is no wildcard trigger and there should not be one -- a construct that subscribed to every concept in the cluster would fire on `lastSeenAt` churn on `v1:identity:user` and on every telemetry row. A shipped automation plus a lookup table therefore cannot express the feature at all for any concept nobody thought of at release time. Generating a construct also inherits the governance apparatus for free: per-rule pause, the per-automation circuit breaker, the cluster kill switch `authoredAutomationsEnabled`, boot re-arm, and the author-scoped actor. None of it is re-implemented here |
| E3 | Deterministic generator, LLM off | The generator is a template over the rule's fields producing `.memql` TEXT. The `authoringEmit` LLM path stays off. Two reasons and the second decides it: a rule that mails strangers is not a place for a model to improvise, and a deterministic generator is one whose OUTPUT A PERSON CAN READ -- the app shows the generated construct, and "why did this fire" is answered by reading it rather than by re-prompting. A generator that produced a different construct for the same form would also make the retire-then-arm regeneration path (E7) unauditable |
| E4 | Activation is immediate | `ActivateApprovedBundle` has **no production caller anywhere in the tree** (verified below). Activation therefore takes effect only at the next boot, through `App.rearmActiveAuthoredBundles`. T10 wires the caller: `campaignActivateEmailRule` runs generate -> bundle -> validate -> activate and the construct is armed before the builtin returns. A feature whose "on" switch takes effect at the next restart is one an operator concludes is broken |
| E5 | Two lanes, chosen by WHO receives | `recipientMode=cluster_roles` is OPERATIONAL and rides `stageOutboundRequest`: egress allowlist applies, no unsubscribe footer, and the marketing suppression list is **neither consulted nor written**. `audience` and `row_address` are MARKETING and ride `campaignSendToRecipient`: suppression at the point of send, RFC 8058 pair attached, sender identity applied, outcome ledgered. The app asks about recipients and never presents a lane toggle -- the lane is a consequence of the answer, and an operator who could pick it could pick wrong |
| E6 | Operational mail never touches suppression | Stated separately from E5 because it is the property that would be lost first by a "simplification" that ran both lanes through one primitive. An unsubscribe from a newsletter must not silence a security alert, and an operational address must not land on a cluster-wide do-not-mail list that a marketing send then honours. The two lanes share a template concept and share nothing else |
| E7 | Edit supersedes, atomically | Editing an active rule generates a NEW bundle naming the old one as `supersedesBundleId`, and `ActivateApprovedBundle` retires the superseded version as step 5 of the SAME call -- after the new constructs are registered. A rule is therefore never armed twice, which is the only failure here that produces DUPLICATE MAIL rather than none, and never armed zero times either, which an explicit retire-then-arm sequence would risk if the second half failed. `constructName` derives deterministically from the rule id, so the registry's own `(owner, kind, name)` key makes the replacement rather than an accumulation |
| E8 | Recipient resolution for `cluster_roles` is GO, not DSL | The obvious generated body -- call `activeUsers(role: "admin")` and loop -- cannot work: `activeUsers` is `@serverOnly`, an authored run's origin is `OriginClient` (the zero value; nothing stamps it), and the function validator refuses. So the operational lane's roles-to-addresses step is a Go-backed builtin whose executor stamps internal origin in an allowlisted package, and the generated automation calls THAT. See E11 |
| E9 | Only `created` and `updated` fire a rule | A delete has no row left to read recipients or merge data from, so a delete-triggered rule could describe nothing about what was deleted. The enum is closed at two rather than carrying a third value that every code path would have to refuse |
| E10 | The condition is validated at generate time | `emailRule.condition` is the runtime STRING evaluator's grammar (the one surface where `!` is legal). The generator parses and validates it BEFORE the bundle is written, so a typo is a typed refusal on the rule row rather than a construct that fails to load and leaves the rule sitting at `active` with nothing armed |
| E11 | The author's envelope decides what a rule can read | The generated construct runs under `AuthorContext` -- the rule author's `userId`, role `writer`, no internal origin. Everything the rule touches must be readable from THAT envelope, and `campaignActivateEmailRule` verifies it at arm time rather than at fire time: the template, the audience and the sender identity are re-read under the caller's own actor during activation, and a rule naming one the author cannot read is refused with the reason on `lastError`. A rule that fires forever and mails nobody is the failure this removes |
| E12 | Governance is inherited, not rebuilt | Pause is the registry's `Pause`, not a second flag; the breaker is the shipped `AuthoredBreaker` with its `onTrip` auto-pause; the cluster kill switch is `authoredAutomationsEnabled` and halts every rule on every node. The rule row's `status` MIRRORS what the runtime did (a tripped breaker moves the row to `paused` with the breaker's reason on `lastError`) and never substitutes for it |

## A. What exists today, and the one thing that does not

The runtime authoring pipeline is complete and live-capable. Bundles and
constructs persist as `v1:authoring:*` rows; `MemQLEngine.ActivateApprovedBundle`
(`component/memql/authoring_activation_engine.go`) is the Gate-3 transition
that loads a `dryRunPassed` bundle, compiles it and registers it into the
`AuthoredRuntimeRegistry` plus the `AuthoredScheduler`;
`app/engine_authored.go`'s `wireAuthoredRuntime` stands the whole thing up
at boot, binds the scheduler's run hook to a live automations `Executor`,
binds the breaker's `onTrip` to `Deactivate` + registry `Pause`, binds the
global gate to `v1:identity:clusterSettings.authoredAutomationsEnabled`,
and then calls `rearmActiveAuthoredBundles` so every already-active bundle
fires again after a restart.

**The gap is that nothing in production ever calls the activation
function.** A grep over every git-tracked `.go` file finds exactly three
kinds of hit: the definition and its `WithStore` variant
(`authoring_activation_engine.go:83-100`), four test call sites
(`component/memql/authoring_activation_test.go`), and seven comments in
five files that describe it -- `component/grpc/authoring_handlers.go:23`
("this surface only validates + injects"),
`component/memql/authoring_promote_durable.go:14` and `:189`,
`component/memql/authoring_session.go:29`, `:250` and `:573`, and
`app/engine_authored.go:8` and `:213`. There is no handler, no builtin and
no automation step that reaches it. So today the ONLY path from "a bundle
became active" to "its automation fires" is the boot re-arm, which means a
freshly approved bundle does nothing until the next restart -- and does it
silently, because every row says `active` and every gate is green.

That is the single wire T10 adds, and it is why the activation builtin is
part of this phase rather than an infrastructure follow-up. The rest of
this spec would be a feature that works after `kubectl rollout restart`.

## B. The generator

`campaignActivateEmailRule(emailRuleId)` is the whole activation verb. It
runs, in order:

1. **Read the rule under the caller's actor.** The composite tier is the
   authorization: a caller who cannot read the rule cannot arm it, and the
   author recorded on the row -- not an argument -- becomes the bundle
   owner. This is the same "authorization is the first read" property the
   four campaign-scoped builtins already have.
2. **Resolve and validate the referents.** Template, audience (when
   `recipientMode=audience`) and sender identity are re-read under that
   same actor (E11). `recipientField` is validated to resolve on the
   trigger concept; `triggerConcept` is validated against the LIVE concepts
   registry rather than a hardcoded list, so a rule may name a concept a
   product bundle added after this release. `condition` is parsed by the
   runtime evaluator (E10).
3. **Render the construct.** A Go text template over the rule's fields
   produces one automation and, where the lane needs a loop or a
   conditional, one logic construct beside it. The construct's name is
   derived from the rule id, so regeneration replaces.
4. **Retire the previous bundle**, when `bundleId` is non-empty (E7).
5. **Stage, validate and activate** through the ordinary pipeline, ending
   in the newly-wired `ActivateApprovedBundle` call.
6. **Stamp the outcome on the rule row.** `bundleId`, `constructName`, and
   `status=active`; or `status=failed` with the engine's own sentence on
   `lastError`. The app renders that sentence verbatim, because a
   paraphrase of a refusal the app does not understand is worse than the
   refusal.

The generated shape for the operational lane is the one the fleet pack
already proves. `deploy/fleet/dsl/fleet/automations.memql`'s
`welcomeOnInstanceRunning` is the worked example: a `node.updated` trigger
on one concept, an args block naming the fields it reads, a `switch` on the
discriminating field, and a `stageOutboundRequest` whose `requestId` is
`concat`-derived so a re-fire on the same row state is idempotent rather
than a second email. `trial.memql`'s `trialNudgeDay7` is the same shape
with a cron trigger and a `forEach`.

**Idempotency of the generated send is a generator decision, and it is the
one place a rule can produce duplicate mail.** `stageOutboundRequest` is
idempotent by `requestId` and `@createOnly("status", "attempts")` means a
re-stage preserves worker-owned delivery state rather than resending. The
fleet's dunning automation records the trap in as many words: a key that is
constant per subject means the SECOND occurrence, months later, lands on a
row already marked `sent` and no mail goes out; a key that changes on every
firing means an at-least-once event stream mails twice. The generator keys
on `(ruleId, triggering row id, the event's version timestamp)` -- one
message per rule per row-version, which is what "when this happened, tell
someone" means. The marketing lane needs no such construction: its ledger
row is per `(campaign-or-rule, recipient)` and the absence of the row is
the work queue, exactly as for a campaign send.

## C. The two lanes, and the wall between them

| | Operational (`cluster_roles`) | Marketing (`audience`, `row_address`) |
|---|---|---|
| Primitive | `stageOutboundRequest` -> the outbound worker | `campaignSendToRecipient` |
| Who receives | this cluster's own people, by role | audience members, or an address on the triggering row |
| Egress control | `MEMQL_OUTBOUND_EMAIL_ALLOWLIST` | the cluster suppression list, at the point of send |
| Unsubscribe | none | RFC 8058 pair + footer |
| Sender identity | the deployment's configured transactional sender | the rule's `senderIdentityId`, else the env default |
| Outcome record | `v1:platform:outboundRequest.status` | `v1:campaigns:delivery`, stamped with `emailRuleId` |
| Suppression list | never read, never written | read before anything leaves |

The split mirrors the engine's own campaigns-versus-outbox division, which
[campaign-sending.md](../../public/operate/campaign-sending.md) already
states from the other side: campaigns do not use the transactional outbox
because the campaign worker has to see the provider's 429, because a
terminal per-recipient outcome is the point of the delivery row, and
because `MEMQL_OUTBOUND_EMAIL_ALLOWLIST` is a domain allowlist that a
marketing audience would either fail or force wide open. Read in the other
direction, the same three facts say operational mail must NOT ride the
campaign machinery: a colleague's address is exactly the case the egress
allowlist exists to protect, and an operational alert must not be
suppressible by a marketing unsubscribe (E6).

`campaignSendToRecipient` is deliberately not a `sendEmail` builtin. It
takes a template id and a recipient id, not a subject and a body. The
absence of a free-form DSL send is a standing decision recorded in
`dsl/identity/automations.memql` -- the identity domain's deletion
reminders publish an event for Go to pick up rather than composing mail --
and this feature does not reverse it. The two lanes are the two sanctioned
shapes.

## D. The actor trap

This is the failure mode the phase is most likely to ship, so it gets its
own section rather than a bullet.

An authored automation runs under `AuthorContext`
(`component/automations/authored_scheduler.go`), which stamps the author's
`userId` and `auth.RoleWriter` into claims, token info and the
`AccessContext`. It is deliberately NOT the system actor the core scheduler
injects: `contextWithSystemActor` in `component/automations/executor.go`
applies ONLY WHEN NO AccessContext was inherited, precisely so an authored
run keeps the author's envelope and never picks up the engine-wide bypass.
The trap is documented twice already, in the two places somebody hit it:
`component/campaigns/schedule.go`'s header ("`which campaigns are due`
across every operator is a question no actor can ask") and
`deploy/fleet/dsl/fleet/billing.memql` ("an automation's actor is not the
row's owner and a caller-scoped read from inside one returns nothing while
looking entirely correct").

Three consequences bind this design, and the third is the one that is easy
to miss:

- **Owned rows are the author's, or invisible.** Template, audience,
  recipients and sender identity are composite-tier. Read from inside a
  rule authored by user A, they are A's rows or nothing. E11 turns that
  into an arm-time refusal with a sentence, rather than a fire-time silence.
- **Cluster-wide questions cannot be asked from inside a rule.** Neither
  the marketing suppression list nor the send-job queue is readable from a
  writer envelope. Both live behind Go that borrows the campaign owner's
  authority for owned reads and uses the engine's own operator identity for
  cluster-tier ones -- the "two identities" pattern the drain worker
  already implements. `campaignSendToRecipient` is inside that boundary,
  which is why the lane primitive is a builtin and not a mutation.
- **The origin is CLIENT, so no `@serverOnly` construct is reachable.**
  `auth.CallOrigin`'s zero value is `OriginClient` and nothing on the
  authored path stamps otherwise; the default is deliberately the
  inconvenient one. `dsl/identity/queries.memql`'s `activeUsers` -- the
  natural way to answer "which users hold role admin" -- is `@serverOnly`
  for exactly the reasons that file states, so the generated DSL cannot
  call it (E8). The roles-to-addresses step is therefore Go, in a package
  on the internal-origin allowlist, downstream of a gate a test can
  enumerate. SP1's hardening work brings the campaigns engine packages
  onto that allowlist -- `@serverOnly` on the six lifecycle writers plus
  internal-origin stamping at their Go call sites, applied INLINE as the
  argument to the one `Execute` that needs it rather than bound to a
  variable that flows on -- and this phase inherits that entry rather than
  adding a second one.

**Both lanes' send paths are verified reachable from that envelope, by
test rather than by argument.** The operational lane's write is reachable
because `v1:platform:outboundRequest` declares no `@rowAuthz` tier at all
and `stageOutboundRequest` is not `@serverOnly` -- true today, and worth
saying out loud precisely because it is the undeclared long tail rather
than a granted permission: if that concept ever declares a tier, this lane
breaks, and the test named in G is what will say so. The marketing lane's
write is reachable because the builtin's Go executor performs the
cluster-tier reads under the engine's operator identity and the owned reads
under the rule author's, resolved from the rule row the ARM-TIME caller had
already read under their own actor -- so a rule can only ever mail as a
user its author could already act as.

## E. Governance, and what an operator can stop

Nothing here is a new control surface; each row of the table is a control
the authored runtime already has, named so the app can render it.

| Control | Mechanism | Blast radius |
|---|---|---|
| Pause one rule | `campaignRetireEmailRule` on delete, registry `Pause` on the operator's stop button; `status=paused` on the row | one rule |
| A faulting rule stops itself | `AuthoredBreaker` trips after N consecutive failures; `onTrip` calls `Deactivate` + `Pause` | one rule, no cascade |
| Stop every rule on the cluster | `v1:identity:clusterSettings.authoredAutomationsEnabled` -> the scheduler's `GlobalGate` | every authored automation on every node |
| Bound what a rule may do | the authoring capability gate, keyed on the AUTHOR: the per-user kill switch and the standing scope grant | one author's rules |
| Survive a restart | `rearmActiveAuthoredBundles` at boot | every active bundle |

Two properties follow that the app must present honestly. A rule paused by
the breaker was paused BY THE SYSTEM, and the row's `lastError` carries the
run failure that tripped it -- rendering it as "you paused this" loses the
only diagnostic. And the cluster kill switch is not per-rule state: with it
off, every rule reads `active` and none fires, so the Rules section shows
the cluster-level state as a banner over the list rather than as a status
on each row.

## F. Error handling

- **Generation, validation and activation refusals are typed and land on
  `lastError`, and the rule goes to `failed`.** It does not stay `draft`:
  draft means never generated, and conflating the two hides the case where
  an operator pressed the button and nothing happened.
- **A fire-time failure is the breaker's business, not the row's.** The rule
  records `lastFiredAt` and `firedCount`; a failing run increments only that
  rule's breaker, and the trip is what changes the row.
- **A rule whose template was deleted out from under it fails at fire time,
  once per firing, until the breaker trips.** That is the correct
  behaviour -- the alternative is a rule that silently stops mailing -- and
  the app's rule detail shows the referent by id when it cannot resolve it,
  never blank.
- **The operational lane's allowlist refusal is the outbound worker's**, on
  the `outboundRequest` row, with its own status and `lastError`. The rules
  list links to it rather than copying it; two places to read one failure is
  two places to keep in sync.

## G. Testing

- **A generator golden-file suite.** Each `(recipientMode, eventKind,
  condition present/absent)` combination renders to a fixed `.memql` text
  compared byte-for-byte, and every rendered construct is then PARSED by
  the real parser. The campaigns suite drives a fake engine that records
  call STRINGS, which hides render bugs by construction -- the same trap
  SP1's D15 addresses -- so a golden file that is never parsed proves only
  that the template is stable.
- **An activation test that fails against today's tree.** Arm a rule, fire
  its trigger event WITHOUT restarting the process, and assert the send
  happened. It must fail before the `ActivateApprovedBundle` caller is
  wired and pass after; a test that only exercises the boot re-arm proves
  nothing about E4.
- **An envelope test per lane** (the D verification). Drive each lane's
  send path from a context built by `AuthorContext` -- not from a test
  context with a system actor -- and assert the mail is produced. The
  operational lane's test additionally asserts the origin is `OriginClient`
  at the call, so the day `outboundRequest` declares a tier or the mutation
  gains `@serverOnly`, this test is what says so rather than a silent
  production regression.
- **A suppression-isolation test.** Suppress an address cluster-wide, fire
  an operational rule at a user holding it, and assert the message is sent
  and no consent event is written (E6). The inverse -- a marketing rule at
  a suppressed address skips and ledgers the reason -- is the same test in
  the other lane.
- **A regeneration test.** Edit an active rule, assert exactly one
  construct is armed afterwards and the old bundle is retired; fire once
  and assert exactly one message (E7).
- **Idempotency:** re-deliver the same trigger event and assert one message
  on each lane, for the reason the fleet's dunning comment records.
- **Conformance and the fan-out:** the concept, queries, mutations and
  builtins classify under the authz gates and relationship targets
  resolve. `make test` (the module-path form) is the verification command;
  the db-gated lane covers the rule lifecycle and both send paths.

## H. Delivery

- **PR 8 -- T10 (#4829), engine:** the generator, the `ActivateApprovedBundle`
  production caller, the two lane paths behind
  `campaignSendToRecipient` and the operational roles resolver, and the
  three builtins' Go executors. The `emailRule` concept, its queries and
  mutations, and the builtin declarations landed ahead of this with the
  schema commit.
- **PR 9 -- T11 (#4830), OS:** the Rules section in the Campaigns app --
  list, the create/edit form (trigger concept from the live registry,
  event kind, condition, template, recipients, account tie, sender
  identity), arm / pause / retire, the generated construct shown read-only,
  and `lastError` rendered verbatim.

Each PR branches from and targets `main` -- never stacked, since a stacked
base gets no CI here -- and each leaves `make test` and the db-gated lane
green on its own.
