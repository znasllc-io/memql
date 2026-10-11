# Pipelines local green (epic A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The engine repository's own `memql-package.yaml` passes every stage on a local MemQL cluster on the owner's amd64 workstation, proven on the epic branch before one merge and again on main after it.

**Architecture:** `make up` gains a dedicated build node (a k3d agent labelled and tainted `memql.io/pipeline-pool=builds`, capped at 16 CPUs and 40 GB); the local overlay pins pipeline steps to it, allows six at once and composes the 8-hour run profile. The package moves to `linux/amd64` on an amd64 toolchain pinned by digest. `pipelinesRunLatest` gains an optional `branch`, surfaced in MemQL OS as **Other branch**, so the epic branch is run and fixed until three consecutive full runs pass, then everything ships as one PR.

**Tech Stack:** Go 1.27.2, Bash (capability scripts), kustomize over k3d 5.9 / k3s v1.32.13, Argo CD v2.13.3, MemQL DSL, React and TypeScript (MemQL OS, vitest), the `gh` CLI, GitHub Actions only for the one toolchain image build.

**Spec:** `docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md` (epic A is its phases 0 to 2; Task 1 amends it for the single-PR delivery the owner asked for on 2026-10-11).

## Global Constraints

- Platform `linux/amd64`; the toolchain image is `ghcr.io/znasllc-io/memql-toolchain@sha256:<64 hex>`, never a tag.
- The build node is `k3d-<cluster>-build-0`, labelled `memql.io/pipeline-pool=builds` and tainted `memql.io/pipeline-pool=builds:NoSchedule`; on this workstation `BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g`; defaults are two thirds of the Docker host, at least 2 CPUs and 4g.
- Local overlay: `count/jobs.batch: "6"`; LimitRange unchanged (limits 2 CPU / 4Gi / 20Gi disk, requests 250m / 512Mi / 1Gi); `MEMQL_PIPELINES_NODE_POOL: builds`; `components/pipelines-long-runs` composed (480 minutes on agent and workbench). The cloud overlays change nothing.
- No engine code branches on the deploy target (`TestNoEnvironmentBranchingInEngineCode`).
- Shell: `#!/usr/bin/env bash`, `set -euo pipefail`, function-based, `main` last; capability contract (params via `cap_spec_param` and `cap_param`, one JSON envelope on stdout, logs on stderr, exit 0 / 2 / 3 / 4 / 5). Never end a function on `[[ ... ]] && cmd` or `(( ... )) && cmd`; use `if`.
- `git add <explicit paths>` only; never `git add -A` or `git add .`. Never run prettier; the repository is hand-formatted.
- Verify Go with `make test` or module paths (`go test github.com/znasllc-io/memql/...`), never a bare `go test ./...`; db-gated packages need `MEMQL_REQUIRE_DB=1` and a real Postgres.
- Docs: no real hostnames (use `example.com` or `<domain>`), never call MemQL a database, no emojis.
- UI: load `.agents/skills/memql-ui-design/SKILL.md` and the `frontend-design:frontend-design` skill, read `clients/os/DESIGN.md` and `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`; clean and minimal without losing function; one primary button per action bar, everything else a text act; no unseen-update dots.
- One PR for the whole epic. Every closing keyword on its own line (`Closes #a.` then `Closes #b.`).
- Commit trailer, on every commit:

  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf
  ```

## Review Focus

1. **An existing dev cluster.** `make up` on a cluster created before this change must add the build node without recreating the cluster, and a pipelines cache already bound to another node must be called out with its repair instead of leaving steps unschedulable. Pinned in Task 3 (existing-node and bound-cache tests).
2. **A smaller Docker host.** On a developer's Mac the defaults must never exceed the host or fall below 2 CPUs / 4g, and explicit values larger than the host must be refused before anything is created. Pinned in Task 3 (defaults and refusal tests).
3. **Branch names a person types.** Slashes, dots, dashes and underscores pass; `..`, spaces, `~ ^ : ? * [ \`, a leading `-` or `/`, a trailing `/` or `.`, `.lock`, `@{` and `refs/...` are refused before any GitHub call. Pinned in Task 6 (leaf table and no-GitHub-call test).
4. **One commit through two branches.** The same SHA reached through the default branch and a named branch is one attempt and one check run. Pinned in Task 6.
5. **The branch field.** Blank or whitespace-only input sends nothing, Enter runs, Cancel and Escape send nothing, and a refusal keeps the typed name. Pinned in Task 7.

## File Structure

| File | Responsibility |
|---|---|
| `scripts/ci/prtiming/timing.go` (new) | Pure measurement: Go-touching runs, both clocks, quantiles, plan-log parsing, the markdown report |
| `scripts/ci/prtiming/main.go` (new) | The CLI: reads runs, jobs and plan logs through `gh api` |
| `scripts/ci/prtiming/timing_test.go` (new) | Unit tests for the pure half |
| `docs/internal/ops/ci-design.md` | D6 gains the M0 measurement and T |
| `scripts/ci/shard-timings.tsv`, `scripts/ci/workflow_timeout_test.go`, `.github/workflows/ci.yml` | #5697's refresh: timings, the sharded db-tests median, its timeout |
| `scripts/k3d/up.sh` | Build-node params, sizing, `resolve_build_node`, `ensure_build_node`, cache warning |
| `scripts/k3d/bringup.sh`, `Makefile` | Forward the three build-node params |
| `scripts/k3d/up_build_node_test.go` (new) | Stubbed-driver tests for the build node |
| `deploy/k8s/overlays/local/pipelines-values.yaml`, `deploy/k8s/overlays/local/kustomization.yaml` | Six Jobs, the builds pool, the long-run profile |
| `deploy/k8s/overlays/render_pipelines_test.go`, `deploy/k8s/overlays/render_pipeline_long_runs_test.go`, `deploy/k8s/components/examples/pipelines-long-runs/kustomization.yaml` | The tests and example that follow the local values |
| `memql-package.yaml`, `memql_package_manifest_test.go` | amd64 and the new toolchain digest |
| `component/pipelines/branch.go`, `component/pipelines/branch_test.go` (new) | `ValidBranchName` |
| `component/pipelines/refusal.go`, `component/packages/refusal.go` | `pipeline_branch_invalid` |
| `component/pipelinerun/manual.go`, `component/pipelinerun/manual_test.go` | `RunLatest(ctx, pipelineID, branch)` |
| `dsl/pipelines/builtins.memql` | `pipelinesRunLatest` takes `branch` |
| `sdk/go/client/generated_builtins.go`, `sdk/ts/src/client/generated_builtins.ts` | Regenerated |
| `clients/os/src/apps/deployables/pipelines/calls.ts`, `PipelinePage.tsx`, `BranchChoice.tsx` (new) | **Other branch** |
| `clients/os/src/apps/deployables/packages/refusals.ts` | Copy for `pipeline_branch_invalid` |
| `clients/os/test/deployables/pipelines/pipelinePage.test.tsx` | Tests for **Other branch** |
| Docs named in Tasks 3, 5, 10 | Kept true |

---

### Task 1: The epic, its issues, the branch and the spec amendments

**Files:**
- Modify: `docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md`
- Create (ccpm, ignored by git): `.claude/epics/pipelines-local-green/epic.md` and one task file per issue

**Interfaces:**
- Produces: the epic issue number `E` and task issue numbers used in every commit message and in Task 10's `Closes` lines; the branch `epic/pipelines-local-green` on origin.

- [ ] **Step 1: Rename the branch.** The worktree already holds the spec commit on `docs/pipelines-local-green-and-fast`.

  ```bash
  WT=/home/znas/memql-projects/memql-pipelines-green
  git -C "$WT" branch -m docs/pipelines-local-green-and-fast epic/pipelines-local-green
  git -C "$WT" branch --unset-upstream
  ```

- [ ] **Step 2: Amend the spec for one PR and for branch runs.** Make these edits in the spec, keeping its prose style:
  - D3: replace its paragraph with: "Phase 2 starts every run from **Run latest** on a disconnected pipeline. A manual run may name a branch of the same repository (section 8, engine change 4), so the epic's own branch is run and fixed before it merges. A manual run writes its check run on the commit it tests, so during phase 2 `MemQL / memql` appears only on the epic's own commits. Once green, the pipeline reconnects with `poll` delivery and reports a non-required `MemQL / memql` check on every pull request and main push. No ruleset change is made in this phase."
  - Section 3, phase 2 row, "Done when": "Three consecutive full runs pass every stage, on three different commits of the epic branch, with no engine rollout during any of them; after the merge, one more on main's head."
  - Section 4, "What merges before the first run": replace with "**Nothing merges before the first run.** A manual run names the epic branch, whose head carries the amd64 manifest and the overlay values, and `make up` points Argo CD at the current branch, so the cluster runs the epic's own overlay."
  - Section 8: add "4. **A manual run names a branch.** `pipelinesRunLatest` takes an optional `branch`; empty keeps the default branch. A name GitHub would not accept is refused `pipeline_branch_invalid` before any request leaves the cluster. MemQL OS offers it as **Other branch** beside **Run latest**, listing the repository's branches through the source's own connection."
  - Section 10: replace the epic A and epic B bullets' PR lines with: "The owner asked on 2026-10-11 for one PR per epic covering all its issues, tracked with ccpm. Epic A ships as one PR after the amd64 toolchain dispatch. Epic B ships as one PR too, so its Tailwind change takes #5816's second proposal (the tools directory from the environment, pointed at a declared cache) rather than a toolchain rebuild, which would need a merge before its digest exists." Change section 7 lever 3's Tailwind sentence to match: "the Tailwind CLI comes from a declared cache directory (#5816)".
  - Section 9: add "**Branch runs:** leaf tests for `ValidBranchName`, `RunLatest` tests for a named branch, a malformed name refused before GitHub, and one commit through two branches being one attempt; OS tests for **Other branch**."

- [ ] **Step 3: Commit the amendment.**

  ```bash
  git -C "$WT" add docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md docs/superpowers/plans/2026-10-11-pipelines-local-green.md
  git -C "$WT" commit -m "docs(pipelines): one PR per epic, and manual runs that name a branch" -m "Amends the local green-and-fast design record for the owner's 2026-10-11 instruction (one PR covering all of an epic's issues) and adds the branch argument that lets the epic branch be proven before it merges. Adds epic A's implementation plan." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf"
  git -C "$WT" push -u origin epic/pipelines-local-green
  ```

- [ ] **Step 4: Create and sync the epic with ccpm.** Invoke the `ccpm` skill and follow its `references/structure.md` and `references/sync.md`, with three overrides from this repository:
  - strip frontmatter with `awk 'BEGIN{c=0} c<2 && /^---$/ {c++; next} c>=2 {print}'`, never the skill's `sed '1,/^---$/d; 1,/^---$/d'`, which empties every body under GNU sed;
  - labels: the epic carries `epic`, `epic:pipelines-local-green`, `feature`, `claude`, `area/build`, `area/deploy`; each task carries `task`, `epic:pipelines-local-green`, `claude` and its area (`area/build`, `area/deploy`, `area/engine`, `area/os`); create `epic:pipelines-local-green` first with `gh label create epic:pipelines-local-green --repo znasllc-io/memql --color 1d76db`;
  - task bodies open "Part of #E." and say "Ships in the epic's single PR."; the epic body ends "All issues `claude`-labeled."

  The epic links the spec and this plan. Its tasks, one issue each:
  1. "make up creates the dedicated pipeline build node" (Task 3, `area/deploy`)
  2. "Local overlay: six steps at once, the builds pool, the 8-hour run profile" (Task 4, `area/deploy`)
  3. "The engine's package targets linux/amd64 on a pinned amd64 toolchain" (Task 5, `area/build`)
  4. "A manual run can name a branch (pipelinesRunLatest branch)" (Task 6, `area/engine`)
  5. "Run another branch from the pipeline page in MemQL OS" (Task 7, `area/os`)
  6. "Bring-up: the local cluster runs the engine's own pipeline" (Task 8, `area/deploy`)
  7. "Green: three consecutive full runs pass every stage" (Task 9, `area/build`)

  #5697 is Task 2's issue: do not file a duplicate. Add the epic label to it and comment "Part of #E; measured in that epic's single PR."

- [ ] **Step 5: Verify every body arrived.**

  ```bash
  for n in E T1 T2 T3 T4 T5 T6 T7; do gh api repos/znasllc-io/memql/issues/$n --jq '"\(.number) \(.body|length) \(.title)"'; done
  ```

  Expected: every length above 200. A length of 0 means the frontmatter strip ate the body: rewrite it with `gh issue edit <n> --body-file <file>`.

- [ ] **Step 6: Link the epic from #5480.**

  ```bash
  gh issue comment 5480 --repo znasllc-io/memql --body "Epic A of the next phase is #E (design record docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md). It ships as one PR."
  ```

---

### Task 2: The M0 baseline (#5697)

**Files:**
- Create: `scripts/ci/prtiming/timing.go`, `scripts/ci/prtiming/main.go`, `scripts/ci/prtiming/timing_test.go`
- Modify: `docs/internal/ops/ci-design.md` (D6), `scripts/ci/shard-timings.tsv`, `scripts/ci/workflow_timeout_test.go`, `.github/workflows/ci.yml` (the db-tests job's `timeout-minutes`)

**Interfaces:**
- Produces: `go run ./scripts/ci/prtiming --since=YYYY-MM-DD --until=YYYY-MM-DD [--repo=owner/name]`, printing a markdown report. Epic B extends it to compare MemQL's check against `ci-required`. Exported for tests: `GoTouching([]Job) bool`, `Clocks(Run) (time.Duration, time.Duration, bool)`, `Quantile([]time.Duration, float64) time.Duration`, `ParsePlanLog(string) (string, int)`, `SlowestDBShard(Run) time.Duration`, `Report([]Run, string, string) string`.

- [ ] **Step 1: Write the failing tests.**

  ```go
  // scripts/ci/prtiming/timing_test.go
  package main

  import (
  	"strings"
  	"testing"
  	"time"
  )

  func at(min int) time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute) }

  func TestGoTouchingCountsAShardThatRan(t *testing.T) {
  	cases := []struct {
  		name string
  		jobs []Job
  		want bool
  	}{
  		{"a go shard passed", []Job{{Name: "go-tests (default-1)", Conclusion: "success"}}, true},
  		{"a db shard failed", []Job{{Name: "db-tests (memql-1)", Conclusion: "failure"}}, true},
  		{"shards skipped", []Job{{Name: "go-tests (default-1)", Conclusion: "skipped"}, {Name: "os-checks", Conclusion: "success"}}, false},
  		{"a tag pass is not a shard", []Job{{Name: "go-tests -tags agent", Conclusion: "success"}}, false},
  	}
  	for _, c := range cases {
  		if got := GoTouching(c.jobs); got != c.want {
  			t.Errorf("%s: GoTouching = %v, want %v", c.name, got, c.want)
  		}
  	}
  }

  func TestClocksEndAtCiRequired(t *testing.T) {
  	r := Run{CreatedAt: at(0), Jobs: []Job{
  		{Name: "plan", StartedAt: at(1), CompletedAt: at(2)},
  		{Name: "go-checks", StartedAt: at(2), CompletedAt: at(9)},
  		{Name: "ci-required", StartedAt: at(10), CompletedAt: at(11)},
  	}}
  	fromCreation, fromFirstJob, ok := Clocks(r)
  	if !ok || fromCreation != 11*time.Minute || fromFirstJob != 10*time.Minute {
  		t.Fatalf("Clocks = %v, %v, %v; want 11m, 10m, true", fromCreation, fromFirstJob, ok)
  	}
  	if _, _, ok := Clocks(Run{CreatedAt: at(0), Jobs: []Job{{Name: "plan", StartedAt: at(1), CompletedAt: at(2)}}}); ok {
  		t.Fatal("a run whose ci-required never completed has a clock")
  	}
  }

  func TestQuantileInterpolatesBetweenRanks(t *testing.T) {
  	var ds []time.Duration
  	for _, m := range []int{7, 1, 9, 3, 5, 2, 10, 4, 8, 6} {
  		ds = append(ds, time.Duration(m)*time.Minute)
  	}
  	for _, c := range []struct {
  		q    float64
  		want time.Duration
  	}{{0.5, 330 * time.Second}, {0.25, 195 * time.Second}, {0.75, 465 * time.Second}} {
  		if got := Quantile(ds, c.q); got != c.want {
  			t.Errorf("Quantile(%v) = %v, want %v", c.q, got, c.want)
  		}
  	}
  	if Quantile(nil, 0.5) != 0 || Quantile([]time.Duration{time.Minute}, 0.75) != time.Minute {
  		t.Error("empty or single-sample quantile is wrong")
  	}
  }

  func TestParsePlanLogReadsModeAndSelection(t *testing.T) {
  	affected := "2026-10-10T05:15:40.1234567Z ### CI plan: affected\n2026-10-10T05:15:40.2Z \n2026-10-10T05:15:40.3Z 3 seed package(s), 42 selected.\n"
  	if mode, selected := ParsePlanLog(affected); mode != "affected" || selected != 42 {
  		t.Errorf("affected log = %q, %d", mode, selected)
  	}
  	if mode, selected := ParsePlanLog("2026-10-10T05:15:40Z ### CI plan: full\n"); mode != "full" || selected != 0 {
  		t.Errorf("full log = %q, %d", mode, selected)
  	}
  	if mode, _ := ParsePlanLog("no plan here"); mode != "" {
  		t.Errorf("garbage log read as %q", mode)
  	}
  }

  func TestSlowestDBShardIsTheLongestDbShard(t *testing.T) {
  	r := Run{Jobs: []Job{
  		{Name: "db-tests (memql-1)", Conclusion: "success", StartedAt: at(0), CompletedAt: at(7)},
  		{Name: "db-tests (work)", Conclusion: "success", StartedAt: at(0), CompletedAt: at(3)},
  		{Name: "go-tests (default-1)", Conclusion: "success", StartedAt: at(0), CompletedAt: at(9)},
  	}}
  	if got := SlowestDBShard(r); got != 7*time.Minute {
  		t.Fatalf("SlowestDBShard = %v, want 7m", got)
  	}
  }

  func TestReportCountsGoTouchingRunsAndPrintsT(t *testing.T) {
  	run := func(id int64, end int, mode string, goShard bool) Run {
  		jobs := []Job{{Name: "ci-required", Conclusion: "success", StartedAt: at(end - 1), CompletedAt: at(end)}}
  		if goShard {
  			jobs = append(jobs, Job{Name: "go-tests (default-1)", Conclusion: "success", StartedAt: at(1), CompletedAt: at(end - 2)})
  		}
  		return Run{ID: id, CreatedAt: at(0), Jobs: jobs, Mode: mode}
  	}
  	out := Report([]Run{run(1, 10, "affected", true), run(2, 12, "full", true), run(3, 5, "affected", false)}, "2026-10-04", "2026-10-10")
  	for _, want := range []string{"Runs measured: 3", "Go-touching: 2", "T (median, creation to ci-required): 11.0 min"} {
  		if !strings.Contains(out, want) {
  			t.Errorf("report lacks %q:\n%s", want, out)
  		}
  	}
  }
  ```

- [ ] **Step 2: Run them to see them fail.**

  Run: `go test -count=1 ./scripts/ci/prtiming/`
  Expected: FAIL, "undefined: GoTouching" (and the other names).

- [ ] **Step 3: Write the pure half.**

  ```go
  // scripts/ci/prtiming/timing.go

  // Command prtiming measures GitHub Actions pull-request runs: the M0 baseline
  // of the pipelines programme (memql#5697; design record
  // docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md,
  // section 7). Its clock runs from a run's creation, which is when GitHub saw
  // the push, to the completion of ci-required, the check the ruleset
  // requires; MemQL's own check is later timed on the same clock.
  package main

  import (
  	"fmt"
  	"math"
  	"regexp"
  	"sort"
  	"strconv"
  	"strings"
  	"time"
  )

  // Job is one job of a workflow run, as the Actions API reports it.
  type Job struct {
  	ID          int64
  	Name        string
  	Conclusion  string
  	StartedAt   time.Time
  	CompletedAt time.Time
  }

  // Run is one ci.yml run, the jobs it ran and what its plan job printed.
  type Run struct {
  	ID        int64
  	CreatedAt time.Time
  	Jobs      []Job
  	Mode      string // "affected", "full", or "" when the plan log could not be read
  	Selected  int    // packages an affected plan selected
  }

  const requiredJob = "ci-required"

  func ran(j Job) bool { return j.Conclusion == "success" || j.Conclusion == "failure" }

  // GoTouching reports whether a run tested Go: a go-tests or db-tests shard ran.
  func GoTouching(jobs []Job) bool {
  	for _, j := range jobs {
  		if (strings.HasPrefix(j.Name, "go-tests (") || strings.HasPrefix(j.Name, "db-tests (")) && ran(j) {
  			return true
  		}
  	}
  	return false
  }

  // Clocks returns a run's two wall clocks, both ending when ci-required
  // completed: from the run's creation (T's clock) and from its first job's
  // start (#5697's clock). ok is false when ci-required never completed.
  func Clocks(r Run) (fromCreation, fromFirstJob time.Duration, ok bool) {
  	var end, first time.Time
  	for _, j := range r.Jobs {
  		if j.Name == requiredJob && !j.CompletedAt.IsZero() {
  			end = j.CompletedAt
  		}
  		if !j.StartedAt.IsZero() && (first.IsZero() || j.StartedAt.Before(first)) {
  			first = j.StartedAt
  		}
  	}
  	if end.IsZero() || first.IsZero() || r.CreatedAt.IsZero() {
  		return 0, 0, false
  	}
  	return end.Sub(r.CreatedAt), end.Sub(first), true
  }

  // Quantile is the q-th quantile by linear interpolation between closest ranks
  // (R type 7, NumPy's default). The input need not be sorted; empty answers 0.
  func Quantile(ds []time.Duration, q float64) time.Duration {
  	if len(ds) == 0 {
  		return 0
  	}
  	s := append([]time.Duration(nil), ds...)
  	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
  	pos := q * float64(len(s)-1)
  	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
  	return s[lo] + time.Duration((pos-float64(lo))*float64(s[hi]-s[lo]))
  }

  var (
  	planModeLine     = regexp.MustCompile(`### CI plan: (affected|full)`)
  	planSelectedLine = regexp.MustCompile(`(\d+) seed package\(s\), (\d+) selected\.`)
  )

  // ParsePlanLog reads the plan job's mode and selected-package count from its
  // log (scripts/ci/selection Plan.Summary). An unreadable log answers "".
  func ParsePlanLog(log string) (mode string, selected int) {
  	if m := planModeLine.FindStringSubmatch(log); m != nil {
  		mode = m[1]
  	}
  	if m := planSelectedLine.FindStringSubmatch(log); m != nil {
  		selected, _ = strconv.Atoi(m[2])
  	}
  	return mode, selected
  }

  // SlowestDBShard is the longest db-tests shard of a run: what the lane's
  // timeout has to cover.
  func SlowestDBShard(r Run) time.Duration {
  	var slowest time.Duration
  	for _, j := range r.Jobs {
  		if strings.HasPrefix(j.Name, "db-tests (") && ran(j) && !j.StartedAt.IsZero() && !j.CompletedAt.IsZero() {
  			if d := j.CompletedAt.Sub(j.StartedAt); d > slowest {
  				slowest = d
  			}
  		}
  	}
  	return slowest
  }

  func minutes(d time.Duration) string { return fmt.Sprintf("%.1f min", d.Minutes()) }

  // Report renders the baseline for ci-design.md D6. Medians, never a best case.
  func Report(runs []Run, since, until string) string {
  	var b strings.Builder
  	var creation, firstJob, dbSlowest []time.Duration
  	byMode := map[string][]time.Duration{}
  	var selected []time.Duration
  	perJob := map[string][]time.Duration{}
  	touching := 0
  	for _, r := range runs {
  		if !GoTouching(r.Jobs) {
  			continue
  		}
  		c, f, ok := Clocks(r)
  		if !ok {
  			continue
  		}
  		touching++
  		creation = append(creation, c)
  		firstJob = append(firstJob, f)
  		byMode[r.Mode] = append(byMode[r.Mode], c)
  		if r.Mode == "affected" {
  			selected = append(selected, time.Duration(r.Selected))
  		}
  		if d := SlowestDBShard(r); d > 0 {
  			dbSlowest = append(dbSlowest, d)
  		}
  		for _, j := range r.Jobs {
  			if ran(j) && !j.StartedAt.IsZero() && !j.CompletedAt.IsZero() {
  				perJob[j.Name] = append(perJob[j.Name], j.CompletedAt.Sub(j.StartedAt))
  			}
  		}
  	}
  	fmt.Fprintf(&b, "Window: %s to %s, green pull-request ci.yml runs.\n\n", since, until)
  	fmt.Fprintf(&b, "Runs measured: %d\nGo-touching: %d\n\n", len(runs), touching)
  	fmt.Fprintf(&b, "T (median, creation to ci-required): %s; middle half %s to %s\n",
  		minutes(Quantile(creation, 0.5)), minutes(Quantile(creation, 0.25)), minutes(Quantile(creation, 0.75)))
  	fmt.Fprintf(&b, "First job to ci-required (#5697's clock): median %s; middle half %s to %s\n\n",
  		minutes(Quantile(firstJob, 0.5)), minutes(Quantile(firstJob, 0.25)), minutes(Quantile(firstJob, 0.75)))
  	fmt.Fprintf(&b, "| Plan | Runs | Median (creation clock) |\n|---|---:|---:|\n")
  	for _, mode := range []string{"affected", "full", ""} {
  		name := mode
  		if name == "" {
  			name = "unread"
  		}
  		if ds := byMode[mode]; len(ds) > 0 {
  			fmt.Fprintf(&b, "| %s | %d | %s |\n", name, len(ds), minutes(Quantile(ds, 0.5)))
  		}
  	}
  	if len(selected) > 0 {
  		fmt.Fprintf(&b, "\nAffected plans selected a median of %d packages.\n", int(Quantile(selected, 0.5)))
  	}
  	fmt.Fprintf(&b, "\nSlowest db-tests shard per run: median %s, maximum %s.\n\n", minutes(Quantile(dbSlowest, 0.5)), minutes(Quantile(dbSlowest, 1)))
  	names := make([]string, 0, len(perJob))
  	for name := range perJob {
  		names = append(names, name)
  	}
  	sort.Slice(names, func(i, j int) bool { return Quantile(perJob[names[i]], 0.5) > Quantile(perJob[names[j]], 0.5) })
  	fmt.Fprintf(&b, "| Job | Runs | Median | Middle half |\n|---|---:|---:|---|\n")
  	for _, name := range names {
  		ds := perJob[name]
  		fmt.Fprintf(&b, "| %s | %d | %s | %s to %s |\n", name, len(ds), minutes(Quantile(ds, 0.5)), minutes(Quantile(ds, 0.25)), minutes(Quantile(ds, 0.75)))
  	}
  	return b.String()
  }
  ```

- [ ] **Step 4: Run the tests.** Run: `go test -count=1 ./scripts/ci/prtiming/`. Expected: PASS.

- [ ] **Step 5: Write the CLI.**

  ```go
  // scripts/ci/prtiming/main.go
  package main

  import (
  	"bufio"
  	"flag"
  	"fmt"
  	"os"
  	"os/exec"
  	"strconv"
  	"strings"
  	"time"
  )

  func gh(args ...string) (string, error) {
  	out, err := exec.Command("gh", args...).Output()
  	if err != nil {
  		return "", fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
  	}
  	return string(out), nil
  }

  func parseTime(s string) time.Time {
  	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(s))
  	return t
  }

  func fetchRuns(repo, since, until string) ([]Run, error) {
  	out, err := gh("api", "--paginate",
  		fmt.Sprintf("repos/%s/actions/workflows/ci.yml/runs?event=pull_request&status=success&created=%s..%s&per_page=100", repo, since, until),
  		"--jq", `.workflow_runs[] | [.id, .created_at] | @tsv`)
  	if err != nil {
  		return nil, err
  	}
  	var runs []Run
  	sc := bufio.NewScanner(strings.NewReader(out))
  	for sc.Scan() {
  		f := strings.Split(sc.Text(), "\t")
  		if len(f) != 2 {
  			continue
  		}
  		id, _ := strconv.ParseInt(f[0], 10, 64)
  		r := Run{ID: id, CreatedAt: parseTime(f[1])}
  		jobs, err := gh("api", "--paginate", fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", repo, id),
  			"--jq", `.jobs[] | [.id, .name, (.conclusion // ""), (.started_at // ""), (.completed_at // "")] | @tsv`)
  		if err != nil {
  			return nil, err
  		}
  		js := bufio.NewScanner(strings.NewReader(jobs))
  		for js.Scan() {
  			g := strings.Split(js.Text(), "\t")
  			if len(g) != 5 {
  				continue
  			}
  			jid, _ := strconv.ParseInt(g[0], 10, 64)
  			r.Jobs = append(r.Jobs, Job{ID: jid, Name: g[1], Conclusion: g[2], StartedAt: parseTime(g[3]), CompletedAt: parseTime(g[4])})
  		}
  		for _, j := range r.Jobs {
  			if j.Name == "plan" {
  				if log, err := gh("api", fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repo, j.ID)); err == nil {
  					r.Mode, r.Selected = ParsePlanLog(log)
  				}
  			}
  		}
  		runs = append(runs, r)
  	}
  	return runs, sc.Err()
  }

  func main() {
  	repo := flag.String("repo", "znasllc-io/memql", "the repository")
  	since := flag.String("since", "", "first day, YYYY-MM-DD")
  	until := flag.String("until", "", "last day, YYYY-MM-DD")
  	flag.Parse()
  	if *since == "" || *until == "" {
  		fmt.Fprintln(os.Stderr, "usage: prtiming --since=YYYY-MM-DD --until=YYYY-MM-DD [--repo=owner/name]")
  		os.Exit(2)
  	}
  	runs, err := fetchRuns(*repo, *since, *until)
  	if err != nil {
  		fmt.Fprintln(os.Stderr, err)
  		os.Exit(1)
  	}
  	fmt.Print(Report(runs, *since, *until))
  }
  ```

  Check that the plan job's name in `.github/workflows/ci.yml` is literally `plan` (`grep -n "^  plan:" .github/workflows/ci.yml` and its `name:`); if its display name differs, match that name in the loop above.

- [ ] **Step 6: Measure.**

  ```bash
  cd "$WT"
  go run ./scripts/ci/prtiming --since=2026-10-04 --until=2026-10-10 | tee /tmp/claude-1000/-home-znas-memql-projects-memql/6dc3b75b-28d6-4b70-8c32-cacb99b10e64/scratchpad/m0-baseline.md
  gh api "repos/znasllc-io/memql/actions/caches?per_page=100&sort=last_accessed_at" --jq '.actions_caches[] | select(.key | startswith("go-v1-")) | [.key, .size_in_bytes, .ref, .created_at, .last_accessed_at] | @tsv'
  bash scripts/ci/refresh-shard-timings.sh --runs=5
  ```

  Expected: the report prints `T (median, creation to ci-required)`. The cache listing shows the `go-v1-*` keys, their size and ref.

- [ ] **Step 7: Record it.** In `docs/internal/ops/ci-design.md`, under "## D6. The CI bridge", add a subsection "### The M0 measurement (2026-10-11, memql#5697)". It holds the report verbatim, a sentence saying whether M0 (a Go-touching PR under 7 minutes) is met, and T as the target of the local green-and-fast design record. Add the Go-cache finding: how many `go-v1-*` keys, total size, whether main writes them daily and the shards restore them.

- [ ] **Step 8: Move the db-tests median and timeout.** In `scripts/ci/workflow_timeout_test.go` set `"db-tests"` in `timeoutMedianMinutes` to the report's "Slowest db-tests shard per run" median, rounded up to one decimal. Replace its comment with "Re-measured 2026-10-11 from the sharded lane: the median, over the M0 window's Go-touching runs, of each run's slowest db-tests shard (scripts/ci/prtiming)." In `.github/workflows/ci.yml`, set the db-tests job's `timeout-minutes` to the smallest multiple of 5 that is at least both 3 x that median and 1.5 x the reported maximum, and say so in a comment beside it.

- [ ] **Step 9: Run the gates the change touches.**

  Run: `go test -count=1 ./scripts/ci/ ./scripts/ci/prtiming/`
  Expected: PASS, including `workflow_timeout_test.go`, the path-bucket coverage gate and the toolchain workflow test.

- [ ] **Step 10: Commit.**

  ```bash
  git -C "$WT" add scripts/ci/prtiming/timing.go scripts/ci/prtiming/main.go scripts/ci/prtiming/timing_test.go docs/internal/ops/ci-design.md scripts/ci/shard-timings.tsv scripts/ci/workflow_timeout_test.go .github/workflows/ci.yml
  git -C "$WT" commit -m "ci: measure the M0 baseline and move the sharded db-tests budget (#5697)" -m "Part of #E." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf"
  ```

---

### Task 3: `make up` creates the dedicated build node

**Files:**
- Modify: `scripts/k3d/up.sh` (params, globals, a BUILD NODE section, `main` wiring)
- Modify: `scripts/k3d/bringup.sh`, `Makefile` (`up`, `up-refresh` and their comments)
- Create: `scripts/k3d/up_build_node_test.go`
- Modify: `deploy/k8s/components/pipelines/README.md` ("Dedicated build placement": `make up` provides the node locally), `docs/public/operate/reproduce-the-cloud-locally.md` (the build node and its sizes)

**Interfaces:**
- Produces: node `k3d-<cluster>-build-0` labelled and tainted `memql.io/pipeline-pool=builds`, CPU-capped by `docker update --cpus`, memory-capped by `k3d node create --memory`; result fields `buildNode` (bool), `buildNodeName`, `buildNodeCpus`, `buildNodeMemory`. Shell functions `docker_host_cpus`, `docker_host_memory_bytes`, `default_build_node_cpus <cpus>`, `default_build_node_memory <bytes>`, `resolve_build_node`, `warn_cache_bound_elsewhere <node>`, `ensure_build_node`.

- [ ] **Step 1: Write the failing tests.**

  ```go
  // scripts/k3d/up_build_node_test.go
  package k3d

  import (
  	"errors"
  	"os"
  	"os/exec"
  	"path/filepath"
  	"strings"
  	"testing"
  )

  // The dedicated pipeline build node (design record
  // docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md,
  // section 4). The drivers are stubs that log every call; up.sh is sourced, so
  // each function runs exactly as `make up` runs it.

  type buildNodeWorld struct {
  	listed  bool   // k3d node list already shows k3d-memql-build-0
  	label   string // what kubectl reports for the pool label
  	taint   string // what kubectl reports for the pool taint's effect
  	boundTo string // the node the pipelines cache's volume is bound to, "" for none
  	cpus    string // docker info NCPU
  	bytes   string // docker info MemTotal
  }

  func runUpFunctions(t *testing.T, w buildNodeWorld, body string) (string, string, error) {
  	t.Helper()
  	root := repoRoot(t)
  	dir := t.TempDir()
  	log := filepath.Join(dir, "calls.log")
  	stubs := map[string]string{
  		"k3d": "#!/usr/bin/env bash\necho \"k3d $*\" >> \"$STUB_LOG\"\nif [[ \"$1 $2\" == \"node list\" && \"$STUB_LISTED\" == true ]]; then echo 'k3d-memql-build-0   agent   memql   running'; fi\nexit 0\n",
  		"docker": "#!/usr/bin/env bash\necho \"docker $*\" >> \"$STUB_LOG\"\nif [[ \"$1\" == info && \"$3\" == '{{.NCPU}}' ]]; then echo \"$STUB_CPUS\"; fi\nif [[ \"$1\" == info && \"$3\" == '{{.MemTotal}}' ]]; then echo \"$STUB_BYTES\"; fi\nexit 0\n",
  		"kubectl": "#!/usr/bin/env bash\necho \"kubectl $*\" >> \"$STUB_LOG\"\ncase \"$*\" in\n  *'get node'*'pipeline-pool}'*) printf '%s' \"$STUB_LABEL\" ;;\n  *'get node'*taints*) printf '%s' \"$STUB_TAINT\" ;;\n  *'get pvc memql-pipelines-cache'*) if [[ -n \"$STUB_BOUND\" ]]; then printf pv-cache; fi ;;\n  *'get pv pv-cache'*) printf '%s' \"$STUB_BOUND\" ;;\nesac\nexit 0\n",
  	}
  	for name, body := range stubs {
  		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
  			t.Fatal(err)
  		}
  	}
  	cmd := exec.Command("bash", "-c", "set -euo pipefail\nsource \"$UP_SH\"\n"+body+"\n")
  	cmd.Dir = root
  	cmd.Env = append(os.Environ(),
  		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
  		"UP_SH="+filepath.Join(root, "scripts", "k3d", "up.sh"),
  		"STUB_LOG="+log,
  		"STUB_LISTED="+map[bool]string{true: "true", false: "false"}[w.listed],
  		"STUB_LABEL="+w.label, "STUB_TAINT="+w.taint, "STUB_BOUND="+w.boundTo,
  		"STUB_CPUS="+w.cpus, "STUB_BYTES="+w.bytes)
  	out, err := cmd.CombinedOutput()
  	calls, _ := os.ReadFile(log)
  	return string(out), string(calls), err
  }

  func exitCode(err error) int {
  	var exit *exec.ExitError
  	if errors.As(err, &exit) {
  		return exit.ExitCode()
  	}
  	return 0
  }

  func TestBuildNodeDefaultsAreTwoThirdsOfTheDockerHost(t *testing.T) {
  	for _, c := range []struct{ cpus, bytes, want string }{
  		{"24", "66793443328", "16 41g"},
  		{"1", "2147483648", "2 4g"},
  	} {
  		out, _, err := runUpFunctions(t, buildNodeWorld{cpus: c.cpus, bytes: c.bytes},
  			`echo "$(default_build_node_cpus "$(docker_host_cpus)") $(default_build_node_memory "$(docker_host_memory_bytes)")"`)
  		lines := strings.Split(strings.TrimSpace(out), "\n")
  		if err != nil || lines[len(lines)-1] != c.want {
  			t.Errorf("host %s CPUs / %s bytes: got %q (err %v), want %q", c.cpus, c.bytes, out, err, c.want)
  		}
  	}
  }

  func TestEnsureBuildNodeCreatesCapsLabelsAndTaintsANewNode(t *testing.T) {
  	out, calls, err := runUpFunctions(t, buildNodeWorld{label: "builds", taint: "NoSchedule"},
  		"CLUSTER_NAME=memql BUILD_NODE=true BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
  	if err != nil {
  		t.Fatalf("ensure_build_node failed: %v\n%s", err, out)
  	}
  	for _, want := range []string{
  		"k3d node create memql-build --cluster memql --role agent --memory 40g --k3s-node-label memql.io/pipeline-pool=builds --k3s-arg --node-taint=memql.io/pipeline-pool=builds:NoSchedule --wait --timeout 120s",
  		"docker update --cpus 16 k3d-memql-build-0",
  		"kubectl label node k3d-memql-build-0 memql.io/pipeline-pool=builds --overwrite",
  		"kubectl taint node k3d-memql-build-0 memql.io/pipeline-pool=builds:NoSchedule --overwrite",
  	} {
  		if !strings.Contains(calls, want) {
  			t.Errorf("missing %q in:\n%s", want, calls)
  		}
  	}
  }

  func TestEnsureBuildNodeKeepsAnExistingNodeAndReappliesItsCap(t *testing.T) {
  	out, calls, err := runUpFunctions(t, buildNodeWorld{listed: true, label: "builds", taint: "NoSchedule"},
  		"CLUSTER_NAME=memql BUILD_NODE=true BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
  	if err != nil {
  		t.Fatalf("ensure_build_node failed: %v\n%s", err, out)
  	}
  	if strings.Contains(calls, "k3d node create") {
  		t.Errorf("an existing build node was created again:\n%s", calls)
  	}
  	if !strings.Contains(calls, "docker update --cpus 16 k3d-memql-build-0") {
  		t.Errorf("the CPU cap was not re-applied:\n%s", calls)
  	}
  }

  func TestEnsureBuildNodeRefusesANodeWithoutItsTaint(t *testing.T) {
  	out, _, err := runUpFunctions(t, buildNodeWorld{listed: true, label: "builds", taint: ""},
  		"CLUSTER_NAME=memql BUILD_NODE=true BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
  	if exitCode(err) != 5 || !strings.Contains(out, "is not labelled and tainted") {
  		t.Fatalf("an untainted build node was accepted (exit %d):\n%s", exitCode(err), out)
  	}
  }

  func TestEnsureBuildNodeSkipsWhenDisabled(t *testing.T) {
  	out, calls, err := runUpFunctions(t, buildNodeWorld{}, "CLUSTER_NAME=memql BUILD_NODE=false\nensure_build_node")
  	if err != nil || strings.Contains(calls, "k3d") || strings.Contains(calls, "docker") {
  		t.Fatalf("--build-node=false still touched the drivers (err %v):\n%s\n%s", err, out, calls)
  	}
  }

  func TestEnsureBuildNodeSaysWhenTheCacheIsBoundElsewhere(t *testing.T) {
  	out, _, err := runUpFunctions(t, buildNodeWorld{listed: true, label: "builds", taint: "NoSchedule", boundTo: "k3d-memql-server-0"},
  		"CLUSTER_NAME=memql BUILD_NODE=true BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g\nensure_build_node")
  	if err != nil {
  		t.Fatalf("a cache bound elsewhere failed the bring-up: %v\n%s", err, out)
  	}
  	if !strings.Contains(out, "bound to k3d-memql-server-0") || !strings.Contains(out, "delete pvc memql-pipelines-cache") {
  		t.Fatalf("no warning with the repair:\n%s", out)
  	}
  }

  func TestResolveBuildNodeRefusesWhatTheHostCannotHonour(t *testing.T) {
  	for _, c := range []struct{ vars, wantWord string }{
  		{"BUILD_NODE=maybe", "--build-node"},
  		{"BUILD_NODE=true BUILD_NODE_CPUS=0", "--build-node-cpus"},
  		{"BUILD_NODE=true BUILD_NODE_MEMORY=40", "--build-node-memory"},
  		{"BUILD_NODE=true BUILD_NODE_CPUS=64", "more than the Docker host"},
  		{"BUILD_NODE=true BUILD_NODE_MEMORY=128g", "more than the Docker host"},
  	} {
  		out, _, err := runUpFunctions(t, buildNodeWorld{cpus: "24", bytes: "66793443328"},
  			"BUILD_NODE_CPUS=''\nBUILD_NODE_MEMORY=''\n"+c.vars+"\nresolve_build_node")
  		if exitCode(err) != 2 || !strings.Contains(out, c.wantWord) {
  			t.Errorf("%s: exit %d, want 2 naming %q:\n%s", c.vars, exitCode(err), c.wantWord, out)
  		}
  	}
  }

  func TestUpAndBringupDeclareTheBuildNodeParams(t *testing.T) {
  	for _, script := range []string{"up.sh", "bringup.sh"} {
  		out, err := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "k3d", script), "--print-spec").CombinedOutput()
  		if err != nil {
  			t.Fatalf("%s --print-spec: %v\n%s", script, err, out)
  		}
  		for _, p := range []string{"build-node", "build-node-cpus", "build-node-memory"} {
  			if !strings.Contains(string(out), `"`+p+`"`) {
  				t.Errorf("%s does not declare --%s", script, p)
  			}
  		}
  	}
  }
  ```

- [ ] **Step 2: Run them to see them fail.** Run: `go test -count=1 -run 'BuildNode' ./scripts/k3d/`. Expected: FAIL. The functions are missing ("command not found") and `--print-spec` lacks the params.

- [ ] **Step 3: Declare the params.** In `scripts/k3d/up.sh`, after `cap_spec_param "agents" ...`:

  ```bash
  cap_spec_param "build-node"        "create the dedicated pipeline build node (a k3d agent labelled and tainted memql.io/pipeline-pool=builds): true (default) or false"
  cap_spec_param "build-node-cpus"   "CPUs the build node may use (default: two thirds of the Docker host's, at least 2)"
  cap_spec_param "build-node-memory" "memory the build node may use, in whole gigabytes such as 40g (default: two thirds of the Docker host's, at least 4g)"
  ```

  Beside the other globals that `main` fills, add `BUILD_NODE="true"`, `BUILD_NODE_CPUS=""` and `BUILD_NODE_MEMORY=""`.

- [ ] **Step 4: Add the BUILD NODE section** after `create_cluster`:

  ```bash
  #=============================================================================
  # BUILD NODE
  #=============================================================================

  # The dedicated pipeline build node (design record
  # docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md,
  # section 4). The workbench pins every pipeline step to the pool
  # MEMQL_PIPELINES_NODE_POOL names, which the local overlay sets to `builds`;
  # this is the node that pool means locally. Labelled and tainted, so serving
  # pods never land on it; capped, so a burst of steps cannot reach the serving
  # node or the rest of the machine.
  readonly BUILD_POOL_LABEL="memql.io/pipeline-pool"
  readonly BUILD_POOL_VALUE="builds"

  # docker_host_cpus -- the Docker host's CPU count, or 0 when Docker cannot say.
  function docker_host_cpus() {
      local n
      n="$(docker info --format '{{.NCPU}}' 2>/dev/null || true)"
      if [[ "$n" =~ ^[0-9]+$ ]]; then
          echo "$n"
      else
          echo 0
      fi
  }

  # docker_host_memory_bytes -- the Docker host's memory in bytes, or 0.
  function docker_host_memory_bytes() {
      local b
      b="$(docker info --format '{{.MemTotal}}' 2>/dev/null || true)"
      if [[ "$b" =~ ^[0-9]+$ ]]; then
          echo "$b"
      else
          echo 0
      fi
  }

  # default_build_node_cpus <host cpus> -- two thirds of the host, at least 2.
  function default_build_node_cpus() {
      local cpus=$(( $1 * 2 / 3 ))
      if (( cpus < 2 )); then
          cpus=2
      fi
      echo "$cpus"
  }

  # default_build_node_memory <host bytes> -- two thirds of the host in whole
  # gigabytes, at least 4g.
  function default_build_node_memory() {
      local gib=$(( $1 * 2 / 3 / 1073741824 ))
      if (( gib < 4 )); then
          gib=4
      fi
      echo "${gib}g"
  }

  # resolve_build_node -- fills the defaults and refuses, before anything is
  # created, a value the host cannot honour.
  function resolve_build_node() {
      case "${BUILD_NODE}" in
          true|false) ;;
          *) cap_fail 2 "--build-node must be true or false, got '${BUILD_NODE}'" ;;
      esac
      if [[ "${BUILD_NODE}" == "false" ]]; then
          return 0
      fi
      local host_cpus host_bytes
      host_cpus="$(docker_host_cpus)"
      host_bytes="$(docker_host_memory_bytes)"
      if [[ -z "${BUILD_NODE_CPUS}" ]]; then
          BUILD_NODE_CPUS="$(default_build_node_cpus "${host_cpus}")"
      fi
      if [[ -z "${BUILD_NODE_MEMORY}" ]]; then
          BUILD_NODE_MEMORY="$(default_build_node_memory "${host_bytes}")"
      fi
      if ! [[ "${BUILD_NODE_CPUS}" =~ ^[1-9][0-9]*$ ]]; then
          cap_fail 2 "--build-node-cpus must be a whole number of CPUs, got '${BUILD_NODE_CPUS}'"
      fi
      if ! [[ "${BUILD_NODE_MEMORY}" =~ ^[1-9][0-9]*g$ ]]; then
          cap_fail 2 "--build-node-memory must be whole gigabytes such as 40g, got '${BUILD_NODE_MEMORY}'"
      fi
      if (( host_cpus > 0 && BUILD_NODE_CPUS > host_cpus )); then
          cap_fail 2 "--build-node-cpus=${BUILD_NODE_CPUS} is more than the Docker host's ${host_cpus} CPUs"
      fi
      if (( host_bytes > 0 && ${BUILD_NODE_MEMORY%g} * 1073741824 > host_bytes )); then
          cap_fail 2 "--build-node-memory=${BUILD_NODE_MEMORY} is more than the Docker host's $(( host_bytes / 1073741824 ))g"
      fi
  }

  # warn_cache_bound_elsewhere <node> -- a local-path claim stays on the node it
  # first bound to, so a cache bound before the build node existed cannot be
  # mounted by a step pinned to the pool. It holds only caches; say how to let
  # it re-bind rather than leave every step waiting.
  function warn_cache_bound_elsewhere() {
      local node="$1" volume bound
      volume="$(kubectl -n memql-pipelines get pvc memql-pipelines-cache -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)"
      if [[ -z "${volume}" ]]; then
          return 0
      fi
      bound="$(kubectl get pv "${volume}" -o jsonpath='{.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[0].values[0]}' 2>/dev/null || true)"
      if [[ -n "${bound}" && "${bound}" != "${node}" ]]; then
          warn "The pipelines cache is bound to ${bound}, not the build node ${node}; steps pinned to the build pool cannot mount it."
          warn "Repair: kubectl -n memql-pipelines delete pvc memql-pipelines-cache (Argo CD recreates it, and it binds on ${node} at the next step)."
      fi
  }

  # ensure_build_node -- create the node if it is missing, re-apply its CPU cap,
  # label and taint, and prove them. An existing cluster gains the node on its
  # next `make up`.
  function ensure_build_node() {
      if [[ "${BUILD_NODE}" != "true" ]]; then
          info "Build node: skipped (--build-node=false)."
          cap_result_set_raw buildNode false
          return 0
      fi
      local name="${CLUSTER_NAME}-build"
      local node="k3d-${name}-0"
      if ! k3d node list 2>/dev/null | grep -q "^${node}[[:space:]]"; then
          info "Creating build node ${node} (${BUILD_NODE_CPUS} CPUs, ${BUILD_NODE_MEMORY})..."
          k3d node create "${name}" \
              --cluster "${CLUSTER_NAME}" \
              --role agent \
              --memory "${BUILD_NODE_MEMORY}" \
              --k3s-node-label "${BUILD_POOL_LABEL}=${BUILD_POOL_VALUE}" \
              --k3s-arg "--node-taint=${BUILD_POOL_LABEL}=${BUILD_POOL_VALUE}:NoSchedule" \
              --wait \
              --timeout "120s" >&2 || cap_fail 5 "could not create the build node ${node}"
          cap_changed
      fi
      docker update --cpus "${BUILD_NODE_CPUS}" "${node}" >/dev/null \
          || cap_fail 5 "could not cap the build node ${node} at ${BUILD_NODE_CPUS} CPUs"
      kubectl label node "${node}" "${BUILD_POOL_LABEL}=${BUILD_POOL_VALUE}" --overwrite >/dev/null \
          || cap_fail 5 "could not label the build node ${node}"
      kubectl taint node "${node}" "${BUILD_POOL_LABEL}=${BUILD_POOL_VALUE}:NoSchedule" --overwrite >/dev/null \
          || cap_fail 5 "could not taint the build node ${node}"
      local pool effect
      pool="$(kubectl get node "${node}" -o jsonpath='{.metadata.labels.memql\.io/pipeline-pool}' 2>/dev/null || true)"
      effect="$(kubectl get node "${node}" -o jsonpath='{.spec.taints[?(@.key=="memql.io/pipeline-pool")].effect}' 2>/dev/null || true)"
      if [[ "${pool}" != "${BUILD_POOL_VALUE}" || "${effect}" != "NoSchedule" ]]; then
          cap_fail 5 "build node ${node} is not labelled and tainted ${BUILD_POOL_LABEL}=${BUILD_POOL_VALUE}:NoSchedule (label: ${pool:-none}, taint: ${effect:-none})"
      fi
      warn_cache_bound_elsewhere "${node}"
      info "Build node ${node}: ${BUILD_NODE_CPUS} CPUs, ${BUILD_NODE_MEMORY}, pool ${BUILD_POOL_VALUE}."
      cap_result_set_raw buildNode true
      cap_result_set     buildNodeName "${node}"
      cap_result_set_raw buildNodeCpus "${BUILD_NODE_CPUS}"
      cap_result_set     buildNodeMemory "${BUILD_NODE_MEMORY}"
  }
  ```

- [ ] **Step 5: Wire it into `main`.** Beside the other `cap_param` lines:

  ```bash
      BUILD_NODE="$(cap_param build-node "true")"
      BUILD_NODE_CPUS="$(cap_param build-node-cpus "")"
      BUILD_NODE_MEMORY="$(cap_param build-node-memory "")"
  ```

  After `check_prerequisites` and before `create_cluster`, call `resolve_build_node`, so a value the host cannot honour is refused before anything exists. After `ensure_local_traefik ... || cap_fail 5 ...`, add:

  ```bash
      # Outside create_cluster for the same reason as the ingress repair: an
      # existing cluster needs the node too.
      ensure_build_node
  ```

- [ ] **Step 6: Forward the params.** In `scripts/k3d/bringup.sh`:
  - declare the three params with `cap_spec_param`, using the same names and a "(forwarded to up.sh)" description;
  - add `BUILD_NODE=""`, `BUILD_NODE_CPUS=""` and `BUILD_NODE_MEMORY=""` beside its other defaults;
  - resolve them in its `main` with `cap_param <name> ""`, as it resolves `servers`;
  - append them to the `up.sh` call in `bringup()`:

  ```bash
          ${BUILD_NODE:+--build-node="${BUILD_NODE}"} \
          ${BUILD_NODE_CPUS:+--build-node-cpus="${BUILD_NODE_CPUS}"} \
          ${BUILD_NODE_MEMORY:+--build-node-memory="${BUILD_NODE_MEMORY}"} \
  ```

  In the `Makefile`'s `up` and `up-refresh` recipes, after the `AGENTS` line:

  ```make
  		$${BUILD_NODE:+--build-node=$${BUILD_NODE}} \
  		$${BUILD_NODE_CPUS:+--build-node-cpus=$${BUILD_NODE_CPUS}} \
  		$${BUILD_NODE_MEMORY:+--build-node-memory=$${BUILD_NODE_MEMORY}} \
  ```

  Add to `up`'s usage comment: `##   make up BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g   # size the pipeline build node (default: two thirds of the Docker host)`.

