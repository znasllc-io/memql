---
title: Inspectable skills and goal evaluations
audience: internal
status: draft
area: planning
sinceVersion: 0.28.0
owner: znas
---

# Inspectable skills and goal evaluations

This program extends the existing skills, Training, work spine and proving
suite. It does not add a competing skill runtime. The goal harness stays
domain independent; software expertise belongs in reviewed skills and lessons.

## Delivery roadmap

| Milestone | Deliverable | Acceptance evidence | State |
| --- | --- | --- | --- |
| Catalog | Versioned goal scenarios, fixtures, oracle contracts, model matrix and result format under `test/proving/goals` | Loader rejects ambiguous versions, missing criteria and unsafe repository targets | In progress |
| Inspect | Training Skills and Evaluations lists with drilldown into content, provenance, exact versions and evidence | UI behavior and desktop/narrow light/dark inspection | Implemented locally; review and live verification pending |
| Bind | Multiple relevant skills and reviewed passages selected by installed DSL; immutable receipt on the owned work run | Exact text/version/score/reason survives a checkpoint and a replica hop | Skill snapshots tested across two engine instances; merged PR6070 supplies reviewed lessons through the existing Training path |
| Learn | Manual owner-scoped proposal from saved Ask guidance, review and evaluation-bound enablement | Immutable source and revision decisions; application and counterexample results; normal retrieval excludes candidates | Manual slice implemented; live candidate evaluation pending. Automatic mining deferred |
| Execute | Manual goal evaluation through MemQL, observed by independent artifact oracles | MemQL and the selected model perform the task; evaluator performs only fixture setup, declared injections and verification | Runner and five local case adapters pass workflow and cross-replica publication checks. Nine advanced adapters remain prerequisites |
| Baseline | First manually run model/scenario matrix | Complete raw result records including failures and interventions | Not run |
| Repeat | Configurable monthly product evaluation, comparison with prior compatible runs | Explicit model/budget/fixture authority; schedule disabled until owner enables it | Planned |
| Demonstrate | Documentation cases connecting capability claims to real goals and observable artifacts | Original request, full setup, actual bindings, acceptance evidence, human assistance and limits | Publishing format added; software case5817 remains unfinished |
| Archive | Bounded case exports across software, documents, files, research and short/long workflows | Results, proof, intervention history, coverage and verified ZIP manifest retained in MemQL | Protocol documented; initial assisted snapshot captured manually; automatic capture planned |
| Publish | Deployable public case library and compatible baseline comparisons | Reviewed exports, inspectable failures, artifact downloads, native CI and deployment receipt | Planned; no website built or deployed by this phase |

Milestones are slices of this request, not claims that later work is already
implemented. Update their state only when their acceptance evidence exists.
Remove this active plan when the program ships; keep the catalog and public
reference as the durable versioned record.

### Verification of the first implementation slice

The focused real-database checks pass in an isolated scratch database: the
four reviewed skill seeds materialize, client writes cannot impersonate a
baseline, concurrent selections share one receipt, and exact content survives
a second engine instance, catalog changes and replay-of-replay. Prompt and
checkpoint checks preserve the same selected versions. Training type checking
and five behavior tests pass; visual checks use explicitly synthetic records.

The whole-workspace test run exposed embedded inventory, automation corpus,
architecture model and integration hash-convention failures. The inventory,
automation and hash failures have been corrected and their targeted gates
pass; the regenerated model also passes its drift check. That full
run skipped database cases on its default connection, so only the separately
required-database checks above establish persistence coverage.

This slice is not a live benchmark: no model calls or paid-provider spend,
evaluation-repository effects, monthly executor or production deployment have
occurred. The first oracle checks pass locally, including false-success,
behavioral red/green, build-failure rejection and preservation controls.
The scoped workflow, no-paid-provider ceiling, strict DSL boot and publication
checks now pass. The publication check uses the isolated scratch database and
two engine instances. The live baseline still awaits release of the shared model.
The advanced adapters, automatic learning and baseline milestones remain open. The manual Learn slice is described below.

## Source of truth and scope

