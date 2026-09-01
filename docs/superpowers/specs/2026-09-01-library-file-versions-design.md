# Library file versions -- Design

- **Date:** 2026-09-01
- **Status:** approved (epic memql#4806, owner-approved scope; the design
  fork #4807 defers to this record is resolved in D1 below)
- **Scope:** `dsl/library/` (the `fileVersion` concept plus its mutations /
  queries / shapes and two fields on `file`),
  `component/server/artifact_handler.go` +
  `artifact_upload_sessions.go` + a new `component/server/fileversion`
  stamp package, `integrations/library/` (analysis re-entry on a
  superseded head), `docs/public/operate/library.md`, and the Files app's
  inspector + upload provider in `clients/os/`.
- **Ships as ONE pull request** covering #4807 and #4808 together, at the
  owner's instruction (the epic's own "two PRs" note is superseded).
- **Not in scope** (the epic's list, restated so a reader of this file
  alone does not have to guess): content diffing, cross-user dedup by
  sha256, retention pruning, automatic same-file detection for browser
  uploads, two-way sync, version history for the records kinds.

## Why

Nothing in the engine ever destroyed an earlier version of a file. Every
row write is retained in the hypertable, every upload's bytes sit at an
immutable per-`fileId` blob path, and archiving keeps both. None of that
was a FEATURE: ordinary queries collapse to the latest row per id, a
re-upload of the same document minted a sibling artifact rather than a
version 2, and the Files inspector told a file's provenance story with no
history behind it.

Documents have had the full treatment since memql#1230 (`documentVersion`
rows, a history drawer, restore-as-append). This epic brings the same
honesty to uploaded files, in the two increments that need no new storage
semantics: **upload a new version of THIS file**, and **see the history**.

## The one constraint

**Hypertable row history is not client-readable.** The engine's query path
returns one row per id by design, so "just read the old versions" is not a
thing a client can do. A version surface must therefore be built out of
DISTINCT rows, which is exactly why `documentVersion` exists rather than a
`documentHistory` read.

---

## D1 -- the mechanism: a `fileVersion` concept, head-on-the-file-row

**Chosen.** `v1:library:file` stays the HEAD -- the newest bytes, the row
everything already reads. Each supersede snapshots the OUTGOING head into
an immutable `v1:library:fileVersion` row and then updates the head in
place with the new bytes.

```
artifact  v1:library:artifact:<hash(v1:library:file:f-1)>   <- never re-points
   |  sourceConceptRef
   v
file      v1:library:file:f-1        versionNumber 3   <- the head, newest bytes
              fileVersion f-1-v2     versionNumber 2   <- superseded, immutable
              fileVersion f-1-v1     versionNumber 1   <- superseded, immutable
```

**Rejected: version-chained file rows** (each version a fresh
`v1:library:file` carrying `logicalFileId`, the index re-pointing to the
newest). It dies on the artifact index's two load-bearing derivations:
`createArtifact`'s id is `concat("artifact-", hash(sourceConceptRef))` and
promotion resolves the current row through
`libraryArtifactBySourceConceptRef`. Re-pointing `sourceConceptRef` gives
version 2 a DIFFERENT artifact id -- a second row in the Library for one
file, with the first one orphaned. Pinning the derivation to the first
version's ref instead keeps the id but makes the index point at a row that
no longer holds the bytes, so every reader (the content route, the
analysis pass, the inspector) needs a second hop and a new index field to
find the head. And `indexFileOnCreate` fires on `graph.node.created` for
every `v1:library:file` at `status=="stored"`, so each new version row
would promote itself into its own artifact unless a new filter conjunct
held it back.

The chosen shape touches none of that: `sourceConceptRef` is written once
and never changes, so the idempotency key, the promotion resolve and the
artifact id are exactly what they were.

**What the shape costs, stated plainly:** two byte-bearing concepts for one
file, and a content route that must learn which one a request means (D8).
That is the tension #4807 named, and it is the cheaper of the two.

