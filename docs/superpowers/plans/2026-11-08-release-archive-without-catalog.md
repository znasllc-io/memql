# Archive Without Catalog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An owner-only `releaseArchiveWithoutCatalog(candidateId)` that releases the build inputs of an approved release candidate with no signed catalog, on the proof of supersession plus fresh re-verification of what it published.

**Architecture:** Same three-phase shape as `releaseArchiveCandidate` (`integrations/release/candidate_archive*.go`): a short snapshot transaction under the candidate row lock, network verification with no lock held, and a fence transaction that re-reads everything, writes the archive journal row and sets `archived`; only then are the candidate's own references released. The journal row carries `basis = 'supersession'` and no catalog digest.

**Tech Stack:** Go, PostgreSQL (`database/sql`), the existing `ociregistry` / `githubrelease` / `npmregistry` verifiers, MemQL DSL builtin, generated SDKs.

**Spec:** `docs/superpowers/specs/2026-11-08-release-archive-without-catalog-design.md`

## Global Constraints

- Owner only; the owner wall (`candidateOwner`) precedes every database or provider port.
- Never callable by the 48-hour schedule; `nextCandidateArchive` keeps its `JOIN release_catalog`.
- A candidate with a `release_catalog` row is refused with a message naming `releaseArchiveCandidate`.
- Supersession: for every manifest component, another candidate of the same owner has a catalog that verifies under the configured publisher trust and contains a component of the same name at the same or a higher version (`normalizeVersion`, `newer`), read from the verified payload.
- Quiet period: `2 * time.Hour`, held above `releasePublicationOperationLimit` (70 minutes) by a test.
- Proof freshness: observation no older than 5 minutes and not in the future at the fence.
- No remote write. Tags, reserved tag keys, drafts, approvals, publication intents and receipts, catalogs and successions are never modified.
- Only `candidateArtifactReferences(record.Manifest, record.ID)` are released, through `ReleaseRunFileReference` under internal origin.
- Destinations that need a read are resolved through `archivalCandidateFor` (profile binding, else current configuration) at the approved digest; destinations that need no read are never resolved.
- Stage files by explicit path; tests run with `MEMQL_REQUIRE_DB=1` and `-p=1` on a fresh database; never `go test ./...`.

## Review Focus

- A destination whose current configuration changed since approval: refused only when it needs a read (complete publication); a pending or never-begun one still archives. Test owned by Task 4.
- The owner seals the candidate between snapshot and fence: the fence sees the catalog and refuses; sealing after the fence is refused by `draftTransaction`. Test owned by Task 5.
- A renewal chain of two hops (A renewed by B renewed by C, draft bound to C): A resolves its release through the chain. Test owned by Task 3.
- A quiet-period boundary: a pending intent updated 1 h 59 min ago refuses, 2 h 1 min ago archives. Test owned by Task 3.
- Re-running on a candidate archived with the `catalog` basis refuses and writes nothing. Test owned by Task 5.

---

### Task 1: Archive basis migration and catalog-only readers

**Files:**
- Create: `component/database/memory-nodes/migrations/20261108210000_release_archive_supersession.up.sql`
- Create: `component/database/memory-nodes/migrations/20261108210000_release_archive_supersession.down.sql`
- Modify: `integrations/release/candidate_archive_ledger.go` (`readCandidateArchive`)
- Modify: `embed_inventory_test.go` (component/database count +2, measured with `go list`, noted like its neighbours)
- Test: `integrations/release/candidate_archive_uncatalogued_db_test.go`

**Interfaces:**
- Produces: column `release_candidate_archives.basis text NOT NULL DEFAULT 'catalog'`, nullable `catalog_digest`, constraint `release_candidate_archives_basis_check`: `basis IN ('catalog','supersession') AND ((basis = 'catalog') = (catalog_digest IS NOT NULL))`.

- [ ] **Step 1: Write the failing tests**

