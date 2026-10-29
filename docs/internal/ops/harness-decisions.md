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

Human corrections sent through **Discuss in Ask → Send guidance** are durable
instructions for unfinished work. Previously they survived only in the immediate
decision checkpoint. A later permission receipt could make the older correction
archivable, and a fresh Apps worker could resume the superseded task. The native
guidance reader now retrieves answered corrections from the owner's approval
journal, including the current run and its root, on every normal guidance read.
An acknowledgment is not a delivery cursor: a later worker reads them again and
deduplicates in its own active context. Retry and replan generation changes do
not invalidate human direction about the remaining goal.

`workApprovalGuidanceForOwnerRun` selects only the owner's answered feedback,
with the same owner as decision maker. The reader validates the typed guidance
contract, decision time and run binding; approved commands and rejected requests
are not instructions. These corrections have their own 16-message/16,000-rune
context bound. The query reads an extra row so overflow refuses explicitly
instead of silently dropping legitimate history. This is a context bound, not
a goal lifetime limit. Routed messages, late optional answers and corrections
are presented in human timestamp order so source-query order cannot reverse a
later change of direction. Restoring feedback neither grants permission nor
replays an operation. Existing ownership, cancellation and execution checks
still gate the read and every effect.

`work_approval_guidance_db_test.go` reproduces the missing correction using real
storage and verifies later approvals, retries, another replica, delegated child
work, chronology, ownership and overflow. The regression uses document editing;
no repository, language or feature-specific branch is involved. These checks
establish delivery, not model adherence or a completed user task.

The live Apps check exposed a second ordering seam: the newer correction was
present, but compaction had archived an older additive step override, and the
Apps delegate appended that override at the very end again. The guidance reader
now includes the exact current override instructions and dislike guidance at
the rerun's recorded `requestedAt`, before any later human corrections. Their
exact rendering prevents the delegate's missing-instruction fallback from
re-appending them. The two bounded override messages belong only to the current
step/context and do not alter model, effort, input or permission bindings. A
missing decision time refuses instead of inventing chronology. Explicit whole
session prompt replacement remains a separate contract for a fresh rerun. The
database regression
`TestStepOverrideKeepsItsPlaceBeforeLaterHumanCorrection` checks both older and
newer human corrections around the override and the flattened Apps prompt.

An approval continuation keeps the recovered handover at the last Apps delegate
boundary, including when the original rerun used whole-prompt replacement. The
delegate previously reapplied that old replacement after the engine had restored
the completed tool results, pending operation and latest recorded decision. The
saved continuation was intact, but the new App received only the original short
override and repeatedly restarted recovery. On a native continuation, the
delegate checks the run, owner, step, checkpoint/decision presence and override
fingerprint, then preserves the handed-over prompt verbatim. Model and effort
overrides still apply; a fresh explicit rerun still replaces its whole prompt.
This changes context delivery, not permission: dispatch continues to enforce
the approved operation, remaining use, expiry, revocation and local consent.
`TestSessionWholePromptOverridePreservesApprovedNativeContinuation` follows the
delegate through the actual AppSessionStart specification, checks completed
source and pending-operation retention, rejects mismatched bindings before
launch, and retains the fresh-rerun control. The existing database continuation
test covers cross-replica checkpoint/decision validation upstream. These checks
do not establish model adherence or completion of the downstream task.

Whole-prompt replacement must also reach checkpoint creation upstream. A live
rerun stored the current replacement correctly and launched its first App with
it, but the native context builder marked the older compiled section as active.
Every resumed App then recovered that obsolete instruction. The builder now
selects the executing step's nonempty whole-prompt override before assigning the
retention marker and skill-preparation hint. It does not append a duplicate
replacement after later human answers. Additive overrides keep their existing
chronological guidance path; an empty replacement retains the compiled section.
The final delegate still preserves the complete restored handover on native
resume. No checkpoint is rewritten, permission extended, effect replayed, or
owner/step/generation/override binding relaxed.

`TestWholePromptInstructionIsRetainedBeforeNativeCheckpoint` exercises the
installed context-builder entry with ordinary, additive, whole and empty
replacements. `TestWorkDBWholePromptSurvivesNestedPermissionResumes` reproduces
the old compiled instruction on the first checkpoint, then covers a fresh rerun
and two nested permission waits across engines, exact completed effects, later
human correction, unchanged source checkpoint and one-use permission consumption.
The model boundary is controlled; these tests do not prove downstream adherence.

Native approval resumes must not reinsert already archived question bodies.
The history builder previously loaded every answered feedback question, then the
checkpoint restorer deduplicated only exact messages still in active context.
Once a question had been compacted, the next approval made its original body
look new again. A live checkpoint grew by roughly 28 KB without any new tool
results. A validated continuation now projects only its current wait's recorded
answer from that query; its required checkpoint supplies the earlier history.
Fresh executions still load all answered questions. Late optional answers,
routed corrections and approval guidance keep their independent owner-scoped,
chronologically ordered reads at model/tool boundaries. Missing or changed
checkpoints still refuse; no immutable source or completed effect is rewritten.

`TestNativeContinuationDoesNotReinjectArchivedAnswers` reproduces reinsertion
and checks both permission and feedback waits plus fresh-execution controls.
The `archived-history` database case in
`TestWorkDBNativeContinuationProjectsExactDecisionOnAnotherReplica` runs the
installed context builder against stored historical answers and a compacted
checkpoint on another engine; it retains the memory, active instruction, exact
completed tools and current permission receipt without expanding those answers.
Existing late-answer and additive-correction ordering tests cover the separate
guidance paths. These checks prove context selection, not model adherence or a
measured latency improvement.

Final reply review must use current owner direction, not only the original goal
and conversation snapshot. A resumed execution could follow a replacement yet
its final reviewer reinstated an obsolete hold. The reviewer now reads the same
owner-bound, generation-checked guidance as execution on the finishing replica,
including a whole-prompt override at the rerun's recorded time. Later corrections
remain later. Original intent stays historical context; these instructions are
separate from source receipts and do not prove effects or grant an operation.
Missing lineage, unavailable guidance or oversized request context refuses the
review rather than silently omitting direction.

Only the review helper receives a copied override with instruction text removed:
the ordered request already contains it. Model, effort and level remain pinned.
This prevents the generic model seam from appending an older instruction after a
newer correction or an Apps door from replacing the review contract. The actual
execution override, checkpoints, permissions and completed effects are unchanged.
`TestReplyReviewUsesCurrentOwnerDirectionAfterRetryAcrossReplicas` reproduces the old
behavior for additive and whole-prompt reruns using a document-publication case,
then checks chronology, foreign-user exclusion, routing and immutability at the
structured model boundary. These tests establish context delivery, not model
adherence. This repair does not broaden the lookup continuation policy, retry an
input interruption or equate a successful turn with an accepted goal outcome.

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

The newest recorded human decision stays exact independently of the first and
last human messages. Goal guidance can be reattached after a fresh approval;
using message position alone archived the decision while leaving the prepared
command in context. A new Apps session then searched nested history or repeated
inspection instead of continuing. Selection, tail-fit checks and the proactive
budget now count the same current receipt. Earlier decisions remain archivable;
tool-output quotations cannot replace a human receipt. This retention grants no
permission, proves no execution and changes no ownership, revocation, use-count
or spending checks. A strict provider limit still refuses when these exact
inputs cannot fit. `work_context_decision_test.go` reproduces the selection bug
and tests exact retention on two database-backed engines, with no effect replay
or summarizer call. This preserves the decision in context; it does not guarantee
that a model chooses the next action or that a UI result is correct.

Approving a desktop, file-write or HTTP session with a server-bound native
checkpoint resumes the existing execution generation. The old legacy-scope
branch created a new rerun on approval, invalidating the checkpoint immediately
after a successful operation and preventing subsequent visual verification.
The approval classifier recognizes the hashed native witness; actual resumption
rechecks its transcript identity, pending call, request hash, owner and execution
generation before reasoning or effects. Legacy requests without that witness
keep their existing retry behavior; rejection still stops the run. No completed
effect is replayed and no access duration is extended by this change.
`TestScopeCheckpointApprovalPreservesExecutionAcrossReplicas` reproduces the
generation replacement and checks all three session kinds, ordinary and rerun
executions, rejection and a forged pending-request hash with real storage on two
engines. It verifies decision and transcript restoration, not a live screenshot.

## Produced file readability

Apps output transport must declare the file's format without changing its bytes.
The Cockpit uploader previously used multipart's unconditional binary default,
so a retained JSON checkpoint had a valid digest but the text reader refused it.
The repair belongs in the producer: `ResolveLibraryMIME` continues to honor an
explicit type and `libraryReadFile` retains its owner, revision, MIME, UTF-8 and
size checks. Cockpit classifies the complete body conservatively before upload:
binary signatures survive, and text requires full UTF-8 and control-byte checks.
Only validated JSON with a JSON extension receives `application/json`; Markdown
and ordinary text remain format hints, never trusted instructions. Existing
opaque rows are not silently rewritten. A normal authorized upload/version is
needed to change their type; archive extraction and semantic indexing are not
added by this repair.

