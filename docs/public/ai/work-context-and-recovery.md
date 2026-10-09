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
3. **Checkpoint older complete exchanges.** The DSL checkpoint prompt retains
   facts, exact references, decisions, constraints, receipts, uncertainty,
   failed approaches and unfinished work. Older checkpoints can be
   consolidated with fresh evidence; their sources remain retrievable.
4. **Retrieve only what is needed.** `recallWorkHistory` searches a bounded
   page of archive records. An exact checkpoint and message index retrieve
   character pages of the original serialized message, including tool-call
   arguments. `nextOffset` continues that message; `cursor` continues a
   search. An empty page with `hasMore` does not mean the evidence is absent.
   `matchLimitReached` means that some excerpts could not fit the response;
   narrow the search or read exact indexes from its listed source records.

The runtime bounds summarizer input and output, model calls per compaction,
and recall excerpts. A single exchange too large for the summarizer is
archived with a preview instead of overflowing the summarizer itself.
Summaries and retrieved sources are historical data, never new authority.
The source is saved before an active-context replacement is returned. Failed
storage, an invalid summary, or a checkpoint that saves no space returns a
failure without mutating the caller's original conversation.

The proactive working-set threshold is an estimate that includes message
envelopes, tool schemas and arguments. The inference router also sizes each
actual call with completion headroom; provider context-overflow recovery can
request a smaller checkpoint. An active request and tool contracts can
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
