package dev

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stale_claims_test.go -- memql#2834.
//
// stale-claims.sh is the first script in scripts/dev/ that CAN write to GitHub,
// so the load-bearing property is not "it is read-only" (its sibling's) but
// "it writes ONLY under --apply, and only to cold claims on closed issues".
//
// Both halves are checked BEHAVIOURALLY: the script is executed against a stub
// `gh` that records every invocation and refuses anything outside a read
// allowlist. That is what the #2873 review established as the only durable
// approach -- three earlier rounds of a static source scan on the sibling
// script were defeated three different ways, the last simply by quoting a
// token (`"gh" pr merge`). Out-parsing shell quoting, eval and indirection is
// not a winnable game, so this does not try; it observes what bash actually
// ran.
//
// THE STUB'S SCREENER IS AN ALLOWLIST IN EVERY ARM, AND THERE IS A META-TEST.
//
// That is the #2873 lesson, and the first cut of this file re-broke it in the
// one arm where it mattered. `issue edit` -- the only branch that permits a
// write -- enumerated six flags to refuse and had no `-*)` default, so anything
// unenumerated was silently accepted. Review fired the sibling's must-refuse
// corpus at it and 7 of 14 spellings got through: every pflag attached form
// (`--add-label=x`, `--title=x`, `--body=x`, `--add-assignee=x`) plus
// `--body-file`, `--add-project` and `--remove-project`. Patching the real
// script to smuggle any of those alongside the legitimate `--remove-label`
// left the entire suite green.
//
// So: every arm now ends in `-*) FORBIDDEN`, and TestStaleClaims_StubRefusesWrites
// fires that corpus at the stub directly. A guard this suite depends on needs
// its own guard, or "the tests pass" only means "nobody tried that spelling".

const staleClaimsScript = "stale-claims.sh"

// stubIssue is one issue the fake `gh` serves.
type stubIssue struct {
	num   int
	state string
	title string
	// claimedAt is what the timeline's `labeled` event reports for this
	// issue's claim. noEvent means "no readable event" -> the script must
	// classify UNKNOWN and never sweep.
	claimedAt time.Time
	noEvent   bool
	// closedAt is the issue's close time, which is what the sweep gate
	// measures. It is SEPARATE from claimedAt on purpose: gating on claim age
	// instead only shields a session whose entire lifetime is under
	// IDLE_HOURS, so a long-running session that closed an issue one second
	// ago got no grace at all. A fixture with no close-time field cannot
	// express that difference, and the first one did not have it.
	closedAt time.Time
	// parked marks the issue as carrying the `parked` label.
	parked bool
	// extraLabels ride alongside on `gh issue view`.
	extraLabels []string
}