## D2 -- the version row's id is derived, and derived in Go

`<fileId>-v<n>`. Deterministic, so a retried supersede RE-VERSIONS the same
row instead of appending a duplicate to somebody's history; readable in a
row dump; and it follows `createLibraryFile`'s own precedent of taking an
engine-minted id as an argument rather than `createArtifact`'s
hash-in-the-DSL. A hash would need `concat` over an int, and the key here
is already unique and already short.

## D3 -- write order: the snapshot first, then the head

A supersede is two writes and there is no transaction across them, so one
of the two failure pictures has to be chosen deliberately.

- **Snapshot first (chosen).** A crash between the writes leaves a version
  row whose facts equal the head's. The history fold keys on
  `versionNumber` with the head winning, so the reader sees one v2, not
  two; and the retry is idempotent by D2, so the second attempt writes the
  same snapshot and then the head.
- **Head first (rejected).** A crash between the writes loses the
  superseded version's ROW while its bytes stay in storage: history skips a
  number, and nothing anywhere says it happened. Silent, and unrecoverable
  without a blob-storage walk.

## D4 -- the head must not re-enter `status: "stored"`

`indexFileOnCreate` carries `@filter(payload.status == "stored")`, and
`graph.node.created` fires on every write because the engine is
append-only. A supersede that reset the status to `stored` would re-run
promotion through `createArtifact`'s bare `insert{}` -- which names no
`labels` -- and would re-file the artifact at the FILE row's `folderId`,
the initial filing that a later move deliberately never came back to
update. That is the memql#4288 carry-forward hazard reached through the
promotion path: labels wiped and the file silently moved, as a side effect
of uploading a new version.

So `supersedeLibraryFileHead` stamps `status: "analyzing"` directly. The
analysis pass carries it on to `ready` or `failed` exactly as it does for a
fresh upload, and where there is nothing to analyse the handler closes it at
`ready` through the same `startAnalysis` branch a fresh upload uses.

## D5 -- `sha256` is written on every supersede, blank when unmeasured

`update{}` has been a read-merge since memql#1628, so an ABSENT `sha256`
keeps whatever the previous version measured -- v1's hash sitting on v2's
bytes. That is not a missing fact, it is a false one, and the field is an
integrity check.

`sha256: args.sha256 ?? ""` writes the blank explicitly on the chunked
path, where the handler never held the file. The concept's own reading of
absence -- "not measured yet, never no hash exists" -- stays true, and the
analysis pass stamps the real value when it streams the committed blob.

## D6 -- the versioned blob path

`library/{userId}/{fileId}/v{n}/{name}` for every version after the first.
Version 1 keeps the unversioned `library/{userId}/{fileId}/{name}` path it
already has, so nothing is migrated.

The D12 durability invariant (`docs/public/operate/library.md`) holds by
construction rather than by care: a supersede only ever writes to a path no
version has used, so there is no code path in this epic that can overwrite
stored bytes.

## D7 -- the target is verified before a byte moves

`targetArtifactId` arrives on the one-shot form and in the chunked init
body. Three gates run in order, all before any upload, all under the
CALLER's own actor:

1. the artifact resolves through `store.Artifact` -- which returns nil for
   both "not there" and "not yours", deliberately, the same way the
   download route's refusals are 404s;
2. its `kind` is `file`. A document target is refused with a sentence
   naming the flow that does version documents, because the person asking
   is not wrong, they are in the wrong place;
3. the backing `v1:library:file` row resolves.

Fail-fast at chunked INIT matters as much as at one-shot: the alternative
is discovering a foreign target after somebody has streamed gigabytes.

## D8 -- the content route learns `?version=N`, not a new path

