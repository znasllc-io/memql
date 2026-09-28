// Behavioural guards over merge-as-owner.sh's readiness check (memql#5016).
//
// THE BUG THESE PIN. The guard refused on EVERY failing check, including lanes
// the ruleset does not require. This repository has two that no pull request
// can turn green: CodeQL's `Analyze (go)`, which crashes on a 2GiB query result
// above roughly 300 changed files and is red on pristine `main`, and
// `install-cluster-e2e`, which is documented as flaky and installs a pinned
// RELEASED stack rather than the branch under test. So the guard was strictest
// on exactly the pull requests it was written for -- large refactors, removal
// epics, regenerations -- and named no way out. A guard that cannot be
// satisfied is not a safety measure; it is a reason to reach for
// `gh pr merge --admin`, which skips the script and its reporting entirely.
//
// WHY THESE ARE BEHAVIOURAL AND NOT A GREP. The interesting cases are all
// about what the script DOES with a mixed rollup, and the dangerous one --
// failing OPEN when the required-check set cannot be read -- is invisible to
// any static reading: an intersection against an unknown set is empty, and an
// empty intersection passes every red build. So the script is run for real
// against a stubbed `gh`, and the exit code is the assertion.
package dev

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mergeGuardGhStub writes a `gh` on PATH that answers from canned JSON. It honours
// `--jq` by piping through the real jq, because the script relies on gh doing
// that filtering server-side for some calls and does it locally for others --
// a stub that ignored the flag would exercise neither path faithfully.
//
// `pr merge` is answered, not performed: reaching it at all is the signal a
// test wants, and a stub that could merge would be a stub that can do damage.
// compareJSON is optional and defaults to a branch that is level with its base,
// because every test written before the staleness guard existed assumes one --
// and the interesting cases are the two that are not level.
func mergeGuardGhStub(t *testing.T, rulesetsJSON, rulesetJSON, prJSON string, compareJSON ...string) string {
	t.Helper()
	dir := t.TempDir()
	cmp := `{"status":"ahead","ahead_by":1,"behind_by":0}`
	if len(compareJSON) > 0 {
		cmp = compareJSON[0]
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rulesets := write("rulesets.json", rulesetsJSON)
	ruleset := write("ruleset.json", rulesetJSON)
	pr := write("pr.json", prJSON)
	compare := write("compare.json", cmp)
	// Every compare target the script asks for, so a test can assert WHICH two
	// commits were compared and not merely that a comparison happened.
	compareLog := filepath.Join(dir, "compare-targets")

	stub := `#!/usr/bin/env bash
# Minimal gh: dispatch on the sub-command, apply --jq with the real jq.
set -uo pipefail
sub="${1:-}"; shift || true
jqfilter=""
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --jq) jqfilter="$2"; shift 2 ;;
    -q)   jqfilter="$2"; shift 2 ;;
    --json) shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
emit() {
  if [ -n "$jqfilter" ]; then jq -r "$jqfilter" < "$1"; else cat "$1"; fi
}
case "$sub" in
  api)
    target="${args[0]:-}"
    case "$target" in
      *compare/*)  printf '%s\n' "$target" >> ` + shellQuote(compareLog) + `; emit ` + shellQuote(compare) + ` ;;
      *rulesets/*) emit ` + shellQuote(ruleset) + ` ;;
      *rulesets)   emit ` + shellQuote(rulesets) + ` ;;
      *)           echo '{}' ;;
    esac
    ;;
  pr)
    case "${args[0]:-}" in
      view)  emit ` + shellQuote(pr) + ` ;;
      merge) echo "STUB-MERGE-INVOKED" ;;
      *)     echo '{}' ;;
    esac
    ;;
  auth) exit 0 ;;
  *)    echo '{}' ;;
esac
`
	p := filepath.Join(dir, "gh")
	if err := os.WriteFile(p, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runMergeAsOwner runs the real script against the stub and returns its
// combined output plus its exit code. It is never given --check, because
// --check returns BEFORE guard_readiness and would test nothing here.
func runMergeAsOwner(t *testing.T, stubDir string) (string, int) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "dev", "merge-as-owner.sh")
	cmd := exec.Command("bash", script, "--pr=1")
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the script: %v\n%s", err, out)
	}
	return string(out), code
}

const rulesetsActive = `[{"id":16630577,"enforcement":"active"}]`

