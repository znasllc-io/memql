# Archiving an approved release candidate that has no catalog -- Design

- **Date:** 2026-11-08
- **Status:** approved (in-session with the owner: the proof rule was chosen
  from three options, then this design was approved section by section)
- **Scope:** one owner-only builtin, `releaseArchiveWithoutCatalog`, in
  `integrations/release`; one migration on `release_candidate_archives`; the
  publications view's archived-state check; documentation in
  `docs/public/operate/release-candidates.md` and the Retention section of
  `docs/public/operate/pipelines-substrate.md`.
- **Not in scope:** a Releases screen action, automatic use by the 48-hour
  schedule, deleting the orphaned GitHub draft for v0.29.22, retiring the nine
  never-approved `ready` candidates (an existing owner action).

## Why

A release candidate pins its evidence files and every component artifact in
`pipeline_artifact_references` until it reaches a terminal state. Two
terminal transitions exist, and neither is open to an approved candidate that
has no signed catalog:

- `releaseRetireCandidate` refuses any candidate with a publication or draft
  intent (`candidateLedger.change`), because an intent means an external effect
  may have happened.
- `releaseArchiveCandidate` and the scheduled `sweepPublishedReleaseInputs`
  require a signed `release_catalog` row (`candidateArchiveCatalog`; the sweep's
  scan joins `release_catalog`).

On production, six approved candidates from 2026-10-17 to 10-21 pin 380
uploads, 6.0 GB of blob, that the pipeline artifact sweep reports retained
forever. A read-only measurement of production on 2026-11-08 found three
shapes, not one:

| Candidate | What it is | Publication state | Pins |
|---|---|---|---|
| `094315f3` | engine 0.29.22 to ACR, 7 images | 1 push pending since 10-17 (agent), 6 never begun | 66 uploads (1.5 GB), shared by all three 0.29.22 candidates |
| `d416cdbe` | engine 0.29.22 to GHCR, 7 images | 1 push pending (agent), tag absent on GHCR, 6 never begun | the same 66 |
| `ef8d916e` | engine 0.29.22 GitHub release files | 8 assets uploaded; release v0.29.22 is still a draft | the same 66 |
| `b7bb0d59` | engine 0.29.31 to GHCR, 7 images | 7 of 7 complete, tags present | 136 (2.0 GB) |
| `f72a6c76` | engine 0.30.4, the runtime-repair workbench image (digest only) | 1 of 1 complete | 136 (2.0 GB) |
| `8cc989db` | cockpit 0.20.1 GitHub release files | 18 of 41 under its approval; renewed by `78458cb1`, which finished the release and was archived with a catalog on 10-22 | 42 (0.7 GB) |

So engine 0.29.22 was never published; two candidates were completely
published but never sealed; and one was superseded by a draft renewal. The
renewal case is general: a superseded candidate can never seal (the draft
transaction refuses it and its draft binding moved to the successor), so every
future renewal leaves its predecessor pinned the same way.

## The decision the owner made

Without a catalog, the proof is **supersession plus fresh re-verification of
what was actually published**:

1. Every component the candidate contains has a later or equal release that
   carries a signed catalog.
2. Every copy the candidate actually published is re-read at its approved
   destination before the fence.
3. Nothing can still be running.

The alternatives were rejected: "published copies only" would have left the
never-published 0.29.22 trio pinned for good; "seal late where possible" would
have added two October entries to the catalog that installations discover,
one of them a lone runtime-repair image. Supersession is also what keeps the
operation safe for the future: the newest release of a component never has a
later catalogued release, so it can never skip its catalog through this path.

## Design

### 1. The operation

`releaseArchiveWithoutCatalog(candidateId)` is an `@sdk` builtin backed by
`integration.release.archiveWithoutCatalog`. Owner only: the owner wall
precedes every port, as in archival. It is an explicit owner action; the
48-hour schedule never calls it, and its scan still joins `release_catalog`.
A candidate that has a catalog is refused with a message naming
`releaseArchiveCandidate`, so this operation never replaces ordinary
archival.

### 2. Eligibility (snapshot: one short transaction under the candidate row lock)

- The caller's own candidate, `approved` (or already `archived` by this
  operation, which resumes the reference release), with a consistent approval.
- No `release_catalog` row for it.
- **Supersession.** For every component in its manifest, another candidate of
  the same owner has a signed catalog that verifies under the configured
  publisher trust and contains a component of the same name at the same or a
  higher version (numeric per part, `normalizeVersion` and `newer`). The same
  version counts: a renewal successor, or a later attempt that did publish.
  The component is read from the verified catalog payload, never from the
  superseding candidate's manifest.
