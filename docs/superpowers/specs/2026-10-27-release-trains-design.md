---
title: Release trains -- pull requests collect, are tested together once, and reach main only with the owner's approval
audience: internal
status: draft
area: operate
sinceVersion: 0.31.3
owner: znas
---

# Release trains

Owner brief, 2026-10-27. Many pull requests to this repository run the full
pipeline at the same time, and every merge leaves the others behind `main` to
run again. The owner wants changes to collect and reach production on a
schedule, about twice a day, with the owner approving what goes into `main`
and Discord as the place that says what is happening and what needs the
owner. This record designs the first of three projects that get there. Every
fork below was put to the owner as a question in the brainstorm of 2026-10-27
and answered.

## 1. Why

Measured on 2026-10-27 and 2026-10-28 while combining seven ready pull
requests into memql#6325:

- **No pull request here can be approved.** Every one is authored by the
  owner's account: 35 opened and 18 merged in the seven days to 2026-10-28,
  all by `znas-io`. The `main` ruleset requires a code-owner review, and GitHub
  never lets a pull request's author approve it. So every merge goes through
  `scripts/dev/merge-as-owner.sh`, which uses the admin bypass
  (`gh pr merge --admin`). The owner's approval is an instruction to an agent
  in chat; GitHub records an admin override, not a review.
- **Nothing batches.** The bypass skips `main`'s merge queue (`ALLGREEN`, up to
  five entries, one build at a time). Each merge leaves every other open pull
  request BEHIND `main`; `merge-as-owner.sh` refuses a BEHIND pull request, so
  each one updates and runs the whole pipeline again. Pull-request runs never
  reuse earlier results.