- [ ] **Step 7: Run the tests.** Run: `go test -count=1 ./scripts/k3d/ ./scripts/lib/`. Expected: PASS. The capability contract test proves both scripts still answer `--print-spec`, run non-interactively, and are valid bash.

- [ ] **Step 8: Document it.** In `deploy/k8s/components/pipelines/README.md`, change "The local overlay permits one Job at a time." to "Locally, `make up` creates the pool's node (`k3d-<cluster>-build-0`, two thirds of the Docker host by default), and the local overlay permits six Jobs at once on it." In `docs/public/operate/reproduce-the-cloud-locally.md`, add a short "Pipeline build node" subsection: what `make up` creates, the three variables, and the cache-rebinding warning.

- [ ] **Step 9: Commit.**

  ```bash
  git -C "$WT" add scripts/k3d/up.sh scripts/k3d/bringup.sh Makefile scripts/k3d/up_build_node_test.go deploy/k8s/components/pipelines/README.md docs/public/operate/reproduce-the-cloud-locally.md
  git -C "$WT" commit -m "k3d: make up creates the dedicated pipeline build node" -m "Part of #E." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf"
  ```

---

### Task 4: Local overlay values

**Files:**
- Modify: `deploy/k8s/overlays/local/pipelines-values.yaml`, `deploy/k8s/overlays/local/kustomization.yaml`
- Modify: `deploy/k8s/overlays/render_pipelines_test.go`, `deploy/k8s/overlays/render_pipeline_long_runs_test.go`
- Modify: `deploy/k8s/components/examples/pipelines-long-runs/kustomization.yaml` (its base becomes the cloud overlay)