- **Nothing still running.** No live draft upload lease; no pending
  publication intent, and no draft in `creating` or `promoting`, updated within
  the last two hours. The longest publication operation is bounded at 70
  minutes (`releasePublicationOperationLimit`) and both lifecycle workflows at
  one hour; a test holds the quiet period above the longest bound.

### 3. Fresh verification (no lock held)

A destination that needs a read is resolved exactly as archival resolves it:
through the release profile the candidate is bound to, or otherwise the
current configuration, at the digest the candidate was approved with. One that
is no longer authorized refuses the whole operation, as it refuses archival.
A destination that needs no read is never resolved, so a target removed from
the configuration does not block recording that nothing was published there.
The one exception, recorded after review: archival resolves a release profile's
whole selection at once, so a profile-bound candidate that has a copy to read
needs its profile to still authorize every destination it selected, read or
not. Narrowing that resolution to the destinations read was not done.
The release a file publication went to is identified from the target frozen
in its intent (`target_snapshot`), which says whether that release was ever
published before any current configuration is consulted.

| Destination's state | Verification | Recorded as |
|---|---|---|
| Complete registry publication | OCI availability by image digest: every manifest, config and layer byte | `verified` |
| Complete npm publication | npm observation of the exact tarball digest and size | `verified` |
| Complete file publication whose release is `published` (bound to this candidate, or to the successor that renewed it) | the whole release read back by bytes, once per release, as archival does | `verified` |
| Complete file publication whose release was never published | none | `uploaded` |
| Pending intent | none | `pending` |
| No intent | none | `notBegun` |

A renewed candidate's draft binding belongs to its successor. The plan of an
unchanged file release is identical for both by construction
(`unchangedFileRelease`), so the binding is read under the successor's
approval with the predecessor's plan, and accepted only when the succession
journal links the two.

### 4. The fence (second short transaction)

It re-reads the candidate, its approval, catalog absence, the superseding
catalogs, and every intent and draft state, and refuses if any differs from the
snapshot or if the observation is older than five minutes (or in the future).
It writes the `release_candidate_archives` row with `basis = 'supersession'`,
no catalog digest, and the proof (the per-destination outcomes, the verified
targets, and the superseding candidate for each component); then sets the
state to `archived`. Only then are this candidate's own references released
and `inputs_released_at` recorded. An interrupted run leaves the fence; a
rerun on any replica sees the `archived` state and resumes the reference
release without reading a provider.

### 5. Schema and readers

- **Migration.** `release_candidate_archives.catalog_digest` becomes
  nullable; a `basis` column (`catalog` by default, or `supersession`) is
  checked against digest presence. The down migration refuses while a
  `supersession` row exists, as the original refuses while archive evidence
  exists.
- **Readers.** `readCandidateArchive` reads only the `catalog` basis.
  `releaseCandidatePublications` keeps "an archived candidate's intents are all
  complete" for the catalog basis and allows pending intents on the
  supersession basis. The OS Releases screen needs no change: the state is
  still `archived` and its notice is still true.

### 6. What stays unchanged

No remote write. Registry tags and reserved tag keys, drafts (including the
GitHub draft for v0.29.22), approvals, publication intents and receipts,
catalogs and successions are untouched. Only this candidate's references are
released, so uploads shared with another live candidate stay until that one is
terminal too: the 0.29.22 trio frees its 66 uploads only once all three are
archived. Once released, the pipeline artifact sweep's next visit retires the
files, whose windows have long passed.

## Testing

Database-gated tests in `integrations/release`, using the existing fixtures,
with two or more hosts over one database (`freshDraftHost`):

- the owner wall precedes every port;
- refusals: a catalogued candidate; no superseding catalog; a superseding
  catalog of a lower version, another owner, or one failing trust; a pending
  intent inside the quiet period; a live upload lease; a change between the
  snapshot and the fence; a stale or future proof;
- a complete registry publication: an unavailable registry refuses and keeps
  the pins; an available one archives, releases only this candidate's
  references, and afterwards prepare, approve, publish and begin-publication
  refuse and a delayed pin is refused by storage;
- pending and never-begun destinations archive with no provider read, and the
  publications view still lists the pending intent;
- a renewal predecessor verifies its successor's published release, and a
  corrupted asset refuses;
- three hosts racing produce one fence row; an interrupted reference release
  resumes on another host without reading a provider;
- the down migration refuses while a supersession row exists.

Then `make test` and the database-gated trees at `-p=1` on a fresh database.

## Rollout

The builtin reaches production with the next release. Running it for the six
needs the owner's word in the session that runs it, then the same read-only
measurement confirms the references are released; the sweep retires the files
on its next visits to their producers.
