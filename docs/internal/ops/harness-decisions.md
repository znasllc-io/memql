---
title: Harness decision map
audience: internal
status: stable
area: ops
sinceVersion: 0.30.9
owner: znas
description: Maintainer map of Apps delegation, evidence reuse, waiting and context recovery decisions.
---

# Harness decision map

This is a maintained implementation reference for agents and developers changing
the harness. Follow the [required documentation rule](../../../CLAUDE.md#harness-changes-document-decisions-and-paths-required)
and update this map when a covered path changes. User-facing behavior belongs in
[work context and recovery](../../public/ai/work-context-and-recovery.md).
These paths are shared across goals and projects; repository names, user-specific
paths and one task's acceptance criteria must not become routing rules.

## Delegation and return

The proactive context target includes exact current call arguments as required
input, including a prepared operation followed by a human approval. The
compactor cannot retire that call, so omitting it from the target caused an
otherwise valid continuation to refuse before reaching the model. Incomplete
call groups also retain their returned results until the protocol is complete.
Completed result bodies do not increase this floor: they can be referenced
from the journal. A later assistant response makes an older completed call
ordinary history again. This changes a history-compaction preference only;
actual model-window recovery retains its strict target and output reserve.
`TestWorkContextPreparedCallSurvivesApprovalWithoutRelaxingHardRecovery`
reproduces the refusal and verifies exact resumption without a model call,
while a deliberately smaller hard target still refuses. No command is run,
replayed or authorized by this calculation.

| Decision | Reason and resulting path | Implementation |
| --- | --- | --- |
| Select a model and execution route | Use the current policy and requested reasoning level. An Apps route does not grant tools, host access or ownership. Missing app-session wiring is a platform fault, not permission to open another machine. | [App session provider](../../../component/memql/app_session_provider.go) |
| Admit a delegated tool call | Recheck current ownership, agent role, captured tool ceiling, expiry and run lifecycle. Reject cancelled, superseded or foreign work. Shared storage allows the receiving replica to make the same decision. | [MCP transport](../../../app/transport_mcp.go), [scope mutation guard](../../../component/memql/app_session_scope_guard.go) |
| Pause for information or access | Persist native context and exact server-observed public tool results before creating the request. Incomplete receipts or exceeded scan/byte bounds stop the pause path. App prose cannot certify an effect or reconstruct a continuation. | [Delegated source receipts](../../../component/memql/work_delegated_sources.go), [continuations](../../../component/memql/work_continuation.go) |
| Return after a native wait | Inspect the native control plane, including an answer that arrived before the app finished. Yield the old invocation without creating a second question or overwriting the resume. The adapter must expose this check to the agent loop. | [Human wait handling](../../../integrations/agent/human_wait.go), [production adapter](../../../app/adapters.go) |
| Rehydrate after an approval | Carry the exact new decision, then restore the required native checkpoint. Do not append the original conversation's source inventory to each approval: it is already represented in saved work, and repeated copies trigger unnecessary summaries. Fresh executions without a required checkpoint still receive that inventory. | [Work context](../../../integrations/agents/work_context.go), [continuation validation](../../../component/memql/work_continuation.go) |
| Resume filesystem state | An empty snapshot needs no restoration. A populated snapshot still requires supported restoration; do not substitute an empty workspace or replay completed effects. | [Automation resume](../../../component/automations/resume.go), [child continuation](../../../component/automations/child_continuation.go) |
| Associate returned evidence | The session runner supplies the native run and step, while the app supplies its reported model and effort. Resolve the owned artifact and its distinct backing file before stamping both; refuse missing, foreign or conflicting producer links. | [Artifact provenance](../../../component/worker/appsession_store.go), [session runner](../../../component/worker/runner.go) |

Returned Apps evidence is stamped as `app_session` on both the Library index
and its backing file. `producedByRunId` comes from the native invocation;
the file also records `producedByStepKey`, normalizing the session's canonical
step ID to the logical key used by Library filters and delivery receipts.
Reads use the owner's actor and a
fresh cross-replica view; only the trusted provenance mutations use internal
origin. File IDs are resolved through `sourceConceptRef`, not guessed from
artifact IDs. Non-file outputs receive the index stamp without an invented
file. An unbound conversation omits native identity fields, preserving any
existing association. A different nonempty producing run is refused rather
than rewriting history. These checks grant no access to another owner's data.

The index and file writes are separate, retryable read-merge operations. A
failed stamp is logged and returned to the runner without invalidating the
already completed app work; this is not an atomic two-row transaction or an
automatic backfill of older evidence. `TestAppArtifactProvenanceLinksOwnedFileAndRun`
executes the actual DSL against a required test database and covers both
rows, preserved bytes/metadata, unbound sessions, foreign/missing links and
conflicting runs. Runner tests verify native identity forwarding and that
unreported model/effort remain empty. MIME detection, file selection, duplicate
intermediates and byte-readback verification are independent of this repair.

`PrepareDelegatedFeedback` starts with the native context actually delegated,
including compacted memories and archive references. It appends only that app
session's observed exchanges in execution order and an unexecuted pending call.
It scans at most 16 pages of 16 receipts, with a 4 MiB retained exchange-content bound.
An individual source receipt exceeding 1 MiB records an explicit omission and
cannot be accepted as a complete continuation. These are refusal boundaries,
not permission to truncate evidence or silently reconstruct missing state.

Before counting retained bytes, a result larger than 64 KiB becomes an exact
reference to its already durable public source receipt. The call arguments,
call/result IDs and ordering stay intact; the reference carries the original
result's byte length, SHA256 and authorized `recallWorkHistory` arguments.
This avoids duplicating screenshot/document bytes in another archive and does
not invoke a summarizer. A missing source identity still refuses suspension.
The original per-receipt bound, scan bound and retained-content bound remain;
large call arguments cannot evade them. References make no semantic or success
claim and do not create Library files. Explicit artifact retention and image
inspection are separate steps. `TestDelegatedFeedbackReferencesLargeDurableResultsAcrossReplicas`
reproduces seven large results exceeding the old continuation bound and verifies
bounded resumption, unchanged originals, paired calls and recall on another
replica. The existing exact-feedback test covers unchanged small receipts.

`TestNativeContinuationDoesNotReinjectConversationSourcePreview` reproduces the
approval-resume duplication with a large source inventory and verifies that the
exact decision survives, fresh executions still receive source context, and a
mismatched decision is refused. This change affects prompt rehydration only; it
does not rewrite earlier checkpoints, discard journal records, or establish that
an operation ran. Existing continuation validation still refuses missing or
changed required checkpoints. Previously accumulated copies can still require
compaction, and end-to-end latency depends on the selected model and remaining
work; the regression is not a timing guarantee.

The active compiled instruction has a native retention marker separate from
the first and latest human messages. Approvals and guidance can arrive after
that instruction; they must not make its stage boundary eligible for summary
or archival. Context selection pins it verbatim, includes it in the retained
size floor, and passes a bounded excerpt into summary relevance. If the retained
instructions exceed a model's capacity, ordinary capacity checks still apply.
The marker retains a user-role message; it grants no permission or higher role.
Conversation history cannot supply the marker.

After exact checkpoint, owner and execution validation, resume restores the
instruction supplied by the executing automation. This repairs older checkpoints
whose unmarked stage instruction was compacted away. It replaces stale marked
copies and upgrades an identical legacy copy, while preserving completed tool
exchanges and later human corrections in order. It does not rewrite the saved
receipt. Empty or ambiguous current instructions fail rather than selecting a
contract arbitrarily. `TestWorkContextKeepsCurrentInstructionAfterApprovalAndGuidance`,
`TestRestoreWorkInstructionPreservesEffectsAndHumanCorrections` and the required
database continuation test cover retention, idempotence and exact effects across
replicas. This prevents instruction loss; it does not certify that a model obeyed
the stage, completed the work or delivered faster results.

Worker output backpressure must not prevent a disconnect from taking effect.
App chunks and model deltas watch the shared stream's cancellation while waiting
for buffer space. Delivery and channel closure are serialized, with the terminal
signal closed first to release a blocked sender. This preserves ordered output
while connected without letting an old connection prevent replacement.
Caller cancellation and model idle/operation watchdogs finish the local output
stream after requesting remote cancellation; a missing remote `End` must not
leave the parent's drainer waiting forever. The first terminal decision remains
authoritative if a late `End` arrives. These are invocation boundaries, not goal
lifetime limits or result acceptance, and local cancellation does not prove the
remote process stopped or that all artifacts returned.

The regressions in
[stream cancellation tests](../../../component/worker/stream_cancellation_test.go)
reproduce open drains after cancellation and a full app buffer ignoring a
disconnect. They also cover model idle/whole-call watchdogs, model backpressure,
late completion and concurrent send/finish under the race detector. These tests
establish the recovery defects independently; they do not prove that every live
offline report has the same cause.

## Computer capability discovery

The fleet catalog supplies a missing legacy `display=true` routing label from
a validated worker descriptor reporting computer use on Quartz or X11. This
lets current workers be discovered by existing display selectors without a
manual label. Explicit reported or operator labels still win; absent, invalid,
headless and unsupported descriptors do not infer a display. The projection
changes no stored registration, ownership, permission or consent. Action-level
dispatch and worker preflight remain mandatory.

The same projection is used on every replica through
`component/worker/fleetcatalog/store.go`. The required-database regression
`TestDisplayDescriptorSuppliesMissingRoutingLabelAcrossReplicas` covers supported
displays, explicit exclusions and invalid/headless descriptors. A routing-ready
display does not prove that a screenshot or UI interaction succeeded; those need
their own execution evidence.

### Native screenshots through Apps

After the existing tool and computer permission checks succeed, the MCP
presentation adapter recognizes a successful `workerComputer` screenshot
receipt and supplies its exact PNG or JPEG bytes as MCP image content. A
base64 string inside text is not a visual observation. Metadata, including the
worker receipt and coordinate scale, stays in a text block; an image-content
index, size and SHA-256 replace the duplicated base64 field. Both ordinary MCP
and delegated Apps calls use this path. Delegated source recording saves the
exact image-bearing result returned to the app, so resume evidence agrees with
what was delivered rather than the old text-only representation.

The adapter accepts only the native successful worker envelope for the
screenshot action. It bounds encoded input to 4 MiB of image bytes and decoded
dimensions to 16 megapixels, validates the full PNG/JPEG and its declared size,
format and dimensions, and never fetches a URL or changes the pixels. Invalid
or oversized images retain their original text receipt with an explicit image
delivery failure. Capture is not retried and `isError` is not set for this
presentation failure: the underlying operation already happened. Failed
captures, unrelated tools and already structured MCP content pass through.
No permission, owner, goal lifetime or native capture policy changes here.

The existing journal receipt-size limit still applies; the adapter does not
turn an oversized receipt into a Library artifact or relax retention bounds.
Image delivery, persistent artifact readback and inspection of the intended
interface are separate claims. A visible desktop screenshot of another app is
not a valid UI baseline. Test coverage in
`component/mcp/computer_images_test.go` checks both MCP paths, exact delegated
recording, unchanged bytes, malformed/corrupt/oversized pixels and unaffected
tool results. Tests exercise both raw worker graphs and the engine's actual
single-text-block `ToolCallResult` envelope, preserving outer metadata without
duplicating its original image text. Mixed/already-image content passes through.
Live delivery and task-specific visual verification require
their own recorded observations.

## Evidence and reuse

| Decision | Reason and resulting path | Implementation |
| --- | --- | --- |
| Reuse existing research | Prefer relevant authorized originals when freshness and applicability are established; refresh only missing evidence when economical. This is shared guidance, not a guarantee of identical model choices or a grant of cross-goal access. | [Work reply guidance](../../../dsl/agents/prompts/workAgentReply.tmpl) |
| Distinguish an original from a preview | Project paired executed source calls/results. Exclude generated prose, secrets, pending calls and retrieval wrappers; keep the complete journal separately. Preserve historical indexes when projection rules change. | [Source projection](../../../component/memql/work_source_context.go), [history retrieval](../../../component/memql/work_history_sources.go) |
| Search older receipts efficiently | Scan at most eight four-row pages per call, stopping on matches or a useful directory. Keep the last cursor for older records. This reduces empty model round trips without making scans or response size grow with the full journal. | [History retrieval](../../../component/memql/work_history_sources.go) |
| Read a saved file | Resolve Library bytes through the configured storage integration and recheck owner, revision and content constraints. Metadata or a blob reference alone is not evidence that content was read. | [MCP bootstrap](../../../app/transport_mcp.go), [Library integration](../../../integrations/library) |
| Retain versus reuse a working resource | Save useful deliverables and bounded supporting evidence with provenance. Keep checkouts and dependency caches as working resources; do not archive an entire repository by default. Preserve explicit omission/error records when a file cannot be retained. | Shared work guidance and the companion worker's artifact collector |

Exact source history selects the current owned run or an earlier run in its
saved conversation. An arbitrary run ID, even for the same owner, is not an
authorization grant. A checkpoint/message index addresses the unchanged source;
`nextOffset` pages its serialized characters and `cursor` searches older rows.
Byte recovery and content verification are different from semantic retrieval.

## Context pressure and optional work

Known context-capacity refusals at a reasoning boundary use the existing native
model-call checkpoint suspension. Proactive compaction enters it directly after
its bounded attempts; provider overflow first tries the strictly smaller context
handoff. Both streaming and non-streaming loops preserve the current exchange
before returning `HumanWait`. A generic failed-step Retry creates a new execution
version and therefore cannot stand in for this suspension. No historical
waiting links are invented, and uncertain in-flight tool failures never enter
this path.

`workModelCallRecovery` supplies the capacity-specific question; the native
engine enforces zero further automatic retries for this case, unchanged owner,
run, goal and execution identity, and a saved checkpoint before publishing the
question. Another replica verifies that checkpoint and resumes the same child.
Storage errors, authorization failures and cancellation are not context-capacity
signals. `TestContextCapacitySuspendsExactPreparedWorkInBothLoops` reproduces the
lost suspension and checks a retained prepared call and completed effect.
The `context-capacity` cases in `TestWorkDBModelCeilingKeepsExactNestedProgress`
exercise actual nested journals, the public approval decision, peer dispatch and
resume without replaying the completed read or workflow prefix. The other cases
retain coverage for failed checkpoint writes, stale work and changed bindings.

Completed old calls whose arguments alone exceed the checkpoint model's input
window are archived before semantic compaction spends calls on smaller history.
This handles large file/image transfers without depending on a tool name or
task category. The same selector protects authority, the active instruction,
original/latest human requests, incomplete pairs and the latest exchange.
At most eight exchanges of at most 4 MiB are archived per pass, largest first;
each exact source is durable before its incomplete preview enters active
context. This creates no semantic summary and never replays an effect. Failure
to store an archive stops compaction; ineligible or larger exchanges remain for
the existing bounded path. `work_context_large_calls_db_test.go` reproduces
unnecessary inference before this ordering change and verifies zero summary
calls plus exact cross-replica readback afterward. Ordinary small receipts,
instructions and the fresh tail remain verbatim in that regression.

[Context management](../../../component/memql/work_context.go) first archives
bulky completed results, then uses a bounded semantic pass over complete older
exchanges. It preserves authority, current human direction and pending tool
protocol. After eight semantic attempts, exact archival of remaining completed
results gets a final chance to fit the target without more inference. A request
whose small calls and previews still exceed the target can offload whole older
completed exchanges, including arguments. This final pass makes at most eight
storage attempts of at most 4 MiB each and never calls a model. It uses the same
protected-unit selector and saves exact bytes before returning an incomplete
preview/reference. This handles dense histories that per-result offloading
cannot shrink, while refusing to fabricate a semantic summary. A request that
still cannot fit fails with its originals retained.

Semantic summary identity includes both source bytes and task relevance. This
prevents an old request's focused summary from silently answering a changed
request. It also means fresh human direction can legitimately cause new summary
calls. Exact archives remain reusable independently. Do not remove the relevance
key merely to improve a latency metric; assess instruction retention as well.

[Observation indexing](../../../component/memql/executor_mutation.go) happens
after the durable write and is optional. It skips administrative labels and
indexes meaningful summaries instead of archive hashes. Its five-second child
deadline preserves the caller's authority and earlier cancellation. Optional
knowledge search is bounded similarly; required instructions and evidence reads
must not be converted into optional failures. A failed vector write can reduce
semantic recall until backfill, but does not remove the exact journal source.

Optional Training similarity retrieval uses a five-second child deadline for
its query embedding. A timeout of that child records `embedding-timeout` on the
normal owner-scoped selection receipt and contributes no passages. Caller
cancellation, ownership/source failures, provider errors and explicit evaluation
lessons still propagate. This prevents a cold or busy optional inference door
from spending an entire turn before execution. Coverage includes
`TestOptionalTrainingVectorBoundsOnlyItsOwnWait` and required-database
`TestOwnedTrainingReadsReviewedTaughtSourcesAcrossReplicas`, including omission
readback on another engine. The Training interface is unchanged.

## Progress windows and result acceptance

Time and model-call estimates bound a work window, not a goal's lifetime.
`integrations/work/progress_continuation.go` renews only after successful new
receipts or a verified human answer, under the shared run decision lock. The
old three-automatic/sixteen-total caps and fixed two-hour context deadline are
removed. Monetary limits, scoped permissions, cancellation, per-operation
watchdogs and the existing stalled-loop detector remain independent.

Counters never reset on renewal. A human-directed restart opens a new evidence
epoch and clears only its bounded duplicate cache; receipts older than the epoch
cannot fund another window. Original evidence remains in the journal. This
keeps a memory bound from becoming a lifetime limit while preventing an old
answer or receipt from buying another window on a second replica. The signal is
completed activity, not certification that the user's objective is achieved.

Execution writers no longer close goals on success, failure or cancellation.
`workAcceptGoalResults` accepts an exact finished run revision only from its
owner's interactive request. It serializes with rerun, branch, replay and cancel, refuses newer or
live results, persists optional feedback and is idempotent for the same decision.
`cancelGoal` also refuses calls from running agents. A person can accept a useful
partial result, so acceptance does not require the model's success label.
Initial plan approval and final result acceptance are separate decisions.
New executions of a closed goal require the owner's interactive request;
an agent cannot undo an acceptance by replaying or branching the result.
`TestGoalAcceptanceDBPreservesOwnerDecisionAcrossReplicas` exercises acceptance,
refused model reopening, explicit owner continuation and stale acceptance refusal
through separate engines. Connecting result acceptance to the interface remains
pending; the endpoint alone is not a verified owner review experience.

Coverage: `TestProgressContinuationsRequireNewWorkBeyondLifetimeCaps`,
`TestProgressCanRenewAfterDaysWithoutALifetimeLimit`,
`TestProgressFeedbackEpochDoesNotRecycleOldEvidence`, the required-database
`TestProgressDBWorkflowAcrossReplicas`, and `TestAcceptGoalResultsRequiresExactHumanReviewedResults`.
These tests are not a months-long soak. Usage reconciliation still has its
separate fail-closed receipt tracking bound; it must not be described as
unbounded in-memory tracking or as owner acceptance.

## Recovery boundaries and remaining limits

Ordinary Ask follow-up routing considers preparing, running and waiting goals.
An abandoned goal is outside that active set, so ordinary intake can create new
work. Use the original request's explicit recovery/guidance controls when
continuing it; do not describe automatic routing to abandoned work as supported.

The software-development guidance asks for source discovery, visual and
interaction checks for UI work, native checks, authorized delivery and cleanup.
Those instructions do not prove that a particular goal completed them. Keep
actual source revisions, screenshots, tool receipts and CI results with the work.
Likewise, login guidance should request a separate browser tab and verified
resume; a written instruction is not an end-to-end login test.

## Regression evidence

| Contract | Regression coverage |
| --- | --- |
| Exact delegated checkpoint, native wait and early answer | [Delegated feedback database tests](../../../component/memql/work_delegated_feedback_db_test.go) |
| Owner, lineage and unchanged continuation bytes across replicas | [Continuation resume database tests](../../../component/memql/work_continuation_resume_db_test.go) |
| Original-source projection, wrapper exclusion and stable paging | [Source projection tests](../../../component/memql/work_source_context_test.go), [history database tests](../../../component/memql/work_history_sources_test.go) |
| Context exhaustion, exact archives and summary reuse on another replica | [Context selection database tests](../../../component/memql/work_context_selection_db_test.go), [fresh-result tests](../../../component/memql/work_context_fresh_result_witness_db_test.go) |
| Meaningful optional indexing and caller deadlines | [Work progress tests](../../../component/memql/work_progress_test.go) |

Required-database tests must run with `MEMQL_REQUIRE_DB=1` against the supported
disposable test database; a skipped test is not validation. Report the exact
commit and test result separately from live workflow evidence. These tests do
not establish complete UI delivery, arbitrary cross-goal freshness, all worker
artifact formats, or a completed login handoff.
