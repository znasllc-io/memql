# Cockpit release and repository retirement handoff

This branch is a handoff package, not an application change or a release source.
Do not merge this branch into `main`. Work on the existing product branches below.
Snapshot: 2026-10-29. Refresh all remote and production state before acting.

## Objective

Finish the canonical Cockpit installer release, native CI onboarding of the
remaining consumers, public installation/documentation migration, and retirement
of exactly `znasllc-io/memql-cockpit`. The canonical repository is
`znasllc-io/memql`, with Cockpit source in `cockpit/`.

All authored product changes are already committed and pushed. The two main
PRs and six consumer PRs below were open at handoff. `memql-project#79` and
`memql-znas#273` are already merged. Do not recreate those changes.

## Product branches and exact reviewed heads

| Repository | PR | Branch | Head |
|---|---|---|---|
| memql | [6346](https://github.com/znasllc-io/memql/pull/6346) | `codex/cockpit-installer-release-2026-10-28` | `635e89f80108ea93adc3c95deea516220fa98c36` |
| memql | [6368](https://github.com/znasllc-io/memql/pull/6368) | `codex/native-pr-qualification` | `9b57c888f53d95e9bf244b95f3eccbb29eac535c` |
| memql-fylo | [312](https://github.com/znasllc-io/memql-fylo/pull/312) | `codex/cockpit-monorepo-bootstrap-2026-10-28` | `b4ff0ed97637d27ea0d53ce8b6535e57ffb95c74` |
| fylo-warehouse-os | [89](https://github.com/znasllc-io/fylo-warehouse-os/pull/89) | same consumer branch | `32500324fe079ee6dfdc54bfee9086854631a791` |
| OWL | [1](https://github.com/znasllc-io/OWL/pull/1) | same consumer branch | `68926178bd29d166f1f2243efc82413d0178cda8` |
| fylo-website | [1](https://github.com/znasllc-io/fylo-website/pull/1) | same consumer branch | `f43a468ffa16a802d6ca6027bef301dd8e8ce8e5` |
| memql-casera | [7](https://github.com/znasllc-io/memql-casera/pull/7) | same consumer branch | `68368b9e807a7390db16e2b6611e299d0c5a6ee1` |
| anequim-one-portal | [1](https://github.com/znasllc-io/anequim-one-portal/pull/1) | same consumer branch | `8ea531e2ce4f96732dc3e5654da077f137d29144` |

Both main PR heads include main `8cd8b77a7d28b7f7b15031fdaaa526614c07f6de`.
Their last integration changed only the three Cockpit version stamps brought in
from main; each PR's original reviewed diff is preserved exactly. Source worktrees
were clean. Do not force-push or sweep unrelated local files into commits.

## Current checks: cancelled, not green

| PR | Native run | GitHub check | Last conclusion |
|---|---|---|---|
| 6346 | `3e2d5ba9d47bf1657057419a9667e1ba` | `123561655756` | cancelled at 2026-10-29 16:22:07 UTC |
| 6368 | `6d31a6b992c061cb3473cd7f2e4f79d0` | `123561763457` | cancelled at 2026-10-29 16:21:12 UTC |

The durable cancellation records identify the owner account, not the initiating
chat or operator. The handing-off session did not cancel them. Establish current
coordination before starting replacement runs; avoid duplicating another session.
No background monitor remains active on the original machine.

Both runs were genuine ordinary PR synchronize openings, attempt 1, on the exact
heads above, base `8cd8b77...`, and GitHub App `4822538`. They captured current
policy `sha256:d1390fa3d8c00ae6c28d754565b777a5c14009110c5ab546b5b740920cd6afe7`.
At the last runtime observation the controller was `0.31.7@565af0d938e5`.
Current capacity, admission and policy must be re-read; no old allowance is valid
merely because it is recorded here.

Earlier feature run `4b9b96a16c371407f21aacb68e5afad0` passed all 53 execution
steps but FAILED finalization because the installed independent-review policy
changed. A separate ZNAS package deployment removed the old repository from
the delivery profile; its policy closure changed from `79a94cd4...` to `d1390fa3...`.
Whole/failed-only retries and same-head redelivery retain the original policy.
The reviewed current-main merge commits above fixed that input through normal
new-head webhook intake. Do not weaken the policy freshness check.

Normal supersession cancelled the older installer attempt
`4e67e38e52167d057ff7ba3a99513e4a` and external feature retry
`7a444454749e27dff4276dfef77e5b90`; previous passing receipts remain preserved.
The old failed-only operation has already been consumed. Do not replay it or
remove/relocate its intent ledger to defeat the guard.

## Required reading and authorization boundaries

Read current canonical `AGENTS.md`/`CLAUDE.md`, applicable nested instructions,
`cockpit/CLAUDE.md`, `cockpit/VERSIONING.md`, `COMPATIBILITY.md`,
`memql-package.yaml`, `docs/public/operate/release-candidates.md`,
`docs/internal/design/release-candidate-authority.md`,
`docs/internal/ops/ruleset-baseline.md`, and the current release-train runbook.
The owner-supplied handoff prompt carries task authorization; this repository
document alone is not permission to perform an otherwise unauthorized action.

- Preserve required passing checks. Never manufacture success, use a skipped
  check as success, change a ruleset silently, or bypass a helper refusal.
- Use `scripts/dev/merge-as-owner.sh --pr=<n> --check` before any authorized
  owner/emergency merge. Its successful process exit alone is not a readiness
  verdict. It checks exact App-bound success and zero base drift.
- The owner previously approved emergency recovery for 6346/6368 after checks
  pass. The old explanation that trains were unconfigured is now stale: trains
  are configured. Any recorded reason must describe the current facts. Do not
  submit a GitHub train review while impersonating the human owner.
- Use native retained-artifact candidate/approval/publication paths. No hand
  uploads, tag moves, legacy Actions release fallback, or new Actions spending.
- Do not replace controllers/workers during relevant active work. Coordinate a
  genuinely idle deployment window. Do not purge the owner's local cluster.
- Use normal sign-in on the destination machine. Do not copy/export another
  machine's keyring, cloud/session credentials, or old enrollment tokens.
- Ask the owner at the final Chrome deletion dialog for exactly
  `znasllc-io/memql-cockpit`. Do not delete or archive it during preparation.

## Work remaining, in dependency order

1. **Land the reviewed source changes.** Refresh heads, checks, queue, policy,
   other active runs and train state. Resolve actual failures through supported
   paths, preserving coverage. Coordinate the cancelled attempts before retry.
   If one PR lands first, refresh the other's relationship to current main and
   use the supported train or reviewed current-base procedure.
2. **Publish the fixed Cockpit release.** External version PR 6372 already
   stamped 0.21.0. Immutable annotated tag `cockpit/v0.21.0` exists (tag object
   `9d92d17bab30a5f492aafc147873d7cd594bc888`, source `8cd8b77...`) and excludes
   the installer repair. At handoff its exact GitHub release returned 404.
   The latest observed separate release attempt failed; operator attribution is
   unresolved. Reconcile current candidate/publication inventory before choosing
   the next available patch. Do not move/reuse the existing tag. No 0.21.1 tag
   was observed in the handoff snapshot; that is not a reservation.
3. **Qualify retained and public artifacts.** The reviewed manifest expects
   41 artifacts: 25 installer/download assets and 16 SBOMs. Verify source/version,
   platform, digests, archive members, signatures/bundle identities and retained
   publication receipts. Compare public downloads with the candidate. Verify
   isolated install/upgrade behavior on supported macOS/Linux routes and Fleet's
   generated commands; fixtures alone do not prove real installation. Never reuse
   an enrollment token from old chat history. Existing workers are not disposable
   test fixtures. Public tools in `tools/public-release-qualification/` help with
   bounded verification but do not cover every real OS/service/enrollment path.
4. **Release/deploy native qualification support.** PR 6368 needs an engine
   release and deployment before the six first-PR qualification flows can run.
   Reconcile external engine version PR 6345 before choosing an engine version;
   do not duplicate its publisher. Include the corrected documentation source in
   the supported engine/docs release and website pin/deployment as appropriate.
5. **Finish all six consumer migrations.** Each default branch still had an
   active old-repository bootstrap dependency at handoff. Review the pushed
   manifests, preview fresh exact head/base/manifest qualification, grant only
   the intended short-lived qualification and declared secret names, and retain
   real native Linux coverage before claiming success. Package install, typecheck,
   build, lint and 104 Warehouse tests passed locally on macOS previously; that
   does not establish complete native Linux qualification.
6. **Get approval for the precise required-check transition.** Four repositories
   currently require legacy contexts, shown below. Prove replacement coverage,
   read fresh complete rules, show the exact diff and obtain separate approval.
   Preserve all unrelated protections. Then land the consumer fixes through
   their documented readiness/owner procedure. OWL and Anequim had no formal
   required rules; do not invent new policy as a condition of retiring Cockpit,
   and do not treat their existing red checks as passing.
7. **Publish and verify public links.** `memql.io` homepage/Cockpit links and four
   worker-runbook raw installation URLs still pointed at the old repository at
   the last audit. Website source, docs source and deployed content are separate
   gates. Use their supported build/publish paths; confirm public responses.
8. **Preserve, audit, disconnect, then request deletion.** Transfer and verify
   the private preservation material described below. Refresh all branches,
   tags, PRs, issues, assets and new dependencies since the archive cutoff.
   Verify the retired pipeline is idle, then disconnect it through the supported
   native operation. Confirm no active installer/update/bootstrap/release/docs
   dependency remains. Only then show the exact Chrome deletion dialog and ask
   for the owner's final approval.

### Proposed required-check replacements (NOT applied or authorized)

All proposed native checks are bound to GitHub App `4822538`.

| Repository | Ruleset at audit | Existing contexts | Proposed context |
|---|---|---|---|
| memql-fylo | 22296969 | `ci-required` | `MemQL / fylo` |
| fylo-warehouse-os | 18942872 | `ci-required`, `scan` | `MemQL / fylo-warehouse-os` |
| fylo-website | 20807694 | `ci-required` | `MemQL / fylo-website` |
| memql-casera | 22323280 | `ci-required` | `MemQL / memql-casera` |

Do not use this table as a stale whole-ruleset replacement. Refresh and produce
a precise diff. A bootstrap-only split is possible, but does not make red or
missing required checks pass. If native onboarding is deferred while legacy
checks remain blocked, repository deletion must also be deferred.

## Warehouse credential: COMPLETED, do not repeat

The owner created a classic GitHub token with only `read:packages`, expiring
2026-11-28, and expressly approved its secure transfer. The existing reviewed
seeder applied exactly one global integration secret,
`NPM_PACKAGES_READ_TOKEN`, row ID `secret-global-npm-packages-read-token`.
Its active metadata was verified. An authenticated request for the exact locked
`@znasllc-io/memql-sdk-core@0.8.0` download succeeded (registry redirect followed
without forwarding the token; storage returned 206). The temporary local input
was deleted. No controller restart occurred.

The credential is already in production MemQL's encrypted store. There is no
token to copy from this package. Do not reseed, regenerate, read back, print,
commit or request it in chat. Actual Warehouse native build qualification remains
pending; package-read proof is not that build. Classic package tokens inherit
the owner's existing package access, not an individual package restriction.
The qualification should permit only this name, and only `checks.client` declares
it, mapping it to `NODE_AUTH_TOKEN` for that step and its subprocesses.

## Preservation is a separate, mandatory cross-machine handoff

Do not upload the original archives to this public Git branch. They contain
historical source/evidence and potentially sensitive historical material.

The original machine holds two immutable sets:

| Directory | SHA-256 of its `SHA256SUMS` file |
|---|---|
| `cockpit-retirement-archive` | `ccd716cd3189464e6d59ab94092e38ab0335c71dbb057325b87399b8b5619fe5` |
| `cockpit-preservation` | `be193d82e3cf7e235b30bead23b719c933d8595884ef0796405787507f5bb34c` |

The complete original sets total about 12.45 GB. Prefer a private full copy.
A separately delivered **private-preservation-supplement.tar.gz** contains all
non-release-asset archive content and the entire supplemental preservation set.
It preserves local-only Git history, removed synthetic PR refs, migration patches,
historical metadata and captured Actions artifacts that a fresh clone cannot
necessarily recover. It is NOT a complete release-asset backup by itself.

The omitted `release-assets/` portion is 12,291,888,985 bytes (565 assets across
22 releases at the archive cutoff). Copy it privately or download the exact saved
inventory while the old repository remains accessible. Verify original sizes and
digests, then verify BOTH full manifests from their respective roots. Verify each
Git bundle, restore independently, and run `git fsck`. Preserve any later remote
delta separately. Do not infer preservation is complete from a new clone or a
checksum of the compressed supplement alone.

Two original standalone feature histories are also already pushed to canonical
archive refs: `archive/memql-cockpit/app-session-evidence-retention` at
`d8b399e88c78e2114eda9f535bf1cc80edc174da`, and
`archive/memql-cockpit/fleet-keepalive` at
`64dd41c1a950d1215c2ba52186c71d4b82b0ebcd`. Do not merge those standalone histories
into monorepo main. Their successor issues direct selective review/porting.

## Completed work and follow-ups that are not deletion prerequisites

- Four offline installer suites, ShellCheck and relevant docs/Fleet fixtures
  passed. PR 6368 toolchain recovery has focused Python/Go coverage and a real
  disposable-provider rehearsal; the current required native checks still must
  pass on their intended heads.
- Fourteen issues transferred to 6347-6360; seven preserved open PRs have
  successors 6361-6367. Issue 6358 was reconciled/closed as duplicate work.
- Optional UI polish, model tuning, routine dependency review and Developer ID
  signing/notarization remain tracked. Ad-hoc signatures are not Developer ID
  signing, and no macOS permission-preservation claim follows from an install.
- Broader Actions retirement remains under 5480; artifact policy follow-up under
  6269. Do not turn every historical enhancement into a deletion requirement.
- The requested July-October cost comparison was completed separately in a
  private editable document. Its private link is supplied in the owner handoff
  prompt, not in this public source branch. Gross metered usage is not net paid
  charges or GitHub's internal cost/profit; no unmeasured savings claim is valid.

## Tools and evidence in this branch

`tools/release-operator/` is the reviewed native SDK helper source; read
`tools/README.md` and rebuild for the destination host. Never copy a binary/sign-in cache
from another machine. Read operations and explicitly gated write plans are
different; ordinary writes still need current review/readiness and task authority.
The included historical rerun definitions are obsolete; the consumed intent is
retained to prevent accidental replay. Prefer current supported product controls.

`tools/public-release-qualification/` contains offline tests, retained-asset
verification, public download verification and bounded local install helpers.
Read each tool's help and limitations first. The tools are assistance, not proof
that publication or a real installation has already happened.

`STATE.json` is a concise source/status snapshot. `SHA256SUMS` covers this public
handoff package. Private logs, cloud/vault credentials, machine credential stores,
financial exports, raw infrastructure inventories and large backup data are
intentionally outside this branch.