**Interfaces:**
- Consumes: Task 3's pool name `builds`.
- Produces: rendered local values `count/jobs.batch: "6"`, ConfigMap `memql-pipelines` key `MEMQL_PIPELINES_NODE_POOL: builds`, `MEMQL_PIPELINES_RUN_MAX_MINUTES=480` on the agent and workbench Deployments.

- [ ] **Step 1: Write the failing tests.** In `render_pipelines_test.go`, change the `"local"` entry of `pipelinesSizing` to `ceiling: "6"` and add:

  ```go
  // The local overlay pins every step to the build node `make up` creates
  // (scripts/k3d/up.sh, ensure_build_node). The cloud overlays name no pool, so
  // their steps keep unconstrained Linux placement until a cloud build pool is
  // approved (design record 2026-10-11-pipelines-local-green-and-fast, D7).
  func TestOnlyTheLocalOverlaySelectsTheBuildPool(t *testing.T) {
  	for _, overlay := range pipelinesOverlays {
  		t.Run(overlay, func(t *testing.T) {
  			var cm struct {
  				Data map[string]string `yaml:"data"`
  			}
  			theOne(t, renderedObjects(t, overlay), "ConfigMap", pipelinesConfigMap).decode(t, &cm)
  			want := ""
  			if overlay == "local" {
  				want = "builds"
  			}
  			if got := cm.Data["MEMQL_PIPELINES_NODE_POOL"]; got != want {
  				t.Errorf("MEMQL_PIPELINES_NODE_POOL is %q, want %q", got, want)
  			}
  		})
  	}
  }
  ```

  Replace `render_pipeline_long_runs_test.go` with:

  ```go
  package overlays

  import (
  	"reflect"
  	"testing"
  )

  // The long-run profile (components/pipelines-long-runs) sets
  // MEMQL_PIPELINES_RUN_MAX_MINUTES=480 on the agent and the workbench and
  // nothing else. The local overlay composes it ON PURPOSE: the engine's own
  // memql-package.yaml declares a 6-hour step, and the local cluster runs that
  // package (design record 2026-10-11-pipelines-local-green-and-fast, section 4).
  // The cloud overlays do not; a cloud install opts in by composing the
  // component, as components/examples/pipelines-long-runs shows.
  const longRunKey = "MEMQL_PIPELINES_RUN_MAX_MINUTES"

  func longRunValues(t *testing.T, overlay string) map[string][]string {
  	t.Helper()
  	values := map[string][]string{}
  	for name, deployment := range parseDeployments(t, render(t, overlay)) {
  		for _, container := range deployment.Spec.Template.Spec.Containers {
  			for _, variable := range container.Env {
  				if variable.Name == longRunKey {
  					values[name] = append(values[name], container.Name+"="+variable.Value)
  				}
  			}
  		}
  	}
  	return values
  }

  func TestOnlyTheLocalOverlayOptsIntoLongRuns(t *testing.T) {
  	want := map[string][]string{"agent": {"agent=480"}, "workbench": {"workbench=480"}}
  	for _, overlay := range instanceOverlays {
  		got := longRunValues(t, overlay)
  		if overlay == "local" {
  			if !reflect.DeepEqual(got, want) {
  				t.Fatalf("local carries %v, want exactly %v", got, want)
  			}
  			continue
  		}
  		if len(got) != 0 {
  			t.Fatalf("%s silently opts all runs into the longer budget: %v", overlay, got)
  		}
  	}
  }

  func TestLongPipelineProfileIsExplicitAndPreservesOtherConfiguration(t *testing.T) {
  	before := parseDeployments(t, render(t, "cloud"))
  	after := parseDeployments(t, render(t, "../components/examples/pipelines-long-runs"))
  	if len(before) != len(after) || len(before) < len(meshDeployments) {
  		t.Fatal("long-run profile changed or lost the deployment inventory")
  	}
  	for name, deployment := range after {
  		count := 0
  		for i := range deployment.Spec.Template.Spec.Containers {
  			container := &deployment.Spec.Template.Spec.Containers[i]
  			for j := len(container.Env) - 1; j >= 0; j-- {
  				variable := container.Env[j]
  				if variable.Name != longRunKey {
  					continue
  				}
  				count++
  				if variable.Value != "480" || container.Name != name {
  					t.Fatalf("%s has an incorrect long-run deadline or container", name)
  				}
  				container.Env = append(container.Env[:j], container.Env[j+1:]...)
  			}
  		}
  		want := 0
  		if name == "agent" || name == "workbench" {
  			want = 1
  		}
  		if count != want {
  			t.Fatalf("%s: got %d deadline values, want %d", name, count, want)
  		}
  		if !reflect.DeepEqual(deployment, before[name]) {
  			t.Fatalf("%s: profile changed other container environment or secret/config wiring", name)
  		}
  	}
  }
  ```

  If `render(t, "cloud")` needs the same domain inputs other cloud render tests pass, use the same call those tests use.

