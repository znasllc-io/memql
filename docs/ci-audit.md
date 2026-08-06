# CI Bottleneck Audit

**Repo:** `znasllc-io/memql` (public, Team plan, 2 seats)
**Audit date:** 2026-08-06
**Scope:** read-only. Workflow inventory, ruleset/branch protection, last 50
workflow runs, last 20 PRs, job- and step-level timings.
**Status:** findings only. No solutions proposed, per the brief.

---

## Summary

Three independent things are being conflated under "CI is slow":

1. **CI is not a gate right now — because the gate was removed today as an
   incident workaround and has not been restored.** The repository currently
   has **zero required status checks**. Until 2026-08-06T19:29:07Z the
   `default` ruleset enforced `required_status_checks` (3 checks) **and**
   `merge_queue`; both were removed in a single ruleset edit, and merges
   resumed 55 seconds later. This was a deliberate operator action taken
   because the platform incident in point 2 had made CI unpassable — not a
   repo that was never configured. The exposure is that the workaround is
   still in effect.

2. **The pain observed on 2026-08-06 is a GitHub Actions platform
   incident, not this repo's configuration.** All 15 non-green runs in the
   last 50 trace to infrastructure: jobs cancelled after exactly 15 minutes
   without ever being assigned a runner, or jobs failing with
   `Failed to resolve action download info. Error: Service Unavailable`.
   **Zero genuine test or code failures appear in the sampled window.**

3. **Underneath that, the pipeline is genuinely wasteful** — roughly 40
   job-minutes per PR revision (82 when the merge queue was active), of
   which the critical path is only 6.5 minutes. CodeQL runs twice on every
   PR. Six matrix jobs contribute nothing to the critical path.

The time-cost ranking is at the end. Broken and wasteful are separated.

---

## 1. Workflow inventory

Measured wall-clock is the median of successful runs in the sample; `n` is
the sample size. "Reports as" is the check name GitHub publishes.

### 1.1 PR-triggering workflows

| Workflow | File | Triggers | Concurrency | Path filters |
|---|---|---|---|---|
| CI | `ci.yml` | `pull_request`→main, `push`→main, `merge_group` | `CI-${PR‖sha}`, cancel-in-progress | per-job, via `changes` |
| CodeQL | `codeql.yml` | `pull_request`→main, `push`→main, `merge_group`, cron Mon 06:00 | `CodeQL-${PR‖sha}`, cancel-in-progress | none |
| gitleaks | `gitleaks.yml` | `pull_request`→main, `push`→main, `merge_group`, cron Mon 06:00 | `gitleaks-${PR‖sha}`, cancel-in-progress | none |
| deploy-gate-image | `deploy-gate-image.yml` | `pull_request`→main, `push`→main | none | `cmd/deploy-gate-check/**`, `go.mod`, `go.sum`, own workflow |
| govulncheck | `govulncheck.yml` | `push`→main, `merge_group`, cron Mon 06:00 | `govulncheck-${ref}`, **no** cancel | none |

INFO: `govulncheck` has **no `pull_request` trigger**. It cannot gate a PR
and never reports a check on one.

### 1.2 Non-PR workflows (release / dispatch / scheduled)

| Workflow | Triggers |
|---|---|
| `build-engine-images.yml` | `workflow_dispatch` only (matrix of 8 node types) |
| `dispatch-engine-images-on-release.yml` | `release: published` |
| `publish-docs-bundle.yml` | `release: published`, `workflow_dispatch` |
| `publish-sdk-core.yml` | `push` tags `memql-sdk-core-v*`, `workflow_dispatch` |
| `sbom.yml` | `release: published`, `workflow_dispatch`, `push`→main on `go.mod`/`go.sum` |
| `scorecard.yml` | `branch_protection_rule`, cron Mon 07:00, `push`→main |
| `mirror-fixture-images.yml` | `workflow_dispatch` only |

These are not on the PR path and are not a bottleneck. They are listed for
completeness.

### 1.3 CI job breakdown

`ci.yml` defines 9 jobs; the `build-node-tags` matrix expands to 6, so a
full CI run is **14 jobs**.

