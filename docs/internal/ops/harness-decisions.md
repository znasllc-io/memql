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

## A stored file is not a failed push (2026-10-30)

The Library's upload route waits a bounded time for `indexFileOnCreate` to
promote a file, then answers 201 with the file ID alone
(`waitForPromotion` in `component/server/artifact_handler.go`). Its reasoning is
that the bytes are durable and the artifact will appear. Cockpit read that same
answer as a failed push and failed the whole app session.

**Evidence.** On production on 2026-10-30, three of five Claude Code sessions
ended failed with exit code 0 and a result in hand, reporting "server returned no
artifact id", and Ask showed the person a failure. Measured from MemQL OS at the
time, the lookup behind the wait took 0.8 to 4.5 seconds against its 3-second
budget, so one slow read can spend the whole wait.

**Chosen path.** Cockpit treats a file ID alone as stored. The output manifest
records that file by its file ID. An answer naming neither ID proves nothing was
stored and remains a failed push, which keeps the workspace for recovery.

**Alternative rejected.** Only raising the server's wait needs an engine release
and still leaves a bound that a slow enough read can exceed, so a late index row
could still discard a finished answer.

**Invariant.** `produced_artifact_ids` names artifacts only, because the engine
stamps provenance on each one. A file ID never rides in it.

**Known limits.** A file whose artifact was late gets no produced-by provenance.
The slow lookup itself is not addressed here. Regression tests:
`TestSession_AStoredFileAwaitingItsArtifactDoesNotFailTheSession`,
`TestPush_AFileIdAloneIsAStoredFile`, `TestPush_AnAnswerNamingNothingIsAFailure`.
A real Library upload with a late index row has been exercised only through the
test server until this build runs on a fleet machine.

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

## A refused app call moves on only when nothing ran (2026-10-29)

Production, engine 0.31.9: Ask with routing set to apps failed twice on one
machine. Codex was out of credits and the call stopped there, then Claude Code
failed with `OAuth session expired`, as it had since 2026-10-24. The
[spec](../../superpowers/specs/2026-10-29-fleet-app-readiness-design.md) records
what each app really reports.

**The proof** (`appPreWorkRefusal`, `integrations/agent/worker/app_inference.go`).
A failed session may move to another app or machine only when nothing ran: no
served model, no structured result, no recorded or dropped action, no tokens, and
no prose except the refusal's own sentence. Anything else may have caused effects
and is returned as it is, because running it again would repeat them.
`usage.known` is not part of the proof. A Codex turn refused by its limit reports
no usage at all, which is why failover never fired for it. Stated tokens still
disprove it. The alternative, trusting the sentence alone, would replay a turn
that had already started tools. Tests:
`TestPreWorkRefusalRequiresNoModelWorkOrRecordedActions`,
`TestAppFallbackCrossesReplicaForARefusalReportingNoUsage`.

**Two refusals, because they are about different things**
(`appRefusalExclusion`, `component/memql/app_selection.go`). A usage limit is the
account's, so it rules the app out on every machine for the rest of the call:
trying each would open a session on each to hear the same answer. A login is one
machine's, so it rules out that machine's app and the same app elsewhere can still
answer. A pin stays its app either way, because the selector is asked for the
pinned app every time. When nothing is left, the caller gets the last refusal
rather than "no enabled app matches", which would send somebody to a setting when
their login has expired. The walk stops at 16 exclusions. Tests:
`TestAppRefusalExclusionMatchesWhatTheRefusalIsAbout`,
`TestPinnedAppNeverMovesToAnotherApp`,
`TestAppFallbackThatRunsOutKeepsTheLastRefusal`.

**The block is availability evidence, not permission to replay.**
`v1:worker:appSession.quotaRefused` and `authRefused` are stamped from the error's
words whatever the session did, and `appAvailabilityEvidence` reads the newest of
them and of a success, on any replica. A refusal blocks until the reset its
sentence names, and otherwise for a 15-minute recheck (`appRefusalUntil`). A login
has no reset, and a sign-in in a terminal writes no row, so the next call after
the window is what finds it. Test: `TestAppAuthRefusalDBBlocksOneMachineAcrossReplicas`.

**Codex's reset names no zone.** "try again at Nov 3rd, 2026 11:52 PM" is in the
machine's zone, which a cluster node does not know. Reading it as unknown blocked
an app out of credits for days for only 15 minutes, so every call met the refusal
again. It is now read as the earliest instant it can mean (UTC+14), so the block
never outlasts the real reset and can end up to a day early. A reset read as
already past takes the recheck, not no block. Test:
`TestAppRefusalUntilRechecksAResetThatHasAlreadyPassed`.

**Limits.** The classification reads each app's sentence, so a sentence neither
list knows is an ordinary failure: no failover and no block. The phrases were
measured on Claude Code 2.1.304 and codex-cli 0.162.1 against fabricated expired
logins, and on one real out-of-usage Codex. A real expired refresh token and
Claude Code's own usage-limit sentence were not reproduced here. Failing over a
signed-out Codex costs about 20 seconds of its own retries before the next app
starts; checking before routing is the follow-up that removes that wait. Neither
the failover nor the block has run against the production fleet.

## Pipeline AI runs local first (2026-10-29)

Owner brief, 2026-10-28: a pipeline's AI -- the independent PR review, a
failed run's diagnosis, the repository change a diagnosis requests -- runs on
the fleet's local models first, then a signed-in app, with a paid vendor as the
balance. On the production installation that day, the diagnosis of a failed
release parked on `inferenceUnavailable`: the app door refused, two local
models failed, an embedding model was tried as a chat model, and there was no
vendor to try. The [spec](../../superpowers/specs/2026-10-29-pipeline-ai-local-first-design.md)
records the evidence and the owner's decisions; this section maps the paths.

