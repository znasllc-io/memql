---
title: Building a Spine
audience: public
status: stable
area: build
sinceVersion: 0.25.0
owner: znas
---

# Building a Spine

The **Spine** is the harness that takes a goal through planning and execution.
Its planning, execution-recipe and recovery policies are installed MemQL workflows. The native runtime owns
identity, budgets, distributed claims, the execution journal, cancellation,
approval integrity and safe recovery mechanics. A planner is one phase of the Spine; an agent
is a participant that a resulting automation can invoke.

The `work.spine/1` contract supports selection of compilation, draft assembly,
tool fallback, failure response, replan and guided repair recipes.
It does not replace the executor or turn an agent's token and tool loop into a
second DSL interpreter. It uses the same automation interpreter as other
MemQL workflows. See [Engine, integration and workflow boundaries](integration-boundary.md).

## Select a workflow

Install an enabled `@template` automation in your product DSL bundle, under
your own namespace and with its own name. Pass its name to `createGoal`:

```memql
use work.builtins.{ createGoal }

@template
automation companyInvoiceRequest {
  args { week string! }
  return builtin createGoal(
    statement: "Produce the weekly invoice summary",
    input: {week: args.week},
    spine: "companyCatalogSpine",
    ceilings: {maxModelCalls: 4, wallClockMs: 120000}
  )
}
```

Omitting `spine` selects `defaultWorkSpine`. Existing Ask and responsibility
intake use that default. A product can select its workflow from its own intake
wrapper. This release does not add a per-tenant default selector to Ask.
The selector is a name, never uploaded source, a Go handler name, or an
execution-authority object. Do not replace core namespaces to customize policy.
The Spine entry is called without arguments and must not require any; read the
goal and its input through `spineContext`. Child templates can declare arguments
supplied by their callers.

Admission resolves the complete reachable template, pure-logic and scoped-action closure and
checks every branch before opening the goal. It persists the source, entry,
native contract version and SHA-256 fingerprint in `v1:work:run.spine`.
The engine admits that field only from native writes and refuses replacement
or removal after admission, including through raw or authored mutations.
The receiving planner reconstructs the workflow exclusively from that row;
changing or removing an installed child cannot silently change accepted work.
Forks and branches inherit the snapshot. Direct agent and reviewed document
goals also pin the default recipes; delegation from a run owned by the same
person retains that run's snapshot. A separate owner's goal uses the default.
The compiled work template has its own
existing version and fingerprint: planning code and the plan it produces are
different artifacts.

The snapshot supports at most 64 constructs and 512 KiB of source. An unknown
entry, unsafe child, recursion, malformed snapshot or incompatible native
contract fails closed. A pre-upgrade run without a Spine snapshot cannot start
compilation with an implicitly substituted workflow; open a new goal. Already
compiled runs continue using their existing template and journal. Earlier
`work.spine/1` snapshots without phase entries retain their pinned compilation
policy and use the installed default recipes for the newly exposed execution
and recovery hooks. New snapshots pin all phases.

## An exact-catalog-only Spine

This complete workflow spends no model calls. It chooses to stop on a catalog
outage or a miss. Another workflow can choose a paid fallback.

```memql
use planner.builtins.{ spineCandidates, spineUseCandidate, spineRefuse }

@template
automation companyCatalogSpine {
  catalog := builtin spineCandidates(kind: "exact")
  if !catalog.ok {
    return builtin spineRefuse(message: "The catalog could not be read. Try again after it is available.")
  }
  if catalog.candidates.count() == 0 {
    return builtin spineRefuse(message: "This request needs a reviewed catalog automation.")
  }
  for candidate in catalog.candidates {
    return builtin spineUseCandidate(handle: candidate.handle)
  }
}
```

Candidate handles refer only to evidence read within this invocation under
the goal owner's authority. Supplying a construct ID cannot select work that
the scope never read. A conversational follow-up cannot reuse a text-only
catalog match even if a custom workflow asks for one.

The checked example bundle at `examples/spine/company/automations.memql` also
includes `companyAnswerSpine`: it classifies an ad-hoc question or file request,
uses a pure logic to choose a deterministic draft, and refuses model-authored
automation source. Its tests load and execute the actual example source after
a JSON snapshot round trip.

## The default policy

`defaultWorkSpine`, `workSpineDraft`, `workSpineAuthor` and `workSpineRoute` live
in the planner DSL namespace. Their decisions are inspectable source:

1. Read owned exact catalog and learned-procedure candidates. Prefer a served
   procedure, then an authored exact match; neither calls a model.
2. On a miss, consider a near match with similarity at least `0.82`.
3. Otherwise classify once, with the owner's bounded description guidance and
   conversational context. Optionally repair missing acknowledgment prose once.
4. Use pure logic to choose a direct answer, a sectioned draft or source
   authoring. Validate decomposition boundaries before constructing a draft.
   Sectioned files receive a focused planning pass: one named output per step,
   at most 800 estimated words, with explicit coverage and dependencies. The
   A separate coverage check compares the plan with the full request; the
   default DSL permits one correction of an invalid or incomplete plan using
   the stronger model tier. Intermediate steps
   return text; the final delivery saves the file once.
