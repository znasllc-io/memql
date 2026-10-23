---
title: Work context and recovery
audience: public
status: stable
area: ai
sinceVersion: 0.25.0
owner: znas
description: How long-running work retains evidence, compacts active context, and distinguishes task recovery from platform defects.
---

# Work context and recovery

A goal can outlive a model call, a context window, a client connection, or an
execution replica. Progress belongs in the durable work journal. A model's
active context is a bounded working set, not the sole record of the goal.

The current stage's execution instructions remain explicit when older evidence
is summarized, including after an approval or later guidance. Resuming work
restores those instructions from the executing workflow alongside the verified
saved context and new human answer. Completed tool results stay in order and
the original checkpoint remains unchanged. Retaining the instructions does not
grant new access or prove that the model followed them.

## Task failures and platform defects

These have different owners and recovery paths:

| Situation | Expected response |
| --- | --- |
| A PDF extractor reports that the document has no text layer | Use the failure as evidence, inspect the file, try an authorized OCR capability, verify the extracted result, and continue the goal. |
| A source is temporarily unavailable | Retry with backoff within the run's budget when the evidence supports a transient failure. |
| An approach or generated result fails its declared requirement | Repair the failed step or replan the remaining work, preserving completed effects and their receipts. |
| Required information, permission, or a substantive decision is missing | Ask a focused question or request the appropriate scoped approval; preserve progress while waiting. |
| Attempts repeat without progress, or the recovery budget is exhausted | Stop the loop and explain what was attempted, what remains, and what input could unblock it. |
| The engine violates an invariant, platform DSL is invalid, or an integration has an implementation defect | Preserve diagnostics and progress for a MemQL developer to fix. Do not blame the user's goal, silently change platform code, or retry the same defect indefinitely. |

A command's nonzero exit status is not, by itself, a reason to abandon a
goal or ask a human. The harness should analyze its evidence and continue
using capabilities already within the goal's authority. A missing executable
does not grant permission to install software; an access refusal does not
grant permission to obtain broader access. Recovery must still respect the
same authorization and side-effect gates as the original operation.

The current agent loops return tool failures to the model for bounded
self-correction. Their repeated-action and consecutive-error guards stop
unproductive loops. Failures that escape the loop reach the work spine's
deterministic classifier, then its model classifier when the rules have no
answer. The DSL selects retry, localized repair, replanning, or human input.
An environment repair that changes a reusable template still requires its
existing plan-review approval. There is no promise that every failure can be
recovered automatically or that a language model can identify every platform
bug correctly.

A worker model call that reaches its whole-call time limit before a complete
response is handled at the agent's reasoning boundary. The installed recovery
recipe permits one automatic retry of that model call, with the exact completed
tool exchanges retained. Each call still consumes the current model and time
allowances. A partial streamed response is not retried automatically. If recovery
is exhausted or the recipe requires a person, the run saves its checkpoint and
asks **Retry** or **Abandon**. Retry continues the same suspended child from that
checkpoint on any replica; it does not rewrite the workflow, replay completed
tool effects, grant permission or replenish a budget. **Discuss in Ask** can
supply guidance instead; the same checkpoint continues with that recorded
feedback in its context, without a structural replan or an access grant.
In-flight tool failures cannot enter this path. Missing or changed checkpoints and failures to save the
suspension stop execution. Older failed runs without native waiting child links
still require the existing explicit recovery controls; no links are inferred.

If a plan failure exhausts automatic recovery, **Revise plan** authorizes one
further replanning attempt for the unfinished work. It preserves the completed
prefix and accumulated usage, and does not replenish automatic retries or
raise resource limits. A transient failure's **Retry** instead repeats the
failed step. Either decision is persisted for the cluster to resume safely.

Clarifications can be optional when useful authorized work can proceed without
the answer. The pinned Spine policy allows 60 seconds to answer; the durable
two-minute sweep resumes the saved work on its next pass. The question remains
available while work continues. A late answer joins the active execution at a
safe model or tool boundary, without replaying completed operations. Required
information, access approvals, spending approvals and substantive decisions
still wait for an explicit answer. Silence supplies neither a fact nor consent.
Continuation preserves accumulated usage, resource limits and permissions.

