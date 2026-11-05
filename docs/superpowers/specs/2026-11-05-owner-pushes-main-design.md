# The owner pushes to main: tested pushes, releases by hand, no trains

**Date:** 2026-11-05. **Owner:** znas. **Status:** implementing.
**Replaces:** [2026-11-02 nightly release from main](2026-11-02-nightly-release-from-main-design.md)
and [2026-10-27 release trains](2026-10-27-release-trains-design.md), whose train
machinery this change deletes.

## Why

Pull requests, merges and release trains became the bottleneck for getting work
out. The owner asked for, in their words, speed "without sacrificing testing and
putting too much risk on deployment, and with a good rollback plan". Concretely:

- Only the owner moves `main`, on the repositories that matter. On `memql-fylo`,
  its main developer Jesus Mendivil does too.
- No pull requests.
- Every push to `main` is tested, and work already verified is reused rather
  than run again.
- Releases are cut by hand, only from a commit whose tests passed.
- Deploys are done by hand, with a rollback plan.
- Trains are removed completely, not deprecated.

## Decisions

### D1. GitHub: only the owner moves `main`, and pull requests are off (applied)

On `memql`, `memql-znas`, `memql-project` and `memql-fylo`:

- The `default branch` ruleset carries `update` (Restrict updates), so only its
  bypass actors can move `main`.
- The bypass names the user `znas-io`; on `memql-fylo` it also names
  `jmendivilznas`.
- The `no force push` ruleset, with no bypass, stops everyone from rewriting or
  deleting `main`.
- `has_pull_requests` is `false`.

The record, the undo and the audit are in
[ruleset-baseline.md](../../internal/ops/ruleset-baseline.md).