// requiresCIRequired mirrors the shape of this repository's own default
// ruleset: one required context, and an admin bypass.
const requiresCIRequired = `{
  "rules": [
    {"type":"pull_request","parameters":{"require_code_owner_review":true,"required_approving_review_count":0}},
    {"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci-required"}]}},
    {"type":"merge_queue","parameters":{}}
  ],
  "bypass_actors": [{"actor_type":"RepositoryRole","actor_id":5,"bypass_mode":"pull_request"}]
}`

func prRollup(checks string) string { return prRollupIn("BLOCKED", checks) }

// prRollupIn is the same envelope with the merge state as a parameter. The
// state decides whether merge_pr reaches for --admin, and two states cannot be
// served by it at all -- see TestGuardRefusesBehindAndUnknownByName.
func prRollupIn(state, checks string) string {
	return `{
  "state":"OPEN","title":"t","author":{"login":"znas-io"},
  "mergeable":"MERGEABLE","mergeStateStatus":"` + state + `","reviewDecision":null,
  "baseRefName":"main","headRefName":"topic","headRefOid":"0ddc0de0ddc0de0ddc0de0ddc0de0ddc0de0ddc0",
  "statusCheckRollup":[` + checks + `]
}`
}

const (
	ciRequiredGreen = `{"name":"ci-required","conclusion":"SUCCESS"}`
	ciRequiredRed   = `{"name":"ci-required","conclusion":"FAILURE"}`
	codeqlRed       = `{"name":"Analyze (go)","conclusion":"FAILURE"}`
	installRed      = `{"name":"round-trip","conclusion":"FAILURE"}`
)

// The headline case: the two lanes no pull request can turn green are red, the
// one context the ruleset requires is green, and the merge proceeds.
func TestGuardProceedsWhenOnlyNonRequiredChecksAreRed(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollup(ciRequiredGreen+","+codeqlRed+","+installRed))
	out, code := runMergeAsOwner(t, stub)
	if code != 0 {
		t.Fatalf("guard refused a pull request whose only red lanes are ones the ruleset does not "+
			"require; that is memql#5016 and it makes the script unusable on exactly the large "+
			"pull requests it exists for. exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "STUB-MERGE-INVOKED") {
		t.Errorf("the script did not reach the merge; output:\n%s", out)
	}
	if !strings.Contains(out, "proceeding over 2 failing check(s) the ruleset does not require") {
		t.Errorf("a merge that proceeded over red lanes must SAY so -- silence here is how the "+
			"next reader learns nothing was skipped. output:\n%s", out)
	}
}

// The other half of the same rule, and the reason the change is not a
// weakening: a red REQUIRED check still refuses.
func TestGuardStillRefusesARedRequiredCheck(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollup(ciRequiredRed+","+codeqlRed))
	out, code := runMergeAsOwner(t, stub)
	if code != 3 {
		t.Fatalf("a red REQUIRED check must refuse with exit 3, got %d\n%s", code, out)
	}
	if strings.Contains(out, "STUB-MERGE-INVOKED") {
		t.Fatal("the script merged over a failing required check")
	}
	if !strings.Contains(out, "REQUIRED check(s) FAILED") {
		t.Errorf("the refusal must name what it refused on; output:\n%s", out)
	}
}

// The report has to show the distinction the guard now turns on. A reader who
// sees only "FAILED" cannot tell whether the script was about to proceed.
func TestReportMarksWhichRedChecksAreRequired(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollup(ciRequiredGreen+","+codeqlRed))
	out, _ := runMergeAsOwner(t, stub)
	if !strings.Contains(out, "failed (not required): Analyze (go)") {
		t.Errorf("a non-required red check must be reported as such; output:\n%s", out)
	}
}

