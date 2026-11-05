# Nightly release from main: code lands without a pull request, and one automatic release a day carries it to production

- **Date:** 2026-11-02
- **Status:** removed. The train machinery this record built was deleted on 2026-11-05
  ([the owner pushes to main](2026-11-05-owner-pushes-main-design.md), D2): the owner
  pushes to `main` with pull requests off, and releases and deploys by hand. This record
  is kept as history. It was built from the owner's decisions below, made in chat on
  2026-11-02, one question at a time. The owner asked for the work to be built
  overnight, ahead of reading this record, so it has not had the usual written review.
  Section 2 says which decisions are the owner's and which are this record's.
- **What it is:** a change lands by pushing to `main`. Once a day, at a quiet hour, or
  whenever someone starts one by hand, a release branches off `main`, runs the whole
  pipeline, and, when it passes, cuts a version and deploys it. A failed deploy rolls
  itself back.
- **Why now:** the per-change gate is the bottleneck (section 1), and no scheduled
  train has ever landed.
- **Builds on:** [release trains](2026-10-27-release-trains-design.md), whose state
  machine this reuses and whose boarding, approval and rebuild rules it replaces for a
  repository that opts in; [fleet-first](2026-10-28-pipeline-fleet-first-retries-failover-design.md);
  [same-tree reuse](2026-10-29-release-same-tree-reuse-design.md).
- **Parks:** several steps per machine (fleet-first D12). Removing most runs removes
  most of the queue it was meant to relieve. Measure a nightly run first.

## 1. Where it stands (read 2026-11-02)

| Measured | Result |
|---|---|
| Train departures, 2026-10-28 to 11-01 | 14: 3 landed, all express. None of the 9 scheduled ones landed, and 5 of those had no PR ready |
| PRs left off the last four trains that left any off | 89, of which 75 had not passed their own check and 1 conflicted |
| Runs in 7 days, four repositories | 335, of which 238 (7 in 10) were pull request runs |
| `memql` pull request runs in 7 days | 175: 31 passed, 79 failed, about 4 hours each on average |
| A train's own run | 5 of the last 10 passed, the passing ones taking 129 to 360 minutes |
| A release run | 11 of the last 22 finished attempts passed, the passing ones taking 93 to 193 minutes |
| A dry-run merge of `memql`'s 33 open PRs onto `main` | 30 clean, 3 conflicts; 2 of the 3 against `main` itself, on branches 145 and 361 commits behind |
| Database changes in the last 13 engine releases | 9 none; 2 only lookup indexes (0.31.8, 0.31.10); 2 tables or data (0.31.0, 0.31.1) |
| Activity by hour, Pacific, 14 days, mostly the owner's | desktop rows 73 at 03:00, 145 at 20:00, 363 to 454 from 11:00 to 13:00 |

Between a landed train and production, every step is a person's today: Check source,
Start release build, Prepare candidate, Approve release, Publish release (owner only,
`integrations/release`, `clients/os/src/apps/cluster/releases`), then a hand-made pin in
`memql-znas`, `scripts/dev/argocd-sync.sh --apply` on someone's kubeconfig, and
`deployment.verify-rollout`, which is `on: [manual]`. Nothing deploys itself.

## 2. Decisions

**The owner's, in their words where it matters:**

- "I want to remove PRs from normal code changes." `main` needs no pull request. The
  owner changed the rulesets on `memql`, `memql-znas`, `memql-project` and `memql-fylo`
  on 2026-11-02.
- Tests still gate a release: "we do want the test to pass during the release".
- **One automatic deploy a day; "the other ones are going to be manual."**
- **Nightly:** it leaves at 21:00 Pacific and deploys when it finishes.
- A failed deploy rolls back automatically. The owner overruled a proposal not to: "if
  deployment fails, we don't automatically roll back to what was working before ...
  That doesn't make sense to me."
- A release that changes tables or data is tested and versioned, then **waits for the
  owner's Deploy button**.
- Failure handling stays as automatic as it can be, and "ultimately, is the human if AI
  is not enabled". AI is off by default (`MEMQL_PIPELINES_AI`).
