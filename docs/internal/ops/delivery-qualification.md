---
title: Qualifying repository delivery and selected installations
audience: ops
status: stable
area: ops
sinceVersion: 0.30.10
owner: znas
---

# Qualifying repository delivery and selected installations

This runbook covers the handoff from agent-authored changes to verified main,
published artifacts and an explicitly selected installation. Passing checks,
publishing a candidate and installing it are separate recorded events. Main
may advance while an installation remains pinned to its selected release.

## Ownership and configuration

The engine provides generic integrations, execution isolation, fresh authority
checks, immutable evidence, journals and installation locks. MemQL-ZNAS owns
the workflow composition in `dsl/znasdelivery` and its configuration compiler
in `scripts/delivery/configure.py`. Its maintained `docs/delivery-policy.md`
documents the exact inputs and interpreter checks. Other installations supply
their own installed DSL and operator bindings.

A repository manifest declares commands and component relationships. It
cannot replace the installed policy resolver, acquire writer credentials,
approve its own request or choose an arbitrary installation target. An
installation owner or developer can request an upstream change; the upstream
maintainer's approval and current provider permission are still required.
Remote request credentials authenticate submissions and status reads only.

Cockpit lives in `cockpit/` as its own Go module and CLI. It shares source
review and dependency analysis with the engine while retaining independent
artifacts and `cockpit/vX.Y.Z` tags. Engine node tags continue to select engine
wiring. This avoids treating an operator CLI as a cluster node and avoids
requiring every CLI change to publish all engine images.

## Before local qualification

1. Use a reviewed source checkout and record its revision, including whether
   it contains uncommitted work. Build with the supported `make dev` path into
   the local k3d installation. Verify the context is the intended local
   cluster and that the resulting pod image identities match the new images.
2. Run the whole multi-module suite with `MEMQL_REQUIRE_DB=1 make test` against
   a disposable UTF-8 PostgreSQL database with TimescaleDB and pgvector. Give
   concurrent test processes separate databases. Check database capacity;
   a process accepting connections while PostgreSQL is recovering is not a
   usable test fixture. On a busy development machine, use
   `MEMQL_REQUIRE_DB=1 GOMAXPROCS=2 make test GOFLAGS=-p=2`; the Makefile sets
   `GOFLAGS`, so an environment-only override does not limit parallelism.
3. Run the OS tests, type checks and production build. Regenerate SDK,
   configuration, concept and architecture outputs after their inputs change.
4. Lint the instance DSL with the matching engine, run its interpreter policy
   assertions and configuration compiler tests, then install that package
   through the normal package stage/roll path. Do not overwrite core namespaces
   or add an alternative DSL mount to bypass package validation.
5. Configure real source identities, policy bindings, request-only peers and
   the receiving installation. Use existing named secret references. A missing
   receiver or provider connection must produce a setup/refusal state.

Do not use the active installation's database as a test fixture. A local
development image is useful for UI and integration qualification but is not a
signed release candidate. Installation qualification additionally requires an
immutable candidate and rollback publication, verified artifacts and trust,
an immutable reviewed overlay and the actual receiving controller.

Run [artifact qualification](delivery-artifact-qualification.md) for the
real PostgreSQL, Library, Azurite and OCI registry handoffs. That affected
CI lane refuses skipped provider cases and records its fixture cleanup;
it does not replace the selected installation/controller rehearsal below.

To qualify the reviewed instance DSL locally without installing its websites,
prepare a clearly labeled **DSL-only qualification ZIP** from one recorded
instance commit. Preserve the complete `dsl/znas/` and `dsl/znasdelivery/`
trees byte for byte, including their language lines and prompt/template files.
Use a generated root manifest with `formatVersion: 1`, a distinct qualification
package name and `deployables: []`; domains are discovered from the DSL tree.
Omit website assets, campaign/email settings and production configuration.
Record the source commit, copied-file hashes, derived manifest and final ZIP
hash so this artifact cannot be mistaken for the full production package.

Upload that ZIP through Files, then choose **Deployables → Add a deployable →
A zip in Files**. Analyze it as an artifact source with no repository identity.
Expect exactly the two DSL domains, zero web deployables and no problems, then
select **Install MemQL changes**. The full ZNAS package's external website
assets require a repository source and are validated before the address step;
skipping websites later cannot make that full ZIP admissible. A direct offline
`packages.Analyze` call alone does not exercise this source-kind gate.