Cockpit's `TestLibraryPushContentTypePreservesBytes` covers both upload entry
points, exact multipart bytes, identity, binary signatures and misleading text
extensions/suffixes. `TestDocumentRevisionWorkflow/HistoryAndApproval/UploadedJSONReadAcrossEngines`
uses the real upload handler and persisted owner-stamped rows, then reads JSON
on a second engine without an artifact index. The same bytes under binary MIME
remain refused, and unauthenticated/foreign readers never fetch the blob.

## Knowledge requirements can change during execution

An initial role or skill binding is not a complete account of a goal's needs.
Repository discovery, an unfamiliar output format, a source conflict or a failed
check can expose a new requirement. The shared guidance makes identifying that
requirement the working model's responsibility, within its current reasoning
turn; it does not require the person to supply a technical procedure. No extra
classifier runs on every greeting or tool call, and no language-name switch in
the harness chooses a fixed engineering workflow.

`consultWorkKnowledge` provides an explicit next action for that new need.
`integrations/work/knowledge_consultation.go` accepts bounded need/context text,
checks current owner/run/step through the skill reader and retrieves a small
allocation of current reviewed instructions and Training. Initial skill context
remains frozen for replay; consultation gets a separate receipt. The broad goal
text is not automatically prepended to the narrow lookup, because it would keep
displacing the newly relevant specialty. Matching is lexical for skill selection
and semantic for Training. Neither score establishes readiness. The response
states `not-assessed`; applicability and unresolved needs remain model judgments
that must be checked against source and outcome evidence.

The Library gateway opens only the fixed reviewed-lesson reader and stamps
internal origin only for that call. Current source ownership, approval,
supersession, cancellation, inherited inference routing and spending checks still
apply. It returns the recorded availability status even with zero passages;
unindexed, unavailable and empty sources are not evidence of absence. It does
not run corpus installation, training or indexing on each consultation. Fresh
live consultations recheck publication; replay and a fork's shared prefix must
consume original tool receipts, not spend another retrieval or change history.
Journal failure is an error rather than an unrecorded successful consultation.

Research can continue through authorized concept reads, semantic memory recall,
artifact search and source readers. The tool makes its scope explicit: reviewed
knowledge consultation does not search all records or blob contents. Owned work
also receives `artifactSearch` directly; permissions remain in its existing
handler. A search result is a locator, not a claim of current contents or approved
Training. **Known coverage limit:** the existing Library similarity path ranks a
bounded cross-owner candidate pool before owner filtering. It can miss relevant
owned artifacts. It also lacks a uniform index-coverage receipt across all
concepts and artifact formats. These are remaining retrieval work, not fixed by
offering the tool. The [adaptive knowledge plan](../planning/adaptive-work-knowledge.md)
records the connected requirements and evaluation boundaries.

`TestSkillConsultationAdaptsWithoutRewritingInitialSnapshot` checks newly relevant
instructions, later deactivation and unchanged initial context across replicas.
`TestOwnedTrainingReadsReviewedTaughtSourcesAcrossReplicas` exercises the ordinary
caller gateway, actual returned passages, empty-corpus status, review changes and
replay/foreign-scope refusals with real storage. `TestKnowledgeConsultationReportsRetrievalWithoutCertifyingReadiness`
checks bounded composition and honest status. The owned-work prompt test checks
tool delivery across software, research, documents and answer-only sections.
These tests do not establish that a model will recognize every missing need;
that requires held-out goals, interaction evidence and measured interventions.

## Execution decision map

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

Default history discovery searches original tool fields across owned execution
receipts and compaction archives. Searching serialized consolidated context had
ranked newer recall requests, nested recalled answers, and generated call-ID
substrings ahead of the underlying evidence. The source search skips those
derived matches and reads at most eight pages of four records before yielding a
cursor. It does not silently fall back to recursive context search. Original
tool results saved only by compaction remain searchable, while explicit
checkpoint/conversation selectors retain exact bytes, indexes and offsets.
Argument-only matches select their completed tool result and identify the match
field; they never return raw assistant prose alongside the call. The bounded
directory filters excluded call arguments while preserving index slots.
Owner and earlier-conversation-run checks remain authoritative across replicas.
`TestWorkHistoryDefaultSearchSkipsRecallEchoesAndOpaqueIDsAcrossReplicas`
reproduces forty newer echoes and ID-only false positives ahead of an original
receipt, checks source hashes and archive-only recovery, and denies a foreign
owner. `TestWorkHistoryOriginalSearchPreservesOffsetsAndExcludesPrivateArchiveFields`
checks Unicode/JSON offsets, argument-only results and filtered directories.
These retrieval tests establish source selection and preservation, not improved
model completion time or successful downstream execution.

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

### Reported worker failures

A worker denial or partial failure stays failed in both execution paths.
The dispatch builtin returns its receipt alongside a typed error, so a native
automation cannot journal the effect as completed. The canonical `workerHost`
and `workerComputer` tool handlers recognize only a receipt bound to that exact
tool and carry its immutable bytes through the ordinary result codec with MCP
`isError=true`. The cognition adapter classifies the reported failure as
non-retryable and retains its output. A generic transport error, an unrelated
tool, or a payload outside the declared worker result contract does not take
this path. Missing or invalid receipt evidence cannot authorize another dispatch.

This preserves the permission decision without turning a reported refusal into
a transport failure or a successful effect. Database-free coverage lives in
`TestWorkerBuiltinDoesNotCompleteAFailedForwardedEffect` and
`TestWorkerToolExecutionFailureKeepsOnlyTheDeclaredReceiptInBand`;
`TestWorkDBWorkerRefusalReachesCanonicalToolAndAdapter` checks the installed
tool and adapter on independent database-backed engines. Compile-only validation
of that last test does not establish its execution result.

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

## Verify the delivered target

The shared [execution guidance](../../../dsl/agents/prompts/workAgentReply.tmpl)
distinguishes changed inputs, produced output and the target consuming that
output. This belongs in the common workflow prompt, not a Go-specific command
or a repository-name switch: a service may require compilation and restart, a
frontend may require a bundle rebuild, an interpreter may reload automatically,
and a document may require a fresh export. Discover the applicable procedure
from the project's instructions and current target, with bounded reads.

The automatic-reload path requires an observation that the target received the
change. The explicit-transformation path uses the supported authorized build,
export or load procedure and then checks the actual result. A later-stage path
hands off exact changed inputs, existing checks and the remaining delivery gap.
Source tests and mocks support these checks but do not prove that the requested
target changed. A build/content identity links the output to that target;
behavioral or visual inspection establishes the requested result. Retained
procedures carry their source version, scope and evidence so subsequent work
can check applicability economically instead of repeating setup research.

Authority is evaluated against the whole request and recorded decisions. Do
not ask again for a local rebuild or verification action already authorized;
do not turn source-edit permission alone into production deployment or broader
access. Existing native operation gates remain in force.

This is reusable model guidance, not a new deterministic deployment verifier,
an automatically promoted reviewed skill, or proof that a model obeys it. The
owned-turn prompt and delegation checks validate delivery of the instructions;
the supervised goal must still supply observed target identity and outcome
evidence. A screenshot of an old running build cannot close that evidence gap.

## Context pressure and optional work

### An answer received before the App stops

An approved invocation can take longer to unwind than the two-second dispatch
readiness wait. The decision event then leaves its answer claim unspent. The
local Ask trial exposed a missing wakeup: the App stopped seven seconds after
the answer, but the cron leader was an identity node with no dispatcher, so its
sweep abandoned the approved run instead of resuming it. The previous regression
explicitly invoked an agent's recovery entry and did not cover that topology.

After the executor saves its suspended receipt, `workRunDispatcher.report`
freshly checks the same answer, owner-readable run, execution generation,
cancellation and absence of running steps, then re-offers the ordinary run
event. The existing same-answer claim and execution fences remain authoritative;
there is no new permission, generation, replay rule, lease or polling interval.
An unanswered wait remains parked, a later answer follows its usual event, and
a stale settlement cannot adopt a newer rerun. The stopped invocation's cancelled
context is detached only for this bounded metadata read, retaining its actor;
dispatch independently restores persisted owner authority again.

`TestWorkDBHumanSettlementWakesWithoutExecutingSweepLeader` reproduces abandonment
with two peer engines, an early real permission decision, a delayed nested
receipt and an actual non-executing sweep integration. It checks same-answer
dispatch without a manual agent sweep, stale-answer/generation refusal and
duplicate event/settlement suppression. Existing recovery tests retain coverage
for claimant death, lease takeover, cancellation and uncertain effects. This
repair does not rewrite the trial's already-abandoned execution or make its
manual recovery an uninterrupted run.