**A runtime that cannot compile the schema** (`cockpit/internal/worker/modelcall/ollama_schema.go`,
`ollamaSchemaRefused`). Ollama 0.33.3 answered both local models with
`400 Failed to initialize samplers: failed to parse grammar`; the llama.cpp in
Ollama 0.40.6 compiles the same schemas. A 400 to a `format` request with no
tools, whose error names the grammar, now takes the schema-in-prompt path the
501 already took: one retry on the same machine, the answer held to the whole
schema, otherwise the call fails. Any other 400 fails as before. Safe because
Ollama refuses before generating. The held-back answer reports runtime
progress, so the idle watchdog does not end a slow answer as a stall. Tests:
`TestStructuredCallTheRuntimeCannotCompileAsksInThePrompt`,
`TestStructuredCallKeepsOtherBadRequests`,
`TestTheHeldBackAnswerKeepsTheIdleClockAlive`. Takes effect only on machines
running a Cockpit release that carries it; upgrading Ollama removes the retry.

**A machine's failure keeps its code** (`component/worker/modelcall_failed.go`).
The stream's handle, the holder's forward response and the sender each kept a
failed call's machine code (`runtime_error`, ...) only as text, and the holder
replaced it with `model_call_failed`, so the decision record said `other`.
`ModelCallFailed` carries the code with each path's old text shape; the router
names `runtime_error`, `model_call_failed` and `worker_stopped` in its own words
and never puts the runtime's text in `considered`. `Code()` repeats a machine's
code only when it is a stable word (lowercase and underscores, at most forty),
because the decision line can be read by someone the machine was lent to;
anything else reads `model_call_failed`
(`TestAMachineCodeThatIsNotAStableWordIsNotRepeated`). The in-process loopback now
fails a call the way the real stream does. Tests:
`TestAFinishedCallWithAnErrorKeepsTheMachinesCode`,
`TestAMachinesFailureCodeCrossesTheModelHop` (planner to agent over the real
NodeService envelope), `TestAMachineFailureIsNamedOnTheDecisionLine`.