// THE FAIL-CLOSED CASE, and the one worth the whole file. When the ruleset's
// required-check list cannot be read, the intersection is empty -- and an empty
// intersection would pass every red build. The guard must fall back to
// refusing on ANY red check instead.
func TestUnreadableRequiredChecksFallBackToRefusingEverything(t *testing.T) {
	// A ruleset with no required_status_checks rule at all: the jq that reads
	// the contexts produces nothing, which is indistinguishable from a read
	// that failed.
	noRequired := `{
      "rules": [{"type":"pull_request","parameters":{"require_code_owner_review":true}}],
      "bypass_actors": [{"actor_type":"RepositoryRole","actor_id":5,"bypass_mode":"pull_request"}]
    }`
	stub := mergeGuardGhStub(t, rulesetsActive, noRequired, prRollup(codeqlRed))
	out, code := runMergeAsOwner(t, stub)
	if code != 3 {
		t.Fatalf("with the required-check set unreadable the guard must refuse on any red check "+
			"(an intersection against an unknown set is empty, which would pass everything). "+
			"exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "could not read the ruleset's required checks") {
		t.Errorf("the fallback must announce itself, or a reader cannot tell which rule applied; "+
			"output:\n%s", out)
	}
}

// A pending REQUIRED check still means "CI has not settled". Its non-required
// twin does not, for the same reason its failure does not.
func TestPendingRequiredRefusesButPendingNonRequiredDoesNot(t *testing.T) {
	pendingRequired := `{"name":"ci-required","status":"IN_PROGRESS"}`
	pendingOther := `{"name":"round-trip","status":"IN_PROGRESS"}`

	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired, prRollup(pendingRequired))
	if out, code := runMergeAsOwner(t, stub); code != 3 {
		t.Fatalf("a pending REQUIRED check must refuse, got exit %d\n%s", code, out)
	}

	stub = mergeGuardGhStub(t, rulesetsActive, requiresCIRequired, prRollup(ciRequiredGreen+","+pendingOther))
	out, code := runMergeAsOwner(t, stub)
	if code != 0 {
		t.Fatalf("a pending NON-required check must not block, got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "non-required check(s) still running") {
		t.Errorf("proceeding past a running lane must be stated; output:\n%s", out)
	}
}

// BEHIND and UNKNOWN are refused BY NAME, and this is the regression test for a
// reassuring else (memql#5019 shipped it; a peer session hit it merging behind
// a sibling).
//
// merge_pr reaches for --admin on BLOCKED and takes the ordinary path
// otherwise. Neither of these states can be served by the ordinary path: the
// author cannot give themselves the code-owner review it needs. So both used to
// log "no bypass needed", merge nothing, and report the non-merge as something
// that might still be enqueued -- every line consistent with success.
//
// BEHIND must be REFUSED rather than added to the bypass: the ruleset sets
// strict_required_status_checks_policy=true, so BEHIND means CI has not run
// against the current base, and forcing it is precisely what `strict` exists to
// prevent. The refusal names `gh pr update-branch` instead.
func TestGuardRefusesBehindAndUnknownByName(t *testing.T) {
	for _, tc := range []struct{ state, wants string }{
		{"BEHIND", "update-branch"},
		{"UNKNOWN", "recomputing mergeability"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
				prRollupIn(tc.state, ciRequiredGreen))
			out, code := runMergeAsOwner(t, stub)
			if code != 3 {
				t.Fatalf("%s must be refused with exit 3, got %d\n%s", tc.state, code, out)
			}
			if strings.Contains(out, "STUB-MERGE-INVOKED") {
				t.Fatalf("%s reached the merge; the ordinary path cannot satisfy a code-owner "+
					"review the author may not give, so this merges nothing and says it worked", tc.state)
			}
			if strings.Contains(out, "no bypass needed") {
				t.Errorf("%s fell through to the reassuring else; output:\n%s", tc.state, out)
			}
			if !strings.Contains(out, tc.wants) {
				t.Errorf("the refusal must say what to do next (%q); output:\n%s", tc.wants, out)
			}
		})
	}
}

// The control for the pair above: a state the ordinary path CAN serve is not
// caught by the new refusals. Without this, "refuse everything that is not
// BLOCKED" would pass every assertion in this file.
func TestGuardStillMergesACleanPullRequest(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollupIn("CLEAN", ciRequiredGreen))
	out, code := runMergeAsOwner(t, stub)
	if code != 0 {
		t.Fatalf("a CLEAN pull request must still merge, got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "STUB-MERGE-INVOKED") {
		t.Errorf("the script did not reach the merge; output:\n%s", out)
	}
}

