---
title: DSL boundary audit and implementation record
audience: internal
status: historical
area: design
sinceVersion: 0.25.0
owner: znas
---

# DSL boundary audit and implementation record

> Historical: implementation record for 0.25.0; kept for rationale.

October 6, 2026. The implementation moves confirmed workflow policy into the
installed DSL and retains mandatory execution invariants in native code. The
durable authoring contract is [Engine, integration and workflow boundaries](../../public/build/integration-boundary.md).
This record includes engine code, not only directories named integrations.

## Scope and evidence

Engine base: `a6ac38f89`. Product checkouts inspected read-only:
MemQL-ZNAS `88be5ac1b4fd977d1c903281ad6f4861eee9dc7f` and
MemQL-Fylo `225519fb6e615d2b52cbdcb6643783b7cf1ccb6d`.
The integration inventory used capability registrations and execution call-site
searches across all 34 implementation directories. Focused reads followed the
workflows listed below through their engine callers. This is an architectural
audit, not a claim that every source line received a security review.

## Implemented extractions

| Native entry point | DSL now owns | Native responsibilities retained |
| --- | --- | --- |
| `component/pipelinerun` | Stage sequence, bounded concurrency, failure blocking, notification exception; event mode/version and notification copy | Immutable compiled steps, worker authority, leases, secrets, receipts, recovery and final verdict from actual results |
| `integrations/release` | Candidate selection, validation/publication sequence, optional extension pin, completion record | Semver/tag parsing, exact commit/version checks, owner gate and remote effects |
| `integrations/knowledge` | Catalog and corpus data, seed tiers/recipes, ingestion and embedding loops, training prompt/tools, bridge composition | Extraction, chunk/vector operations, input identity and bounded model execution |
| `integrations/library` | Extract/summary/index sequence, status decisions, training and reviewed revision recipe | Row authority, exact revision approval, bytes, version locks and work receipts |
| `integrations/compose` | Recover/resolve/generate/render/file sequence and output naming | Run recovery, authorized source bytes, renderers, persisted artifacts |
| `integrations/groups` | Account group presentation defaults | Membership, verified-domain joins, bootstrap locks and archive/revocation invariants |
| `integrations/customdomain` | Which bindings/accounts/doors to reconcile and batch error policy | One binding's DNS proof, ownership, TLS readiness and reservation lifecycle |
| `integrations/planner` | Responsibility routing, cadence, cooldowns, convergence decisions and training refresh recipe | Owner-bound queries/effects, shared durable claims, bounded model calls, atomic directive append and work-goal port |
| `integrations/agents` | Factory catalog/order, corrective analysis attempts, match/extend/create choice | Model schema validation, cost ceiling, owner checks and locked catalog constraints |
| `integrations/router`, `component/router` | Evidence window/categories, proposal floor/thresholds, form, explanation and review sequence | Aggregation arithmetic, single rule renderer, exact source hash and approval enforcement |
| `component/memql` | Policy/rule proposal catalog and prompt composition | Configuration author gate, catalog facts, schema/dry validation and exact revision binding |
| `component/campaigns` | Drain phase order, per-page selection and continuation, warmup increase/reduction decisions | Consent, suppression, immutable reviewed snapshots, rate ceiling, delivery receipts and atomic schedule cursors |
| `app/` scheduler wiring | Node and lease placement alongside the DSL schedule | Role validation, shared leader leases and owner-scoped authored scheduling |

The scoped host uses the existing interpreter; it introduces no alternate
language. Its callbacks are local closures under existing authority. It
preflights transitive children, refuses detached work and independent journals,
and preserves cancellation, actor and run context. The pipeline host additionally
pins workflow/action definitions and deduplicates repeated step invocations.

The language extension is `@trigger(schedule=..., node=..., lease=...)` for
installed schedules. User-authored activation refuses installation placement.
The durable claim table gained an expiry column so six-hour workflow claims are
not erased by the ordinary one-hour pruning policy. Knowledge refresh fields
are optional schema additions; existing rows remain writable.

## Remaining native integration inventory

