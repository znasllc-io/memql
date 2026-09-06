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
func mergeGuardGhStub(t *testing.T, rulesetsJSON, rulesetJSON, prJSON string) string {
	t.Helper()
	dir := t.TempDir()
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

func prRollup(checks string) string {
	return `{
  "state":"OPEN","title":"t","author":{"login":"znas-io"},
  "mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","reviewDecision":null,
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