- [ ] **Step 2: Run them to see them fail.** Run: `go test -count=1 -run 'BuildPool|LongRuns|LongPipelineProfile|Sizing|Ceiling' ./deploy/k8s/overlays/`. Expected: FAIL. Local's ceiling is "1", it names no pool, it carries no 480, and the example renders local.

- [ ] **Step 3: Change the values.** In `deploy/k8s/overlays/local/pipelines-values.yaml`, set `count/jobs.batch: "6"` and rewrite the header paragraph "THE CEILING AND THE LIMITS" to:

  ```yaml
  # THE CEILING AND THE LIMITS are this overlay's decision. Steps run on the
  # dedicated build node `make up` creates (scripts/k3d/up.sh,
  # ensure_build_node), capped at the CPUs and memory it was given -- 16 CPUs
  # and 40 GB on the workstation the 2026-10-11 design record sizes -- so six
  # steps may run at once across all workbench replicas, each container
  # reserving 250m / 512Mi / 1 GiB of disk and capped at two CPUs / 4 GiB /
  # 20 GiB of disk. The node's own caps, not this count, keep a burst off the
  # serving node; the count is tuned from the peaks phase 2 measures.
  ```

  In the same file's `memql-pipelines` ConfigMap `data`, add:

  ```yaml
    # THE BUILD POOL. Every step Job and isolation probe is pinned to nodes
    # labelled memql.io/pipeline-pool=builds and tolerates exactly that
    # NoSchedule taint (components/pipelines/README.md, "Dedicated build
    # placement"). `make up` creates that node; a cluster without it leaves
    # steps waiting rather than falling back onto the serving node.
    MEMQL_PIPELINES_NODE_POOL: builds
  ```

  In `deploy/k8s/overlays/local/kustomization.yaml`, after `- ../../components/pipelines`:

  ```yaml
    # THE 8-HOUR RUN PROFILE (design record
    # 2026-10-11-pipelines-local-green-and-fast, section 4). The engine's own
    # memql-package.yaml declares a 6-hour step and a 3-hour one, so a full
    # run of it cannot finish under the 2-hour default. Composed here on
    # purpose: this cluster runs that package. The cloud overlays keep the
    # default until a cloud cluster runs it.
    - ../../components/pipelines-long-runs
  ```

  In `deploy/k8s/components/examples/pipelines-long-runs/kustomization.yaml`, change `- ../../../overlays/local` to `- ../../../overlays/cloud`, with a comment saying the example shows a cloud install opting in, now that local composes the profile itself.