These are classifications of the inspected responsibilities, not deferred
workflow migrations. The extracted entries above account for every confirmed
violation found during this audit.

| Directories | Evidence for retaining native implementation |
| --- | --- |
| `agent`, `agentdef` | Provider streams, bounded tool-loop interpreter, generation-definition projection, cancellation and session transport; authored prompt bodies and tool declarations remain DSL |
| `auth`, `identity`, `rbac` | Authentication, delegation and rank/row governance must apply even to a replacement workflow |
| `azureblob`, `openai`, `stt`, `email` | Vendor protocols, byte streams, sender receipts and bounded delivery; campaign recipient/phase decisions live above these operations |
| `database`, `worktrace` | Health/statistics and observability recording |
| `deployversion`, `timeutil`, `voice` | Semver/timezone calculations and vendor voice catalog facts; caller chooses the release or voice |
| `embedding`, `similarity`, `harnessrecall` | One provider/vector/search operation plus authorization and ranking; knowledge/library own batching and corpus recipes |
| `fileprocessor` | Format extraction and binary codecs |
| `pipelinesteps`, `workbench`, `sitepreview` | Authorized command/preview execution, filesystem/resource isolation, process lifetime, forwarding and cleanup |
| `procedure` | Replay fingerprints and exact approval/evidence state transitions; the shipped procedure-promotion automation owns the event workflow |
| `skills` | Verification and transfer of exact approved capability bytes to the selected execution surface, owner/target checks and receipts |
| `shopify` | Generated schema mirror, webhook validation and external synchronization protocol; storefront product decisions remain in pack/product DSL |
| `work` | Goal/run claims, admission, dispatch, approvals, journals and recovery; these are runtime capabilities despite their registration location |

Engine reads also covered `workTemplate` validation, work-context compaction,
seed materialization and the symptom-classifier seam. These interpret or
validate authored data, preserve bounded context, or perform one structured
classification. They do not choose a release, training or campaign recipe.
Compiler-generated email-rule DSL remains a compiler responsibility. Package
installation retains dependency resolution and atomic lifecycle enforcement.

## Product findings

Neither inspected product contains an implementation `integrations/`, `bff/`
or `packs/` tree. Fylo's application-arrival notification and
approval-to-entitlement behavior are already in `dsl/fylo/automations.memql`.
ZNAS's product behavior is in `dsl/znas`; its capabilities call the shared
engine. No product Go workflow was left for a later migration.

ZNAS's database-health monitor remains outside MemQL because it must report
when the engine's backing database is unavailable. Its configured thresholds
and Kubernetes observations are instance operations, not an engine workflow
that can survive the same outage. The pinned deployment observer remains until
the delivery replacement is applied and rehearsed; changing shared template
assets belongs in the template repository, not divergent product copies.
Both products' release promotion scripts validate and pin exact digests into
a lockfile/overlay. That is one mechanical effect; release selection and target
choice belong to the caller's workflow.

The existing knowledge catalog write surface is intentionally absent in an
engine-only bundle (`dsl/knowledge/concepts.memql` records why). Moving its
existing seed recipe/data to DSL does not silently activate hundreds of seed
rows or paid training at boot. The host reports unresolved required operations;
this change adds no automatic catalog bootstrap trigger.

## Verification contract and application

Regression coverage includes an independently named pipeline recipe and a
composed default, refusal of uncompiled steps/unscoped effects, repeated-action
receipt deduplication, changed-definition recovery refusal, native adapter
behavior and DSL interpreter conformance. Database tests exercise pipeline
recovery and claim pruning; planner tests exercise a second host sharing durable
claims but no local state. Scoped tests cover required arguments, child
preflight, preconditions, typed errors, cancellation and preserved run authority.

Applying the reviewed branch and repeating the other delivery chat's live
cluster rehearsal remains an application step. Local unit/database results do
not claim those running images contain this change.

The research comparison was the separation of
[workflow definitions](https://docs.temporal.io/workflow-definition) from
[activity definitions](https://docs.temporal.io/activity-definition): workflow
policy versus bounded effects and recovery obligations. No additional
orchestration service was introduced.