The front-door path set is generated (memql#3703) and a new path shape
would change it. A query parameter keeps both the head and the history
under the one `/artifacts/{id}/content` rule that already exists.

Absent means the head, which is what every existing caller sends.
`?version=N` resolves through the owner-gated version read, so a version
the caller may not see does not resolve, and `Range` keeps working on the
head exactly as before.

## D9 -- the quota counts every version

`libraryFileVersionSizesForOwner` joins `StorageFootprint`'s sum beside the
file and open-session halves. Retention is real -- superseding destroys
nothing -- and a quota that ignored history would make its own refusal
unreadable: a person under the cap by the numbers they can see would be
refused by numbers they cannot.

Archived versions count too, for the reason `libraryFileSizesForOwner`
already gives: otherwise "archive, then re-upload" mints unbounded storage.

## D10 -- the version writes are `@serverOnly`, stamped from one small package

`fileVersion.blobUrl` is a storage path a caller must never author: a
forged one would mint a version row pointing at another user's object, and
`?version=N` would then serve those bytes. Same reasoning that made
`createUploadSession` `@serverOnly`.

Satisfying `@serverOnly` from an HTTP handler means stamping internal
origin on a REQUEST-DERIVED context, which `component/auth/call_origin.go`
treats as the dangerous shape. So the stamp lives in
`component/server/fileversion`, a package containing nothing else, holding
the same three preconditions `component/server/uploadsession` asserts:

- the stamped context is a local derived per call and no method returns a
  context, so it cannot outlive one write;
- no rendered write names `ownerUserId` -- the mutation stamps it from the
  actor already on the caller's context;
- the READS are not stamped at all, so row admission is the owner check.

`store_internal_origin_test.go` asserts all three, and the root
`call_origin_conformance_test.go` allowlist entry cites it by name.

## D11 -- the history panel reads on demand, and says when it read

`graph.node.*.v1:library:fileVersion` has NO routing rule, and gets none.
The Files app's live feed exists for rows written on one replica and read
on another -- the folder tree, the artifact index. A version row is written
by the bff that took the upload and read by the panel that asked for it,
and that panel already re-reads the moment a version lands.

The panel therefore carries an honest caption -- when it read, and a way to
read again -- rather than implying a feed it does not have. That is the
Fleet lesson applied in the direction it actually points.

## D12 -- the resume ledger key gains the target

The chunked resume ledger keys on `(name, size, lastModified)`. Dropping
`report.pdf` as a fresh upload and then dropping the same `report.pdf` as a
new version of an existing artifact would RECALL the first session -- which
carries no target -- and the version would silently land as a second
artifact. The target id joins the key, appended only when non-empty so
existing fresh-upload entries keep the keys they have.

---

## Coordination with #4783 (watched-folder backup)

**There is one version mechanism, and this is it.** #4783 versions the same
logical file keyed by `(uploadedFromWorkerId, uploadedFromPath)` on
re-push. That epic adds a RESOLVER -- key to file row to artifact id -- and
then calls the same seam this one calls:

```
explicit target (this epic)   a person names targetArtifactId
key-matched   (#4783)         the cockpit's (workerId, path) resolves to one
                              ------------------------------------------------
                              both -> LibraryStore.SupersedeFile
                              both -> a fileVersion snapshot + a head update
                              both -> one artifact row, unchanged id
```

What #4783 must NOT do is mint its own version concept, its own version
numbering, or a second supersede write. Its synced/stale/origin-gone states
are facts about the WATCH, not about the version chain, and belong on
whatever row expresses the watch.

## Testing

- Db-gated: a version chain across one-shot and chunked; the three refusals
  (foreign target, non-file kind, quota); quota summing across versions; the
  #4288 carry-forward with the new index fields; an older version's
  download.
- Handler-level, no database: the gate ORDER (nothing stored on a refusal),
  the `?version=` selector, `Range` on the head.
- `component/server/fileversion`: the three internal-origin preconditions,
  plus every rendered statement parsed by the real engine front end.
- OS: history rendering from fixtures, the provider seam carrying the
  target (with `onePath.test.ts` still pinning the provider as the only
  route speaker), refusal rendering, and the pure folds.