`memql-casera` (no pipeline, no recent commits) and `memql-cockpit` (its own
pull-request rules; Cockpit's source lives in `memql/cockpit`) were left alone.

### D2. Trains are deleted, with the automatic deploy built on them

Deleted from the engine, the DSL, MemQL OS, the docs and production's settings:

- trains, both of pull requests and from `main`;
- the express departure;
- standing approval;
- "Publish by itself", "Deploy by itself" and **Deploy newest release**;
- the `MEMQL_PIPELINES_TRAINS` and `MEMQL_RELEASE_INSTALLATION_OVERLAY`
  settings.

The deploy goes with the trains because of how it lands a release. It commits
the pin to a branch of the installation repository, verifies the rollout, and
merges the pin through a pull request as the cluster's GitHub App. On the
installation repository, pull requests are now off and only the owner may move
`main`, so that path can no longer finish. It was also never configured in
production: the paused `MEMQL_PIPELINES_TRAINS` row has no `deployment` entry.

**Alternative considered:** keep it as a standalone "deploy this release"
action that pushes the pin as the owner. Rejected for now, because the engine
would have to hold a credential that can move the installation's `main`, and the
owner asked for manual deploys. An automatic deploy schedule can come back later
as its own design.

**What replaces its verification and roll-back** is the manual runbook in D8.

### D3. Every push to `main` keeps its full run

Push runs are `main`'s only CI and the release's evidence. The installed policy
answers `full` for a push on every connected repository, and the nightly design's
plan to stop push runs once trains ran is dropped.

### D4. A burst of pushes runs the newest commit, not every commit

Today three pushes in ten minutes open three full runs that queue behind each
other, and the newest commit's verdict arrives last.

**The change.** When a push to the default branch opens a run, it supersedes
every older push run of the same pipeline on that branch that has not started a
step. Such a run is concluded `cancelled` with `cancelledBy: superseded by <the
new run>`. A run that is already executing finishes.

**The result.** At most one run executes and the newest waits. A commit in
between is covered by the newer commit's run, because `main` only moves forward:
no force push, no deletion.

**The guards.** A delivery that arrives late for an older head supersedes
nothing, which is the same rule cancel-on-push applies to pull requests. A
release run, a scheduled run and a manual run are never superseded.

**As implemented** (`component/pipelinerun/push_lane.go`, the push lane;
operator view in `docs/public/operate/pipelines-substrate.md#the-push-lane`).
More specific than the decision above in these ways:

- **Opt-in.** A push run joins the lane only when its own saved policy's
  `supersession` phase answers yes for a full push from a webhook or the poll.
  The portable policy answers no, so nothing changes until an installation adds
  that case to its supersession template (`memql-znas`, rollout step 3).
- **"Has not started a step" is "no agent claimed it".** A queued run nobody
  claimed is waiting and can be superseded. A claimed run is executing, even
  before its first step, and finishes.
- **One at a time.** A waiting lane run is not claimed while another push run
  of the pipeline executes. Nothing is written, so its run ceiling has not
  started. The replica that concludes the run ahead starts the next waiting
  run: the one at the head, else the newest queued. Recovery does the same
  within a minute.
- **Proof, not order.** A push supersedes a waiting run only when GitHub names
  the new commit as the default branch's head and proves the waiting run's
  commit a proper ancestor of it (`CompareAncestry`). A failed head read, a
  failed compare or a commit that is not an ancestor supersedes nothing.
- **Late deliveries.** An older commit's run that opens after the head's own
  run supersedes nothing when it opens. Recovery supersedes it toward the
  head's run, which must exist and must not have been asked to stop.
- **Soft.** Each claim takes only its own run's gate, so two waiting runs
  claimed at the same instant on two replicas can both start. That costs a
  runner. A claim and a supersede of one run share that gate, so exactly one
  wins.
- **A force-push back** to a superseded commit runs it again as its key's next
  attempt, as for pull requests (R37). The ruleset in D1 refuses force pushes;
  the engine does not rely on it.
- **Notifications** pass over a superseded run when they decide whether a pass
  recovered.

### D5. A push run reuses the verified result of a step whose inputs did not change

A push run in full mode would otherwise re-execute every verification step.

**The change.** In a push run on the default branch, a verification step, or one
package slice of a packaged step, is satisfied by an earlier receipt when all of
these hold:

- An ancestor commit holds a **directly completed**, successful receipt for that
  step and slice. "Directly completed" means executed there, not itself reused.
- The receipt is younger than the reuse window (24 hours at most).
- It was recorded under the same owner, source, engine revision, policy
  fingerprint and command definition.
- The affected selection between that commit and this one leaves the step or
  slice untouched.

The check run lists the step as reused, naming the run it came from.

**The bounds.**
- Only directly completed receipts carry forward, so every step executes for real
  at least once a day while `main` moves.
- A step that reads more than its tree (`checkoutHistory`) never carries
  forward.
- Release preparation proves each reused step against the run it came from, as
  it already does for same-tree reuse.

The implementation details are in the change itself and in
[pipelines.md](../../public/operate/pipelines.md#reusing-verification-evidence).

### D6. A release is prepared from the newest commit whose run passed

Before this change, `releasePrepareBuild` released the tip of `main` and refused
while the tip's run was still going. So `main` had to sit still for the two to
three hours a full run takes.

**The change.** Preparation walks `main`'s first-parent history from the tip, at
most 20 commits. It picks the newest commit whose required App-bound check
(`MemQL / memql / push`) completed successfully, then proves that commit with
the unchanged `ObserveReleaseSource`. Newer commits are skipped:
- one whose run is still going (`pending`);
- one whose run failed (`failed`);
- one that was never tested (`missing`): an intermediate commit of a
  multi-commit push, or a run superseded under D4.

**What is recorded.** The walk is saved inside the release's immutable selection
and shown on the release journey. A walk that finds nothing green is refused
`release_source_untested`, naming what it found.

**What the walk aborts on.** Evidence it cannot trust: a truncated listing, an
unbound check, or the required name reported by another App. It never skips past
such a commit.

**Policy change.** The installed reuse window from a push run to a release
widens from 6 to 24 hours. A release now often comes from a commit whose run
finished hours earlier.

### D7. The heavy test steps stay on the cluster

A step declared `verification: true` must be a pinned cluster container: that is
what makes its receipt reusable, and validation refuses any other placement.
`go-tests` and `db-tests` are the largest reusable cost.

Moving them to `placement: pool` would:
- forfeit the reuse in D5 and D6;
- put nine shards on machines that run one command at a time;
- under production's configured delivery profile, which has no cloud fallback,
  leave a busy fleet holding the very push run a release waits on.

The fleet keeps running the steps that need a real machine (macOS, native
Docker, the installers). Offloading non-reusable cluster steps through `pool` is
possible, but needs the fleet-first delivery profile with its cloud fallback,
which is an open owner decision.

### D8. Deploy and roll back by hand

The runbook lives where the installation lives, in `memql-znas`. It covers:
- pinning a published release;
- pushing the pin to `main` as the owner;
- full syncs with `scripts/dev/argocd-sync.sh`, then verifying;
- rolling back by reverting the pin commit and syncing again;
- a release that changes tables or data, which cannot simply be pinned back:
  take an on-demand backup first, and fix forward or restore from the point-in-time
  archive.

### D9. Who may do what

| Who | May do |
|---|---|
| The owner | Prepare, approve, publish and seal releases. Releases are owner-only in code: `requireOwner`, refusal `not_owner` |
| Developers (Jesus on `memql`) | Deploy packages through Deployables (DSL included) and roll them back. Install a published release through Cluster > Updates |

Developers cannot cut an engine release. `memql-fylo` ships as a package, so its
main developer can deploy and roll it back.

### D10. `memql-fylo`'s pushes are tested

`memql-fylo` declares a pipeline, but no run has ever reported on its commits:
it is not connected on the production cluster. Connecting it, with compute
`cluster`, is part of the rollout.

## Rollout

1. GitHub controls (D1). Done 2026-11-05.
2. The engine change ships in the release after 0.31.15:
   - trains deleted (D2);
   - coalescing (D4), carry-forward (D5) and the newest-green walk (D6);
   - docs.

   It is pushed only after 0.31.15's release run has started, because a push
   adds a full run to the shared queue.
3. Installation (`memql-znas`):
   - the delivery policy's supersession, reuse and train comments;
   - `docs/instance.md`;
   - the deploy and rollback runbook (D8).
4. Production, after the new engine is deployed:
   - delete the paused `MEMQL_PIPELINES_TRAINS` row;
   - connect `memql-fylo`'s pipeline.

## Known limits

- Coalescing and carry-forward change what a push run executes, not what a
  release requires. A release still needs a green required check on the exact
  commit it ships.
- Carry-forward is only as sound as the affected selection, which pull requests
  have relied on for months. The 24-hour window and "directly completed only"
  bound the exposure.
- Release preparation stays owner-only. Letting a developer publish would be a
  separate decision.
