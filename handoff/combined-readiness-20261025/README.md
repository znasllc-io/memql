# MemQL portable handoff — October 25, 2026

This branch is the single starting point for continuing the combined Ask,
harness, pricing, evaluation-evidence and native-CI work on another computer.
The source, this handoff, validation summaries, public native-check receipts and
the exact outstanding standalone Cockpit patch are all committed here. No file
from the previous computer is required to understand, build or continue the
source work.

## Start on the new computer

```sh
git clone --single-branch --branch handoff/combined-readiness-20261025 https://github.com/znasllc-io/memql.git
cd memql
git status --short
git log -1 --oneline
```

Read the repository `AGENTS.md`, this file, [manifest.json](manifest.json),
[validation.json](validation.json), and [native-checks.json](native-checks.json).
Follow normal repository setup for dependencies; dependency directories and
binaries are deliberately not transported.

**CI is paused until the human explicitly says MemQL CI/CD is ready.** Creating
this portable handoff does not lift that hold. Do not open a PR for this branch,
update a PR head, rerun CI, merge, deploy, change pipeline settings or start a new
benchmark while the hold remains. Production deployment is not authorized.

This handoff branch has no PR. Native delivery ignores pushes to non-default
branches unless an open PR also changes; polling follows the default branch and
open PR heads. Keeping existing PR #6290 unchanged lets this handoff be pushed
without restarting qualification. Do not interpret missing checks on this
branch as a pass.

## Source inventory

The implementation snapshot is
`aeee127803bd42cd79a2d03bbcda8292eadd5423`. The handoff commit adds only the
portable handoff directory. The following source heads are ancestors of that
snapshot and therefore travel with this single branch:

| Work | Frozen source | Existing PR |
| --- | --- | --- |
| Harness | `14f260a158c7cbd24135e58edee28ac45a196874` | [#6286](https://github.com/znasllc-io/memql/pull/6286) |
| Ask message actions | `81eb6833b540bff0294fe0c9c9a2005fd3b110b2` | [#6287](https://github.com/znasllc-io/memql/pull/6287) |
| Pricing and evaluation evidence | `482b6eb0c8a80c9cfe08de1f4e4218920f5765a7` | [#6283](https://github.com/znasllc-io/memql/pull/6283) |
| Combined integration and repairs | `aeee127803bd42cd79a2d03bbcda8292eadd5423` | [#6290](https://github.com/znasllc-io/memql/pull/6290) |

The observed integration base was
`50f2ea9bf32d47f6a391f1623a2784e4f4211a1a`; recheck main before resuming. The
original PR branch is `ci/combined-readiness-20261025`. Do not independently merge
the three source PRs and accidentally lose the integration repairs.

### Included behavior

- Ask removes the header Activity control and adds a message hover background
  and top-right menu: Copy message, Copy details and per-message Activity.
  Keyboard/focus, draft and scroll behavior are preserved.
- Harness changes cover progress-aware continuation instead of a fixed lifetime
  call ceiling; durable ordinary Ask feedback; permission-aware reuse of source,
  research and training context; source discovery; retained execution receipts;
  bounded reporting-only recovery without replaying completed operations;
  workbench lifecycle and focus guards. Goal acceptance remains manual.
- Pricing provides API-equivalent price evidence and owner-confirmed local
  electricity profiles, research and estimates. Evaluation work provides private
  case records, archive tools, scenario contracts and documentation.

The canonical machine-side worker is already in `cockpit/` in this branch.
There is no need to fetch another feature branch to obtain its implementation.
Read `cockpit/CLAUDE.md` for the consolidated build layout.

### Standalone Cockpit preservation

The older standalone repository still has unmerged
[PR #562](https://github.com/znasllc-io/memql-cockpit/pull/562), head
`d8b399e88c78e2114eda9f535bf1cc80edc174da`. Its runtime changes are present in
the canonical `cockpit/` tree. The monorepo additionally retains process-probe,
repository-walk, numeric-conversion and disk-headroom hardening. Its repository
guidance and engine pin intentionally differ from the standalone layout.

For exact recovery or later standalone PR disposition, this branch includes
[cockpit-pr-562.patch.gz](cockpit-pr-562.patch.gz), the complete 60-path delta from
standalone base `55cc83443ace058c7121c6edc76d8297dc4597c9`. Applying it to that base
was verified to reproduce the exact head tree
`30dc97355a3efd20c6fa4062299a4031cbb0ba99`. The patch hash, base and head are in
the manifest. This is a small source delta, not a repository or dependency dump.

Do not apply this patch over `cockpit/`: that would overwrite newer canonical
changes. If standalone recovery is needed after the hold is lifted, clone the
public standalone repository at the manifest's base and apply the saved patch
there after decompressing the patch with gzip. The old standalone feature branch
is not required to recover the code.
Decide whether PR #562 should be merged or superseded only after checking the
then-current repository policy and exact native status.

## Integration repairs and rationale

| Commit | Repair |
| --- | --- |
| `0b3bf16c94c7ea1baf72da128580ce13ccc9630d` | Add the standalone work module's missing test dependencies; retain module isolation. |
| `3785be31984ba2980426963ed5e3352fa9bd32fe` | Use DSL accept/stamp syntax for workbench continuation; document server-only authority checks and the outcome-format prompt's existing model tier. |
| `d26d4beb63dd9e4f9d84dd027b4e08b0a8967fdd` | Fence energy-profile row versions/absence; separate diagnostic redaction from credential secrecy; preserve builtin credential exclusions and actual diagnostic redaction; correct stale guidance/usage fixtures and measured embed inventories. |
| `aeee127803bd42cd79a2d03bbcda8292eadd5423` | Align agent fixtures with the strict completion contract and real router; retain receipt assertions; obtain inference-wait identity through the production event/timer/claim path and retire fixtures after each case. |

The energy race tests synchronize two prepared decisions before either commits,
then require the loser to return `ErrRowVersionChanged` and preserve the winner.
Cases cover first creation, saves, and research racing location or consent
changes. The profile fence supplements the machine-registration fence.

`FunctionArgsField.Redact` retains broad diagnostic redaction, including ordinary
`prompt` and `sectionKeys` values. Semantic `Secret` comes from declared secret
metadata and still excludes private inputs recursively from discovery and direct
`workExecute`. Explicit builtin declarations cover native private energy inputs,
API keys, source tokens, integration configuration values and Shopify credentials.
Do not restore the broad logging classifier as a capability exclusion or bypass
the recursive secret checks.

Agent fixture changes do not relax runtime authority, heartbeat, version,
completion, no-replay or budget checks. Provider doubles reject unexpected
structured schemas. Knowledge/Training tests wire the real router and assert
review; startup tests still reject unexpected and duplicate dispatches.

Detailed durable rationale is in
[docs/internal/ops/harness-decisions.md](../../docs/internal/ops/harness-decisions.md).

## CI state and validation limits

The exact public check receipts are in [native-checks.json](native-checks.json).
All cited native checks belong to GitHub App **4822538**.

| Candidate | Native outcome | Interpretation |
| --- | --- | --- |
| `9a0815892aecbf09c8b4eb0d9885572784e3c7ae` | Failure, check `121173885074` | Standalone work-module metadata; repaired by `0b3bf16c9`. |
| `0b3bf16c94c7ea1baf72da128580ce13ccc9630d` | Failure, check `121180756354` | 27 test lanes passed; five source-failing lanes and three required-artifact-retention failures. |
| `aeee127803bd42cd79a2d03bbcda8292eadd5423` | Cancelled, check `121204915531` | Human pause; **not a pass**. |
| Standalone Cockpit `d8b399e88c78e2114eda9f535bf1cc80edc174da` | Success, check `120769306397` | Historical standalone native pass; recheck before future merge. |

The latest engine native run is
[`5f792de5c01231e97e26b2a45a3c8fbf`](https://os.memql.znas.io/?pipelineRun=5f792de5c01231e97e26b2a45a3c8fbf).
It was observed cancelled on October 25 at 08:11 Phoenix (15:11 UTC). Native run
pages require the user's authorized cloud login on the new computer. Do not copy
browser sessions, cookies, credentials or scoped kubeconfigs between computers.

The previous full attempt's Library/context/connection timeouts prevented three
required artifact captures even though their commands passed. These remain
failures. Coordinator recovery started six previously unexecuted lanes after
unconfirmed cleanup; their saved output passed. Do not describe that as lost
completed results, and do not turn an artifact-missing step green manually.

[validation.json](validation.json) records the local checks and regression names.
Local compilation of database tests is not execution. The final repaired native
database and agent suite has **not** passed. Some local test groups overlap; do
not add them together as a unique test count. Follow the repository's module-aware
test commands; bare `go test ./...` misses the engine. Do not reuse the old
computer's local/shared database or assume a configured cluster on the new one.

## Resume after explicit CI/CD readiness

1. Fetch current main and PR metadata; confirm the handoff contains the source
   heads in the manifest. Inspect mutable CI and production settings again.
2. Continue from this branch. To retain PR #6290, fast-forward its existing
   `ci/combined-readiness-20261025` branch to this handoff head only after readiness
   and only if its current head is still an ancestor. Never overwrite another
   session's new commits. Alternatively coordinate a replacement PR. Do not open
   duplicate qualification runs.
3. Run required native qualification on the final exact candidate. Investigate
   infrastructure failures separately from source failures. Preserve actual run
   results and artifacts. No GitHub Actions substitution, weakened gate or
   fabricated green status is permitted.
4. Recheck production holds. Previously, source deployment modes were Manual,
   installation selection was unconfigured and MemQL/znas-cloud Argo automated
   sync was absent. Those observations are historical, not current guarantees.
   Do not change memql-znas or unrelated ingress settings.
5. The engine pipeline had been temporarily connected for qualification. The
   pause did not change settings. After successful qualification and retained
   receipts, coordinate restoring its prior Disconnected setting before merge
   to avoid an unwanted duplicate main run; do not interrupt an active required
   run or change other sources.
6. Follow current repository merge policy, including the documented
   `scripts/dev/merge-as-owner.sh --check` path where appropriate. Re-read exact
   head, base and required App-bound checks immediately before merging.
   Production deployment/publication remains unauthorized.
7. Verify resulting main ancestry and PR disposition, then remove obsolete
   feature references. The source PRs may close automatically through ancestry.
   Do not rerun the intentionally superseded old #6280/81873b0 duplicate.

Planned post-merge issue updates concern #5799, #6068, #5835 and #6167. These are
broad issues with unfinished acceptance criteria: add precise progress comments,
but do not close them solely for age or partial completion. No merge-related
comments or closures were posted because the merge did not happen.

## Evidence and benchmark interpretation

MemQL performed the Ask before/after UI checks using its own screenshots in
dark/light/narrow/reduced-motion configurations. Coarse-pointer/touch behavior
was not independently verified. No combined candidate was deployed during the
consolidation, so do not assume any running UI serves this branch.

Both comparison runs use Apps Astra with extra-high effort. Recorded settings
are not independent proof of served model identity. The delivery/reporting
failure, fresh-trial failure, recovery failure and later warm handoff success
remain distinct phases. Warm timing/token counters can overlap and do not prove
a speed improvement. Integration CI failures are supervised integration
validation, not new Apps benchmark attempts.

Live utility lookup/owner confirmation and six evaluation scenario contracts
remain unrun. The public evaluation website and automatic capture are planned
work. Goals remain open/unaccepted; no agent accepted their results for the user.

This repository is public. Raw private App transcripts, screenshots and private
evaluation ZIPs are deliberately not published by this handoff. Their protected
originals and native journals were preserved; they are not part of this clone
and their cross-machine availability has not been established. The source and
validation handoff do not depend on them. If a future task requires inspecting
those originals, use the owning MemQL instance's authorized Library/export path
or arrange a private transfer; do not claim to have inspected them from this
checkout or publish them implicitly.

For future end-to-end feature work, use MemQL Ask/Apps and MemQL's own vision,
computer-use and evidence tools. Direct harness repairs and independent review
are separate from the measured native task. Any previous personal-computer
consent has expired; permission and application focus must be established on the
new machine before input. Never type into an unverified foreground application.

## Session closeout

The three helper chats finished and are idle. Seven redundant worktrees, one
redundant clone, five temporary test binaries, a task-scoped kubeconfig and five
stale local aliases were removed after preservation checks. Native evidence,
private archives and recovery copies were retained. No monitor or scheduled
task remains attached to this work. No source change was merged into main and
no production deployment was performed.

The prior handoff's absolute paths were specific to the previous computer and
are superseded by this branch. This file, the manifest and the public receipts
are the portable record; do not require another chat's local filesystem to
resume source integration.