- [ ] **Step 4: Run the tests.** Run: `go test -count=1 ./deploy/... && make env-registry-check`. Expected: PASS. If a render gate elsewhere pinned the example's local base, it names itself; update it to the cloud base for the same reason.

- [ ] **Step 5: Commit.**

  ```bash
  git -C "$WT" add deploy/k8s/overlays/local/pipelines-values.yaml deploy/k8s/overlays/local/kustomization.yaml deploy/k8s/overlays/render_pipelines_test.go deploy/k8s/overlays/render_pipeline_long_runs_test.go deploy/k8s/components/examples/pipelines-long-runs/kustomization.yaml
  git -C "$WT" commit -m "deploy(local): six steps at once on the builds pool, with the 8-hour run profile" -m "Part of #E." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf"
  ```

---

### Task 5: The amd64 toolchain and the engine's manifest

**Files:**
- Modify: `memql-package.yaml`, `memql_package_manifest_test.go:446`
- Modify: `docs/public/operate/pipelines-substrate.md` ("the engine's own pipeline" section)

**Interfaces:**
- Produces: `pipeline.platform: linux/amd64` and `pipeline.image: ghcr.io/znasllc-io/memql-toolchain@sha256:<digest of 1.2.0-amd64>`.

- [ ] **Step 1: Build the toolchain on main.** This is the one owner-approved Actions dispatch.

  ```bash
  gh workflow run build-toolchain-image.yml --repo znasllc-io/memql --ref main -f version=1.2.0-amd64 -f platform=linux/amd64
  sleep 15
  RUN=$(gh run list --repo znasllc-io/memql --workflow build-toolchain-image.yml --limit 1 --json databaseId -q '.[0].databaseId')
  gh run watch "$RUN" --repo znasllc-io/memql --exit-status
  ```

  Expected: the run succeeds. If the tag already exists, the workflow refuses: choose the next free version (`1.2.1-amd64`) rather than overwrite.

- [ ] **Step 2: Read the digest and check the Go version.**

  ```bash
  DIGEST=$(gh api "orgs/znasllc-io/packages/container/memql-toolchain/versions?per_page=30" --jq '.[] | select(.metadata.container.tags | index("1.2.0-amd64")) | .name')
  echo "$DIGEST"
  docker run --rm "ghcr.io/znasllc-io/memql-toolchain@${DIGEST}" go version
  grep -E '^go ' "$WT/go.work"
  ```

  Expected: `sha256:` followed by 64 hex, and `go1.27.2 linux/amd64`, matching `go.work`'s `go 1.27.2`.