5. If a draft fails **before persistence**, optionally try live sections and
   then inline sections. A write failure never authorizes this fallback.
6. For source authoring, design once, emit once, validate, and optionally repair
   and revalidate before persistence. Every model stage checks the remaining
   run budget; repairs also have a native iteration ceiling.

The DSL chooses when to stop, which tiers to consult, similarity thresholds,
representation fallbacks and how many repairs to try. Native code validates
evidence, delivery contracts, source closure and side-effect boundaries.
A workflow can be more restrictive than the runtime, but cannot weaken those
gates with `on error continue` or a fabricated success return.

## Reuse or replace execution and recovery recipes

The platform ships one default harness. Accounts and organizations do not have
to install or define one. Developers select bundle code, and can reuse defaults
while replacing particular policies. An optional ordinary template named
`<entry>Phases` returns a map of hook names to installed template names:

```memql
use planner.builtins.{ spineRemedyAsk }

@template
automation companyAnswerSpinePhases {
  return {
    workSpineRecovery: "companyReviewedRecovery",
    workSpineReplan: "companyReviewedReplan"
  }
}

@template
automation companyReviewedRecovery {
  args { symptom string! retriesSpent int! maxRetries int! hasGoal bool! }
  return {act: "ask", retrySeconds: 30}
}

@template
automation companyReviewedReplan {
  return builtin spineRemedyAsk(reason: "Review the failed step before changing this plan.")
}
```

The configuration template has no native operations and takes no arguments.
Admission checks it before opening rows and freezes its source, the mapping,
and every selected dependency. Unknown hooks, missing templates and effectful
configuration fail admission. Omitted hooks use the defaults. The checked
company example contains these definitions alongside `companyAnswerSpine`.
This is a naming convention over existing automations, not a new construct.

| Hook (also the default template) | Role | Native boundary |
|---|---|---|
| `workSpineDraftProgram` | Assemble answer, navigation, research, section and file steps; choose their instructions | `spineDraftFacts` exposes validated facts. `spineDraftAppend` quotes and encodes source, validates section order and requires the promised output. It does not execute the generated work. |
| `agentWorkbenchRecovery` | Choose a headless retry or read-only computer observation after a workbench environment mismatch | `agentRecoveryContext` supplies verified pre-start evidence. Scoped actions can retry the original call once, observe the computer, or request missing consent. |
| `workSpineRecovery` | Choose `retry`, `repair`, `replan`, `heal` or `ask`, with `retrySeconds` from 1 to 3600 | Pure decision, no operations. Receives the argument shape shown above. Native terminal-failure checks, retry ceilings and replay/effect evidence remain mandatory. |
| `workSpineReplan` | Generate, validate, persist and install a replacement unfinished suffix, or request review | `spineRemedyContext`, `spineRemedyGenerate`, `spineRemedyValidate`, `spineRemedyPersist`, `spineRemedyInstall`, `spineRemedyAsk`. One model attempt, immutable completed prefix, Gate 1 and an atomic run-state check. |
| `workSpineRepair` | Request a guided rerun of the failed step or ask a person | `spineRemedyRepair`, `spineRemedyAsk`. Existing journal, effect and current-wait checks still apply. |

Except for the failure decision, hooks receive their facts through scoped
operations and take no arguments. Each invocation receives only its own
operations. A phase cannot borrow another phase's authority simply because
both definitions appear in the same snapshot. A standalone engine embedder must
wire the scoped snapshot interpreter to execute a pinned recovery policy; an
unwired engine fails closed for pinned runs.

The default draft recipe is in `dsl/planner/automations.memql`. It chooses
research before rendering when needed, catalog/live/inline sections, collection,
and final answer or file delivery. `spineDraftAppend` accepts a `kind` and,
where relevant, `section`, `instruction`, `inputHeading` and `sectionsHeading`.
Its source encoder remains native, like the parser and compiler. The resulting
automation runs through the normal durable executor.

## Headless work and computer-use fallback

The shipped fallback is expressed through ordinary actions and logic:

```memql
use capabilities.integration.agents.{ spineRetryHost, spineObserveComputer }

action retrySpineHost { capability spineRetryHost() }
action observeSpineComputer { capability spineObserveComputer(action: "window_list") }
```

`agentWorkbenchRecovery` calls the observation action for a display-only
mismatch, and the host retry action for other unmet needs. Only a verified
**pre-start** environment mismatch enters this recipe. An ordinary command
failure, timeout or uncertain outcome does not authorize repeating a command.
The retry preserves the original arguments, owner, run and worker constraints.

Computer observation currently supports `window_list` and `display_info`.
It supplies structured context for the next agent turn; it does not invent
mouse clicks, implement a visual agent or translate a refused command into an
automatic desktop action. The normal `workerComputer` capability remains the
route for authorized subsequent computer work.

The `agentRecoveryConsent` child requests the minimum required scope only on
an actual missing-consent or insufficient-scope refusal. Kill switches are
returned unchanged. Consent wording lives in pure logic, while the native gate
binds the actual owner and run. After dispatch, a transport error remains an
unknown outcome rather than reverting to the earlier pre-start refusal.