// THE BUG THIS PINS, and the reason the BEHIND guard above was never once
// reached on this repository.
//
// `mergeStateStatus` is ONE value, and GitHub returns the strongest blocker it
// finds: BLOCKED outranks BEHIND. Every pull request here is blocked on a
// code-owner review its author may not give -- CODEOWNERS routes the paths a
// session actually touches (CLAUDE.md, app/, component/auth/, .github/) to
// @znasllc-io/human-lead-developers, whose only member is the author. So a
// stale branch reads BLOCKED, never BEHIND, the case above matches nothing,
// and merge_pr reaches for --admin and lands a tree CI never tested against
// the current base. That is exactly what strict_required_status_checks_policy
// exists to prevent, and the bypass walks straight through it.
//
// Observed 2026-09-21: #5589 sat 16 commits behind main with 29/29 green, and
// `merge-as-owner.sh --pr=5589 --check` reported `MERGEABLE (BLOCKED)` with no
// mention of the drift.
//
// The fix cannot read mergeStateStatus at all for this question. It compares
// the head against the base directly, which is a fact rather than a summary.
func TestGuardRefusesAStaleBranchGitHubReportsAsBlocked(t *testing.T) {
	behind := `{"status":"diverged","ahead_by":3,"behind_by":16}`
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollupIn("BLOCKED", ciRequiredGreen), behind)
	out, code := runMergeAsOwner(t, stub)

	if code != 3 {
		t.Fatalf("a branch 16 commits behind its base must be refused with exit 3 even though "+
			"GitHub reports BLOCKED rather than BEHIND; merging it lands a tree CI never tested "+
			"against the current base. exit=%d\n%s", code, out)
	}
	if strings.Contains(out, "STUB-MERGE-INVOKED") {
		t.Fatalf("the script merged a stale branch through the admin bypass:\n%s", out)
	}
	if !strings.Contains(out, "update-branch") {
		t.Errorf("the refusal must say what to do next; output:\n%s", out)
	}
	if !strings.Contains(out, "16") {
		t.Errorf("the refusal must name how far behind the branch is -- 'behind' and '16 commits "+
			"behind' are different sizes of problem; output:\n%s", out)
	}
}

// THE CONTROL, and the file's own standard: without it, "refuse every BLOCKED
// pull request" would satisfy the test above and break the script's whole
// purpose. A BLOCKED pull request whose branch is LEVEL with its base is the
// ordinary case here, and it must still merge through the bypass.
func TestGuardStillMergesABlockedPullRequestThatIsCurrent(t *testing.T) {
	level := `{"status":"ahead","ahead_by":3,"behind_by":0}`
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollupIn("BLOCKED", ciRequiredGreen), level)
	out, code := runMergeAsOwner(t, stub)

	if code != 0 {
		t.Fatalf("a BLOCKED pull request level with its base must still merge through the bypass "+
			"-- that is the script's entire purpose. exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "STUB-MERGE-INVOKED") {
		t.Fatalf("the script did not reach the merge:\n%s", out)
	}
}

// FAIL CLOSED, for the same reason the required-check intersection does. A
// comparison that cannot be read yields no number, and treating "no number" as
// zero would restore the bug for every case where the API call fails -- which
// is the case a reader would never think to test.
func TestUnreadableBaseComparisonRefuses(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollupIn("BLOCKED", ciRequiredGreen), `{}`)
	out, code := runMergeAsOwner(t, stub)

	if code != 3 {
		t.Fatalf("an unreadable base comparison must refuse: absent is not zero, and reading it "+
			"as zero reinstates the stale merge. exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "could not compare") {
		t.Errorf("the refusal must announce which rule applied; output:\n%s", out)
	}
}

