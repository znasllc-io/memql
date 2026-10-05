---
title: MemQL self-hosted delivery implementation
audience: internal
status: draft
area: planning
sinceVersion: 0.24.0
owner: znas
---

# MemQL self-hosted delivery

This plan supersedes conflicting implementation choices in the October 4
delivery plan. The owner authorized implementation and local testing on
October 5, 2026. Public publication and production deployment require a later
approval of the exact candidate. Do not retire working CI before parity is
measured. Do not add Azure capacity for this work.

## Boundaries

- Ship the default CI/CD workflow as sealed core DSL. Clients configure it,
  call its public constructs, or compose separately named workflows. The
  template must not copy the core workflow into each client repository.
- Keep orchestration decisions in DSL; integrations provide bounded effects.
  Change the runtime only for missing general primitives or correctness.
  Audit queries, mutations, specifications, logic and automations while
  implementing the workflow. Record each missing primitive with a non-CI
  use case and an independent regression test. Do not put CI policy in Go
  merely because the present DSL cannot express it.
- Keep independent component versions in one coordinated release record.
  Approval binds commits, artifact digests, evidence and targets; changed
  inputs invalidate it. One approval covers public publication and deployment.
- Keep client values in the instance repository. The template supplies generic
  consumption conventions and upgrade tests.
- Use existing cluster and opted-in Cockpit compute. Portable builds use an
  explicit container contract on either surface; native builds require the
  appropriate host. Preserve repository consent and reserve capacity across
  replicas and across the clusters a worker serves. Queue bounded work when
  suitable capacity is busy. Never infer Docker health from an old label.

## Implementation sequence

- [x] Recover the delivery branch against current main; preserve both histories.
- [x] Resolve merge conflicts and pass compiler, driver, outbound and verifier
  baseline suites (database-backed cases still require separate verification).