| Job id | Reports as | Needs | Median | n | Matrix |
|---|---|---|---|---|---|
| `changes` | `changes` | — | 0.1m | 6 | — |
| `go-checks` | `go-checks` | changes | **6.3m** | 6 | — |
| `db-tests` | `db-tests` | changes | 5.8m | 6 | — (postgres service) |
| `vscode-extension` | `vscode-extension` | changes | 4.0m | 2 | — |
| `build-voice` | `build voice (cgo)` | changes | 2.5m | 6 | — |
| `build-node-tags` | `build node tags (<tag>)` | changes | 1.9m each | 6×6 | identity, mcp, cognition, agent, planner, workbench |
| `conformance` | `mcp-conformance` | changes | 1.6m | 6 | — (postgres service) |
| `sdk-ts-typecheck` | `sdk-ts-typecheck` | changes | 0.3m | 2 | — |
| `ci-required` | `ci-required` | all 8 above | 0.1m | 6 | — |

**Caching:** every Go job uses `actions/setup-go@v7` with `cache: true`
(module + build cache). `sdk-ts-typecheck` and `vscode-extension` use
`actions/setup-node@v4` **without** a `cache:` key. There are no explicit
`actions/cache` steps anywhere.

**Timeouts:** WARNING: only one `timeout-minutes` exists in the entire
`.github/workflows` tree — `publish-docs-bundle.yml:49` (20 min). No CI job
has a timeout, so every CI job inherits the 360-minute default.

**Path filter buckets** (`changes` job, `dorny/paths-filter`): `ci`, `gates`,
`go`, `dsl`, `sdkts`, `voice`, `proto`, `vscode`. The `gates` bucket
(`**/*.md`, `**/*.memql`, `scripts/**`, two manifests,
`component/architecture/embedded/**`) exists specifically to close a
previously-shipped fail-open documented inline as memql#2972.

---

## 2. Ruleset and branch protection

### 2.1 Classic branch protection

```
$ gh api repos/znasllc-io/memql/branches/main/protection
{"message":"Branch not protected","status":"404"}
```

### 2.2 Active rulesets

Two, both `active`, both scoped to `~DEFAULT_BRANCH`:

**`default` (id 16630577)** — updated 2026-08-06T12:29:07-07:00
- `deletion`
- `non_fast_forward`
- `pull_request`: `required_approving_review_count: 0`,
  `require_code_owner_review: true`, `require_last_push_approval: false`,
  `required_review_thread_resolution: false`,
  `allowed_merge_methods: [rebase, merge]`

**`Code Quality Copilot review for default branch` (id 19450314)**
- `copilot_code_review`: `review_on_push: true`,
  `review_draft_pull_requests: true`

### 2.3 REQUIRED STATUS CHECKS — removed 2026-08-06T19:29:07Z

**Currently there are none:**

```
$ gh api repos/znasllc-io/memql/rules/branches/main
[copilot_code_review, deletion, non_fast_forward, pull_request]
```

Confirmed a second way from the PR side — every check reports
`isRequired: null`:

```
$ gh pr view 3153 --json statusCheckRollup
Analyze (go)                    SUCCESS req=None
Analyze (javascript-typescript) SUCCESS req=None
Analyze (python)                SUCCESS req=None
```

**But they existed earlier today.** The rule-suite evaluation history
(`gh api repos/znasllc-io/memql/rulesets/rule-suites`) records which rules
were evaluated on each push to `main`, and shows the exact moment the gate
was dropped. All times PT, as returned:

| Pushed at (PT) | UTC | Rules evaluated on `main` | Result |
|---|---|---|---|
| 07:59:59 | 14:59:59Z | `required_status_checks`, `pull_request`, `deletion` | pass |
| 12:08:38 | 19:08:38Z | `secret_scanning`, `pull_request`, **`required_status_checks`**, **`merge_queue`**, `non_fast_forward`, `deletion` | **fail** |
| 12:23:46 | 19:23:46Z | **`required_status_checks`**, **`merge_queue`**, `pull_request`, `non_fast_forward`, `deletion` | **fail** |
| **12:29:07** | **19:29:07Z** | **ruleset 16630577 updated** | — |
| 12:30:02 | 19:30:02Z | `pull_request`, `non_fast_forward`, `deletion` | pass |
| 12:55:44 | 19:55:44Z | `secret_scanning`, `pull_request`, `non_fast_forward`, `deletion` | fail |
| 13:01:14 | 20:01:14Z | `pull_request`, `non_fast_forward`, `deletion` | pass |

