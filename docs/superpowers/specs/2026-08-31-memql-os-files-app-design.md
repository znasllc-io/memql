# MemQL OS -- Files App + Library File System -- Design

- **Date:** 2026-08-31
- **Status:** approved (two rounds of in-session Q&A with the owner; every
  fork below records the choice that was made and why)
- **Scope:** `dsl/library/` (folder + uploadSession concepts, provenance
  fields, promotion threading), `component/server/` (artifact handler:
  chunked upload sessions, streaming + Range downloads),
  `integrations/azureblob/` (block + streaming APIs),
  `integrations/library/` (index carry-forward), `component/node/routing.go`
  (folder broadcast rules), `deploy/k8s/` (front-door body-size allowance),
  `clients/os/` (the Files app, desk folder unification, upload provider
  v2), the epic rewrite (#4721 and its tasks). New HTTP routes are additive
  members of the existing `/artifacts` documented exception, approved by the
  owner in this design session. No proto changes.
- **Amends:** the desktop-shell spec (2026-08-26) D3/D4. "The OS has no
  file system" becomes "the Library carries a Drive-model folder tree the OS
  renders"; desk folders stop being local icon-groups inside the desktop
  document and become shortcuts to Library folders (the popover
  presentation survives). The artifacts-app epic as originally written
  (#4721: frontend-only, records lens included) is superseded by this spec.
- **Follow-ups filed, not built here:** watched-folder backup (its own
  epic), zip extraction (fast-follow after folders), a records surface,
  server-side recursive cascade + move guard (hardening, see D11).

## Why

Owner's brief, condensed: the app ships as **Files**, not Artifacts --
artifacts are just files, with authors and origins. Files arrive by dragging
onto the OS desktop or uploading inside the app; where a file came from
matters (this machine, one of the user's fleet machines, or MemQL itself
through a workbench), authorship matters (human vs AI), and the overlap with
Fleet and Training must be respected, not rebuilt. There is deliberately no
file system today, but almost everybody knows how an operating system works
-- the absence of subfolders confuses people. Do not build an entire OS; do
make transfers production-grade: big files (videos), interruption recovery,
resume, and honest failure states the user can act on. An upload from a
machine is also the seed of a backup story: the MemQL copy must survive the
origin's deletion, with an eventual "unsynced" signal -- and the unit of
backup people actually want is a folder, not only a file.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Name | The app is **Files** (`clients/os/src/apps/files/`, app id `files`); epic #4721 is rewritten in place with its tasks. Engine names stay: `v1:library:artifact` (index), `v1:library:file` (bytes), `POST /artifacts`, the VS Code `kind=artifact` wire. The internal split -- artifact = thin index row, file = byte-bearing row -- remains accurate, and an engine rename would fan out across generated SDKs, gates, docs and the extension for zero user-visible gain |
| D2 | App scope | Content-bearing kinds only: `file`, `document`, `generated_output`. The records lens (notes / todos / calendar events / memories / live sources) leaves this app; those rows stay indexed and wait for their own surface in a later epic |
| D3 | Hierarchy | The **Drive model, not POSIX**: `v1:library:folder` rows plus `folderId` on the index. Ids, never path strings; sibling name duplicates allowed (folders are collections, not namespaces); depth cap 12; no permission tree -- row authz is unchanged. A move is one row update; renames touch nothing else |
| D4 | Desk folders | **Unified**: a desk folder is a shortcut to a Library folder; its popover is a live view of the folder's contents; desk create/rename are Library mutations; removing from the desk removes the shortcut and never archives. The foundation's local icon-groups are dropped -- pre-release, `sanitizeDocument` lifts their children back onto the grid |
| D5 | Provenance | `uploadedFromWorkerId` / `uploadedFromWorkerName` / `uploadedFromPath` on `v1:library:file`, stamped only where honestly known -- a cockpit push names its worker registration, a browser physically cannot name a machine or a path, and no identity is ever invented. Claims are **verified**: the named registration must belong to the uploading user or the upload is refused. Promotion forwards machine id/name into the index's existing `producedByWorkerId/Name`, whose meaning generalizes from "computer_use only" to "when a machine is known". Vocabulary rule: this axis is called *provenance*; "origin" stays reserved for the data-origins system (memql#4378) |
| D6 | Training | Knowledge documents list like any indexed item (they already carry a `validationStatus` facet); no train-into-domain action in v1. Upload is not training (memql#4340 D7) stands; `libraryTrainFile` remains the recorded bridge |
| D7 | Zip | Opaque in v1: stored, downloadable, declined by the VS Code handoff as binary. "Extract to folder" is a fast-follow once folders exist: server-side, entries stamped `source=derived` with provenance to the archive row, zip-slip and zip-bomb guards, count/size limits |
| D8 | Transfer | Files <= 32 MiB keep the existing one-shot multipart POST (it gains `folderId` and provenance fields). Above that: **chunked resumable sessions over Azure block-blob staging** -- 16 MiB chunks, sequential in v1, per-chunk retry with backoff. Staged blocks live with the blob in storage, not in any replica's memory, so any bff serves any chunk (the multi-node rule holds by construction) and abandoned staging garbage-collects on Azure's ~7-day clock with no sweeper |
| D9 | Caps | `MEMQL_LIBRARY_MAX_UPLOAD_BYTES` default rises to **4 GiB** (still an operator knob; a client's Content-Length stays a claim, not the limit). New `MEMQL_LIBRARY_USER_QUOTA_BYTES` (default **100 GiB**) enforced at session init and one-shot upload -- a per-file cap alone cannot stop one user filling the account. The bff-http front-door rules carry an explicit body-size allowance (~48m), which also fixes the latent bug that ingress-nginx's 1 MB default makes the documented cap unreachable in the cloud today |
| D10 | Hashing | `file.sha256` relaxes to optional: one-shot uploads keep computing it inline; a chunked upload gets it stamped by the analysis pass, which streams the committed blob once. It remains a dedup hint and an integrity check, never an access key |
| D11 | Recursive ops | Client-driven in v1, because the client holds the owner's whole (small) tree and the engine has no loop construct: folder archive walks the subtree behind an in-surface confirm naming the count; move-cycle prevention is a client check plus a cycle- and orphan-tolerant tree fold. A server-side cascade + integrity guard is recorded as a hardening follow-up, not built |
| D12 | Sync/backup | Invariant: **the MemQL copy is independent and durable -- origin deletion or disconnection never touches it, and archiving in MemQL never touches the origin.** v1 captures identity (D5) and ships one-time recursive folder drops; continuous **watched-folder backup** (one-way, machine -> MemQL; re-push versions the same logical file keyed by (machine, path); `synced` / `stale` / `origin-gone` states with folder rollup) is its own follow-up epic. Deliberately not a data-origins mirror: a mirror reconciles deletions, a backup retains and flags. The UI avoids the word "Backup" until re-push exists |
| D13 | Downloads | The content route learns streaming and single-range `Range`. The OS download action streams through a service worker where available, falls back to the buffered object-URL path below 512 MiB, and above that without a worker renders an in-surface sentence naming the limit and the alternatives (VS Code, cockpit) |
| D14 | Delivery | Five tasks, three PRs: PR1 = engine (T1 folders + provenance, T2 transfer), PR2 = OS browse + desk (T3, T4), PR3 = OS transfer UI + settings (T5). The grouping is stated on the epic and on every task |

## A. What exists today (the ground this builds on)

Verified in-session, so the tasks inherit facts rather than assumptions:

- The Library is an index (`v1:library:artifact`) over six backing concepts;
  `file` is the only byte-bearing one; both declare the composite tier
  (`@rowAuthz(owner="ownerUserId", clusterOwner)`).
- `v1:library:artifact` and `v1:library:file` **already carry broadcast
  routing rules** (`component/node/routing.go`), so the live browse needs no
  engine routing work. `v1:library:folder` will need its own created/updated
  rules in the same block.
- Promotion mechanics: `indexFileOnCreate` fires on
  `graph.node.created.v1:library:file` behind
  `@filter(payload.status == "stored")`; `createArtifact` is idempotent via
  the derived id `hash(sourceConceptRef)` and its body is a bare `insert{}`
  that drops unnamed fields -- the memql#4288 label-wipe class. The analysis
  pass re-stamps the index through `integrations/library`'s `touchArtifact`,
  carrying `labels` and `archived` forward. **Every new index field joins
  that carry-forward or gets silently wiped on the first status flip.**
- The desktop document roams (`v1:os:desktop`, opaque client-owned document,
  last-writer-wins revision). Desk folders today are flat local icon-groups
  holding file shortcuts only (desktop-shell spec D4).
- Transfers buffer whole files in bff memory in both directions
  (`io.ReadAll` -> `sha256.Sum256` -> `Upload([]byte)`; `DownloadURL` ->
  `[]byte`), with no Range support; the storage integration's API is
  `[]byte`-only. No body-size annotation exists anywhere under `deploy/`.
- A cockpit Library push is indistinguishable from a browser upload today:
  no machine fields exist on `file`, and `producedByWorker*` is stamped only
  on the computer-use `generatedOutput` path.

## B. Data model

### B1. `v1:library:folder`

```memql
@rowAuthz(owner="ownerUserId", clusterOwner)
concept folder {
  ownerUserId     string!  @serverSet
  name            string!
  parentFolderId  string        // empty/absent = root
  archived        bool  @default("false")
}
```

- Composite tier, matching `artifact` and `file`.
- Nesting via `parentFolderId`; depth cap 12 and cycle refusal are enforced
  client-side in v1 (D11), and the tree fold tolerates violations
  defensively (B4), so a hostile or buggy writer can degrade the picture but
  never break it.
- Sibling name duplicates are allowed. No uniqueness machinery, no rename
  conflicts.
- Mutations: `createLibraryFolder`, `renameLibraryFolder`,
  `moveLibraryFolder` (updates `parentFolderId`), `archiveLibraryFolder`.
  Queries: `libraryFolders` (owner's folders, `archived != true` -- the
  null-safe spelling), `libraryFolderById`.
- Broadcast rules `graph.node.created.v1:library:folder` and
  `graph.node.updated.v1:library:folder` join the library block in
  `component/node/routing.go`.

### B2. `folderId`

- On the **index** (`v1:library:artifact.folderId`): the organizational
  truth. Any kind can be filed -- documents and generated outputs, not only
  files. `moveArtifactToFolder(artifactId, folderId)` is a read-merge
  `update{}` (safe since memql#1628).
- On **`file`** as the *initial filing only*: the upload route knows the
  target folder, and the promotion automation can only forward what the
  backing row carries -- the same "the writer knew it" reasoning `format`
  already uses. After promotion the index is authoritative; the file row's
  copy is not tracked further, and its field description says so.
- `createArtifact` gains the optional `folderId` arg; `indexFileOnCreate`
  forwards it; **`touchArtifact` carries it forward** (the #4288 class; a
  test pins that an analysis re-stamp preserves `folderId` alongside
  `labels`).
- Root listing and every folder filter run client-side over the live
  snapshot. Two reasons: the owner-scoped set is small (the existing epic
  already composes filters client-side), and `folderId == ""` cannot match
  pre-field rows whose payload has no key at all -- the `archived != true`
  lesson makes the client-side fold the honest reader of a field that old
  rows do not carry.

### B3. Provenance fields on `file`

- `uploadedFromWorkerId`, `uploadedFromWorkerName`, `uploadedFromPath` --
  all optional strings.
- Stamped by the upload routes when the caller supplies them. Verification
  at write time: the named registration must exist and belong to the same
  user (read under the caller's actor); a claim that fails verification
  **refuses the upload** with a typed error rather than silently blanking --
  a silently-dropped field would render as "uploaded here", which is a lie.
- Promotion forwards worker id/name into the index's existing
  `producedByWorkerId` / `producedByWorkerName`; their descriptions
  generalize from "when source=computer_use" to "when a machine is known".
  The path stays on the file row only -- the index does not need it, and the
  sync epic reads the file row.
- Browser uploads carry none of the three. A browser cannot name its machine
  or the dropped file's path; this is recorded so nobody "fixes" it with
  user-agent guessing.
- `file.sha256` doubles as the origin-content fingerprint for the future
  sync epic (same bytes, same hash); no extra field.

### B4. The client-side tree fold

A pure function from (folders, artifacts) snapshots to a tree: cycle-tolerant
(a folder whose ancestor chain revisits a node or exceeds depth 12 renders at
root with a marker), orphan-tolerant (parent archived or missing renders at
root with a marker), stable sort (name, then id -- the collection folds
events in cluster order, and a map that depends on input order reshuffles
while somebody is watching). Fixture-tested with no DOM.

### B5. Folder archive semantics

Client-driven walk (D11): the confirm names the count ("Archive 'Client
videos' and its 37 items?"), then `archiveArtifact` per contained artifact
(the existing artifact->file automation archives backing files), then
`archiveLibraryFolder` children-first. Idempotent under interruption:
re-running archives the remainder; the confirm recomputes from live rows.
Folders past ~500 items batch sequentially with in-surface progress.
Removing a desk shortcut is never an archive.

## C. Transfer

### C1. Routes

All bearer-authenticated; all declared through `server.ArtifactPaths()` so
the front-door path generator routes them (they appear in none of the three
public/handler/self aggregates, which is exactly the class the generator
exists for); front-door blocks regenerated; the root CLAUDE.md HTTP-exception
table gains the rows in the same PR.

| Route | Purpose |
|---|---|
| `POST /artifacts` | Existing one-shot multipart, <= 32 MiB; gains `folderId` + `uploadedFrom*` form fields |
| `POST /artifacts/uploads` | Open a chunked session: `{name, size, mimeType, folderId, labels, uploadedFrom*}` -> `{uploadId, chunkSize}`; quota + provenance verification happen here |
| `GET /artifacts/uploads/{id}` | Staged-chunk inventory (Azure uncommitted block list) -- what resume reads |
| `PUT /artifacts/uploads/{id}/chunks/{n}` | One raw chunk <= 16 MiB, streamed to a staged block |
| `POST /artifacts/uploads/{id}/complete` | Commit the block list -> create rows -> `201 {artifactId, fileId}` |
| `GET /artifacts/{id}/content` | Existing export; learns streaming + single-range `Range` (C5) |

### C2. `v1:library:uploadSession`

```memql
@rowAuthz(owner="ownerUserId", clusterOwner)
concept uploadSession {
  ownerUserId            string!  @serverSet
  name                   string!
  size                   int!
  mimeType               string
  folderId               string
  labels                 []string
  uploadedFromWorkerId   string
  uploadedFromWorkerName string
  uploadedFromPath       string
  blobPath               string!   // library/{userId}/{fileId}/{name}; fileId minted at init
  fileId                 string!   // the id complete will use
  chunkSize              int!
  status                 enum("open", "completed", "abandoned")!
}
```

The row is the per-chunk authorization (row admission is the owner check) and
the record of init-time facts that complete needs. No sweeper: an abandoned
session row is inert, its staged blocks garbage-collect server-side on
Azure's ~7-day clock, and clients purge their local resume records on the
same clock.

### C3. Chunk mechanics

- 16 MiB chunks (a constant, not a knob), sequential in v1 -- parallel
  staging is a later throughput optimization, resume does not need it.
- The handler bounds every chunk body (`MaxBytesReader`, chunk size plus
  slack) and refuses `n > ceil(size / chunkSize)`, so staging is bounded by
  the declared size. Block ids are fixed-width encodings of `n`.
- Replica-agnostic by construction: staged blocks live with the blob; no bff
  holds session state in memory. Pinned by a test that completes one upload
  through two independent handler instances.
- `complete`: commit the block list in order, verify committed size ==
  declared size (mismatch refuses and the session stays open), then
  `createLibraryFile(status="stored", folderId, uploadedFrom*, sha256
  absent)` -> promotion -> the same `201 {artifactId, fileId}` shape as
  one-shot (the artifact id is derived deterministically from the source
  ref, which the one-shot handler already relies on). Session -> completed.

### C4. Caps and quota

- `MEMQL_LIBRARY_MAX_UPLOAD_BYTES` default becomes 4 GiB. Both enforcement
  layers keep treating the client's Content-Length as a claim.
- `MEMQL_LIBRARY_USER_QUOTA_BYTES` (new; default 100 GiB): at session init
  and at one-shot upload, the sum of the owner's file sizes plus the
  declared sizes of their open sessions must stay under the quota; the
  refusal is typed and names both numbers. Archived files keep their bytes
  and keep counting -- retention is real. Both env vars get manifest
  entries.
- The chunked path structurally sidesteps giant single bodies; the
  front-door allowance (~48m) covers a 32 MiB one-shot plus multipart
  framing and a 16 MiB chunk with slack. The exact seam for the annotation
  (generator vs overlay patch) is T2's to decide -- the generated block is
  never hand-edited. Local traefik enforces no default limit; the parity
  note is recorded either way.

### C5. Downloads

- The content route streams file bytes (`DownloadStream` -> `io.Copy`) with
  Content-Length from the row, honoring a single-range `Range` header
  (206 / Content-Range). Non-file kinds are small rendered bodies and stay
  as they are.
- The OS download action (D13): service-worker streaming when available;
  buffered object-URL below 512 MiB otherwise; above that with no worker, an
  in-surface sentence naming the limit and the alternatives. Authorization
  stays bearer-only -- no signed URLs, no cookies, no redirects (memql#4341
  D1 upheld).

### C6. Storage integration

`integrations/azureblob` grows staged-block upload (StageBlock /
CommitBlockList / uncommitted-list inventory) and streaming download with
Range; the `[]byte` Upload/Download stay for the small paths. Azurite
supports all of it locally.

## D. OS frontend

### D1. The Files app

- `clients/os/src/apps/files/`; the registry's artifacts stub is replaced
  (id `files`, name "Files"). Sections: Browse and Settings; Ask context
  tags `app:files/<section>`.
- Two live collections retained at the app root (folders + artifacts), one
  shared selection -- the Deployables rule: a second subscription is one
  that can disagree.
- List scope: kinds `file` / `document` / `generated_output` only (D2).
  Filters: kind, source, archived (default excluded, visibly marked when
  shown). Search over title/summary/labels. Sort `row.createdAt` desc
  default. Every filter/search/tree derivation is a client-side fold over
  the snapshots.
- Arrival cue fingerprint: title, folderId, status, archived, labels --
  what a person would call a change. Never liveness, never a raw
  `updatedAt` (analysis re-stamps would strobe it).
- Inspector: the provenance story ("uploaded here", "uploaded from
  <machine>", "made on <machine> by computer use", "produced by plan
  <id>"), `kit/ProvenanceDot` when a machine is named, label chips,
  monospace ids, `validationStatus` for documents, timestamps.
- Errors render in-surface with retry; no toasts; empty vs filtered-empty
  vs degraded distinguishable (LiveState caption).

### D2. Desk unification

- The `DesktopItem` folder variant becomes
  `{ kind: "folder", id, folderId, name }` -- a shortcut. The popover
  mounts its own retained, folder-scoped live collection while open; the
  desk itself stays subscription-free until then.
- Desk create-folder calls `createLibraryFolder` and places the shortcut;
  rename calls `renameLibraryFolder`; remove-from-desk removes the shortcut
  only. Sending an item already on the active desk focuses it (the existing
  dedupe rule).
- Dropping a host file on a desk folder uploads with that `folderId`;
  dropping on the desk uploads to root. App drop targets `stopPropagation`
  on drop and dragover, even when disabled (the Training rule).
- `sanitizeDocument` migrates old local icon-groups by lifting their
  children back onto the grid as plain shortcuts (the `deleteFolder`
  shape), so nobody loses a shortcut to the rename.

### D3. Upload provider v2

- The provider interface gains per-file progress and resume; the result
  stays the artifact shape; abort stops sending (server staging
  garbage-collects).
- <= 32 MiB one-shot; above: init -> sequential chunk PUTs (retry with
  backoff; a mid-file failure never restarts the file) -> complete.
- Resume across reloads: localStorage maps `(name, size, lastModified)` ->
  `uploadId` (purged after 7 days, matching the staging GC); on re-drop the
  provider reads the inventory and uploads only the missing chunks. No
  File System Access handle persistence -- Safari has none, and the
  re-drop flow is the honest cross-browser resume.
- Folder drops (desk and app): directory-entry walk, bounded (<= 500 files,
  depth <= 12), creates the folder tree first, then uploads with modest
  concurrency; one aggregate placeholder with per-file progress; a partial
  failure leaves the landed files landed and lists the failures with
  retry.
- Over-cap and over-quota refusals render the server's sentence verbatim,
  in-surface; the client duplicates no limit.
- One upload path: apps import the provider; a test or lint pins that no
  second `POST /artifacts` call site exists in `clients/os`.

### D4. Settings

Default sort, confirm-before-archive (default on, consumed by the archive
flows), show-archived toggle. Versioned, sanitized per-app store (the fleet
pattern); the title-bar gear jumps to the section.

## E. Sync roadmap (recorded, not built)

The D12 invariant lands in `docs/public/operate/library.md` with T1. The
watched-folder epic (filed alongside this spec) covers: the cockpit watches
a chosen folder on a fleet machine -> initial recursive push into a Library
folder -> a re-push of a changed file versions the same logical file, keyed
by `(uploadedFromWorkerId, uploadedFromPath)` -> per-file link states
(`synced` / `stale` / `origin-gone`) with a folder rollup -> one-way
forever; a deletion at the origin flags and never deletes. Cross-repo split:
the watcher and scheduling live in `memql-cockpit`; the states, rollup and
UI live here.

## F. Testing

- Engine: dslconformance dimensions for the new promotion args; a
  carry-forward test that an analysis re-stamp preserves `folderId` and
  `labels` (the #4288 class); db-gated session lifecycle tests (init /
  chunks / inventory / complete; out-of-range `n`; oversize chunk; size
  mismatch refusal; quota refusal including open-session counting;
  provenance verification refusal); the two-handler-instances completion
  test (no in-memory session state); Range tests (206 bounds; non-file
  kinds unchanged); sha256-stamped-by-analysis.
- OS: pure-function tests for the tree fold (cycle and orphan tolerance),
  filters/sort, and the folder-drop walker bounds; component tests for the
  cue (fires on move/rename, silent on analysis re-stamps), the placeholder
  lifecycle including resume-after-reload with a mocked inventory, the desk
  folder popover live view, stopPropagation, the confirm naming the count,
  and refusals rendered verbatim. The existing `os-checks` lane covers the
  app; engine tests ride `make test` and the db-gated lane.

## G. Delivery

| Task | Content | PR |
|---|---|---|
| T1 | Engine: folders + provenance (B1-B5 engine half, routing rules, docs) | PR1 |
| T2 | Engine: production transfer (C1-C6, caps/quota, front door, CLAUDE.md table) | PR1 |
| T3 | OS: Files browse (D1) | PR2 |
| T4 | OS: desk unification + row actions (D2 + send-to-desktop / VS Code / download / archive) | PR2 |
| T5 | OS: upload provider v2 + settings (D3, D4) | PR3 |

Issue numbers are recorded on epic #4721. Docs updated at implementation
time: `library.md` (folders, provenance, transfer, quota, the D12
invariant), the env-vars doc + registry, the root CLAUDE.md exception table
(T2), the OS README app roster (T3).

## H. Not building (settled)

Path strings, mounts, permission trees; sibling-name uniqueness; two-way
sync or conflict resolution; in-OS viewers or editors beyond the VS Code
handoff; the records lens; zip extraction (fast-follow); watched folders
(own epic); server-side recursive cascade (hardening follow-up); signed
URLs of any kind.