Prerequisite recovery preserves prepared work. The shared execution prompt
distinguishes an operation that could not start from a produced result that
failed validation. It asks for the specific missing setup information, input
freshness checks, partial-effect reconciliation and the normally authorized
repair, rather than a new source survey or regenerated artifact. Reads should
deduplicate resolved aliases and retrieve bounded relevant passages. This is
goal-independent model guidance, not a new automatic installer, filesystem
grant, or guarantee that the model will choose the cheapest recovery path.

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

Capacity pauses and provider-call ceilings share the native checkpoint decision
path. The pause writer emitted `context_capacity`, but its classifier recognized
only `model_call_ceiling`; Retry consequently created an ordinary rerun with a
new generation and empty override, and exact checkpoint validation refused it.
The classifier now recognizes both exact failure kinds. Owner/receipt checks,
override equality and execution lineage remain enforced; capacity decisions do
not renew progress limits or grant operation permission. The required-database
`TestContextCapacityDecisionPreservesCheckpointGenerationAcrossReplicas` follows
the actual pause writer through Retry and guidance on another engine, retaining
the Astra/xhigh override, generation, spent budget and exact completed exchange.
This prevents new invalid reruns; it does not rewrite a previously failed run's
history or prove that its downstream work has resumed.

Memory-only history can remain over budget after the last fresh exchange has
been summarized. Semantic selection deliberately refuses to re-summarize those
derived claims, but sharing that restriction with exact archival stranded an
otherwise shrinkable context. The storage selector now accepts eligible
memory-only spans, including at the semantic pass's no-candidate exit. It keeps
the same pinned authority, original/current requests, newest decision, pending
calls and fresh exchange. The existing eight-storage-attempt bound and strict
size-reduction check remain; each exact source is durable before its reference
is published. There is no new model call, enlarged window, permission or effect
replay. `TestWorkContextArchivesMemoryOnlyHistoryWithoutAnotherSummary` reproduces
the capacity refusal and verifies both retained archive layers on a second
database-backed engine, immutable continuation bytes and refusal at a hard
target that cannot fit required inputs. This is a synthetic reproduction, not
proof that the live continuation has completed its downstream verification.

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

Conversation recall reserves time for its exact-source fallback. Semantic
search and lazy historical backfill share at most ten seconds, or half the
caller's remaining deadline when shorter. An initial provider failure skips
backfill rather than retrying the same unavailable embedding path. This bounds
optional work without changing the embedding worker's memory guards.

After that phase, source validation and exact phrase search use the original
authorized context. Caller cancellation still fails the request; it is never
detached. The response explicitly reports semantic unavailability, and an empty
exact phrase result does not imply that no memory exists. It may require another
phrase or page. `TestRecallMemoryKeepsExactFallbackAfterEmbeddingTimeout` blocks
the semantic provider until its deadline and verifies that another engine still
reads the owner's original conversation from the database, then checks caller
cancellation. It does not prove semantic retrieval or indexing is available.

## Progress windows and result acceptance

Time and model-call estimates bound a work window, not a goal's lifetime.
`integrations/work/progress_continuation.go` renews only after substantive new
source facts, native capability contracts or a verified human answer, under
the shared run decision lock. The
old three-automatic/sixteen-total caps and fixed two-hour context deadline are
removed. Monetary limits, scoped permissions, cancellation, per-operation
watchdogs and the existing stalled-loop detector remain independent.

Recovery observes that same current cumulative window. `ModelCallBudget`
previously reloaded the original goal estimate after calculating the renewed
allowance, so a run beyond its first window appeared permanently exhausted to
compaction. This forced exact archival even when the current window could fund
a semantic checkpoint. The observation now uses the fresh progress-adjusted
allowance and unchanged reconciled usage. It cannot renew a window or admit a
call; provider caps, monetary limits and dispatch checks still apply. Unit and
cross-replica database regressions cover peer renewals, workload allowance
changes and usage retention without writes. This restores the compactor's
budget choice; it does not guarantee that a model follows archived evidence.

Counters never reset on renewal. A human-directed restart opens a new evidence
epoch; receipts older than that epoch cannot fund another window. Automatic
renewal does not advance that timestamp: doing so would discard receipts that
arrive during the read/write interval. Existing legacy cutoffs remain in force.
Receipt reads page beyond non-progress successes and consumed evidence, stopping
at a page with fresh credit. Each read has a 30-second watchdog and bounded page
memory; a failed page refuses renewal instead of asserting no progress.
Consumed facts have immutable, deterministic owned receipts;
only the latest 100 keys stay in the active index. The target window survives
a partial write, so a second replica can finish the update without reusing or
losing the credit. The textual source signal deliberately excludes opaque exec
output, polling and work notes; it does not certify task relevance or correctness.
Other effects need their own verified native evidence or scoped owner feedback.

Native agent and Apps/MCP source receipts use the same bounded source-content
parser. The delegated recorder derives its protected digest only from a registered
query's executed result, never from model arguments or submitted prose. It keeps
the existing exact source exchange for recovery. Failed results, work journals,
opaque commands and equivalent source facts cannot supply another credit.
`TestDelegatedSourceReadsRenewProgressAcrossReplicas` executes a museum-note read,
records it through the delegated path and renews on a peer; unchanged, failed and
argument-forged results refuse renewal. The MCP adapter's existing receipt test
covers dispatch forwarding into this recorder.

The provider boundary reserves actual attempts, including owned embeddings and
HTTP retries, under the same workload gate plus its admission gate. Its immutable
cumulative head is timestamp-fenced across peer clock skew. This replaces the
compiled 600-call lifetime backstop for governed work; an explicitly configured
call cap remains hard. Missing wiring, malformed accounting, foreign ownership,
closed goals and declared spending ceilings refuse admission. Old runs retain
their logical baseline and mark historical transport attempts unknown; they
cannot certify explicit hard caps or goal monetary limits without recovery.
Logical-answer reconciliation pages fresh receipts and removes confirmed entries
from its bounded pending index, rather than retaining every historical ID.
Provider admission reads that local logical total before taking either shared
gate; it never takes the local budget mutex while holding a gate. Logical
admission may hold that mutex while waiting to renew, so the reverse order would
deadlock. `TestProviderAdmissionDoesNotInvertConcurrentProgressLocks` forces that
overlap with a peer completion and verifies both admissions finish.

Provider admission also distinguishes execution from native work phases. A
`running`-only test incorrectly refused the compiler's first classification call:
the receiving planner had correctly claimed a run whose status was still
`compiling`. Widening that test to all waiting or finished runs would allow stale
contexts to infer after work stopped. The native entry paths instead attach a
private, call-local phase stamp after their existing admission gates:

| Phase | Fresh evidence checked under the workload gate | Stop condition |
| --- | --- | --- |
| Compilation | The claimed planner's recorded node and heartbeat still match the compiling run. | Cancellation, a changed owner/goal or compiler, or any status transition. |
| Replan or repair | The waiting run still names the exact remedy kind, step and `since` value claimed by the receiving planner. | A later wait, human approval wait, cancellation, status change or expired operation window. |
| Answer validation | The designated answer-check path has selected an exact succeeded run revision and completed step version; its configured cross-replica claim still bounds the call. | A changed run or step version, cancellation, or the original check deadline. |

Each entry path constructs its stamp in the call that uses it. No helper
returns a stamped context: `TestNoMethodReturnsAContext` enforces that boundary.
The stamp keeps its original phase identity and deadline; provider admission
still checks fresh run evidence.

Ordinary waiting and terminal contexts remain refused. Even if a stamped run
returns to `running`, its old compiler or remedy context cannot borrow execution
authority. The answer-check path preserves its existing behavior when no claimer
is configured; that does not let a caller manufacture the private phase stamp.
Owner authentication, goal ownership, current cancellation and cumulative
provider reservations remain mandatory for every phase.

The remedy's ten-minute context and phase expiry start **before** its
fifteen-minute claim is acquired. The answer check's fifteen-minute context and
expiry start before its claim and prompt preparation. Claim latency, scheduling
delay and rendering consume these original windows; they cannot create another
window or leave inference running after its deadline. These per-operation limits
do not close the goal or reset usage. Compilation has a different durable
ownership contract: its recorded heartbeat prevents a later claimant from
restarting the same compile after a lease expires.

The stamps are native context values, not DSL arguments or serialized grants.
The provider boundary checks them before Fleet transport. A receiving planner
builds its own stamp from its newly admitted operation, without relying on
intake-node memory. A restart must return through the existing claim and recovery
path; retained accounting receipts survive independently of the temporary stamp.