`required_status_checks` and `merge_queue` are evaluated up to 19:23:46Z and
never again after the 19:29:07Z ruleset edit.

The two failing evaluations name the blocker precisely:

```
suite 3585892724  2026-08-06T12:08:38-07:00  refs/heads/main  FAIL
  fail  required_status_checks  3 of 3 required status checks are expected.
  fail  merge_queue             Changes must be made through the merge queue
  fail  pull_request            Changes must be made through a pull request.

suite 3586014227  2026-08-06T12:23:46-07:00  refs/heads/main  FAIL
  fail  required_status_checks  3 of 3 required status checks have not succeeded: 2 expected.
  fail  merge_queue             Changes must be made through the merge queue
```

"3 of 3 required status checks are expected" means all three required checks
were **never reported** — the signature of §4.3 starvation, where the
workflow runs never produced a check at all.

INFO: the names of the three required checks are not recoverable from the
API once the rule is removed, and ruleset edit history is not exposed. Given
what the merge queue ran, `ci-required`, `scan` (gitleaks) and `CodeQL` are
the plausible set, but this audit cannot confirm it.

INFO: `secret_scanning` appears only in suites for direct pushes to `main`
(19:08:38Z, 19:55:44Z). There are no push-target rulesets on this repo, so
this is GitHub secret-scanning push protection surfacing as a rule
evaluation, not a ruleset rule — it was not part of the 19:29:07Z edit.

The only merge gate on `main` today is code-owner review. `.github/CODEOWNERS`
assigns `*` to `@znas-io` — the sole reviewer, and the same account that
authors the PRs.

---

## 3. Cross-reference: required checks vs. reported jobs

As of the audit there are no required checks, so no required check can fail
to report. The useful inversion is: **which jobs were built to be required,
and are they today?** ("Required until 19:29Z" marks the three-check set
removed in §2.3 — membership inferred, not confirmed.)

| Check name | Intended role | Required today | Reports on every PR? |
|---|---|---|---|
| `ci-required` | Single aggregator gate over all 8 CI lanes | **NO** (likely required until 19:29Z) | Only when the CI workflow runs at all |
| `go-checks` | Build/vet/test/lint/drift | NO | Path-skippable |
| `db-tests` | DB-gated suites | NO | Path-skippable |
| `mcp-conformance` | Conformance regression gate | NO | Path-skippable |
| `build voice (cgo)` | CGO build | NO | Path-skippable |
| `build node tags (×6)` | Tagged builds | NO | Path-skippable |
| `sdk-ts-typecheck` | SDK typecheck | NO | Path-skippable |
| `vscode-extension` | Editor asset drift | NO | Path-skippable |
| `scan` (gitleaks) | Secret scan | **NO** (likely required until 19:29Z) | Yes, when triggered |
| `CodeQL` / `Analyze (go)` | SAST | **NO** (likely required until 19:29Z) | Yes, when triggered |
| `Analyze (python)`, `Analyze (javascript-typescript)` | Code Quality SAST | NO | Yes (dynamic) |
| govulncheck `scan` | Vuln scan | NO | **Never — no `pull_request` trigger** |

The `ci-required` job is well-designed for the role it is not being given.
It uses `if: always()`, aggregates `join(needs.*.result, ' ')`, treats
`success|skipped` as pass, and explicitly refuses to pass on an empty
result set — with an inline comment naming the fail-open it backstops:

> `# An empty result set is a fail-open, not a pass: it means the`
> `# aggregator waited on nothing, so EVERY lane is advisory at once`
> `# (memql#3019). The static guard for this lives in go-checks -- which`
> `# is itself one of the aggregated lanes, so emptying`needs`also`
> `# stops that guard gating. This is the independent backstop.`

It works. Run 31114735246 shows it correctly rejecting
`success success success success success abandoned abandoned success`.
Nothing consumes the verdict.

### 3.1 What the last five merges actually show

Check-run coverage on the head SHA of the last five merged PRs:

| PR | Merged | Checks present on head SHA |
|---|---|---|
| #3121 | 2026-08-06T03:04Z | full set (20) incl. `ci-required`, `go-checks`, `db-tests`, `scan` |
| #3130 | 2026-08-06T11:23Z | full set (20) |
| #3136 | 2026-08-06T15:12Z | full set (20) |
| **#3152** | 2026-08-06T19:30Z | **`Analyze (go)`, `Analyze (python)`, `Analyze (javascript-typescript)` only** |
| **#3153** | 2026-08-06T20:01Z | **`Analyze (go)`, `Analyze (python)`, `Analyze (javascript-typescript)` only** |

PR #3153 was created 19:57:42Z and merged 20:01:14Z — **3m32s**, which is
shorter than the 6.5-minute CI critical path. The CI, gitleaks, and CodeQL
workflows produced no run for its head SHA at all:

```
$ gh api "repos/.../actions/runs?head_sha=219edbc1928fdeebd602d03223db9c642746c6b3"
Code Quality: PR #3153   dynamic   success      <- the only run
```

**These two merges are not evidence of a chronically unguarded repo.** Per
the repository owner, they were merged deliberately by dropping the ruleset
rules, precisely because the §4 platform incident had made the required
checks unpassable — without that, neither merge would have been possible.
The rule-suite record in §2.3 corroborates this exactly: `main` was failing
`required_status_checks` and `merge_queue` at 19:08:38Z and 19:23:46Z, the
ruleset was edited at 19:29:07Z, evaluation passed at 19:30:02Z, and PR
#3152 merged at 19:30:03Z — one second later.

The correct reading of the split in the table above is therefore: PRs #3121,
#3130 and #3136 merged under an enforced three-check gate; #3152 and #3153
merged after that gate was deliberately lifted to work around the incident.

The finding is not "nothing ever blocked anything." It is that **the
incident workaround is still live** — `required_status_checks` and
`merge_queue` remain absent from the ruleset as of this audit
(§2.3), so the next PR merges under the same lifted gate whether or not
anyone intends it to. Both incident merges are on `main` now (`3730ec1b`,
`dbfc84a3`) and neither was verified by CI.

---

## 4. Failure analysis

### 4.1 Run volume and shape

200 runs pulled, spanning 2026-08-05T23:48Z → 2026-08-06T20:01Z (~20 hours).

| Day | Runs | Success | Failure | Cancelled | p50 | max |
|---|---|---|---|---|---|---|
| 2026-08-05 | 21 | 21 | 0 | 0 | 5.6m | 11.3m |
| 2026-08-06 | 179 | 161 | 13 | 5 | 5.4m | **154.1m** |

The median is stable at ~5.5m on both days. **The p50 is fine; the tail is
the entire problem.** 179 runs in one day is itself notable — the run mix
over the last 200:

```
  42  CodeQL (dynamic / Code Quality)
  28  CodeQL (pull_request)
  28  gitleaks (pull_request)
  28  CI (pull_request)
   9  each of: Scorecard/gitleaks/CodeQL/govulncheck/CI (push→main)
   8  CI (merge_group)      7 each: gitleaks/CodeQL/govulncheck (merge_group)
```

### 4.2 Classification of every non-green run in the last 50

15 of 50 runs were non-green. Classified:

| Run ID | Workflow | Event | Branch | Result | Starved jobs | Failed jobs | Class |
|---|---|---|---|---|---|---|---|
| 31126921970 | CodeQL | dynamic | main | failure | 2 | — | infra |
| 31126678407 | CodeQL | dynamic | pull/3152 | failure | 3 | — | infra |
| 31119998665 | CodeQL | pull_request | issue/3120 | failure | 1 | — | infra |
| 31119998624 | gitleaks | pull_request | issue/3120 | failure | 1 | — | infra |
| 31119998594 | CI | pull_request | issue/3120 | cancelled | 7 | — | infra |
| 31119995504 | CodeQL | dynamic | pull/3151 | failure | 2 | `Analyze (go)` | infra |
| 31117594609 | CI | pull_request | issue/3120 | failure | 1 | `changes` | infra |
| 31117594456 | gitleaks | pull_request | issue/3120 | cancelled | 1 | — | infra |
| 31117594440 | CodeQL | pull_request | issue/3120 | failure | 1 | — | infra |
| 31117592952 | CodeQL | dynamic | pull/3151 | failure | 2 | — | infra |
| 31117477962 | CI | pull_request | issue/3123 | failure | 1 | `changes` | infra |
| 31117475349 | CodeQL | dynamic | pull/3150 | failure | 1 | `Analyze (go)` | infra |
| 31116571475 | CI | pull_request | issue/3096 | failure | 9 | `ci-required` | infra (cascade) |
| 31116565297 | CodeQL | dynamic | pull/3137 | failure | 0 | `Analyze (python)`, `Analyze (javascript-typescript)` | infra |
| 31114735246 | CI | **push→main** | main | failure | 0 | `db-tests`, `mcp-conformance`, `ci-required` | infra (cascade) |

