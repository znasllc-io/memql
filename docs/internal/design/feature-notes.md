---
title: Feature notes -- per-epic write-ups for the shipped feature areas
audience: internal
status: stable
area: design
sinceVersion: 0.22.8
owner: znas
---

# Feature notes

The per-epic record for each shipped feature area: what it is, and the rules
about it that the code does not say. Split out of the root `CLAUDE.md`, which
carries a one-paragraph index to this file.

---

## Views, layouts and living pages -- RETIRED (epic memql#4984)

The portal's arrangement system (`PageManifest`, `dsl/portalviews`, the `view`
concept, the compose / uiAssist prompts) is gone with the portal. **MemQL OS
has no replacement and is not getting one by default**: its answer is
hand-built sections under the twelve interface rules in `clients/os/DESIGN.md`,
and reaching for a runtime arrangement engine there is a design decision.
`sdk/ts-viewkit` still ships because the VS Code extension consumes its
vnode / render / styles core; its arrangement layers have no caller, which is
a fact about the tree rather than a licence to delete them. The retirement
record, `docs/superpowers/specs/2026-09-06-portal-removal-design.md`, says
where every portal capability went.

## Nexus -- being rebuilt on MemQL OS (sub-project B)

Nexus is the living map of a goal: a run and its world, drawn as the system
works and replayable from the rows' own timestamps. The portal's version was
DELETED (epic A1, D7); what survived is the pure scene library
`clients/os/src/nexus/scene/` (functions over rows, tested on fixtures with no
GPU). Sub-project B re-points `concepts.ts` at `v1:work:run` / `v1:work:step`
and draws it in 2D -- **the OS carries no WebGL** (epic memql#4785, owner
requirement), so the Deployables app's 2D map is the shape to adapt. Design
record: `docs/superpowers/specs/2026-09-05-work-spine-design.md`.

## Invitations (Identity Primitive)

