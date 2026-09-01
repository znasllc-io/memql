---
title: The Library
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# The Library

The Library is where a person's own material lives: files they upload, and the
records the assistant keeps for them. It has two halves that are easy to
confuse, so the distinction is worth stating once.

- **`v1:library:artifact` is an INDEX.** One thin row per item, carrying the
  provenance and type spine the Artifacts page lists, filters and sorts on. It
  owns no content at all (memql#693).
- **The content lives on a BACKING row.** Six concepts are promoted into the
  index today: `v1:library:file`, `generatedOutput`, `note`, `todo`,
  `calendarEvent` and `memory`. `file` is the one that carries bytes.

Every read of the index and of `v1:library:file` is gated by
`@rowAuthz(owner="ownerUserId", clusterOwner)`: the row's owner, or a cluster
owner. There is no third answer.

---

## Files

`v1:library:file` is the sixth backing row and the only one with bytes behind
it. It carries `name`, `mimeType`, `size`, `sha256`, `blobUrl`, `source`,
`status`, `summary`, `embeddingStatus`, `trainedIntoDomainIds`, `archived`,
the initial filing (`folderId`), the upload provenance trio
(`uploadedFromWorkerId` / `uploadedFromWorkerName` / `uploadedFromPath`) and
the version pair (`versionNumber` / `versionUploadedAt`, epic memql#4806).

The bytes go to blob storage under `library/{userId}/{fileId}/{name}` through
`integration.storage.upload` -- Azure Blob in the cloud, Azurite locally. Every
version after the first lands under `library/{userId}/{fileId}/{key}/{name}`
with a fresh key per upload, so no upload can ever write over another's bytes.

**`sha256` is a dedup hint and never an access key.** Knowing a file's hash
grants nothing; every read re-resolves ownership.

**The MemQL copy is independent and durable** (memql#4781, design D12). A file
uploaded from a machine survives that machine's deletion of the original and
survives the machine disconnecting; archiving in MemQL never touches the
origin. This is the invariant the future watched-folder backup builds on, and
it is why the copy here is deliberately NOT a data-origins mirror: a mirror
reconciles deletions, a backup retains and flags.

### Uploading -- `POST /artifacts`

Multipart, field `file`, with optional `name` and `labels` (comma-separated).
Bearer-authenticated, and the actor owns the result. Answers
`201 {artifactId, fileId}`.

**Any MIME type is accepted**, sniffed when the client does not say. An
unknown type is stored opaquely, never executed, and goes straight to
`status: ready` with no chunks.

The one-shot route also accepts `folderId` and the provenance trio
(`uploadedFromWorkerId` / `uploadedFromWorkerName` / `uploadedFromPath`) as
form fields -- see Upload provenance below.

**Two caps apply, and their refusals name their numbers.**

- `MEMQL_LIBRARY_MAX_UPLOAD_BYTES` (default 4294967296, 4 GiB) caps one
  FILE -- a one-shot body, or a chunked session's declared size. Over it is
  `413`. On the one-shot route enforcement is two-layer so a client's
  `Content-Length` stays a claim rather than the limit; on the chunked path
  the declared size is checked at init and the staged bytes must EQUAL it at
  complete. A value that is set but unparseable or non-positive falls back
  to the default: an unbounded upload route is the one outcome a
  misconfigured cap must never produce.
- `MEMQL_LIBRARY_USER_QUOTA_BYTES` (default 107374182400, 100 GiB) caps one
  USER's whole Library: stored file bytes -- **archived files keep counting;
  retention is real** -- plus the declared sizes of their open upload
  sessions, plus the new upload. Over it is `507`, naming the would-be total
  and the quota. It exists because a per-file cap alone cannot stop one
  person filling the account.

This is one of the documented HTTP exceptions to the gRPC-first policy. It is
an **authenticated** route, so it appears in none of `PublicPaths()`,
`HandlerAuthorizedPaths()` or `SelfAuthenticatedPaths()` -- exactly the class
`cmd/frontdoorpaths` exists to route.

### Chunked resumable uploads -- `POST /artifacts/uploads`

Files past the one-shot threshold arrive as sessions over Azure block-blob
staging (memql#4782, design D8): open a session, PUT 16 MiB chunks,
commit.

| Route | Purpose |
|---|---|
| `POST /artifacts/uploads` | Open a session: `{name, size, mimeType, folderId, labels, uploadedFrom*}` answers `201 {uploadId, chunkSize}`. The quota check and provenance verification happen HERE, before anyone streams gigabytes |
| `GET /artifacts/uploads/{id}` | The staged-chunk inventory -- what resume reads: `{uploadId, status, size, chunkSize, staged: [{n, size}]}` |
| `PUT /artifacts/uploads/{id}/chunks/{n}` | One raw chunk (16 MiB constant), streamed to a staged block. `n` runs 1..ceil(size/chunkSize); out-of-range is refused, so staging is bounded by the declaration. Re-staging the same `n` replaces it, which is what makes retries coordination-free |
| `POST /artifacts/uploads/{id}/complete` | Verify staged bytes == declared size (a mismatch is `409` and the session STAYS OPEN), commit the block list in order, create the rows, answer the one-shot's own `201 {artifactId, fileId}`. Completing a completed session answers the same ids with `200` -- the kill-after-commit resume case |

Three properties are load-bearing:

- **Replica-agnostic by construction.** The session row lives in the graph
  and staged blocks live with the blob, so ANY bff serves any chunk and
  completes any session. No replica holds session state in memory; a test
  completes one upload through two independent handler instances.
- **The session row IS the per-chunk authorization.** Every route resolves
  it under the caller's own actor, and row admission is the owner check --
  "not yours" and "not there" are one `404`.
- **No sweeper.** An abandoned session's staged blocks garbage-collect on
  Azure's ~7-day uncommitted-block clock; the row is inert. Clients purge
  their local resume records on the same clock.

`sha256` on a chunked file is stamped by the ANALYSIS pass, which streams
the committed blob once -- the handler never held the whole file. Until the
pass runs, the field is absent: "not measured", never "no hash".

### Exporting -- `GET /artifacts/{id}/content`

One route exports the whole Library.

- A **file** streams its bytes with `Content-Type`, `Content-Length` (from
  the ROW, not from buffering), `Accept-Ranges: bytes` and
  `Content-Disposition: attachment` -- constant memory through the bff, and
  a single-range `Range` header answers `206` with `Content-Range`
  (memql#4782, design C5). An unsatisfiable range is `416`; a malformed or
  multi-range header is ignored and the full body served, which RFC 9110
  permits.
- A **note**, **generated output** or **memory** renders its body as
  `text/markdown` or `text/plain` with a filename derived from the title.
- A **todo**, **calendar event**, **document** or **live source** has no body
  this route can render and answers `404`.

Ownership is re-checked per request, on the index row **and separately on the
backing row**. A denial is `404`, never `403` -- the same answer as "nothing
to export", so the response cannot be used to probe for rows.

There are **no redirects and no signed storage URLs.** Bytes come through the
bff. This design adds none.

### Upload provenance

Where a file came FROM is recorded only where it is honestly known
(memql#4781, design D5). The axis is called **provenance**; "origin" stays
reserved for the data-origins system (memql#4378).

- A **cockpit push** names the machine: `uploadedFromWorkerId` (the worker
  registration), `uploadedFromWorkerName` and `uploadedFromPath` ride the
  upload as form fields.
- A **browser upload** carries none of the three. A browser physically cannot
  name a machine or the dropped file's path, and nothing here guesses one
  from a user agent.

**Claims are verified, and a failed claim refuses the whole upload.** The
named registration must be one of the CALLER's own machines -- checked under
the caller's own actor, before any byte reaches storage -- and the stored
`uploadedFromWorkerName` is resolved from the registration row itself
(`displayName`, else the reported hostname), so the label the Files app
renders can never disagree with the fleet page. A silently-dropped claim
would render as "uploaded here", which is a lie; hence `403`, naming the
refused registration id.

Promotion forwards the machine id and name into the index's
`producedByWorkerId` / `producedByWorkerName`, whose meaning generalizes from
"computer_use only" to "when a machine is known". The **path stays on the
file row**: the index does not need it, and the sync epic reads the file row.

### Versions

A file can be replaced in place. The person **names the target** -- from that
file's own inspector, or with `targetArtifactId` on the upload -- and the
result is a new **version** of the same artifact: the same row in the Files
list, the same id, the same folder, the same labels, the same client tags.
What was there is **superseded**, never overwritten. Nothing this epic does
destroys bytes.

**The current version is the file row; every superseded one is a
`v1:library:fileVersion` row.** That split is the design (epic memql#4806,
design D1) rather than an implementation detail, and it is why the artifact
index, the content route and the analysis pass all still read exactly one row:
`sourceConceptRef` is written once at promotion and never re-points, so the
index's derived id and `libraryArtifactBySourceConceptRef` are untouched.

- **Uploading a version** -- `POST /artifacts` with `targetArtifactId` in the
  form, or `POST /artifacts/uploads` with `targetArtifactId` in the init body.
  Both answer `{artifactId, fileId, versionNumber}`, and both keep the ids
  they were given. The chunked path gates the target at INIT, before any
  chunk is staged.
- **Three refusals, all before a byte is stored.** A target that is not the
  caller's is `404` (the same answer as "not there", for the reason every
  refusal on these routes gives). A target backed by something other than a
  file is `400`, with a sentence saying that a document is versioned by
  editing it. Over quota is `507`.
- **Reading a version** -- `GET /artifacts/{id}/content?version={n}`. Omitted
  means the current one, which is what every caller written before versions
  sends; naming the current version's own number serves the same bytes with
  the same headers and the same `Range` support. A version the caller may not
  see does not resolve, and answers `404`.
- **The list** -- `libraryFileVersionsForFile` returns the superseded versions
  newest-first, paged at 200. The head's own `versionNumber` says how many
  versions exist, so a client that received fewer rows than that can say so
  rather than showing a prefix as if it were everything.

**Every version is a full citizen.** It re-runs the analysis pass on its own
bytes, carries its own verified `uploadedFrom*` provenance (never inherited --
a file first pushed from a laptop and later replaced from a browser has one
version that names the laptop and one that names nothing), keeps its own name,
size, hash and summary, and lands with the same arrival cue any other change
does.

**The quota counts every version.** Retention is real: superseded bytes are as
real as current ones, and a quota that ignored them would refuse a person
using numbers they cannot see anywhere. The `507` sentence says so.

**A version's `sha256` can be absent, and absent means "not measured yet".**
A chunked upload's handler never holds the whole file, so the head lands
without a hash and the analysis pass stamps it after streaming the committed
blob. Readers render a dash, never an error -- and the supersede writes that
blank EXPLICITLY, because leaving the previous version's hash in place would
be a false integrity claim rather than a missing one.

**What is not here.** No content diffing and no diff viewer; no restore-to-an-
earlier-version (download it and upload it as a new version, which keeps the
history honest); no automatic same-file detection for browser uploads --
identity is explicit here, and key-matched in the watched-folder epic (#4783),
which uses this same mechanism rather than a second one.

---

## Folders

The Files app's tree is the **Drive model, not POSIX** (memql#4781, design
D3): `v1:library:folder` rows plus a `folderId` pointer on the index. Ids,
never path strings. Sibling name duplicates are allowed -- a folder is a
collection, not a namespace -- so there is no uniqueness machinery and no
rename conflict. A move is one row update; a rename touches nothing else. No
permission tree exists: row authz is unchanged, and a folder confers nothing.

- **Writes:** `createLibraryFolder`, `renameLibraryFolder`,
  `moveLibraryFolder`, `archiveLibraryFolder`, and `moveArtifactToFolder`
  (a read-merge, so labels and archived survive a re-filing untouched). All
  client-reachable; ownership is stamped from the actor.
- **Reads:** `libraryFolders` (the whole owner tree, unbounded on purpose --
  the client-side fold needs every row at once) and `libraryFolderById`.
- **Live:** `graph.node.created/updated.v1:library:folder` carry broadcast
  routing rules, so the tree moves under a watching browser with no engine
  work per surface.
- **The index is the organizational truth.** `file.folderId` is the initial
  filing only; after promotion, moves update the index and never chase the
  backing row.
- **Depth (12) and cycle refusal are the client's in v1** (design D11); the
  client's tree fold is cycle- and orphan-TOLERANT anyway, rendering a
  violating folder at root with a marker, so a bad write degrades the
  picture without breaking it. Server-side cascade + integrity guard is a
  recorded hardening follow-up.
- **Archive is client-driven and children-first** (design B5): contents via
  `archiveArtifact` (whose automation archives backing files), then folders.
  Idempotent under interruption -- re-running archives the remainder.

---

## Search by meaning

After an upload, a detached analysis pass runs: extract text for the known
types, write a summary, chunk at roughly 1800 characters with 180 of overlap,
and embed each chunk into `v1:library:fileChunk`. The file moves
`stored` -> `analyzing` -> `ready`, or `failed` **with the reason on the row**
-- never a silent partial. `embeddingStatus` (`none` / `partial` / `complete`)
is how a caller tells "not similar" from "not indexed yet".

`librarySimilarArtifacts(text | artifactId, limit)` runs the similarity read
over those chunks under the caller's own actor and folds the hits to artifacts
by best score.

> **WARNING: the candidate pool is an approximation.** The underlying
> `similarTo` applies no per-row authorization and cannot be told whose rows to
> consider, so the pool is widened (20x the limit, bounded 100-500) and then
> filtered to the caller. A caller whose best hit falls below the pool cut-off
> still misses it. Making this exact needs an owner-aware vector read in
> `integrations/similarity`.

Chunks are `v1:library:fileChunk` and deliberately **not**
`v1:knowledge:documentChunk`: that concept requires a knowledge domain, and
the domain concept is not declared in the engine tree.

---

## Train into a domain

**Uploading a file does not train it.** A file is yours the moment it lands;
putting it into a knowledge domain is a separate, recorded decision, and
nothing is trained by default.

`libraryTrainFile(fileId, domainId)` ingests the file's text into the named
domain with `sourceRef: artifact:<artifactId>`, appends the domain to the
file's `trainedIntoDomainIds`, and audits it.

> **WARNING: the domain-write check is a seam, and it is deny-by-default.**
> `knowledgeDomain` is declared in no `.memql` file in this tree, so the engine
> cannot read the row or lean on a row-authz tier to decide who may write to
> it. `libraryTrainFile` therefore delegates to a
> `library.DomainWriteAuthorizer`, allows a cluster owner unconditionally, and
> **refuses everyone else when nothing is wired**. Nothing in this repository
> wires it, so on the engine alone only an operator can train. A product that
> owns the knowledge concepts must supply the authorizer.
> Permissive was rejected deliberately: `integration.knowledge.ingest`
> performs no authorization of its own.

Training reconstructs the text from the stored chunks (overlap-aware) rather
than re-extracting from the blob, because the plug-in context exposes no blob
download. The bias is one-directional -- a missed seam repeats a little text,
never deletes any.

---

## Archive

`archiveArtifact(artifactId)` is the first write that removes something from
the Library. It is a **soft** delete: the engine is append-only by
`(id, createdAt)`, so an archive is a re-version carrying `archived: true`.
History, labels and `sourceConceptRef` all survive. When the artifact is
file-backed, the backing `v1:library:file` is archived with it.

`libraryArtifacts()` excludes archived rows; the portal offers a toggle.

> **INFO: the list filter is spelled `archived != true`, not
> `archived == false`, and the difference is load-bearing.** Measured against
> Postgres, `!= true` keeps rows whose payload has no `archived` key at all --
> every artifact promoted before the field existed. The equality spelling would
> drop them, emptying every existing Library on deploy.
>
> The Bin's own read spells it the other way -- `libraryArchivedArtifacts`
> filters `archived == true` -- and that asymmetry is correct rather than an
> oversight. "Not archived" has to be null-safe because a row with no key is
> not archived; "IS archived" is a positive test, and the same row genuinely
> fails it. The two are inverses in meaning and not in spelling.

### Restore (memql#4784)

`restoreArtifact`, `restoreLibraryFile` and `restoreLibraryFolder` take things
back out. Nothing was destroyed, so nothing is reconstructed: each is a
re-version carrying `archived: false`, and the row arrives with its labels, its
folder, its provenance and every earlier version exactly where they were.

**The pair is CLIENT-DRIVEN, and that is not a shortcut.** Archiving an
artifact archives its backing file through `archiveFileOnArtifactArchive`; the
mirror of that automation cannot exist, because it would ride `node.updated`
filtered on `archived == false` -- essentially every artifact update -- and
together with the archive automation the two close a cycle where each write
publishes an event the other subscribes to. So the Bin calls both mutations,
index first, exactly as the recursive archive walk calls its own writes.
Re-running an interrupted restore is idempotent: whatever landed is simply
absent from the next plan.

**A folder comes back EMPTY.** Everything that was inside it is its own row and
its own decision, which is what lets somebody take one file back without
undoing everything they filed away.

**Nothing expires.** There is no retention sweep, no quota-driven cleanup and
no expiry anywhere in the product: archived items accumulate and are kept until
somebody acts. Archived file bytes DO count against the user quota (see
`StorageFootprint`), which is the one place the accumulation is visible.

---

## Origin link states (epic memql#4783)

A file pushed from a fleet machine carries `uploadedFromWorkerId` and
`uploadedFromPath`, and those two together are the key a re-push resolves on:
uploading the same path from the same machine again versions the SAME logical
file rather than adding a row. Both upload routes do it -- and the chunked one
matters more, because a watched folder of client video is entirely above the
one-shot threshold.

`linkState` records how the copy stands against that machine:

| State | Means |
|---|---|
| absent | No origin link. A browser upload has none to have. |
| `synced` | Matched the origin when it was last checked. |
| `stale` | The machine has newer bytes that have not arrived. |
| `origin_gone` | Not at that path any more, or the machine is unreachable. |

**Absent is not a fourth state.** "We do not track this file" and "we track it
and it is fine" are different answers, and a reader that collapsed them would
badge the whole Library as in sync.

**The engine writes exactly one of them.** An upload naming a `(machine, path)`
is stamped `synced` -- at the instant those exact bytes arrived the copy did
equal the origin, which is the strongest evidence there is. The other two are
answers only something looking at the origin can give, so the cockpit's verify
lane reports them through `setLibraryFileLinkState`; `linkCheckedAt` says when.

**It only ever FLAGS.** A deletion or a disconnection at the origin sets
`origin_gone` and never archives, moves or deletes the copy. That is the whole
point of a backup, and it is why this is deliberately not a data-origins mirror
(memql#4378): a mirror reconciles deletions, a backup retains and flags.

**A re-push after archiving starts a fresh file.** The key read excludes
archived rows, so new bytes never land in a row nobody can see -- which would
show the person nothing arriving while the backup reported success.

---

## Agent tools

Both run as the person the agent is acting for, never with wider reach.

| Tool | What it does |
|---|---|
| `artifactSearch` | Looks through that person's own Library by meaning. Takes `text` or `artifactId`, plus a `limit` (default 5). |
| `artifactTrain` | Trains one of their files into a knowledge domain. Takes the `fileId` (not the artifact id) and a `domainId`. Refused when the file is not theirs, when it has no readable text, or when they may not write to that domain. |

`artifactTrain` is for when the person asks. Uploading is not training, and an
agent should say which file and which domain it is about to use before calling
it.

---

## Promotion, and one thing to know before changing it

A file becomes an index row through the `indexFileOnCreate` automation, which
carries `@filter(payload.status == "stored")`.

**That filter is load-bearing.** `graph.node.created` fires on every write, not
only the first, so without it every `setLibraryFileStatus` would re-promote the
file -- and because `createArtifact`'s body is a bare `insert{}` that drops
unnamed fields, each re-promotion would wipe the artifact's labels. It is also
why `createLibraryFile` stamps `status` itself rather than accepting it as an
argument.

For the same reason, the analysis pass **re-stamps** the index row itself when
it finishes, carrying `labels` and `archived` forward. Anything else that
updates an artifact must do the same.

The index's `source` enum is the **union** of every backing concept's own
`source` enum, and has to be: the promotions pass the backing value straight
through, so a value a backing row can hold and the index cannot is a promotion
that refuses at execute time -- the row keeps its bytes, gets no index row, and
never appears in the Library.
`TestEveryBackingSourceValueIsPromotable` pins the containment.

---

## Related

- [Deployables](deployables.md) -- publishing a zip artifact as a hosted site.
- [Front door](front-door.md) -- how a site hostname gets a certificate.
- Sub-project J (commerce memory: orders, line items and inventory mirroring by
  webhook with Admin-API reconciliation) has its own record. Nothing about
  orders or inventory is stored by the Library or by a storefront deployable.