// THE FORK HAZARD, and why the comparison is built from the OID.
//
// A fork pull request's `headRefName` is the FORK's branch name, and the
// commonest one is `main`. `compare/main...main` resolves BOTH sides in this
// repository and answers `behind_by: 0`, so a branch-name comparison measures
// every such pull request as current no matter how stale it is -- fail-open,
// in the one case nobody writes a test for, which is the same shape as the bug
// the whole guard exists to close.
//
// Verified against the live API 2026-09-21: `compare/main...main` returns
// `{"ahead_by":0,"behind_by":0}`.
//
// The oid names ONE commit, is the commit CI actually ran on, and cannot be
// re-pointed between the read and the comparison.
func TestBaseComparisonIsBuiltFromTheHeadOidNotTheBranchName(t *testing.T) {
	// headRefName is `main` -- the fork case -- while the oid is a real,
	// distinct commit. A comparison built from the name would ask
	// `main...main` and be told zero.
	forkish := `{
  "state":"OPEN","title":"t","author":{"login":"somebody"},
  "mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","reviewDecision":null,
  "baseRefName":"main","headRefName":"main","headRefOid":"f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0",
  "statusCheckRollup":[` + ciRequiredGreen + `]
}`
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired, forkish,
		`{"status":"diverged","ahead_by":1,"behind_by":9}`)
	out, code := runMergeAsOwner(t, stub)

	targets, err := os.ReadFile(filepath.Join(stub, "compare-targets"))
	if err != nil {
		t.Fatalf("the script never compared the head against the base at all: %v\n%s", err, out)
	}
	got := string(targets)
	if !strings.Contains(got, "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0") {
		t.Errorf("the comparison must name the head OID, not the branch name -- a fork branch "+
			"called `main` compares against itself and answers zero. asked: %q", got)
	}
	if strings.Contains(got, "compare/main...main") {
		t.Errorf("the script compared main against main, which is always zero commits behind "+
			"itself. asked: %q", got)
	}
	if code != 3 {
		t.Fatalf("a head 9 commits behind must be refused whatever its branch is called, "+
			"got exit %d\n%s", code, out)
	}
}

// THE ABSENT REQUIRED CHECK, and the fail-open every guard above shared.
//
// The guard counted required checks that had FAILED and required checks that
// were PENDING, and refused on either. A required check that does not EXIST
// is neither, so both counts were zero and the guard passed. `ci-required` is
// an `if: always()` aggregate with `needs:` on every other lane, and GitHub
// does not create a job's check run until its `needs:` have finished -- so for
// most of a CI run the one check the ruleset requires is ABSENT from the
// rollup, and a merge attempted then went straight through the bypass.
//
// Observed 2026-09-28: `merge-as-owner.sh --pr=5709` printed `checks: 3
// passed, 0 failed, 13 pending`, warned that it was proceeding with 13
// non-required checks still running, and merged memql#5709 while all five
// db-tests shards were pending and no `ci-required` check run existed on the
// head commit.
//
// The fix is to ask the positive question: is every required context PRESENT
// with a SUCCESS conclusion? Not "is none of them red or running".
func TestGuardRefusesARequiredCheckThatHasNotReported(t *testing.T) {
	// The #5709 shape: a few lanes green, many running, no ci-required at all.
	running := `{"name":"changes","conclusion":"SUCCESS"},` +
		`{"name":"plan","conclusion":"SUCCESS"},` +
		`{"name":"db-tests (1)","status":"IN_PROGRESS"},` +
		`{"name":"db-tests (2)","status":"QUEUED"}`
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired, prRollup(running))
	out, code := runMergeAsOwner(t, stub)

	if code != 3 {
		t.Fatalf("a required check that has not reported must refuse with exit 3: it is neither "+
			"failed nor pending, so counting those alone passes it. That is how memql#5709 merged "+
			"with db-tests still running. exit=%d\n%s", code, out)
	}
	if strings.Contains(out, "STUB-MERGE-INVOKED") {
		t.Fatalf("the script merged with the required check absent:\n%s", out)
	}
	if !strings.Contains(out, "ci-required has not reported yet") {
		t.Errorf("the refusal must NAME the required context that is missing; output:\n%s", out)
	}
}

// The same hole at its widest: a pull request whose checks have not even been
// queued. Every count is zero, which is what "all clear" looked like too.
func TestGuardRefusesAnEmptyRollupWhenACheckIsRequired(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired, prRollup(``))
	out, code := runMergeAsOwner(t, stub)
	if code != 3 {
		t.Fatalf("an empty rollup must refuse when the ruleset requires a check, got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "ci-required has not reported yet") {
		t.Errorf("the refusal must name the missing context; output:\n%s", out)
	}
}