Token-hashed invitation credential, under `v1:identity:invitation` -- the USER
invitation. The guest flow went with the space concept it scoped to (epic
memql#4988); `kind` keeps its `guest` value for rows already written and
nothing produces one. Key files: `dsl/identity/{concepts,queries,shapes}.memql`
and `integrations/email/` (self-registering plug-in exposing
`integration.email.sendEmail` -- GraphSender via Microsoft Graph `sendMail`
preferred, SMTPSender fallback, LogSender for dev; env `AZURE_TENANT_ID` /
`AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` / `MAIL_SENDER` / `MAIL_FROM_NAME`,
or the `SMTP_*` family, or neither).

**"or neither" is a LOCAL-ONLY option (memql#4477).** `LogSender` returned `nil`,
so mail failed UPWARD: the wizard said the link was sent and the audit row
recorded success. Log-only is decided from `MEMQL_DOMAIN`: unset, a loopback
literal, `*.localhost` or `*.local.<domain>` keeps it; anything else REFUSES
BOOT, naming the four Graph vars and the SMTP pair. Break-glass:
`MEMQL_EMAIL_ALLOW_LOG_ONLY=true`. Elsewhere a `LogSender` returns a permanent
`SendError`, the audit row records `outcome=failure` (the ROW and the HTTP
response stay identical, so a response cannot enumerate registered addresses)
and the console shows `unhealthy`.

**`Mail.Send` (Application) is tenant-wide until it is scoped** (memql#4478):
narrowing it to the one sender mailbox needs an Exchange
`ApplicationAccessPolicy` (Exchange Online PowerShell, not reachable from
`az`). Any automation that adds a secret must pass `az ad app credential reset
--append`; without it the command DELETES every existing secret on the
registration.
[azure-entry-install.md](../../public/operate/azure-entry-install.md#mailsend-is-tenant-wide-until-you-scope-it).

## Email campaigns + the sending engine

Campaigns are ordinary graph state (memql#3323) plus a Go sending engine
(memql#3348). **Thirteen** concepts under `dsl/campaigns/`: nine
operator-facing on the COMPOSITE tier `@rowAuthz(owner="ownerUserId",
clusterOwner)` (`audience`, `recipient`, `template`, `senderIdentity`,
`campaign`, `delivery`, `consentEvent`, `engagementEvent`, `emailRule`), four
engine-owned and clusterOwner-tier (`sendJob`, `suppression`,
`reputationWindow`, `warmupState`). Runbook:
[campaign-sending.md](../../public/operate/campaign-sending.md).

**The composite tier is oversight, not sharing.** A cluster owner READS every
operator's campaigns and drives the builtins on them (their gate is "can the
caller read the campaign row") and CANNOT rewrite the rows -- the write guard
ignores the second argument (memql#4312). A plain owner tier has no
cluster-owner escape on the READ path, so a fleet-wide campaigns view would
silently render one person's subset.

**The account tie is a record, never a visibility scope** (accounts D1).
`accountId` + a `forAccount` relationship on `campaign` / `audience` /
`template` / `senderIdentity` / `emailRule`; recipients inherit through their
audience; `campaignsForAccount` is the rollup. **No query narrows a read
because of it.** Requiring one at create is APP behaviour, not schema.

**A `senderIdentity` is a mailbox declaration with NO secret material.**
Authentication stays the cluster's one Graph credential. Resolution is
`campaign.senderIdentityId` -> else the env default, and **the engine never
infers an identity from `accountId`** -- prefill is UX, resolution is
explicit. A missing or `disabled` identity is REFUSED, never defaulted: a
silent fallback mails a client's list from the wrong mailbox under the wrong
SPF/DKIM and the send looks successful.

**The two identities is the design.** The engine BORROWS the owner's authority
rather than out-ranking it: `component/campaigns`' drain worker runs its
clusterOwner-tier reads (job queue, suppression list) under the engine's own
operator identity and everything owned under
`auth.ContextWithUserActor(ctx, job.campaignOwnerUserId)` -- a value copied
off a campaign row the STARTING CALLER had already read under their own actor.

- *Suppression is CLUSTER-WIDE and digest-keyed* -- one deployment, one sending
  mailbox, one reputation. The row id is the SHA-256 of the normalized address
  and the only readable field is the domain. Enforced at the POINT OF SEND,
  before the recipient row's own status.
- *A hard bounce suppresses; it does NOT delete the membership.* Deleting
  destroys the audit trail and lets the next import resurrect a dead address.
- *Idempotency is the ledger.* One `v1:campaigns:delivery` row per (campaign,
  recipient) at a derived id; the batch is "roster minus ledger, plus retries
  that are due". The absence of the row IS the work queue.
- *Two rate limits.* Ours is a per-process token bucket
  (`MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE`); theirs is the 429, surfaced as a
  typed `email.SendError` and honoured by parking the job until its
  `Retry-After`.

**RFC 8058 one-click** rides two headers, which forced the Graph sender onto its
base64-MIME form (Graph's structured payload only carries `x-` headers).

**The unsubscribe token names its key**
(`u2.<keyId>.<owner>.<recipient>.<campaign>.<tag>`, memql#3458), verified
against a ring of two: `MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` signs,
`..._SECRET_PREVIOUS` only verifies. The key id is a truncated HMAC **of the
key**, not a slot. `_PREVIOUS` is a permanent second reader key, NOT a
migration window: unsubscribe links never expire, so **the window is counted
in rotations, not days** -- rotate at most once short of key compromise.

**Open/click tracking rides the SAME key ring under a different context
string** (see the HTTP exceptions table for `GET /t/o` and `/t/c`), so neither
token verifies as the other. Hits land on `v1:campaigns:engagementEvent`,
unique by DELIVERY rather than by address.

**Two figures `campaignStats` refuses to invent.** A unique open/click count
folded from a bounded read that came back AT its bound reports `unmeasured`
rather than a floor dressed as a total; there is no per-campaign soft-bounce
figure at all, because nothing measures one. An absent figure and a zero are
different answers.

**An event-email rule is a FORM; a generated authored automation is the
MECHANISM** (memql#4829). `campaignActivateEmailRule` renders a construct
DETERMINISTICALLY from a `v1:campaigns:emailRule` row (the LLM `authoringEmit`
path stays off) and arms it through the runtime authoring pipeline, because an
automation's `@trigger` names ONE concept at load time. **Two lanes, chosen by
who receives** -- cluster roles ride `stageOutboundRequest` (allowlist, no
unsubscribe, suppression neither consulted nor written); audience and
row-address recipients ride `campaignSendToRecipient`. **The actor trap
applies:** the generated construct runs under `AuthorContext` (author's
userId, role writer, origin CLIENT), so owned rows are the author's or
invisible, no `@serverOnly` construct is reachable, and cluster-wide
questions must be asked from Go.

The scheduler (`campaignScheduleSend`), the evidence-driven warming ramp, and
bounce/complaint feedback ingestion are all built -- the runbook is current.
On Graph, nothing dials us: the mailbox reader
(`MEMQL_EMAIL_NDR_POLL_SECONDS`) stages DSNs from the sending mailbox, and
they are acted on only once `graph-mailbox=rfc3464` is in
`MEMQL_CAMPAIGNS_FEEDBACK_SOURCES`. The multi-account campaigns program is
specced in
[2026-09-01-email-campaigns-program-design.md](../../superpowers/specs/2026-09-01-email-campaigns-program-design.md),
with a record per sub-project:
[the campaigns backend](../../superpowers/specs/2026-09-01-campaigns-backend-design.md),
[integration config](../../superpowers/specs/2026-09-01-integration-config-design.md),
[the Campaigns OS app](../../superpowers/specs/2026-09-01-campaigns-os-app-design.md),
[event emails](../../superpowers/specs/2026-09-01-event-emails-design.md).

## The work spine -- goals, runs, steps (epic memql#4966)

A person gives a goal; the system works it out once, records the steps as
atomic variable-taking units, and from then on replays them without a model
unless reasoning is genuinely needed. `v1:work:{goal,run,step,modelCall,
approval,observation}` is that spine, every concept on the composite owner
tier. Design record:
[the work spine](../../superpowers/specs/2026-09-05-work-spine-design.md).

**The layer split is what makes the claims checkable.** `component/work` is a
leaf module of PURE decisions -- the compile order, the symptom rules table,
the derived step kind, postcondition derivation, the footprint union, the run
ceilings, the approval builders, the three replay modes. No engine, no
provider, no database. So the two headline claims are properties of a
function over values rather than counts against a mock: an exact catalog hit
returns `NeedsModel=false`, and a rules-classified symptom returns without
reaching the classifier prompt. `integrations/work` is the wiring; only it
touches rows.

- **Compile runs cheapest-first, and the order IS the design.** Catalog exact
  match on `GoalSignature` (the normalized statement plus the SORTED input arg
  names), then near match at 0.82 with a gap list, then ONE triage call that
  answers complexity and sectionability together. An exact hit reaches no model
  at all -- not even triage. `GoalSignature` is a NEW key, not `CatalogKey`,
  which hashes construct SOURCE and refuses `automation` outright.
- **Rules classify before a model does.** `ClassifyByRules` returning
  `ok=false` is the only path that costs a call. Order inside the table is
  load-bearing: the stalled-step rule sits ABOVE the transient matchers,
  because a repeated action that also looks transient must escalate rather
  than retry forever.
- **Repair beats resample.** A contract miss re-runs the failed step with the
  violation as guidance; a plan miss re-plans from that step with the prefix
  KEPT. Never from the start -- `replanGap` is shown the completed steps as
  fixed evidence and never re-emits them.
- **Never a silent edit (D5).** Healing proposes its four typed patches and a
  person approves them as a `planReview` approval.
- **One approval concept for every human gate (D6).** `v1:work:approval`
  replaces the plan's feedback fields, the canvas cards and the safety gate's
  `v1:safety:approvalRequest` sink. **The artifact hash is the guarantee**: an
  approval is a decision about one specific command, patch, message or draft,
  and resume compares the hash, so it can never carry to a modified artifact.
  The surface is the MemQL OS Work app -- without one, a human gate is
  invisible, which is exactly what the planner's canvas cards already were in
  an engine-only cluster.
- **The three-counter split is inherited from the plan row and means the same
  thing.** Dollar ceilings (`tokenBudget`, `costCeiling`) EXCLUDE subscription
  and local spend; loop caps (`maxModelCalls`, `maxRetries`, `maxEvents`,
  `wallClockMs`) INCLUDE every call. A ceiling of zero is UNSET, never
  "nothing allowed".
- **The ceilings are enforced at the MODEL SEAM, and that placement answers
  three questions at once** (memql#5580). `component/memql`'s `modelSeam` is
  the one funnel both covered call sites pass through, it reads the run off
  the context, and it sits BEFORE the provider call -- so the call can still
  be refused. It asks a `RunCeilingGuard` (implemented by
  `integrations/work.RunCeilings`, which reads the goal's ceilings and folds
  the run's `v1:work:modelCall` rows into its spend) and refuses with a typed
  `*memql.RunCeilingError`. The three:
  - **A breach PARKS, it does not fail.** The refused call fails its step, and
    `closeRun` recognises the typed error and parks the run at `waiting` on a
    `budget` approval carrying `{ceiling, limit, actual, reason}` -- plus the
    ceiling's name and the run's `spent` on the row. **No `resumeAt`, ever**:
    only a person changes a ceiling. Nothing in flight is cancelled, because
    the check runs before the provider is asked.
  - **Every ANSWER is one model call, whoever answered** -- a provider, a
    fleet machine, a subscription app, the replay journal, or the ai()
    runtime's in-process caches (which answer ABOVE the seam, so `Invoke`
    asks the guard before consulting them). The dollar buckets take only what
    MemQL was BILLED for, so a replayed or cached answer costs a loop call and
    nothing else. `component/work.AddCall` is that fold.
  - **A resumed run continues its first attempt's budget.** The guard seeds
    from the run's own journal rows, so park-approve-park is not a way to buy
    another allowance; a replay or a fork is a different run id and rightly
    gets its own.

  Two edges, stated because silence about them would be the same defect one
  layer down. **`maxRetries` and `maxEvents` are the EXECUTOR's counters and
  reach no model call**, so this seam clears them and WARNS rather than
  clearing them silently against a spend it never sees; and a run whose
  ceilings **cannot be read** is REFUSED under the sentinel ceiling
  `unevaluated` -- a limit nobody could read must not read like a limit nobody
  set. A run with no `goalId` inherits no ceilings and costs the guard no read
  at all, which is what keeps it off every automation run in the cluster.
- **Both sweeps are in `maintenanceAutomations`, and that is not optional.**
  The work concepts declare the composite owner tier, so under the default
  reader actor these reads answer ZERO ROWS AND NO ERROR: a sweep that resumes
  nothing is indistinguishable from a cluster with nothing parked.
- **`v1:planner:plan`, `task` and `taskState` ARE RETIRED** (epic memql#5000).
  The spine is the only model: `agent()` and `produceArtifact` open goals, a
  tool call is a `v1:work:observation`, training is a goal.
  `v1:planner:responsibility` STAYS -- the reactive loop reads it, and its
  mesh routing rules were NARROWED rather than deleted, because deleting them
  takes the responsibility intake dark cross-replica, silently.
  `component/planner` keeps exactly two things: the container-executor seam
  and the delegation resolver.

## The proving suite -- what the platform measures about itself (epic memql#4993)

A corpus of scenarios and benchmarks answering "does this work, and how well",
measured as **the platform against the same model in a bare tool loop** with
its machinery switched off -- no catalog, no journal, no replay, no classifier,
no healing. No other framework is named or measured. Design record:
[the proving suite](../../superpowers/specs/2026-09-06-proving-suite-design.md).

**The numbers must be honest before they are good, and that is a TYPE rather
than a policy.** `component/proving/figure` carries either a `Stat` or an
`AbsentReason`, never both and never neither; a `Stat` has `N`, median, spread
and **no `Mean` field and no single-number constructor**; and `Render` refuses
a figure whose provenance is incomplete for its tier. So `unmeasured` is a
value distinct from zero all the way to the pixel, and "medians and spread,
never a best case" is unrepresentable rather than merely discouraged.

- **Two tiers, and what each may publish is a DECISION** (P1). A CI replay is
  deterministic by construction, so pass-rate variance is trivially perfect and
  wall-clock belongs to the runner: both read `notMeasurableOnReplay` until the
  live tier fills them. The live lane ships **DISARMED** (P3) --
  `proving-live.yml` is `workflow_dispatch` with no schedule -- and the
  scorecard's tier table says so rather than leaving empty columns unexplained.
- **An absent figure names its own reason, and the reason CHANGES rather than
  the number appearing.** `governance.modelCallsJournaled` is absent as
  `notMeasurableOnReplay`: the CI tier plays model responses from a cassette
  through a fake step registry, so no call reaches the journal seam. Reporting
  "zero provider calls" when nothing in the path calls a provider is a lie that
  reads exactly like the headline result, and so is 1.0 for a ratio whose
  denominator is zero. `Render` prints the figure's DETAIL when it has one.
- **Every zero-claim is paired with a NEGATIVE CONTROL that must produce a
  non-zero**, and a control reading zero FAILS the suite. Usually the control
  is the same scenario's baseline arm -- a bare loop with no journal restarts
  from the beginning, so it re-executes and re-delivers. A counter that never
  rises on any path reads as zero forever.
- **The CI gate blocks on STRUCTURAL properties and reports on cost and speed**
  (P2). A scenario that stops passing, a duplicated side effect, a failed
  governance property, a dead control, or **a metric that stopped being
  measured at all** fail the `proving` lane. Cost and speed never do: a
  threshold that reds the lane for runner noise gets widened until it means
  nothing, and the structural half dies with it.
- **`cmd/memql-bench` adopts the capability-script contract**, gated by
  `component/proving/capability` (whose test reads the printf formats out of
  `scripts/lib/capability.sh`) rather than by `capability_contract_test.go`,
  whose walk is `scripts/**/*.sh`.
- **A claim may not outlive its number** (P4). A published numeric claim in
  README or `docs/public` carries a `<!-- proving: metric=... value=... -->`
  marker; `TestPublishedClaimsRestOnAScorecardNumber` fails the build when the
  committed scorecard does not carry it, reports it unmeasured, or measures it
  differently. `<!-- proving-pending: ... -->` is the OPPOSITE marker for the
  "does NOT make yet" table, and the mirror gate fails when one of those has
  quietly become true. NEGATIVE CONTROLS are excluded from claim checking --
  without that, every zero-claim is unclaimable.
- **The pure sub-packages are a BUILD-GRAPH fact.** `figure`, `scenario`,
  `scorecard`, `capability`, `world` and `cassette` import nothing outside the
  standard library and each other, asserted by reading `go list -deps`.
  `component/proving` is deliberately DATABASE-FREE; the database-touching
  verification is the binary running end to end in the `proving` job.
- **The journal's per-step cost is MILLISECONDS, not a ratio.** A ratio over a
  trivial-step scenario has a degenerate denominator and measures something
  other than its name. The right instrument is the same automation run twice in
  one process with `Engine` set and nil -- `newWorkJournal` returns nil for a
  nil executor, so the only difference is whether journal rows are written.
- Rows: `v1:bench:run` and `v1:bench:sample`,
  `@rowAuthz(clusterOwner, rankFloor="admin")`, broadcast, every mutation
  `@serverOnly` -- a client-reachable write here is a primitive for forging the
  numbers the README rests on. Surfaced at MemQL OS Settings -> Benchmarks
  (`{ min: "admin" }`), where an absence takes the same room as a number and an
  unmeasured run draws an OPEN NOTCH rather than a bar of height zero.
  **`rankFloor=` relaxes the READ and leaves the WRITE at clusterOwner**
  (memql#5216); without it a non-owner admin was admitted to the screen and
  served nothing, and the refusal rendered as UNMEASURED. The four reads carry
  `@requiresRank("admin")` to match, adjudicated in `tierDecidesTheRead`.

## Planner / Knowledge / Validation

The schema is stable, so new features add fields/automations without migrations.
Fields are in the `.memql` files; the concepts are `v1:agents:agentAuthorization`
(standing tiered-trust authorization), the `v1:knowledge:*` family (`document`,
`spreadsheetRow` / `imageRegion`, the append-only `validationEvent`, and
`domainEntitySchema` / `entityIndex` for cross-file dedup), plus
`v1:common:knowledgeDomain` and `v1:common:documentChunk`.

**Every automation execution is journaled** (epic memql#4962): the executor
opens a `v1:work:run` row before the first step and writes a `v1:work:step`
row at `running` before each body and again at `done` / `failed` / `skipped`
after, under a synthetic cluster actor (`component/automations/journal.go`);
resume reads those rows back on the SAME run id. A step at `running` with no
receipt is a crash mid-step and resumes from there. A sandboxed dry-run holds
no journal at all, so a preview leaves nothing resumable.

**Analysis path.** A file's analysis is a system-origin work GOAL running the
deterministic template (extract, chunk, embed, summarize), so the Training
app's live feed is `v1:work:run` rows keyed by file id.

**The planner agent loop and the Plan rows are GONE** (memql#5000, memql#5052).
What is left of `integrations/planner` is the compile pass, the healing
subscriber, the responsibility intake, the reactive loop and the authoring
pipeline. The cost-safety layers the loop carried each moved or say where they
went -- a retirement that quietly drops a ceiling is the failure
[llm-cost-control.md](../../public/ai/llm-cost-control.md) exists to prevent:
the process-wide rate ceiling and identical-request circuit breaker are
UNTOUCHED at the provider chokepoint (`component/memql/ai_guard.go`); the
per-Plan budget is REPLACED by `component/work/budget.go` reading the RUN's
ceilings (dollar ceiling excludes subscription and local spend, loop caps
include every call, asserted by
`TestCheckCeilings_TokenBudgetExcludesSubscriptionAndLocal`); complexity triage
SURVIVES as the compile order's third tier; the specialist-creation gate is
RECORDED in memql#5063. `produceArtifact` opens a goal naming a deterministic
template, which reaches no model to decide anything.

**That replacement was written and NOT WIRED for a while, which is worth
knowing about the sentence above** (memql#5580). `CheckCeilings` had the split,
had the unit test cited here, and had NO PRODUCTION CALLER -- so a run's
ceilings were settable fields nothing read, and the only real stop was the
process-wide guard, which is per-PROCESS and shared with every other caller on
the node. It is wired now, at the model seam -- the work spine section above
describes the seam, the park and the three counting rules. **The test
named above does not hold that property and never did**: it is about the
function's arithmetic and passes whether or not anything calls it. The one that
holds it is `TestTheRunCeilingChainHasAProductionCaller`
(`run_ceilings_have_a_caller_test.go`), which fails when any link of decision /
wiring / seam goes test-only.

## Workers (computer_use_headless / computer_use_embodied)

Agents drive the user's own machine: shell exec, filesystem, HTTP fetch, and
(under the computer-use build) mouse + keyboard + screenshot. Runbook:
[workers-runbook.md](../../public/operate/workers-runbook.md). The sandboxed
first-choice surface for headless work is the Workbench, which stayed in the
root `CLAUDE.md` -- computer use is its FALLBACK, for what a sandboxed
workspace cannot do.

- **Capabilities:** `computer_use_headless` -> `workerHost`;
  `computer_use_embodied` -> `workerComputer`; both plus the cross-cutting trio
  (`workerStatus`, `requestComputerUseScope`, `canvasPublish`). Two slugs so the
  slices grant independently; authorization stays one decision, because both act
  on the user's machine. Expansion map: `component/memql/worker_caps.go`.
- **Gateway:** `WorkerService.Stream` on the agent node, authenticated by
  `mql_wkr_<43>` tokens (the `worker_token` variant on `v1:identity:identity`),
  which the interceptor admits on that path only. Mint/revoke server-side via
  `CreateWorkerTokenMsg` / `RevokeWorkerTokenMsg`; the plain token comes back
  ONCE and only the SHA-256 hash persists (`component/identity/workertoken/`).
- **Worker side:** `memql worker run` / `memql worker setup`, run modes of the
  `memql` command built by `memql-cockpit`. Install:
  `scripts/install/install-{mac,linux}.sh`.
- **Per-user routing, and NO machine id.** Every worker is owned by one
  `v1:identity:user`. The dispatch builtins take `requireLabels` /
  `preferLabels` and **no `workerId`** (D4): an agent says what the work NEEDS,
  the owner's policy decides where it lands -- so **a hallucinated machine id is
  a failure mode this surface does not have**. `no_worker_available` names every
  machine and why it was ruled out.
- **Permission model:** three layers checked BEFORE dispatch -- the agent
  capability flag, standing scope on
  `v1:agents:agentAuthorization.computerUseScope` (observe / full; `interact` is
  RETIRED, kept in the enum for old rows and read as `full`), and the per-user
  kill switch `v1:identity:user.preferences.computerUseEnabled`. Out-of-scope
  calls park the Plan at `awaitingFeedback` with
  `feedbackReason=scope_elevation_required`. Consent is decided BEFORE routing,
  so routing only ever chooses among machines already consented to.
- **The router** (`integrations/agent/worker/router.go`, epic memql#4349):
  `v1:worker:routingPolicy`, one active row per user and ABSENT for most (the
  router then applies `firstFit` + `nextMatching`, the pre-router behaviour).
  Four strategies -- `firstFit`, `roundRobin`, `leastLoaded`, `labelMatch` --
  all STABLE sorts over REGISTRATION order taken from the row rather than
  connection order, so every replica agrees with no shared state.
  `fallback=nextMatching` re-picks ONLY after a refusal BEFORE start (D5); an
  exec that lost its stream may have run. Policy and agent requirements are
  AND-ed and a conflict is left UNSATISFIABLE rather than resolved toward either
  side. Labels match EXACTLY -- there is no "any value" form.
- **Two label maps, and they must not become one.** `labels` is reported by the
  cockpit and OVERWRITTEN from `Register` on every reconnect; `operatorLabels`
  is the owner's and no register/heartbeat path writes it.
  `refreshWorkerRegistration` enforces the split by NOT NAMING the field
  (`update{}` is a read-merge; "completing the field list" is what would break
  it, and `displayName` is absent for the same reason). Routing matches the
  MERGE, operator side winning.
- **`online` is DERIVED, never stored:** unrevoked AND `lastSeenAt` within
  `OnlineWindow` = 2 x `HeartbeatBatchInterval` = **30s**. Exactly two
  implementations, `component/worker.IsOnline` and
  `clients/os/src/apps/fleet/online.ts`, held together by
  `TestFleetOnlineWindowMatchesTheClients`. Deriving it from the in-memory registry
  is refused: that answers "connected to ME", and the fleet needs "connected to
  ANY replica". Beside it, `rttMs` / `rttAt` are the CLUSTER's own evidence
  of the return path (epic memql#5218, D11): the agent pings the machine
  `FirstPingDelay` after RegisterAck and every `PingInterval`, the Pong's
  round trip lands on the row at the next flush, and an ABSENT `rttAt` is
  "not measured" -- a cockpit predating the message -- never "slow".
- **Cross-node dispatch (memql#4352):** `connectedNodeId` names the replica
  holding the stream; any other replica forwards over `WorkerForward*`.
  `refused_before_start` is the re-pick predicate and the one wire field that
  must never be guessed. The receiver re-checks only what it alone can know --
  ownership against the verified `ForwardedAuthority` (never the envelope's
  owner field) and revocation.
- **Row tier + borrowed authority.** `v1:worker:registration` and
  `routingPolicy` declare `@rowAuthz(owner=..., clusterOwner)`, and the READ
  gate has no internal-origin bypass: an unstamped read returns ZERO ROWS, not
  an error. A worker authenticates as `worker:<id>`, so `component/worker`'s
  store runs every registration read and write under `auth.ContextWithUserActor`
  for the owner named by the token's identity row -- and must NOT stamp internal
  origin, which is why it is deliberately absent from `call_origin.go`'s
  allowlist.
- **Operator surface:** the Fleet app in MemQL OS -- the guided install
  (a page over the kit rail: mint, install, connect, checks; the registration
  MATCHED by the mint's identity, never counted; design record
  `docs/superpowers/specs/2026-09-08-cockpit-install-wizard-design.md`),
  rename (`displayName`), edit operator labels, remove (revoke plus the
  uninstall one-liner), edit the routing policy, read each call's `routing`
  record.
- **The worker stream reaches the agent by its service prefix** (memql#5224).
  `WorkerService` is served by the agent and by nothing else, so the front
  door carries `component/frontdoor.WorkerServicePath` -> `svc/agent:50051`
  above the bff catch-all in both overlays and on every account api host,
  gated by render tests; the agent Service carries the same h2c annotation
  the bff's does.
- **Audit + hardening:** security signals on `v1:identity:auditEvent`; per-call
  telemetry on `v1:worker:invocation` (`WORKER_INVOCATION_RETENTION_DAYS`
  default 90); per-call rlimits on Linux + Darwin via `policy.shell.max_*`;
  optional setuid drop via `policy.shell.run_as_user`; loopback-only metrics at
  `127.0.0.1:9100/metrics`.
