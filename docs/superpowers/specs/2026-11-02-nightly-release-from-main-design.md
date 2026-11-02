# Nightly release from main: code lands without a pull request, and one automatic release a day carries it to production

- **Date:** 2026-11-02
- **Status:** the owner's decisions below were made in chat on 2026-11-02, one
  question at a time. The owner asked for the work to be built overnight, ahead of
  reading this record, so it has not had the usual written review. Section 2 says
  which decisions are the owner's and which are this record's.
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
  3) gate each behaviour, so landing this code changes nothing until the settings
  change, and each stage can be turned on or back off alone.
- **D4 -- A train run is always full.** `pipelineModeForEvent` answers `affected` for a
  pull request, and this train's diff is one `VERSION` line, so it would test nothing.
  Go makes a train's run full whatever the installed policy says, as it already does for
  a merge group, because a train's check stands for the whole release.
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
- **D9 -- Standing approval is an owner's setting, recorded as such.** "A model never
  approves" still holds: nothing here asks a model. The owner chose the behaviour by
  setting `landing` and `release`, and the candidate's approval names that policy, not a
  person.
- **D10 -- A release that removes a pipeline step waits for the owner.** The pipeline
  must not be able to switch off its own tests. None of the last 13 releases removed a
  step, so this costs nothing in ordinary use. Stopping every manifest change would have
  stopped 6 of them.

## 3. Settings

`MEMQL_PIPELINES_TRAINS` gains four optional keys per repository. The parser stays
strict, so the engine must be deployed before any of them is written.

| Key | Values | Default | Meaning |
|---|---|---|---|
| `source` | `pull_requests`, `main` | `pull_requests` | `main`: section 4. Nothing about `pull_requests` changes |
| `landing` | `approval`, `automatic` | `approval` | `automatic`: a passing train lands without a review. Requires `source: main` |
| `release` | `manual`, `automatic` | `manual` | `automatic`: a landed train's version is built, approved under D9 and published. Requires `landing: automatic` |
| `deploy` | `manual`, `automatic` | `manual` | `automatic`: a published release deploys inside `deployWindow`. Requires `release: automatic` |
| `deployWindow` | `{"start":"HH:MM","end":"HH:MM"}` in `timeZone` | none | When an automatic deploy may start. Required with `deploy: automatic`; may cross midnight |

The owner's end state:

```json
{"repositories":{"znasllc-io/memql":{"enabled":true,"departures":["21:00"],
  "timeZone":"America/Los_Angeles","channel":"znas-instance",
  "ownerDiscordUserId":"<the owner id>","source":"main","landing":"automatic",
  "release":"automatic","deploy":"automatic",
  "deployWindow":{"start":"00:00","end":"07:00"}}}}
```

## 4. Stage 1: a train from main

With `source: main`:

- **Departure.** At each `departures` time, or when an owner starts one by hand. A
  departure finding `main` where the last landed train left it is recorded as cancelled,
  *Nothing new on main since the last release*, and sends nothing.
- **Building.** The train branch is `main`'s head plus the version bump, built on the
  scratch ref as today. It reads no pull requests and has no cars. The pull request lists
  the commits since the last release (at most 50), and says how many more.
- **The run** is a `pull_request` run at the train head, full (D4).
- **Pass.** `landing: approval`: it waits for the owner's review, as today.
  `landing: automatic`: it lands at once.
- **Landing does not require `main` to be where the train left it** (D5). GitHub merges the
  train branch into `main`'s tip. If that conflicts, which only a human fix or a changed
  `VERSION` can cause, the train holds and says why.
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

## 6. Stage 2: landing and publishing by themselves

`landing: automatic` is section 4. `release: automatic` extends the train past landing, as
the train's own states, so a takeover resumes from the row like every other step:

`landed` becomes `releasing`, `published`, in that order, then `deploying`, `deployed` or
`rolled_back` (section 7). A train that stops here is `held`, as any other.

1. **Build.** The coordinator calls the same native operations as Check source and Start
   release build: the source is the train head (D5), the tag `v<VERSION>`.