The required-database tests in
[`provider_phase_db_test.go`](../../../integrations/work/provider_phase_db_test.go)
cover `TestProviderAdmissionCompileCrossesReplicaBeforeExecution` (including the
original refusal), `TestProviderAdmissionRemedyIsBoundToClaimedWaitAcrossReplicas`,
`TestProviderAdmissionAnswerCheckBindsSucceededVersionAcrossReplicas`,
`TestProviderAdmissionRemedyClaimDelayConsumesOriginalWindow`, and
`TestProviderAdmissionAnswerCheckCannotExtendClaimDuringPromptPreparation`.
They verify admission, stale-context refusal and durable accounting through
separate engines. They use a provider-boundary probe, so they do not establish
live Fleet availability or successful installed-cluster rollout; those still
require a fresh end-to-end inference on the deployed revision.

Admission and progress-credit receipts also bypass optional observation indexing.
Their labels contain no reusable finding, and embedding one while its accounting
locks are held would recursively request provider admission and deadlock. This
does not bypass an embedding's permission or spending checks: no embedding is
requested. Exact owned receipts remain durable and queryable.
[`TestAccountingReceiptsPersistWithoutRecursiveEmbedding`](../../../integrations/work/accounting_embedding_db_test.go)
exercises both writes through a real engine with indexing registered;
`TestWorkObservationIndexesMeaningInsteadOfArchiveLabels` retains meaningful
checkpoint indexing while excluding both administrative receipt kinds.

Regression coverage includes
[`TestProgressDBWorkflowAcrossReplicas`](../../../integrations/work/progress_continuation_db_test.go)
(400 facts, replay after eviction, partial-write recovery, more than 100
non-progress successes and an arrival during renewal),
[`TestProviderAdmissionsReserveAcrossReplicasAndKeepHardLimits`](../../../integrations/work/provider_admission_db_test.go)
(concurrent admissions, skew, monetary limits and legacy uncertainty), and
the [logical accounting tests](../../../integrations/work/runceilings_union_test.go)
(5,000 receipts and refusal of regressed reads). Provider estimates are not billing
reconciliation; no test invents unavailable historical transport usage.

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

Failure Retry creates a new execution request but preserves the failed step's
latest admitted version override. Creating that request with an empty override
restored the compiled section prompt and default routing after an owner had
replaced them. The recovery path now copies the same step's instructions,
inputs, whole-prompt flag and model/effort; it does not borrow a neighboring
step's override, revive an older cleared override or restore a workspace
snapshot. `overrideRequestedAt` retains instruction chronology separately from
the new request's timestamp, so later owner corrections still take precedence.
Missing, malformed or future instruction times refuse recovery rather than
falling back to a bare run or assigning the retry's fresh timestamp.
The two-engine installed-builder regression covers failure decision through
resumed native input; unit tests cover empty, stale and different-step settings.
This does not make a new failure rerun an unchanged-checkpoint continuation or
grant permission to replay an operation.

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

## Package confirmation and connection loss (2026-10-25)

A production `0.31.2` confirmation was cancelled when its browser WebSocket
closed after a missed ping response. The BFF recorded `context canceled`
while closing the deployment and writing its audit; the heartbeat sweep later
recorded `deployment_abandoned`. The saved report and source snapshot survived.
This establishes request-lifetime coupling, not a proven WebSocket dispatch
concurrency defect. Background analysis already detached from the request;
confirmed execution did not.

The temporary bootstrap uses the existing Go SDK over native gRPC, under the
normal owner session, with a reviewed fresh parked-attempt receipt and one
exclusive durable confirmation intent. A lost response remains uncertain:
observe the exact deployment and never replay the call. The original temporary
operator's 8 top-level / 30 offline cases passed, including the actual SDK/protobuf result
boundary. That proof does not establish a successful live deployment or DSL
loading on every consumer.

The isolated correction is a **candidate, not released behavior**. It claims
the existing parked row under the package's database gate before acknowledging
background confirmation; concurrent synchronous confirmation shares that gate.
Repository confirmation requires the saved report and snapshot and refuses a
redirect to another attempt. Execution keeps the caller's authority, detaches
transport cancellation under a plan-derived time budget, and retains durable
Cancel, heartbeat and bounded terminal bookkeeping. Cancel and abandonment
checks re-read under the same gate. This is not a persistent job executor:
process loss, including during the DSL restart, can still abandon the attempt
before publication. No automatic continuation or effect replay is promised.

## Repository requests in owned Work (source candidate, 2026-10-25)

An owned goal can need a repository change while ordinary capability discovery
exposes functions without enumerating their assistant-tool aliases. The owned
Work scope therefore offers the already installed `inspectRepositoryAuthority`,
`requestRepositoryChange`, `repositoryChangeRequestStatus` and
`listRepositoryChangeRequests` tools. It adds each name once, only when its
actual tool contract is installed and the current actor matches the persisted
run owner with a goal. Ordinary chat, a foreign actor and a missing goal keep
their existing tool surface; an absent contract is not advertised.

This changes discoverability, not repository authority. The tools retain their
declared assistant/specialist role gates, and native requests still resolve
current source ownership, provider access and any required maintainer decision.
A host's owner or developer role alone cannot approve a foreign core repository. A
refused request follows the existing refusal/feedback path; adding its tool
name cannot manufacture an approval, replay an uncertain effect or reopen a
cancelled run. Restored owned Work reconstructs this scope from its persisted
identity on the execution replica, without borrowing an originating session.
Progress renewal, spending limits and explicit owner acceptance of goal results
remain unchanged.

`TestOwnedWorkOffersShippedRepositoryToolsWithTheirRoleGates` checks the actual
shipped contracts; `TestRepositoryWorkToolsRequireOwnedRunAndInstalledContracts`
checks the missing/foreign contexts. Existing persisted-owner and Fleet-lane
context tests cover the restored execution boundary. The live Ask-to-PR and
merge-queue journeys require separate verification against the installed release.

## Independent pipeline review restores its source owner

A scheduled PR review has no originating user session. Passing its synthetic
scheduler actor into the captured prompt leaves App routing with no person,
even though the review journal already names the authorized source owner.
`pipelineIndependentReviewContext` therefore rereads the active source binding
and matches its owner, repository and connection against the captured evidence,
then uses the existing persisted-owner resolver to load the actual local user
and current role. It preserves the call's existing origin, cancellation and run
accounting, carries verified forwarding authority to another replica, and clears
draft overrides through `repositoryIndependentReviewContext`. A prompt argument,
repository manifest or incoming actor cannot select that owner.

The driver still verifies the exact source, current provider grant, captured
policy and lease before the attempt and before recording/reporting its result.
An absent identity or removed/reassigned source stops before inference; a
provider failure leaves the ordinary attempt unresolved rather than replaying
it or inventing a verdict. Both full-file and complete-hunk prompts use the
same tool-free model seam and its existing admission, consent and spending
gates. No application permission or shared subscription session is created.

`TestPipelineIndependentReviewRestoresCurrentSourceOwner` exercises the real
captured-prompt seam with fake model output, separate database-backed engines,
no originating session, a scheduler and a foreign incoming actor. It checks
forwarded identity, fresh role changes, missing/changed source authority,
independent instructions and cancellation. The existing driver regression
`TestIndependentReviewRefusesLateAuthorityBeforePersistenceAndEffects` covers
late provider/policy refusal. These tests do not qualify a live App session or
a deployed PR review; those require installed-path verification.

## A pipeline run says why it failed outside its steps (2026-10-27)

A pipeline's workflow can fail after every step passed. On 2026-10-27 the
independent review above reached no model (`every_door_shut`) on memql PRs
6312, 6313 and 6318. The driver's verdict carried that reason to the work
run's journal only, so the required check read *Failed* over a stage table of
*Passed* and the MemQL OS run page said nothing more. The cause was found by
opening the Nexus work run.

The driver now builds such a verdict with `failedOutsideSteps`
(`component/pipelinerun/driver.go`). It covers a workflow error, a required
step blocked or never reported, an empty plan for a run whose work already
began or that re-runs only what failed, and a resumed plan that diverged after
every step had finished. In the
same write as the conclusion, `conclude` stores the code and the masked
sentence on the run row (`failureCode`, `failureMessage`, read as
`Run.Failure`). The final check run, recovery's republish on any replica and
the run page all read the reason from that row; none depends on a drive's
memory. A verdict that only restates a failed step (*Failed at ...*, or a
diverged resume that failed its unfinished steps with its message) writes
nothing, because the step rows already carry it.

Two existing places were rejected. The refusal fields mean the run ended
before any step ran: `Run.Refusal` makes the check run a refusal report with no
stage table, and the OS run list phrases an unknown refusal code as "the
repository could not be read". Notes, by contract, change no outcome, and the
run page draws them as warnings with note copy. The maintenance policy's
`failureCode` input still reads only `refusalCode`, so an installed policy
sees what it saw before.