* `v1:skills:skill` holds named capability bundles, instructions and versions.
  Existing domains and `specializes` edges express areas and specialties.
* Reviewed release lessons use the Training seed manifest introduced by
  PR6070. Fresh installation is reproducible from exact reviewed bytes.
  That PR is a dependency, not permission to copy private learned material.
* Private manual lessons use the existing Training Review destination and
  Library retrieval, with immutable owned Work observations as the source.
  Existing taught documents retain their Library source and knowledge review. An official baseline requires a repository change
  and review. A successful run is evidence for a proposal, not a proof that its
  advice generalizes. Normal retrieval and learning do not change model weights.
* `v1:work:observation` holds owned selection and learning evidence. Saved
  continuations preserve task constraints, decisions, completed actions,
  approvals and selected versions. Retrieval cannot replace this durable state.
* The proving suite owns versioned goal evaluation contracts. Replay/cassette
  checks establish structural correctness only; they never count as completed
  live goals or a measured model baseline.

## Evaluation contract

The evaluator supplies a goal, a versioned isolated fixture and declared
constraints. It may prepare fixtures, inject a specified blocker, deliver a
specified approval or feedback, and read outcomes. It never supplies the patch,
regression, conflict resolution or review response that the scenario asks MemQL
to produce. Every other intervention is recorded and changes the outcome from
autonomous to assisted. Denial and safe parking can be correct outcomes where
the scenario expressly requires them.

GitHub scenarios require a dedicated evaluation repository with an explicit
allowlist and a marker bound to the fixture version. No arbitrary issue URL or
production/default product branch can qualify. Branch, issue and PR identifiers
belong to one evaluation run; cleanup removes only that run's artifacts after
evidence is saved. Local fixtures likewise live in a unique directory and must
preserve pre-existing and deliberately injected unrelated work.

Results bind the scenario and fixture versions/digests, engine and harness
commits, complete routing setup and actual model use, skill and lesson versions, each criterion's
verdict and evidence, root goal/run, PR/commit references, human interventions,
elapsed time, and separately actual, estimated or unavailable usage. A tool-loop
visible-text estimate must never be called total provider usage. Comparisons
require the same scenario/fixture contract and compatible measurement basis;
different model configurations are shown explicitly rather than pooled.

## Isolation and current verification boundary

Work uses `codex/training-evaluation` in its own managed checkout. The active
issue5817 acceptance browser, cluster, worker permissions, model routing and
Ollama are reserved by the parent chat. No production deployment is in scope.
Use bounded compilation and a uniquely named scratch database only; never
delete shared data, caches, containers or another chat's checkout.

The owner selected fleet:qwen3.8:27b for the initial reasoning source with no
paid-provider spend. Evaluate the complete MemQL routing setup, not one model:
record configured roles/levels/rules/ordered doors and actual models called,
including fallback sources, call counts and separate usage evidence. The matrix
compares named setups across goal/skill areas. Software development is the first
available suite; Document creation and Research are Coming soon. Capture the
remaining configured routes before the first run without changing the active
acceptance test’s settings.
Monthly scheduling is a MemQL capability, disabled until explicitly configured;
no Codex heartbeat is a substitute. A result row labelled Not run is honest
evidence of missing measurement, not a green baseline.

## Proof of work

The owner's additional request is documentation first: demonstrate claims
through inspectable real work, with no separate product runtime required.
[Proof of work](../../public/overview/proof-of-work.md) defines the publication
format and its evidence boundary. The active issue5817 acceptance task is a
candidate case, not a completed success or an autonomous benchmark. Its
coaching, approvals and missing regression/delivery evidence must remain
visible. Do not retrofit the new skill bindings onto a run that did not record
them, and do not treat its live product issue as an evaluation fixture.

### Case collection and public website plan

The owner expanded this into a growing suite across categories and planned goal
lengths, including PDF/document creation, other files, research and extended
workflows. Follow the [case record and ZIP protocol](../../public/overview/evaluation-case-records.md).
Keep private manually observed snapshots in owner storage outside the public
checkout. Only reviewed, authorized projections enter `test/proving/cases`; the existing
`test/proving/goals/results/index.json` remains the controlled runner's result
index. Do not insert an assisted production exercise into its unrun baseline.

