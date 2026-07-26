package dev

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stale_claims_test.go -- memql#2834.
//
// stale-claims.sh is the first script in scripts/dev/ that CAN write to GitHub,
// so the load-bearing property is not "it is read-only" (its sibling's) but
// "it writes ONLY under --apply, and only to closed issues".
//
// Both halves are checked BEHAVIOURALLY: the script is executed against a stub
// `gh` that records every invocation and refuses anything outside a read
// allowlist. That is what the #2873 review established as the only durable
// approach -- three earlier rounds of a static source scan on the sibling script
// were defeated three different ways, the last simply by quoting a token
// (`"gh" pr merge`). Out-parsing shell quoting, eval and indirection is not a
// winnable game, so this does not try; it observes what bash actually ran.
//
// The stub's screener is an ALLOWLIST, which is the other #2873 lesson. Its
// first version enumerated write flags and pflag's attached forms walked past
// it -- `-XPOST`, `--method=POST`, `--field=x`, `--raw-field=x` were all
// accepted as read-only. Every argument must now match a known-safe form and
// anything unrecognised is refused by default.

const staleClaimsScript = "stale-claims.sh"

// ghStubConfig is the canned API data the stub serves.
type staleClaimsStub struct {
	issueList string // stdout for `gh issue list`
	listFail  bool
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeStaleClaimsStub emits a `gh` that serves canned reads, records every
// call, and REFUSES anything outside the allowlist.
//
// `issue edit --remove-label` is the one mutation the script may make. It is
// admitted here -- recorded with an EDIT marker so a test can assert on it --
// precisely so the tests can tell "the script did not write" from "the stub
// blocked a write it attempted". A stub that refused it outright would make
// those two indistinguishable, which is the failure mode that matters most for
// a script whose default must be inert.
func writeStaleClaimsStub(t *testing.T, cfg staleClaimsStub) (dir, logPath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "calls.log")

	fail := "exit 0"
	if cfg.listFail {
		fail = "exit 1"
	}

	stub := `#!/usr/bin/env bash
printf '%s\n' "$*" >> ` + shQuote(logPath) + `

# ALLOWLIST, not a blocklist (memql#2873's lesson): every argument must match a
# known-safe form, and anything unrecognised is refused by default. Enumerating
# write spellings loses to pflag's attached forms -- -XPOST, --method=POST,
# --field=x and --raw-field=x all slipped past an enumerating screener.
case "$1 $2" in
  "--version "*) echo 'gh version 2.45.0'; exit 0 ;;
  "auth status") exit 0 ;;
  "issue list")
    for a in "$@"; do
      case "$a" in
        issue|list|-R|--state|--limit|--json|--jq) ;;
        all|100|` + shQuote("znasllc-io/memql") + `) ;;
        number,state,labels,updatedAt,title) ;;
        .*|'"'"'*'"'"') ;;
        -*) printf 'FORBIDDEN %s (unrecognised issue-list flag %s)\n' "$*" "$a" >> ` + shQuote(logPath) + `
            echo "stub gh: refusing $a" >&2; exit 1 ;;
      esac
    done
    printf '%s' ` + shQuote(cfg.issueList) + `; ` + fail + ` ;;
  "issue edit")
    # The ONE tolerated mutation. Recorded distinctly so a test can assert the
    # default run made none, and that --apply made exactly the right ones.
    seen_remove=0
    for a in "$@"; do
      case "$a" in
        --remove-label) seen_remove=1 ;;
        --add-label|--add-assignee|--body|--title|--milestone|--remove-assignee)
          printf 'FORBIDDEN %s (unexpected edit flag %s)\n' "$*" "$a" >> ` + shQuote(logPath) + `
          echo "stub gh: refusing $a" >&2; exit 1 ;;
      esac
    done
    if [ "$seen_remove" -eq 1 ]; then
      printf 'EDIT %s\n' "$*" >> ` + shQuote(logPath) + `
      exit 0
    fi
    printf 'FORBIDDEN %s (issue edit with no --remove-label)\n' "$*" >> ` + shQuote(logPath) + `
    exit 1 ;;
esac

printf 'FORBIDDEN %s (non-read invocation)\n' "$*" >> ` + shQuote(logPath) + `
echo "stub gh: refusing non-read invocation: $*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub gh: %v", err)
	}
	return dir, logPath
}

type staleClaimsResult struct {
	stdout, stderr string
	code           int
	calls          []string
}

func (r staleClaimsResult) edits() []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "EDIT ") {
			out = append(out, strings.TrimPrefix(c, "EDIT "))
		}
	}
	return out
}

func (r staleClaimsResult) forbidden() []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "FORBIDDEN ") {
			out = append(out, strings.TrimPrefix(c, "FORBIDDEN "))
		}
	}
	return out
}

// staleClaimsEnv passes only PATH and HOME plus what a case sets.
//
// os.Environ() is deliberately NOT inherited -- the #2873 review found that
// exporting the sibling's documented IDLE_MINUTES broke its whole suite from
// outside. IDLE_HOURS and REPO are the same hazard here.
func staleClaimsEnv(stubDir string, extra ...string) []string {
	return append([]string{
		"PATH=" + stubDir + ":" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}, extra...)
}

func runStaleClaims(t *testing.T, cfg staleClaimsStub, args []string, env ...string) staleClaimsResult {
	t.Helper()
	dir, logPath := writeStaleClaimsStub(t, cfg)

	cmd := exec.Command("bash", append([]string{staleClaimsScript}, args...)...)
	cmd.Env = staleClaimsEnv(dir, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", staleClaimsScript, err)
	}

	raw, _ := os.ReadFile(logPath)
	var calls []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			calls = append(calls, line)
		}
	}
	return staleClaimsResult{stdout: stdout.String(), stderr: stderr.String(), code: code, calls: calls}
}

// tsv builds one issue-list row.
func tsv(num, state, labels, updated, title string) string {
	return strings.Join([]string{num, state, labels, updated, title}, "\t") + "\n"
}

// mixedClaims covers one issue per branch the sweep decision can take: the
// CLOSED issue that IS swept, plus all three OPEN shapes that must NOT be --
// FRESH (under IDLE_HOURS), SUSPECT (over it), and UNKNOWN (unusable
// timestamp).
//
// The SUSPECT row is the one that carries the safety property. An earlier
// version of this fixture had only a single OPEN row stamped 9999999999,
// intending "fresh"; because idle_hours treats a future timestamp as clock skew
// that row landed in UNKNOWN, so NO row ever reached the SUSPECT branch and the
// suite stayed GREEN against a script that swept open SUSPECT issues under
// --apply -- the exact regression these two tests exist to catch. Hence the
// branch-reached assertions in mustReachEveryOpenBranch: without them a later
// fixture edit can silently re-introduce the same vacuum.
func mixedClaims() staleClaimsStub {
	fresh := strconv.FormatInt(time.Now().Add(-30*time.Minute).Unix(), 10)
	return staleClaimsStub{issueList: "" +
		tsv("2801", "CLOSED", "claimed:s4b9e2,bug", "1000000000", "already closed, claim left behind") +
		tsv("2999", "OPEN", "claimed:s7c2e9", fresh, "live work, claimed 30min ago") +
		tsv("2998", "OPEN", "claimed:s6f08e5", "1000000000", "open and long idle -- may still be live") +
		tsv("2997", "OPEN", "claimed:sQ", "notanumber", "open, timestamp unusable"),
	}
}

// mustReachEveryOpenBranch fails unless the report actually classified an open
// claim into each of the three non-sweepable states. A fixture that does not
// reach a branch cannot defend it.
func mustReachEveryOpenBranch(t *testing.T, stdout string) {
	t.Helper()
	for _, want := range []struct{ issue, state string }{
		{"2999", "FRESH"},
		{"2998", "SUSPECT"},
		{"2997", "UNKNOWN"},
	} {
		line := ""
		for _, l := range strings.Split(stdout, "\n") {
			if strings.Contains(l, "#"+want.issue) {
				line = l
				break
			}
		}
		if line == "" {
			t.Errorf("issue #%s absent from the report; the %s branch is untested.\ngot:\n%s",
				want.issue, want.state, stdout)
			continue
		}
		if !strings.Contains(line, want.state) {
			t.Errorf("issue #%s classified as %q, wanted %s -- the %s branch is not being exercised, "+
				"so nothing here proves a claim in that state is left alone.", want.issue, strings.TrimSpace(line), want.state, want.state)
		}
	}
}

// TestStaleClaims_DefaultRunMakesNoMutation is THE property. A reporter that
// writes by default cannot be put on a loop or in CI.
func TestStaleClaims_DefaultRunMakesNoMutation(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), nil)

	if res.code != 0 {
		t.Fatalf("expected a clean run, got exit %d\nstderr: %s", res.code, res.stderr)
	}
	if len(res.calls) == 0 {
		t.Fatal("the stub recorded no gh calls at all; this test would pass vacuously")
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("the DEFAULT run mutated GitHub:\n  %s\nWithout --apply it must make no write at "+
			"all -- that is what makes it safe to run unattended (memql#2834).", strings.Join(got, "\n  "))
	}
	if got := res.forbidden(); len(got) > 0 {
		t.Errorf("the script invoked gh in a way the stub refuses:\n  %s", strings.Join(got, "\n  "))
	}
	// And it must still have REPORTED the abandoned claim, or "no mutation" is
	// satisfied by a script that does nothing.
	if !strings.Contains(res.stdout, "2801") || !strings.Contains(res.stdout, "ABANDONED") {
		t.Errorf("the default run did not report the abandoned claim on the CLOSED issue.\ngot:\n%s", res.stdout)
	}
	mustReachEveryOpenBranch(t, res.stdout)
}

// TestStaleClaims_ApplySweepsOnlyClosedIssues is the other half: --apply must
// remove the closed issue's claim and MUST NOT touch the open one.
func TestStaleClaims_ApplySweepsOnlyClosedIssues(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), []string{"--apply"})

	if res.code != 0 {
		t.Fatalf("expected a clean run, got exit %d\nstderr: %s", res.code, res.stderr)
	}
	edits := res.edits()
	if len(edits) != 1 {
		t.Fatalf("expected exactly ONE label removal, got %d:\n  %s\nOnly the CLOSED issue's claim "+
			"may be swept -- a claim on an open issue may be live work.", len(edits), strings.Join(edits, "\n  "))
	}
	if !strings.Contains(edits[0], "2801") || !strings.Contains(edits[0], "claimed:s4b9e2") {
		t.Errorf("swept the wrong thing: %q", edits[0])
	}
	// Every OPEN issue by number, against every recorded edit -- not just
	// edits[0]. The count check above would already have caught a second write,
	// but naming the numbers is what makes the failure say WHICH state leaked.
	for _, open := range []string{"2999", "2998", "2997"} {
		for _, e := range edits {
			if strings.Contains(e, open) {
				t.Errorf("swept a claim on OPEN issue #%s: %q\nOnly CLOSED issues may be swept: a claim "+
					"on an open issue may be live work, and telling a live worker from a dead one is the "+
					"unsettled half of memql#2834.", open, e)
			}
		}
	}
	if got := res.forbidden(); len(got) > 0 {
		t.Errorf("forbidden call under --apply:\n  %s", strings.Join(got, "\n  "))
	}
	mustReachEveryOpenBranch(t, res.stdout)
}

// TestStaleClaims_ParkedOpenIssueIsNotAFinding -- `parked` means a human already
// recorded why the issue is not moving, so the claim is not what hides it.
func TestStaleClaims_ParkedOpenIssueIsNotAFinding(t *testing.T) {
	cfg := staleClaimsStub{issueList: tsv("2870", "OPEN", "claimed:s7c2e9,parked", "1000000000", "parked with a diagnosis")}
	res := runStaleClaims(t, cfg, []string{"--apply"})

	if !strings.Contains(res.stdout, "PARKED") {
		t.Errorf("a parked open issue was not classified PARKED.\ngot:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "SUSPECT") {
		t.Errorf("a parked issue was reported as SUSPECT; a human already recorded why it is not moving.\ngot:\n%s", res.stdout)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("swept a claim on an OPEN parked issue: %v", got)
	}
}

// TestStaleClaims_ClockSkewIsUnknownNotAFinding -- an updatedAt in the future
// must never manufacture a finding, and must never be swept.
func TestStaleClaims_ClockSkewIsUnknownNotAFinding(t *testing.T) {
	cfg := staleClaimsStub{issueList: "" +
		tsv("3001", "OPEN", "claimed:sX", "99999999999", "future timestamp") +
		tsv("3002", "OPEN", "claimed:sY", "notanumber", "unparseable timestamp"),
	}
	res := runStaleClaims(t, cfg, []string{"--apply"})

	if strings.Contains(res.stdout, "SUSPECT") {
		t.Errorf("clock skew or an unparseable timestamp produced a SUSPECT finding.\ngot:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "UNKNOWN") {
		t.Errorf("expected UNKNOWN for both rows.\ngot:\n%s", res.stdout)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("swept an UNKNOWN claim: %v", got)
	}
}

// TestStaleClaims_IdleThresholdGatesTheOpenReport pins that IDLE_HOURS is the
// knob it documents, using a hermetic env so an exported IDLE_HOURS cannot
// change the answer.
func TestStaleClaims_IdleThresholdGatesTheOpenReport(t *testing.T) {
	// updatedAt = epoch 1000000000 (2001), so the age is enormous either way;
	// the threshold is what decides.
	cfg := staleClaimsStub{issueList: tsv("3100", "OPEN", "claimed:sZ", "1000000000", "old open claim")}

	suspect := runStaleClaims(t, cfg, nil, "IDLE_HOURS=4")
	if !strings.Contains(suspect.stdout, "SUSPECT") {
		t.Errorf("an open claim older than IDLE_HOURS was not SUSPECT.\ngot:\n%s", suspect.stdout)
	}

	// A threshold above the age must classify it FRESH. 9999999 hours is ~1141
	// years, comfortably past any real age.
	fresh := runStaleClaims(t, cfg, nil, "IDLE_HOURS=9999999")
	if strings.Contains(fresh.stdout, "SUSPECT") {
		t.Errorf("IDLE_HOURS did not gate the report.\ngot:\n%s", fresh.stdout)
	}
}

// TestStaleClaims_BadThresholdIsAParameterError -- honest exit codes.
func TestStaleClaims_BadThresholdIsAParameterError(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), nil, "IDLE_HOURS=4h")
	if res.code != 2 {
		t.Errorf("IDLE_HOURS=4h gave exit %d, want 2 (bad parameter)\nstderr: %s", res.code, res.stderr)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("mutated GitHub while rejecting a bad parameter: %v", got)
	}
}

// TestStaleClaims_ListFailureIsLoud -- a failed list must not read as
// "no stale claims".
func TestStaleClaims_ListFailureIsLoud(t *testing.T) {
	cfg := mixedClaims()
	cfg.listFail = true
	res := runStaleClaims(t, cfg, []string{"--apply"})

	if res.code != 4 {
		t.Errorf("a failed issue list gave exit %d, want 4 (gh unusable)\nstdout: %s", res.code, res.stdout)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("mutated GitHub after a failed list: %v", got)
	}
}

// TestStaleClaims_UnrecognisedArgIsRejected -- and rejecting it must not run the
// report, so a typo like `--aply` can never silently mean "report only" OR
// silently mean "apply".
func TestStaleClaims_UnrecognisedArgIsRejected(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), []string{"--aply"})
	if res.code != 2 {
		t.Errorf("an unrecognised argument gave exit %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("a typo'd flag mutated GitHub: %v", got)
	}
}

// TestStaleClaims_HelpDoesNotHitTheAPI -- `--help` must be free. The sibling
// script's `main` never read $@, so `--help` ran a full live API sweep.
func TestStaleClaims_HelpDoesNotHitTheAPI(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), []string{"--help"})
	if res.code != 0 {
		t.Errorf("--help gave exit %d, want 0", res.code)
	}
	if !strings.Contains(res.stdout, "READ-ONLY") {
		t.Errorf("--help did not print usage.\ngot:\n%s", res.stdout)
	}
	for _, c := range res.calls {
		if strings.HasPrefix(c, "issue list") || strings.HasPrefix(c, "EDIT ") {
			t.Errorf("--help hit the API: %q", c)
		}
	}
}

// TestStaleClaims_MultipleClaimsOnOneIssueAreEachHandled -- the race the loop
// is supposed to prevent leaves TWO claim labels on one issue, which is the
// state a sweeper most needs to get right.
func TestStaleClaims_MultipleClaimsOnOneIssueAreEachHandled(t *testing.T) {
	cfg := staleClaimsStub{issueList: tsv("3200", "CLOSED", "claimed:sA,bug,claimed:sB", "1000000000", "two claims")}
	res := runStaleClaims(t, cfg, []string{"--apply"})

	edits := res.edits()
	if len(edits) != 2 {
		t.Fatalf("expected TWO removals for two claim labels, got %d:\n  %s", len(edits), strings.Join(edits, "\n  "))
	}
	joined := strings.Join(edits, " ")
	for _, want := range []string{"claimed:sA", "claimed:sB"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s was not swept: %v", want, edits)
		}
	}
	if strings.Contains(joined, "bug") {
		t.Errorf("swept a non-claim label: %v", edits)
	}
}
