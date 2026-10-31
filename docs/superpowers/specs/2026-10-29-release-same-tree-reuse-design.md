# A release reuses the checks of the run that tested its tree

Status: direction decided by the owner on 2026-10-29 ("let release runs reuse the
PR's results on the same tree, keeping the release-only build steps"). This spec
records the design.

## Problem

A release run repeats what its pull request already ran on the same code. The
0.31.9 release run took 2h14m on production; its stages' own run time added up to
under an hour, and the rest was queueing on the single build node. Three rules
stop a release from reusing anything:

1. `reuseVerification` (`component/pipelinerun/evidence.go`) returns at once for a
   release (#6344), whatever the installed policy says.
2. Candidate preparation accepts a `pipeline_passed_earlier` skip only with a proof
   from an earlier attempt of the same release (#6369,
   `integrations/release/candidate_carried.go`).
3. A verification fingerprint binds the commit. A merge commit has a different
   commit id from the pull request head it merged, so the two never match.

Rule 1 exists because of rule 2. A release that reused a push run's receipt could
never be prepared, and 0.31.4 and 0.31.5 were lost that way. This change must not
bring that failure back.

## Goal

A release run skips an artifact-free verification step when a successful run of the
same pipeline executed it on the same git tree with the same definition. Every step
that produces an artifact runs in the release. The result still becomes a candidate,
and the candidate names where each reused receipt came from.

Non-goals: no change to reuse for other events, no automatic retry, no new OS
control, and no reuse of steps outside the `verification: true` contract.

## Decisions

### D1. Trees, not commits

The driver reads the run's commit from GitHub once, before it reads the plan
(`GET /repos/{repo}/git/commits/{sha}`, `tree.sha`), for every run of every event.
It writes the hash in two places:

- `treeHash` on the `v1:pipelines:run` row, which is how a release finds its sources
  (`pipelineRunsForPipelineTree`);
- `treeHash` in the work run's input, which is where the proof reads the tree a
  receipt tested, from the journal rather than from a mutable row.

The input's tree stays out of a receipt's digest. An engine from before this change
computes the same digest for the same rows, so a rollback refuses no candidate that
reused nothing. The digest still commits to the tree through `treeFingerprint`, which
is part of the step's call.

A read that fails does not fail the run. The run has no tree hash, so it neither
offers reuse nor takes it: a release executes every step. Refusing a pull request's
checks because GitHub answered badly to an optional read would be the worse failure.

### D2. A fingerprint that means "the same check on the same tree"

`verificationFingerprint` keeps its meaning (same commit). `treeFingerprint` is new,
on the step's call, written for each `verification: true` command when the tree is
known. It binds what the verification fingerprint binds with two changes:

- the tree hash replaces the commit;
- `dependsOn` is removed. It lists the previous stage's steps, so it differs between
  a pull request plan and a release plan on one tree (`secrets-current` runs on one
  and not the other). It orders the plan; it is not an input to the command.

It keeps the engine revision, the workflow fingerprint, the repository, pipeline,
owner, compute, domain, command, image, services, shard and packages. Different
engine, different workflow or different compute means the step runs again.

The fingerprint is only sound if a verification step cannot see which commit it is
on, so `MEMQL_SHA` joins the variables a verification step does not receive. A step
that declares `checkoutHistory` has inputs beyond the tree, so it gets no
`treeFingerprint` and is never reused across commits.

### D3. Only `verification: true`, and no artifact

The existing declaration is the reuse contract: a digest-pinned cluster container,
no secrets, no repository token, no artifacts, no event or run identity in its
environment. Twenty-seven of the release plan's steps qualify today, among them the
long ones (`go-tests`, `db-tests`, the `test-*` lanes, `build-*`,
`module-boundaries`).

Everything else keeps running, deliberately:

- every release-only build: the images, `cockpit-release`, `release-files`,
  `build-tools`;
- every step that declares artifacts or an image build, whatever its stage: the
  vulnerability scans, `sbom`, `os-checks`, the editor lanes, `proving`,
  `provider-qualification`;
- artifact-free steps outside the contract: `go-checks` (reads `MEMQL_BASE_SHA`),
  `fuzz` and `mcp-conformance` (not declared), and the two Cockpit lanes (native
  fleet steps, which `validateVerification` refuses). Extending reuse to evidence a
  machine produced is the owner's open decision from the fleet-first work.

A source row is also judged by what it reported: a row that named an artifact, in
files or intents, is never reused (`releaseProducer`).

### D4. The vulnerability scans always run in a release

They are excluded three times over: they declare artifacts, their command branches
on `MEMQL_EVENT` (a release records findings and continues, a pull request fails,
#6381), and they are not a function of the tree, since advisories change daily. A
freshness bound would be defensible, but it would ship a report older than the
release. The release keeps its own report in `.memql-security/`.

### D5. The policy decides again, and native gates decide first

The engine no longer short-circuits a release. A candidate source must pass
`pipelines.SameTreeProblem` (D7), have the same policy version, and be admitted by
the installed `evidenceReuse` selector, asked with its existing arguments. The
engine default stays `false`, so a cluster with no policy reuses nothing.

Source mode is not a native gate for a release. A pull request runs in `affected`
mode, and what matters is what the step executed: the fingerprint binds its
packages. The policy still receives `sourceMode` and may refuse.

Explicit retries keep their rule. A re-run (`rerunOf`) never asks for reuse: a whole
re-run executes everything, and a failed-only re-run carries inside its own chain.

### D6. A sharded step is reused whole

A shard's slice comes from the timing table, which every successful full run moves,
so a release's slices rarely equal its source's. If slices had to match exactly,
`go-tests` and `db-tests` would almost never be reused.

So a step is judged as a group of its shards. When the source's rows for those keys
hold exactly the packages the plan selected, the release takes the source's slice
for each shard and compares fingerprints under those slices. Either every shard is
covered or none is. Coverage is unchanged because the union is identical; only the
partition differs.

A failed-only re-run of a release takes the previous attempt's slices the same way
(`adoptReleaseSlices`). Otherwise a re-run would plan new slices, find no match and
run every test shard again, losing what #6369 gives.

### D7. The runner may not accept what the proof would refuse

This is the rule that keeps 0.31.4 from happening again. A successful release run is
never replaced (`adoptableBuildRun`), so a reuse the proof refuses costs a version.
Both halves therefore call the same code on the same facts:

- `pipelines.SameTreeProblem(reuser, source, at)`: owner, pipeline, repository, tree,
  a completed successful source that was not cancelled, not a scheduled run, within
  24 hours. The runner passes `at` as the later of now and the run's start; the proof
  passes the run's start, so the runner's bound implies the proof's.
- `pipelines.InspectReusedReleaseReceipt(owner, run, step, runRow, stepRow)`: the real
  receipt inspector over the real rows, requiring an executed, artifact-free
  successful command inside a work run that succeeded. The runner reads the same
  rows (`WorkRunRow`, `WorkSteps`) before it plans a skip.

If the proof ever refuses anyway, the release is unblocked by the installed policy
returning `false` for a release and cutting the next version, the path every lost
version has taken so far.

### D8. Provenance in the manifest

`ReleaseEvidence` gains `reusedFrom`:

```
{pipelineRunId, workRunId, attempt, event, commit, treeHash, receiptDigest}
```

Assembly writes it, and `coverage` recomputes it from the journal and refuses a
candidate whose record differs, so client JSON cannot assert it. It is omitted when
absent, so every existing candidate keeps its bytes and its identity. The recipe
accepts a `pipeline_passed_earlier` skip with `carriedFrom` or `reusedFrom`, and
refuses a bare one.

The proof re-derives everything from journal rows on every read, including at
publication and overlay time, and does not ask GitHub again. Each tree hash was read
from GitHub's commit object by the run's own driver.

### D9. Failed-only re-runs

Attempt 2 carries attempt 1's reused step like any other pass. The proof walks the
attempt chain as #6369 does, and the first attempt whose row names `evidenceRunId`
is the reuser: its start bounds the age, and the provenance names the source run. An
attempt that executed a step keeps today's `carriedFrom`. The chain's 24-hour window
is unchanged, so a late re-run carries nothing and runs everything.

`resume` and `reopenFromRows` restore `evidenceRunId` and both fingerprints from the
rows. A takeover replays each step's call from its declaration, and a call that
changed would make the run's definition fingerprint stop matching, which refuses the
candidate.

### D10. A live candidate holds its sources

Coverage proves a reused step on every read, including when an overlay is built from
the candidate much later. Finished pipeline runs leave after 30 days. A release's own
rows stay while its artifacts are pinned, but a pull request has no artifact to keep
it, so a candidate would become unprovable a month after it was cut.

So retention keeps a run while a candidate that is neither retired nor archived names
it in `reusedFrom`. The alternative was to accept the sealed record once the rows are
gone, which would stop proving exactly when nothing else could notice. An archived
candidate holds nothing, because its own run's rows leave on their usual window and
holding its sources past them keeps nothing provable.

The hold reads the candidates once per statement, as an uncorrelated `NOT IN`. A probe
per row would detoast every live manifest once per version, and an expression index
does not help because the planner keeps its sequential scan over a table of a few
pages. That took a guarded delete past its 8 s statement timeout at about a hundred
candidates, and a cancelled delete repeats on the same page every night.

## Rollout and limits

- It helps only once production runs an engine with this change. Sources must be
  opened by that engine too, since older rows have no tree hash.
- The engine change alone reuses what `znasDeliveryEvidenceReuse` already admits for
  a release: `merge_group` and `push` sources in `full` mode, up to six hours old.
  Reusing a pull request's results also needs that policy to admit
  `sourceEvent == "pull_request"` for a release, in `memql-znas`. That policy change
  is a separate pull request; it must ship after the engine, because a policy
  version change makes runs opened before it ineligible as sources.
- An `affected` pull request usually covers fewer packages than a full release, so
  its `go-tests` and `db-tests` qualify only when its selection went full. The `push`
  run of the release commit covers them when it has finished before the release
  starts. The release does not wait for it.
- A successful pull request whose steps passed across a failed-only re-run offers
  only the steps its last attempt executed. A carried pass is not reused, so the
  chain of skips cannot renew its own freshness.
- Rolling back to an engine from before this change refuses a candidate with
  `reusedFrom`, as an adopted re-run's journal already is, and leaves a run row holding
  a `treeHash` unwritable, so a run in flight at the rollback stops being driven. A
  candidate that reused nothing still verifies.
- A candidate prepared from a failed-only re-run reads the release's earlier attempts
  on every read and nothing holds them, so past the retention window a new overlay from
  it is refused. This is true of every carried step since #6369, and it is not fixed
  here.
- A reused step's log stays on its source, under that run's shorter retention.
- `checkoutHistory` aside, "the tree determines the check" stays the author's
  assertion. A command could still ask git for its commit.

## Alternatives considered

- **A candidate that also covers the source run.** Rejected. The source's complete
  evidence includes steps a release does not run, and its artifacts would become
  eligible for pinning.
- **Exact slices only.** Rejected: see D6.
- **Reusing any artifact-free step by normalizing its environment.** Rejected. A step
  that receives `MEMQL_EVENT` may branch on it, as the vulnerability scans do, and a
  normalized digest would hide that.
- **Same-commit sources only.** Rejected. It misses the pull request, which is the
  case the owner asked for.
- **Reading the source's tree from GitHub at candidate time.** Rejected. It adds a
  provider dependency to publication and overlay reads, and the journal is already the
  trust root for whether a test passed.