`pipelines.CheckOutput` titles the run *Failed: \<reason\>* only when no step
failed; a failed step keeps the title. The summary lists the reason under
**Run failure** after the stage table, before any failed steps, within 2,000
runes (200 in the title), with the escaping and 65,535-byte field limits that
step messages get. The run page shows *The run failed outside its steps.* with
the reason as the notice's detail, and in place of "This run had nothing to
run." when no stage exists.

`TestAWorkflowThatFailsAfterEveryStepPassedSaysWhy` and
`TestARepublishedCheckRunStillSaysWhyTheRunFailed`
(`component/pipelinerun/run_failure_test.go`) drive a merge-group run whose
workflow fails after every step passed, through the real work journal. The
second republishes the final check run from a replica that never drove it.
The `TestCheckOutputOfARunThatFailed*` goldens pin the check copy;
`TestTheDSLStoreOverRealRows` round-trips the fields through the DSL on a real
database; the run page tests in
`clients/os/test/deployables/pipelines/runPage.test.tsx` cover the notice. A
live cluster run and a check run accepted by GitHub itself are not verified
here.

## Release trains carry main (2026-10-27)

Every pull request to this repository is opened under the owner's account, so
GitHub never lets one be approved: each merged through `merge-as-owner.sh`'s
admin bypass, which skips the merge queue, and every merge left the other open
pull requests behind `main` to run the whole pipeline again. The owner chose
trains (design record `docs/superpowers/specs/2026-10-27-release-trains-design.md`):
ready pull requests collect, a train tests them together once, and only the
owner's GitHub approval of the train pull request moves `main`. The connected
workflow is in [pipelines.md](../../public/operate/pipelines.md#release-trains).

**Trigger.** `tickPipelineTrains` runs every minute on the one agent replica
holding the `pipelines-trains` lease (`TestReleaseTrainsTickOnlyOnElectedAgent`):
an agent, because a retry opens the same failed-only re-run a person's re-run
opens, and only an agent's driver picks that up. `pipelinesTrainTick` is
internal-origin only (`TestTickRefusesAClientCaller`). A repository takes part
only when `MEMQL_PIPELINES_TRAINS` names it with `enabled: true`; a value that
does not parse runs no train anywhere and logs the field at fault, rather than
guessing at a typo every minute. An express train is
`pipelinesTrainExpress`, the pipeline owner's alone, under
`execute app:pipelines/rerun`.

**Evidence, and what it is not.** A pull request boards on its pipeline's own
pull request check, `pipelines.CheckRunName(name)`, completed `success` on its
current head, and on its labels, and only when its head is a branch of this
repository. Two independent guards, because anything that writes checks can
report one of that name, a fork's own workflow included. The check counts only
from the App that minted the train's token (`RepositoryTrainToken` answers its
id; `TestBoardingCountsOnlyTheAppsOwnCheck`), and a fork never boards
(`TestAForkNeverBoards`): its code would otherwise run in this repository's
train run with the pipeline's secrets, which the engine's refusal of fork runs
(`pipelines/trigger.go`, D6) exists to prevent. A branch in this repository
needs write access, which the repository already trusts. The token also reads
checks, which a private repository's installation token cannot without it.

A failed train run is judged only on its FAILED steps (a cancelled step proved
nothing), their error codes and Go packages; on each boarded pull request's
files at the BOARDED commit (`compare/<base>...<boarded head>`, 300 at most);
on what those files reach in the train head's import graph, through the
pipeline's own affected analysis (`pipelines.TrainReachOf`, mirroring the
shipped `pipelinePackageSelection` policy's global paths); and on `main`'s
newest completed push run's failed steps with their packages (`TestTrainReachOf`,
`TestADependencyMakesTwoSuspects`, `TestTriageReadsTheBoardedCommitsFiles`,
`TestACancelledStepNamesNoSuspect`). The approval is the latest `APPROVED` or
`CHANGES_REQUESTED` review by the pipeline owner's login, learned from their own
GitHub connection, and it counts only for the commit it names.

**The invariant: what lands is what the owner approved.**
`pipelines.ApprovalLands` requires that review to name the train pull
request's current head; landing requires `main` to still be the train's base;
and the merge sends that head as GitHub's guard. Any of them moving sends the
train back to `building` and the owner is asked again
(`TestRebuiltTrainAsksAgain`, `TestMainMovingRebuildsAWaitingTrain`,
`TestApprovalForAnOlderHeadDoesNotLand`, `TestAnApprovalBeforeARebuildNeverLandsTheRebuiltTrain`).
Nothing in the coordinator creates a review. That alone does not keep a model
from approving: agent sessions act on GitHub under the owner's account, so the
rest of the guarantee is the `CLAUDE.md` rule that a session never reviews or
approves a pull request. Boarding merges the head commit read at
boarding, not the branch, so a push after boarding never rides along
(`TestTrainMergesTheBoardedCommitNotTheNewHead`).

**Ownership and permissions.** Every GitHub call uses an installation token
minted through the pipeline owner's GitHub connection, and is refused if the
repository's installation changed since the pipeline was connected. The train
row is on the owner tier like every pipelines concept; its two mutations are
`@serverOnly`. The merge goes through the branch's normal rules with no bypass.

**Freshness and replicas.** The row is the state, not the process. Each tick
reads it with `memql.ContextWithFreshRead` and takes one step; every write is a
compare-and-swap on `(id, state, updatedAt)` under `TrainGateKey`'s Postgres
lock, with `updatedAt` written in nanoseconds and moved strictly forward as the
row's version. GitHub is never called under the gate. A build makes many GitHub
calls, so a `building` train younger than 10 minutes belongs to the tick that
claimed it and an older one is reclaimed and rebuilt from `main`
(`TestTwoTicksBuildOneTrain`). A replica lost between approval and merge
leaves the train `landing`, and the next tick merges it or records the merge
it finds (`TestLandingResumesAfterAReplicaChange`). Both race tests are
in-process: two ticks over one store, not a live two-replica cluster.

**Failure handling, in order** (`pipelines.TriageTrainFailure`,
`TestTriageTrainFailure`): every failed step environmental and not yet
retried, retry once; `main` failing one of the same steps, hold, because
dropping a pull request would blame its author for `main`'s break; the rebuild
cap reached, hold; exactly one suspect, drop it with a comment and
`train:dropped`, and rebuild; anything else, hold. Rejected: dropping the
newest or largest pull request when several are suspects, because a wrong drop
costs its author a day and blames them for someone else's break. Splitting a
train to isolate the cause needs a run limited to named steps, which the engine
cannot start, so it waits until after the switch. A held train waits for the
owner: closing its pull request cancels it, and `train:hold` on a pull request
on board, or `main` moving, rebuilds it.

**Retained and temporary.** The train rows are kept as the history of every
departure, a cancelled empty departure included: recording it stops the next
tick re-reading GitHub every minute until the next departure time, at the cost
of one row. The train branch is deleted on landing. A train's messages are
keyed `<event>@<headSha>` on the row and on the outbound delivery's dedupe
key, so a message staged twice by two replicas, or before a write failed, is
one delivery (`TestTrainMessagesAreSentOnce`); a message that cannot be sent is
logged and never stops the train.

**Admission.** A train run is the pull request run of a `train/` branch; the
admission file's optional `allowTrainRuns` admits it while `allowNewRuns`
holds other runs (`TestAdmissionAdmitsATrainRunUnderTheTrainAllowance`,
`TestAdmissionWithoutTheTrainFieldIsUnchanged`). The engine reads the field
before any guard emits it, because the reader refuses unknown fields and an
older engine would hold every run. A train run queued 15 minutes while
admission holds it sends the owner an ACTION NEEDED message.

**The bypass becomes break-glass.** `scripts/dev/merge-as-owner.sh` refuses to
merge without `--emergency=<why>` and posts the reason on the pull request
before merging; a reason it cannot post stops the merge, so the trail always
says why `main` moved outside a train.

**Not verified here.** No live train has run. Unverified until the first one:
that the App may create `train/*` branches and merge into them under this
repository's rulesets; that the train pull request's run starts from the App's
own pull request event or the poll; and that the owner's review of an
App-authored pull request satisfies `require_code_owner_review` together with
`require_extra_approval_for_unattributed_changes`. Verified on 2026-10-28:
GitHub marks a pull request merged when its head reaches `main` through
another pull request's merge (memql#6325 marked memql#6318 to memql#6324
merged).

## Pipeline capacity selection and spending holds