**Eligibility is decided per online machine** (`FleetModel.eligibleFor`,
`FleetMachine.serves`). The catalog's model-level flags are a union over every
registration, offline ones included, and one online machine's pre-#551 Cockpit
plus old offline registrations claimed tools and structured output for
`qwen3-embedding:0.6b`. A model is a candidate only through one online machine
that allows the purpose in its model setup and advertises every capability and
the context window itself (`fleetcatalog.Project` keeps each machine's
window). An embedding model never answers a non-embedding call, in the catalog
and in `ModelAttributes.Satisfies` (owner decision: "never", replacing
memql#6238's allowance for a model claiming both). Fixtures that set only the
union use `FleetModelAdvertisedOnEveryMachineForTest`. Tests:
`TestAnEmbedderIsNeverAChatCandidateEvenWhenAMachineClaimsTools`,
`TestACapabilityOnlyAnOfflineMachineClaimsServesNothing`,
`TestSetupAndCapabilityMustHoldOnTheSameMachine`,
`TestTheContextFloorIsTheServingMachinesWindow`,
`TestProjectKeepsEachMachinesWindow`, `TestAnEmbeddingModelAnswersEmbeddingCallsOnly`.

**A tool-free app turn in an Auto goal is a model call**
(`integrations/agent/worker/app_gate.go`, `toolFreeTurnWaivesDestination`).
Pipeline maintenance opens the diagnosis goal for the owner with no route
choice, so its destination is Auto, and the gate refused every app session in
it as a move to the computer. A chat-door turn with no images and no inputs
already runs the app with no tools. The destination refusal is waived for such
a turn only when the goal is Auto (owner decision: an explicit Workbench goal
keeps its refusal), and only on a machine whose app descriptor declares
`inferenceOnly` (`worker.proto`, set by the Cockpit for `claude-headless` and
`codex-app-server`, never `codex-mcp`; absent reads false). The agent's local
selection, its forward and the holder all judge the machine; the holder uses
its live descriptor; an agent that does not hold the stream reads the claim off
the registration row through `workerservice.AppDescriptorFromMap`, the one
decoder both row readers use (the routing store once dropped the key, which
refused every machine a sibling replica held). A planner or bff sender runs no
gate and leaves the judgment to the holder. Session-door steps and turns with images or inputs
keep the refusal. Tests: `TestAToolFreeTurnInAnAutoGoalRunsOnAMachineThatDeclaresIt`,
`TestAToolFreeTurnSkipsAMachineThatCannotRunWithoutTools`,
`TestAnAutoGoalStillRefusesATurnWithInputs`,
`TestAWorkbenchGoalStillRefusesAToolFreeTurn`,
`TestTheSessionDoorStillNeedsDestinationConsent`,
`TestAToolFreeTurnIsJudgedOnTheHoldersMachine` (hop; shown load-bearing by
disabling the holder's check), `TestAnAgentWithoutTheStreamJudgesTheRegistrationRow`
(hop with an agent sender reading the real row), `TestTheRoutingRowReaderKeepsInferenceOnly`.
App SELECTION (`SelectAppForCall`) still pins one machine by score without the
claim: during a Cockpit rollout an app door can be skipped in an Auto goal while
an updated machine is online; the route moves to its next entry.

**The pipeline rule** (`dsl/rules/rules.memql` `pipelineLocalFirst`,
`dsl/policies/policies.memql` `localFirstStrongest`, `app/work_call_tags.go`).
Calls carrying the `pipeline` tag at `reasoning` take fleet:strongest, app:*,
federation:strongest in the order written; Auto ranks only the three general
chains. The tag comes from the review's own call attribution
(`pipelineCallContext`) and from the run context of a work run whose goal has
`requestedVia: "pipeline"`, read off the goal row on the replica doing the work
(`integrations/work.GoalCallTags`) by the agent's dispatcher and by the
planner's remedy, whose reasoning-level replan would otherwise take
reasoningParks' app-first order; `ResolveFor` adds the run's tags to a request
built outside the engine. A goal that cannot be read routes by the ordinary
rules and is logged. A goal a pipeline run opens itself (`agent()`, a compose)
routes by its own origin. Tests: `TestAPipelineReasoningCallMatchesThePipelineRule`,
`TestAutoLeavesThePipelineChainInItsWrittenOrder`,
`TestAFullSizeReviewSkipsTheLocalDoor` (a 1 MiB review's 266,240-token floor
exceeds a 262,144-token local window), `TestCallTagsFromTheCallerAndTheRunReachTheRequest`,
`TestExecutionHopCarriesTheRunsCallTags`,
`TestAGoalThatCannotBeReadLeavesTheRunUntagged`,
`TestTheIndependentReviewCallIsTaggedPipeline`,
`TestAPipelineGoalsReplanCarriesThePipelineTag`,
`TestResolveForAppliesTheRunsCallTags`, and the shipped-rule gates in
`test/dslconformance/local_first_policies_test.go` and the OS routing tests.

**Not verified here.** No change ran on the production cluster or on a fleet
machine. The grammar failure was not reproduced on Ollama 0.33.3 itself. The
app waiver helps only machines on a Cockpit that declares `inferenceOnly`. The
production installation has no vendor configured, so the third door stays
empty until the owner configures one. The `inferenceUnavailable` sweep did not
re-try the parked diagnosis after its `resumeAt`; that is not addressed here.

## First pipeline PR qualification keeps approval on one attempt (2026-10-28)

Ordinary connect reads the default branch, so it cannot test a repository's
first native CI configuration before that configuration lands. A source owner
can instead preview an open, same-repository PR and qualify its exact head,
base and manifest digest with explicit compute permission, secret-name scope,
expiry and request identity. The typed builtin and generated SDK are the entry
point; there is no parallel HTTP action or private script granting execution.

The pipeline is created atomically in `qualification` state. That state reserves
the repository but remains outside automatic delivery, schedules, ordinary
manual runs and release builds. Its run carries create-only authority binding
the source, current owner and account, credential, provider repository and
installation, App identity, PR, execution policy and approved scope. Native
execution rechecks mutable authority before effects, including after restoring
the run on another replica. A later ordinary connect reads the default branch
again; qualification history cannot supply an ordinary delivery's policy or
deduplication result. Disconnect preserves the inactive origin as
`qualification_revoked`, including on repeated disconnect; generic disconnected
pipelines have manual-run behavior that a revoked qualification must not inherit.
Ordinary rerun cannot copy its approval.

The durable attempt exists before the first GitHub check creation request. An
uncertain response is recovered by observing the exact head, check name, App
and external run identity, across a bounded complete result set. Missing,
ambiguous or incomplete evidence cannot authorize another POST or establish
success. A crash between intent and the first POST can therefore leave an
unresolved attempt; cancel it before authorizing a new request ID. This trades
automatic recovery of that narrow window for an honest effect boundary.

`TestQualificationAuthoritySurvivesRealRowsAndReplacementDriver` exercises the
actual DSL rows, create-only scope and database gates with a replacement driver;
its provider and execution substrate remain fixtures.
`TestQualificationBuiltinsKeepAuthorityAndReceiptsAcrossEngines` checks typed
builtin dispatch, permission refusal, optional argument defaults and the actual
engine-to-protobuf JSON receipt through two engines with separate database
pools. It does not traverse a transport interceptor or a live SDK connection.
The qualification unit
suite covers exact-request retries, current authority refusals, ordinary-run
isolation and uncertain provider receipts. The GitHub adapter tests cover
pagination, duplicate receipts and incomplete observations, while the app
capture tests retain source-owner and PR head/base checks. These tests do not
qualify a deployed integration or any consumer repository. Live runs and
equivalent coverage must be verified separately. Required-check changes, merge,
release publication and controller deployment remain separate decisions.

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

## Failed-run diagnosis reads bounded log excerpts (2026-10-29)

A pipeline run that fails on its first attempt opens one owner-bound analysis
goal (`work.OpenAnalysisGoal`, key `pipeline-maintenance:<run id>`) running the
product recipe `znasDeliveryDiagnosis` (memql-znas). On production between
2026-10-26 and 2026-10-29 it never produced a diagnosis. Apart from the 18 runs
no model could serve (memql#6375), it refused 34 failures before any model saw
them: 19 had zero or several failed steps, 9 had no execution journal, 4 had a
step log over its 64 KB check, and 2 had a failed step with no log. Each refusal
was an `error(...)`. The failure path (`component/automations/failure_path.go`)
recognises none of those sentences, makes one `classifySymptom` call, and parks
the run on a Retry or Abandon question that no retry can change. 49 such
questions were open.

The evidence behind the change:
- `StepResult.LogCapped` says only that the live log-store copy hit its line
  cap. The Library archive is complete up to 64 MiB, so refusing a capped step
  as "truncated" was wrong.
- The archive is secret-masked when it is captured, on MemQL's own node for
  both cluster and fleet steps (`integrations/pipelinesteps/capture.go`). The
  check-run tail is cut from the same capture, so an excerpt of the archive is
  masked the way the tail is and needs no secret value.
- `libraryReadFile` refuses anything over 2 MiB.
- A body cannot call a builtin once per list item, so reading several logs
  needs a builtin that reads several files.

The engine adds `libraryReadFileExcerpts`
(`integrations/library/read_file_excerpts.go`, `excerpt.go`). It reads up to
eight of the caller's text files, each on `libraryReadFile`'s path
(`readableTextFile`), under one budget of at most 64 KiB, shared max-min fair in
ascending stored size. Each excerpt keeps the first lines, the earliest lines
matching the caller's RE2 patterns with three lines before and twelve after
while the window budget lasts (`matchedLines` counts them all), and the end of
the file, with every omitted range marked. The budget bounds the output,
markers included. A line is held to 64 KiB, matched on its first 4 KiB and
kept to 2 KiB, and at most 64 MiB is read. A file the caller cannot read, or
whose bytes fail, is its own `unavailable` entry. A database or gate failure
fails the call, so the failure path can retry it. The builtin masks nothing and
grants nothing: `label` is only echoed back.

Any signed-in caller can call the builtin with their own file and patterns, so
its cost is bounded natively, not by the recipe:
- patterns compile to at most 1,024 RE2 instructions together;
- every call has a two-minute scan deadline (`excerptScanTime`), and a file it
  stops is reported as not excerpted in time;
- the version gate covers the row reads only and is released before a file's
  bytes are opened. A version's bytes are never overwritten, so a scan does
  not hold a gate-pool connection.

The recipe reads the first three failed steps with logs, in `seq` order, as
48 KiB of excerpts. It lists every failed step and the run's own
`failureCode`/`failureMessage`, keeps the quote check, the independent review
and `repositoryChangeRequest`, and treats model answers as proposals only.
Every stop a retry cannot change returns `{repair: "none", reason}`: the run
succeeds, the answer validator skips goals a pipeline requested
(`integrations/work/validator.go`), and no classifier call or question
follows. The goal still waits for the owner to accept or close it.

Two stop conditions stay errors on purpose, because restoring the source or
run and retrying can change the outcome:
- a lost, inactive or mismatched source;
- a failed run that can no longer be read.

Rejected alternatives (design record
`docs/superpowers/specs/2026-10-29-failed-run-diagnosis-evidence-design.md`,
section 8):
- a pipeline-bound evidence builtin;
- the check-run tail persisted on step rows, which is only the last 40 lines;
- a one-file reader;
- a new terminal failure code;
- re-masking with re-resolved secrets.

Release order is a hard constraint. On 0.31.7 the recipe's
`use library.builtins.{ libraryReadFileExcerpts }` does not resolve and strict
boot refuses the whole bundle. So the recipe deploys only with an engine that
carries the builtin.

Tests:
- `TestExcerpt*` and `TestCompileExcerptPatternsRefusesBadInput`
  (`excerpt_test.go`) cover the algorithm. A seeded property test walks every
  excerpt back to its file line by line.
- `TestReadFileExcerpts*` cover malformed input refused before any read, the
  budget split (including a wrong stored size), and a foreign file reported
  without a fetch.
- `ReadFileExcerptsAcrossEngines` in `TestDocumentRevisionWorkflow` is the
  database-backed two-replica case.
- memql-znas's `check-delivery-policy.go` drives the recipe through
  `workflowhost.Run` and compiles every pattern.

Not verified here:
- a diagnosis on production, which also needs a model that answers;
- excerpt quality on real failing logs;
- Retry on a question the old recipe raised. It reruns the failed step under
  the installed recipe and keeps the old run's prefix.

## A step that ran out of memory says so (2026-10-29)

On 2026-10-29 the second attempt of the `cockpit/v0.21.0` release run failed
at `tests.go-tests#1` with exit 137 and no failure code. Only the kubelet's
termination reason tells an out-of-memory kill from a command that exited 137
itself, and it is written on the pod alone. `Classify` read it into
`Observation.Detail`, but `decision` copies only the status, exit code and
failure into the step's result, and the driver's sentence for an exit with no
failure is *The step's command exited with status 137.* By the time anyone
looked, the Job and its pod were deleted and no cluster event named the kill,
so whether the step ran out of memory could not be established.

`stepEnded` (`integrations/pipelinesteps/classify.go`) now fails a non-zero
exit the kubelet calls `OOMKilled` with `pipeline_step_memory_exceeded` and
keeps the exit code: 137 when the command itself was killed, the command's own
code when the kernel killed a process it started and the command exited
afterwards. The sentence names the memory limit the step container shows on
the pod -- its `memoryMiB`, or the LimitRange default the API server admitted
-- and names none when the pod shows none, never another container's. A zero
exit stays a pass. The failure takes the existing path: the observation
recorded on the Job (`memql.io/observation`), which an adopting replica
settles by; the step row's `errorCode` and `errorMessage`; the check run; and
the OS copy for the code.

Two alternatives were rejected. A field on `pl.StepResult` for the kubelet's
reason would have needed every reader -- the driver, the check run, the run
page -- to learn it, where a catalogued code already reaches all of them. A
note changes no outcome, and the step has failed. The code is
`ClassFailure`, like `pipeline_step_disk_exceeded`: the step's own doing. It
is not one of the release train's `EnvironmentalStepCodes`, so it earns no
automatic retry; whether an intermittent out-of-memory kill should is a
separate decision.

The out-of-memory rows of `TestClassify`,
`TestClassifyNamesTheMemoryLimitOnlyWhenThePodSaysIt`,
`TestClassifyNeverRepeatsTheStepsOwnTerminationMessage` and
`TestRunnerSaysAStepRanOutOfMemory` (`integrations/pipelinesteps`) cover the
classification, the limit, the step's termination message never repeated, and
the result and recorded observation through the runner's fake API server.
`TestEveryPipelineCodeIsCatalogued` and
`clients/os/test/deployables/refusals.test.ts` hold the catalogue copies
together. Not verified: a live cluster's out-of-memory kill through this
path, and the kubelet's `OOMKilled` for a killed child process (an exit other
than 137), which is a constructed case. A fleet machine reports no such
reason, so a fleet step killed for memory still reads as its exit code.

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

## A failed release re-runs only what failed (2026-10-28)

On 2026-10-28 the owner lost engine versions 0.31.4 to 0.31.7 to one-step
release failures. The last was an editor lane whose Chromium download stalled
on one fleet Mac. A failed release could not be recovered: the saved release
refused any run but the one it recorded ("native release observation changed
its execution identity"), and `releasePrepareCandidateWorkflow` refused the
`pipeline_passed_earlier` receipts a failed-only re-run carries. Each failure
therefore cost a version bump, a pull request check and a full release run of
about three hours. Design record:
`docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md`; the
connected workflow is in
[release-cutting.md](../../public/operate/release-cutting.md#recover-a-failed-build).

- **Trigger.** The owner re-runs a failed release attempt, usually its failed
  steps only, from the run page.
- **Planning** (`carryPassed`, `releaseChainFresh` in
  `component/pipelinerun/driver.go`). A release re-run never carries a step
  whose earlier result named artifacts, because a candidate pins artifacts only
  from the run it names. It carries nothing once the release's first attempt was
  opened more than 24 hours before the re-run (`releaseCarryWindow`, the
  evidence-reuse window). A read that does not answer stops the drive; a chain
  that cannot be followed carries nothing.
- **Attempt chain** (`releaseAttemptChain` in
  `component/pipelinerun/release_build.go`). Each observation reports the
  earlier attempts of the release run key, newest first, through `rerunOf`
  alone. A missing link, a different run key, an attempt that does not decrease
  or more than 50 attempts leaves the chain empty. Observation keeps working;
  nothing can adopt the attempt.
- **Adoption** (`adoptableBuildRun`, `integrations/release/build_adopt.go`). The
  saved release replaces its recorded run only with a descendant of a recorded
  attempt that completed without success, before any candidate or publication.
  The journal's SQL fence (`build_ledger.go`) admits exactly that change, so a
  replica holding an older view cannot write an earlier run back. A release
  blocked by a failed build keeps the `observe` action, which the OS release
  journey runs on every read; every other block keeps none.
- **Proof** (`carriedFrom`, `integrations/release/candidate_carried.go`).
  Coverage proves each carried step: the carrying run is a full release re-run,
  its chain stays inside the release and reaches attempt 1 within 24 hours, and
  the nearest earlier attempt that did not carry the step executed it as an
  artifact-free successful command (`pl.InspectCarriedReleaseReceipt`) with the
  same definition digest and packages. The recipe accepts
  `pipeline_passed_earlier` only together with that proof; anything unproven
  refuses the candidate, naming the step.

Rejected: a candidate whose evidence spans attempts (attempt 1's complete
evidence includes its failed step, its artifacts would be pinned from a failed
run, and the candidate format would change), and adopting only full re-runs
(saves the version bump and pull request check but not the long run; the design
adopts full re-runs too, which carry nothing). Cross-event reuse stays off for
releases (#6344): a receipt from a push, pull request or merge-group run has no
chain inside the release.

Compatibility: `RerunOf` and `Chain` are omitted when empty, so a run without
re-run facts encodes exactly as before. Journals written earlier keep their
selection digest, and an older engine still decodes every journal that never
adopted a re-run; one that did is unreadable to it after a rollback
(`TestReleaseBuildRunEncodingKeepsEarlierJournals`). The runner's carry rule is
a subset of what the proof accepts: a step that declares `artifacts` or an
image build, or whose earlier receipt named an intent or a file, always runs
again, and the runner stops at the proof's 50-attempt bound.

Invariants: a successful or running recorded attempt is never replaced; a
prepared, approved or published candidate is never re-pointed; every artifact
in a candidate comes from the run it names; a carried step is accepted only
with a native proof tied by digest to a step that executed within the same
release run key. What survives a retry or a replica change: the chain is read
natively on every observation, adoption is recorded in the journal under its
fence, and the proof is recomputed from journal rows at candidate preparation.

Tests: `TestCarriedReleaseReceipt*` and
`TestReleaseBuildRunEncodingKeepsEarlierJournals` (`component/pipelines`);
`TestReleaseAttemptChain*`, `TestCarryPassedReleaseRerunsArtifactProducers`,
`TestCarryPassedReleaseRerunsDeclaredProducers`,
`TestReleaseChainFreshStopsAtTheAttemptBound` and
`TestReleaseRerunCarriesNothingPastTheWindow` (`component/pipelinerun`);
`TestAdoptableBuildRun`, `TestReleaseBuildAdoptsAFailedOnlyRerunOfItsRun`
(including a refused stale write-back), `TestCarriedChain*`,
`TestCarriedReceipt*` and the proven-carry case of
`TestCandidateWorkflowChoosesAllowedPlannedSkips` (`integrations/release`, the
last ones db-gated). Not verified here: a live release re-run on a cluster.
This change helps only after it ships in an engine release; the release that
carries it is still prepared under the previous rules.

## A GitHub check per pipeline step (2026-10-28)

The owner asked to see each step of a run on a pull request, not one check for
the whole run, sectioned by stage (approved 2026-10-28). Every step now writes
its own check run beside the run's; the connected workflow is in
[pipelines.md](../../public/operate/pipelines.md#a-check-per-step).

**Names, and why they cannot gate anything.** A step check is named after its
run's own check plus `/ <stage> / <step>` (`pipelines.StepCheckName`): GitHub
lists check runs by name, which gives the sections. Push, release and manual
runs keep their run's suffix, because two runs of different events on one
commit select different stages and must not overwrite each other's steps.
Stage and step names cannot hold a slash, so the name always has more
segments than any context a ruleset, a release source
(`githubapp.releaseChecks`), the merge queue or a train's boarding asks for by
exact name; a step check can never satisfy or fail a required context
(`TestStepCheckName`).

**A writer per drive, off the run's path (the invariant).** A publish writes
the run's own check first and, only once that landed, hands the drive's writer
goroutine the token it minted and each step's newest state
(`offerStepChecks`). The writer sends one write at a time, a second apart,
keeping only each step's newest state, so neither the merge-gating check, a
step, nor the run row's conclusion -- which a release train waits on -- ever
waits on a step check. A step's check is created when the step first leaves
pending, not when the run opens: sixty creates at once would spend a third of
GitHub's per-minute secondary write limit, and several runs opening together
would exceed it. Rejected: creating every check queued at the start (the first
version, which the independent review measured at ~55 POSTs in one publish),
and writing inside the publish (it delayed the first step by tens of seconds
and the final row by up to the two-minute call budget). Tests:
`TestADrivenRunReportsACheckPerStep`, `TestAFailedStepsCheckShowsItsMaskedOutput`
(masked with the drive's secret values, like the run's check). The writer
composes from a copy of the run and pipeline that each offer hands it, never
from the drive's run row, which only the drive's goroutine touches: the race
detector caught a version that copied the row while the drive patched it. No
pipeline step runs this package under `-race`, so that check is a local
`go test -race ./component/pipelinerun/`.

**Rate limits and refusals.** A rate-limited answer pauses the writer for a
minute and keeps every state it still owes (`TestARateLimitedStepCheckPausesTheWriter`:
one limited create, then the rest). `githubapp` now also files a 403 carrying
`Retry-After`, or a message naming a secondary rate limit, as rate limiting
(`TestRateLimitIsRecognisedInBothSpellings`): before, such a 403 on a run's
own final write was recorded `refused` with the missing-permission note and
never retried, leaving a required check unfinished. A 403 creating a step check
is the missing permission and stops step checks for the drive
(`TestAMissingChecksPermissionStopsStepChecks`); a 403 moving a check the
writer adopted drops that one and creates the run's own. Anything else waits a
few seconds and is tried again; no failure fails the run
(`TestAStepCheckThatCannotBeWrittenNeverFailsTheRun`).

**At conclusion, and what survives.** The final publish hands the writer every
step's final state, a started step that never ended as cancelled; the drive
returns and the writer keeps trying for ten minutes, then exits. A drive that
ends without concluding stops its writer (`endStepChecks`), so no goroutine
outlives a drive that will not finish its steps. When GitHub took no completed
check at conclusion, recovery's republish of the run's own check also
completes the step checks the run left open (`settleStepChecks`,
`TestRecoverySettlesAConcludedRunsOpenStepChecks`). Step check ids live in the
writer's memory only; no row stores them.

**Steps that did not run here.** A failed-only re-run's carry
(`pipeline_passed_earlier` with no evidence run) writes no check: attempt 1's
pass stays on the commit (`TestARerunLeavesCarriedStepsChecksAlone`). A step
that reused another run's verification shows a pass naming that run
(`TestReusedEvidenceShowsAsAPass`). A drive that took a run over without
reading its plan again (`adoptRows`) now keeps the plan's skip, so a carried
step stays recognised across the hop (`TestATakeoverKeepsACarriedStepsPass`).

**Replicas.** A writer lists its run's existing step checks once, by external
id, the newest for each step, and moves them rather than creating a second
(`TestATakeoverAdoptsTheStepChecksAnotherReplicaCreated`, a real two-replica
hop). A writer whose drive lost its lease stops before its next write. A
listing that fails, or a commit with more than a thousand check runs (now an
error, not a silent short list), costs at most duplicates.

**Re-run on a step's check.** GitHub offers Re-run on every check an App
writes. `check_run.rerequested` for a step's check is resolved by its external
id at the re-requested commit: a failed run gets a failed-only re-run, a passed
one a whole re-run (`TestRerequestingAStepCheckRerunsItsRun`).

**Known limits.** A step check stays open when GitHub refused its final write
for the whole ten-minute window while the run's own check landed, or when the
agent restarted inside that window. A step that starts and settles between two
progress reports shows no *In progress* state. A manifest with several hundred
steps per event could push a busy commit past GitHub's thousand-run listings,
which release-source verification and merge-queue admission read; no cap is
enforced.

## Owner-scoped run reads cost what they return (2026-10-29)

On 2026-10-29 around 09:40Z, reads made as the cluster owner in MemQL OS hit
the eight-second statement limit (SQLSTATE 57014): `workRunsForGoal` twice and
`workRunningRunsForOwner` once. `workGoalForOwner`, `workApprovalsForOwner` and
`pipelineRunsForOwner` answered in the same minutes. The goal read is what
`cancelGoal` (Ask's stop, Nexus's close) and `workAcceptGoalResults` read
before they act; the running read was the computer-use kill switch's
selection. Neither failed for want of data: both read the owner's whole run
history to return a handful of rows.

**Why the history.** Every pipeline run is a `v1:work:run` owned by the
pipeline's owner, which in production is the cluster owner, and every step
boundary writes the run again. The run owner lookup index
(`memory_nodes_work_run_owner_lookup_idx`) was built for one person's runs
among many system runs; for this owner its range is the history. Two engine
behaviors met it (`component/memql/latest_scan_lookup.go`):

- The lookup ANDed every required string term into its id scan, so
  `goalId` -- which no index served -- was read from every version's TOASTed
  payload in that range.
- With two served terms ANDed, the planner cannot choose: PostgreSQL ignores a
  partial index's statistics when it estimates a payload equality, both
  ranges cost the default 0.5%, and the tie went to the owner index even after
  ANALYZE. A new index alone would not have fixed the goal read.

**Chosen path.** The lookup now ANDs only terms a lookup index serves, and
never a coarse one (`payloadLookup.coarse`, the run owner) beside a finer one;
every other term is decided once per candidate on its latest version. Migration
`20261029100000_work_run_goal_lookup_index` adds the goal lookup
(`memory_nodes_work_run_goal_lookup_idx`), so the goal read costs one goal's
runs. On a 9,600-version fixture the goal read went from 38,621 buffers to 32;
`workRunsForOwnerGoalSignature`, which ANDed `goalSignature` and `status` the
same way, from 38,615 to 27,117.

No index bounds "running now": it is a property of a run's latest version,
every run passes through it, and an index holds every version. An index-only
walk of every current run id was already measured past the limit cold in
production ([operational retention](operational-retention.md#bounded-current-run-recovery)).
The kill switch now reads the recovery projection, `work_run_heads`
(`runningRunsForOwner` in `integrations/work/recovery_heads.go`), and
`workRunningRunsForOwner`, which had no other caller, is deleted along with
its SDK methods.

**Freshness and replicas.** The kill switch does not refresh the projection:
`refresh_work_run_heads` claims the projector's state row with `NOWAIT`, so a
refreshing kill switch would fail whenever another replica's sweep was
projecting. It reads the heads at `running` plus every id in
`work_run_head_events`, then each candidate's latest version in the same
statement. A run written after its last projection is a candidate through its
event, so the answer matches a refreshed projection's. The projection lives in
the database, so any replica gives the same answer.

**Refusals.** The read refuses while the projection's backfill is unfinished,
while a capture trigger is missing or disabled, or while more than 10,000
events wait, because an empty list would read as "nothing to stop". The
readiness check and the read share one repeatable-read snapshot. A refusal
fails the kill switch visibly; the pre-dispatch gate refuses the owner's next
machine call either way.

**Ownership.** The read runs under the owner's borrowed authority, matches the
owner in both spellings in SQL and admits every row as fetched. It keeps the
DSL query's 500-run bound and its staged-data ruling is MUST-NOT-GATE: a staged
run that used the owner's machine must still be stopped.

**Regression evidence.** `TestWorkRunReadsDoNotScanTheOwnersHistory`
(`component/memql/work_run_goal_lookup_db_test.go`) runs the real queries as
the cluster owner over an owner-owns-everything history. It pins the lookup
SQL, the planned buffers and agreement with the concept-wide read.
`TestLatestScanKeysLookUpOnlyMandatorySetTermsOnALookupField` holds both lookup
rules on the rendered SQL without a database.
`TestRunningRunsForOwnerReadsTheProjectionNotTheHistory`
(`integrations/work/running_runs_db_test.go`) pins the answer, including runs
only a pending event names, row admission, the three refusals and a bound of
500 buffers over a 2,400-version history. The kill switch's selection tests
(`killswitch_test.go`) answer through the same seam. Both database tests need
`MEMQL_REQUIRE_DB=1`.

**The owner's feeds and the selective reads beside them.** A read narrowed by
the owner alone still enumerated the owner's whole range. It paid a heap fetch
per version of the owner's runs, because an expression index allows neither an
index-only scan nor TimescaleDB's SkipScan, plus one latest payload per run.
That covered `workRunSummariesForOwner` (Nexus's run list), `workRunsForOwner`
(the Training app and the procedure learner's pages), the procedure corpus read
(`workRunsForOwnerGoalSignature`) and the three Library revision reads. Two
mechanisms now bound them, chosen by measurement on 3,000 runs of 20 TOASTed
versions:

- **A newest-first walk** for a feed the owner term decides
  (`latestScanWalk`, `component/memql/latest_scan_walk.go`, over the new
  `memory_nodes_work_run_owner_walk_idx`). It walks the owner's versions in page
  order and keeps a version only when no newer one of its run exists, then stops
  at the page: 4,152 buffers for a page of 50 where the lookup took 97,252. A
  LATERAL `LIMIT 1` probe was chosen over `NOT EXISTS`, because TimescaleDB
  excludes chunks for it at run time (10,145 buffers otherwise). The cursor is
  pushed in as `createdAt <= t AND (createdAt < t OR id > i)`, because the
  engine's top-level `OR` sent the planner to a sort of the whole range
  (127,725 buffers for a deep page; 4,431 in this form). The walk is taken only
  when the filter, with the owner term and the concept assumed, is true for
  every row. A status or a signature can reject what the walk finds, and with
  nothing matching the walk cost 402,923 buffers, four times the lookup.
- **Partial lookup indexes for the selective reads**
  (`payloadLookup.partial`). `memory_nodes_work_run_signature_lookup_idx` holds
  only succeeded versions, one entry per finished run, and serves the corpus
  read because it requires `status == "succeeded"`.
  `memory_nodes_work_run_revision_lookup_idx` holds only
  `reviseLibraryDocument` runs and serves the revision reads, which name it.
  An index over every automation was rejected: a common automation, a
  pipeline's, would drive some other read with every owner's history. Measured:
  105 buffers through the signature index and an index-only scan through the
  revision index.

`TestWorkRunReadsDoNotScanTheOwnersHistory` pages the owner's feed to its end
on the real SQL, against the concept-wide read and its cursor, with no run
twice and the run the owner gave away gone. Pages 1, 2 and 12 cost 1,541 to
2,015 buffers over about 9,600 versions of history, against 27,117 before.
The test also holds the corpus read to 34 buffers and the revision read to 30.
`TestLatestScanWalkTakesOnlyReadsItsTermDecides` pins when the walk is taken,
without a database, and `TestPayloadWalksAreTheMigratedWalkIndexes` holds the
walk registry to the catalog. Each new index is about 100 bytes per run
version, except the two partial ones, which hold a slice. The owner lookup
index now serves only the owner reads the walk does not take; dropping it is a
later migration, once the walk has served production.

**Not verified, and not fixed.** Nothing here ran against production; it takes
effect in the engine release after 0.31.7. A page of the owner's feed still
visits each version of the runs on it, so a page costs its runs times their
versions. That is bounded, and no longer the history. `workSignedRunsForOwner`
(the procedure reuse sweep) stays on the owner lookup: its status term can
reject what a walk finds. The same sweep's `workAutomationStepsForOwner` and
`workGoalsForOwner` read concepts with no owner index at all, so bounding the
run read alone would not let the sweep finish.

## Pipeline AI is off until the owner turns it on (2026-10-29)

The owner decided that a pipeline should mostly run by itself: AI review and AI
failure diagnosis are off by default, with a switch for each in Pipelines >
Settings > AI. Connected workflow:
[pipelines.md](../../public/operate/pipelines.md#ai-review-and-failure-diagnosis-are-off-until-you-turn-them-on).

- **Where the switches live.** The global variable `MEMQL_PIPELINES_AI`
  (`pipelines.AISettingsVariable`, `component/pipelines/ai.go`), JSON
  `{"review": bool, "diagnosis": bool}`, like `MEMQL_PIPELINES_TRAINS`. The OS
  writes it through `setGlobalVariable` (row `var-global-memql-pipelines-ai`).
- **Read at use, not at run open** (`Deps.aiSettings`,
  `component/pipelinerun/ai_settings.go`). A captured-policy switch would make
  every flip refuse the reviews in flight ("installed independent review policy
  changed", the fence in `independent_review.go`), which cost three pull
  requests a full CI run each on 2026-10-29. Read at use, a flip applies to the
  next review or failure and never to one already started.
- **Off is all-off.** Absent, empty, unparseable, or carrying an unknown field
  reads as both off (`ParseAISettings`, `TestParseAISettingsIsOffUnlessTurnedOn`),
  and so does a node without a variable reader.
- **Review gate** (`runDriver.independentReview`). Off answers what a pipeline
  without a reviewer answers, nothing, so the reviewed workflow returns and the
  run is decided by its steps: no capture, no model, no `MemQL / review` report
  (`TestAIReviewOffLeavesTheRunToItsOwnChecks`, `TestAIReviewOnStillReviews`).
  On, the installed policy's opt-in still decides
  (`TestIndependentReviewNoOptInAndPublicArgumentsCannotStart`).
- **Diagnosis gate** (`reconcileMaintenance`). Off turns the policy's
  `analysis` into `none` with "AI failure diagnosis is off in Pipelines
  settings.", so no goal opens; the policy's own `none` and `human` stand
  (`TestAIDiagnosisOffOpensNoGoal`, `TestAIDiagnosisOffLeavesAHumanDispositionAlone`).
  Recovery's maintenance pass reads the switch again, so a run left `pending`
  while diagnosis was on is decided under the switch as it is when recovered.

The other review and maintenance tests run with both switches on
(`bindTestWorkflow`). Not verified here: the OS toggle against a live cluster,
and a train's pull request with review off (it is an ordinary pull-request run,
so the same gate applies). Known limit: a reader error reads as off and is not
reported on the run.

## A release records vulnerability findings and keeps going (2026-10-29)

The 0.31.8 release run (tag `v0.31.8`, `f19b3db57`) failed
`vulnerabilities.npm-vulnerabilities` on a moderate advisory for `vite` 6.4.4 in
`editors/browser`, published after its pull request's checks passed. The gate
fails on any advisory, a re-run cannot pass, and a release tag never moves, so
one advisory cost a new version and a full release run. The owner decided that
releases should flag findings, not fail on them.

- **Chosen path** (`memql-package.yaml`, the `vulnerabilities` stage). On
  `MEMQL_EVENT=release`, `go-vulnerabilities` turns govulncheck's exit 3
  (findings) into a warning, and `npm-vulnerabilities` runs
  `scripts/ci/npm-audit.py --report-only`. Both still run and keep their reports
  as artifacts, so a candidate pins the same evidence as before.
- **Invariant.** Only findings are relaxed. A scan that could not run or whose
  report cannot be read still fails the release (govulncheck's other exit codes;
  `--report-only` fails on an invalid report), so a broken scan can never look
  clean. Every other event still fails on findings, which keeps fixing an
  advisory a pull request's job.
- **Rejected for now.** Skipping the stage on releases: today's candidate
  workflow refuses a release whose steps were skipped for any reason but
  `pipeline_not_affected` or a proven `pipeline_passed_earlier`, so a skip would
  block the candidate rather than save time. Reusing the pull request's
  evidence in the release run is the larger saving (its tests repeat the PR's on
  the same tree), but release runs reuse nothing by design (#6344), and changing
  that is an engine decision of its own.
- **Tests.** `TestEngineSecurityScansDoNotTurnFindingsIntoSuccess` pins the
  release-only relaxation and that every other event still fails;
  `TestNPMAuditRejectsFindingsAndIncompleteReports` covers `--report-only`
  (findings pass, an unreadable report fails). Not verified here: a live release
  run with a finding.
