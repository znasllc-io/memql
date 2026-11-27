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

## Player boundaries and versioned catalog

An authorized Change checkpoints the previous active run as paused before the
replacement definition and role bindings are checked. Failure leaves its clocks
frozen and advances the revision/control epoch; the original exact request is
then stale and cannot install a replacement. A permitted caller must refresh and
review a new request or explicitly resume. An unauthorized or stale request never
creates this checkpoint. A successful replacement retains normal exact-retry
protection and never runs a media effect automatically.

Extend bounds the resulting advisory time remaining by whole seconds before the
current funded call deadline, for ordinary steps and each parallel branch. Paused
guidance cannot extend the call or funding. Overtime is advisory and has one stable
visit-scoped Activity entry per branch, using the same role/host visibility rules
as other events; refresh updates that entry instead of appending duplicates.

The fourteen originally published built-ins remain unchanged. Presentation and
office-hours version 2 contain actual presentation visits with reviewed presenter,
audience and temporary-speaker guidance. Planning version 2 demonstrates a real
two-branch join-all workshop, and the new training version 1 uses three explicit
practice/reflection/acknowledgment rounds. Choosing these versions does not grant
delegated approval or trigger an effect, device, provider or payment action.

## Readiness help and reviewed alternatives

A guidance node with a declared video, screen-share or connected-supervisor
condition may configure `readiness: {timeoutSeconds, help, fallbackNodeId?}`.
Timeout is 1–3600 seconds; inert help is nonempty and at most 1000 UTF-8 bytes.
A fallback is an extra declared outgoing edge alongside the ordinary path.
Ordinary Next never chooses that edge. The fallback stays within the current
repeat round or parallel branch; structured exits cannot bypass later rounds or
the join-all barrier. Repeat/parallel controllers, effect and finish nodes cannot
configure fallback readiness. Each configured wait contributes its timeout to
worst-case simulation time, within the existing 24-hour/256-visit plan limits.

Entering a configured step with unavailable readiness records an immutable
visit-bound `blockedAt` and `deadline`. Wall time continues during pause; expiry
only exposes a reviewed alternative and never changes the visit automatically.
Restoration clears that wait; losing readiness again starts a fresh wait.
Snapshots and restart preserve the current wait. Help follows the node's existing
host/role visibility and is blank in an unauthorized projection.

The explicit `fallback` operation has a closed revision/epoch/idempotency envelope
and accepts no supplied target. Parallel fallback also requires the exact branch
and branch-visit IDs. Native admission, host authority, Insights/family gates,
funded call deadline, the delegate's exact approved Skill version and room,
current acknowledgment, and target readiness remain authoritative. Fallback never
runs a media effect or starts a publisher's camera or microphone. A pending
window reports `workflow_readiness_timeout_pending`; absent/invalid alternatives
report `workflow_fallback_unavailable`. Ordinary prerequisite, acknowledgment,
stale-revision and structured-navigation refusals remain unchanged.

Visit history distinguishes `completed`, `skipped`, `revisited` and `abandoned`
outcomes, with the actual navigation action. Seek/fallback is not completion;
Previous and same-step Seek record a revisit. Cancel and hard call expiry archive
abandoned visits. A successful Change exposes only bounded `previousRunExit`
metadata to the host; failed Change preserves the old paused run. Older retained
visits without outcome metadata remain unknown rather than claiming completion.
Native coverage is in `workflow_readiness*` and `workflow_player_limits*`; actual
origin and deployed acceptance are recorded separately.