Only an explicit manifest `placement: pool` asks for shared capacity preference.
The driver's optional `computePlacement` role comes from the captured installed
delivery policy, evaluated with immutable run identity and no native operations.
The bounded result is copied into each step request and included in its existing
execution-definition fingerprint. If an authored clock expression changes that
answer on recovery, the journal refuses divergence before effects. An old snapshot or portable
default grants no cloud fallback; repository code cannot widen the operator's
policy. Dedicated native, image-build and privileged checkout paths keep their
own contracts. Current machine consent and resources remain independent gates.

Version 2 of the operator admission file separates a global new-run pause from
cloud spending. Cost holds can therefore leave eligible Fleet work running.
The optional release-train allowance still admits train runs under a global
hold, but never overrides an explicit version 2 `allowCloud:false`; that hold can
therefore leave an admitted train waiting for capacity
(`TestTrainAllowancePreservesIndependentCloudHold`; how a legacy decision treats
trains is in the release-train section below).
Workbench reads the live cloud decision again before each command or isolation
probe Job, including a later step in an already-started run. A held attempt saves
its exact queued definition before waiting; a new replica can recover that
positive evidence, while an uncertain dispatch remains uncertain. Existing Job
adoption, settlement and cancellation do not allocate and remain available.
Global waits precede the run clock; capacity and cloud waits after admission use
the existing run ceiling, without spending a command timeout before creation.

`TestSplitAdmissionKeepsCostAndOperatorHoldsSeparate` covers the versioned and
legacy admission boundary. `TestComputePlacementUsesCapturedPolicyAcrossReplicas`
and `TestComputePlacementRejectsUnboundedOrAmbiguousPolicy` cover captured source
and strict output bounds; `TestComputePlacementDecisionIsPartOfPoolRecoveryDefinition`
checks the recovery binding. `TestCloudAdmissionWaitSurvivesReplicaAndLateHold`,
`TestCloudAdmissionDoesNotBlockAdoptionOrCancellation` and
`TestCloudAdmissionRechecksIsolationProbeAllocation` cover the cloud effects.
These source regressions do not establish live Fleet availability, installed
policy activation or a completed Fleet-to-cloud CI journey; those require
separate verification against the selected deployment.

Fleet CI owns the host build lock exclusively. Local model calls share that
lock with persistent workspaces, serialize inference separately, and reserve
every workspace slot's full RAM cap. This keeps a goal's model/tool/model cycle
working without treating a live workspace as abandoned or charging host RAM
against a discrete GPU's VRAM. Unlocked retained workspace records still refuse
inference. `TestModelToolModelKeepsSameWorkbench` exercises both managers and
retained files; `TestInferenceReservesEveryWorkbenchSlotWithoutChargingVRAM`
covers the resource boundary.

A runtime's terminal response is separate from answer validity and delivery.
The activity witness clears after confirmed completion even when schema
validation or delivery fails, preserving that original error. Cancellation,
truncation and a reply lost before completion leave the witness fenced until
the time it records: the call's own deadline, at which its request is cut off
(that the runtime then stops is not verified; see below), or 30 minutes after it
began for a context with no deadline (no Cockpit limit bounds a local call, so the bound is chosen). A
fence only success cleared turned one routine cancel into a machine refusing
pipeline steps, workbenches and model calls until someone removed the file by
hand. Past its time the witness reads as absent; a reader that excludes every
writer (it holds `inference.lock`, or `pipeline.lock` exclusively) removes it,
and the workbench, which shares `pipeline.lock` with model calls, only ignores
it, because a path-based removal there could remove a record a call has just
written. The local build guard (`scripts/lib/host_build_guard.py`, which
`make dev` runs) reads the end by the same rule for every record the Cockpit
writes, and leaves a witness past its end in place: it is another program's
record, which the Cockpit's next exclusive reader removes
(`test_inference_witness_past_its_end_admits_a_build_and_is_kept`,
`test_inference_witness_with_no_time_ends_the_bound_after_it_was_written`). On
text no Cockpit writes, its parser and Go's can disagree. A witness with no time (an older Cockpit's, or a write that did not
complete) ends 30 minutes after its file was written, so an upgrade clears it; a
symlink or directory is never read or removed. The headroom checks still
measure what is actually free, so a generation still finishing after its
deadline is bounded by them. `TestManagerReleasesOnlyCompletedRuntimeErrors`
exercises real runtime responses and a second manager against the durable
witness; `TestACancelledCallFencesTheMachineUntilItsDeadline`,
`TestAnInferenceFenceEndsAtTheTimeItNames`,
`TestAnInferenceRecordWithNoTimeEndsTheBoundAfterItWasWritten`,
`TestPipelineCapacityWaitsForAnInferenceFenceUntilItsEnd` and
`TestWorkbenchIgnoresAnInferenceFencePastItsEndWithoutRemovingIt` cover the
expiry. Not verified: a live runtime that keeps generating past its caller's
deadline.

On macOS the pipeline headroom gate refuses a step for memory pressure only at
the kernel's critical level (4). At warn (2), which a busy Mac reads for long
stretches, the available-versus-reserve check decides, and a level the worker
does not know reads as pressure (`TestDarwinPressureBlocksOnlyAtCritical`). The
model-call admission keeps its own reading, in which warn is pressure: a local
call waits for headroom, up to its deadline, rather than being refused at once.
That reading is unchanged.

## Pipeline step attempts, retries and failover