## Native operations

These builtins are scoped ports, not standalone API endpoints. Direct calls
outside an authenticated compilation fail. Operations serialize within a
compile, including calls made from parallel DSL branches.

| Operation | Input | Result / invariant |
|---|---|---|
| `spineContext` | none | Goal statement, input, conversational flag, bounded repair-attempt list. No credentials. |
| `spineCandidates` | `kind: "exact"`, `"procedure"` or `"near"` | `{ok, candidates, message}`. Each candidate has `handle`, `name`, `similarity`, `missingArgs`. Owner-scoped evidence, not permission to act on an arbitrary ID. |
| `spineUseCandidate` | `handle` | Selects the previously read plan; terminal for this compile. Learned procedures retain their execution-time ladder check. |
| `spineClassify` | none | Validated `{complexity, intent, sectionable, requiresFile, conversational, workload, acknowledgement}`. At most once. |
| `spineAcknowledge` | none | One optional prose repair after classification; returns a boolean. Cannot reclassify or execute work. |
| `spineRefineSections` | `prompt`, `maxWords`, `timeoutSeconds` | `{ok, message}`. Refines a file's routing sketch before section preparation, preserving its delivery contract. At most two metered calls with a timeout no greater than 600 seconds; rejects multiple outputs per step, oversized estimates and intermediate file effects. |
| `spineReviewSections` | `prompt`, `timeoutSeconds` | `{ok, message}`. One metered coverage assessment per refinement with a timeout no greater than 600 seconds. Missing or invalid verdicts cannot approve a plan; deficiencies feed the next refinement. |
| `spinePrepareSections` | `route: "trivial"` or `"sectionable"` | `{valid, reason}` after native boundary checks and owner-scoped section evidence. At most once. |
| `spineDraft` | `mode: "default"`, `"live"` or `"inline"` | `{ok, message, hasCatalog, hasLiveSections}`. Each mode at most once. A safe pre-write failure is data; a persistence failure aborts. Success is terminal. |
| `spineDesign` | none | One bounded model design operation, after classification. Conversational source authoring requires automation intent. |
| `spineEmit` | none | Source emission from the scope's design. Multi-phase designs meter every constituent model call. |
| `spineValidate` | none | `{ok, message}` from Gate 1 for the current source. |
| `spineRepair` | none | Repairs failed diagnostics. Checks call budget, maximum attempts, allowed changes and repeated source; invalidates the previous validation. |
| `spinePersist` | none | Seals and revalidates the passing source before writing a run-bound draft. Terminal; does not promote it into the shared catalog. |
| `spineRefuse` | `message` | Ends compilation with an explicit failure. This is not a durable human-approval wait. |

The repair ceiling is the smaller of the operator's configured authoring
repair cap and 32; the normal default is four. The run's model-call and time
ceilings can stop it sooner. Returning from a workflow without selecting or
persisting a valid plan fails compilation.

## Versioning and recovery limits

A worker's explicit whole-model-call time ceiling is a planning failure.
The default recovery asks for smaller checkpointed work instead of treating
that ceiling as a network outage and repeating the same call. Ordinary
transport and idle timeouts retain their separate classifications. Replanning
reads the original owner-scoped source exactly as sealed, keeps the completed
prefix and private dependencies, and validates the replacement before execution.
A changed source closure is refused rather than silently reinterpreted.

The source fingerprint pins the templates, pure logic, actions and phase mapping. It is **not**
a promise of byte-identical model output or a snapshot of the entire
installation. The typed native operations in contract v1 retain the engine's
shipped classifier, design, emission and repair prompt recipes, provider
routing and agent runtime. This contract does not yet expose an arbitrary
prompt/model/tool callback inside compilation. Record the engine release and
bundle digest alongside the workflow fingerprint when reproducing a result.

Templates may use expressions, branches, finite loops, synchronous children,
joined parallel blocks and pure logic. Actions are allowed only when their capability is explicitly bound by the
invoking phase. Arbitrary queries, mutations, ambient integration calls,
independent journals, scheduled triggers and detached children are outside
these scopes. Work produced by the Spine runs through the normal
executor, where actions, bounded agent turns and human approvals already have
their own contracts.

Snapshots make workflow definitions portable across replicas. They do not
make compilation a resumable transaction. Existing claims and heartbeats
prevent concurrent planners from compiling one run. A stale compile that may
have written data is handled by the existing run-failure path, not restarted
from the beginning by this API. A transport failure after a draft write may
leave partial bundle rows; the run fails and must not select them as a
validated plan. This is distinct from replaying completed execution steps
through the work journal.

## Validate a company bundle

Run the normal DSL linter against the installed bundle. Exercise both the
chosen path and refusals with the scoped host, including a snapshot serialized
on one host and restored on another. Tests should cover a catalog miss, invalid
classification, exhausted budget, failed validation, uncertain persistence,
cancellation and an attempted direct call to a scoped builtin.

The engine's own tests cover the default workflow, the example company
workflows, immutable transitive source, refusal before effects, and the
BFF-to-planner handoff through stored rows. No special parser extension or
custom Go runtime is required for these examples.