// Present is not the same as passed. The old guard recognised exactly two red
// conclusions (FAILURE, TIMED_OUT) and two running statuses (QUEUED,
// IN_PROGRESS); a required check in any other state -- cancelled, skipped, a
// commit status still PENDING or in ERROR -- was neither, and passed. GitHub
// itself accepts SKIPPED and NEUTRAL for a required check; this script does
// not, because it runs AROUND the ruleset, and an aggregate that did not run
// has said nothing about the lanes beneath it.
func TestGuardRefusesARequiredCheckThatIsPresentButNotSuccessful(t *testing.T) {
	for _, tc := range []struct{ name, entry, wants string }{
		{"cancelled check run", `{"name":"ci-required","status":"COMPLETED","conclusion":"CANCELLED"}`, "CANCELLED"},
		{"skipped check run", `{"name":"ci-required","status":"COMPLETED","conclusion":"SKIPPED"}`, "SKIPPED"},
		{"pending commit status", `{"context":"ci-required","state":"PENDING"}`, "PENDING"},
		{"errored commit status", `{"context":"ci-required","state":"ERROR"}`, "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired, prRollup(tc.entry))
			out, code := runMergeAsOwner(t, stub)
			if code != 3 {
				t.Fatalf("a required check in state %s must refuse with exit 3, got %d\n%s", tc.wants, code, out)
			}
			if strings.Contains(out, "STUB-MERGE-INVOKED") {
				t.Fatalf("the script merged over a required check in state %s:\n%s", tc.wants, out)
			}
			if !strings.Contains(out, "ci-required") || !strings.Contains(out, tc.wants) {
				t.Errorf("the refusal must name the context and the state it is in (%s); output:\n%s", tc.wants, out)
			}
		})
	}
}

// The control for the pair above: a required context reported as a commit
// STATUS (`.context` + `.state`) rather than a check run (`.name` +
// `.conclusion`) satisfies the guard when its state is SUCCESS. Without this,
// "only a check run can pass" would satisfy every refusal test here.
func TestGuardAcceptsASuccessfulRequiredCommitStatus(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollup(`{"context":"ci-required","state":"SUCCESS"}`))
	out, code := runMergeAsOwner(t, stub)
	if code != 0 {
		t.Fatalf("a required commit status in state SUCCESS must satisfy the guard, got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "STUB-MERGE-INVOKED") {
		t.Errorf("the script did not reach the merge; output:\n%s", out)
	}
}

// With more than one required context, EACH is judged, and the refusal names
// every one that is missing and none that is not. A guard that asked "is some
// required check green" would pass this pull request.
func TestGuardNamesEachMissingRequiredContext(t *testing.T) {
	requiresThree := `{
  "rules": [
    {"type":"pull_request","parameters":{"require_code_owner_review":true,"required_approving_review_count":0}},
    {"type":"required_status_checks","parameters":{"required_status_checks":[
      {"context":"ci-required"},{"context":"lint"},{"context":"license/cla"}
    ]}}
  ],
  "bypass_actors": [{"actor_type":"RepositoryRole","actor_id":5,"bypass_mode":"pull_request"}]
}`
	stub := mergeGuardGhStub(t, rulesetsActive, requiresThree, prRollup(ciRequiredGreen))
	out, code := runMergeAsOwner(t, stub)
	if code != 3 {
		t.Fatalf("two of three required contexts are absent; the guard must refuse, got exit %d\n%s", code, out)
	}
	for _, missing := range []string{"lint has not reported yet", "license/cla has not reported yet"} {
		if !strings.Contains(out, missing) {
			t.Errorf("the refusal must name every missing required context (%q); output:\n%s", missing, out)
		}
	}
	if strings.Contains(out, "ci-required has not reported yet") {
		t.Errorf("ci-required reported SUCCESS and must not be named as missing; output:\n%s", out)
	}
}

// --check returns before the guard, so the report is the only place a reader
// running it learns that the required check is absent. It has to say so, or
// `--check` on the #5709 shape reads as "0 failed" and nothing else.
func TestReportMarksARequiredCheckThatHasNotReported(t *testing.T) {
	stub := mergeGuardGhStub(t, rulesetsActive, requiresCIRequired,
		prRollup(`{"name":"db-tests (1)","status":"IN_PROGRESS"}`))
	script := filepath.Join(repoRoot(t), "scripts", "dev", "merge-as-owner.sh")
	cmd := exec.Command("bash", script, "--pr=1", "--check")
	cmd.Env = append(os.Environ(), "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	outB, err := cmd.CombinedOutput()
	out := string(outB)
	if err != nil {
		t.Fatalf("--check merges nothing and must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NOT REPORTED (REQUIRED): ci-required") {
		t.Errorf("the report must mark a required context that has not reported; output:\n%s", out)
	}
}