**Real failures: 0. Flakes: 0. Config errors: 0. Never-reported: 0.
Infra/timeout: 15 of 15.**

### 4.3 Infra failure mode A — runner starvation (15-minute hard cancel)

Starved jobs never receive a runner: `runner_name: ""`, `steps: []`.

```json
{"name":"build voice (cgo)","conclusion":"cancelled","runner_name":"",
 "created_at":"2026-08-06T17:25:40Z","started_at":"2026-08-06T17:25:40Z",
 "completed_at":"2026-08-06T17:40:43Z","steps":[]}
```

Every starved job dies at **exactly 15m00s ± 4s** after being queued:

| Queued | Cancelled | Delta | Workflow | Branch |
|---|---|---|---|---|
| 16:29:14 | 16:44:16 | 15m02s | Code Quality PR#3151 | pull/3151 |
| 17:05:52 | 17:20:55 | 15m03s | CI | issue/3120 |
| 17:25:40 | 17:40:43 | 15m03s | CI (9 jobs) | issue/3096 |
| 17:26:34 | 17:41:36 | 15m02s | gitleaks | issue/3120 |
| 17:26:55 | 17:41:57 | 15m02s | CodeQL | issue/3120 |
| 18:09:11 | 18:24:15 | 15m04s | CI | issue/3120 |

In the 16:00–19:00Z window, **27 of 60 jobs (45%) were starved**.

Competing explanations, each tested and eliminated:

- [x] **Org concurrency limit** — ELIMINATED. Measured peak concurrent job
      slots in that window: **14**. Team plan allows 60.
- [x] **Actions minutes exhausted / spending limit** — ELIMINATED. The repo
      is **public** (`"visibility":"public"`), so standard runners are free
      and unmetered.
- [x] **`cancel-in-progress` push supersession** — ELIMINATED. Branch
      `issue/3096-ensureschema-setenv-after-load` has exactly **one** CI run
      (31116571475) in the entire history window. Its 9 jobs were cancelled
      at 17:40:43 with no superseding run in the concurrency group.
- [x] **One synchronized external cancellation event** — ELIMINATED. The
      cancellation instants are spread across 16:44, 17:05, 17:20, 17:40,
      17:41, 17:41 and 18:24, each exactly 15m after its own queue entry.

What remains is a GitHub-side runner-assignment failure with a 15-minute
scheduling deadline.

WARNING: The 15-minute constant is not a GitHub timeout I can verify from
here, and org-level Actions settings were **not readable** — `gh api
orgs/znasllc-io/actions/permissions` returns HTTP 403 (needs `admin:org`).
An org-level runner-group or policy has not been ruled out and is the one
remaining hypothesis this audit could not test.

### 4.4 Infra failure mode B — action download service unavailable

Jobs that *did* get a runner failed before executing a single step, and
report a distinct `abandoned` result. This is what broke `main`:

```
Run 31114735246 (CI, push→main), job db-tests (92661251908):
  2026-08-06T15:12:56Z  Prepare all required actions
  2026-08-06T15:14:30Z  Failed to resolve action download info. Error: Service Unavailable
  2026-08-06T15:14:30Z  Retrying in 29.726 seconds
  2026-08-06T15:16:40Z  Failed to resolve action download info. Error: Service Unavailable
  2026-08-06T15:17:43Z  ##[error]Service Unavailable
  2026-08-06T15:17:43Z  ##[error]Failed to resolve action download info.
```