`TestArchiveBasisPairsCatalogDigestWithCatalogBasis`: on a prepared+approved candidate (`candidatePublicationFixture`, `preparePublication`), inserting an archives row with `basis='supersession'` and a catalog digest fails; `basis='catalog'` with NULL digest fails; `basis='supersession'` with NULL digest succeeds (inside a rolled-back transaction).
`TestArchiveSupersessionRollbackRefusesWhileEvidenceExists`: with a supersession row inserted in a transaction, executing the down file in that transaction fails with an error containing `release archive supersession evidence is not empty`.

- [ ] **Step 2: Run to verify they fail**

Run: `MEMQL_REQUIRE_DB=1 go test -count=1 -p=1 -run 'TestArchiveBasis|TestArchiveSupersessionRollback' ./integrations/release/`
Expected: FAIL (column `basis` does not exist).

- [ ] **Step 3: Write the migration pair and the reader filter**

Up: drop NOT NULL on `catalog_digest`, add `basis`, add the constraint, with a comment saying why (the supersession basis has no catalog). Down: a `DO` block raising `release archive supersession evidence is not empty` while any `basis <> 'catalog'` row exists, then drop constraint and column and restore NOT NULL. `readCandidateArchive` adds `AND basis='catalog'` so a supersession row is never read as catalog evidence.

- [ ] **Step 4: Run the tests and the inventory gate**

Run: `MEMQL_REQUIRE_DB=1 go test -count=1 -p=1 -run 'TestArchiveBasis|TestArchiveSupersessionRollback|TestCandidateArchive' ./integrations/release/` then `go test -count=1 -run TestEmbed . `
Expected: PASS.

- [ ] **Step 5: Commit** (`git add` the four paths above).

### Task 2: Supersession by a verified catalog

**Files:**
- Create: `integrations/release/candidate_archive_supersession.go`
- Test: `integrations/release/candidate_archive_supersession_test.go` (unit), `integrations/release/candidate_archive_uncatalogued_db_test.go`

**Interfaces:**
- Produces: `func catalogSupersedes(published pl.PublishedRelease, component pl.ReleaseComponent) bool` and `func supersedingCatalogs(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord, publisher string, trust map[string]ed25519.PublicKey) (map[string]string, error)` returning component name to superseding candidate id, or an error naming the first component with none.

- [ ] **Step 1: Write the failing tests**

Unit `TestCatalogSupersedesSameOrHigherVersionOfTheSameComponent`, table over `engine 0.25.0`: catalog `engine 0.25.0` true, `0.25.1` / `0.26.0` / `1.0.0` true, `0.24.9` false, `cockpit 0.25.0` false, malformed `0.25` false.
DB `TestSupersedingCatalogsCountOnlyTheOwnersVerifiedCatalogs`: candidate A approved, no catalog anywhere for its key: error. Candidate B (second `candidatePublicationFixture`) published and sealed with `sealArchiveFixture`: returns `{"engine": B}`. With B's `release_candidates.owner_user_id` changed to another owner: error. With a trust map holding a different key: error.

- [ ] **Step 2: Run to verify they fail** (undefined functions).

- [ ] **Step 3: Implement**

`catalogSupersedes` compares names and `!want.newer(got)` after `normalizeVersion` on both. `supersedingCatalogs` reads, per component, at most 256 `release_catalog` rows of this publisher joined to `release_candidates` of this owner, excluding `rec.ID`, newest first, narrowed by `manifest->'components' @> [{"name": …}]`; verifies each envelope with `pl.VerifyPublishedRelease`, requires the projection's `CandidateID` to equal the row, skips rows that fail, and decides from the verified payload only. Comment why the manifest narrowing can only cause a refusal, never an admission.

- [ ] **Step 4: Run to verify they pass.**

- [ ] **Step 5: Commit.**

### Task 3: Snapshot and classification

**Files:**
- Create: `integrations/release/candidate_archive_uncatalogued_ledger.go`
- Test: `integrations/release/candidate_archive_uncatalogued_db_test.go`

