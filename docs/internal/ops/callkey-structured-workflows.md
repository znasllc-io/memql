---
title: Bounded structured Callkey workflows
audience: ops
status: draft
area: ops
sinceVersion: unreleased
owner: znas
---

# Bounded structured Callkey workflows

UTCP 2.8 uses the same native Workflow requests for human and delegated controls.
Every live Skill requires Insights and current room/account authority. A delegate
also needs the approved Skill version and scope, and cannot bypass a human hold.
Timers are advisory: pausing guidance never pauses call time or billing.

## Finite repeats

A `repeat` controller declares `repeat: {count, untilNodeId}` with one body entry
edge and one inclusive exit. Count is 1–8. Every body path reaches that exit;
its sole edge names the successor after all rounds. Repeat bodies cannot overlap
or contain another repeat or parallel region. The native run pins its activation,
round and count. Every round creates new visit IDs and fresh acknowledgments.
Previous and Seek stay within the current round. Returning to the controller
after leaving it starts a new explicitly requested activation.

## Parallel guidance with join-all

A `parallel` controller declares two to four branches, each with a stable `id`,
`entryNodeId` and inclusive `untilNodeId`. Controller edges exactly match the
branch entries. Branch bodies are disjoint DAGs with no outside entry or early
exit, and every exit has one edge to the same successor. Branches support bounded
guidance, roles, readiness, acknowledgment and conditional navigation. Nested
repeat/parallel, presentation ownership, resource ownership and media effects
are rejected inside a branch until their concurrent authority can be arbitrated.

Entering the controller creates all branch visits. The outer node/visit remains
at that controller until every branch joins and an authorized outer Next enters
the common successor. An authorized outer Previous or Seek can explicitly leave
the current parallel activation; it archives its visits and never resumes that
activation implicitly. Seek cannot enter a branch body directly. Re-entering the
controller starts new branch visits and acknowledgments. Branch requests carry both `branchId` and the exact
`branchVisitId`, as well as the shared run revision, control epoch and idempotency
key. One branch cannot enter a sibling, reuse its acknowledgments, or advance the
outer run. The server projects each branch's current allowed actions and visited
Seek targets; clients do not infer authority from graph edges.

Branch clocks, visits, acknowledgment and readiness persist in the native run.
Lost readiness freezes only that branch. Restoration clears its blockers but
requires explicit global Resume, which preserves unaffected clocks. Global
pause/recovery uses durable anchors; it never dispatches effects or starts media.
History remains immutable, with activation/branch attribution. Both the global
visit journal and each current branch history retain at most 256 entries;
navigation cannot invent a visit that aged out. Hidden node text and events keep
their existing role/host privacy projection, including placeholder graph nodes.

Simulation uses sequential repeat time and the maximum concurrent branch time,
while visit bounds include every branch. Structured plans allow at most 256
planned visits and 24 hours; explicit manual revisits remain bounded by retained
history rather than becoming automatic loops.

## Explicit microphone-mute effect

The only effect node is `effect: {action: "end_turn_mute"}`. It requires an assigned
role, zero timer and exactly one successor; existing readiness rules may apply.
Entry, resume and revisit never execute an effect. Ordinary Next/End turn cannot
bypass the node. A host or agent with both `skills_control` and `control` reviews
the assigned participant and submits the existing exact `end_turn_mute` request.

The existing durable mute intent pins the current visit, actor admission, target
seat generation, provider participant SID and microphone tracks. It dispatches
once and advances only after provider readback confirms the same microphones are
muted. Pending/uncertain exact retries reconcile without dispatching again.
Cancellation or authority loss leaves the node unadvanced; it cannot undo a mute
already sent. A later visit requires a new explicit reviewed operation. There is
no arbitrary effect payload, automatic replay, unmute or device-start action.

Native tests live in `workflow_repeat*`, `workflow_parallel*` and
`workflow_effect_node*` alongside the existing combined-mute regressions. Actual
origin UI/media acceptance and deployment source receipts remain separate
evidence; a native test pass does not establish a deployed capability.