- Consolidating resolves conflicts; a change is never left out because it conflicts.
- A change carries its tests, and its branch stays current with `main`.
- A model call never waits for a step (shipped separately in memql#6422).

**This record's, each for the reason given:**

- **D1 -- Land on `main`, not on `change/` branches.** The owner first chose a branch per
  change, then floated this on the same night ("leave it like that, and then for
  releases, we just do a release/ or something like that. Just think about it"). This
  record takes the second. Whoever pushes second resolves a conflict at push time,
  usually the session that wrote the change, so nobody fixes conflicts at 21:00, and
  `main` is always consolidated. It needs no boarding, rehearsal or merging `main` into
  every branch. The cost: `main` can be broken between releases, and leaving a bad
  change out means reverting it. A branch-per-change mode can still be added: a train
  with cars and a train with none are the same machine.
- **D2 -- A train is still the vehicle.** The state machine, the record, the lease, the
  compare-and-swap, the Discord messages and the check run all exist, are cross-replica
  tested, and do not care how many cars a train has. The "release branch" is the train
  branch: `main` at departure plus the version bump. It keeps its pull request: a
  `train/` head on a pull request is how the run is admitted and how the fix has a home,
  and it is the one pull request the owner asked for ("it creates a PR that has
  consolidated everything into main").
- **D3 -- Everything is opt-in per repository, off by default.** Settings keys (section
  3) gate how a train leaves and lands, and the owner's own switches (D9) gate whether it
  publishes and deploys, so landing this code changes nothing until those change, and each
  stage can be turned on or back off alone.
- **D4 -- A train from `main` always runs in full.** `pipelineModeForEvent` answers `affected`
  for a pull request, and this train's diff is one `VERSION` line, so it would test nothing.
  Go makes its run full whatever the installed policy says, as it already does for a merge
  group, because its check stands for the whole release. A train of pull requests keeps what
  the policy gives it: its triage reads a failure against its cars.
- **D5 -- The release is of the tested commit.** `main` may move during a run. The
  version is tagged on the train head that passed, which is an ancestor of `main` after
  landing. Later pushes go out in the next release.
- **D6 -- No `push` run on `main`.** Direct pushes would otherwise each start a full run
  of about 2.5 hours. The installed delivery policy already declines an event by
  answering no mode (`open.go`), so `memql-znas`'s `znasDeliveryEventMode` declines
  `push`, and its evidence policy admits a full train run as a source for a release.
- **D7 -- A test failure re-runs once, on a train from `main`.** With no CI on each change,
  a flaky test would otherwise stop the night's release. The reason today's rule forbids
  it, not hiding flakes, is met by reporting it: the message names the steps. A pull
  request train keeps the old rule.
- **D8 -- A fix goes on the consolidated pull request.** A push to the train branch starts
  its run (`synchronize`) and the train follows its head. Since a train from `main` never
  rebuilds itself (section 5), the fix is never lost.
- **D9 -- Standing approval is the owner's own word, recorded as such.** "A model never
  approves" still holds: nothing here asks a model. It is not a setting, because an instance
  variable can be written by people who may not approve a release, and the engine says
  background work "must never acquire owner authority just because its owner holds that role"
  without a captured grant. So it is one row per pipeline, `v1:pipelines:standing`, written by
  a button only the pipeline's owner in the owner role can press, which captures their
  authority (`auth.CaptureExecutionAuthority`): a ceiling, never a credential. Every tick
  rebuilds it from the owner's CURRENT stored role (`auth.ContextWithPersistedOwner`), so
  losing the role ends it on the next tick. The candidate's journal row names the owner,
  because every publication check compares against them; the train's row records
  `policy:standing-release:<owner>`.
- **D10 -- A release that removes a pipeline step waits for the owner.** The pipeline
  must not be able to switch off its own tests. None of the last 13 releases removed a
  step, so this costs nothing in ordinary use. Stopping every manifest change would have
  stopped 6 of them.

## 3. Settings

`MEMQL_PIPELINES_TRAINS` gains these optional keys per repository. The parser stays strict, so
the engine must be deployed before any of them is written.

| Key | Values | Default | Meaning |
|---|---|---|---|
| `source` | `pull_requests`, `main` | `pull_requests` | `main`: section 4. Nothing about `pull_requests` changes |
| `landing` | `approval`, `automatic` | `approval` | `automatic`: a passing train lands without a review. Requires `source: main` |
| `releaseProfile` | a release profile's name | none | What a landed train publishes under, once the owner allows it. Requires `landing: automatic` |
| `deployment` | an object, below | none | Where a published release goes. Requires `releaseProfile` |

`deployment`: `repository` (the installation's), `applications` (ArgoCD Applications in sync
order, the engine before the product that runs on it), `namespace` (default `argocd`),
`verifyStages` (the installation manifest's stages that verify a rollout), `window`
(`{"start":"HH:MM","end":"HH:MM"}` in `timeZone`; may cross midnight; an automatic deploy needs
one, and the button does not).

Two switches are not settings: **Publish by itself** and **Deploy by itself**, in Pipelines >
Releases, the owner's alone (D9). A switch cannot be turned on until the settings above can do
what it asks. `MEMQL_RELEASE_INSTALLATION_OVERLAY` names the files a pin changes (section 7).

The owner's end state:

```json
{"repositories":{"znasllc-io/memql":{"enabled":true,"departures":["21:00"],
  "timeZone":"America/Los_Angeles","channel":"znas-instance",
  "ownerDiscordUserId":"<the owner id>","source":"main","landing":"automatic",
  "releaseProfile":"engine",
  "deployment":{"repository":"znasllc-io/memql-znas","applications":["memql","znas-cloud"],
    "verifyStages":["deployment","deployment-notification"],
    "window":{"start":"00:00","end":"07:00"}}}}}
```

## 4. Stage 1: a train from main

With `source: main`:

- **Departure.** At each `departures` time, or when an owner starts one by hand. A
  departure finding `main`'s tree where the last release tested it is recorded as cancelled,
  *Nothing new on main since the last release*, and sends nothing. It counts from the commit
  that release tested and not the merge that landed it, because a commit pushed during that
  run is an ancestor of the merge and in no release (D5). A release that failed after landing
  and was replaced does not count: the person who started this one wants it tried again.
- **Building.** The train branch is `main`'s head plus the version bump, built on the
  scratch ref as today. It reads no pull requests and has no cars. The pull request lists
  the commits since the last release (at most 50), and says how many more.
- **The run** is a `pull_request` run at the train head, full (D4).
- **Pass.** `landing: approval`: it waits for the owner's review, as today.
  `landing: automatic`: it lands at once.
- **Landing does not require `main` to be where the train left it** (D5). GitHub merges the
  train branch into `main`'s tip. A refusal is asked again for five minutes, because GitHub
  answers a merge that caught `main` moving with 405 "Base branch was modified". If it keeps
  refusing, which only a human fix or a changed `VERSION` can cause, the train holds and says
  why.
- **Nothing rebuilds it.** A moved `main` does not, and there are no cars to drop.
- **Following its head.** While running or held, a train whose branch moved updates its
  head, goes back to running and may retry once again. The webhook's `synchronize` opens
  the new head's run.
- **Starting by hand.** `pipelinesTrainExpress` with no pull requests starts one, under the
  same rules as an express train: owner only, it replaces a train that is held or waiting,
  and it is refused while one is building or running. Pipelines > Releases gets a **Start
  release now** button and shows the newest train, its state and why it stopped.

`main` stops running on every push once `memql-znas` declines `push` (D6).

Also in this stage, because the rules already changed:

- `CLAUDE.md` and `AGENTS.md`, here and in `memql-znas`: a change lands by pushing to
  `main` after pulling it and running its tests. Sessions stop opening pull requests.
- `scripts/ci/ruleset-drift.sh` and its baseline: the rulesets were recreated with new ids
  and no `pull_request` rule, and the Copilot ruleset is gone.
- `scripts/dev/merge-as-owner.sh` reads the first active ruleset, which is no longer the
  one with the bypass.

## 5. Failure handling, on a train from main

In this order:

1. **The first failure of any kind re-runs the failed steps once** (D7). Passing on the
   re-run carries on, and the message names the steps as flaky.
2. **Still failing, or a run cancelled or refused: hold.** Discord says what failed and
   mentions the owner. When the failed steps name packages, the message lists the commits
   since the last release whose files reach them (at most 50 commits examined), as a place
   to start. It accuses nobody and reverts nothing.
3. **While held:** a push to the train branch or a re-run of its run resumes it. Closing
   its pull request cancels it, and the next departure starts fresh. The next scheduled
   departure does not start while it is held, so a reminder goes out instead.
4. **AI diagnosis**, when switched on in Pipelines > Settings > AI, sees the failed run as
   it sees any other (`znasDeliveryMaintenance`). Off, nothing asks a model.

Main's own failed run is not consulted: there is no push run to read (D6).

## 6. Stage 2: publishing by itself

With **Publish by itself** on, a train that lands goes to `releasing` instead of `landed`, and
stays on its own row, so a takeover resumes from it like every other step. `published` is its
end. It walks the same saved journey a person walks in Pipelines > Releases, one step per tick,
through `AdvanceTrainRelease`, so a person can finish by hand whatever it stopped at:

1. **Prepare**, selecting the commit the train's run tested (D5), not the head of `main`, which
   a direct push leaves with no check of its own. The version must be the train's.
2. **Build.** Tag it and open the release run. Same-tree reuse carries the train run's receipts
   over: `znasDeliveryEvidenceReuse` admits a full `pull_request` source for a `release`.
3. **Candidate**, as `releaseResumeBuild` does.
4. **Approve** under D9, reverifying first, as a person's press does.
5. **Publish.** It outlives any tick, so it runs detached under the journal's lease: the next
   tick finds it running, and a pod that dies gives the lease back. It starts at most three times.

A failed release run re-runs its failed steps once, as the owner's re-run does, and the saved
release adopts it. Then it holds, and the version is spent: the next release takes the next
number. A step that keeps failing, for instance because the owner lost the role, holds after 30
minutes. While held, the owner finishing it by hand moves it on, the next departure stays behind
it with a reminder, and **Start release now** replaces it.

Internal origin only, and not a builtin: neither a request nor a model can reach it.

## 7. Stage 3: deploying and rolling back by themselves

Production has no native installation receiver (`integrations/installation`), so a deploy is what
a person does today, made safe to run unattended. A deploy is its own row, `v1:pipelines:pipelineDeployment`,
because it must not fight a train for the newest-train slot, and because the button can deploy a
release cut by hand. Each tick takes one step from a fresh read and ends in a compare-and-swap. That
matters more here than anywhere: syncing the mesh restarts the agent replica running the tick, and
the next one must simply carry on.

1. **Waiting.** For a newly published release, with **Deploy by itself** on. It waits for the window,
   and for any running train, because restarting `agent` kills running CI, and that train carries a
   newer release anyway. No train departs while a deploy is moving, for the same reason.
2. **Guards.** It holds and asks for the Deploy button when, between what the installation runs and
   this release:
   - a migration does anything but add an index. A Go migration counts as indexes only when every
     statement it runs is written out and about an index, and every helper it calls is visibly harmless,
     so one calling a helper this cannot read asks, even beside an index. A SQL one counts only when every
     statement creates or drops an index. Over the 16 Go and 73 SQL migrations this repository has, that
     asks of one index migration it did not need to, and that costs one press;
   - any other engine code changes a table (`CREATE TABLE` in `embedder_tables.go`, say), because not all of
     them are made by a migration;
   - a worker action contract changes, on the engine's side or a machine's. The match is exact, so this one needs
     more than the button: machines and engine move together in the order `COMPATIBILITY.md` gives, and a rollout
     cannot be verified on a machine on the other contract, so a press without that order goes back;
   - a pipeline step is removed (D10);
   - or it cannot tell: GitHub stopped listing at 300 files, or gave no patch for more files than it will read
     whole. GitHub drops patches part way through a large comparison (18 of 181 files between two real
     releases), so it reads those files itself, up to 60.

   Each says which, and why. Because they read from what runs now, a release held back here is still in the
   range of the next, so nothing slips past. A newer published release carries a waiting one.
3. **Pin**, on its own branch, through the native proposal the instance already binds: the seven-file
   pin from `MEMQL_RELEASE_INSTALLATION_OVERLAY`, every byte from the signed catalog or the file it
   replaces. It waits for that pull request's check, with one re-run. A failure here touches nothing: the
   deploy is cancelled, automatic deploys are not paused, and the owner is told.
4. **Sync.** First a last look: the owner still holds the role, their word still stands if it is an automatic
   deploy, and the window is still open (a pin can pass after it closed, and then it waits for the next). Then
   each Application, in order, at the pin's exact commit: `operation.sync.revision`, with `targetRevision` left
   on `main`. Written as `argocd-sync.sh` writes it, for the reason that script exists: prune written false, one
   declared option, and the persisted operation state cleared in the same fenced write, so nothing is inherited.
   It first records the commit each Application last synced at, which is what it goes back to, and refuses to
   start without one. Done is Succeeded, not selective, and Healthy once the controller has compared since the
   operation ended. Real syncs of `memql` have taken up to 9 minutes, and each has 30.
5. **Verify**, from outside the cluster: a manual run limited to `verifyStages`, at the pin's commit. A
   run of the same commit that answers instead, a person's or an earlier verification's, is not taken for
   this rollout's.
6. **Land.** Only now does the pin merge. If GitHub refuses, production already runs it, so that is a
   chore: after 30 minutes it reports deployed and says main lacks its pin.
7. **Roll back on failure.** A sync that fails, hangs past 30 minutes (its own operation is terminated first,
   and never another person's) or a verification that fails syncs back each Application this deploy started on,
   last started first, so the product returns before the engine it runs on, to the commit it ran before. It
   verifies that and closes the pull request. Main never carried the bad pin, and going back needs no CI, which
   may be what is broken. Discord says what failed and that production is back.
8. **A release that changes tables or data does not go back by itself.** Old code on a new schema is not
   proven safe. It stops and mentions the owner, who pressed the button and is there.
9. **Pause.** After a rollback, automatic deploys stop until the owner presses Resume, because the bad
   change is on `main` and the next release would carry it again. If the rollback itself fails, it stops,
   mentions the owner, and pauses too.

A step that fails on every tick for 45 minutes ends: before production moved it says it did not deploy, and
after, it goes back, or stops if it was already going back. A release that cannot start deploying at all says so
too. It counts how long a step has been failing and not how old its state is, because a passing error during a
long rollout says nothing about the rollout.

The Deploy button is the same path with the guards acknowledged and the window ignored, and works for a
release cut by hand.

**Proven by tests:** the patch, against production's API server with `--dry-run=server`; the client,
against both production Applications' real JSON. **Not proven, and must be before Deploy by itself is
turned on:** a real sync and rollback, and an older engine starting on a database with newer indexes.

## 8. Testing

Cross-replica cases come first (`CLAUDE.md`: multi-node is the default).

- **Takeover:** a second replica's tick resumes a train from `main` at every state, and two
  ticks at once act once.
- **Moved `main`:** a train lands after `main` moved, and is never rebuilt for it.
- **Nothing new:** a departure at the last landed head is cancelled without a pull request.
- **Following the head:** a push to the train branch while held or running resumes on the new
  head and resets the one re-run.
- **Failure order:** the first failure re-runs, the second holds, a pass after a re-run reports
  the steps, and a test failure on a pull request train is still never retried.
- **Mode:** a train run is full whatever the policy answers, and any other pull request stays
  as the policy says.
- **Parsing:** each key's values, defaults and dependencies, and a bad one turning trains off
  as today.
- **Stage 2:** on a real database and real catalog, the whole journey across two replicas to a sealed
  release; refused without internal origin, the standing authority or the owner's current role. On real
  rows across two replicas: landed to published, the row keeping the journey. Each failure path.
- **Stage 3:** each guard over real migration text; the window across midnight; on real rows across two
  replicas with the real opening, policy and driver: a whole deploy, and a failed one going back, each
  verification limited to its stage. A failed pin touches nothing; a hung sync is stopped; a failed
  rollback stops; tables never go back by themselves; the pin from the catalog with no receiver.

## 9. Rollout

No step here is taken by landing the code.

1. Land the code on `main` in both repositories. Nothing changes: every new key and switch is off.
2. The owner adds the required checks (`MemQL / memql`, `MemQL / znas`, `MemQL / memql-project`, from the
   app `memql-for-znas-llc`). Until then a release refuses: it cannot verify its source.
3. Release the engine and deploy it, the usual way. `memql-znas`'s DSL and the Role that lets the engine
   sync its two Applications ship with it. **This has to wait for Cockpit 0.22.0:** any engine from current
   `main` requires worker contract 5 for pipeline steps, an exact match, and every machine is on contract 4.
   The order is `COMPATIBILITY.md`'s: publish Cockpit 0.22.0, hold new runs, release and pin the engine, upgrade
   the machines, release the hold. It comes from that contract, not from this work: landing here changes nothing
   about it.
4. Set `source: main`, then `departures: ["21:00"]`. Watch one night. This is stage 1, with the owner's
   approval still in place.
5. Set `landing: automatic` and `releaseProfile`. The owner turns on **Publish by itself**. Watch one night.
6. Write `MEMQL_RELEASE_INSTALLATION_OVERLAY` (`scripts/delivery/deploy-overlay.py`) and `deployment`.
   Press **Deploy** once with someone watching, which proves section 7's real sync. Force a rollback once,
   which proves the other half. Then the owner turns on **Deploy by itself**.

At the switch, the open pull requests are not converted. Each one's session or owner lands it by pushing
to `main`, and the rest can be closed. Dependabot and forks stay pull requests, and they merge once their
check is green, with no train.

## 10. Risks and accepted limits

- **`main` can be broken between releases**, and every session starts from it. Local tests are
  the only check until 21:00. This is D1's cost, and the owner's choice over the gate that
  stopped everything.
- **Code reaches production with nobody reading it** once `landing` and `release` are automatic.
  Any session that can push to `main` can reach production overnight. The tests and D10 are what
  stand in the way, and D10 cannot see a test weakened inside a script.
- **GitHub requires no check today.** The rulesets keep the rule with an empty list, so only the
  train's code requires a passing run, and `scripts/release/release-engine.sh` and a train's release
  both refuse to publish until the list names the pipeline's check. The owner adds `MemQL / memql`,
  `MemQL / znas` and `MemQL / memql-project`, from the app `memql-for-znas-llc`. Direct pushes keep
  working through the owner's Always-allow bypass; the app is no bypass actor, so a pin merges only
  once its check is green.
- **The preflight does not run.** `upgrade-preflight.sh` needs a person's credentials. Its prune gate
  does not apply with prune off. A surge that cannot be scheduled, or a digest that does not resolve,
  shows as a rollout that never comes up, and goes back. Slower than refusing, and safe, because old
  pods stay until new ones are ready.
- **A workaround a new tag absorbed breaks the render** (`docs/instance.md`: "Delete every workaround").
  The pin's check, or the sync, fails, and it does not deploy. That needs a person, as it does today.
- **Verifying needs a Fleet machine.** `deployment.verify-rollout` is placed there. With the owner's
  machines off it cannot run, and an unverifiable rollout goes back. The window is when they are usually on.
- **A new engine that passes readiness and cannot tick stops its own rollback.** Old pods keep the
  coordinator through a rollout that never comes up, but nothing rolls back one that comes up broken in
  this exact way. The out-of-band availability check still alerts.
- **The analyzer pin stays behind.** `scripts/dev/package-analysis-engine.ref` is the eighth file a hand-made
  pin moves. It only pins tooling, the checks do not compare it with the engine, and it is outside the seven
  the native overlay binds, so it stays until someone moves it.
- **Only the engine releases itself.** A product change on `memql-znas`'s `main` deploys when the next engine
  pin syncs that commit's tree, or the way it does today. Its images are built on a release of its own, by hand.
- **A held release stops that night's deploy** until a human acts, and a fix pushed in the morning
  deploys only in the next window or by the button.
- **Half of release attempts fail today**, for reasons not yet looked into, so an unattended flow
  will stop often at first. Each stop is a message, not a broken production.
- **A cycle measured 4 to 9 hours before deploying,** on a queue shared with every PR run. It should
  shrink, but that is not measured. A run past the window deploys the next night.
- **A restart of `agent` kills running CI**, so a daytime DSL rollout can still cost a night's run if
  it is timed badly. The nightly slot makes that unlikely.
- **Cockpit releases are not part of this.** A contract change stops the flow (section 7), and
  `cockpit/v*` stays a person's release.
- **Code reaches production overnight on a machine's word that tests passed.** That is the owner's choice,
  behind a switch that says so.