Manually observed assisted exercises retain failed verdicts, external repairs
and incomplete coverage. They do not establish autonomous success. Exact
requests, private lineage and unreviewed receipts stay in owner storage.

Implement in these slices without changing the Training UI in this exercise:

1. **Document and capture:** retain immutable case snapshots, public guidance,
   observed receipts, external interventions and bounded ZIPs. Verify readability
   from MemQL and goal/run links separately from a successful upload.
2. **Automate export:** extend existing owned Work, Library and evaluation paths;
   discover root/child lineage and deduplicate receipts. Add category-independent
   collection with category-specific artifact checks. Preserve partial results
   on failures, permission waits and cancellation. Require actual served-model
   attribution, independent cost-known semantics and complete usage coverage
   before claiming totals. Export failure must remain visible and must not discard
   the only working copy of necessary evidence.
3. **Build the website:** generate a read-only site from reviewed exports. Provide
   filters, setup comparisons, case/trial timelines, readable criteria, artifact
   previews and ZIP downloads. Show unfinished and failed trials by default;
   publish no private journal access, credentials or raw internal instructions.
   Use the existing docs/site delivery pipeline where suitable; select the host
   and deployment target when implementation begins.
4. **Establish baselines:** run versioned fixtures with declared budgets and
   assistance policies across categories and lengths. Repeat compatible trials,
   report denominators/variance and surface regressions. Keep the current fleet
   matrix unchanged; an Apps matrix requires its own explicit setup and budget.
5. **Deliver and maintain:** run native CI on the exact site/export revision,
   verify archive checksums and artifact rendering, retain deployment provenance,
   and clean only temporary resources after evidence persistence. Scheduling
   remains disabled until explicitly configured.

Acceptance for the website includes a fresh export containing a success, failure,
unfinished case, missing-usage case and assisted case; working downloads with
checked hashes; incompatible comparisons visibly refused or separated; and
privacy review of the exact public export. A generated page alone does not prove
these behaviors. The case corpus informs reviewed lesson proposals and regression
tests; publication does not automatically enable a lesson or change model weights.

## Work-quality changes from the coaching exercise

The approved first phase extends the existing recovery checkpoint, optional
Training reader and provider observations. It does not implement the planned
Learn milestone, change the baseline model matrix or start its scheduled runs.
Worker range/search reads use the existing fs_read operation and directory
permission, with an advertised contract checked again on the stream-holding
replica. Cockpit and engine must both carry that contract.

Targeted implementation evaluations use the existing test and goal-fixture
infrastructure. They are not live model benchmarks:

| Case | Executable evidence | Negative/control case |
| --- | --- | --- |
| Repeated investigation | `TestReadEvidenceObservationSurvivesResumeWithoutChangingRecovery` | Changed query wording with the same result remains visible; observation cannot park the run |
| Legitimate exploration | `TestReadEvidenceDoesNotTreatWritesOrUnknownToolsAsReads` and existing progress-recovery tests | New source bytes/pages reset the repeated-result signal; unknown effects are not guessed |
| Named output and missing evidence | `TestWorkCheckpointPreservesNamedGapAndEvidenceWithinBudget` | Large background text cannot crowd out a fitting gap and source reference |
| Focused read and permission boundaries | Cockpit `TestBoundedFileInspectionBothPermissionLanes`, `TestFileInspectionDoesNotEscapeApprovedDirectory`; engine `TestExtendedReadContractRecheckedAcrossReplicaHop` | Old read implementation refuses the new requests; incomplete searches cannot establish absence; stale contract or symlink escape refuses |
| Lesson selection and delivery | `TestOwnedTrainingReadsReviewedTaughtSourcesAcrossReplicas`, `TestWorkDBApprovedTrainingReachesOrdinaryTurnOnAnotherReplica`, `TestTrainingReceiptReachesActualProviderObservationsInBothLoops` | Foreign/rejected sources withheld, oversized text omitted whole with a reason, actual attempt IDs remain separate from selection |
| Recovery and skill versions | Existing `TestSkillSelectionSurvivesReplicaHopAndCatalogChange` and goal-fixture oracle suite | Saved versions remain unchanged after a catalog update; false regression greens and lost unrelated work are rejected |