- **Main tests the same content twice.** A merge creates a new commit, and
  evidence reuse matches the exact commit only ("Equal trees with different
  commit hashes are currently checked separately",
  [pipelines.md](../../public/operate/pipelines.md#reusing-verification-evidence)),
  so `main`'s run tests the merged content again in full.
- **A failure costs an hour and shows one problem.** Stages run strictly in
  order. On run `03ced2fb` the 3-minute vulnerability scan started only after
  the 47-minute fleet `cockpit` stage, and its failure (GO-2026-6866 and
  GO-2026-6860 in `golang.org/x/net`, published at 21:16Z) skipped `tests` and
  `gates`.
- **Every run needed a manual window.** The Azure cost guard holds new runs and
  rewrites its decision every hour, so each run waited for an admission window
  opened by hand.

## 2. Decisions

| Fork | Decision |
| --- | --- |
| Where the owner decides | Once per train. Pull requests board automatically; the owner approves the train pull request, which covers every pull request on it |
| Scope | A MemQL Pipelines feature, turned on per repository; this repository first |
| A failing train | Drop the pull request that caused it and re-run the rest; split the train when the cause is unclear; a failure `main` also has is fixed on `main` first |
| Urgent fixes | An express train: off schedule, only the named pull requests, the same checks and the same approval |
| Mechanism | MemQL builds the train. Not GitHub's merge queue, which admits a pull request only with its own approval; not a long-lived `next` branch, where unreviewed changes pile up and a bad one must be reverted |
| How many train branches | One, rebuilt from `main` at each departure |
| Communication | Discord, in #znas-instance through the existing `znas-instance` channel, for train, release and deploy status, failures, and everything that needs the owner |
| Switch-over | Immediately, with no shadow period: production has no clients relying on it yet, and the owner chose speed over a trial week. The first scheduled train after the switch is the test |

## 3. The three projects

1. **The train** (this record): pull requests collect, are tested together
   once, and land on `main` only with the owner's approval.
2. **Release from the train** (its own record): the train's run also builds the
   release, so the one approval covers publishing it, and the tag is created
   on approval. This changes the release order (build before tag) and is
   designed separately.
3. **Deploy from the train** (its own record): production updates through the
   installation update proposal
   ([release-candidates.md](../../public/operate/release-candidates.md#propose-a-complete-installation-update))
   instead of hand-made pin pull requests, then syncs and verifies health.
   Needs memql-znas#271 first: the CNPG webhook refuses the digest-only
   `memql-db` image, so every full sync of the `memql` application fails.

Until projects 2 and 3 exist, a release is cut from `main` after a train lands,
as today. The train's version bump (4.3) means no separate version pull
request is needed.

## 4. How a train runs

### 4.1 Boarding

A pull request is ready to board when all of these hold:

- it is open, not a draft, and its base is `main`;
- its `MemQL / memql` check succeeded on its current head. That is the PR lane
  (5.1) once the two lanes exist, and the full pull-request run until then;
- it carries neither `train:hold` (the owner or a session adds it) nor
  `train:dropped` (a train adds it, 6; the session removes it once its fix is
  pushed).

Agent sessions keep opening pull requests under the owner's account, get the
PR lane green, and stop. They never merge.

### 4.2 Departures

- Two scheduled departures a day, set per repository in the owner's time zone.
  Default: boarding closes at 09:00 and 15:00. A full run takes about two
  hours, so each train reaches the owner around 12:00 and 18:00.
- An express train departs on demand with only the pull requests named. The
  owner starts it, or tells an agent to. It goes ahead of a train that is
  waiting for approval or held: as built, that train is cancelled and its pull
  requests board the next train. It is refused while a train is building or
  running.
- A departure with no ready pull request sends nothing. As built, it is
  recorded as a cancelled train, so the next tick does not re-read GitHub
  every minute until the next departure.

### 4.3 Building the train

At departure the coordinator acts as the MemQL GitHub app, with a scoped
installation token minted the way `app/pipelines_token_minter.go` mints them
today:

1. It creates `train/<yyyy-mm-dd>-<n>` at `main`'s head (`POST /git/refs`).
2. It merges each ready pull request's head into the train branch, in pull
   request number order, on GitHub's side (`POST /merges`); nothing is checked
   out. A
   pull request that conflicts with `main` or with an earlier one stays off
   this train and gets a comment naming the conflict.
3. It commits the version bump: the patch number plus one in the profile's
   version file (`VERSION`), unless a pull request on board already changed it.
4. It opens one train pull request, authored by the app, listing every pull
   request on board (number, title, link), those left off and why, and the
   version.

The app (`memql-for-znas-llc`) already holds `contents: write`,
`pull_requests: write` and `merge_queues: write` on this repository.

### 4.4 One train at a time

- While a train is building, running or waiting for approval, a scheduled
  departure does not start, and ready pull requests keep collecting.
- Only trains move `main`, so a waiting train stays current. If `main` moves
  anyway (an emergency merge, an express train), the waiting train is rebuilt
  on the new `main` and asks again. What the owner approves is always exactly
  what lands.

### 4.5 The train record

`v1:pipelines:train` is owner-tier like every pipelines concept. It holds the
repository, the departure (scheduled or express), the state, the pull requests
on board (number and head commit at boarding), those left off (number and
reason), the train branch and head commit, the train pull request, the current
run, the rebuild count, the approval (reviewer and reviewed commit) and the
landed commit.

States: `building`, `running`, `awaiting_approval`, `landing`, `landed`,
`held` (needs the owner) and `cancelled`. Dropping a pull request returns the
train to `building`.

Settings live in the global variable `MEMQL_PIPELINES_TRAINS`, read the way
`MEMQL_PIPELINES_REPOSITORY_CHANGE_RULES` is: a JSON object keyed by
repository, each entry naming `enabled`, `departures` (`HH:MM` in `timeZone`),
`timeZone` (IANA), `channel`, `ownerDiscordUserId` and `maxRebuilds`
(default 2). A malformed variable sends no train anywhere and says why in the
log.

A scheduled automation drives it, every minute, on the one agent replica
holding the `pipelines-trains` lease (an agent, because a retry opens the
failed-only re-run only an agent's driver picks up). Every transition is a
compare-and-swap on the record under a Postgres lock, with a read that skips
the node's result cache, so a coordinator that moves to another replica in the
middle of a train resumes from the record (the cross-node rule in
`CLAUDE.md`).

## 5. What runs

### 5.1 Two lanes

- **PR lane**, for an ordinary pull request: build and lint (`checks`),
  `vulnerabilities`, `secrets-current`, the Go tests of the packages the pull
  request changes, and the AI review (`independentReview`). It skips `cockpit`
  (fleet machines), `delivery-artifacts`, `installer-smoke` and the full
  `gates`. The target is under 30 minutes; planning measures the exact split.
- **Train lane**, for a train pull request: every stage, as a full run. The
  quick stages that read outside data (`vulnerabilities`, `secrets-current`)
  move ahead of `cockpit`, so a failure like the 2026-10-27 advisory surfaces
  in minutes. Planning first checks whether today's order has a reason.
- The installed stage selectors choose the lane from verified pull-request
  facts: a train pull request has the app as its author and a `headRef`
  starting with `train/`
  ([pipelines.md](../../public/operate/pipelines.md) documents the
  `pullRequest` argument). A source manifest cannot choose its own lane.

### 5.2 After landing

An approved train lands only when `main` has not moved, so `main`'s new commit
has exactly the tree the train run tested. `main`'s push run records that it
was verified by the train and runs nothing: the stage selector finds the
landed train whose tested head has that tree, and excludes each stage with the
reason "Verified by train <id> (run <id>)". A push whose tree matches no train
runs in full, as today.

### 5.3 Admission

Trains are scheduled, so the cost guard admits train runs from a daily train
allowance, and PR-lane runs from a small separate allowance, instead of hourly
windows opened by hand. This changes the cost guard in memql-znas
(`azure-cost-guard`) and belongs to this project.

The engine's admission file (`component/pipelinerun/admission.go`) gains an
optional `allowTrainRuns`; a train run is a pull-request run whose head branch
starts with `train/`. The reader refuses unknown fields, so the engine ships
before the guard emits the new field. The PR-lane allowance comes with the two
lanes, after the switch.

## 6. When a train fails

In this order:

1. **An environmental failure retries once.** A step timeout
   (`pipeline_step_timeout`), a download or an infrastructure error gets one
   automatic re-run of the failed steps. A test failure is never retried.
2. **A single suspect is dropped.** Each failed step covers a package slice. A
   pull request on board that changed files in that slice is a suspect. When
   there is exactly one, it is dropped.
3. **No suspect: check `main`.** If `main`'s newest completed push run failed
   the same step, `main` has the problem: the train is `held`, the owner is
   told (9), and the fix rides an express train; the held train is rebuilt
   after it lands. Otherwise the train is `held` and the owner is told the
   cause is unclear. Running only the failed steps against `main`'s head needs
   a run limited to named steps, which the engine cannot start today; it comes
   with splitting (step 4), after the switch.
4. **Several suspects, or none while `main` passes: split.** The suspects are
   split in half, and only the failing steps run on a train branch carrying
   each half, until one pull request is isolated. A split costs minutes, not
   hours.
5. **The dropped pull request goes back to its session.** It gets a comment
   with the failure and the run link, and the `train:dropped` label. The train
   is rebuilt without it and runs the full pipeline again.
6. **Two rebuilds is the cap.** After two rebuilds in one departure the train
   is `held` with a summary for the owner.

A held train waits for the owner. Closing the train pull request cancels it,
and the next departure starts fresh; adding `train:hold` to a pull request on
board rebuilds the train without it.

## 7. Approval and landing

- The owner approves the train pull request on GitHub: a code-owner approval,
  recorded under the owner's name.
- To pull a pull request off the train first, the owner adds `train:hold` to
  it. The train is rebuilt without it and runs again before it asks again.
- Landing requires the approval to name the train pull request's current head.
  The app then merges through the normal rules (merge method `merge`), with no
  bypass. If the head or `main` moved after the approval, it does not merge;
  the train is rebuilt and asks again.
- The first release learns of an approval by reading the train pull request's
  reviews on every one-minute tick. Subscribing the app to
  `pull_request_review` events comes after the switch: today the intake's
  classifier deliberately ignores review payloads
  (`component/pipelines/trigger.go`, `deliveryKind`), so the event needs its
  own kind and route.
- After landing, each pull request on board shows as merged, because its head
  is in `main`, and gets a comment naming the train.
- A model never approves. The approval is read from GitHub as the owner's
  review, and nothing in the coordinator can create one.

## 8. GitHub rules and agent workflow

- **CODEOWNERS:** the owner alone owns every path, `*` and the lead-only paths,
  so only the owner's approval counts on `main`. Today `*` belongs to
  `@znasllc-io/human-developers`, which includes `jmendivilznas` (maintain).
- **`main` ruleset:** remove the `merge_queue` rule, since the train batches.
  Keep `pull_request` with `require_code_owner_review: true`, and keep
  `MemQL / memql` required; on a train pull request it is the full run. Save
  the current ruleset JSON first, for the fallback (12).
- **The app:** its permissions already suffice. The `pull_request_review`
  subscription waits for the review-event route (7).
- **Until the switch** (owner decision, 2026-10-28): `main` requires
  `MemQL / memql / push` instead of `MemQL / memql`, so a release can be cut
  from `main`'s own run while pull requests merge at once through the admin
  bypass. The switch restores `MemQL / memql`, which the train pull request's
  run reports, and the release source then comes from the merged train pull
  request. Both rulesets as they stood before are saved for that restore.
- **The bypass:** keep it, for emergencies only. `merge-as-owner.sh` refuses to
  merge without `--emergency=<reason>` and records the reason on the pull
  request as a comment.
- **`CLAUDE.md` Branch Workflow:** a session opens a pull request, gets its PR
  lane green, and stops. The train carries it. A dropped pull request is fixed
  by its session for the next train. An urgent fix asks the owner for an
  express train.

## 9. Discord

- **One place.** Every message goes to #znas-instance through the existing
  `znas-instance` channel. That channel names the sealed secret
  `ZNAS_DEPLOYMENT_DISCORD_WEBHOOK`, and the outbound worker owns delivery,
  retries and the receipt. No new webhook; the `memql` pipeline is allowed on
  the channel.
- **ACTION NEEDED messages mention the owner, and they are the only ones that
  do:**
  - a train is ready for approval: the pull requests on board, the result and
    the train pull request link;
  - a train is held because `main` fails: what failed and what fixes it;
  - a train stopped after two rebuilds;
  - the cost guard is holding a train run;
  - a train is still waiting for approval at the next departure.
- **Status messages** mention no one: a train left (the pull requests on board,
  and those left off and why); a pull request was dropped, and why; a train
  landed; a release was published; production was deployed and verified (the
  existing deployment message, in the same format); something recovered.
- **Per-pull-request results stay on GitHub.** A pull request appears in Discord
  only when it is left off or dropped.
- **One format.** A title saying what happened and where; a one-line summary;
  the same fields each time (train, version, run, what the owner needs to do);
  links to the train pull request, the run page and MemQL OS; text labels such
  as ACTION NEEDED, FAILED and PASSED, never emojis. `component/pipelinenotify`
  gains the new message kinds; there is still one composer. Text from pull
  requests and commits stays neutralized, so it can never produce a mention;
  the owner's mention comes only from the train settings
  (`ownerDiscordUserId`, 4.5), sent in the message's `content` with
  `allowed_mentions` naming that one user. Mentions inside embeds do not
  notify on Discord.
- **Today's messages get fixed.** The outbound delivery records of the last two
  weeks are audited, each wrong message is fixed, and every message kind gets
  a golden test of its exact payload.

## 10. Build order

**First release: what the switch needs.**

1. The train record and coordinator: scheduled and express departures,
   building the branch, the version bump and the train pull request as the
   app, behind a per-repository setting.
2. Approval and landing (7).
3. Discord: the ACTION NEEDED approval message and the train status messages,
   in the one format (9).
4. Failure handling, steps 1, 2, 3, 5 and 6 of section 6: the environmental
   retry, dropping a single suspect, the `main` check, handing the dropped
   pull request back, and the cap. Any other failure holds the train and tells
   the owner.
5. The cost guard's train allowance (memql-znas).
6. The GitHub rules and CODEOWNERS, `merge-as-owner.sh --emergency`, the
   `CLAUDE.md` Branch Workflow, and the docs: a "Release trains" section in
   [pipelines.md](../../public/operate/pipelines.md), the
   [harness decision map](../../internal/ops/harness-decisions.md), and
   memql-znas's `docs/deployment-notifications.md`.

**Right after the switch:**

- The two lanes (5.1). Until then every pull request runs the full pipeline,
  but no pull request re-runs because another merged, since nothing merges on
  its own.
- Splitting a train (step 4 of section 6).
- `main`'s push run verified by the train (5.2).
- The quick stages ahead of `cockpit` in the train lane.
- The audit and fixes of today's Discord messages, and golden tests for every
  message kind.

The train code itself ships through today's flow: its pull requests merge with
`merge-as-owner.sh`, and a release carries it to production. The switch
happens once it runs there.

## 11. Testing

- The decisions are pure functions over values, with table tests: boarding,
  ordering, conflicts, suspects, the split, state transitions, the landing
  conditions and message composition.
- The GitHub calls run against a fake GitHub server.
- Cross-node: a train whose coordinator moves to another replica in the middle
  of a run resumes from its record, tested in the style of `test/clustere2e/`.
- Each Discord message kind has a golden test of its exact payload.

## 12. Switch-over

There is no shadow period (2): production has no clients relying on it yet.

1. **Switch** as soon as the first release (10) runs in production: save the
   ruleset; change CODEOWNERS and the ruleset; make `merge-as-owner.sh`
   emergency-only; update `CLAUDE.md`; turn the train setting on. The next
   scheduled departure is the first live train, and the owner's approval of it
   is the acceptance test.
2. **Fallback.** Turn the setting off, restore the saved ruleset, and merge with
   `merge-as-owner.sh --emergency` until the train is fixed.

## 13. Not in this project

- Building the release in the train run and approving it with the train
  (project 2).
- Updating production from the release (project 3).
- Turning trains on for other repositories, memql-znas included.

## 14. To settle in planning

- The PR lane's exact stage list and its measured time.
- Whether a split's "only the failing steps" runs can be expressed with the
  installed stage selectors, as expected, or need an engine change.
- The owner's time zone and departure times.
- The cost guard's allowance sizes.

## 15. Assumptions not yet verified

- Verified 2026-10-28: GitHub marks a pull request merged when its head
  reaches `main` through another pull request's merge. memql#6325's merge
  marked #6318, #6319, #6320, #6321, #6322, #6323 and #6324 merged.
- The app's token may create `train/*` branches and merge into them with
  `POST /merges` under this repository's rulesets. Today's rulesets target
  `main`.
- The owner's review of an app-authored pull request satisfies
  `require_code_owner_review`. The ruleset also sets
  `require_extra_approval_for_unattributed_changes: true`; what it asks of a
  pull request carrying commits by several authors is checked on the first
  live train.