- [ ] **Step 3: Update the failing test first.** At `memql_package_manifest_test.go:446`, change `step.Platform != "linux/arm64"` to `step.Platform != "linux/amd64"`, and change the arm64 wording in the error message beside it. Run: `go test -count=1 -run 'Manifest|Portable' .`. Expected: FAIL, because the package still says arm64.

- [ ] **Step 4: Edit the package.** In `memql-package.yaml`:
  - Replace the header sentence "Local ARM64 pins do not prove amd64 releases." with "The package targets linux/amd64, the architecture of the local build node and of AKS (design record 2026-10-11-pipelines-local-green-and-fast, D9)."
  - Set `platform: linux/amd64`.
  - Set `image:` to `ghcr.io/znasllc-io/memql-toolchain@<DIGEST from Step 2>`.
  - Replace the toolchain comment's version sentence with "Pinned by the digest build-toolchain-image.yml's run on main printed for version 1.2.0-amd64 (run <RUN from Step 1>)."

- [ ] **Step 5: Run the manifest gates.** Run: `go test -count=1 -run 'Manifest|Portable|Package|Vendor' .`. Expected: PASS.

- [ ] **Step 6: Refresh the substrate doc.** In `docs/public/operate/pipelines-substrate.md`'s section on the engine's own pipeline, state the current facts:
  - the stages the package declares, in order;
  - `platform: linux/amd64` and toolchain 1.2.0-amd64;
  - the tag, cluster-e2e and module-boundary passes are now steps of the package;
  - full runs need the 8-hour profile, which the local overlay composes.

  Replace the stale example block at line 432 accordingly.

- [ ] **Step 7: Commit.**

  ```bash
  git -C "$WT" add memql-package.yaml memql_package_manifest_test.go docs/public/operate/pipelines-substrate.md
  git -C "$WT" commit -m "build(pipelines): the engine's package targets linux/amd64 on toolchain 1.2.0-amd64" -m "Part of #E." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf"
  ```

---

### Task 6: A manual run can name a branch

**Files:**
- Create: `component/pipelines/branch.go`, `component/pipelines/branch_test.go`
- Modify: `component/pipelines/refusal.go`, `component/packages/refusal.go`
- Modify: `component/pipelinerun/manual.go`, `component/pipelinerun/manual_test.go`, and every other `RunLatest(` caller (`git grep -n "RunLatest(" -- '*.go'`)
- Modify: `dsl/pipelines/builtins.memql`
- Regenerate: `sdk/go/client/generated_builtins.go`, `sdk/ts/src/client/generated_builtins.ts`
- Modify: `clients/os/src/apps/deployables/packages/refusals.ts`

**Interfaces:**
- Produces: `pipelines.ValidBranchName(name string) bool`; `pipelines.CodeBranchInvalid = "pipeline_branch_invalid"`; `packages.CodePipelineBranchInvalid = "pipeline_branch_invalid"`; `(*Integration).RunLatest(ctx context.Context, pipelineID, branch string) (Run, error)`; `builtin pipelinesRunLatest(pipelineId: string!, branch: string)`; TS `PipelinesRunLatestArgs { pipelineId: string; branch?: string }` rendering `builtin pipelinesRunLatest(pipelineId: "...", branch: "...")` only when `branch` is set.

- [ ] **Step 1: Write the leaf's failing test.**

  ```go
  // component/pipelines/branch_test.go
  package pipelines

  import (
  	"strings"
  	"testing"
  )

  func TestValidBranchName(t *testing.T) {
  	for _, name := range []string{"main", "trunk", "feature/login", "epic/pipelines-local-green", "fix_1.2", "release-0.25", "a/b/c", "UPPER", "dependabot/go_modules/x-1.2.3"} {
  		if !ValidBranchName(name) {
  			t.Errorf("%q refused", name)
  		}
  	}
  	for _, name := range []string{"", " main", "main ", "a b", "tab\there", "-main", "/main", "main/", "main.", "a..b", "a//b", "a@{1}", "@", "main~1", "main^", "a:b", "a?b", "a*b", "a[b", `a\b`, "refs/heads/main", ".hidden", "a/.b", "a.lock", "a/b.lock", strings.Repeat("a", 256)} {
  		if ValidBranchName(name) {
  			t.Errorf("%q accepted", name)
  		}
  	}
  }
  ```

  Run: `go test -count=1 github.com/znasllc-io/memql/component/pipelines/...`. Expected: FAIL, "undefined: ValidBranchName".

- [ ] **Step 2: Implement it.**

  ```go
  // component/pipelines/branch.go
  package pipelines

  import "strings"

  // ValidBranchName reports whether name is a plain branch name GitHub accepts,
  // checked before any request leaves the cluster: the part of git
  // check-ref-format a person can type, plus GitHub's length limit. It never
  // accepts a full ref ("refs/heads/main"), a revision expression ("main~1",
  // "main@{1}") or surrounding whitespace.
  func ValidBranchName(name string) bool {
  	if name == "" || len(name) > 255 || name == "@" || strings.HasPrefix(name, "refs/") {
  		return false
  	}
  	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
  		return false
  	}
  	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
  		return false
  	}
  	for _, r := range name {
  		if r < 0x20 || r == 0x7f || r == ' ' || strings.ContainsRune(`~^:?*[\`, r) {
  			return false
  		}
  	}
  	for _, part := range strings.Split(name, "/") {
  		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
  			return false
  		}
  	}
  	return true
  }
  ```

  In `component/pipelines/refusal.go`, beside `CodeManualRequiresDisconnected`:

  ```go
  	// CodeBranchInvalid: a manual run named a branch GitHub would not accept.
  	// Refused before any request leaves the cluster.
  	CodeBranchInvalid = "pipeline_branch_invalid"
  ```

  If that file keeps a list or map of every code (`git grep -n CodeManualRequiresDisconnected component/pipelines`), add the new code to it too. Run the leaf tests: PASS. That includes the purity test, which keeps the leaf stdlib-only.

- [ ] **Step 3: Write `RunLatest`'s failing tests.** In `component/pipelinerun/manual_test.go`, change every `h.integ.RunLatest(personCtx(X), p.ID)` to `h.integ.RunLatest(personCtx(X), p.ID, "")`, then add:

  ```go
  func TestRunLatestRunsTheNamedBranchHead(t *testing.T) {
  	h := newHarness(t)
  	p := testPipeline(DeliveryWebhook)
  	p.Status = PipelineDisconnected
  	h.store.addPipeline(p)
  	h.github.repos[repoName] = githubapp.RepositoryInfo{FullName: "acme/shop", DefaultBranch: "trunk"}
  	h.github.heads[repoName+"@trunk"] = headAnswer{SHA: shaA, Message: "Default head"}
  	h.github.heads[repoName+"@epic/green"] = headAnswer{SHA: shaB, Message: "Branch head\nbody"}

  	got, err := h.integ.RunLatest(personCtx(ownerID), p.ID, "epic/green")
  	if err != nil {
  		t.Fatal(err)
  	}
  	if got.SHA != shaB || got.HeadBranch != "epic/green" || got.Title != "Branch head" ||
  		got.Event != pipelines.EventManual || got.Mode != pipelines.ModeFull || got.Trigger != TriggerManual {
  		t.Fatalf("named-branch run = %+v", got)
  	}
  	if len(h.github.created) != 1 {
  		t.Fatalf("check runs written = %d, want 1", len(h.github.created))
  	}
  }

  func TestRunLatestRefusesAMalformedBranchBeforeGitHub(t *testing.T) {
  	for _, branch := range []string{" main", "a..b", "-x", "refs/heads/main", "x:y", "feature/", "a b"} {
  		t.Run(branch, func(t *testing.T) {
  			h := newHarness(t)
  			p := testPipeline(DeliveryWebhook)
  			p.Status = PipelineDisconnected
  			h.store.addPipeline(p)
  			if _, err := h.integ.RunLatest(personCtx(ownerID), p.ID, branch); refusalCode(err) != pipelines.CodeBranchInvalid {
  				t.Fatalf("refusal = %v, want %s", err, pipelines.CodeBranchInvalid)
  			}
  			if len(h.github.tokenMints) != 0 || len(h.github.created) != 0 {
  				t.Fatalf("a malformed branch reached GitHub: tokens %d checks %d", len(h.github.tokenMints), len(h.github.created))
  			}
  		})
  	}
  }

  func TestRunLatestThroughTwoBranchesToOneCommitIsOneAttempt(t *testing.T) {
  	h := newHarness(t)
  	p := testPipeline(DeliveryWebhook)
  	p.Status = PipelineDisconnected
  	h.store.addPipeline(p)
  	h.github.repos[repoName] = githubapp.RepositoryInfo{FullName: "acme/shop", DefaultBranch: "trunk"}
  	h.github.heads[repoName+"@trunk"] = headAnswer{SHA: shaA, Message: "One commit"}
  	h.github.heads[repoName+"@also"] = headAnswer{SHA: shaA, Message: "One commit"}

  	first, err := h.integ.RunLatest(personCtx(ownerID), p.ID, "")
  	if err != nil {
  		t.Fatal(err)
  	}
  	second, err := h.integ.RunLatest(personCtx(ownerID), p.ID, "also")
  	if err != nil || second.ID != first.ID || len(h.github.created) != 1 {
  		t.Fatalf("second request = %+v, checks %d, error %v", second, len(h.github.created), err)
  	}
  }

  func TestRunLatestReportsABranchGitHubDoesNotHave(t *testing.T) {
  	h := newHarness(t)
  	p := testPipeline(DeliveryWebhook)
  	p.Status = PipelineDisconnected
  	h.store.addPipeline(p)
  	h.github.repos[repoName] = githubapp.RepositoryInfo{FullName: "acme/shop", DefaultBranch: "trunk"}
  	if _, err := h.integ.RunLatest(personCtx(ownerID), p.ID, "gone"); err == nil {
  		t.Fatal("a missing branch opened a run")
  	}
  	if len(h.github.created) != 0 {
  		t.Fatalf("a missing branch wrote %d check runs", len(h.github.created))
  	}
  }
  ```

  Run: `go test -count=1 -run RunLatest github.com/znasllc-io/memql/component/pipelinerun/...`. Expected: FAIL to compile, because `RunLatest` takes two arguments.

- [ ] **Step 4: Implement it in `component/pipelinerun/manual.go`.** `handleRunLatest` passes the argument:

  ```go
  	run, err := i.RunLatest(handlerContext(ctx), stringArg(args, "pipelineId"), stringArg(args, "branch"))
  ```

  `RunLatest` becomes the following. Keep every existing line between the excerpts unchanged.

  ```go
  // RunLatest opens a full manual rehearsal of a branch head: the named branch
  // of the pipeline's repository, or its default branch when branch is empty.
  // Automatic delivery must already be disconnected, so a manual run cannot
  // duplicate a webhook or poll run. A name GitHub would not accept is refused
  // before any request leaves the cluster. The source's owner's GitHub grant is
  // used to read and check the exact commit; the commit is pinned in the run row.
  func (i *Integration) RunLatest(ctx context.Context, pipelineID, branch string) (Run, error) {
  	// ... personFrom, store and GitHub checks, the pipelineId check: unchanged ...
  	if branch != "" && !pipelines.ValidBranchName(branch) {
  		return Run{}, pipelines.Refuse(pipelines.CodeBranchInvalid, "", "%q is not a branch name GitHub accepts", branch)
  	}
  	// ... the owner-scoped read, the active and credential checks, the token,
  	// the installation check and d.GitHub.Repository: unchanged ...
  	if branch == "" {
  		branch = strings.TrimSpace(info.DefaultBranch)
  		if branch == "" {
  			return Run{}, fmt.Errorf("pipelines: GitHub names no default branch for %s", p.Repository)
  		}
  	}
  	sha, title, err := d.GitHub.BranchHead(ctx, token, p.Repository, branch)
  	// ... unchanged from here ...
  ```

  Update the remaining callers from the `git grep` to pass `""`. Run: `go test -count=1 -run RunLatest github.com/znasllc-io/memql/component/pipelinerun/...`. Expected: PASS.

- [ ] **Step 5: Mirror the code where the OS reads it.** In `component/packages/refusal.go`, beside `CodePipelineManualRequiresDisconnected` (line 497):

  ```go
  	CodePipelineBranchInvalid = "pipeline_branch_invalid"
  ```

  In `clients/os/src/apps/deployables/packages/refusals.ts`, beside the `pipeline_manual_requires_disconnected` copy entry:

  ```ts
    pipeline_branch_invalid: {
      // A mistyped branch is caught before GitHub is asked, so the answer
      // names the rule and the way to write it.
      title: "That is not a branch name GitHub accepts",
      next: "Use the name as GitHub shows it, such as main or feature/login.",
    },
  ```

  It is a fault with a repair, so it does NOT join the decision set.

- [ ] **Step 6: Declare the argument in the DSL.** In `dsl/pipelines/builtins.memql`, replace `pipelinesRunLatest`'s `@description` and body with:

  ```memql
  @description("Start a full manual rehearsal of a branch head for one of the caller's disconnected pipelines: the branch named, or the repository's default branch when none is. Automatic delivery must be disconnected so this run cannot duplicate a webhook or poll. The source owner's GitHub grant reads the branch, the exact SHA is pinned in the run, and the run writes its check run on that commit. A name GitHub would not accept is refused pipeline_branch_invalid before GitHub is asked. The sealed workflow includes stages with no `on` list and stages explicitly marked `on: [manual]`; it does not treat manual as push or release, so future publication steps remain opted out. Repeated requests for the same head resolve to the existing attempt, whichever branch named it; re-run that attempt to start another.")
  builtin pipelinesRunLatest {
    pipelineId  string!  @description("The caller's v1:pipelines:pipeline. It must be disconnected.")
    branch      string   @description("A branch of the same repository, named as GitHub shows it (main, feature/login). Absent or empty runs the default branch.")
  }
  ```

- [ ] **Step 7: Regenerate and run the gates.**

  ```bash
  cd "$WT"
  go run ./cmd/memqllint dsl/
  make sdk-gen
  git diff --stat sdk/
  make sdk-gen-check
  make arch-model-check || make arch-model
  go test -count=1 github.com/znasllc-io/memql/component/pipelines/... github.com/znasllc-io/memql/component/pipelinerun/... github.com/znasllc-io/memql/component/packages/...
  ```

  Expected: lint clean. The SDK diff touches only `pipelinesRunLatest` in the two `generated_builtins` files. Both checks pass, and the tests pass.

- [ ] **Step 8: Commit.**

  ```bash
  git -C "$WT" add component/pipelines/branch.go component/pipelines/branch_test.go component/pipelines/refusal.go component/packages/refusal.go component/pipelinerun/manual.go component/pipelinerun/manual_test.go dsl/pipelines/builtins.memql sdk/go/client/generated_builtins.go sdk/ts/src/client/generated_builtins.ts clients/os/src/apps/deployables/packages/refusals.ts
  ```

  Then add the other `RunLatest` callers and any regenerated architecture-model files by explicit path (`git -C "$WT" status --short`).

  ```bash
  git -C "$WT" commit -m "feat(pipelines): a manual run can name a branch" -m "Part of #E." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf"
  ```

---

### Task 7: **Other branch** in MemQL OS

**Files:**
- Modify: `clients/os/src/apps/deployables/pipelines/calls.ts`, `clients/os/src/apps/deployables/pipelines/PipelinePage.tsx`
- Create: `clients/os/src/apps/deployables/pipelines/BranchChoice.tsx`
- Modify: `clients/os/test/deployables/pipelines/pipelinePage.test.tsx`

**Interfaces:**
- Consumes: Task 6's `pipelinesRunLatest({ pipelineId, branch })`; `useSourceProbe()` from `../sources/useProbes` (`probe(repoUrl, credentialId)`, `reply.branches`, `busy`); kit `Field`, `Select({ id, label, value, onChange })`, `Input({ id, label, value, onChange, placeholder, onEnter })`.
- Produces: `runLatestPipeline(query, pipelineId, branch = "")`; `BranchChoice` props `{ branches: readonly string[]; defaultBranch: string; value: string; onChange: (next: string) => void; onRun: () => void; onCancel: () => void }`.

- [ ] **Step 1: Load the design rules.** Read `.agents/skills/memql-ui-design/SKILL.md`, `clients/os/DESIGN.md` and `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`, and invoke the `frontend-design:frontend-design` skill. The surface follows the page's existing ask-in-place pattern (Disconnect's "Keep / Disconnect"):
  - one primary button;
  - Cancel drawn as text;
  - the field in the page's own Panel, under the Facts;
  - no new colours, no icons beyond the kit's, and no unseen-update marker.

