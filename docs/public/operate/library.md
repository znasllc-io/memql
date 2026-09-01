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
the initial filing (`folderId`) and the upload provenance trio
(`uploadedFromWorkerId` / `uploadedFromWorkerName` / `uploadedFromPath`).

The bytes go to blob storage under `library/{userId}/{fileId}/{name}` through
`integration.storage.upload` -- Azure Blob in the cloud, Azurite locally.

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

The cap is `MEMQL_LIBRARY_MAX_UPLOAD_BYTES`, default 268435456 (256 MB) --
sized so a site bundle fits, since the deployables path publishes from one.
Over the cap is `413`, enforced in two layers so that a client's
`Content-Length` is treated as a claim rather than as the limit. A value that
is set but unparseable or non-positive falls back to the default: an
unbounded upload route is the one outcome a misconfigured cap must never
produce. The in-memory multipart threshold is a separate fixed 32 MB and
deliberately does not scale with the cap.

This is one of the documented HTTP exceptions to the gRPC-first policy. It is
an **authenticated** route, so it appears in none of `PublicPaths()`,
`HandlerAuthorizedPaths()` or `SelfAuthenticatedPaths()` -- exactly the class
`cmd/frontdoorpaths` exists to route.

### Exporting -- `GET /artifacts/{id}/content`

One route exports the whole Library.

- A **file** streams its bytes with `Content-Type`, `Content-Length` and
  `Content-Disposition: attachment`.
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