After the restart, verify that every DSL consumer loaded the new definitions;
a successful restart request alone is not readiness. Operator policy bindings,
credentials and GitHub access remain separate setup from installing this DSL.

## Local acceptance walkthrough

| Area | Action | Required observation |
|---|---|---|
| Fleet | Connect this machine and use an installed model | Steady heartbeats and a real response through the Fleet connection |
| Pipelines | Open Overview, Repositories, Changes, Runs and Releases | Each destination owns one clear task; setup states explain missing configuration |
| Repository request | Submit an owned-source change as an owner and as a developer | Current provider authority determines whether work can start |
| Upstream request | Submit a core change from an installation without core write access | Maintainer approval is required; requester receives readable status/reason without private maintainer goal identifiers |
| Delegation | Decline, revoke, expire or change the installed recipe while work is pending | Subsequent reads/effects refuse the old delegation; a role label alone grants nothing |
| PR checks | Change a component with a known dependency graph | Relevant checks run and report their actual scope; uncertainty falls back to full verification |
| Queue admission | Authorize delivery, wait for checks, then interrupt a queue response | The exact reviewed proposal is recovered once; changed heads, withdrawn authority and closed goals stop delivery |
| Queue verification | Advance/remove a queue ref and separately make the provider unavailable | Confirmed obsolete work releases capacity; provider unavailability does not count as removal |
| Main | Land a queued change, then an unqueued main commit | Exact eligible evidence may be reused; an unverified commit still runs checks |
| Release | Prepare and publish verified artifacts | Publication produces receipts and does not install anything |
| Cluster Updates | Select a reviewed candidate, prepare and inspect its diff | One saved plan binds candidate, overlay, current revision and operator |
| Recovery | Reload during preparation; cancel a prepared plan | Preparation resumes from saved identity; cancellation releases its own slot and cannot cancel an applying update |
| Installation | Apply the selected plan and observe it | Completion requires actual controller, image, migration and authenticated health evidence |
| Nexus | Open successful, blocked and failed goals on wide/narrow screens | Readable actions, reasons, outcomes and token usage; technical data is optional |
| Navigation | Follow a deployable's pipeline link and return | Pipeline control belongs to Pipelines; Deployables retains relevant context |

Check keyboard navigation and focus retention, not only screenshots. A model
listing alone is not inference proof, a prepared plan is not an applied
installation, and an accepted write is not a confirmed outcome.

## Native Go preparation

Native Mac checks read GOOS and GOARCH in one successful command before their
Go build or test starts. The selected Go launcher retains checksum verification;
only this preparation command may retry, at most three times within one
600-second deadline, including two seconds between failures and up to five
seconds for launcher cleanup. Each attempt receives only the remaining budget;
neither backoff nor another attempt resets it. A wrong or malformed
platform refuses immediately. Timeout is terminal, and every child stays in the
native worker's process group for its existing cancellation and cleanup. The
actual test command still runs once, and its failure remains a failure.

The 30-minute native check reserves ten minutes for preparation, ten minutes for
the test command, and ten minutes for checkout, CSS and compilation overhead.
The earlier 120-second preparation limit stopped a verified toolchain download
before tests. An isolated cold Go 1.27.3 download on the same machine completed
in 86 seconds, while a separate public archive sample transferred at a rate
requiring roughly 219 seconds for the full archive. The shared deadline allows
for that observed variability without multiplying the lane budget by retries.

Toolchain and build caches remain inside the exact attempt checkout. The
managed reusable cache supports containers only; sharing an attempt-writable
native toolchain directory across repositories or installations would cross
that boundary. The fixtures in `native_go_platform_test.py` and
`TestNativeMacLanePreparesToolchainBeforeRunningTestsOnce` cover transient and
permanent preparation failures, a shrinking shared deadline, platform refusal
and once-only tests. They do
not establish live network availability; the required native check must still
pass on the repaired head.

## Active delivery contract