`ci-required` then correctly caught the cascade:

```
  RESULTS: success success success success success abandoned abandoned success
  FAIL: a required lane reported 'abandoned'
  ci-required: at least one lane did not pass
```

The same signature explains the two Code Quality "failures" in run
31116565297 (`Analyze (python)`, `Analyze (javascript-typescript)`) — both
are `Service Unavailable` at the action-resolution step, not language
misconfiguration. INFO: the repo genuinely contains Python (74 KB) and
TypeScript (257 KB), so scanning those languages is legitimate.

### 4.5 Last 20 PRs

4 merged, 16 closed without merge (5 of those drafts). Open-to-merge latency
for the merged set: 3m, 20m, 10h24m, 11h27m. The bimodality maps exactly to
section 3.1 — the two fast merges are the two that ran no CI.

---

## 5. Critical path to green

For a Go-touching PR where every lane runs:

```
changes (0.1m)
   |
   +-- go-checks .............. 6.3m   <== CRITICAL PATH
   +-- db-tests ............... 5.8m
   +-- vscode-extension ....... 4.0m
   +-- build voice (cgo) ...... 2.5m
   +-- build node tags x6 ..... 1.9m each, fully parallel
   +-- mcp-conformance ........ 1.6m
   +-- sdk-ts-typecheck ....... 0.3m
   |
   +-- ci-required (0.1m)

CRITICAL PATH = 0.1 + 6.3 + 0.1 = 6.5m
```

Everything except `go-checks` finishes with 0.5m–6.0m of slack. The pipeline
is already well-parallelized; the wall-clock is one job.

### 5.1 Inside `go-checks` — 15 serial steps, 364s total

Step timings from run 31112993211:

| Step | Time | Share |
|---|---|---|
| `go test -count=1 -timeout=300s ./...` | **223s** | **61%** |
| `go build ./...` | 70s | 19% |
| `go vet ./...` | 26s | 7% |
| `install protoc` (`apt-get update && apt-get install`) | 14s | 4% |
| `go test -tags mcp` | 11s | 3% |
| checkout + setup-go | 5s | 1% |
| `go test -tags identity` | 3s | <1% |
| `dsl lint` | 3s | <1% |
| `engine load check` | 3s | <1% |
| `proto-gen-check` | 2s | <1% |
| `go test -tags agent` | 1s | <1% |
| `harness eval` | 1s | <1% |
| `env-registry-check` | 1s | <1% |
| `sdk-gen-check` | 1s | <1% |
| `go test (gate inputs)` | skipped | — |

A single `go test ./...` invocation is **57% of the entire wall-clock to
green** (223s of 390s).

### 5.2 Cost accounting

Measured job-minutes for PR #3136, one revision:

| Run | Jobs | Job-minutes |
|---|---|---|
| CI (31112993211) | 12 | 26.8 |
| Code Quality dynamic (31112986644) | 3 | 7.0 |
| CodeQL pull_request (31112990764) | 1 | 5.3 |
| gitleaks (31112990700) | 1 | 0.9 |
| **PR subtotal** | **17** | **40.0** |
| CI merge_group (31113705653) | 14 | 29.6 |
| gitleaks merge_group (31113707454) | 1 | 11.7 |
| govulncheck merge_group (31113705711) | 1 | 0.3 |
| CodeQL merge_group (31113707409) | 1 | 0.1 |
| **Total to land one PR** | **34** | **81.8** |

40 job-minutes buy a 6.5-minute answer. Every push to the branch repeats it.

### 5.3 Merge queue status