- [x] Protect server-authored run/configuration records through a general DSL
  contract; verify direct writes cannot forge evidence (#5822).
- [ ] Restrict inbound/outbound row visibility and verify every legitimate
  reader, writer and subscription (#5802, #5804).
- [ ] Complete runner readiness, egress isolation proof, service-account
  isolation, capacity and recovery cases (#5803, #5810, #5811, #5812, #5821,
  #5823; check newer issue state before implementation).
- [ ] Finish channel management, false-green evidence and rollout verification
  in the existing OS surfaces (#5504, #5505, #5506).
- [ ] Refactor default workflow policy into public core DSL with an explicit
  selection/configuration boundary and a second workflow proving composition.
- [ ] Add candidate preparation, compatible component versions, owner approval,
  artifact provenance and idempotent release/deployment receipts.
- [ ] Reproduce all necessary build/test/security/docs/release lanes for engine,
  Cockpit, SDK, editors, template and instance; classify obsolete lanes with
  evidence before removing them.
- [ ] Update template and instance consumption/configuration, docs and diagrams;
  reconcile already-resolved issues with merged evidence.
- [ ] Test real local cluster execution, multi-replica ownership and recovery,
  Cockpit native/container execution, candidate approval and local rollout.
- [ ] Review the rendered OS in Chrome with desktop/narrow and light/dark
  states. Label fixtures and simulated external services explicitly.
- [ ] Present a working local demonstration and remaining production-only
  checks. Preserve all production gates pending owner review.

## Failure and resource cases

Exercise duplicate/reordered events, cancelled and superseded runs, lost
worker/agent/workbench connections, lease expiry with a surviving old process,
restart during publication, expired credentials, unavailable images, exhausted
disk/CPU/memory, stale hardware reports, occupied capacity, revoked consent,
partial artifact upload, failed health verification and failed rollback.
Confirmed effects are adopted from durable receipts; uncertain publication is
reconciled against the destination before retry. No absence of evidence means
success. Build capacity must leave room for the serving installation.

Recovery is a per-step contract. Reconcile a disconnected worker before
reassigning its work; fence expired attempts so a returning laptop cannot
publish results for its replacement. Use bounded retry budgets and persist
the reason a run stops. Retain completed steps only when their pinned inputs
and durable artifacts still match. A compiler normally restarts; checkpointed
tasks or uploads may resume when their capability supports it. Offer a safe
user resume after exhausted retries without silently repeating uncertain
side effects. Nexus redesign is outside this work.

## Evidence boundaries

Local integration tests may replace GitHub delivery and public publication
endpoints with explicit fixtures, while exercising the real engine, database,
worker and isolated local rollout. They do not prove cloud identity federation,
registry/marketplace permission, native signing, public webhook reachability,
cloud storage behavior or production recovery. Those remain named rehearsal
steps. Time-window milestones #5506–#5509 stay open until observed.

## Fleet repository routing progress

Capability descriptors now carry generic, action-specific repository scopes.
Pipeline selection uses them before dispatch; an older or malformed scope is
unknown consent. Cockpit advertises its live allow flag and repository list as
one policy snapshot and re-registers when either changes. The command retains
its independent live-policy check. Cross-replica tests cover skipping a narrow
machine for an eligible one, refusal before dispatch when none accepts the
repository, and missing advertisements. This requires a coordinated Cockpit
upgrade; no existing machine policy has been changed.

## Queued routing progress

The generic workbench router now reports the actual selected replica before
sending a long-running request. Pipeline status prefers that route, with the
existing healthy-peer fallback. This avoids treating another replica's absent
local queue as lost work. An in-process hop through real forwarded handlers
and two real runners verifies one forward for a healthy queue, recovery after
restart or peer loss, and one eventual Kubernetes Job. The API server is a
fixture in these cases; this is not a live two-pod claim. Partitions can still
leave an old request alive, requiring the existing shared-Job deduplication.

## Queued Secret retention progress

Jobs and Secrets now carry the agent's absolute run deadline. Orphan cleanup
uses that deadline plus the outcome TTL, so a shorter workbench ceiling cannot
remove credentials while a step is still queued. Tests cover different agent
and workbench limits, the exact expiry boundary, and bounded cleanup of old or
malformed metadata (#5823).

## Workbench identity progress

The workbench now has `memql-engine-workbench`; only that subject receives the
pipeline Job/Secret Role. Existing custom-domain and package-roll operations
retain explicit grants. Overlay and federation-shape tests pass. A disposable
local namespace test asked the real API server about Job creation and Secret
reads: workbench allowed, shared engine/identity/step accounts denied. Cloud
activation still requires vendor trust preflight. OpenAI's current one-mapping
rule requires an exact two-subject CEL allowlist in the existing mapping, not
a second mapping for the same provider/account pair. The runbooks record the
order; no external trust or deployed workload has been changed.

## Isolation proof progress

The runner now requires a positive connection to the same listener used by
its restricted connector. Both share the listener ingress rule; only the
control has a narrow egress exception. Unit, real shell and rendered-overlay
suites pass. An opt-in test on local k3d created and removed a disposable
namespace and verified four actual CNI cases: normal rules pass; missing
egress fails; a missing IP exception list fails; missing listener ingress is
inconclusive. No running engine Deployment was replaced. This addresses the
ingress-masking failure in #5810; cloud and multi-node rollout remain unverified.

## Delivery privacy progress

Inbound and outbound concepts now declare the cluster-operator row tier.
Named reads, generic row admission and subscriptions reject ordinary users;
internal call origin alone does not grant a read. Package webhook and campaign
feedback handlers use an internal-only, bounded operator lookup. Datasync
retains its operator identity, email-rule staging retains author provenance,
and the outbound drain now uses the shared deployment actor on all auth
surfaces. Real Postgres tests cover those consumers and pipeline notification
receipts, including an operator client being unable to forge a server's sent
receipt. Package, campaign and email-rule suites pass with the test database.

Issues #5802 and #5804 remain open until review/merge and installation-level
verification, including external product-bundle audit surfaces. Direct client
outbox staging now requires operator authority; tree-loaded automations retain
their server-side staging path.

## DSL and runtime gaps found during implementation

| Capability | Evidence | Decision and verification |
| --- | --- | --- |
| Server-authored concepts | Named `@serverOnly` mutations did not protect raw writes to the same concept. | Added general `@serverWritten`, preserving separate read authorization. Real database tests reject client inserts and updates while authorized pipeline writers pass. Parser, annotation documentation and language conformance cases cover the declaration. |
| Required automation records | `component/automations/journal.go` continued after journal failures; the shared driver journal also previously hid failed step writes. | Shared driver-journal writes now return errors. Added opt-in `@journalRequired` for generic automations: require run/step intent, receipts and completion; stop on failed heartbeats; propagate the requirement to descendants. Fault, race and real-database tests cover a fresh executor resuming an unfinished read without repeating a completed read. This is not an external-effect or ownership guarantee. |
| Definition identity on resume | The automation fingerprint covered step IDs/types/conditions, omitting call arguments, nested bodies and execution policy. | Fingerprint the complete serialized definition, excluding source location, caches and top-level prose. Changed definitions and old incomplete fingerprints refuse resume. Tests cover changes to callees, arguments, retry/error/journal policy, nested bodies and input contracts. Transitive callee identity still requires pinned engine/bundle versions in the release candidate. |
| Safe replay | Generic resume guards side effects with `AllowSideEffects`; pipeline recovery has a separate driver. | Reuse the journal and recovery ownership primitives, but replace blanket replay permission with per-effect reconciliation. Validate pinned inputs, artifact availability and uncertain external outcomes before resuming. |
| Default workflow policy | Event-to-mode selection and substantial orchestration currently live in `component/pipelines` and `component/pipelinerun`. | Move the default choices to sealed public DSL; preserve bounded execution, distributed ownership and protocol validation in runtime capabilities. Prove composition with a separately named workflow. |

Remaining generic recovery gaps include ownership fencing, a run interrupted
before its first step intent, a run whose steps completed but terminal receipt
did not, and reconciliation before repeating uncertain effects. Do not turn
an absent record into permission to replay: a different replica may still be
working. Existing parser support for loops, branches, parallel blocks and
retries should be reused before introducing new syntax.
