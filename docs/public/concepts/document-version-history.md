---
title: Library Document Version History
audience: public
status: stable
area: concepts
sinceVersion: 0.9.34
owner: znas
---

# Library Document Version History

## Overview

Every Library document carries an append-only version history. Producing,
editing, or restoring a document never overwrites the prior content: it
APPENDS a new immutable version with the next monotonic version number,
recording who authored the change (user, assistant, or system) and an
optional note describing what changed. The artifact index and the
backing document always point at the latest version; the full history is
retained behind them and is browsable + restorable.

This is the feature that lets a user (or an assistant acting for them)
edit a generated document with confidence: no edit is destructive, and
any earlier version can be brought back at any time.

## Vocabulary

| Term | Meaning |
|------|---------|
| **Logical document** | A document's stable identity across all its versions (the `v1:library:generatedOutput` id). |
| **Version** | An immutable content snapshot of the logical document at one point in its history (`v1:library:documentVersion`). |
| **Version number** | A monotonically increasing integer within a logical document, starting at 1. |
| **Author kind** | Who authored a version: `user`, `assistant`, or `system`. |
| **Latest pointer** | The newest version; what the Library viewer + artifact index resolve to. |
| **Restore** | Forward-append a new latest version equal to a chosen earlier one. Non-destructive. |

## The append-only model

```
v1  (system, "produced")        <- initial version on document creation
v2  (user, "fixed the totals")  <- user edit (memql#1229)
v3  (assistant, "added 3 rows") <- assistant edit (memql#1231)
v4  (system, "restored from v2")<- restore of v2 (memql#1230)  == content of v2
                                   ^ latest pointer
```

Each arrow is a NEW row. v1-v3 are never mutated or deleted when v4
lands; restoring v2 does not roll back -- it appends a fresh v4 whose
content equals v2's, so the history stays linear and complete.

## Concept

`v1:library:documentVersion` (see `dsl/library/concepts.memql`) carries:

| Field | Purpose |
|-------|---------|
| `documentId` | The logical document id. A plain string FK (not an `@relationship`) -- it points across concepts to the backing document; the valid relationship types (parent / contains / alias / equals) do not model a cross-concept content-history link. The grouping key for every history read. |
| `versionNumber` | Monotonic version, computed server-side as `latest + 1`. |
| `content` / `attachmentId` | The immutable body snapshot (inline text and/or a file-backed attachment). |
| `authorKind` / `authorId` | Who authored the version + their identity. |
| `note` | Short human-readable description of the change. |
| `parentVersionId` | Back-pointer to the version this one derived from. |
| `producedByPlanId` / `spaceId` | Provenance parity with the artifact spine. |
| `ownerUserId` | Per-row authz key; threaded from the document's owner. |

Per-row authz is **owned**: every read gates on
`ownerUserId == actor.userId`, so a caller only ever sees their own
document history.

## Storage and the time-series choice

Like every other concept, `documentVersion` rows live in the
`MemoryNodes` TimescaleDB hypertable, which is already partitioned on
`createdAt`. So version history is inherently time-partitioned -- there
is no separate physical table, and the engine's per-row authz, partition,
and mutation middleware apply uniformly. This mirrors the decision the
observability `invocation` concept documents (the difference is that
observability rows are written by the observe runtime directly, while
`documentVersion` rows go through the engine like any other mutation).

The migration `20260609000000_document_version_history` adds two storage
optimizations to the shared hypertable:

1. A **partial index** on `(documentId, versionNumber DESC)` scoped to
   the `documentVersion` concept, so "every version of document X,
   newest first" is an index read, not a scan.
2. A **long-window compression policy**: chunks older than 90 days are
   compressed (segmented by `concept` so a single document's cold
   history decompresses contiguously); recent history stays uncompressed
   for fast edit/restore reads.

History is **long-lived by design**: there is NO scheduled retention
drop. Versions are never evicted on a timer -- the only lifecycle is
compression of the cold tail. (Contrast the observability hypertable,
which sets a 7-day retention.)

## The edit / restore path

Appending a version is a read-then-compute-then-write operation the
MemQL DSL cannot express alone (the next version number is `latest + 1`,
and MemQL has no arithmetic or id-mint in a mutation body). So the
orchestration lives in the `library` Go integration
(`integrations/library/`), exposed to the DSL + SDK as builtins:

| Surface | Purpose | Issue |
|---------|---------|-------|
| `editDocument` (builtin / SDK) | User edit: append a version (`authorKind=user`), bump the number, update the latest pointer. Optimistic concurrency via `expectedVersion`. | memql#1229 |
| `documentVersions` / `documentVersionById` (query / SDK) | Read the ordered history / a single version's full content. | memql#1230 |
| `restoreDocumentVersion` (builtin / SDK) | Append-as-latest restore of a chosen version. Non-destructive. | memql#1230 |
| `editDocument` (agent tool) | Assistant edit: append a version (`authorKind=assistant`, `authorId=agentId`) with planner provenance. | memql#1231 |

The owning user is always threaded from the backing document ROW, not
from the caller, consistent with the other Library mutations -- an edit
can never reassign ownership, and the appended history is always
attributed to the document's owner.

### Optimistic concurrency

An edit may pass `expectedVersion` -- the latest version number the
caller saw. If the document has moved on under them (someone else
appended a version), the edit is rejected as a conflict rather than
appending on top of a stale base. The history itself is never lost; the
caller re-reads the latest and retries.

## Authoring vs. document versioning

This is distinct from
[concept versioning](concept-versioning.md), which versions DSL concept
*schemas*. Document version history versions the *content* of a Library
document.