A command step that fails for a transient reason is tried again, a bounded
number of times: in the same place, then on another eligible machine, then, for
a `pool` step whose installation allows cloud, on the cluster. Design record:
`docs/superpowers/specs/2026-10-28-pipeline-fleet-first-retries-failover-design.md`.
What owners and operators see is in
[Retries and failover](../../public/operate/pipelines.md#retries-and-failover)
and [the fleet](../../public/operate/pipelines-substrate.md#the-fleet).

**Trigger.** An attempt of a command step that did not succeed. The driver runs
the attempts (`runStep`, `component/pipelinerun/driver.go`). The steps it
delivers itself (a notification, a SARIF report, a repository change) never
reach the executor and are never retried; the manifest refuses `repeatable` on
the last two rather than ignore it.

**Evidence: the attempt's class and the row's history, nothing else.** The
executor classifies every attempt that did not succeed (`pipelines.AttemptClass`:
nothing ran, retry requested, outcome unknown, final), because only it knows
whether a dispatch reached a machine and what came back. It drops any class on a
runner's reply, and the driver reads an empty or unknown class as final, so a
replica of another version cannot make an ordinary failure retryable. "Nothing
ran" needs proof, since a wrong one runs a release step twice and a missed one
costs one retry. On the cluster almost nothing proves it: the runner reports
while the Job and its pod are alive, and the kubelet or the scheduler can still
start the command until the acknowledgement deletes them. The cluster's one
"nothing ran" is the agent's own `pipeline_runner_unavailable`, made before any
runner took the step and never on a recovery. On a machine the dispatcher's
`RefusedBeforeStart` verdict decides, and `pipeline_recovery_required` is an
unknown outcome because the Cockpit answers it after a command too. The codes
the engine itself makes when it refuses before sending (`worker_feature_disabled`,
`worker_feature_unavailable`, `pipelines_not_allowed`) are nothing ran only
beside that verdict: a reply carrying one unflagged takes the default class, so
another version's sibling cannot make an ordinary failure retryable. A refusal
that will not change within a retry's delay marks the machine not retried here:
Host commands off, pipelines no longer allowed, the machine's own policy or
container caps (`denied_by_policy`), and a reservation it cannot take
(`pipeline_capacity_unavailable`, which a local model call's fence answers until
its deadline). Exit 75 is a requested retry without `repeatable`, because the
command vouches; a timeout is final, because a hang and a stall look alike from
outside. The reasoning for
each code is in `integrations/pipelinesteps/attempt_class.go` and
`attempt_class_fleet.go`. The history is the row's `binding.attempts`; each
attempt's place is recomputed from the records before it (`placementOf`), never
stored.

**Chosen path.** The order is in the public page and in `chooseNext`'s comment
(`component/pipelinerun/attempts.go`), a pure function of the step, the run's
captured policy and the records, so every replica decides alike. Its less
obvious choices:

- After an unknown outcome on a machine, untried machines come before one return
  to it: only that machine's own reservation, refusing a second command, guards
  against two copies there.
- A repeatable `placement: cluster` step retries the cluster after an unknown
  outcome, up to `sameMachine` times, each after the previous attempt's Job,
  Secret and pods are confirmed gone. Otherwise it would get no retries, since
  nearly every cluster failure is an unknown outcome.
- A `pool` step reaches the cluster at most once, whatever ended its attempt
  there: the owner's cost bound on the one place that spends cloud.
- An unpinned failover answered nothing ran and not retried here, or naming no
  machine, means the fleet is exhausted for the rest of the step. The cluster
  rung comes next, never a pin on the last busy machine the wait named.
- A pool step's first attempt goes on to the cluster inside itself only when
  the fleet stayed full through `fleetWaitSeconds` and `cloudFallback` allows
  it: the fleet's own answer, which names no machine (`fleetPreferenceRanOut`).
  A refusal one machine made before start for another reason (its container
  caps, its policy, Host commands off), which the dispatcher verified
  (`RefusedBeforeStart`, not `RefusedByGate`), ends that attempt nothing ran,
  naming the machine, and the order decides: the same machine or another, then
  the cluster where `cloudFallback` allows, which covers a failed fleet as well
  as a full one. It was a final `pipeline_executor_error`, so a step declaring
  more than a machine's caps failed for good, while the same refusal on a later
  attempt read nothing ran. A refusal by the engine's own gate stays final.
- Waits, each at least a second because zero would mean no bound: a pinned
  machine at most the largest delay; a failover to another machine at most the
  largest delay too, or `fleetWaitSeconds` for a pool step whose policy allows
  the cloud and whose one cluster attempt is still ahead, because the cluster
  follows it; once that attempt is spent, the largest delay. A fleet step's
  failover used to
  wait until the run's ceiling, holding its stage's slot for hours where the
  step had failed at once, and a pool step's with no cloud one second, which
  ended the step on any busy fleet. No wait shortens a started command.
- A step whose last attempt only found the fleet exhausted settles with the
  failure before it (`failureToSettle`), so its receipt and the check run say
  three failed fetches, not `pipeline_no_machine_for_need`. The search stays in
  the history, and its own resources are acknowledged.
- Nothing widens: the executor refuses a place the step's placement or the
  captured `cloudFallback` does not allow (`refusePlacement`, a final
  `pipeline_step_invalid`).

**Alternatives rejected.** Retry loops in the DSL workflow: every installation's
workflow would change, recovery would rebuild loops over finished steps, and the
safety rules would still live in Go, so twice. Attempts inside the executor: the
journal would never see them, so a takeover would lose its place and the check
run could not show them. Accepting worker contracts 4 and 5 together:
`ActionContracts.Supports` is exact, so it would take a second requirement and
argument encoding per step kind, and the pre-release rule forbids compatibility
shims on Go wire contracts.

**Stop conditions.** `MaxStepAttempts` (8) whatever the policy says; the run's
ceiling (no wait begins into its last second, and no attempt starts with less
than a second left); the captured policy's limits; and `repeatable` for any
retry after an unknown outcome. A cancel during a wait settles the step
cancelled. A lost lease, a refused journal write, an unconfirmed cleanup or the
end of the workflow call running the step writes nothing more, and the row says
what comes next.

**The policy.** `pipelineComputePlacementPolicy` gains `retries`, and the driver
now reads the policy for every run with a command step (`selectComputePlacement`,
`readPlan`), because the limits apply to every step. So a policy phase that
errors refuses every such run, as `pipeline_stage_invalid` scoped
`computePlacement` and naming the field, rather than run it without the
operator's limits. A snapshot with no placement phase takes the portable
defaults (`sameMachine: 2`, `delaysSeconds: [30, 120]`, `otherMachines: true`,
no cloud): "no limits named" is the case defaults are for, and "no retries"
would let deleting a policy change how steps fail. Once a policy writes
`retries`, all three limits are required and a null is refused
(`parseRetryPolicy`), so the policy source always says how hard the installation
retries. The policy is captured with the run and bound into each step's recovery
definition, so no replica decides under other limits. Verification fingerprints
leave it out: evidence binds what a command computes, not where or how often it
ran.

**Invariants.** A command whose outcome is unknown runs again only when its step
is repeatable. `repeatable` means harmless to run again even while an earlier
copy may still be running elsewhere: a failover after an unknown outcome on a
machine does not wait for that copy to stop, because the machine is usually
unreachable, and a stop that cannot be proved would block the failovers that
matter. The earlier copy's late answer and artifacts are never filed. Between
attempts the row never reads finished, so the stage workflow and the check run,
which treat a failed step as final, see a running step.

**Ownership and consent.** Only the engine's pipeline purpose may pin or exclude
machines: `purposeRefusal` refuses either field on any other purpose
(`denied_pipeline_purpose`), so a model still cannot name a machine. Both narrow
the router's plan after its consent, contract and Host-commands checks and admit
nothing, and the replica receiving a forward re-checks all three. The router
skips a machine whose owner turned Host commands off (`HostCommandsOff`, also
true for a malformed setting, as admission reads it). One turned off after
planning refuses before send on the holding replica with the typed
`worker_feature_disabled`, locally or forwarded, and the dispatch moves on; an
agent's call does too, under its owner's fallback.

**What survives a replica change: the row.** Its attempt number,
`binding.attempts` (each record: attempt, class, code, message, surface,
workerId, machineName, nodeId, jobName, startedAt, finishedAt, logFileId and
notRetriedHere, every string masked) and `binding.nextAttemptAt`. The time is in
the binding, not the step's `resumeAt`, which the work spine's sweeps read for
parked work, and the binding is replaced whole on every write that names it, so
a receipt cannot keep a stale timer. A replica that takes the run over sends an
attempt with no record again as recover-only (on the cluster it adopts that Job
by name; on a machine the outcome is unknown, so a repeatable step continues and
any other fails `pipeline_execution_uncertain`), continues a recorded attempt at
the next once its resources are confirmed gone again and `nextAttemptAt` has
passed, and keeps a finished receipt. A history that does not lead to the row's
attempt diverges the run rather than guess.

**Retained and temporary.** Retained: each attempt's record, its own log file in
the Library, and for a machine attempt its own `v1:worker:invocation`, whose
`reroutedFrom` names the last machine it moved from. Temporary: the check run's
*Retrying* line, published once the outcome is durable, gone when the next
attempt's own outcome is recorded, and not restated by a takeover.

**Published names.** The check run is public on GitHub for a public repository,
so it names a machine only by the display name its owner chose in Fleet, else
the first eight characters of its registration id, never the name the machine
reported, which is usually its hostname. `pipelines.MachineWords`
(`component/pipelines/machine_words.go`) is the one place that rule is written;
the attempt lines and the dispatcher's and router's refusal sentences, which a
failed step quotes whole, all call it, and the `pipelines_not_allowed` sentence
takes its contract number from `PipelineStepContract`. The independent review's
failed model call reaches the run-failure line as a stable code and the
router's own words (`router.FailureReason`, for example *failed when called:
every_door_shut: no door to a model is open for this call*), never as its text
(`TestAFailedReviewModelCallIsReportedByItsCodeNotItsText`). The original is
logged at warn level, tagged with the pipeline, repository, pull request and
head commit, and the router's call ledger keeps the provider's own message. When Host commands off is the
only reason a machine cannot take pipeline steps, MemQL OS names it and points
at the Fleet switch, not at `policy.yaml`.

**Queue priority and scheduled scans.** Each attempt is a new Job with its
event's Kueue priority class, set when it is created and never changed, so an
installation's controller that promotes waiting Jobs near the run's deadline
promotes a retry's Job like any other. Scheduled scans take the same route:
nothing in placement or the retry order reads the event, apart from the
installed policy, which receives it.

**Exit 75 for a stalled VS Code download.** The editor host runners exit with
`exitCodeFor(error)` (`scripts/vscode/download.cjs`). It answers 75 only for a
failure marked as it leaves `downloadVSCode`: a stall (a 2-minute idle limit, at
most three attempts), a transport failure, or the 20-minute total time limit. A
bad version, a missing receipt, a failed host test and anything thrown elsewhere
stay 1. `scripts/ci/editor-host.sh` passes the status through. Known limitation:
the 20-minute limit asks for a retry only when it ends before the step's own
`timeout: 25m` (the editor-host steps in `memql-package.yaml`). Everything before
the download counts against those 25 minutes (the dependencies, two `npm ci`
runs and the compiles, uncached on the darwin steps, which declare `caches: []`),
so with more than about five minutes of setup the step times out first, and a
timeout is final. The stall path is unaffected.

**Rollout.** With no other change, the deploy turns retries on through the
portable default policy, and an installed policy that names no `retries` gets
the same values. Placement does not change until a manifest marks a step `pool`,
which follows the deploy because an older engine's strict manifest decoder
refuses the key. Worker contract 5 is exact, so the engine and every machine's
Cockpit switch together: operator hold; let in-flight runs finish; deploy the
engine; upgrade every Cockpit; release the hold and watch the first run,
holding again if it fails. A hold cannot let exactly one run through (an
allowance admits every queued run), so the check follows the release. Only the
old engine's hold stops nothing but new runs: under this engine a hold also
stops new cloud Jobs of runs already started (the workbench reads the run
allowance before every new Job), so holding again after the release leaves
those runs' cluster steps waiting under their ceiling until it is released.
The contract-5 Cockpit release is cut while production still runs the old
engine, because its macOS steps need today's machines. The engine change leaves
`cockpit/VERSION` as it is: the contract-5 Cockpit release bumps it in its own
release PR, as memql#6372 did, because the version belongs to the release, and
eligibility keys on the contract number, not the version. The manifest change
cannot move a `verification: true` step: reusable verification requires
`placement: cluster` (`component/pipelines/verification.go`). Twenty steps
carry it, the Go and database tests and the node builds and tests among them,
so moving those waits on an owner decision: give up evidence reuse for them, or
extend reusable verification to evidence a machine produced.

**Regression tests.** Classification (`integrations/pipelinesteps`):
`TestAttemptClassTable`, `TestFleetAttemptClassTable`,
`TestAClusterVerdictMadeWhileTheJobIsAliveIsAnUnknownOutcome`,
`TestAClusterRunnerUnavailableIsNothingRanOnlyWhenTheAgentMadeIt`,
`TestAMachinesRefusalEndsAPoolStepsFirstAttempt`,
`TestAPoolStepOverAMachinesCapsReachesTheCluster` (agent tag). Order, stops
and waits: `TestChooseNextFollowsTheRetryOrder`, `TestAFailingStepStopsAtTheEngineCap`,
`TestAFailoverWaitsTheLongestDelayWhenNoCloudFollows`,
`TestAPoolStepRefusedOverAMachinesCapsReachesTheCluster`,
`TestAStepThatOnlyFoundTheFleetFullEndsWithItsRealFailure`,
`TestRunCeilingStopsTheRetries`, `TestCancelDuringARetryWaitSettlesCancelled`,
`TestARepeatableClusterStepRetriesAnUnknownOutcomeOnTheCluster`,
`TestHeldCloudGateMakesTheCloudFailoverWait`,
`TestAPlacedFleetAttemptNeverSpillsToTheCluster`,
`TestACloudAttemptOfAPoolStepNeedsThePoliciesLeave`,
`TestPinnedOfflineMachineWaitIsBoundedToo`. Policy and manifest:
`TestRetriesParseStrictly`, `TestEveryRunGetsAPolicyEvenWithoutPoolSteps`,
`TestRetryPolicyIsPartOfTheRecoveryDefinition`, `TestRepeatableManifestKey`.
Journal and takeover: `TestStepAttemptsAreVersionsOfOneRow`,
`TestResumeBetweenAttemptsContinuesAtTheNextAttempt`,
`TestAResumedRetryConfirmsTheEndedAttemptsCleanupFirst` and, with
`MEMQL_REQUIRE_DB=1`, `TestATakeoverMidRetryResumesOverRealRows`,
`TestATakeoverOfAnInFlightFleetAttempt`,
`TestATakeoverAdoptsTheClusterJobOfTheCurrentAttempt`,
`TestBetweenAttemptsTheStepNeverReadsFinished`. Two agent replicas in process
(`integrations/agent/worker`): `TestPinnedRetryReachesTheSiblingHoldingTheMachine`,
`TestExcludedMachinesAreSkippedAcrossReplicas`,
`TestPinAndExcludeAreRefusedOutsideThePipelinePurpose`,
`TestHostCommandsOffIsSkippedByTheRouterForPipelines`,
`TestHostCommandsTurnedOffAfterPlanningMovesOnAcrossReplicas`,
`TestMidCallFeatureWithdrawalIsNeverRepicked`. What is published:
`TestTheCheckRunNeverPromisesARetryTheRowDoesNotHold`,
`TestAStepReportSaysRetryingOnlyWhileItIs`, `TestMachineWords`,
`TestARefusalNeverNamesAMachineByWhatItReported`, the run page and Compute tests
in `clients/os/test/deployables/pipelines/`, and the exit-75 cases in
`scripts/vscode/download.test.cjs`. Across the mesh, with `MEMQL_REQUIRE_DB=1`:
`TestAFleetStepRetriedOnTheAgentReplica` (`test/pipelinehop`: a push delivered
on the bff, the run driven on the agent under the policy the bff captured, a
first attempt that ran nothing retried on the same machine). Scheduled scans:
`TestAScheduledPoolStepGoesToTheFleet` (`integrations/pipelinesteps`, agent
tag).

**Known limitations.**

- A repeatable cluster step's earlier attempt can start after its runner
  reported and before its Job was deleted, so it may have run in part; the two
  never overlap. Follow-up: the runner stops the pod when it decides a failure
  before start, so those codes could read nothing ran.
- `pipeline_recovery_required` is one Cockpit code for before and after a
  command, so a non-repeatable step meeting a machine that needs recovery fails
  as it did before retries. Follow-up: split the code. Likewise a runner site
  that answers `pipeline_runner_unavailable` before any Job exists reads as
  unknown until it has a code of its own.
- An unknown outcome on the only eligible machine cannot fail over: a macOS step
  with one Mac allowing pipelines fails as it does without retries.
- A step waiting between attempts keeps its stage's slot, and each machine still
  runs one step at a time.
- A step wrongly marked repeatable can run twice, and a dishonest exit 75 is
  retried within the limits; review owns both.
- A failover to another machine waits for the check run's *Retrying* publish:
  one GitHub call, bounded at two minutes in an outage, and publishes go one at
  a time, so several steps retrying at once can queue behind each other.
- The model and app door texts (`fleet_inference.go`, `fleet_context_allocation.go`
  and `app_forward.go` in `integrations/agent/worker`) keep a machine's reported
  name, because they
  serve the owner's own screens, and words a machine's own Cockpit composes are
  quoted as they arrive.
- A wait for a busy machine that runs out marks that machine not retried for
  the rest of the step, although nothing ran there; a machine that frees up
  later is not tried again by that step.
- A failover onto a fleet that stays busy for longer than the longest retry
  delay (or `fleetWaitSeconds`, for a pool step the cloud follows) ends the
  step, or sends a pool step on to the cluster. That is never worse than no
  retry.
- After a takeover, a step whose attempt was in flight and whose secrets then
  fail to resolve settles without re-attaching to that attempt, so its Job N can
  go on running unacknowledged. It is bounded by the Job's
  `activeDeadlineSeconds` and `ttlSecondsAfterFinished`
  (`integrations/pipelinesteps/jobspec.go`): a leaked Job, not a second run.
- A pool step's attempt on a machine still asks the workbench to confirm its
  cluster cleanup (`AcknowledgeReceipt`, `integrations/pipelinesteps/receipt.go`),
  because a pool step does not require the fleet. With the workbench
  unreachable, the step stops the drive for recovery instead of retrying on the
  fleet. The step's final acknowledgement already depends on it the same way.
- A step settled with an earlier attempt's record (after a takeover, or after a
  last attempt that only found the fleet exhausted) reads failed even when that
  attempt was refused: the record keeps the attempt's class and code, not its
  outcome. Its code, words, place and log are that attempt's own; the check run
  says "failed" where the attempt said "refused".
