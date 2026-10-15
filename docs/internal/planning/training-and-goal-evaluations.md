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
| Learn | Successful work, human guidance and research can propose owner-scoped lessons | Candidate never silently validates; source and verification receipts required for promotion; no private-to-official publication | Planned |
| Execute | Manual goal evaluation through MemQL, observed by independent artifact oracles | MemQL and the selected model perform the task; evaluator performs only fixture setup, declared injections and verification | Runner and five local case adapters pass workflow and cross-replica publication checks. Nine advanced adapters remain prerequisites |
| Baseline | First manually run model/scenario matrix | Complete raw result records including failures and interventions | Not run |
| Repeat | Configurable monthly product evaluation, comparison with prior compatible runs | Explicit model/budget/fixture authority; schedule disabled until owner enables it | Planned |
| Demonstrate | Documentation cases connecting capability claims to real goals and observable artifacts | Original request, full setup, actual bindings, acceptance evidence, human assistance and limits | Publishing format added; software case5817 remains unfinished |

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
The advanced adapters, learning and baseline milestones remain open.

## Source of truth and scope

* `v1:skills:skill` holds named capability bundles, instructions and versions.
  Existing domains and `specializes` edges express areas and specialties.
* Reviewed release lessons use the Training seed manifest introduced by
  PR6070. Fresh installation is reproducible from exact reviewed bytes.
  That PR is a dependency, not permission to copy private learned material.
* Private learned lessons stay attached to the owner's Library sources and
  existing knowledge review. An official baseline requires a repository change
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