- [ ] **Step 2: Write the failing tests.** In `pipelinePage.test.tsx`, change the disconnect test's two expectations to `expect(bar().acts).toEqual(["Run latest", "Other branch", "Connect again"]);` and `expect(bar().buttons).toEqual(["Run latest"]);`, then add:

  ```tsx
  it("runs another branch, chosen from the repository's branches", async () => {
    const connection = fakeConnection(seed({
      pipelines: [pipelineRow({ status: "disconnected" })],
      sourceProbe: { "cred-1": { reachable: true, defaultBranch: "main", branches: ["main", "feature/cart"] } as unknown as Row },
      pipelinesRunLatestResult: { runId: "run-branch", sha: "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b", branch: "feature/cart" } as unknown as Row,
    }));
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    expect(bar().word).toBe("Run a branch");
    expect(bar().acts).toEqual(["Cancel"]);
    const select = await screen.findByLabelText("Branch to run");
    fireEvent.change(select, { target: { value: "feature/cart" } });
    await waitFor(() => expect(bar().acts).toEqual(["Cancel", "Run"]));
    await click(screen.getByRole("button", { name: "Run" }));
    expect(connection.callsNamed("pipelinesRunLatest")).toEqual(['builtin pipelinesRunLatest(pipelineId: "pl-shop", branch: "feature/cart")']);
    await waitFor(() => expect(bar().word).toBe("Manual run queued"));
    expect(bar().detail).toBe("feature/cart · 9a8b7c6");
  });

  it("takes a typed branch when the repository lists none, and sends nothing for a blank one", async () => {
    const connection = fakeConnection(seed({
      pipelines: [pipelineRow({ status: "disconnected" })],
      sourceProbe: { "cred-1": { reachable: true, defaultBranch: "main", branches: [] } as unknown as Row },
    }));
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    const field = await screen.findByLabelText("Branch to run");
    fireEvent.change(field, { target: { value: "   " } });
    expect(bar().acts).toEqual(["Cancel"]);
    fireEvent.keyDown(field, { key: "Enter" });
    expect(connection.callsNamed("pipelinesRunLatest")).toEqual([]);
    fireEvent.change(field, { target: { value: "feature/typed" } });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(connection.callsNamed("pipelinesRunLatest")).toEqual(['builtin pipelinesRunLatest(pipelineId: "pl-shop", branch: "feature/typed")']));
  });

  it("cancels without a call, by Cancel or by Escape", async () => {
    const connection = fakeConnection(seed({ pipelines: [pipelineRow({ status: "disconnected" })] }));
    mount(connection);
    await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(bar().word).toBe("Disconnected");
    await click(screen.getByRole("button", { name: "Other branch" }));
    fireEvent.keyDown(await screen.findByLabelText("Branch to run"), { key: "Escape" });
    await waitFor(() => expect(bar().word).toBe("Disconnected"));
    expect(connection.callsNamed("pipelinesRunLatest")).toEqual([]);
  });

  it("keeps the typed name and says why when the branch is refused", async () => {
    const connection = fakeConnection(seed({
      pipelines: [pipelineRow({ status: "disconnected" })],
      sourceProbe: { "cred-1": { reachable: true, defaultBranch: "main", branches: [] } as unknown as Row },
      pipelinesRunLatestError: 'pipeline_branch_invalid: "a..b" is not a branch name GitHub accepts',
    }));
    mount(connection);
    const page = await openSettings();
    await click(screen.getByRole("button", { name: "Other branch" }));
    const field = await screen.findByLabelText("Branch to run");
    fireEvent.change(field, { target: { value: "a..b" } });
    await click(screen.getByRole("button", { name: "Run" }));
    expect(await within(page).findByText("That is not a branch name GitHub accepts")).toBeTruthy();
    expect((screen.getByLabelText("Branch to run") as HTMLInputElement).value).toBe("a..b");
    expect(bar().word).toBe("Run a branch");
  });
  ```

  Import `fireEvent` from `@testing-library/react` beside the file's existing imports. If the harness's `sourceProbe` row needs other keys for `sourceProbeFromRow`, add them to the seed rows above.

  Run: `cd "$WT/clients/os" && npx vitest run test/deployables/pipelines/pipelinePage.test.tsx`. Expected: FAIL, because there is no "Other branch".

- [ ] **Step 3: Pass the branch through.** In `calls.ts`:

  ```ts
  export async function runLatestPipeline(query: QueryClient, pipelineId: string, branch = ""): Promise<{ runId: string; sha: string; branch: string }> {
    const named = branch.trim();
    const result = await query.pipelinesRunLatest(named === "" ? { pipelineId } : { pipelineId, branch: named });
    const row = firstRow(result);
    return { runId: row ? rowString(row, "runId") : "", sha: rowString(row ?? {}, "sha"), branch: rowString(row ?? {}, "branch") };
  }
  ```

- [ ] **Step 4: Write `BranchChoice.tsx`.**

  ```tsx
  import { Field, Input, Select } from "../../../kit";

  /**
   * THE BRANCH TO RUN. A list when the repository answers with its branches,
   * the way the deploy flow's "Branch or tag" does; a text field when it
   * answers none (a pasted-token source, a probe that failed), so the act is
   * never blocked on a read. The default branch is left out: Run latest is
   * that act, one press away.
   */
  export function BranchChoice({
    branches,
    defaultBranch,
    value,
    onChange,
    onRun,
    onCancel,
  }: {
    branches: readonly string[];
    defaultBranch: string;
    value: string;
    onChange: (next: string) => void;
    onRun: () => void;
    onCancel: () => void;
  }) {
    const others = branches.filter((name) => name !== defaultBranch);
    function onKeyDown(event: React.KeyboardEvent) {
      if (event.key === "Escape") onCancel();
    }
    return (
      <div className="pipeline-branch-choice" onKeyDown={onKeyDown}>
        <Field label="Branch">
          {others.length > 0 ? (
            <Select id="pipeline-branch" label="Branch to run" value={value} onChange={onChange}>
              <option value="">Choose a branch</option>
              {others.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </Select>
          ) : (
            <Input id="pipeline-branch" label="Branch to run" value={value} onChange={onChange} placeholder="feature/login" onEnter={() => { if (value.trim() !== "") onRun(); }} />
          )}
        </Field>
      </div>
    );
  }
  ```

  If the kit's `Select` or `Input` take their accessible name from a different prop than `label`, use that prop so that `getByLabelText("Branch to run")` finds the control.

- [ ] **Step 5: Wire it into `PipelinePage.tsx`.**
  - Import `BranchChoice` and `useSourceProbe` (`../sources/useProbes`).
  - Add `const [choosing, setChoosing] = useState(false);`, `const [branch, setBranch] = useState("");` and `const probe = useSourceProbe();`.
  - `runLatest` takes `(named = "")` and calls `runLatestPipeline(connection.query, pipeline.id, named)`. On success it does `setManualRun(result); setChoosing(false); setBranch("");`. On a refusal it keeps `choosing` and the typed `branch`.
  - Add `function chooseBranch() { setProblem(null); setBranch(""); setChoosing(true); void probe.probe(pkg.repoUrl, pkg.credentialId); }` and `function stopChoosing() { setChoosing(false); setBranch(""); }`.
  - In the `active` effect's non-active branch, keep `choosing` as it is; when the pipeline becomes active, call `stopChoosing()`.
  - The disconnected `bar` gets a new branch before the plain Disconnected one:

  ```tsx
        : choosing
          ? {
              state: "Run a branch",
              detail: "Its head runs in full, like Run latest.",
              tone: "live",
              acts: [
                { label: "Cancel", text: true, onAct: stopChoosing },
                ...(branch.trim() !== "" ? [{ label: "Run", tone: "primary" as const, busy, onAct: () => void runLatest(branch) }] : []),
              ],
            }
  ```

  The plain Disconnected acts become `Run latest` (primary, unchanged), then `{ label: "Other branch", text: true, onAct: chooseBranch }` under `can.rerun`, then `Connect again`. Render `{choosing && !active ? <BranchChoice branches={probe.reply?.branches ?? []} defaultBranch={pipeline.defaultBranch} value={branch} onChange={setBranch} onRun={() => void runLatest(branch)} onCancel={stopChoosing} /> : null}` inside the Panel, after the `ProblemNotice` line.

- [ ] **Step 6: Run the OS gates, one at a time** (`make os-test` runs `npm ci`; never run two at once).

  ```bash
  cd "$WT"
  make os-typecheck
  make os-test
  make os-build
  ```

  Expected: all pass, the four new tests included. `os-build` is the only one that parses the stylesheet; if `pipeline-branch-choice` needs spacing, add the smallest rule to `pipelines.css` using the page's existing tokens.

- [ ] **Step 7: Commit.**

  ```bash
  git -C "$WT" add clients/os/src/apps/deployables/pipelines/calls.ts clients/os/src/apps/deployables/pipelines/PipelinePage.tsx clients/os/src/apps/deployables/pipelines/BranchChoice.tsx clients/os/test/deployables/pipelines/pipelinePage.test.tsx
  ```

  If you added the CSS rule, also add `clients/os/src/apps/deployables/pipelines/pipelines.css`.

  ```bash
  git -C "$WT" commit -m "feat(os): run another branch from the pipeline page" -m "Part of #E." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf"
  git -C "$WT" push
  ```

  The real-browser pass (light and dark, desktop and narrow) happens in Task 8 on the live cluster, because only a rendered OS shows it.

---

### Task 8: Bring-up on this workstation

**Files:** none (operational). Evidence goes on Task 8's issue.

**Interfaces:**
- Consumes: Tasks 3 to 7 pushed on `epic/pipelines-local-green`.
- Produces: a cluster `memql` with `k3d-memql-build-0`; this cluster's GitHub App; the engine repository as a source with a disconnected pipeline whose id is `PIPELINE`; a personal access token in `~/.config/memql/local-pat` (mode 600).

