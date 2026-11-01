---
title: Fleet workbench lifetime and recovery
audience: internal
status: stable
area: ops
sinceVersion: 0.20.1
owner: znas
---

# Fleet workbench lifetime and recovery

The engine owns the goal's durable placement and workspace generation. Cockpit
owns its ephemeral container, machine capacity reservation and tombstone. The
`workerWorkbench` version 2 constraint carries both generation and lease;
receivers lacking that contract cannot serve these calls.

An active workspace accepts fresh engine-authorized leases of at most one hour.
The original creation time is not a goal deadline. Renewal changes only the live
generation's deadline; five idle minutes, lease expiry, cancellation, uncertain
execution and worker shutdown still retire its container. The engine rechecks
owner, run destination, enrollment and sharing before renewal and dispatch.
This does not change computer-access permission windows or pipeline exclusion.

A retired generation cannot be reopened. Cockpit reports typed state loss and
keeps its tombstone. The engine records the loss; explicit `workspace_recover`
with the exact generation uses a durable row-version fence to prepare one new
generation on the same machine. The next authorized operation creates its empty
container under a different key. No lost operation is replayed, and no files are
migrated. Restore useful files from retained evidence. A cancelled goal, revoked
sharing, changed machine or stale generation refuses; a queued older call is
rejected by the receiving engine before it can use the replacement container.
Existing tombstone retention and uncertain-reservation reconciliation remain
in force. Recovery never deletes the old tombstone or bypasses the two-slot
limit, Docker identity check, memory headroom or container isolation.

`TestActiveWorkbenchRenewsLeaseWithoutLosingFiles` advances a controlled clock
past two hours while preserving one container and its files.
`TestExplicitRecoveryUsesNewGenerationWithoutReplayingLostOperation` verifies
idle loss, a new empty container and refusal of the old generation. Existing
cancellation, restart and orphan tests cover cleanup fences. These controlled
checks do not establish an uninterrupted two-hour live production run.
