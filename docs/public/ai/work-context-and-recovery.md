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

If a plan failure exhausts automatic recovery, **Revise plan** authorizes one
further replanning attempt for the unfinished work. It preserves the completed
prefix and accumulated usage, and does not replenish automatic retries or
raise resource limits. A transient failure's **Retry** instead repeats the
failed step. Either decision is persisted for the cluster to resume safely.

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

A timed-out attempt records its failure with a separate,
short write deadline so that an expired model request cannot leave the same
attempt eligible to run again on another replica. This finalization does not
extend model execution; it rechecks the run's current state before writing.

## Browser research fallback

The document DSL first assesses whether feedback needs external evidence using
a local model. It prefers an available research app, assesses the returned
evidence, then uses local headless research for missing evidence or unavailable
apps. Particularly demanding requests select parallel app and headless research.
The `researchApps` policy tries Codex with a balanced model, then Claude Code
with Sonnet; each uses its own tools. Both reports reach the proposal stage,
which combines complementary findings and checks disagreements against primary
sources. Ordinary wording edits do not spend app quota.

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

Owned work uses the following layers:

1. **Keep authority and the request exact.** System messages, the original
   request, the newest human correction, and incomplete tool exchanges are
   not summarized. A complete assistant tool call and all of its results form
   one unit. A preference for recent messages cannot split that unit.
2. **Archive bulky results.** When the context is over budget, completed large
   tool results are stored in the owner-scoped journal before replacement
   with a short, explicitly incomplete preview and an exact reference. The
   call ID and tool name remain intact. This requires no summarization call.
   If several medium results collectively overflow the newest exchange, the
   engine also offloads them until the combined context fits.
3. **Checkpoint older complete exchanges.** The DSL checkpoint prompt retains
   facts, exact references, decisions, constraints, receipts, uncertainty,
   failed approaches and unfinished work. Older checkpoints can be
   consolidated with fresh evidence; their sources remain retrievable.
   A bounded excerpt of the original request and latest correction guides
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

The runtime bounds summarizer input and output, model calls per compaction,
and recall excerpts. A single exchange too large for the summarizer is
archived with a preview instead of overflowing the summarizer itself.
Summaries and retrieved sources are historical data, never new authority.
The source is saved before an active-context replacement is returned. Failed
storage, an invalid summary, or a checkpoint that saves no space returns a
failure without mutating the caller's original conversation.

The proactive working-set threshold is an estimate that includes message
envelopes, tool schemas and arguments. Its preference is 20,000 tokens. When
pinned authority, the original request, the latest human correction and tool
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

Archives and execution checkpoints live in shared persistent storage, under
the run owner's authority. Retrieval from another replica must not depend on
the first replica's memory. A reference grants no access to another owner's
data or another run's archive.

Active context and retrieval pages are bounded. The durable journal grows
with actual work; that is retained evidence, not an in-process cache. Retention
and storage capacity remain operational concerns. Individual calls and
recovery attempts retain finite time and resource budgets. A long-lived goal
must make progress through checkpointed steps and waits; it does not require
one unbounded model call or an open client connection.

A person can approve a budget pause with `decideApproval` and
`answer: {newLimit: 64}` (for example, a model-call limit). This raises only
the ceiling named by that approval; all other limits and accumulated usage
remain intact. The limit must be finite, above recorded usage, and cannot
lower a newer limit. Concurrent decisions on the same goal are serialized
across replicas. Omitting `newLimit` retains the separate-update workflow.

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