// staleClaimsStub is the canned API surface the stub serves.
type staleClaimsStub struct {
	// labelIssues maps a claimed:* label to the issues carrying it. The stub
	// derives the repo's label list from these keys, so a fixture cannot
	// accidentally describe a label with no issues or an issue with no label.
	labelIssues map[string][]stubIssue
	labelFail   bool
	editFail    bool
	// padLabels adds this many NON-claim labels to the label list. The page-cap
	// warning measures the RAW page length, so padding exercises it without
	// inventing hundreds of claims (and without hundreds of issue-list calls).
	padLabels int
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ghStubSource is the fake `gh`. Every arm is default-deny: an argument that is
// not positively recognised is logged FORBIDDEN and refused, including in
// `issue edit`.
//
// `issue edit --remove-label` is the one mutation the script may make. It is
// ADMITTED here -- recorded with an EDIT marker -- precisely so the tests can
// tell "the script did not write" from "the stub blocked a write it attempted".
// A stub that refused it outright would make those indistinguishable, which is
// the failure mode that matters most for a script whose default must be inert.
const ghStubSource = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"

deny() {
  printf 'FORBIDDEN %s (%s)\n' "$*" "${DENY_REASON:-non-read invocation}" >> "$STUB_LOG"
  echo "stub gh: refusing: $*" >&2
  exit 1
}

# jq_arg extracts the value of --jq. THE STUB RUNS THE REAL FILTER.
#
# The first cut served canned bytes and threw the filter away, so not one jq
# expression in the script was exercised: a typo'd startswith, a wrong event
# name, a swapped @tsv column order and a last-vs-first were all TOTAL
# production failures that left the suite green. That is the same shape as the
# --arg bug -- a stub weaker than the real tool -- and refusing --arg only
# closed one spelling of it. Running real jq over canned JSON closes the class.
jq_arg() {
  local prev=""
  for a in "$@"; do
    [ "$prev" = "--jq" ] && { printf '%s' "$a"; return 0; }
    prev="$a"
  done
  return 1
}

run_jq() {
  local file="$1"; shift
  local expr
  expr=$(jq_arg "$@") || { DENY_REASON="no --jq filter supplied" deny "$@"; }
  [ -f "$file" ] || return 0
  jq -r "$expr" "$file" 2>>"$STUB_LOG" || {
    printf 'JQERROR %s\n' "$expr" >> "$STUB_LOG"
    return 1
  }
}

case "$1 $2" in
  "--version "*) echo 'gh version 2.45.0'; exit 0 ;;
  "auth status") exit 0 ;;

  "label list")
    [ -f "$STUB_DIR/labelfail" ] && exit 1
    for a in "$@"; do
      case "$a" in
        label|list|-R|--limit|--json|--jq) ;;
        -*) DENY_REASON="unrecognised label-list flag $a" deny "$@" ;;
        *) ;;
      esac
    done
    run_jq "$STUB_DIR/labels.json" "$@"
    exit $? ;;

  "issue list")
    want=""
    prev=""
    for a in "$@"; do
      [ "$prev" = "--label" ] && want="$a"
      prev="$a"
      case "$a" in
        issue|list|-R|--label|--state|--limit|--json|--jq) ;;
        -*) DENY_REASON="unrecognised issue-list flag $a" deny "$@" ;;
        *) ;;
      esac
    done
    run_jq "$STUB_DIR/issues-$want.json" "$@"
    exit $? ;;

  "issue view")
    # Drains stdin ON PURPOSE. Real gh does not, but a stub that never reads
    # stdin makes the script's </dev/null unobservable -- stripping it left the
    # suite green. With this, a gh call inside a read loop that forgets
    # </dev/null eats the loop's input and the missing rows are visible.
    cat >/dev/null
    num="$3"
    for a in "$@"; do
      case "$a" in
        issue|view|-R|--json|--jq) ;;
        -*) DENY_REASON="unrecognised issue-view flag $a" deny "$@" ;;
        *) ;;
      esac
    done
    run_jq "$STUB_DIR/view-$num.json" "$@"
    exit $? ;;

  "api repos"*)
    num=$(printf '%s' "$2" | sed -n 's#.*/issues/\([0-9]*\)/timeline.*#\1#p')
    for a in "$@"; do
      case "$a" in
        api|--jq) ;;
        -X*|-F*|-f*|--method*|--field*|--raw-field*|--input*)
          DENY_REASON="write-capable api flag $a" deny "$@" ;;
        -*) DENY_REASON="unrecognised api flag $a" deny "$@" ;;
        *) ;;
      esac
    done
    run_jq "$STUB_DIR/timeline-$num.json" "$@"
    exit $? ;;

  "issue edit")
    # Also drains stdin, for the same reason: the sweep runs inside the inner
    # read loop, so a missing </dev/null there loses every row after the first.
    cat >/dev/null
    # The ONE tolerated mutation -- and still default-deny. The first cut
    # enumerated flags to refuse and had no catch-all, so every pflag attached
    # form walked straight through (memql#2873, re-broken and re-fixed).
    seen_remove=0
    for a in "$@"; do
      case "$a" in
        issue|edit|-R|*/*) ;;
        [0-9]*) ;;
        --remove-label) seen_remove=1 ;;
        claimed:*) ;;
        -*) DENY_REASON="unexpected edit flag $a" deny "$@" ;;
        *) DENY_REASON="unexpected edit operand $a" deny "$@" ;;
      esac
    done
    if [ "$seen_remove" -eq 1 ]; then
      [ -f "$STUB_DIR/editfail" ] && { echo "stub gh: edit rejected" >&2; exit 1; }
      printf 'EDIT %s\n' "$*" >> "$STUB_LOG"
      exit 0
    fi
    DENY_REASON="issue edit with no --remove-label" deny "$@" ;;
esac

deny "$@"
`

// writeStaleClaimsStub materialises the stub and its canned data.
func writeStaleClaimsStub(t *testing.T, cfg staleClaimsStub) (dir, logPath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "calls.log")

	// Canned data is real JSON in the shapes gh actually returns, because the
	// stub runs the script's real --jq filters over it.
	labels := []map[string]any{}
	for label, issues := range cfg.labelIssues {
		labels = append(labels, map[string]any{"name": label})

		issueRows := []map[string]any{}
		for _, is := range issues {
			row := map[string]any{"number": is.num, "state": is.state, "title": is.title}
			if is.closedAt.IsZero() {
				row["closedAt"] = nil // gh emits null for an open issue
			} else {
				row["closedAt"] = is.closedAt.UTC().Format(time.RFC3339)
			}
			issueRows = append(issueRows, row)

			timeline := []map[string]any{
				// A decoy: same label name, wrong event type.
				{"event": "mentioned", "label": map[string]any{"name": label},
					"created_at": is.claimedAt.UTC().Format(time.RFC3339)},
				// A decoy: labeled, but a DIFFERENT label.
				{"event": "labeled", "label": map[string]any{"name": "bug"},
					"created_at": time.Now().UTC().Format(time.RFC3339)},
			}
			if !is.noEvent {
				// An earlier application of the SAME label, so `last` vs
				// `first` is observable: first would report this older one.
				timeline = append(timeline, map[string]any{
					"event": "labeled", "label": map[string]any{"name": label},
					"created_at": is.claimedAt.Add(-500 * time.Hour).UTC().Format(time.RFC3339),
				}, map[string]any{
					"event": "labeled", "label": map[string]any{"name": label},
					"created_at": is.claimedAt.UTC().Format(time.RFC3339),
				})
			}
			writeJSON(t, filepath.Join(dir, fmt.Sprintf("timeline-%d.json", is.num)), timeline)

			viewLabels := []map[string]any{{"name": "enhancement"}}
			for _, l := range is.extraLabels {
				viewLabels = append(viewLabels, map[string]any{"name": l})
			}
			if is.parked {
				viewLabels = append(viewLabels, map[string]any{"name": "parked"})
			}
			writeJSON(t, filepath.Join(dir, fmt.Sprintf("view-%d.json", is.num)),
				map[string]any{"labels": viewLabels})
		}
		writeJSON(t, filepath.Join(dir, "issues-"+label+".json"), issueRows)
	}
	for i := 0; i < cfg.padLabels; i++ {
		labels = append(labels, map[string]any{"name": fmt.Sprintf("filler-%d", i)})
	}
	writeJSON(t, filepath.Join(dir, "labels.json"), labels)
	if cfg.labelFail {
		write(t, filepath.Join(dir, "labelfail"), "1")
	}
	if cfg.editFail {
		write(t, filepath.Join(dir, "editfail"), "1")
	}
	write(t, filepath.Join(dir, "gh"), ghStubSource)
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o755); err != nil {
		t.Fatalf("chmod stub gh: %v", err)
	}
	return dir, logPath
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	write(t, path, string(b))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type staleClaimsResult struct {
	stdout, stderr string
	code           int
	calls          []string
}

func (r staleClaimsResult) edits() []string     { return r.marked("EDIT ") }
func (r staleClaimsResult) forbidden() []string { return r.marked("FORBIDDEN ") }

func (r staleClaimsResult) marked(prefix string) []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, strings.TrimPrefix(c, prefix))
		}
	}
	return out
}

// rowFor returns the report line for an issue, or "" when absent.
func (r staleClaimsResult) rowFor(issue int) string {
	for _, l := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), fmt.Sprintf("#%d ", issue)) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// staleClaimsEnv passes only PATH and HOME plus what a case sets.
//
// os.Environ() is deliberately NOT inherited -- the #2873 review found that
// exporting the sibling's documented IDLE_MINUTES broke its whole suite from
// outside. IDLE_HOURS and REPO are the same hazard here.
func staleClaimsEnv(stubDir, logPath string, extra ...string) []string {
	return append([]string{
		"PATH=" + stubDir + ":" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"STUB_DIR=" + stubDir,
		"STUB_LOG=" + logPath,
	}, extra...)
}

func runStaleClaims(t *testing.T, cfg staleClaimsStub, args []string, env ...string) staleClaimsResult {
	t.Helper()
	dir, logPath := writeStaleClaimsStub(t, cfg)

	cmd := exec.Command("bash", append([]string{staleClaimsScript}, args...)...)
	cmd.Env = staleClaimsEnv(dir, logPath, env...)
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

func hoursAgo(h int) time.Time { return time.Now().Add(-time.Duration(h) * time.Hour) }

// mixedClaims covers one issue per branch the sweep decision can take: the cold
// CLOSED claim that IS swept, the freshly-CLOSED one that is not (the live
// session's shutdown window), and all three OPEN shapes -- FRESH, SUSPECT,
// UNKNOWN -- plus an unrecognised STATE.
//
// Every branch is represented on purpose, and mustReachEveryBranch asserts each
// was actually entered. An earlier fixture had a single OPEN row stamped with a
// FUTURE timestamp, intending "fresh"; because that classifies as clock skew it
// landed in UNKNOWN, so no row ever reached SUSPECT and the suite stayed GREEN
// against a script that swept open SUSPECT issues under --apply -- the exact
// regression these tests exist to catch. Un-entered branches defend nothing.
func mixedClaims() staleClaimsStub {
	return staleClaimsStub{labelIssues: map[string][]stubIssue{
		"claimed:s4b9e2": {
			{num: 2801, state: "CLOSED", title: "closed long ago, claim left behind",
				claimedAt: hoursAgo(100), closedAt: hoursAgo(90)},
			// THE DISCRIMINATING ROW. Claimed 9h ago -- well past IDLE_HOURS --
			// but closed ten seconds ago: a long-running session between "merge
			// closed the issue" and "drop the claim". A gate on CLAIM age calls
			// this ABANDONED and sweeps it; only a gate on CLOSE time does not.
			{num: 2802, state: "CLOSED", title: "long session, just closed",
				claimedAt: hoursAgo(9), closedAt: time.Now().Add(-10 * time.Second)},
			// Closed, but with no readable closedAt -- the gate cannot be
			// evaluated, so default-deny applies.
			{num: 2803, state: "CLOSED", title: "closed, no readable closedAt",
				claimedAt: hoursAgo(100)},
		},
		"claimed:s7c2e9": {
			{num: 2999, state: "OPEN", title: "live work, claimed 30min ago", claimedAt: time.Now().Add(-30 * time.Minute)},
			{num: 2998, state: "OPEN", title: "open and long idle -- may still be live", claimedAt: hoursAgo(100)},
			{num: 2997, state: "OPEN", title: "open, no readable labeled event", noEvent: true},
			{num: 2996, state: "LOCKED", title: "unrecognised state", claimedAt: hoursAgo(100)},
		},
	}}
}

// branchExpectations is the classification each fixture row must receive.
var branchExpectations = []struct {
	issue int
	state string
}{
	{2801, "ABANDONED"},
	{2802, "CLOSING"},
	{2803, "UNKNOWN"},
	{2999, "FRESH"},
	{2998, "SUSPECT"},
	{2997, "UNKNOWN"},
	{2996, "UNKNOWN"},
}

// titleOf returns the fixture title for an issue, for the column assertion.
func titleOf(issue int) string {
	for _, issues := range mixedClaims().labelIssues {
		for _, is := range issues {
			if is.num == issue {
				return is.title
			}
		}
	}
	return ""
}

// mustReachEveryBranch fails unless the report classified each fixture row into
// the state it exists to represent. A branch the fixture never enters cannot be
// defended by any assertion about it.
func mustReachEveryBranch(t *testing.T, res staleClaimsResult) {
	t.Helper()
	for _, want := range branchExpectations {
		line := res.rowFor(want.issue)
		if line == "" {
			t.Errorf("issue #%d absent from the report; the %s branch is untested.\ngot:\n%s",
				want.issue, want.state, res.stdout)
			continue
		}
		// The TITLE column must survive too. An empty middle TSV field
		// collapses (tab is IFS whitespace), which shifted every later column
		// left and blanked the title -- visible only on a live run because no
		// fixture assertion looked at it.
		if !strings.Contains(line, titleOf(want.issue)) {
			t.Errorf("issue #%d's row lost its title: %q\nwant it to contain %q. An empty middle "+
				"TSV field collapses under IFS=tab and shifts the later columns left.",
				want.issue, line, titleOf(want.issue))
		}
		if !strings.Contains(line, want.state) {
			t.Errorf("issue #%d classified as %q, wanted %s -- that branch is not being exercised, "+
				"so nothing here proves a claim in that state is handled correctly.",
				want.issue, line, want.state)
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
		t.Errorf("the default run did not report the abandoned claim.\ngot:\n%s", res.stdout)
	}
	mustReachEveryBranch(t, res)
}

// TestStaleClaims_ApplySweepsOnlyColdClosedClaims is the other half: --apply
// must remove the cold closed claim and nothing else.
func TestStaleClaims_ApplySweepsOnlyColdClosedClaims(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), []string{"--apply"})

	if res.code != 0 {
		t.Fatalf("expected a clean run, got exit %d\nstderr: %s", res.code, res.stderr)
	}
	edits := res.edits()
	if len(edits) != 1 {
		t.Fatalf("expected exactly ONE label removal, got %d:\n  %s\nOnly a COLD claim on a CLOSED "+
			"issue may be swept.", len(edits), strings.Join(edits, "\n  "))
	}
	if !strings.Contains(edits[0], "2801") || !strings.Contains(edits[0], "claimed:s4b9e2") {
		t.Errorf("swept the wrong thing: %q", edits[0])
	}
	// Every non-sweepable issue by number, against every recorded edit.
	for _, keep := range []struct {
		issue int
		why   string
	}{
		{2999, "OPEN and fresh"},
		{2998, "OPEN and idle -- may still be live work, the unsettled half of memql#2834"},
		{2997, "OPEN with an unknown claim age"},
		{2996, "an unrecognised state, which must never be swept"},
		{2802, "CLOSED ten seconds ago -- the window between a merge closing the issue and the session dropping its claim, which a CLAIM-age gate misses entirely"},
		{2803, "CLOSED with no readable closedAt, so the gate cannot be evaluated"},
	} {
		for _, e := range edits {
			if strings.Contains(e, fmt.Sprint(keep.issue)) {
				t.Errorf("swept the claim on #%d (%s): %q", keep.issue, keep.why, e)
			}
		}
	}
	if got := res.forbidden(); len(got) > 0 {
		t.Errorf("forbidden call under --apply:\n  %s", strings.Join(got, "\n  "))
	}
	mustReachEveryBranch(t, res)
}

// TestStaleClaims_StubRefusesWrites guards the guard.
//
// Every other test in this file is only as strong as the stub's screener, and
// review proved that is not a formality: the first cut's `issue edit` arm was
// an enumerating blocklist, 7 of these 14 spellings were accepted as harmless,
// and smuggling any of them into the real script's removal call left the whole
// suite green. If this test ever fails, the other results are worthless rather
// than merely wrong.
func TestStaleClaims_StubRefusesWrites(t *testing.T) {
	dir, logPath := writeStaleClaimsStub(t, mixedClaims())

	mustRefuse := [][]string{
		{"issue", "edit", "7", "--add-label", "pwned"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--add-label=pwned"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--title=pwned"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--body=pwned"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--add-assignee=someone"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--body-file", "/etc/hostname"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--add-project", "Roadmap"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--remove-project", "Roadmap"},
		{"issue", "edit", "7", "--remove-label", "claimed:x", "--milestone=m1"},
		{"issue", "close", "7"},
		{"issue", "comment", "7", "--body", "hi"},
		{"api", "-XPOST", "repos/o/r/issues/7/labels"},
		{"api", "--method=POST", "repos/o/r/issues/7/labels"},
		{"api", "repos/o/r/issues/7/timeline", "--field=x"},
		{"api", "repos/o/r/issues/7/timeline", "--raw-field=x"},
		{"pr", "merge", "7", "--squash"},
		{"label", "create", "evil"},
		{"label", "delete", "claimed:x"},
		{"issue", "list", "-R", "o/r", "--label", "claimed:x", "--jq", ".", "--template", "{{.}}"},
		// Not writes -- these guard against the stub being MORE PERMISSIVE than
		// real gh, which is its own way of testing nothing. Neither `gh api` nor
		// `gh issue view` accepts --arg ("accepts 1 arg(s), received 4"), and the
		// first cut of the script used it on both. Every lookup failed in
		// production while the suite stayed green, because the stub took it.
		{"api", "repos/o/r/issues/7/timeline", "--jq", ".", "--arg", "l", "x"},
		{"issue", "view", "7", "-R", "o/r", "--json", "labels", "--jq", ".", "--arg", "w", "parked"},
	}

	for _, args := range mustRefuse {
		cmd := exec.Command(filepath.Join(dir, "gh"), args...)
		cmd.Env = staleClaimsEnv(dir, logPath)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("the stub ACCEPTED a write it must refuse: gh %s\noutput: %s\n\n"+
				"Every arm must be default-deny (`-*) deny`). An enumerating blocklist only "+
				"catches the spellings someone happened to list, and pflag's attached forms "+
				"(--flag=value) walk past it -- that is exactly how this broke before (memql#2873).",
				strings.Join(args, " "), out)
		}
	}

	// And the legitimate call must still be admitted, or the tests above would
	// pass because the stub refuses everything.
	cmd := exec.Command(filepath.Join(dir, "gh"), "issue", "edit", "7", "-R", "o/r", "--remove-label", "claimed:x")
	cmd.Env = staleClaimsEnv(dir, logPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the stub refused the ONE legitimate mutation, so every 'no write happened' "+
			"assertion in this file would pass vacuously: %v\n%s", err, out)
	}
}

// TestStaleClaims_ClaimAgeComesFromTheLabelEvent pins the #2834 requirement
// that age is the CLAIM's, not the issue's.
//
// Asserted per row, not against whole stdout. The earlier whole-stdout form was
// vacuous: another row always supplied the substring, so deleting the guard
// entirely left the suite green.
func TestStaleClaims_ClaimAgeComesFromTheLabelEvent(t *testing.T) {
	cfg := staleClaimsStub{labelIssues: map[string][]stubIssue{
		"claimed:sA": {
			// Claimed 100h ago -> SUSPECT, regardless of any later issue
			// activity. Reading .updatedAt instead would call this FRESH the
			// moment anyone commented.
			{num: 3100, state: "OPEN", title: "claimed long ago", claimedAt: hoursAgo(100)},
			// Claimed in the FUTURE: clock skew. Must be UNKNOWN, never a
			// finding and never swept.
			{num: 3101, state: "OPEN", title: "future timestamp", claimedAt: time.Now().Add(48 * time.Hour)},
			// Closed and skewed: must NOT be swept either.
			{num: 3102, state: "CLOSED", title: "closed, skewed claim",
				claimedAt: time.Now().Add(48 * time.Hour), closedAt: hoursAgo(90)},
		},
	}}
	res := runStaleClaims(t, cfg, []string{"--apply"})

	if got := res.rowFor(3100); !strings.Contains(got, "SUSPECT") {
		t.Errorf("#3100 = %q, want SUSPECT: its claim is 100h old.", got)
	}
	if got := res.rowFor(3101); !strings.Contains(got, "UNKNOWN") {
		t.Errorf("#3101 = %q, want UNKNOWN: a future label timestamp is clock skew, and skew must "+
			"never manufacture a finding.", got)
	}
	if got := res.rowFor(3102); !strings.Contains(got, "UNKNOWN") {
		t.Errorf("#3102 = %q, want UNKNOWN.", got)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("swept a claim whose age is unknown: %v\nDefault-deny: a removal happens only when "+
			"every input is positively known.", got)
	}
}

// TestStaleClaims_IdleThresholdGatesBothSides pins IDLE_HOURS as the single
// cutoff for "cold", on the closed side as well as the open one.
func TestStaleClaims_IdleThresholdGatesBothSides(t *testing.T) {
	cfg := staleClaimsStub{labelIssues: map[string][]stubIssue{
		"claimed:sZ": {
			{num: 3200, state: "OPEN", title: "open, 5h", claimedAt: hoursAgo(5)},
			{num: 3201, state: "CLOSED", title: "closed, 5h", claimedAt: hoursAgo(5), closedAt: hoursAgo(5)},
		},
	}}

	strict := runStaleClaims(t, cfg, []string{"--apply"}, "IDLE_HOURS=4")
	if got := strict.rowFor(3200); !strings.Contains(got, "SUSPECT") {
		t.Errorf("IDLE_HOURS=4, claim 5h old: #3200 = %q, want SUSPECT", got)
	}
	if len(strict.edits()) != 1 {
		t.Errorf("IDLE_HOURS=4: expected the 5h-old CLOSED claim to be swept, got %v", strict.edits())
	}

	lax := runStaleClaims(t, cfg, []string{"--apply"}, "IDLE_HOURS=48")
	if got := lax.rowFor(3200); !strings.Contains(got, "FRESH") {
		t.Errorf("IDLE_HOURS=48, claim 5h old: #3200 = %q, want FRESH", got)
	}
	if got := lax.rowFor(3201); !strings.Contains(got, "CLOSING") {
		t.Errorf("IDLE_HOURS=48: #3201 = %q, want CLOSING -- a recently-closed issue's claim is the "+
			"session's normal shutdown window, not an abandonment.", got)
	}
	if got := lax.edits(); len(got) > 0 {
		t.Errorf("IDLE_HOURS=48: swept a claim younger than the threshold: %v", got)
	}
}

// TestStaleClaims_ParkedOpenIssueIsNotAFinding: a human already recorded why a
// parked issue is not moving, so its claim is not what is hiding it.
func TestStaleClaims_ParkedOpenIssueIsNotAFinding(t *testing.T) {
	cfg := staleClaimsStub{labelIssues: map[string][]stubIssue{
		"claimed:sP": {{num: 2870, state: "OPEN", title: "parked with a diagnosis",
			claimedAt: hoursAgo(100), parked: true}},
	}}
	res := runStaleClaims(t, cfg, []string{"--apply"})
	if got := res.rowFor(2870); !strings.Contains(got, "PARKED") {
		t.Errorf("#2870 = %q, want PARKED", got)
	}
	if strings.Contains(res.stdout, "SUSPECT") {
		t.Errorf("a parked issue was reported as SUSPECT.\ngot:\n%s", res.stdout)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("swept a claim on an OPEN parked issue: %v", got)
	}
}

// TestStaleClaims_LabelListFailureIsLoud: a failed enumeration must not read as
// "no claims anywhere". That false all-clear is the failure this tool exists to
// prevent, so producing one would be worse than not running.
func TestStaleClaims_LabelListFailureIsLoud(t *testing.T) {
	cfg := mixedClaims()
	cfg.labelFail = true
	res := runStaleClaims(t, cfg, nil)
	if res.code == 0 {
		t.Errorf("a failed label enumeration exited 0.\nstdout:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "No claimed:* labels") {
		t.Errorf("a failed enumeration printed an all-clear:\n%s", res.stdout)
	}
}

// TestStaleClaims_FailedSweepExitsNonZero: every removal can be rejected and
// the run must not look successful. The capability contract reserves 5 for
// "op failed"; without it an automated caller cannot tell a complete sweep
// from a total failure.
func TestStaleClaims_FailedSweepExitsNonZero(t *testing.T) {
	cfg := mixedClaims()
	cfg.editFail = true
	res := runStaleClaims(t, cfg, []string{"--apply"})
	if res.code == 0 {
		t.Errorf("every label removal failed and the script still exited 0.\nstderr:\n%s", res.stderr)
	}
	if res.code != 5 {
		t.Errorf("exit %d; want 5 (op failed) per the documented exit codes", res.code)
	}
	if !strings.Contains(res.stderr, "FAILED") {
		t.Errorf("a rejected removal was not reported on stderr:\n%s", res.stderr)
	}
}

// TestStaleClaims_MultipleLabelsAndIssuesAreEachHandled: the state a sweeper
// most needs to get right. Two labels, several issues each.
//
// The stub deliberately does NOT drain stdin, and the script closes stdin on
// every gh call: without that, a `gh` inside a `while read` loop consumes the
// loop's input and only the first row is processed.
func TestStaleClaims_MultipleLabelsAndIssuesAreEachHandled(t *testing.T) {
	cfg := staleClaimsStub{labelIssues: map[string][]stubIssue{
		"claimed:sA": {
			{num: 4001, state: "CLOSED", title: "a1", claimedAt: hoursAgo(100), closedAt: hoursAgo(90)},
			{num: 4002, state: "CLOSED", title: "a2", claimedAt: hoursAgo(100), closedAt: hoursAgo(90)},
		},
		"claimed:sB": {
			{num: 4003, state: "CLOSED", title: "b1", claimedAt: hoursAgo(100), closedAt: hoursAgo(90)},
		},
	}}
	res := runStaleClaims(t, cfg, []string{"--apply"})
	if got := res.edits(); len(got) != 3 {
		t.Fatalf("expected 3 removals across 2 labels, got %d:\n  %s\nIf only one landed, a gh call "+
			"inside a read loop is eating the loop's stdin.", len(got), strings.Join(got, "\n  "))
	}
	for _, want := range []string{"4001", "4002", "4003"} {
		found := false
		for _, e := range res.edits() {
			if strings.Contains(e, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("#%s was never swept: %v", want, res.edits())
		}
	}
}

// TestStaleClaims_BadArgsAreRefusedBeforeAnyWork: a bad argument must exit 2
// without touching the API, and must not leave --apply set.
func TestStaleClaims_BadArgsAreRefusedBeforeAnyWork(t *testing.T) {
	for _, args := range [][]string{
		{"-apply"}, {"--apply=1"}, {"--APPLY"}, {"--aply"}, {"--bogus", "--apply"}, {"--apply", "--bogus"},
	} {
		res := runStaleClaims(t, mixedClaims(), args)
		if res.code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, res.code)
		}
		if got := res.edits(); len(got) > 0 {
			t.Errorf("args %v: made a write: %v", args, got)
		}
	}
}

// TestStaleClaims_BadThresholdIsRefused: IDLE_HOURS gates every sweep decision,
// so an unusable value must stop the run rather than silently defaulting.
func TestStaleClaims_BadThresholdIsRefused(t *testing.T) {
	// "" is deliberately NOT in this list -- see the test below.
	for _, v := range []string{"abc", "-1", "4.5", "4h", " 4", "0x4"} {
		res := runStaleClaims(t, mixedClaims(), []string{"--apply"}, "IDLE_HOURS="+v)
		if res.code != 2 {
			t.Errorf("IDLE_HOURS=%q: exit %d, want 2", v, res.code)
		}
		if got := res.edits(); len(got) > 0 {
			t.Errorf("IDLE_HOURS=%q: made a write: %v", v, got)
		}
	}
}

// TestStaleClaims_EmptyThresholdMeansDefault pins the one value that is NOT an
// error. `${IDLE_HOURS:-4}` treats empty as unset, which is the conventional
// shell reading and is safe in the direction that matters: it falls back to the
// documented default rather than widening the sweep. Pinned rather than left
// implicit, because "empty means 4" and "empty is refused" are both defensible
// and a silent change between them moves what gets swept.
func TestStaleClaims_EmptyThresholdMeansDefault(t *testing.T) {
	empty := runStaleClaims(t, mixedClaims(), []string{"--apply"}, "IDLE_HOURS=")
	unset := runStaleClaims(t, mixedClaims(), []string{"--apply"})

	if empty.code != unset.code {
		t.Errorf("IDLE_HOURS= exited %d, unset exited %d; they must agree", empty.code, unset.code)
	}
	if len(empty.edits()) != len(unset.edits()) {
		t.Errorf("IDLE_HOURS= swept %v, unset swept %v; empty must behave exactly like unset",
			empty.edits(), unset.edits())
	}
	// And it must be the DEFAULT of 4, not some other number: a 100h-old claim
	// is cold, a 30min-old one is not.
	if got := empty.rowFor(2998); !strings.Contains(got, "SUSPECT") {
		t.Errorf("IDLE_HOURS=: #2998 (claimed 100h ago) = %q, want SUSPECT", got)
	}
	if got := empty.rowFor(2999); !strings.Contains(got, "FRESH") {
		t.Errorf("IDLE_HOURS=: #2999 (claimed 30min ago) = %q, want FRESH", got)
	}
}

// TestStaleClaims_HelpMakesNoApiCall: --help must not need credentials.
func TestStaleClaims_HelpMakesNoApiCall(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), []string{"--help"})
	if res.code != 0 {
		t.Errorf("--help exited %d", res.code)
	}
	if len(res.calls) != 0 {
		t.Errorf("--help invoked gh: %v", res.calls)
	}
}

// TestStaleClaims_NoClaimsIsAnHonestAllClear: with no claim labels the summary
// must say so -- and must only be reachable after a SUCCESSFUL enumeration.
func TestStaleClaims_NoClaimsIsAnHonestAllClear(t *testing.T) {
	res := runStaleClaims(t, staleClaimsStub{labelIssues: map[string][]stubIssue{}}, []string{"--apply"})
	if res.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "No claimed:* labels") {
		t.Errorf("expected an all-clear, got:\n%s", res.stdout)
	}
	if got := res.edits(); len(got) > 0 {
		t.Errorf("wrote something with no claims present: %v", got)
	}
}

// labelPageLimit mirrors LABEL_PAGE_LIMIT in the script. Deliberately NOT made
// configurable there -- the script's comment says raising a cap silently is how
// it stops being noticed -- so the test states the same number and
// TestStaleClaims_PageCapConstantsMatchTheScript fails if they drift.
const labelPageLimit = 300

func TestStaleClaims_PageCapConstantsMatchTheScript(t *testing.T) {
	src, err := os.ReadFile(staleClaimsScript)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	want := fmt.Sprintf("LABEL_PAGE_LIMIT=%d", labelPageLimit)
	if !strings.Contains(string(src), want) {
		t.Fatalf("the script no longer sets %s, so TestStaleClaims_FullLabelPageWarns "+
			"is padding to the wrong number and silently stopped testing the cap.", want)
	}
}

// TestStaleClaims_FullLabelPageWarns pins the fix for round-1 BLOCKER 1.
//
// The original guard compared the FILTERED claim count against the cap, so it
// could only fire if 100 issues carried a claim at once -- it never fired while
// the report was silently dropping rows. It also had no test, so restoring that
// exact bug stayed green. This pads the label page to the cap and requires the
// warning.
func TestStaleClaims_FullLabelPageWarns(t *testing.T) {
	cfg := mixedClaims()
	// mixedClaims defines 2 claim labels; pad the rest of the page.
	cfg.padLabels = labelPageLimit - len(cfg.labelIssues)

	res := runStaleClaims(t, cfg, nil)
	if !strings.Contains(res.stderr, "page cap") {
		t.Errorf("a FULL label page produced no warning.\nA clean report from a truncated "+
			"enumeration is a false all-clear, which is the failure memql#2834 exists to "+
			"prevent.\nstderr:\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "incomplete") {
		t.Errorf("the warning does not say the report is incomplete:\n%s", res.stderr)
	}
}

// TestStaleClaims_ShortLabelPageDoesNotWarn is the other direction: the warning
// must not cry wolf on every run, or it stops being read.
func TestStaleClaims_ShortLabelPageDoesNotWarn(t *testing.T) {
	res := runStaleClaims(t, mixedClaims(), nil)
	if strings.Contains(res.stderr, "page cap") {
		t.Errorf("a short label page warned anyway:\n%s", res.stderr)
	}
}

// TestStaleClaims_RemoveClaimRefusesANonClaimLabel exercises remove_claim's
// prefix guard DIRECTLY, by sourcing the script.
//
// The guard is unreachable through the normal flow -- the jq filter already
// selected the label -- which is exactly why it needs this: deleting it left
// the suite green. It is defence in depth for the case where that filter is
// widened or broken, and the honest way to test an unreachable guard is to call
// it, not to fake a path to it.
func TestStaleClaims_RemoveClaimRefusesANonClaimLabel(t *testing.T) {
	dir, logPath := writeStaleClaimsStub(t, mixedClaims())

	run := func(label string) (int, string, []string) {
		cmd := exec.Command("bash", "-c",
			`source `+staleClaimsScript+`; remove_claim 7 "$1"`, "bash", label)
		cmd.Env = staleClaimsEnv(dir, logPath)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("source+call: %v", err)
		}
		raw, _ := os.ReadFile(logPath)
		var calls []string
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(l) != "" {
				calls = append(calls, l)
			}
		}
		return code, errb.String(), calls
	}

	code, stderr, calls := run("bug")
	if code == 0 {
		t.Errorf("remove_claim accepted a non-claim label")
	}
	if !strings.Contains(stderr, "REFUSED") {
		t.Errorf("no REFUSED diagnostic for a non-claim label: %q", stderr)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "EDIT ") {
			t.Errorf("remove_claim issued a write for a non-claim label: %q\nThe prefix check "+
				"must refuse BEFORE calling gh, so a broken selection filter cannot delete an "+
				"unrelated label.", c)
		}
	}

	// And the legitimate label must still go through, or the guard is just
	// breaking the feature.
	code, _, calls = run("claimed:sX")
	if code != 0 {
		t.Errorf("remove_claim refused a legitimate claimed:* label (exit %d)", code)
	}
	found := false
	for _, c := range calls {
		if strings.HasPrefix(c, "EDIT ") && strings.Contains(c, "claimed:sX") {
			found = true
		}
	}
	if !found {
		t.Errorf("the legitimate removal was not issued: %v", calls)
	}
}
