---
title: CI redesign — decision record
audience: ops
status: proposed
area: internal
sinceVersion: 0.14.0
owner: znas
---

# CI redesign — decision record

**Status:** proposed, except Phase 0 and O5 which are **applied** (ruleset
16630577, 2026-08-06T21:32Z and 21:47Z — see Sequencing and Open Items).
No workflow or Go file is modified by this document.
**Input:** [ci-audit.md](ci-audit.md) — the measured audit this responds to.
**Related:** [internal/ops/tier4-build-graph.md](internal/ops/tier4-build-graph.md)
(CI-acceleration epic #854, Tiers 0–3 shipped, Tier 4 = Bazel north star).
This design sits between Tier 3 and Tier 4 and does not conflict with either.

Every number in this document is measured from the audit sample unless
labelled ESTIMATE.

---

## Decisions already taken

Settled in discussion; recorded here so the rationale survives.

| # | Decision | Rationale |
|---|---|---|
| A | **Merge queue stays off.** Gate + up-to-date branch instead. | Restoring it doubles per-merge cost (40 → 82 job-min measured on PR #3136) and adds a full CI cycle of latency. Revisit only if real semantic conflicts appear on `main`. |
| B | **Keep GitHub Code Quality; retire `codeql.yml`.** | Code Quality already covers go + python + javascript-typescript and reports on every PR. Saves 5.3 job-min/PR. Cost accepted: query suite is not pinned in-repo. |
| C | **Real Go modules, compiler-enforced boundaries.** | Independent versioning is the goal, and it is the one goal that genuinely requires modules rather than path filters or an arch test. |
| D | **Only `wire` and `engine` get independent versions.** Everything else lockstep. | They are the only modules with external consumers (`sdk/go` imports `wire`; the engine is the embedded product). Kubernetes model, not google-cloud-go: a deep dependency chain makes independent versioning of interior modules pure bookkeeping. |
| E | **Extract the misfiled L0 leaves in a standalone prerequisite PR.** | Pure mechanical move, independently reviewable, unblocks everything downstream, and leaves the tree honest even if the split stalls. |
| F | **CI ships first**, with path filters written against target module boundaries. | The gate is off right now — that is the live exposure and it should not wait weeks on a refactor. Path filters are directory-scoped, so the split becomes a no-op for CI. |

---

## D1. Restoring the gate

### The failure being designed against

Three checks were required by name. During the 2026-08-06 platform incident
they never reported at all, and the ruleset reported
`3 of 3 required status checks are expected` — a permanent block with no
failing job to look at. Requiring a name that can fail to appear is the
defect. Requiring three of them triples the exposure.

### Finding: the gate already exists and is already correct

`ci-required` in `ci.yml` needs **no changes** to serve as the single gate.
It already satisfies every property required:

| Property | Required because | Current state |
|---|---|---|
| Always runs | A skipped gate reports nothing → "expected" forever | `if: ${{ always() }}` |
| Depends on every lane | Otherwise a lane can fail silently | `needs:` lists all 8 |
| Treats `skipped` as pass | Path-skipped lanes must not block | `case success\|skipped` |
| Treats `cancelled`/`abandoned` as fail | Starvation must not read as green | anything else sets `status=1` |
| Refuses an empty result set | Emptying `needs` would fail open (memql#3019) | explicit `-z` guard |
| Its workflow has no trigger-level path filter | A filtered workflow never runs → never reports | `ci.yml` triggers are unfiltered |

It was observed working correctly during the incident (run 31114735246):

```
RESULTS: success success success success success abandoned abandoned success
FAIL: a required lane reported 'abandoned'
```

**The entire fix is a ruleset change. No workflow edit is required to restore
the gate.**

### Alternatives considered

| Option | Tradeoff |
|---|---|
| **Require `ci-required` only** | One name, one workflow, one re-run to recover. Does not eliminate never-reported (if the CI workflow never starts, nothing reports) but reduces the surface from 3 independent workflows to 1 and makes recovery a single action. |
| Require each lane by name (status quo ante) | Granular PR feedback about which lane blocked. Multiplies the never-reported surface by the lane count, and every new lane is a ruleset edit. This is the configuration that produced the incident. |
| Require `ci-required` + `scan` + Code Quality | Secret-scan and SAST gate independently of CI health. Reintroduces two more names that can fail to report — and gitleaks was starved in the incident (run 31119998624). |

### RECOMMENDATION

Ruleset `default` (16630577) gains exactly one rule:

```
required_status_checks:
  required_status_checks:
    - context: ci-required
  strict_required_status_checks_policy: true
```

and retains `pull_request`, `deletion`, `non_fast_forward`. No `merge_queue`
rule (decision A).

`ci-required` asserts, for every job in `needs`:

- `success` → pass
- `skipped` → pass (path-skipped lane; see D2)
- anything else (`failure`, `cancelled`, `abandoned`) → fail
- empty result set → fail

### Hardening to add (workflow change, small)

One real gap remains. `ci-required`'s `needs` list is hand-maintained: adding
a lane and forgetting to list it means that lane never gates. The inline
comment names this (memql#3019) and notes the existing static guard lives in
`go-checks` — which is itself an aggregated lane, so it cannot catch its own
absence.

Add a test asserting that every job in `ci.yml` except `changes` and
`ci-required` appears in `ci-required.needs`. It parses one YAML file, runs
in milliseconds, and belongs in the `gates` bucket so it runs on workflow
edits.

### WARNING: consequence of `strict` you should weigh

`strict_required_status_checks_policy: true` ("branch must be up to date")
means **every merge to `main` invalidates every open PR**, forcing a full
re-run. At 40 job-min per PR revision, the cost is
`open_PRs × 40 job-min` per merge. With an agent loop holding several PRs
open concurrently this compounds quickly.

This is the tradeoff you accepted when choosing gate-plus-up-to-date over the
merge queue, and it is still the right call — but it is the one setting most
likely to need revisiting under load. Flagged in Open Items.

---

## D2. Path filtering: where the filter is allowed to live

### The distinction that matters

The audit's never-reported failure mode and path filtering are **not the same
risk**, and conflating them leads to the wrong design. The rule:

- **Trigger-level path filters are dangerous.** If `on.pull_request.paths`
  excludes a PR, the workflow does not run, no job reports, and a required
  check sits "expected" forever. This is genuinely the starvation shape.
- **Job-level `if:` conditions are safe** — *provided the required check is
  the aggregator, not the lane*. A skipped job reports `skipped`, the gate
  treats it as pass, and nothing hangs.

The audit's incident was **not** caused by path filters. It was caused by
requiring three lane names that the platform failed to produce. Job-level
filtering was already working correctly.

### Alternatives considered

| Option | Tradeoff |
|---|---|
| **Job-level `if:` from a `changes` job + aggregating gate** (status quo shape) | A skipped lane costs zero runner slots — material during scarcity, where the audit measured peak 14 concurrent against a 60 limit and 45% of jobs starved. `skipped` is already a first-class pass in the gate. Cost: lane results are `skipped` rather than green, which reads as less explicit in the PR UI. |
| Always-run jobs with conditional *steps* | Every lane reports an explicit green. Cost: a runner is allocated per lane regardless — roughly 15–30s of setup each, times 14 lanes, on every PR including doc-only ones. Directly worsens the resource contention that caused the incident. |
| Trigger-level `paths:` on the workflow | Cheapest possible. **Rejected outright** — it is the one configuration that can make a required check never report. `deploy-gate-image.yml` uses this today; it is safe only because nothing requires its checks. |

### RECOMMENDATION

**Job-level `if:` conditions, driven by one `changes` job, with the
aggregating gate as the only required check.** Keep the current shape.

Two invariants to encode as rules, not conventions:

1. **No workflow that produces a required check may have a trigger-level
   `paths:` filter.** Today that means `ci.yml`. Worth asserting in the same
   test that checks the `needs` list.
2. **A lane's filter must include the closure of its dependencies.** If
   `engine` changes, the server lanes must run — a change below the cut has
   to trigger everything above it. This is the property that makes scoped CI
   correct rather than merely fast, and D3 specifies it.

---

## D3. Module topology and the filter buckets that map to it

### Measured layering

From `go list -json ./...` over 182 packages, aggregated to area level:

- The area graph contains one 6-area strongly-connected component:
  `component/{database,harness,actions,language,memql}` + `dsl`.
- **It is not a real package cycle.** It is an artifact of directory
  aggregation. `component/language/parser` imports
  `component/memql/baseparser`, while `component/memql` imports
  `component/language/parser`. Legal for Go; fatal for a module boundary
  drawn at `component/memql/`.
- Five L0 packages (zero internal dependencies) are misfiled inside the
  engine directory: `baseparser`, `baseregistry`, `dslfs`, `literalparity`,
  `liveknowledge`.
- `component/grpc/gen` is L0 with **21 importers** — the most widely shared
  artifact in the tree.
- `component/mcp` is imported by **nothing**. `component/grpc` (excluding
  `gen`) is imported only by `app`. `component/server` by `app` and
  `component/grpc`.
- `component/node` is imported by `app`, `component/grpc`, and three
  integrations. **It is not a protocol server** — it is the mesh substrate,
  and belongs below the servers.
- `component/memql/ai_providers.go` imports `integrations/audio`, an
  848-line mp3/wav/resample codec. This is a misfiled utility, not an
  integration.

### Target modules

| Module | Contents | Version |
|---|---|---|
| `wire` | `component/grpc/gen`, `node/gen`, `bus/gen` | **Independent** |
| `base` | `core/*`, the 5 extracted L0 leaves, `language/{annotations,ast,dslclause}`, `core/audio` (from `integrations/audio`) | Lockstep |
| `engine` | `component/memql`, `language`, `database`, `actions`, `harness`, `dsl` | **Independent** |
| `platform` | `component/node`, `polyphon`, `identity`, `auth`, `events`, `bus`, `safety` | Lockstep |
| `integrations` | `integrations/*` | Lockstep |
| `server-grpc` / `server-http` / `server-mcp` | `component/grpc`, `component/server`, `component/mcp` | Lockstep |
| `app` | `app/`, `main.go`, `cmd/*` | Not published |

Dependency direction is strictly downward:
`app → servers → integrations → platform → engine → base → wire`.

### Prerequisites (decision E — one PR, before any `go.mod` appears)

1. Move `baseparser`, `baseregistry`, `dslfs`, `literalparity`,
   `liveknowledge` out of `component/memql/` into the `base` location.
2. Move `integrations/audio` to `core/audio`; fix `ai_providers.go`.
3. Verify the area graph is a DAG afterwards (re-run the analysis in the
   appendix). ESTIMATE: these two moves alone resolve the SCC.

Until step 3 passes, a module boundary at the engine creates a genuine module
cycle. Nothing else in the split can proceed.

### Filter buckets

Written against **current** directory paths (directory reorganization is
undecided — see Open Items). Each bucket names the lanes it triggers,
including the downstream closure required by D2 invariant 2.

| Bucket | Paths | Triggers |
|---|---|---|
| `ci` | `.github/workflows/**`, `Makefile` | everything |
| `wire` | `**/*.proto`, `component/*/gen/**` | everything |
| `base` | `core/**`, `component/language/{annotations,ast,dslclause}/**`, the 5 extracted leaves | everything above base |
| `engine` | `component/{memql,language,database,actions,harness}/**`, `dsl/**` | engine, platform, integrations, all servers, app |
| `platform` | `component/{node,polyphon,identity,auth,events,bus,safety}/**` | platform, integrations, all servers, app |
| `integrations` | `integrations/**` | integrations, app |
| `server-grpc` | `component/grpc/**` (excl. `gen`) | server-grpc, app |
| `server-http` | `component/server/**` | server-http, app |
| `server-mcp` | `component/mcp/**` | server-mcp, app |
| `app` | `app/**`, `cmd/**`, `main.go` | app |
| `gates` | `**/*.md`, `**/*.memql`, `scripts/**`, the two manifests, `component/architecture/embedded/**` | gate-input lane (unchanged) |
| `sdkts` / `vscode` | unchanged | unchanged |

INFO: today's `go` bucket is `**/*.go`, so any Go change runs every lane.
That is safe but maximally coarse — it is why scoping currently buys nothing.
The buckets above are the first change that makes CI time a function of what
changed.

**When modules land, these filters do not change.** A `go.mod` appearing in
`component/memql/` does not move any path. This is what decision F buys.

---

## D4. Infrastructure resilience

### What actually happened

All 15 non-green runs were infrastructure, in **three** distinct shapes:

- **Starvation** — job queued, never assigned a runner, cancelled at exactly
  15m00s ± 4s. 27 of 60 jobs (45%) in the 16:00–19:00Z window. Confirmed not
  concurrency (peak 14 of 60), not billing (public repo), not supersession.
  Org policy is now also ruled out: `allowed_actions: all`, no runner groups.
- **`abandoned`** — runner assigned, then
  `Failed to resolve action download info. Error: Service Unavailable`
  after two retries, before any step executed.
- **Dispatch failure** — no run created at all. Every
  `.github/workflows/*` workflow stopped dispatching at 2026-08-06T16:29:16Z
  while GitHub-managed `dynamic` workflows continued. See
  [ci-audit.md §4.5](ci-audit.md).

None is retryable *inside* a job — all three happen before the first step,
and the third happens before a job exists. This constrains what a resilience
policy can achieve, and it is worth being honest about the ceiling: **no
in-repo mechanism mitigates dispatch failure.** Timeouts, retries and
job-level policy all presuppose that a run was created.

WARNING: dispatch failure is also the one mode that defeats D1. A single
required check reduces the never-reported surface from three workflows to
one, but if no workflow dispatches, one required name is as unsatisfiable as
three. This was demonstrated live: the gate was restored at 21:32Z and
control PR #3154 sat BLOCKED with no `ci-required` check in existence. The
mitigation is organisational, not technical — the bypass actor in O5, which
is why that item is not optional.

### Alternatives considered

| Option | Tradeoff |
|---|---|
| **Timeouts + fewer action resolutions + step-level retry on network ops + documented re-run** | Bounds the damage and cuts the `abandoned` surface. Does not prevent starvation — nothing in-repo can. Low complexity. |
| Auto-rerun bot (`workflow_run` on failure → inspect → re-dispatch) | Recovers unattended. Cost: a workflow that can re-trigger workflows is a loop risk, needs its own kill switch, and can mask genuine failures as "it passed on retry 3." Not worth it for an incident-frequency event. |
| Self-hosted runners | Eliminates the shared-pool starvation entirely. Real cost: you now operate runners, patch them, and secure them — on a **public** repo, where untrusted fork PRs would execute on your infrastructure. Serious security consideration. |

### RECOMMENDATION

1. **`timeout-minutes` on every job.** Currently exactly one exists repo-wide
   (`publish-docs-bundle.yml:49`). Every CI job inherits the 360-minute
   default. Proposed: 10 for short lanes, 20 for `go-checks` / `db-tests` /
   `conformance`. This bounds a hang; it does not affect starvation, since a
   job with no runner is not yet timing against its own budget.
2. **Reduce action resolutions per job.** Every `uses:` is a call to the
   service that failed. `go-checks` resolves 2; the `abandoned` failures hit
   jobs during `Prepare all required actions`. Replacing the
   `apt-get install protobuf-compiler` step with a pinned, cached protoc
   removes a network dependency from the critical path (also D5).
3. **Step-level retry on network operations only** — `apt-get`, `protoc`
   download, `make sdk-ts-install`. Not on test steps: retrying a test is how
   a flake becomes invisible.
4. **Do not classify infra as pass.** The gate must keep treating `cancelled`
   and `abandoned` as failure. The temptation during an incident is to make
   them non-blocking; that converts a visible outage into a silent one.
5. **A written break-glass procedure** — see Open Items. The incident showed
   the failure mode is not the outage, it is that the workaround outlived it.

NOTE: `fail-fast: false` is **already** set on the `build-node-tags` matrix,
so sibling cancellation is not a contributor. No change needed.

---

## D5. Waste

Current critical path: `changes` (0.1m) → `go-checks` (6.3m) → `ci-required`
(0.1m) = **6.5m**. Cost: **40 job-min per PR revision**.

### D5.1 Duplicate CodeQL — decided (B)

Retire `codeql.yml`; keep GitHub Code Quality. Saves **5.3 job-min/PR**.
Both currently publish an `Analyze (go)` check, visible twice in the check
list for PRs #3121, #3130 and #3136. `dynamic` is already the single
highest-volume run type in the sample (42 of 200 runs) because Code Quality
re-runs on every push.

Removing `codeql.yml` also deletes one of the four workflows that can fail to
report — directly reducing the D1 exposure surface.

### D5.2 The six node-tag builds

They are **not** on the critical path (1.9m median vs `go-checks` 6.3m) and
`fail-fast: false` is already correct. The cost is 6 of 14 runner slots and
~11.4 job-min, which matters only under contention — which is exactly when
the incident happened.

| Option | Tradeoff |
|---|---|
| **Scope by path filter** | Most PRs touch no tag-specific code, so most skip. Keeps parallelism and wall-clock at 1.9m. Requires the D3 buckets to be accurate about which paths affect which tag. |
| Collapse to one job looping 6 tags | 1 slot instead of 6. But ~11m serial makes it the **new critical path**, worse than the 6.5m it was meant to help. Rejected. |
| Fold into `go-checks` | Same failure, plus it lengthens the job already on the critical path. Rejected. |

**RECOMMENDATION:** scope by path filter, keep the matrix.

### D5.3 `go test ./...` — 223s, 57% of time-to-green

Three independent problems, largest first.

**(a) `-count=1` disables the Go test cache.** It appears 10 times in
`ci.yml`. `actions/setup-go` with `cache: true` already restores `GOCACHE`,
which is where Go stores test results — so the cache is being populated and
then explicitly ignored. Dropping `-count=1` makes every unchanged package a
cache hit.

ESTIMATE: on a PR touching a handful of packages this is the difference
between re-running 182 packages and re-running the affected few. It is the
single largest lever available, and it is Tier 2 of the repo's own
CI-acceleration plan, already half-implemented.

WARNING: this is a test-integrity tradeoff and I am not deciding it for you.
`-count=1` guarantees every test runs every time. Removing it means a test
whose behaviour depends on unstated environment can be cached green. If
`-count=1` was added deliberately after such an incident, that context should
win. See Open Items.

**(b) The heaviest packages run twice per PR.** `go-checks` runs
`go test ./...` — all 182 packages. `db-tests` then re-runs:

```
./component/memql/...      (342 test files)
./component/automations/... (100)
./component/grpc/...        (42)
./integrations/cognition/... (14)
./integrations/planner/...   (45)
./examples/referencepack/...
```

That is 543+ test files compiled and executed in both jobs. In `go-checks`
the DB-gated cases skip (no `MEMQL_DATABASE_DSN`, no `MEMQL_REQUIRE_DB`), but
the packages still compile and their non-DB tests still run.

**RECOMMENDATION:** make the split explicit and disjoint — `go-checks`
excludes the db-gated package set, `db-tests` owns it entirely. Define the
list once in a script both jobs source, so it cannot drift.

**(c) `go-checks` is a 15-step serial job mixing heavy and trivial work.**
Measured: `go test` 223s, `go build` 70s, `go vet` 26s, `install protoc` 14s,
and nine drift/lint checks totalling ~25s.

| Option | Tradeoff |
|---|---|
| **Split into `test` and `build-vet-drift` lanes** | Critical path becomes `max(test, build+vet+drift)` instead of their sum. ESTIMATE 6.3m → ~3.5–4m before (a) and (b). Cost: one more runner slot. |
| Shard `go test` across N runners | Largest wall-clock win; ESTIMATE ~2m at N=3. Cost: N slots during contention, plus a sharding mechanism to maintain. Worth revisiting after (a) and (b), which may make it unnecessary. |
| Leave serial | Zero risk, zero gain. |

**RECOMMENDATION:** split into two lanes. Defer sharding — (a) and (b) may
remove the need, and adding slots is the wrong direction while starvation is
a live risk.

### Projected effect

ESTIMATE, requiring measurement after implementation:

| | Now | After |
|---|---|---|
| Critical path | 6.5m | ~3.5m (D5.3c) — lower with (a) |
| Job-min per PR revision | 40 | ~28 (D5.1) — lower with (b) and D5.2 scoping |
| Workflows that can fail to report | 4 | 3 |
| Required check names | 3 (removed) | 1 |

---

## Sequencing

Per decision F, each phase is independently landable and independently
valuable.

**Phase 0 — restore the gate (hours, no workflow change). DONE
2026-08-06T21:32Z.**
`required_status_checks` with the single context `ci-required` added to
ruleset 16630577; the three pre-existing rules preserved unchanged.
`strict` was **not** enabled pending O2. A repo-admin bypass actor
(`bypass_mode: pull_request`) was added at 21:47Z under O5.
End-to-end verification is **incomplete** — the control PR could not be
verified because dispatch is down (D4, third shape). PR #3154 is left open as
the canary: when dispatch resumes, `ci-required` will report on it and close
out the verification.

**Phase 1 — cheap waste (one PR).**
Retire `codeql.yml`. Add `timeout-minutes`. Add the `needs`-completeness and
no-trigger-path-filter tests.

**Phase 2 — test economics (one PR).**
Disjoint `go-checks` / `db-tests` package sets. Split `go-checks` into two
lanes. `-count=1` handled per the Open Item answer.

**Phase 3 — filter buckets (one PR).**
Replace the `**/*.go` bucket with the D3 buckets including dependency
closure. CI is now scoped, and is already shaped for modules.

**Phase 4 — module prerequisites (one PR, decision E).**
Move the 5 L0 leaves and `integrations/audio`. Assert the area graph is a
DAG. No `go.mod` yet.

**Phase 5 — modules.**
`wire` first (L0, 21 importers, unblocks `sdk/go`), then `base`, `engine`,
and the rest. CI lanes do not move.

---

## Open items — your input needed

**O1. `-count=1`: was it deliberate?**
Dropping it is the single largest available speed-up (D5.3a), but it is a
test-integrity decision. If it was added after a caching incident or to
suppress a specific flake, that reason outranks the speed-up.
*Recommendation:* drop it for the `go-checks` lane only, keep it for
`db-tests` and `conformance` where tests touch shared external state, and
measure for two weeks. If you recall a specific reason it exists, say so and
I will design around it instead.

**O2. `strict` re-run cost.**
"Branch must be up to date" costs `open_PRs × 40 job-min` per merge to
`main` (D1). *Recommendation:* start with `strict` on — a broken `main` is
worse than re-runs — and reduce the constant first via Phases 1–3. If
re-run storms become the bottleneck, the next step is the merge queue, not
dropping `strict`.

**O3. Directory reorganization.**
You left this undecided. The D3 buckets are written against current paths.
*Recommendation:* keep current paths. A move to `engine/ servers/
integrations/` would touch every path filter, every `//go:embed`, CODEOWNERS,
and the architecture model — for naming clarity only, since module boundaries
give you the enforcement either way. If you do want the move, it must land
**before** Phase 3, not after.

**O4. `component/identity` placement.**
Filed under `platform` above, but it is 15 packages, ships its own binary,
serves a web UI and an admin app, and is imported by 7 areas. It has a
plausible claim to being its own module — possibly an independently versioned
one, since it is the piece most likely to be consumed standalone.
*Recommendation:* own module, lockstep version initially; promote to
independent only if something outside this repo consumes it. Flagging because
it is the one placement in D3 I am genuinely unsure of.

**O5. Break-glass procedure. PARTLY RESOLVED 2026-08-06T21:47Z.**
The incident's real lesson is not that the gate was lifted — that was correct
— but that the workaround outlived the outage by design (nothing forced its
restoration). *Recommendation:* a named bypass actor on the ruleset rather
than editing rules under pressure. Editing rules loses the original config
(which is why the three check names are unrecoverable); a bypass leaves the
rules intact and auditable.

Applied, and immediately load-bearing given the ongoing dispatch failure:

```json
{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "pull_request"}
```

Repository admins can merge a PR whose checks cannot report; direct pushes to
`main` remain blocked by the `pull_request` rule. `current_user_can_bypass`
moved from `never` to `pull_requests_only`.

Still open: whether "repository admin" is the right holder, or whether it
should be a named individual or team; and whether the bypass should be
removed once dispatch recovers or kept as the standing break-glass. My
recommendation is to keep it — a standing, auditable bypass is what stops the
next outage from being resolved by deleting rules again.

**O6. CODEOWNERS is self-review.**
`*  @znas-io`, and the same account authors the PRs, so
`require_code_owner_review` is satisfied by the author. This is governance
rather than CI, and out of scope for this document, but it means the gate
restored in Phase 0 is the *only* real gate. Noted so it is a choice rather
than an oversight.

---

## Appendix: reproducing the dependency analysis

```bash
go list -json ./... > pkgs.json
# aggregate ImportPath -> Imports to area level (component/X, integrations/X,
# core/X), treating component/*/gen as a distinct 'wire' area; then run
# Tarjan SCC over the area graph.
```

Findings this produced, all reproducible:

- 182 packages across 113 internal areas.
- One 6-area SCC (the engine tangle), resolved by the Phase 4 moves.
- `component/grpc/gen`: L0, 0 internal deps, 21 importers.
- `component/mcp`: 0 importers. `component/grpc` (excl. `gen`): `app` only.
- `component/node`: `app`, `component/grpc`, and 3 integrations — mesh
  substrate, not a protocol server.
- `component/memql` imports `integrations/audio` (1 file) and `docs` (the
  embedded guide).
- Test-file concentration: `component/memql` 342, `component/automations`
  100, `component/language` 99, of 1192 total.

Audit-side evidence for every timing and failure claim: [ci-audit.md](ci-audit.md).