The next useful step depends on the goal and available evidence. It can use
authorized concepts, profile facts, memory, retrieved sources, search, a direct
API, or permitted computer and app capabilities. A failed search provider does
not establish that every web route is unavailable. The agent should choose a
sufficient route within its remaining budget, avoid repeating unchanged failed
attempts, and explain any unresolved input or access requirement. A preference
such as “find it for me” does not grant access or override an explicit restriction.

A message you send in a conversation while one of its goals is preparing,
running or waiting can update that goal instead of starting a second one, when
the goal began in that conversation. You do not choose the goal: Ask checks the
message against the conversation's active goals first, and a message about none
of them is handled as a new request. The message is saved with the goal and read
at the next safe boundary: an operation already in flight can finish, but
unstarted calls from an older model response wait for reasoning with the new
guidance. A goal that is still preparing reads it at its first step. Completed
effects and their results remain intact. Received means a successful model call
used the message, not that the task is finished or that the model followed the
advice. Work started elsewhere, such as a goal begun in Nexus, can appear in a
private linked conversation but never reads guidance sent there; Ask shows that
message as not delivered, unless the work is waiting on a request, which can
still take it. How Ask shows where each message went is described in
[Messages while work runs](../operate/ask-and-voice.md#messages-while-work-runs).

Guidance stays with its goal for the rest of the run. Automatic retries,
repairs, re-plans and your own re-run of a step all keep it, and a message is
shown as not received only if its goal ends before reading it. Guidance still
queued when the goal pauses (for a permission, a limit or other work) stays
queued and is read when the work resumes.

A goal that is waiting on you takes the message through its request instead. A
question you were asked takes it as the answer. A permission request takes it as
a correction, and nothing is approved. A budget pause saves it until you raise
the limit. A request that takes no messages, such as a plan review, does not
receive it; open the request to answer it.

Each message is at most 4,000 characters. A longer message, or one with
attachments, goes through normal intake instead. Routed guidance has its own
budget of 16 messages and 16,000 characters per run, and the late answers to
optional questions described above have a separate budget of the same size. A
message past its budget is not delivered. Guidance does not grant permissions,
raise budgets, reset retries, or approve a waiting request; those stay with the
request's own decision controls. If a send is uncertain, retrying it recovers
the original outcome and delivers nothing twice.

**Discuss in Ask** opens the conversation that originated an approval. Work
delegated to a child run follows its owned parent records back to that
conversation. Work started elsewhere receives one private linked conversation. Opening the
discussion makes no model call and does not decide the approval. Human messages
and advisory replies remain in that conversation across reconnections and
replicas. You can add information while a reply is running.

Ordinary discussion and **Ask another agent** each start a separate, bounded
read-only analysis of that owned request: its recorded failure, selected work
receipt, original goal and recent linked discussion. The second opinion uses
a separate model call and is labelled as advice. Neither path inspects the host,
executes tools, grants permissions, approves the parent, or retries its blocked
action. Each child has one journalled model call and a two-minute ceiling; a
failed analysis ends with its error instead of creating a repair plan or another
approval. The original records are preserved.

For supported requests, **Send guidance** records your own direction and lets
the existing work reconsider its unfinished steps. It is an answered request,
not approval of the blocked action. The completed prefix, permissions, consumed
budget and retry limits remain in force. That direction is also projected into
the original Ask transcript, even when you send guidance without first asking
for advice. The saved message retains a link to its request after reload. If saving succeeds but continuing is interrupted,
retrying the same guidance resumes that saved decision; it does not create a
second answer. Artifact-specific reviews retain their own decision contracts.

For a typed computer-permission request raised inside a saved native turn,
guidance continues that exact checkpoint with the correction in context instead
of rewriting the workflow. The original request remains answered, not approved;
a corrected command or directory request still needs its own applicable
permission. Another replica rechecks the owner, question, execution lineage and
saved exchange before continuing. A missing or changed checkpoint is refused,
and a later failure request cannot borrow an earlier permission checkpoint.

An untyped computer-access request can also receive a correction in its saved
turn when the server recorded proof of the exact pending tool call at creation.
The execution replica checks that call, its original arguments and checkpoint
again before continuing. This proof grants no computer access. Older untyped
requests without that proof retain their existing replanning behavior; the
platform does not infer or backfill a continuation from their descriptions.

Replanning an existing sealed program uses structured, exact source edits so
that repairing a small part does not require the model to repeat a large
program. The runtime rejects ambiguous or overlapping edits and changes to
completed calls or their arguments. It compiles the resulting program and
validates the whole bundle before installation. An installed template without
a private authoring bundle is recovered from the receiving replica's DSL tree
only when its complete definition matches the run's recorded fingerprint.
Missing or changed source stops recovery before inference; journal previews
are never a substitute for the original program. Generated edits retain the
normal caller trust and approval gates.

The Spine also declares invocation substitutions that recovery must refuse.
For example, replacing `runAgentTurn` with `ai` for the same research prompt
would remove tool execution. The validator compares compiled calls, including
nested branches and loops, before persisting the replacement. This check does
not certify the factual quality of an evidence report.

A timed-out attempt records its failure with a separate,
short write deadline so that an expired model request cannot leave the same
attempt eligible to run again on another replica. This finalization does not
extend model execution; it rechecks the run's current state before writing.

A document image acquisition distinguishes an unusable remote resource from a
MemQL permission or storage failure. The DSL may receive a bounded refusal
receipt for a remote HTTP denial, catalog page, oversized or invalid raster.
It researches one alternative source and retries only that requested image,
with its acquisition mode unchanged. A second refusal enters the run's bounded
plan recovery; it cannot silently disappear from the document. Successful
assets and research receipts survive a resume on another replica. This does
not authorize bypassing a source's access restrictions.

Image acquisition intent is classified from human feedback and additional
direction alone. Old source passages and attached draft contents do not enter
that routing call. A mixed request to reuse one supplied image and acquire
another still requires acquisition; the later planner reuses the supplied
asset and plans only the missing one.

Image planning can also expose missing evidence before any download: a catalog
record without its associated raster URL is not a usable acquisition. Even if
prose research was sufficient, the document DSL performs one targeted research
pass for the plan's specific missing assets and one plan completion. Existing
verified entries and research are retained. The completed plan passes the same
native validation. Unresolved evidence stops before image effects or prose
editing, rather than sending a missing-asset problem to a text-only repair.
This bounded recovery uses the existing run budget and durable step receipts.
Source recovery receives only the selected image gaps and prior evidence, without
repeating document passages or reference drafts. It returns as soon as the source
record establishes the subject, exact raster link and reuse terms. Binary
acquisition and visual review belong to the subsequent stages; source research
must not spend its remaining context and calls probing images through text tools.
A source ledger does not claim that pixels have already been inspected.

A saved image is not a completed document edit. Revision proposals check actual
Markdown image nodes against the prepared asset manifest; prose links and code
examples do not count as embedded images. The DSL makes one corrective edit
attempt when requested assets are missing. Proposals also receive a separate
assessment of whether their actual text satisfies all the requested feedback,
including proposals that changed only part of it. Unfinished requests get the
same bounded repair, followed by another assessment. A second incomplete result
stops before approval rather than presenting partial work as complete. These
model assessments guide recovery; they do not replace human factual review.
The assessment uses the prose judgment level and treats the proposed document
as the source of truth for its contents. Original selections and quoted feedback
are context, not proof that rejected wording remains in the candidate.
Every assessment also receives the validated patch count, original line ranges
and feedback IDs. The full preview must retain unaffected sections; those
sections are not additional edits. Patch metadata establishes the proposed
scope, while the model still checks the requested content and evidence. Old
replacement text and the editor's reasons are not repeated in this scope
summary, and execution provenance is not inferred from document front matter.

Before an assessment's objections trigger an edit or block a repaired proposal,
the DSL requests one independent reassessment of those objections against the
same candidate. An exact quote proves that text exists, not that the objection
about it is correct. The prior findings remain untrusted review data. The new
assessment must still pass quote grounding; genuine unmet requirements still
require repair or stop before approval. A satisfied first assessment adds no
extra call. Reassessment is bounded and uses the run's existing model-call budget.

An unchanged request can reuse a completed image batch from an earlier finished
or stopped attempt. The native gate compares the captured source, feedback,
attachments and direction, rechecks owner access and immutable image bytes, and
preserves the original source/model attribution. Changed feedback does not
inherit that authority. Research and generation are not repeated on approval.

When a person modifies one proposed item, the new request captures the prior
proposal’s verified image receipts. Earlier research may still be reused, but
its image acquisition batch is not treated as a new acquisition for changed
feedback. This also applies when the parent proposal itself reused images from
a stopped attempt. Images retained in the revised Markdown
keep their exact bytes and provenance; removed images need not remain in the
manifest. Approval and application revalidate those retained assets on the
receiving replica. Supplying another request’s image identifiers or claiming
inheritance in a model response grants no authority. An amendment that repeats
the prior proposed text also receives the bounded completion assessment; changing
only its explanation does not count as fulfilling the new direction. A separate
assessment identifies specific unmet requirements before an amendment. Those
findings also guide the bounded repair, rather than repeating the editor’s
possibly incorrect claim of completion.

## Browser research fallback

The document DSL first assesses whether feedback needs external evidence using
a local model. It prefers an available research app, assesses the returned
evidence, then uses local headless research for missing evidence or unavailable
apps. Particularly demanding requests select parallel app and headless research.
The `researchApps` policy tries Codex with a balanced model, then Claude Code
with Sonnet; each uses its own tools. Both reports reach the proposal stage,
which combines complementary findings and checks disagreements against primary
sources. Ordinary wording edits do not spend app quota.

Selected source and human feedback travel together to the evidence stages.
Those stages do not receive a second copy of the entire document; a request
anchored at the end retains document context when it has no selected source.
The exact-edit stage still receives the complete original document.

App research is optional. Credit limits, missing apps, connection errors and
bounded timeouts are journaled under the failed branch call. They never cancel
headless research, stop the goal, or require a person to approve an unavailable
app. The proposal uses the evidence available and discloses unsupported work.
No metered provider is in either research chain. A genuine permission gate for
headless or desktop access remains mandatory. Explicit owner routing and step
overrides still take precedence.

A new attempt after a failed or cancelled document review can reuse the completed
app report from its immediately preceding request. Library binds that predecessor
only for the same owner, exact saved source and unchanged feedback; the DSL
reads the successful receipt. Changed feedback starts fresh research. A changed
automation definition still refuses in-place resume: this reuse does not bypass
the definition check or apply an old proposal.

Remaining subscription quota is not inferred from sign-in. A failed session
or bounded timeout remains an error with its attempt receipt. The chain may
continue while the parent run is active; cancellation of the parent stops it.
A session that timed out is closed before another attempt. These mechanisms
reuse the app session's existing permission, timeout and cleanup boundaries.

Research prefers available headless search and bounded source retrieval. When
search APIs are unavailable or disallowed, the agent can use public search
interfaces. If headless tools cannot obtain the evidence, the final fallback
is Computer Use on an eligible enrolled desktop in the cluster. This follows
the same routing policy, scoped approval and kill switch as other desktop
work; a connected machine does not grant access.

`workerStatus` reads the shared fleet, including workers connected to another
replica. `headlessOnline` and `computerUseOnline` distinguish host execution
from a worker that advertises desktop control and a display. Availability is
a snapshot; dispatch rechecks the route and permission before acting. A fleet
read error is reported as a failure, not converted to an offline answer.

The agent retains gathered sources, failed attempts and remaining questions
when switching tools or waiting for permission. Browser actions must follow
the observed interface. A search snippet or access challenge is not proof of
a source's claims. No fallback enables paid services, invents evidence or
broadens the goal's authority. If no eligible desktop is available, the run
reports the missing capability and preserves its progress.

## Keeping active context useful

Before repeating research, the shared work guidance asks the agent to consider
authorized retained sources, relevant prior results and reviewed knowledge.
Reuse depends on applicability and freshness: source revision, content hash,
validator or another appropriate check must still support the current question.
Refresh the missing or changed portion when that costs less than starting over.
An old summary, an unrelated result or an unavailable original is not proof.
This guidance supports reuse across goals; it does not guarantee that a model
will choose the same path, and it grants no access to another goal's records.

Owned work uses the following layers:

The proactive history budget counts a prepared current operation's exact
arguments, including when approval arrives after preparation. It also counts
returned results in an incomplete call group. These cannot be safely replaced
with a summary merely to meet a history preference. Completed result bodies
and older consumed calls still use the archival paths below. The selected
provider's actual context capacity and output reserve remain enforced; this
calculation does not grant permission or execute the approved operation.

1. **Keep authority and the request exact.** System messages, the current stage
   instruction, the original request, the newest human correction, and incomplete tool exchanges are
   not summarized. A complete assistant tool call and all of its results form
   one unit. A preference for recent messages cannot split that unit.
2. **Archive bulky results.** When the context is over budget, completed large
   tool results are stored in the owner-scoped journal before replacement
   with a short, explicitly incomplete preview and an exact reference. The
   call ID and tool name remain intact. This requires no summarization call.
   If several medium results collectively overflow the newest exchange, the
   engine also offloads them until the combined context fits.
   Before an Apps session asks for feedback, a public result larger than
   64 KiB is referenced from its existing journal receipt instead of copied
   into the continuation. The original bytes remain unchanged, and the
   reference includes their hash and exact recall coordinates. Small results
   and call arguments remain intact. This allows a sequence of screenshots
   to suspend without exhausting the 4 MiB retained-content limit; individual
   receipt and scan limits still apply. It does not upload a Library artifact
   or claim that the images were inspected.
3. **Checkpoint older complete exchanges.** The DSL checkpoint prompt retains
   facts, exact references, decisions, constraints, receipts, uncertainty,
   failed approaches and unfinished work. Older checkpoints can be
   consolidated with fresh evidence; their sources remain retrievable.
   Bounded excerpts of the current stage, original request and latest correction guide
   relevance without becoming evidence. This context shares the summarizer's
   input budget and is part of its cache identity. The DSL distinguishes
   retrieved metadata from verified findings and tool parameters from human
   constraints, so incidental output does not crowd out the remaining work.
   Semantic consolidation declares the strong model tier: preserving relevance
   and uncertainty through repeated checkpoints is more than classification.
   Source messages and relevance context share a 30,000-byte limit, allowing
   several bounded retrievals to be read together rather than reducing ordinary
   evidence to previews before consolidation. Provider routing still checks the
   full rendered input and reserves room for output.
   When parallel tool results together exceed the summarizer's input window,
   the engine archives the largest results first so the previous semantic
   checkpoint can still be consolidated with the new evidence.
   If the model returns an oversized checkpoint, the engine retains whole
   entries across these categories within its byte budget and labels the
   memory incomplete. It never truncates a claim or source URL to make it
   fit. Exact source messages are saved before the bounded memory replaces
   them, so verbosity alone does not stop the goal or spend another model call.
4. **Retrieve only what is needed.** `recallWorkHistory` searches a bounded
   page of archive records. An exact checkpoint and message index retrieve
   character pages of the original serialized message, including tool-call
   arguments. `nextOffset` continues that message; `cursor` continues a
   search. An empty page with `hasMore` does not mean the evidence is absent.
   When searching earlier tool receipts in the same owned conversation, one
   call scans at most eight four-receipt pages before returning its cursor.
   Search stops at a page with matches; directory browsing skips receipts
   without original sources. Derived retrieval responses stay in the journal
   but are excluded from the original-source projection, including when a
   capability wrapper performed the retrieval. Saved message indexes remain
   stable so older exact references still identify the same source.
   `matchLimitReached` means that some excerpts could not fit the response;
   narrow the search or read exact indexes from its listed source records.
   If a recalled page itself exceeds the active budget, the engine directs
   the next read to the same original source and offset with a smaller page.
   It never directs the model into another archive of that recall response.
   Exact checkpoint reads also return a bounded `sources[].messages` directory
   of message indexes, roles and tool calls. Derived checkpoints are labelled,
   so the researcher can select the original tool result instead of reopening
   an older summary. `nextMessageIndex` continues the directory; `nextOffset`
   continues the selected message without changing its serialized bytes.
   Combine `search` with a checkpoint and message index to jump to a relevant
   phrase inside that source. Omit `offset` for the initial match; an explicit
   offset still selects the exact page. This avoids scanning a long metadata
   record or bibliography to locate one fact.
   Searches compare decoded JSON string text, including newlines, quotes and
   literal backslashes, and map a match back to the unchanged serialized page.
   Complete JSON object or array envelopes inside strings are searched through
   at most four JSON layers (including the message), with an 8 MiB parsing
   budget per message. Escape-looking prose is not decoded. Searches do not
   span separate JSON values; a deeper or larger source may require exact
   message paging without a search. The raw archive is never rewritten.

The runtime bounds summarizer input and output, model calls per compaction,
and recall excerpts. A single exchange too large for the summarizer is
archived with a preview instead of overflowing the summarizer itself.
Summaries and retrieved sources are historical data, never new authority.
The source is saved before an active-context replacement is returned. Failed
storage or an invalid summary returns a failure without mutating the caller's
original conversation. If a checkpoint would be larger than its source, that
exchange stays exact while the engine tries another complete exchange. A short
older exchange before a pinned correction cannot block later tool evidence
from being compacted. All candidates share the same eight-attempt bound; if
the bounded semantic pass cannot make the context fit, the engine attempts
exact archival of the remaining completed results without another model call.
If small calls and previews still overflow the window, it can archive whole
older completed exchanges, including their arguments. This final storage pass
uses at most eight attempts, each limited to 4 MiB of exact source; these bytes
are never sent to a summarizer. It preserves the same protected requests,
authority and pending exchanges, and labels the replacement preview incomplete.
If the request still exceeds its budget, it fails and retains the originals.

Semantic summaries are cached by both exact source bytes and request relevance.
An unchanged pair can be reused on another replica without inference. New human
direction can require a new summary even when the source is unchanged; reusing
an answer focused on the old direction would be misleading. Exact archives
remain available independently of the semantic summary cache.

The proactive working-set threshold is an estimate that includes message
envelopes, tool schemas and arguments. Its preference is 20,000 tokens. When
pinned authority, the current stage, the original request, the latest human correction and tool
contracts would leave less than 4,000 tokens of working room, the target
becomes their estimated size plus that working room. Accumulated tool results, model replies and
derived memories do not enlarge that allowance. This lets a large document
reach an eligible model without deleting or summarizing its original request.
The inference router also sizes each actual call with completion headroom;
provider context-overflow recovery can request a strictly smaller checkpoint
without this proactive floor. An active request and tool contracts can
themselves exceed a provider's capacity. Compaction cannot guarantee that any
arbitrary input fits any model.

## Durability, resources and validation

When an Apps session returns evidence, MemQL associates the Library entry and
its backing file with the native work run; the file also records the executing
step. Model and effort remain what the app reported, which may be empty.
Ownership is checked before stamping, and evidence already associated with a
different producing run is not reassigned. An ordinary conversation without a
run preserves any existing work identity. This metadata helps find and reuse
evidence; it does not prove that its bytes were inspected or that a goal passed
verification. Older records are not automatically backfilled, and a stamping
failure is logged without discarding the completed app work.

Archives and execution checkpoints live in shared persistent storage, under
the run owner's authority. Retrieval from another replica must not depend on
the first replica's memory. A reference grants no access to another owner's
data or another run's archive.

Optional knowledge search and observation indexing each have a five-second
child deadline, honoring earlier cancellation or deadlines from the caller.
An unavailable embedding model must not hold a durable receipt indefinitely.
Administrative receipt labels are not indexed; meaningful checkpoint summaries
can be indexed. Failed indexing can reduce semantic discoverability until
backfill, while exact journal reads remain available. Required instruction
reads and source verification retain their own failure behavior.

Apps delegation keeps the same owner, run and permission boundaries. Before a
feedback or access request pauses work, the server saves the delegated native
context and the public tool exchanges it observed. The app's claimed narrative
does not establish completed effects. A native wait ends the app session and
resumes the same saved work after the appropriate recorded decision; ordinary
guidance does not approve a blocked operation. Empty workspace snapshots can
resume without filesystem restoration. Populated snapshots remain refused
where restoration is unsupported, rather than pretending their files survived.

Useful documents, scripts, patches, screenshots and verification receipts can
be retained as bounded artifacts with provenance. File metadata alone does not
prove that the bytes are readable. A checkout or dependency cache is a working
resource, not an automatic archival artifact; reuse it only when safe and worth
the cost. Resource reuse does not replay effects or inherit expired permissions.

When Apps calls the native computer screenshot tool through MCP, a validated
PNG or JPEG is delivered as image content, with its original metadata and a
content hash beside it. The delegated journal records that same response.
Base64 text alone does not establish that the model received a viewable image.
Unreadable, inconsistent or oversized captures remain explicit text evidence
with an image-delivery failure; the adapter does not retry capture, enlarge
computer access or bypass retention limits. Receiving pixels is separate from
inspecting the correct interface and retaining a readable evidence artifact.

Large arguments from completed older tool calls, such as a file transfer, can
be archived exactly before spending model calls to compress smaller history.
The active instruction, human requests, pending calls and latest exchange
remain protected. Archive references appear only after durable storage succeeds;
their short previews are incomplete evidence, not verified summaries.

Execution continuations preserve complete tool exchanges in an encoded JSON
receipt, including control characters that the storage layer cannot accept
as plain text. Restoration verifies the original content hash; existing
receipts remain readable. The workbench's `http_fetch` is a UTF-8 text surface:
binary responses return `unsupported_content` with source metadata, rather
than putting image or file bytes into model context. Workflows acquire binary
assets through their image or file capabilities.

A computer permission approved inside a synchronous child automation resumes
that saved child invocation. The engine verifies the owner, parent link,
definition, arguments and exact suspended tool receipt across replicas.
Completed parent and child steps stay complete; the approval grants no blanket
permission to repeat their effects. If an answer arrives before the waiting
receipt is saved, dispatch waits briefly without consuming its claim. The
recovery sweep can deliver the saved answer later under the same claim identity.
The existing stale-executor abandonment policy still applies after a claimant
stops responding.

On resumption, the model receives the recorded permission decision or feedback
alongside its saved exchanges. Permission context includes the approved scope
and any recorded use or revocation. The receipt is context, not authorization:
dispatch still checks the current permission and budget before an operation
can execute. It does not claim that an approved operation has already run.

Active context and retrieval pages are bounded. The durable journal grows
with actual work; that is retained evidence, not an in-process cache. Retention
and storage capacity remain operational concerns. Individual calls and
recovery attempts retain finite time and resource budgets. A long-lived goal
must make progress through checkpointed steps and waits; it does not require
one unbounded model call or an open client connection.

A goal has no fixed lifetime or automatic-extension count. Time and model-call
estimates are renewable work windows. Successful, previously uncredited tool
receipts can renew a window; heartbeats, elapsed time, repeated reads and model
claims cannot. The existing loop detector also pauses repeated stalled work.
When progress cannot justify renewal, MemQL preserves the work and asks for
direction. A fresh human answer renews a window on the same goal, preserving
usage and completed effects. Waiting for the person does not spend active time.
Spending limits, operation timeouts and access approvals remain separate controls.
See [LLM cost control](llm-cost-control.md) for the evidence and recovery bounds.

A run finishing is not acceptance of the goal's results. The goal stays open
until its owner explicitly accepts the reviewed result or chooses to close it.
The acceptance operation records the exact run revision, person, time and
optional feedback. It refuses stale results, live runs and agent self-acceptance.
A person may accept a useful partial result; an execution's success flag cannot
make that decision for them. Initial plan approval only authorizes execution.

The regression suite exercises parallel tool groups, pending calls, repeated
context pressure, exact Unicode/source reconstruction, cross-replica reads,
owner isolation, and storage or summarizer failure. Both agent transport
lanes exercise a failed PDF extraction followed by a successful alternative
without human escalation. These are bounded tests, not evidence of a
months-long leak-free soak or perfect semantic retention by every model.

## Research behind these choices

- [Anthropic: effective context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
  recommends a small, high-signal working context, external references,
  structured notes and compaction tuned against real task traces.
- [LangChain: context management for deep agents](https://www.langchain.com/blog/context-management-for-deepagents)
  describes staged offloading of large tool content before summarization.
  Its example thresholds are not universal values for smaller local models.
- [LangChain: autonomous context compression](https://www.langchain.com/blog/autonomous-context-compression)
  explores compression at useful task boundaries while retaining recoverable
  history. MemQL also enforces mechanical protocol boundaries independently
  of model judgment.
- [The Complexity Trap, JetBrains Research](https://arxiv.org/abs/2508.21433)
  compares observation masking and summarization on software-engineering
  tasks. Its results argue for measuring simple approaches before adding
  complexity; they do not prove one method best for research documents.
- [OpenAI compaction](https://developers.openai.com/api/docs/guides/compaction)
  provides provider-specific compacted state. MemQL's journal-based path
  remains portable to local models rather than depending on that format.

The practical target is enough active context to choose the next correct
action, with exact evidence available when needed. Reducing token count alone
is not a quality measure: verification must also check instruction retention,
source recovery, action receipts, final result quality and bounded recovery.