Software-development baseline version 2 adds explicit recovery-point coverage,
named uncertainty/next-deliverable guidance and conditional inspection advice.
Its behavioral benefit still needs the independently operated live baseline;
passing the checks above is not evidence of faster or more autonomous models.


### Manual Learn slice

A person can propose one candidate from a saved Ask guidance message. The
engine reads that message itself and binds its exact quote, conversation, turn,
run and delivery status. The person supplies the proposed rule, applicability,
exceptions, observed outcome, alternative, principle, separate confidence
statements and two declared scenarios. Duplicate requests return the original
receipt; the same feedback cannot silently create a second candidate.

Training's existing Review destination owns editing, review, rejection,
retirement and enablement. An edit creates a new content version and resets
review and eligibility. Decisions require the current receipt and share a
PostgreSQL gate across replicas. Immutable `v1:work:observation` events retain
history; no new skill catalog, learning service or scheduler is introduced.
Only the native writers can create or amend protected lesson, evaluator,
selection and provider-delivery evidence. Receipt access remains owner scoped.

A candidate can be tested through the existing `memql-bench` goal runner using
`--lesson-id` and `--lesson-receipt`, alongside its existing scenario and setup
arguments. Both flags must be supplied together. The owned goal explicitly
pins the current candidate and declared fixture/scenario; this fleet-only
path records an evaluation-only selection. It does not enable ordinary
retrieval. The publisher verifies the exact candidate version/digest against
a successful provider delivery receipt. The independent harness still owns
artifact verdicts; no delivery receipt proves that advice was followed.

Enablement requires manual review plus two successful saved evaluations for
that exact content version: the declared application and counterexample.
Autonomous versus assisted outcomes remain in those evaluations. Enabling
then indexes the lesson with the configured embedding route. Ordinary work
uses the existing bounded Training reader and selection receipts; a fresh
head check excludes retired, rejected, changed or unreviewed versions even
when an older vector remains indexed. An explicit indexing retry is available.
Neither enablement nor a successful delivery claims general behavioral benefit.

The first private proposal is **Identify the refused gate**: distinguish task
permission from worker policy after a refusal; use a permitted alternative or
identify the operator change needed. Optional diagnostics can be skipped when
unnecessary. It is an unpublished candidate with assisted source evidence,
not an approved lesson or an autonomous success. Its application scenario is
`tool-unavailable`, with `implement-and-check` as the counterexample. The
Training program owns the unavailable-tool adapter and live matrix. This
slice does not implement that adapter, start model runs, change routing or
publish the exercise ledger. Automatic mining remains deferred.

Targeted checks cover two-engine concurrent and stale decisions, cross-owner
refusal, immutable receipts, exact evaluation bindings, candidate-only test
selection, failed delivery versus successful fallback, and withholding an
older indexed version after retirement. These use an isolated PostgreSQL
instance with pgvector and deterministic provider fixtures; they are not a
live benchmark or TimescaleDB cluster certification. UI checks use synthetic
records, including failed-save draft preservation and narrow light/dark views.

### 22 October pricing and cross-category evidence slice

The isolated pricing branch adds API-equivalent quotes, private owner machine
energy settings, and the [cost estimate contract](../../public/operate/inference-cost-estimates.md).
It does not change Training, the active Ask runtime, or the fleet-only matrix.
Catalog 1.1.0 adds six planned scenarios through the existing catalog/fixture
loader; required specialized oracles remain unimplemented and unverified.
The new optional planned-length field preserves the original 14 fingerprints.

The reusable archive command now enforces explicit selection and verifies
bounded members, CRC, inventory and hashes. It requires attributed privacy
review for free text/media and performs no upload or publication. Native
Library retention/goal association, new live trials, exact-revision native CI,
and the public website remain separate outstanding acceptance evidence.