- A pool step whose installation allows no cloud keeps waiting for the fleet,
  up to the run's ceiling, while no machine can take its first attempt, and
  that includes when the owner has no eligible machine at all. A fleet step's
  first attempt is answered at once in that case.

**Not verified.** These tests prove the source contract and in-process replica
hops. Live fleet-to-cloud failover and a production maintenance window for the
contract-5 switch are not verified.

## Release-train allowance across cloud allocation (2026-10-28)

The separate Fleet/cloud spending gate must preserve main's existing train
allowance. A legacy `allowNewRuns:false, allowTrainRuns:true` admits the train's
run, probe and cloud commands. Version 2 additionally requires `allowCloud`, so
an explicit spending hold wins even for a train. Ordinary runs do not inherit
the train allowance.

The driver forwards the immutable run event and head branch in `StepRequest`
and `StepRun`, independent of command environment. Execution and queued-effect
fingerprints bind both, so a replacement replica cannot widen a retained
attempt's admission class. Reusable verification excludes these run-only facts
because its command still receives neither event nor branch. The coordinated
workbench action is `pipelineStepV11`; older actions cannot silently discard
this identity.

A shared isolation proof retains each current waiter's class and cancellation
context, rechecking the live operator decision before every probe POST. A train
can fund that proof only while waiting; after cancellation, ordinary waiters
need their own allowance. A cached successful probe is only isolation evidence,
and every command rechecks its own spending gate after using it. Recovery of
existing Jobs and cleanup remain independent of new-allocation admission.

Regressions: `TestTrainAllowancePreservesIndependentCloudHold`,
`TestTrainAdmissionIdentityComesFromSavedRunAndBindsRecovery`,
`TestTrainCloudAdmissionSurvivesForwardAndFundsIsolation`,
`TestTrainCloudRecoveryBindsEventAndHeadBranch`, and
`TestIsolationProbeDropsCancelledTrainAllowance`. These exercise the source
contract and in-process replica hop; they do not establish that a running
installation has been upgraded.
