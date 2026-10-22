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

| Decision | Reason and resulting path | Implementation |
| --- | --- | --- |
| Select a model and execution route | Use the current policy and requested reasoning level. An Apps route does not grant tools, host access or ownership. Missing app-session wiring is a platform fault, not permission to open another machine. | [App session provider](../../../component/memql/app_session_provider.go) |
| Admit a delegated tool call | Recheck current ownership, agent role, captured tool ceiling, expiry and run lifecycle. Reject cancelled, superseded or foreign work. Shared storage allows the receiving replica to make the same decision. | [MCP transport](../../../app/transport_mcp.go), [scope mutation guard](../../../component/memql/app_session_scope_guard.go) |
| Pause for information or access | Persist native context and exact server-observed public tool results before creating the request. Incomplete receipts or exceeded scan/byte bounds stop the pause path. App prose cannot certify an effect or reconstruct a continuation. | [Delegated source receipts](../../../component/memql/work_delegated_sources.go), [continuations](../../../component/memql/work_continuation.go) |
| Return after a native wait | Inspect the native control plane, including an answer that arrived before the app finished. Yield the old invocation without creating a second question or overwriting the resume. The adapter must expose this check to the agent loop. | [Human wait handling](../../../integrations/agent/human_wait.go), [production adapter](../../../app/adapters.go) |
| Resume filesystem state | An empty snapshot needs no restoration. A populated snapshot still requires supported restoration; do not substitute an empty workspace or replay completed effects. | [Automation resume](../../../component/automations/resume.go), [child continuation](../../../component/automations/child_continuation.go) |

`PrepareDelegatedFeedback` starts with the native context actually delegated,
including compacted memories and archive references. It appends only that app
session's observed exchanges in execution order and an unexecuted pending call.
It scans at most 16 pages of 16 receipts, with a 4 MiB exchange-content bound.
An individual source receipt exceeding 1 MiB records an explicit omission and
cannot be accepted as a complete continuation. These are refusal boundaries,
not permission to truncate evidence or silently reconstruct missing state.

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