The active [`memql-package.yaml`](../../../memql-package.yaml) includes affected
component selection, verification reuse and independent Cockpit release builds.
The complete reviewed contract is retained in
[`delivery-after-bootstrap.yaml`](../../../test/fixtures/pipelines/delivery-after-bootstrap.yaml).
`TestActivatedDeliveryManifestMatchesReviewedContract` compares the entire
parsed manifest with that fixture; any intentional pipeline change must update
both through review. The Cockpit release regression exercises the active
manifest's platform, isolation and retained-artifact requirements.

The agent-tagged suite still requires its own pinned PostgreSQL service and
`MEMQL_REQUIRE_DB=1`. Its app test budget remains 900 seconds, each of the four
other packages retains 300 seconds, and the outer 45-minute limit covers the
combined 35-minute maximum plus setup. Activation does not change repository
protection, install a release, enable a source or grant publication credentials.

## Production cutover

Keep the currently paused automation paused during local qualification. Review
the GitHub App identity/logo, required check context, queue triggers and review
policy together before cutover. Existing rulesets remain authoritative until
an explicit reviewed policy change is applied; independent AI review does not
silently satisfy a separate GitHub code-owner requirement.

The replacement review requirement must bind **MemQL / review** to the
approved GitHub App ID, alongside the existing required test context.
Changing only the code-owner setting would leave direct queue admission
without independent review enforcement. The reserved review context cannot
be used as a pipeline name, including an already stored pipeline.

Qualify that context on both the exact PR head and GitHub's synthetic
merge-group head before changing repository policy. The group report must
prove every included PR's immutable head against its persisted native
accepted review; a matching green title alone is insufficient. Exercise a
two-change group, a changed head, removed membership, a missing native
receipt and a lost provider response. The initial group-proof path requires
the queue's **MERGE** method; squash/rebase cannot be treated as equivalent
parent evidence. Keep existing enforcement until these checks pass with the
actual installed App. Preserve required tests, queue ordering and bypass
restrictions when reviewing the policy diff.

Register the app with the permissions in [GitHub setup](../../public/operate/github-connect.md),
including the explicit GitHub consent for existing installations. Verify the
actual approved App/user intersection can mark a PR ready and enqueue its
expected head. Mocked provider tests do not establish GitHub GraphQL permission
compatibility for a live installation. Use a controlled repository for this
qualification, without changing production protection or enabling paused CI.

Enable one source first and follow its PR, exact queue revision and main run.
Confirm supersession affects only obsolete work, check conclusions reach
GitHub, and maintenance creates bounded proposals when adequate evidence
exists. Missing evidence, repeated repair failures or uncertain authority must
request human attention. Then publish a candidate and perform one explicitly
selected installation before enabling other sources.

GitHub renders the check interface. The App owns its identity and logo; check
titles, summaries, annotations, actions and detail links can be improved.
The native GitHub page layout cannot be replaced by MemQL.

## Evidence behind the design

The October 22, 2026 investigation sampled 44 distinct native checks across
26 selected commit/queue records: 18 succeeded, 21 were cancelled and five
failed. This is a convenience sample, not a cancellation rate for all runs,
and the conclusion alone does not identify who cancelled a run or why. One
observed commit received main and release runs seven seconds apart. Worker
execution uncertainty and resource failures also occurred. These observations
support separating obsolete verification, durable release work and selected
installation state; they do not prove that the merge queue itself was broken.

The engineering basis is incremental integration and independently verifiable
effects. [GitHub's merge-queue account](https://github.blog/engineering/engineering-principles/how-github-uses-merge-queue-to-ship-hundreds-of-changes-every-day/)
and its [queue documentation](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)
support validating the prospective merge tree. [DORA's small-batch guidance](https://dora.dev/capabilities/working-in-small-batches/)
supports independently reviewable changes, while its [2025 AI research](https://dora.dev/research/2025/dora-report/)
describes AI as amplifying the surrounding delivery system.
[Meta's test-selection account](https://engineering.fb.com/2018/11/21/developer-tools/predictive-test-selection/)
illustrates selective testing at scale; this implementation uses explicit
dependency and path evidence with a full-verification fallback, not an
unvalidated predictive model. These sources support the design principles;
they are not evidence that large companies universally use unattended agents
to approve and deploy all production changes.