**Interfaces:**
- Consumes: `supersedingCatalogs` (Task 2).
- Produces:
  - `const uncataloguedQuietPeriod = 2 * time.Hour`
  - outcomes `"verified"`, `"uploaded"`, `"pending"`, `"notBegun"`
  - `type candidateUncataloguedDestination struct { TargetID, Component, Artifact, Outcome string }` (JSON `targetId`, `component`, `artifact`, `outcome`)
  - `type candidateUncataloguedSnapshot struct { record candidateRecord; destinations []candidateUncataloguedDestination; bound map[string]candidateDraftRecord /* targetId -> published draft to read */; supersededBy map[string]string; observedAt time.Time }`
  - `func classifyUncatalogued(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord) ([]candidateUncataloguedDestination, map[string]candidateDraftRecord, error)`, sorted by target, component, artifact; used by snapshot and fence alike
  - `func (l *candidateLedger) uncataloguedSnapshot(ctx context.Context, key, publisher string, trust map[string]ed25519.PublicKey) (candidateUncataloguedSnapshot, error)`

- [ ] **Step 1: Write the failing tests**

`TestArchiveWithoutCatalogQuietPeriodExceedsEveryPublicationBound`: `uncataloguedQuietPeriod > releasePublicationOperationLimit` and `> time.Hour`.
`TestUncataloguedSnapshotRefusesBeforeAnyProviderRead`, subtests, each asserting registry reads unchanged and pins intact: `ready` (not approved), `retired`, `catalogued` (error contains `releaseArchiveCandidate`), `no supersession`, `pending 1h59m` (begin publication, set `updated_at` to now minus 1 h 59 min; error contains `may still be running`), `live upload lease` (own draft row `upload_lease_until` in the future).
`TestUncataloguedClassificationRecordsEveryDeclaredDestination`: pending aged 2 h 1 min is `pending`; a destination with no intent is `notBegun`; a complete registry publication is `verified`; a complete file publication on a `ready` draft is `uploaded`; on a `published` draft it is `verified` with the draft in `bound`.
`TestUncataloguedClassificationFollowsATwoHopRenewal`: A renewed by B renewed by C, draft bound to C and published: A's file destination is `verified` with C's draft in `bound`; a draft bound to a candidate outside the chain refuses.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**

Snapshot transaction: `readCandidate` (FOR UPDATE) and `readCandidateApproval`; state must be `approved`; no catalog row; `supersedingCatalogs`; no own draft with a live upload lease; no pending intent or `creating`/`promoting` draft with `updated_at` inside the quiet period (by the database clock); `classifyUncatalogued`; `clock_timestamp()` as `observedAt`. Classification reads each intent by exact destination and approval, decodes a complete file intent's `target_snapshot` to compute `draftResourceKey`, reads that draft row, and accepts its candidate only if it is this candidate or reachable through `release_candidate_successions` (at most 16 hops); a published draft is read with `readDraft` under its own candidate and approval with this candidate's plan rebuilt from the frozen target. Comment the quiet period's bound and why a renewal predecessor's plan is the successor's.

- [ ] **Step 4: Run to verify they pass.**

- [ ] **Step 5: Commit.**

### Task 4: Fresh verification

**Files:**
- Modify: `integrations/release/candidate_archive.go` (extract the per-destination body of `verifyPublishedInputs`)
- Create: `integrations/release/candidate_archive_uncatalogued.go`
- Test: `integrations/release/candidate_archive_uncatalogued_db_test.go`