Merge queue was in use through 2026-08-06T15:00:19Z (100+ `merge_group` runs
in history, most recent `gh-readonly-queue/main/pr-3136-...`). The
`merge_queue` rule was **still being enforced** as late as 19:23:46Z — it
appears as a failing evaluation in rule suite 3586014227 ("Changes must be
made through the merge queue") — and disappears from all evaluations after
the 19:29:07Z ruleset edit. It was removed in the same edit as
`required_status_checks` (§2.3).

The gap between the last `merge_group` run (15:00:19Z) and the rule's
removal (19:29:07Z) is consistent with the incident: the queue rule was
still in force, but no PR could get through it, because entering the queue
requires the required checks to pass and they were never reporting.

The `merge_group:` triggers still present in `ci.yml`, `codeql.yml`,
`gitleaks.yml` and `govulncheck.yml` are currently dead trigger paths, and
will come back to life if the `merge_queue` rule is restored.

---

## 6. Findings ranked by time-cost and blast radius

### BROKEN

**B1. The incident workaround is still live: `required_status_checks` and
`merge_queue` were removed from the ruleset at 19:29:07Z and not restored.
Blast radius: every future merge to `main`.**
Both rules were enforced as recently as 19:23:46Z (rule suite 3586014227)
and are absent from every evaluation after the 19:29:07Z edit. Removing them
was a legitimate operator decision under B3/B4 — the required checks were
physically unable to report, so `main` was unmergeable. The finding is the
**restore step**, not the removal: as of this audit the repo still has zero
required checks, no merge queue, and code-owner review by the PR author as
its only gate. The `ci-required` aggregator that was built for this role is
currently wired to nothing.
Evidence: §2.3, §3, §5.3.

**B2. Two merges landed on `main` unverified while the gate was lifted.
Blast radius: two unverified commits on the default branch.**
PRs #3152 and #3153 merged at 19:30:03Z and 20:01:14Z with only the three
Code Quality `Analyze` checks present on their head SHA — no CI, no
gitleaks, no CodeQL run exists for either. #3153 merged 3m32s after opening,
faster than the 6.5m critical path. Per the repository owner these were
deliberate incident merges, and the rule-suite timeline confirms the gate
was lifted 56 seconds before the first of them. This is the cost of the
workaround, correctly attributed — not evidence of a chronically open repo.
The commits are `dbfc84a3` and `3730ec1b`.
Evidence: §3.1.

**B3. Runner starvation cancels 45% of jobs at exactly 15 minutes.
Blast radius: every workflow, every PR, during the window.**
27 of 60 jobs in 16:00–19:00Z; peak concurrency 14 against a 60-job limit;
public repo, so unmetered. Not concurrency, not billing, not supersession,
not a single synchronized event. Cost: runs of 154m, 129m, 113m, 73m against
a 5.4m median. Residual unknown: org-level Actions policy is unreadable
without `admin:org`.
Evidence: §4.3.

**B4. Action-download service failures abandon jobs mid-prepare.
Blast radius: broke `main`'s CI run and 3 PR runs.**
`Failed to resolve action download info. Error: Service Unavailable` after
two retries, producing the `abandoned` job result. Run 31114735246 on
`push→main`.
Evidence: §4.4.

**B5. `govulncheck` cannot gate a PR.**
`on:` is `push`→main, `merge_group`, and cron only — no `pull_request`.
Vulnerabilities are detected only after merge. With `merge_group` now dead
(§5.3), its only pre-merge path is gone.
Evidence: §1.1.

**B6. No CI job has a `timeout-minutes`.**
One timeout exists repo-wide (`publish-docs-bundle.yml:49`). Every CI job
inherits the 360-minute default, so a hung job holds a slot for six hours.
The 154m run in this sample was not itself a hang, but nothing bounds one.
Evidence: §1.3.

### WASTEFUL

**W1. CodeQL runs twice on every PR. ~5.2 job-minutes duplicated per PR.**
`.github/workflows/codeql.yml` runs `Analyze (go)`; GitHub Code Quality
independently runs `Analyze (go)` plus `python` and
`javascript-typescript` as `dynamic` runs
(`path: dynamic/github-code-scanning/codeql`). Both publish an
`Analyze (go)` check — visible twice in the check list for PRs #3121,
#3130, #3136. Code Quality also re-runs on every push
(`review_on_push: true`), which is why `dynamic` is the single highest-volume
run type in the sample (42 of 200, more than CI's 28).
Evidence: §1.1, §3.1, §4.1, §5.2.

**W2. `build node tags` × 6 costs ~11.4 job-minutes and zero wall-clock.**
Six jobs, each `go build -tags X ./...` + `go vet -tags X ./...`, median
1.9m — finishing 4.4m before `go-checks`. They occupy 6 of the 14 CI job
slots. Under B3 they compete for runners against the job that is actually on
the critical path.
Evidence: §1.3, §5.

**W3. `go build ./...` (70s) + `go vet ./...` (26s) precede
`go test ./...` (223s) in the same serial job.**
`go test` compiles the tree itself. The separate build step does catch
non-test build breakage, so this is redundancy with a purpose, but it is
96s of the 390s critical path.
Evidence: §5.1.

**W4. `install protoc` shells out to `apt-get update && apt-get install`
on the critical path.** 14s per run, uncached, network-dependent — and
`apt-get` is exactly the class of step that fails during an infra incident.
Evidence: §5.1.

**W5. Node jobs have no dependency cache.**
`sdk-ts-typecheck` and `vscode-extension` use `actions/setup-node@v4` with
no `cache:` key, then run `make sdk-ts-install`. Small today
(`sdk-ts-typecheck` 0.3m) but `vscode-extension` is 4.0m and is the
third-longest lane.
Evidence: §1.3.

**W6. Dead `merge_group` triggers in four workflows.**
No merge queue is configured; four workflows still declare `merge_group:`.
Harmless today, misleading when reading the pipeline.
Evidence: §5.3.

### NOT A PROBLEM (checked, ruled out)

- Concurrency groups are correct on CI, CodeQL and gitleaks
  (`${workflow}-${PR‖sha}`, cancel-in-progress). `govulncheck` deliberately
  keys on `${ref}` without cancellation.
- Go build/module caching is enabled on all five Go jobs.
- Path filtering is thorough, and the `gates` bucket documents a real
  fail-open it was added to close (memql#2972 / memql#3019).
- The `ci-required` aggregator's logic is sound, including its empty-result
  refusal — it caught the `abandoned` cascade correctly.
- Median run time (5.4–5.6m) is stable and did not regress.

---

## 7. Method and limitations

**Commands used:** `gh api repos/znasllc-io/memql/rulesets`,
`.../rulesets/{id}`, `.../rulesets/rule-suites`,
`.../rulesets/rule-suites/{id}`, `.../rules/branches/main`,
`.../branches/main/protection`, `.../actions/runs?per_page=100`,
`.../actions/runs/{id}/jobs`, `.../actions/jobs/{id}/logs`,
`.../commits/{sha}/check-runs`, `gh run list --limit 200`,
`gh pr list --limit 20 --state all`, `gh api repos/.../issues/{n}/timeline`.

**Correction made during the audit.** The first pass concluded that required
status checks had simply never been configured, and read the two fast merges
as proof of a permanently open repo. The repository owner corrected this:
the merges were deliberate, taken because the platform incident had made CI
unpassable. Re-querying `rulesets/rule-suites` — which records per-push rule
evaluations and survives the rule's deletion — confirmed the owner's account
and produced the §2.3 timeline. The `rulesets/{id}` snapshot alone shows
only current state and cannot distinguish "never configured" from "removed
today"; that distinction changes B1 and B2 substantially.

**Limitations:**
- Org Actions policy is **not readable** with the current token
  (HTTP 403, needs `admin:org`). A self-hosted runner group or org-level
  policy is the one hypothesis for B3 that could not be tested.
- The sample spans ~20 hours (2026-08-05T23:48Z → 2026-08-06T20:01Z). 200
  runs was the full page size; 179 of them fall on the incident day, so the
  healthy baseline (21 runs, all green) is thin.
- Logs for runs older than the retention window return
  `BlobNotFound` (HTTP 404); the `changes` job failures in runs 31117594609
  and 31117477962 were classified as infra by correlation with their
  starved siblings and the run-wide pattern, **not** from their own logs.
- Ruleset **edit** history is not exposed, and the org audit log is
  unavailable (HTTP 404 without `admin:org`). The 19:29:07Z change to
  ruleset 16630577 was reconstructed indirectly from `rulesets/rule-suites`
  evaluation records (§2.3), which show which rules were evaluated before
  and after. This pins *what* changed and *when*, but the exact names of the
  three formerly-required checks are unrecoverable.
- Step-level timings are from single representative runs; job-level medians
  are over n=2–6 successful runs as noted per row.