- [ ] **Step 1: Bring the cluster up from the worktree.** `make up` points Argo CD at the current branch, which is pushed.

  ```bash
  cd "$WT"
  make up BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g
  make scale N=2
  make status
  ```

  Expected: the `make up` envelope is `ok` with `buildNode: true`, and `make status` reports unique node ids and one identity keyset. This also switches kubectl from `aks-memql` to `k3d-memql`; confirm with `kubectl config current-context` before any further kubectl command.

- [ ] **Step 2: Prove the node and the values.**

  ```bash
  kubectl get nodes -L memql.io/pipeline-pool
  kubectl get node k3d-memql-build-0 -o jsonpath='{.spec.taints}'; echo
  docker inspect k3d-memql-build-0 --format '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}'
  kubectl -n memql-pipelines get resourcequota memql-pipelines-ceiling -o jsonpath='{.spec.hard}'; echo
  kubectl -n memql exec deploy/workbench -- printenv MEMQL_PIPELINES_NODE_POOL MEMQL_PIPELINES_RUN_MAX_MINUTES
  kubectl -n memql exec deploy/agent -- printenv MEMQL_PIPELINES_RUN_MAX_MINUTES
  ```

  Expected:
  - only the build node shows `builds`, and its taint is `NoSchedule`;
  - `docker inspect` prints `16000000000 42949672960`;
  - the quota is `{"count/jobs.batch":"6"}`;
  - `builds` then `480`, then `480`.

- [ ] **Step 3: The owner's browser steps.** Ask the owner, in one message, to:
  1. claim or sign in as the cluster owner with their passkey at `https://os.memql.localhost`;
  2. open Deployables > Settings > Sources > **Set up GitHub**, choose the organization `znasllc-io`, rename the app on GitHub's page if the default name is taken (for example "MemQL local workstation"), create it, and install it on `znasllc-io/memql` only;
  3. mint a personal access token on the identity service's `/me` page and save it with `install -m 600 /dev/stdin ~/.config/memql/local-pat`.

  Wait for their confirmation.

- [ ] **Step 4: Add the source and park the pipeline.** In the OS:
  1. add a deployable from the GitHub connection for `znasllc-io/memql`, tracking `main`;
  2. on its source page, **Connect pipeline** with delivery `poll` and compute "This cluster", and read the stages before confirming;
  3. on the pipeline page, **Disconnect**.

  Record the pipeline id `PIPELINE` from the page context (`data-os-page-context`).

- [ ] **Step 5: The real-browser pass of Task 7.** On the pipeline page, press **Other branch**; check the list, the typed fallback, Cancel and Escape. Check light and dark, desktop and narrow (about 390 px wide), against `clients/os/DESIGN.md`. Fix anything off in Task 7's files, re-run its gates, commit, `make dev NODE=edge` when no run is in flight, and look again. Do not commit screenshots.

- [ ] **Step 6: Start the first run on the epic branch.**

  ```bash
  PAT=$(cat ~/.config/memql/local-pat)
  curl -sS https://api.memql.localhost/memql/query -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
    -d "{\"query\":\"builtin pipelinesRunLatest(pipelineId: \\\"$PIPELINE\\\", branch: \\\"epic/pipelines-local-green\\\")\"}" | tee /tmp/claude-1000/-home-znas-memql-projects-memql/6dc3b75b-28d6-4b70-8c32-cacb99b10e64/scratchpad/run-1.json
  ```

  The gateway answers 200 even for a refusal, so read the body: `errors` must be absent and the result must carry a `runId`. **Other branch** in the OS is the same act by hand.

- [ ] **Step 7: Prove placement.**

  ```bash
  kubectl -n memql-pipelines get pods -o wide --watch
  kubectl -n memql-pipelines get events --field-selector reason=Scheduled -o custom-columns=POD:.involvedObject.name,MSG:.message
  ```

  Expected: every step pod is scheduled on `k3d-memql-build-0`. Phase 1 exits when this run reaches a terminal state, green or not, with at least one step executed on the build node. Post the run id, commit, verdict and the placement evidence on Task 8's issue.

---

### Task 9: The green loop

**Files:** whatever each failure's fix touches, test first. Evidence goes on Task 9's issue.

**Interfaces:**
- Consumes: Task 8's cluster, `PIPELINE` and the token.
- Produces: three consecutive green full runs on three different commits of the epic branch.

- [ ] **Step 1: Run the epic branch's head** (Task 8 Step 6's command, or **Other branch** in the OS). One run at a time during this phase, so the long scans do not compete. Follow it without polling in the foreground: run `kubectl -n memql-pipelines get pods -w` in the background, and read the check run with `gh api "repos/znasllc-io/memql/commits/$(git -C "$WT" rev-parse HEAD)/check-runs" --jq '.check_runs[] | select(.name | test("MemQL")) | {status, conclusion, title: .output.title}'`. A full run takes about 7 hours with today's stage order.

- [ ] **Step 2: On any failed or refused step, debug it before touching code.** Invoke `superpowers:systematic-debugging`. Collect:
  - the step key, its code and message from the check-run summary;
  - the step's log from the owner's Library (`<stepKey>.log`), or `kubectl -n memql-pipelines logs` while the Job still exists;
  - for capacity questions, `docker stats --no-stream k3d-memql-build-0` sampled during the run.

  Find the root cause. A failure that does not reproduce is unexplained, not fixed.

- [ ] **Step 3: Fix it test-first.** Write the test that fails because of the cause, fix it, and run that package's tests by module path. For db-gated packages, use `MEMQL_REQUIRE_DB=1` against a real Postgres. Commit by explicit path with "Part of #E." and the trailer, then push.

- [ ] **Step 4: Roll and re-run.**
  - An engine fix (runner, driver, workbench) needs `make dev` when NO run is in flight; then "Re-run failed" (`pipelinesRerun` with `failedOnly`) may re-run the same commit.
  - A fix to repository content (a test, a script, the manifest) needs a new run on the new branch head.
  - Never roll the engine while a run is in flight.

- [ ] **Step 5: Keep the ledger.** For every run, append a row to Task 9's issue: run id, commit, verdict, wall clock, per-stage times, failures with codes, and the build node's peak CPU and memory. These are epic B's starting profile.

- [ ] **Step 6: Exit.** Three consecutive full runs pass every stage on three different commits, with no engine rollout during any of them. Between runs, `git merge origin/main` into the branch to get a new commit and stay current. Resolve conflicts, run the touched tests, and push. Post the three run ids on Task 9's issue.

---

### Task 10: The single PR and the merge

**Files:**
- Modify: `docs/internal/planning/pipelines-self-hosting.md`, `docs/superpowers/specs/2026-09-16-pipelines-program-design.md` (a short notes section pointing to the new record), `docs/public/operate/pipelines.md` (manual runs name a branch; a manual run's check run)
- Delete: `docs/superpowers/plans/2026-10-11-pipelines-local-green.md` (a spent plan is deleted in its epic's merge)

**Interfaces:**
- Consumes: every task's commits; the issue numbers from Task 1.

- [ ] **Step 1: Make the docs true.**
  - `pipelines-self-hosting.md`: the status now; the checklist reconciled with what merged, including the "Test real local cluster execution" item with the three run ids; its #5844 sentence changed to "#5844 was closed on 2026-10-11 as superseded; the deferral of Azure capacity and Cilium stands."
  - `pipelines.md`: `pipelinesRunLatest`'s `branch`, **Other branch**, and the check run a manual run writes.
  - The program record: a "## 11. Next phase" section linking the new record.
  - Delete this plan: `git -C "$WT" rm docs/superpowers/plans/2026-10-11-pipelines-local-green.md`.

- [ ] **Step 2: Verify the whole tree locally.**

  ```bash
  cd "$WT"
  make test
  MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN="$DSN" go test -count=1 github.com/znasllc-io/memql/component/pipelinerun/... github.com/znasllc-io/memql/test/pipelinehop/...
  make os-typecheck
  make os-test
  make os-build
  make sdk-gen-check
  make arch-model-check
  go run ./cmd/memqllint dsl/
  make env-registry-check
  ```

  `$DSN` is the real throwaway Postgres your notes name (port 15434), never the k3d load balancer's 5432. Expected: all green. Record anything red with its output; do not move on.

- [ ] **Step 3: Commit the docs** (explicit paths, "Part of #E.", trailer) and push.

- [ ] **Step 4: Open the one PR.**

  ```bash
  gh pr create --repo znasllc-io/memql --base main --head epic/pipelines-local-green \
    --title "Pipelines, local green: the engine's own pipeline passes on a local amd64 cluster" \
    --body-file /tmp/claude-1000/-home-znas-memql-projects-memql/6dc3b75b-28d6-4b70-8c32-cacb99b10e64/scratchpad/pr-body.md
  ```

  The body has:
  - a summary of what changed;
  - the evidence (the three green runs and the post-merge plan);
  - a frontend-team note that the wire is unchanged except for the new optional builtin argument;
  - a test plan;
  - one closing line per issue (`Closes #E.`, `Closes #T1.` through `Closes #T7.`, `Closes #5697.`), each on its own line;
  - the footer:

  ```
  🤖 Generated with [Claude Code](https://claude.com/claude-code)

  https://claude.ai/code/session_01UBKDA2sceef1SooVZFzqLf
  ```

  Then check the links (indexing lags; query twice):

  ```bash
  gh pr view <PR> --repo znasllc-io/memql --json closingIssuesReferences --jq '[.closingIssuesReferences[]|"#\(.number)"]|join(", ")'
  ```

  Expected: all nine numbers.

- [ ] **Step 5: CI to green.** Watch `ci-required` with `gh pr checks <PR> --repo znasllc-io/memql --watch`. Each red is debugged and fixed at its cause, never re-run blind. Read `install-cluster-e2e` too: it is not required, but it tests this branch.

- [ ] **Step 6: Merge.** Check readiness with `scripts/dev/merge-as-owner.sh --pr=<PR> --check`; it must report no drift from the base and every required check SUCCESS. Enqueue with `gh pr merge <PR> --repo znasllc-io/memql`. If the code-owner review blocks the queue, merging is the owner's bypass (`scripts/dev/merge-as-owner.sh --pr=<PR>`). If this session may not run it, ask the owner to type `! scripts/dev/merge-as-owner.sh --pr=<PR>`.

- [ ] **Step 7: Verify main.** `git fetch origin && git log -1 --format='%h %s' origin/main` shows the merge, and the push run of `ci.yml` on main goes green.

---

### Task 11: Post-merge run, close-out and cleanup

**Files:** none in the repository.

- [ ] **Step 1: Point the cluster at main and run main.** Build from a clean checkout of main, never the shared one:

  ```bash
  git -C /home/znas/memql-projects/memql worktree add --detach /home/znas/memql-projects/memql-main-verify origin/main
  cd /home/znas/memql-projects/memql-main-verify
  bash scripts/identity/build-css.sh
  make up REVISION=main BUILD_NODE_CPUS=16 BUILD_NODE_MEMORY=40g
  ```

  Wait until no run is in flight. Then press **Run latest** on the pipeline page (the default branch). Expected: a green full run. Post it on the epic.

- [ ] **Step 2: Close every issue.** Confirm the merge closed the epic, the seven tasks and #5697 (`gh issue view <n> --json state`). Close any still open with an evidence comment. With ccpm, mark the epic `completed` and archive it under `.claude/epics/archived/`. Comment:
  - on #5480: epic A done, with the PR, the runs and what epic B starts from;
  - on #5835: the local recovery evidence from Task 9's ledger, against its last checkbox.

- [ ] **Step 3: Remove this epic's local state.**

  ```bash
  git -C "$WT" clean -fdq -- scripts/ci/__pycache__
  git -C /home/znas/memql-projects/memql worktree remove /home/znas/memql-projects/memql-pipelines-green
  git -C /home/znas/memql-projects/memql worktree remove /home/znas/memql-projects/memql-main-verify
  git -C /home/znas/memql-projects/memql branch -D epic/pipelines-local-green
  git -C /home/znas/memql-projects/memql ls-remote --exit-code origin refs/heads/epic/pipelines-local-green && git -C /home/znas/memql-projects/memql push origin --delete epic/pipelines-local-green
  ```

  Use `branch -D` only after Step 2 confirmed the merge; a squashed or queued merge can leave `-d` refusing a branch whose content is on main.

- [ ] **Step 4: Remove what is stale, and only that.**
  - **Worktrees.** List them with `git worktree list --porcelain`. Remove a worktree only when its branch is an ancestor of `origin/main` (`git merge-base --is-ancestor <branch> origin/main`) AND `git -C <path> status --porcelain` prints nothing. Then `git worktree prune`.
  - **Local branches.** Delete with `git branch -d` those `git branch --merged origin/main` lists, except `main` and any branch checked out in a worktree.
  - **Remote branches.** After `git fetch --prune`, delete a remote branch only when it is merged into `origin/main` and `gh pr list --repo znasllc-io/memql --head <name> --state open` is empty.

  Report what was kept and why. An unmerged branch, a dirty worktree or another person's open PR is not stale.

- [ ] **Step 5: Tell the owner it is done.** Report:
  - the PR and the merge;
  - the green runs (the three on the branch, one on main);
  - how to try it: `https://os.memql.localhost`, Deployables, the engine source, the pipeline page, **Run latest** and **Other branch**;
  - the kubectl context is now `k3d-memql`;
  - what epic B starts from.