**Interfaces:**
- Consumes: `candidateUncataloguedSnapshot` (Task 3).
- Produces:
  - `func (p *candidatePreparer) verifyPublishedDestination(ctx context.Context, record candidateRecord, destination pl.ReleaseDestination, bound *candidateDraftRecord, checkedFiles map[string]bool) (candidateArchiveTarget, error)`, used by `verifyPublishedInputs` unchanged in behaviour
  - `type candidateSupersessionProof struct { CandidateID, ApprovalID string; ObservedAt time.Time; Destinations []candidateUncataloguedDestination; Targets []candidateArchiveTarget; SupersededBy map[string]string }` (JSON camelCase)
  - `func (p *candidatePreparer) verifyUncataloguedInputs(ctx context.Context, snapshot candidateUncataloguedSnapshot) (candidateSupersessionProof, error)`

- [ ] **Step 1: Write the failing tests**

`TestArchiveWithoutCatalogRefusesAnUnavailablePublishedImage`: complete registry publication, superseded, `provider.unusable`: error, pins intact, state `approved`.
`TestArchiveWithoutCatalogReadsNoProviderForUnpublishedEffects`: aged pending plus never-begun: proof has no targets and zero registry reads.
`TestArchiveWithoutCatalogVerifiesARenewedPredecessorThroughItsSuccessor`: renewal fixture (as `TestCandidateDraftRenewalAfterEngineUpgradePreservesEffectsAcrossHosts`), successor promoted and sealed; a corrupted asset refuses; restored, the proof verifies the successor's release once.
`TestArchiveWithoutCatalogResolvesOnlyDestinationsItReads`: change the current target configuration's digest for a never-begun destination: proof succeeds; for the complete one: refuses.
Existing `TestCandidateArchive*` stay green.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**

Move the switch body of `verifyPublishedInputs` into `verifyPublishedDestination`, which takes the bound draft instead of searching `drafts` (archive keeps its ambiguity check while finding it). `verifyUncataloguedInputs` calls it only for `verified` destinations, passing `snapshot.bound[targetId]` for files, and fills the proof from the snapshot.

- [ ] **Step 4: Run to verify they pass** (new tests and `-run TestCandidateArchive`).

- [ ] **Step 5: Commit.**

### Task 5: Fence, reference release and the builtin

**Files:**
- Modify: `integrations/release/candidate_archive_uncatalogued_ledger.go`, `integrations/release/candidate_archive_uncatalogued.go`
- Modify: `integrations/release/release.go` (capability `archiveWithoutCatalog`)
- Modify: `integrations/release/candidate_effects.go` (pending intents allowed on the supersession basis only)
- Modify: `dsl/cluster/builtins.memql` (`@sdk` `releaseArchiveWithoutCatalog { candidateId string! }`, executor `integration.release.archiveWithoutCatalog`)
- Regenerate: `sdk/go/client/generated_builtins.go`, `sdk/ts/src/client/generated_builtins.ts` (`make sdk-gen`)
- Test: `integrations/release/candidate_archive_uncatalogued_db_test.go`

**Interfaces:**
- Consumes: Tasks 3 and 4.
- Produces: `func (l *candidateLedger) archiveWithoutCatalog(ctx context.Context, snapshot candidateUncataloguedSnapshot, proof candidateSupersessionProof, publisher string, trust map[string]ed25519.PublicKey) (candidateRecord, error)`, `func (p *candidatePreparer) archiveWithoutCatalog(ctx context.Context, key, publisher string, trust map[string]ed25519.PublicKey) (candidateRecord, error)`, `func (i *Integration) handleCandidateArchiveWithoutCatalog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error)`.

- [ ] **Step 1: Write the failing tests**