2. **Reuse.** The release run reuses the train run's receipts through same-tree reuse: the
   tag's tree is the tree the train tested. `znasDeliveryEvidenceReuse` admits a full
   `pull_request` source for a `release` event from a `train/` head.
3. **Candidate.** Prepare candidate, as `releaseResumeBuild` does.
4. **Approval under D9.** A new internal operation records the approval as
   `policy:nightly-release`, naming the setting and the owner whose setting it is. It
   refuses unless the settings allow it at that moment, the candidate is exactly the one
   this train's run produced, and the caller has internal origin. No request or model can
   reach it. Publishing then goes through the existing recipe, which requires an approval
   and now finds one.
5. **Failure.** A failed release run re-runs its failed steps once, then holds. The version
   is spent: the next release takes the next number.

## 7. Stage 3: deploying and rolling back by themselves

Production has no native installation receiver, so a deploy is what a person does today,
made safe to run unattended:

1. **Guards, before anything moves.** The release holds and asks for the Deploy button when
   it changes tables or data (a migration in the range that is anything but an added
   index), when it changes a worker action contract, or when it removes a pipeline step
   (D10). Each says which, and why.
2. **Window.** An automatic deploy starts only inside `deployWindow`. Past the end it waits
   for the next night, or for the button.
3. **Pin.** The engine version and digests are committed to `memql-znas`'s `main`, by the
   native proposal operation, which already refuses to invent a fact.
4. **Sync.** The engine Application and then the product, through the ArgoCD client the
   engine already has (`integrations/argocd`), with prune off, as `argocd-sync.sh` does.
5. **Verify.** Both Applications are healthy on the pinned revision, and then the
   `deployment.verify-rollout` step, run by the coordinator, passes. The rule that a
   superseded ConfigMap reads as OutOfSync and is not a failure moves from a person's
   memory into the check.
6. **Roll back on failure.** The pin commit is reverted and the sync and verification run
   again. Discord says what failed and that production is back on the old version.
7. **Pause.** After a rollback, automatic deploys stop until the owner resumes them, because
   the bad change is on `main` and the next release would carry it again. If the rollback
   itself fails, it stops and mentions the owner.

The Deploy button is the same path with the guards acknowledged and the window ignored.

**Not proven by this record's tests, and must be before `deploy: automatic` is set:** an
older engine starting on a database with newer indexes, and a real sync and rollback.

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
- **Stage 2:** the approval refuses without the setting, for another candidate, and outside
  internal origin. Publishing resumes across a takeover.
- **Stage 3:** each guard over real migration text; the window across midnight; a failed
  verification rolls back and pauses; a failed rollback stops.

## 9. Rollout

No step here is taken by landing the code.

1. Land the code on `main`. Nothing changes: every new key is absent.
2. Release the engine and deploy it, the usual way. `memql-znas`'s DSL change ships with it.
3. Set `source: main`, then `departures: ["21:00"]`. Watch one night. This is stage 1, with the
   owner's approval still in place.
4. Set `landing: automatic` and `release: automatic`. Watch one night.
5. Prove section 7's unproven items, then set `deploy: automatic`.

At the switch, the open pull requests are not converted. Each one's session or owner lands it
by pushing to `main`, and the rest can be closed. Dependabot and forks stay pull requests, and
they merge once their check is green, with no train.

## 10. Risks and accepted limits

- **`main` can be broken between releases**, and every session starts from it. Local tests are
  the only check until 21:00. This is D1's cost, and the owner's choice over the gate that
  stopped everything.
- **Code reaches production with nobody reading it** once `landing` and `release` are automatic.
  Any session that can push to `main` can reach production overnight. The tests and D10 are what
  stand in the way, and D10 cannot see a test weakened inside a script.
- **GitHub requires no check today.** The rulesets keep the rule with an empty list, so only the
  train's code requires a passing run, and `scripts/release/release-engine.sh` refuses to publish
  until the list names the pipeline's check. The owner adds `MemQL / memql`, `MemQL / znas` and
  `MemQL / memql-project`, from the app `memql-for-znas-llc`.
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
