---
title: Data Validation Lifecycle
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Data Validation Lifecycle

## Overview

memQL uses a three-state validation lifecycle for data records: **draft**, **checked**, and **confirmed**. This replaces the old staging/production concept split with a unified `v1:data:record` concept where data visibility is governed by validation state and configurable policies.

## Vocabulary

| Term | Meaning |
|------|---------|
| **Draft** | Data is being worked on. Volatile. Not usable. |
| **Checked** | Synthetically validated by a synthetic identity. May be usable if the policy allows. |
| **Confirmed** | Human validated by a human identity. Fully valid. Always usable. |
| **Check** | Forward action: a synthetic identity validates data (draft -> checked). |
| **Confirm** | Forward action: a human identity validates data (checked -> confirmed). |
| **Revert** | Backward action: move data to a previous state (confirmed -> checked, checked -> draft, etc.). |
| **Policy** | Per-concept configuration defining validation requirements. |
| **Log** | Audit trail of all state transitions. |

## State Machine

```
  ┌─────────┐   check    ┌─────────┐   confirm   ┌───────────┐
  │  DRAFT  │ ─────────> │ CHECKED │ ──────────> │ CONFIRMED │
  │         │            │         │             │           │
  └─────────┘ <───────── └─────────┘ <────────── └───────────┘
               revert                  revert
```

- **Draft -> Checked**: Requires synthetic identity check(s) meeting policy threshold
- **Checked -> Confirmed**: Requires human identity confirmation(s) meeting policy threshold
- **Any state -> Draft/Checked**: Revert by identity with sufficient role (per policy)
- **Editing data**: Automatically resets to draft (prior validations invalidated)

## Identity Requirements

| Action | Required Identity Type | Notes |
|--------|----------------------|-------|
| Check | Synthetic | `v1:identity:identity` with `type="synthetic"` |
| Confirm | Human | `v1:identity:identity` with `type="human"` |
| Revert | Human or Synthetic | Must meet `revertMinRole` from policy |

## Concepts

### v1:data:record

Unified data record with validation state. Key fields:

- `validationState` — enum: draft, checked, confirmed (default: draft)
- `checkCount` / `confirmCount` — running tallies
- `lastCheckedBy` / `lastCheckedAt` — last synthetic check metadata
- `lastConfirmedBy` / `lastConfirmedAt` — last human confirm metadata
- All original data fields: `data`, `label`, `recordType`, `spaceId`, `importSource`, etc.

### v1:data:policy

Per-recordType validation configuration:

- `targetRecordType` — which record type this policy applies to
- `spaceId` — optional space scope (null = global)
- `requiredChecks` — number of synthetic checks needed (default: 1)
- `requiredConfirmations` — number of human confirmations needed (default: 1)
- `checkedDataUsable` — whether checked records are usable live (default: false)
- `revertMinRole` — minimum role to revert records (default: admin)

### v1:data:log

Audit trail for state transitions:

- `recordId` — which record
- `action` — check, confirm, or revert
- `fromState` / `toState` — state before and after
- `identityId` / `identityType` — who did it
- `note` — optional reason

## Queries

| Query | Purpose |
|-------|---------|
| `recordsByState` | Filter records by validation state, space, type |
| `usableRecords` | Get records usable per policy (confirmed + optionally checked) |
| `validationLog` | Audit trail for a record or space |
| `detectConflicts` | Find confirmed records with matching natural keys |
| `policy` | Get the validation policy for a record type |

## Mutations

| Mutation | Purpose |
|----------|---------|
| `createRecord` | Create a new record in draft state |
| `createRecordBatch` | Batch-create records in draft state |
| `updateRecord` | Update record data (resets to draft) |
| `deleteRecord` | Soft-delete a record |
| `checkRecord` | Synthetic check (increments count, may transition to checked) |
| `confirmRecord` | Human confirm (increments count, may transition to confirmed) |
| `revertRecord` | Revert to a previous state |
| `setPolicy` | Create or update a validation policy |

## Events

| Event | Trigger |
|-------|---------|
| `graph.node.created.v1:data:record` | New record created (automatic) |
| `data.record.checked` | Record synthetically checked |
| `data.record.confirmed` | Record human confirmed |
| `data.record.reverted` | Record reverted to previous state |
| `data.conflicts.detected` | Conflict found with existing confirmed records |

## Policy Examples

**Strict (default):** Requires 1 synthetic check + 1 human confirmation. Checked data is not usable.
```
requiredChecks: 1, requiredConfirmations: 1, checkedDataUsable: false
```

**Synthetic-sufficient:** For low-risk data where synthetic validation is enough for live use.
```
requiredChecks: 1, requiredConfirmations: 1, checkedDataUsable: true
```

**Multi-reviewer:** For high-stakes data requiring multiple validations.
```
requiredChecks: 3, requiredConfirmations: 2, checkedDataUsable: false
```

**Skip synthetic check:** For data that goes directly to human review.
```
requiredChecks: 0, requiredConfirmations: 1, checkedDataUsable: false
```