`TestArchiveWithoutCatalogOwnerWallPrecedesPorts` (as the archive one, for all three entry points).
`TestArchiveWithoutCatalogFencesOnceAcrossReplicasAndReleasesOnlyItsReferences`: three `freshDraftHost` hosts race: one archives row, `basis='supersession'`, NULL digest; `library.released[candidate]`, `library.pins["another-candidate"]` kept; no registry writes; afterwards prepare, approve, publish, ready, begin-publication and `sealCatalog` refuse, a delayed `PinRunFileReceipts` is refused, `effects` still lists every intent including a pending one, the superseding catalog still verifies.
`TestArchiveWithoutCatalogResumesReferenceReleaseOnAnotherHost`: `interruptedArchiveLibrary` leaves `archived` with pins; another host with `provider.unusable` completes without provider reads and records `inputs_released_at`.
`TestArchiveWithoutCatalogProofCannotCrossItsSnapshot`, subtests: owner, candidate, approval, stale (6 min), future, missing target, extra target, wrong digest, changed outcome.
`TestArchiveWithoutCatalogRefusesAJournalChangedSinceItsSnapshot`: between snapshot and fence, (a) a never-begun destination begins publication, (b) the candidate is sealed: each refuses and keeps pins.
`TestArchiveWithoutCatalogRefusesACatalogArchivedCandidate`: after `archive`, calling it refuses and the archives row count stays 1.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**

Fence: re-read candidate and approval under the row lock; refuse unless `approved` (or `archived` with a `supersession` row, which returns the record for resume); refuse if a catalog row exists; recompute `supersedingCatalogs` and `classifyUncatalogued` and require both equal to the snapshot; check the proof matches the snapshot (candidate, approval, outcomes, a target with the approved digests for exactly the `verified` destinations) and freshness; insert the row (`basis='supersession'`, NULL digest, `verified_at = proof.ObservedAt`, receipt = proof); set `archived`. Preparer: owner wall, nil-port refusal, 5-minute context like `archive`, resume path, then release references and `recordArchiveInputsReleased`. Handler: `archivalCandidateFor`, `i.catalog`, preparer. Effects view: read the basis once for an archived candidate. Builtin doc comment states the three conditions, that nothing remote changes and that the candidate can never prepare, publish or be catalogued again; capability description likewise. Run `make sdk-gen`.

- [ ] **Step 4: Run to verify they pass**, plus `go test -count=1 -run 'TestReleaseIntegrationRegistersLoadedExecutors' ./integrations/release/` and `make sdk-gen-check`.

- [ ] **Step 5: Commit.**

### Task 6: Documentation

**Files:**
- Modify: `docs/public/operate/release-candidates.md` (new subsection after the archival paragraphs, before the schedule paragraph)
- Modify: `docs/public/operate/pipelines-substrate.md` (Retention: the release-candidate consumer row and the paragraph that says these inputs stay)

- [ ] **Step 1: Write the release-candidates section**: when to use it; the three conditions; the outcome table; what stays unchanged; resume; that shared uploads leave only when every candidate pinning them is terminal; that it never replaces sealing for the newest release.
- [ ] **Step 2: Update the Retention row and paragraph** to name the supersession path and remove "keeps its inputs".
- [ ] **Step 3: Run the docs gates**: `go test -count=1 -run 'Docs|Public' .` and `go test -count=1 -run TestNoDatabaseProductClaims .`
- [ ] **Step 4: Commit.**

### Task 7: Verify the whole change and push

- [ ] **Step 1:** Create a fresh database in `memql-testdb` and run `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<fresh> go test -count=1 -p=1 ./integrations/release/...`; then every tree from `scripts/ci/db-gated-packages.sh --trees` at `-p=1`.
- [ ] **Step 2:** `make test`; then the `go-checks` step of `memql-package.yaml` as written (`make arch-model`, `go build`/`go vet github.com/znasllc-io/memql/...`, `make env-registry-check`, `go run ./cmd/memqllint dsl/`, `go test -run TestEngineInitLoadsFullDSL ./component/memql/`, `make sdk-gen-check`, `make concept-snapshot-check`, `make proto-gen-check`), and `go build`/`go vet -tags <type>` for identity, mcp, agent, planner, workbench and edge.
- [ ] **Step 3:** `git pull` main, re-measure the embed inventory with `go list`, resolve conflicts, rerun the affected packages.
- [ ] **Step 4:** Push to `main` (no force), staging only the files above by path; delete this plan in the same push (a spent plan).
