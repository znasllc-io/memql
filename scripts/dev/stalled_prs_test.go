// Contract gate for scripts/dev/stalled-prs.sh (znasllc-io/memql#2833).
//
// The load-bearing property is that the reporter makes no GitHub mutation:
// adversarial review of every PR merged the night this was written found a
// real defect in each one, so "green" is demonstrably not "reviewed" here, and
// a sweeper that enqueued automatically would land unreviewed work at exactly
// the moment nobody is watching.
//
// That property is checked BEHAVIOURALLY -- the script is executed against a
// stub `gh` that refuses any non-read subcommand and records every call. Three
// earlier rounds tried to check it by statically scanning the script and were
// defeated three different ways, the last simply by quoting one token
// (`"gh" pr merge`). Out-parsing shell quoting, expansion, eval, sub-shells
// and indirection is not winnable; running the thing is.
//
// The same stub doubles as the fault injector for the fail-closed guarantees:
// an API failure, an unresolved merge-queue lookup or a PR with no CI must
// never become a STALLED finding, and each of those is now driven end to end
// rather than only through the pure classifier.
package dev

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const stalledPRsScript = "stalled-prs.sh"

func readScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(stalledPRsScript)
	if err != nil {
		t.Fatalf("read %s: %v", stalledPRsScript, err)
	}
	return string(raw)
}

// ghStub configures the fake gh for one run.
//
// There is no headSHA field any more: the head SHA rides in the prList row, so
// `gh pr view` is not called at all (memql#2887). Deleting the field rather
// than leaving it unused is deliberate -- the stub's `pr view` arm went with
// it, so if the call ever comes back it lands in the default-deny case and
// fails loudly instead of being quietly served.
type ghStub struct {
	prList     string // TSV rows for `gh pr list --state open`; "" means no rows
	prListFail bool   // that call exits non-zero
	liveness   string // pr_liveness graphql result: "<queued>\t<oid>\t<epoch>"; "" means fail
	checkRuns  string // `gh api ...check-runs` newline list; "" means no runs
	checksFail bool   // the check-runs call exits non-zero

	// The closed-unmerged sweep (memql#2887). Both default to empty, which
	// means "no closed-unmerged PRs" -- so every pre-existing test keeps
	// exercising exactly the open sweep it was written for.
	closedList     string // TSV rows for `gh pr list --state closed`
	closedListFail bool   // that call exits non-zero
	closedFacts    string // closed_pr_facts graphql result: "<branch>\t<openIssues>"; "" means fail
}

// writeGhStub installs a fake `gh` on PATH.
//
// Anything outside the read-only set is recorded as FORBIDDEN and fails the
// call. Because this is the real gh the script invokes, it observes what bash
// actually ran -- quoting, `eval`, `bash -c`, a helper function, or a command
// built at runtime all arrive here identically.
// shellQuote wraps s for bash. Go's %q is GO quoting -- it renders a tab as
// the two characters \\t, which bash passes through literally, so every TSV
// row arrived as a single field and each PR classified UNKNOWN.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// bucketColumn extracts the STATE column from the report's table rows.
//
// A report line is `#<num> <STATE> <IDLE> <CHECKS> <TITLE>`; everything else
// (the header, the rules, the summary prose) is skipped. Parsing the column
// rather than substring-matching stdout keeps the assertions about what the
// classifier DECIDED instead of about how the summary happens to be worded.
// It is scoped to the OPEN-PR table. The closed-unmerged sweep (memql#2887)
// prints rows in the same `#<num> <STATE> ...` shape below it, so an unscoped
// scan folded both sections into one list -- and every "no STALLED anywhere"
// assertion in this file would then be answered partly by rows the case under
// test never configured.
func bucketColumn(stdout string) []string { return bucketsIn(openSection(stdout)) }

// closedSectionHeading is the divider between the two reports. Kept as a
// constant because both openSection and the closed-sweep tests key on it, and
// a reworded heading must break them together rather than silently rescope
// bucketColumn to the whole report.
const closedSectionHeading = "CLOSED WITHOUT MERGING"

func openSection(stdout string) string {
	if i := strings.Index(stdout, closedSectionHeading); i >= 0 {
		return stdout[:i]
	}
	return stdout
}

func closedSection(stdout string) string {
	if i := strings.Index(stdout, closedSectionHeading); i >= 0 {
		return stdout[i:]
	}
	return ""
}

func bucketsIn(section string) []string {
	var out []string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			out = append(out, fields[1])
		}
	}
	return out
}

// scriptBody returns the script with its `main "$@"` invocation removed, so a
// test can source the function definitions without running the report.
//
// The removal is ASSERTED, and that assertion is load-bearing rather than
// defensive. A silent no-op here does not fail the test -- it makes the
// sourced body EXECUTE main, and the tests that source it put no stub `gh` on
// PATH, so every subtest fires a real sweep against the live GitHub API.
// Measured by breaking the anchor deliberately: runtime went from 1.2s to
// >120s (timeout). In CI, where there is no `gh auth`, the script exits 4 and
// the failure surfaces as `classify(...): exit status 4` -- pointing at the
// classifier, which is not what broke.
func scriptBody(t *testing.T) string {
	t.Helper()
	src := readScript(t)
	body := strings.Replace(src, "\nmain \"$@\"\n", "\n", 1)
	if body == src {
		t.Fatalf("could not find the `main \"$@\"` anchor in %s -- its last line has drifted. "+
			"Without the removal, sourcing this body RUNS the report against the live GitHub "+
			"API instead of just defining its functions.", stalledPRsScript)
	}
	return body
}

// hermeticEnv builds the environment for a bash subprocess under test.
//
// os.Environ() is deliberately NOT inherited. The script documents
// `IDLE_MINUTES=15 bash scripts/dev/stalled-prs.sh`, so a developer who
// exports that variable breaks this suite from the outside -- and not subtly:
//
//	IDLE_MINUTES=200 go test ./scripts/dev/   -> classify(...idle=120) = "FRESH", want "STALLED"
//	IDLE_MINUTES=45m go test ./scripts/dev/   -> 11 failures, incl. an exit-code assertion
//
// REPO is the same hazard. Passing only what a case sets makes the suite
// depend on its inputs rather than on the developer's shell.
func hermeticEnv(extra ...string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	return append(env, extra...)
}

func writeGhStub(t *testing.T, cfg ghStub) (dir, logPath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "calls.log")

	fail := func(b bool) string {
		if b {
			return "exit 1"
		}
		return "exit 0"
	}

	stub := fmt.Sprintf(`#!/usr/bin/env bash
printf '%%s\n' "$*" >> %q

# A write can also hide in the ARGUMENTS of a legitimate read verb, so argv is
# screened before the verb is dispatched.
#
# This is an ALLOWLIST, and the distinction is the whole point. The previous
# version enumerated write flags -- -X, --method, --input, --field,
# --raw-field -- and pflag's ATTACHED forms walked straight past it. All four
# of these were ACCEPTED by that screener:
#
#     api --method=POST repos/o/r/merges
#     api --field=body=hi repos/o/r/issues/1/comments
#     api --raw-field=q=1 repos/o/r/x
#     api -XPOST repos/o/r/merges
#
# -XPOST is the sharpest, because the old code implemented attached-shorthand
# detection for -f/-F only, in a comment that named exactly that hazard.
# Enumerating spellings is the losing side of this game -- the same weakness
# that defeated three earlier rounds of this guard, relocated from the script
# into the stub that was supposed to make the guarantee unconditional.
#
# So every argument must MATCH a known-safe form and anything unrecognised is
# refused by DEFAULT. A new gh flag, a new spelling of an old one, or an
# abbreviation nobody anticipated all land in the default case.
#
# Two gh behaviours still need naming explicitly:
#   - gh sends POST as soon as any field flag is present, with no -X in sight.
#     So -f is tolerated ONLY on the graphql endpoint, never on a REST path.
#   - GraphQL is always POSTed, so read vs write is decided by the document.
#     gh is last-wins on a duplicated -f query=, so more than one is refused.
if [ "$1" = "api" ]; then
  shift
  endpoint=""; expect_jq=""; expect_query=""; qcount=0
  for a in "$@"; do
    if [ -n "$expect_jq" ]; then expect_jq=""; continue; fi
    if [ -n "$expect_query" ]; then
      case "$a" in
        query=*) qcount=$((qcount + 1)) ;;
        *) FORBID="-f value is not a query document: $a" ;;
      esac
      expect_query=""
      continue
    fi
    case "$a" in
      --paginate) ;;
      --jq | -q)  expect_jq=1 ;;
      -f)         expect_query=1 ;;
      -*)         FORBID="unrecognised api flag $a" ;;
      *)          if [ -z "$endpoint" ]; then endpoint="$a"; else FORBID="second positional api argument $a"; fi ;;
    esac
  done
  [ -n "$expect_jq$expect_query" ] && FORBID="dangling flag with no value"
  [ "$qcount" -gt 1 ] && FORBID="duplicate -f query= (gh takes the last)"
  if [ "$qcount" -gt 0 ] && [ "$endpoint" != "graphql" ]; then
    FORBID="-f on the REST path $endpoint makes it a POST"
  fi
  case "$endpoint" in
    graphql | repos/*) ;;
    *) FORBID="api endpoint outside the read allowlist: $endpoint" ;;
  esac
  set -- api "$@"
fi
for a in "$@"; do
  case "$a" in *mutation*) FORBID="graphql mutation" ;; esac
done

if [ -n "${FORBID:-}" ]; then
  printf 'FORBIDDEN %%s (%%s)\n' "$*" "$FORBID" >> %q
  echo "stub gh: refusing $FORBID: $*" >&2
  exit 1
fi

# pr list is called TWICE now -- once for open PRs, once for the
# closed-unmerged sweep -- and api graphql is called with two different
# documents. Dispatching on "$1 $2" alone served the same canned bytes to
# both, which fed open-PR rows into the closed sweep and made its assertions
# meaningless. Each arm is selected by something only its caller sends.
#
# No backticks anywhere in this template: it is a Go raw string literal, and a
# backtick in a shell comment terminates it.
if [ "$1 $2" = "pr list" ]; then
  case "$*" in
    *"--state open"*)   printf '%%s' %s; %s ;;
    *"--state closed"*) printf '%%s' %s; %s ;;
    *) echo "stub gh: pr list with no recognised --state: $*" >&2; exit 1 ;;
  esac
fi

if [ "$1 $2" = "api graphql" ]; then
  case "$*" in
    *isInMergeQueue*) printf '%%s' %s; [ -n %s ] || exit 1; exit 0 ;;
    *headRef*)        printf '%%s' %s; [ -n %s ] || exit 1; exit 0 ;;
    *) echo "stub gh: unrecognised graphql document: $*" >&2; exit 1 ;;
  esac
fi

case "$1 $2" in
  "--version "*)  echo 'gh version 2.45.0'; exit 0 ;;
  "auth status")  exit 0 ;;
esac
case "$1" in
  api) printf '%%s' %s; %s ;;
esac

# Everything else is a write, or something this stub has not been taught.
# Both must fail the test rather than silently succeed.
printf 'FORBIDDEN %%s\n' "$*" >> %q
echo "stub gh: refusing non-read invocation: $*" >&2
exit 1
`,
		logPath,
		logPath,
		shellQuote(cfg.prList), fail(cfg.prListFail),
		shellQuote(cfg.closedList), fail(cfg.closedListFail),
		shellQuote(cfg.liveness), shellQuote(cfg.liveness),
		shellQuote(cfg.closedFacts), shellQuote(cfg.closedFacts),
		shellQuote(cfg.checkRuns), fail(cfg.checksFail),
		logPath,
	)

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub gh: %v", err)
	}
	return dir, logPath
}

type runResult struct {
	stdout, stderr string
	code           int
	calls          []string
}

func (r runResult) forbidden() []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "FORBIDDEN ") {
			out = append(out, strings.TrimPrefix(c, "FORBIDDEN "))
		}
	}
	return out
}

func runScript(t *testing.T, cfg ghStub, env ...string) runResult {
	t.Helper()
	dir, logPath := writeGhStub(t, cfg)

	cmd := exec.Command("bash", stalledPRsScript)
	cmd.Env = hermeticEnv(append([]string{"PATH=" + dir + ":" + os.Getenv("PATH")}, env...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run script: %v", err)
	}

	var calls []string
	if raw, rerr := os.ReadFile(logPath); rerr == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if l != "" {
				calls = append(calls, l)
			}
		}
	}
	return runResult{stdout.String(), stderr.String(), code, calls}
}

// oneStalledPR is the happy path: green, mergeable, unqueued, long idle.
// Epoch 1000000000 is 2001, so it is unambiguously stale.
//
// The head SHA now travels in the prList row (column 5), and the liveness
// response repeats it -- pr_liveness cross-checks the two and resolves to
// `unknown` on a mismatch, so a fixture whose oids disagree would classify
// UNKNOWN rather than STALLED.
var oneStalledPR = ghStub{
	prList:    "7\tfalse\tCLEAN\t1000000000\tdeadbeef\tissue/7-x\tan abandoned green pr\n",
	liveness:  "false\tdeadbeef\t1000000000",
	checkRuns: "success\nskipped\n",
}

// TestStalledPRs_MakesNoMutatingCall is the load-bearing test.
//
// It runs BOTH sweeps. The closed-unmerged fixture is not optional decoration:
// with an empty closedList, report_closed_unmerged returns before its per-PR
// loop, so its ~65 lines never execute and the read-only guarantee the script
// header, the usage text and this file's header all claim is enforced
// behaviourally would not have covered them at all.
func TestStalledPRs_MakesNoMutatingCall(t *testing.T) {
	bothSweeps := oneStalledPR
	bothSweeps.closedList = fmt.Sprintf("11\tissue/11-thing\t%d\ta closed pr\n", time.Now().Unix()-3600)
	bothSweeps.closedFacts = "present\t101"

	for _, tc := range []struct {
		name string
		cfg  ghStub
	}{
		{"open sweep only", oneStalledPR},
		{"both sweeps", bothSweeps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runScript(t, tc.cfg)

			if got := res.forbidden(); len(got) > 0 {
				t.Errorf("the reporter invoked gh in a way the stub refuses:\n  %s\nit must stay read-only -- enqueuing green-but-unreviewed work unattended is the failure it exists to avoid (memql#2833)",
					strings.Join(got, "\n  "))
			}
			if len(res.calls) == 0 {
				t.Fatal("the stub recorded no gh calls at all; this test would pass vacuously")
			}
			// Guard the guard: the stub must actually refuse a write.
			if res.code != 0 {
				t.Fatalf("expected a clean run, got exit %d\nstderr: %s", res.code, res.stderr)
			}
		})
	}

	// Prove the second case genuinely drove the closed sweep, rather than
	// passing because the section was never reached.
	res := runScript(t, bothSweeps)
	if !strings.Contains(res.stdout, closedSectionHeading) {
		t.Errorf("the `both sweeps` fixture did not actually produce a closed section, so it added no coverage.\ngot:\n%s", res.stdout)
	}
}

// TestStalledPRs_StubRefusesWrites proves the harness above has teeth. Without
// it, a stub that quietly succeeded on everything would make
// TestStalledPRs_MakesNoMutatingCall pass no matter what the script did.
func TestStalledPRs_StubRefusesWrites(t *testing.T) {
	dir, logPath := writeGhStub(t, oneStalledPR)

	for _, write := range []string{
		"pr merge 7 --squash --auto",
		"pr comment 7 --body hi",
		"pr review 7 --approve",
		"issue edit 7 --add-label x",
		"label create foo",
		"workflow run ci.yml",

		// The four spellings that DEFEATED the previous screener. It matched
		// -X / --method / --input / --field / --raw-field as exact tokens;
		// pflag also accepts each attached to its value, and all four of these
		// were accepted as read-only. They are pinned individually rather than
		// as one case so a regression names which form came back.
		"api --method=POST repos/o/r/merges",
		"api --field=body=hi repos/o/r/issues/1/comments",
		"api --raw-field=q=1 repos/o/r/x",
		"api -XPOST repos/o/r/merges",

		// The forms the old screener DID catch must stay caught -- inverting
		// to an allowlist is only an improvement if it is a superset.
		"api -X POST repos/o/r/merges",
		"api --input body.json repos/o/r/x",
		"api -fbody=hi repos/o/r/issues/1/comments",

		// An endpoint outside the read allowlist, with no flag to give it
		// away. gh defaults to GET, so this one is harmless in itself -- it is
		// pinned because "the stub only knows the endpoints the script uses"
		// is the property that makes the default-deny meaningful.
		"api user/repos",

		// -f makes gh POST even with no -X, so it is tolerated only on
		// graphql. On a REST path it is a write in disguise.
		"api -f query=x repos/o/r/x",
	} {
		cmd := exec.Command(filepath.Join(dir, "gh"), strings.Fields(write)...)
		if err := cmd.Run(); err == nil {
			t.Errorf("stub gh accepted a write invocation %q; the read-only test would be toothless", write)
		}
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	if !strings.Contains(string(raw), "FORBIDDEN pr merge") {
		t.Errorf("stub did not record the refused write; got:\n%s", raw)
	}
}

// TestStalledPRs_EndToEnd pins that a genuine finding IS reported, and on
// stdout -- `make prs-stalled > report.txt` must not be empty.
func TestStalledPRs_EndToEnd(t *testing.T) {
	res := runScript(t, oneStalledPR)

	if res.code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"PR", "STATE", "#7", "STALLED", "are STALLED"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout missing %q; the report must go to stdout, not stderr.\ngot:\n%s", want, res.stdout)
		}
	}
}

// TestStalledPRs_FailuresNeverBecomeFindings drives each fail-closed guarantee
// END TO END.
//
// These were previously covered only through the pure classifier, so the five
// callers that PRODUCE its arguments were unpinned: reverting "merge-queue
// lookup failed -> unknown", "check lookup failed -> unknown", "empty sha ->
// unknown" or "unreadable timestamp -> unknown" left the whole suite green,
// while each one turns an API hiccup into an invitation to enqueue.
func TestStalledPRs_FailuresNeverBecomeFindings(t *testing.T) {
	base := oneStalledPR

	cases := []struct {
		name    string
		mutate  func(*ghStub)
		wantCol string
	}{
		{"liveness lookup fails", func(c *ghStub) { c.liveness = "" }, "UNKNOWN"},
		{"check-runs lookup fails", func(c *ghStub) { c.checksFail = true }, "UNKNOWN"},
		{"head sha unreadable", func(c *ghStub) {
			// open_prs emits "-" for a missing headRefOid rather than an empty
			// field: tab is IFS whitespace, so an empty MIDDLE column would
			// collapse and shift the title left instead of being read as blank.
			c.prList = "7\tfalse\tCLEAN\t1000000000\t-\tissue/7-x\ta pr\n"
		}, "UNKNOWN"},
		{"no check runs at all", func(c *ghStub) { c.checkRuns = "" }, "NO-CI"},
		{"unreadable timestamp", func(c *ghStub) {
			c.prList = "7\tfalse\tCLEAN\tnot-a-number\tdeadbeef\tissue/7-x\ta pr\n"
			c.liveness = "false\tdeadbeef\tnot-a-number"
		}, "UNKNOWN"},
		{"head commit date unreadable", func(c *ghStub) {
			c.liveness = "false\tdeadbeef\t"
		}, "UNKNOWN"},
		{"head moved mid-sweep", func(c *ghStub) {
			// The oid the liveness call returns disagrees with the one the list
			// gave us: the head moved between the two requests, so the clock is
			// not this PR's and the check-runs already fetched are for a SHA
			// that is no longer the head.
			c.liveness = "false\tcafebabe\t1000000000"
		}, "UNKNOWN"},
		{"head commit in the future", func(c *ghStub) {
			// committedDate is a clock this script does not control.
			c.liveness = "false\tdeadbeef\t99999999999"
		}, "UNKNOWN"},
		{"mergeability still computing", func(c *ghStub) {
			c.prList = "7\tfalse\tUNKNOWN\t1000000000\tdeadbeef\tissue/7-x\ta pr\n"
		}, "UNKNOWN"},
		{"already in the merge queue", func(c *ghStub) { c.liveness = "true\tdeadbeef\t1000000000" }, "QUEUED"},
		{"failing checks", func(c *ghStub) { c.checkRuns = "failure\nsuccess\n" }, "RED"},
		{"checks still running", func(c *ghStub) { c.checkRuns = "in_progress\n" }, "PENDING"},
		{"draft", func(c *ghStub) {
			c.prList = "7\ttrue\tCLEAN\t1000000000\tdeadbeef\tissue/7-x\ta draft\n"
		}, "DRAFT"},
		{"draft flag unrecognised", func(c *ghStub) {
			c.prList = "7\tTRUE\tCLEAN\t1000000000\tdeadbeef\tissue/7-x\ta pr\n"
		}, "UNKNOWN"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			res := runScript(t, cfg)

			// Assert on the STATE COLUMN of the table rows, not on the whole
			// stdout. A bare strings.Contains here reads the summary prose
			// too: rewording the clean-run line to "No STALLED PRs." made
			// every one of these cases fail on a report that was correct.
			// The inverse -- prose that happens to contain the wanted bucket
			// name satisfying wantCol -- is the same bug pointing the other
			// way, and would pass silently.
			buckets := bucketColumn(res.stdout)
			for _, b := range buckets {
				if b == "STALLED" {
					t.Errorf("a %s produced a STALLED finding -- an unresolved state must never invite an enqueue.\ngot:\n%s",
						tc.name, res.stdout)
				}
			}
			if len(buckets) == 0 {
				t.Fatalf("no table rows parsed from the report; this case would assert nothing.\ngot:\n%s", res.stdout)
			}
			found := false
			for _, b := range buckets {
				if b == tc.wantCol {
					found = true
				}
			}
			if !found {
				t.Errorf("want bucket %s in the STATE column, got %v:\n%s", tc.wantCol, buckets, res.stdout)
			}
			if got := res.forbidden(); len(got) > 0 {
				t.Errorf("mutating call during %s: %v", tc.name, got)
			}
		})
	}
}

// TestStalledPRs_ListFailureIsLoud covers the worst possible output for a
// watchdog: learning nothing and reporting all-clear. It must exit 4.
func TestStalledPRs_ListFailureIsLoud(t *testing.T) {
	res := runScript(t, ghStub{prListFail: true, liveness: "false\tdeadbeef\t1000000000"})

	if res.code != 4 {
		t.Errorf("exit = %d, want 4 when the PR list cannot be read", res.code)
	}
	if strings.Contains(res.stdout, "No stalled PRs") {
		t.Errorf("a total API failure printed a reassuring all-clear:\n%s", res.stdout)
	}
}

// TestStalledPRs_BadThresholdIsRejected covers the documented knob that twice
// re-created the manufactured-finding bug.
//
// `[ "$idle" -lt "$IDLE_MINUTES" ]` ERRORS rather than evaluating false when
// the comparison is not a valid integer, and an errored test falls through to
// `echo "STALLED"`. Round 2 hit it with "45m"; round 3 hit it again with a
// value that is all digits but overflows int64 -- so a threshold meaning "show
// me nothing" reported EVERYTHING, at exit 0.
func TestStalledPRs_BadThresholdIsRejected(t *testing.T) {
	for _, bad := range []string{
		"45m",                  // not a number
		"99999999999999999999", // all digits, overflows int64
		" 45",                  // leading space
		"-5",                   // negative
		// NOT included: "". An explicitly-empty IDLE_MINUTES falls to the
		// ${IDLE_MINUTES:-45} default, which is the conventional shell
		// reading of "unset" -- correct behaviour, not a bad parameter.
	} {
		t.Run("IDLE_MINUTES="+bad, func(t *testing.T) {
			res := runScript(t, oneStalledPR, "IDLE_MINUTES="+bad)

			if res.code != 2 {
				t.Errorf("exit = %d, want 2 (bad param)", res.code)
			}
			if strings.Contains(res.stdout, "STALLED") {
				t.Errorf("a malformed threshold produced a STALLED finding:\n%s", res.stdout)
			}
		})
	}

	// And a valid threshold must still work, or the guard above is just a
	// blanket refusal.
	if res := runScript(t, oneStalledPR, "IDLE_MINUTES=1"); res.code != 0 ||
		!strings.Contains(res.stdout, "STALLED") {
		t.Errorf("a valid IDLE_MINUTES must still report; exit=%d out=%s", res.code, res.stdout)
	}
}

// TestStalledPRs_QueriesGitHubCorrectly pins the request shape. The stub
// returns canned output regardless of --json/--jq, so without this the whole
// translation layer is untested: swapping two columns in the @tsv array, or
// dropping a --json field, would leave every other test green.
func TestStalledPRs_QueriesGitHubCorrectly(t *testing.T) {
	res := runScript(t, oneStalledPR)

	// There are TWO `pr list` calls now (memql#2887), so selecting "the last
	// one" silently retargeted every assertion below onto the closed sweep.
	// Each is picked by its own --state.
	var list, closedList string
	for _, c := range res.calls {
		switch {
		case strings.HasPrefix(c, "pr list") && strings.Contains(c, "--state open"):
			list = c
		case strings.HasPrefix(c, "pr list") && strings.Contains(c, "--state closed"):
			closedList = c
		}
	}
	if list == "" {
		t.Fatal("no `gh pr list --state open` call recorded")
	}

	// The --jq array order must match the `read` destructuring in main().
	wantOrder := `.number, .isDraft, .mergeStateStatus, (.updatedAt|fromdateiso8601), (.headRefOid // "-"), (.headRefName // "-"), .title`
	if !strings.Contains(list, wantOrder) {
		t.Errorf("the --jq array order must stay in lockstep with `read -r num draft merge_state updated_epoch head_sha head_branch title`.\nwant substring: %s\ngot: %s", wantOrder, list)
	}
	// fromdateiso8601 is what removed the GNU-only `date -u -d`, under which
	// macOS silently reported every PR FRESH forever.
	if !strings.Contains(list, "fromdateiso8601") {
		t.Error("the epoch must come from jq, not `date -u -d`, which is GNU-only")
	}
	if !strings.Contains(list, "--state open") {
		t.Error("must list only OPEN prs")
	}
	// headRefOid is what removed the per-PR `gh pr view` (memql#2887). Losing
	// it from --json would not fail any behavioural test -- the stub serves
	// canned rows -- it would just make every PR UNKNOWN against real GitHub.
	for _, field := range []string{"number", "isDraft", "mergeStateStatus", "updatedAt", "headRefOid", "headRefName", "title"} {
		if !strings.Contains(list, field) {
			t.Errorf("--json is missing %q, which the report reads", field)
		}
	}

	// The closed-unmerged sweep. Its `select(.mergedAt == null)` cannot be
	// exercised behaviourally -- the stub serves canned bytes and discards the
	// --jq filter -- so the filter is pinned here as a string. That is a weaker
	// guarantee than the rest of this file and is called out rather than left
	// looking equivalent.
	if closedList == "" {
		t.Fatal("no `gh pr list --state closed` call recorded; the closed-unmerged sweep never ran")
	}
	if !strings.Contains(closedList, "is:unmerged") {
		t.Error("the closed sweep must push is:unmerged SERVER-side: `--state closed` returns MERGED PRs too, so an unfiltered page is almost all merged PRs and the sweep finds nothing while looking exhaustive")
	}
	if !strings.Contains(closedList, `select(.state == "CLOSED" and .mergedAt == null)`) {
		t.Error("the mergedAt filter must survive in jq as well as in the search: two independent filters, so neither one becoming a no-op turns a merged PR into a LOST-WORK finding")
	}
	for _, field := range []string{"number", "state", "mergedAt", "headRefName", "closedAt"} {
		if !strings.Contains(closedList, field) {
			t.Errorf("the closed sweep's --json is missing %q", field)
		}
	}

	// check-runs must paginate: GitHub defaults to per_page=30 and live PRs
	// here carry ~20, so an unpaginated call silently truncates and a failing
	// lane on page 2 reads as green -- a manufactured STALLED finding.
	var checks string
	for _, c := range res.calls {
		if strings.Contains(c, "check-runs") {
			checks = c
		}
	}
	if checks == "" {
		t.Fatal("no check-runs call recorded")
	}
	if !strings.Contains(checks, "--paginate") {
		t.Errorf("check-runs must be paginated or a failing lane past run 30 reads as green; got: %s", checks)
	}
}

// TestStalledPRs_NoBlatantMutationVerbs is defence in depth, NOT the guarantee.
// The behavioural test above is the guarantee; this only catches an obvious
// edit early, and three review rounds proved a static scan of shell source
// cannot be relied on for more than that.
func TestStalledPRs_NoBlatantMutationVerbs(t *testing.T) {
	src := readScript(t)
	var code []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code = append(code, line)
	}
	codeSrc := strings.Join(code, "\n")

	for _, verb := range []string{
		"pr merge", "pr edit", "pr close", "pr ready", "pr reopen",
		"pr comment", "pr review", "issue edit", "issue close", "issue comment",
		"label create", "workflow run", "git push", "git commit", "curl ", "wget ",
	} {
		if strings.Contains(codeSrc, verb) {
			t.Errorf("%s contains %q; this reporter must stay read-only (memql#2833)", stalledPRsScript, verb)
		}
	}
}

// classify exercises the bucketing function directly. It complements, rather
// than replaces, the end-to-end cases: combinations that are awkward to reach
// through the stub (precedence between two bad states) are cheap here.
func classify(t *testing.T, draft, mergeState, queued, checks, idle string) string {
	t.Helper()
	script := scriptBody(t) + "\nclassify " +
		strings.Join([]string{draft, mergeState, queued, checks, idle}, " ") + "\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = hermeticEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("classify(%s %s %s %s %s): %v", draft, mergeState, queued, checks, idle, err)
	}
	return strings.TrimSpace(string(out))
}

func TestStalledPRs_ClassifierPrecedence(t *testing.T) {
	cases := []struct {
		name                                  string
		draft, mergeState, queued, checks, id string
		want                                  string
	}{
		{"green and idle is the finding", "false", "CLEAN", "false", "green", "120", "STALLED"},
		{"within the idle window", "false", "CLEAN", "false", "green", "5", "FRESH"},
		{"merge conflict", "false", "DIRTY", "false", "green", "120", "CONFLICT"},
		// BEHIND is uppercase on the wire; spelled lowercase it could never
		// match and every out-of-date branch fell through to STALLED.
		{"behind base branch", "false", "BEHIND", "false", "green", "120", "BEHIND"},
		// BLOCKED is branch protection unsatisfied (usually an awaited
		// review), NOT a conflict -- calling it CONFLICT sends the operator
		// to rebase a perfectly mergeable branch.
		{"blocked on branch protection", "false", "BLOCKED", "false", "green", "120", "BLOCKED"},
		{"unstable merge state", "false", "UNSTABLE", "false", "green", "120", "UNKNOWN"},
		// A draft that is also red is still a draft: the author has not
		// offered it, so its checks are not the story.
		{"draft beats red", "true", "CLEAN", "false", "failing", "120", "DRAFT"},
		// The queue owns it, however long it has sat.
		{"queued beats idle", "false", "CLEAN", "true", "green", "9999", "QUEUED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(t, tc.draft, tc.mergeState, tc.queued, tc.checks, tc.id); got != tc.want {
				t.Errorf("classify(draft=%s state=%s queued=%s checks=%s idle=%s) = %q, want %q",
					tc.draft, tc.mergeState, tc.queued, tc.checks, tc.id, got, tc.want)
			}
		})
	}
}

// TestStalledPRs_CollapseRuns exercises the check-run reducer directly.
//
// Its predecessor asserted only that the script CONTAINED the word "skipped",
// and passed with the logic gutted, because the word survived in a comment.
func TestStalledPRs_CollapseRuns(t *testing.T) {
	body := scriptBody(t)
	call := func(runs string) string {
		q := "'" + strings.ReplaceAll(runs, "'", `'\''`) + "'"
		cmd := exec.Command("bash", "-c", body+"\ncollapse_runs "+q+"\n")
		cmd.Env = hermeticEnv()
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("collapse_runs(%q): %v", runs, err)
		}
		return strings.TrimSpace(string(out))
	}

	cases := []struct{ name, runs, want string }{
		// This repo path-filters CI lanes, so a healthy PR routinely carries
		// skipped runs; treating them as non-green would mark every PR RED.
		{"skipped counts as green", "success\nskipped\nsuccess", "green"},
		{"neutral counts as green", "success\nneutral", "green"},
		{"all success", "success\nsuccess", "green"},
		// A build that has already failed is failing, whatever is still running.
		{"failure outranks pending", "failure\nin_progress", "failing"},
		{"cancelled is failing", "cancelled\nsuccess", "failing"},
		{"timed out is failing", "timed_out", "failing"},
		{"still running", "in_progress\nsuccess", "pending"},
		{"queued", "queued", "pending"},
		{"unrecognised conclusion", "success\nsomething_new", "unknown"},
		{"no runs at all", "", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := call(tc.runs); got != tc.want {
				t.Errorf("collapse_runs(%q) = %q, want %q", tc.runs, got, tc.want)
			}
		})
	}
}

// --- memql#2887 -------------------------------------------------------------

// TestStalledPRs_HeadShaComesFromTheListCall pins finding 1: the head SHA must
// arrive in the `gh pr list` row, not in a per-PR `gh pr view`.
//
// Three assertions, because the obvious one is not sufficient. "No pr view" is
// satisfied just as well by dropping the check-run lookup altogether, which
// would make every PR UNKNOWN and the tool useless while this test stayed
// green -- so the third assertion follows the SHA from the list row all the
// way into the check-runs URL.
func TestStalledPRs_HeadShaComesFromTheListCall(t *testing.T) {
	res := runScript(t, oneStalledPR)

	for _, c := range res.calls {
		if strings.HasPrefix(c, "pr view") {
			t.Errorf("`gh pr view` is back: %q.\nThe head SHA is already served by `gh pr list`; fetching it per PR was a third of this tool's entire API budget (memql#2887).", c)
		}
	}

	// Exactly two per-PR calls: the liveness graphql and the check-runs fetch.
	perPR := 0
	for _, c := range res.calls {
		if strings.HasPrefix(c, "api") {
			perPR++
		}
	}
	if perPR != 2 {
		t.Errorf("got %d per-PR api calls, want 2 (liveness graphql + check-runs).\ncalls:\n  %s", perPR, strings.Join(res.calls, "\n  "))
	}

	// The SHA from the list row must be the one actually queried.
	wantURL := "commits/deadbeef/check-runs"
	found := false
	for _, c := range res.calls {
		if strings.Contains(c, wantURL) {
			found = true
		}
	}
	if !found {
		t.Errorf("no call fetched %q.\nThe SHA from the list row must reach the check-runs URL -- otherwise 'no pr view' is satisfied by not looking up checks at all.\ncalls:\n  %s",
			wantURL, strings.Join(res.calls, "\n  "))
	}
}

// TestStalledPRs_IdlenessKeysOnTheHeadSha pins finding 3, and it is the one
// test here whose pre-fix failure is a WRONG CLASSIFICATION rather than a stub
// refusal: under `updatedAt` the first case reads FRESH.
//
// The two cases are deliberately inverse, so that no cheaper rule passes both:
// keying on updatedAt fails the first, max(commit, updated) fails the first
// too, and min(commit, updated) fails the second. Only the commit date alone
// satisfies both.
func TestStalledPRs_IdlenessKeysOnTheHeadSha(t *testing.T) {
	now := time.Now().Unix()
	recent := fmt.Sprintf("%d", now-60) // one minute ago
	ancient := "1000000000"             // 2001

	cases := []struct {
		name        string
		updatedAt   string
		commitEpoch string
		want        string
		why         string
	}{
		{
			name: "stale commit, fresh comment", updatedAt: recent, commitEpoch: ancient, want: "STALLED",
			why: "a dying session that posts a review summary before it stops must not reset its own clock -- that is the exact failure memql#2887 exists to close",
		},
		{
			name: "fresh commit, stale everything else", updatedAt: ancient, commitEpoch: recent, want: "FRESH",
			why: "a PR pushed a minute ago is being worked on, whatever its updatedAt says",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ghStub{
				prList:    "7\tfalse\tCLEAN\t" + tc.updatedAt + "\tdeadbeef\tissue/7-x\ta pr\n",
				liveness:  "false\tdeadbeef\t" + tc.commitEpoch,
				checkRuns: "success\n",
			}
			res := runScript(t, cfg)
			buckets := bucketColumn(res.stdout)
			if len(buckets) != 1 || buckets[0] != tc.want {
				t.Errorf("got %v, want [%s].\n%s\nstdout:\n%s", buckets, tc.want, tc.why, res.stdout)
			}

			// The IDLE and TOUCHED columns are the user-visible half of this
			// finding, and without asserting them both swapping the two and
			// deleting TOUCHED outright leave the suite green.
			//
			// Row shape: #<num> <STATE> <IDLE> <TOUCHED> <CHECKS> <TITLE...>
			var row []string
			for _, line := range strings.Split(openSection(res.stdout), "\n") {
				if strings.HasPrefix(line, "#") {
					row = strings.Fields(line)
				}
			}
			if len(row) < 5 {
				t.Fatalf("could not parse a report row from:\n%s", res.stdout)
			}
			idleCol, touchedCol := row[2], row[3]
			// The commit is the gate, so IDLE tracks it; TOUCHED tracks
			// updatedAt. In both cases exactly one of them is ~1 minute.
			if tc.want == "STALLED" && !strings.HasPrefix(touchedCol, "1m") {
				t.Errorf("TOUCHED = %q, want ~1m: updatedAt is recent in this case and TOUCHED is what displays it", touchedCol)
			}
			if tc.want == "FRESH" && !strings.HasPrefix(idleCol, "1m") {
				t.Errorf("IDLE = %q, want ~1m: the head commit is recent in this case and IDLE is what displays it", idleCol)
			}
			if idleCol == touchedCol {
				t.Errorf("IDLE and TOUCHED are both %q; this case was built so they differ, so one of them is not reading what it claims", idleCol)
			}
		})
	}

	// The header must name both columns, in order.
	res := runScript(t, ghStub{
		prList:    "7\tfalse\tCLEAN\t1000000000\tdeadbeef\tissue/7-x\ta pr\n",
		liveness:  "false\tdeadbeef\t1000000000",
		checkRuns: "success\n",
	})
	iIdle := strings.Index(res.stdout, "IDLE")
	iTouched := strings.Index(res.stdout, "TOUCHED")
	if iIdle < 0 || iTouched < 0 || iIdle > iTouched {
		t.Errorf("the header must carry IDLE then TOUCHED; got idle=%d touched=%d in:\n%s", iIdle, iTouched, res.stdout)
	}
}

// TestStalledPRs_ClosedUnmergedIsSwept pins finding 2, one case per decision
// branch.
//
// Every branch is covered on purpose: a fixture set that only ever reached one
// outcome would leave the others unexercised while the suite stayed green,
// which is how the sibling stale-claims suite once shipped a sweep that could
// not report anything at all.
func TestStalledPRs_ClosedUnmergedIsSwept(t *testing.T) {
	// RECENT, not the 2001 epoch the open fixtures use. The sweep has an age
	// gate (CLOSED_MAX_AGE_MINUTES, 14 days) that skips old PRs BEFORE
	// spending an API call on them, so a 2001 closedAt would be skipped and
	// every case here would assert against an empty section.
	closedRow := fmt.Sprintf("11\tissue/11-thing\t%d\ta closed pr\n", time.Now().Unix()-3600)

	cases := []struct {
		name        string
		facts       string
		wantBucket  string
		wantPrinted bool
		why         string
	}{
		{
			name: "branch alive and issue open is a finding", facts: "present\t101",
			wantBucket: "LOST-WORK", wantPrinted: true,
			why: "closed without merging, branch still on the remote, issue back in the pool -- the work is written and about to be rebuilt",
		},
		{
			name: "branch deleted is not a finding", facts: "gone\t-",
			wantBucket: "BRANCH-GONE", wantPrinted: false,
			why: "nothing is recoverable, so there is nothing to tell anyone",
		},
		{
			name: "live branch with no open issue is a weaker finding", facts: "present\t-",
			wantBucket: "NO-OPEN-ISSUE", wantPrinted: true,
			why: "the branch is demonstrably still there, and the candidate set is deliberately NOT narrowed with `linked:issue` precisely to catch PRs whose \"Closes #N\" link never formed -- discarding them here would make that widening buy nothing",
		},
		{
			name: "lookup failure is UNKNOWN, never silence", facts: "",
			wantBucket: "UNKNOWN", wantPrinted: true,
			why: "default-deny cuts both ways: an unresolved input must not manufacture a finding, and must not vanish either",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := oneStalledPR
			cfg.closedList = closedRow
			cfg.closedFacts = tc.facts

			res := runScript(t, cfg)
			if res.code != 0 {
				t.Fatalf("exit = %d, want 0\nstderr: %s", res.code, res.stderr)
			}

			buckets := bucketsIn(closedSection(res.stdout))
			if tc.wantPrinted {
				if len(buckets) != 1 || buckets[0] != tc.wantBucket {
					t.Errorf("closed-sweep buckets = %v, want [%s].\n%s\nstdout:\n%s", buckets, tc.wantBucket, tc.why, res.stdout)
				}
			} else if len(buckets) != 0 {
				t.Errorf("closed-sweep printed %v; this case must not be reported.\n%s\nstdout:\n%s", buckets, tc.why, res.stdout)
			}

			// The open-PR report must be unaffected in every case.
			if open := bucketColumn(res.stdout); len(open) != 1 || open[0] != "STALLED" {
				t.Errorf("the open-PR table changed to %v; the second sweep must not disturb the first", open)
			}

			// The read-only guarantee covers the SECOND sweep too. Without
			// this the ~65 lines of report_closed_unmerged sat entirely
			// outside the property the script header claims is enforced
			// behaviourally -- TestStalledPRs_MakesNoMutatingCall's fixture
			// has no closed rows, so its per-PR loop never ran.
			if got := res.forbidden(); len(got) > 0 {
				t.Errorf("the closed-unmerged sweep invoked gh in a way the stub refuses:\n  %s", strings.Join(got, "\n  "))
			}
		})
	}
}

// TestStalledPRs_ClosedSweepSkipsAncientPRsWithoutCallingOut pins the age gate.
//
// Without it the sweep spends one GraphQL call per closed-unmerged PR in the
// repo's entire history on EVERY run: measured at 40 such PRs on this repo, a
// run went from 10 API calls to 48 -- roughly five times what finding 1's
// optimisation saved, to report nothing, since 38 of the 40 were already
// branch-deleted.
//
// The skip must be LOUD. A silently truncated watchdog is the failure mode
// this script's header spends a paragraph rejecting.
func TestStalledPRs_ClosedSweepSkipsAncientPRsWithoutCallingOut(t *testing.T) {
	cfg := oneStalledPR
	// 2001: far outside any sane CLOSED_MAX_AGE_MINUTES.
	cfg.closedList = "11\tissue/11-thing\t1000000000\tan ancient closed pr\n"
	cfg.closedFacts = "present\t101" // would be LOST-WORK if it were examined

	res := runScript(t, cfg)

	if got := bucketsIn(closedSection(res.stdout)); len(got) != 0 {
		t.Errorf("an ancient closed PR was examined and reported as %v; the age gate must skip it before spending an API call", got)
	}
	// Match closingIssuesReferences, not "headRef": both `pr list` calls carry
	// headRefOid / headRefName in their --json, so the looser pattern matched
	// them and this assertion failed on calls the gate has nothing to do with.
	// closingIssuesReferences appears only in closed_pr_facts' document.
	for _, c := range res.calls {
		if strings.Contains(c, "closingIssuesReferences") {
			t.Errorf("closed_pr_facts was called for a PR past the age gate: %q.\nThe gate exists to avoid exactly this call.", c)
		}
	}
	if !strings.Contains(res.stdout, "NOT examined") {
		t.Errorf("the skip must be announced -- a silently truncated sweep reads as an all-clear.\ngot:\n%s", res.stdout)
	}
}

// TestStalledPRs_SupersededIsNotLostWork: a closed PR whose head branch is
// also the head of a PR that is open RIGHT NOW was replaced, not lost.
// Reporting it would send someone to rescue work that is already back in
// flight, which is worse than saying nothing.
func TestStalledPRs_SupersededIsNotLostWork(t *testing.T) {
	cfg := oneStalledPR
	// The open fixture's head branch is issue/7-x; the closed PR shares it.
	cfg.closedList = fmt.Sprintf("11\tissue/7-x\t%d\ta superseded pr\n", time.Now().Unix()-3600)
	cfg.closedFacts = "present\t101" // branch alive, issue open: LOST-WORK but for the match

	res := runScript(t, cfg)

	if got := bucketsIn(closedSection(res.stdout)); len(got) != 0 {
		t.Errorf("a closed PR whose branch is the head of a LIVE open PR was reported as %v; it was superseded, not lost", got)
	}
}

// TestStalledPRs_ClosedSweepFailureIsNotAnAllClear: if the closed list cannot
// be fetched, the tool must say that half of the report is MISSING rather than
// print nothing and let a reader infer there is nothing to find.
func TestStalledPRs_ClosedSweepFailureIsNotAnAllClear(t *testing.T) {
	cfg := oneStalledPR
	cfg.closedListFail = true

	res := runScript(t, cfg)

	// The open report is still good, so the run still succeeds.
	if res.code != 0 {
		t.Errorf("exit = %d, want 0: a failed SECOND sweep must not discard a good first one", res.code)
	}
	if !strings.Contains(res.stderr, "MISSING") {
		t.Errorf("a failed closed sweep must say so on stderr; got:\n%s", res.stderr)
	}
	if open := bucketColumn(res.stdout); len(open) != 1 || open[0] != "STALLED" {
		t.Errorf("the open-PR table = %v, want [STALLED]; it is unaffected by the closed sweep failing", open)
	}
}

// TestStalledPRs_ScriptIsValidBash is a cheap parse gate. The rest of this file
// executes the script, so a syntax error surfaces as a confusing exit code in
// whichever test happens to run first; this one names it.
func TestStalledPRs_ScriptIsValidBash(t *testing.T) {
	cmd := exec.Command("bash", "-n", stalledPRsScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bash -n %s failed: %v\n%s", stalledPRsScript, err, out)
	}
}

// TestStalledPRs_UnknownCloseDateStaysInTheSweep pins the age gate's
// default-deny half, which was previously asserted only by a comment.
//
// Mutating the gate from `age != unknown && age > max` to
// `age == unknown || age > max` -- so a failed date parse DELETES a PR from a
// watchdog -- passed the entire suite before this test existed.
func TestStalledPRs_UnknownCloseDateStaysInTheSweep(t *testing.T) {
	for _, closedAt := range []string{"-", ""} {
		t.Run("closedAt="+closedAt, func(t *testing.T) {
			cfg := oneStalledPR
			cfg.closedList = "11\tissue/11-thing\t" + closedAt + "\ta closed pr\n"
			cfg.closedFacts = "present\t101"

			res := runScript(t, cfg)

			got := bucketsIn(closedSection(res.stdout))
			if len(got) != 1 || got[0] != "LOST-WORK" {
				t.Errorf("closed-sweep buckets = %v, want [LOST-WORK].\nAn unresolvable close date must keep the PR IN the sweep -- excluding it silently is the same class of failure as manufacturing a finding.\nstdout:\n%s", got, res.stdout)
			}
		})
	}
}

// TestStalledPRs_ClosedMaxAgeIsValidated: the knob that gates the sweep must
// be validated like IDLE_MINUTES is.
//
// Unvalidated, CLOSED_MAX_AGE_MINUTES=14d made `[ "$age" -gt "14d" ]` error,
// execution fell through, every candidate was examined anyway, and the
// operator saw a normal report believing a 14-day bound was in force.
func TestStalledPRs_ClosedMaxAgeIsValidated(t *testing.T) {
	// "" is NOT in this list: `${CLOSED_MAX_AGE_MINUTES:-20160}` treats
	// set-but-empty as unset, so an empty value means "use the default" --
	// the same contract IDLE_MINUTES has. Rejecting it would break
	// `CLOSED_MAX_AGE_MINUTES= make prs-stalled`.
	for _, bad := range []string{"14d", "99999999999999999999", " 60", "-5"} {
		t.Run("CLOSED_MAX_AGE_MINUTES="+bad, func(t *testing.T) {
			res := runScript(t, oneStalledPR, "CLOSED_MAX_AGE_MINUTES="+bad)
			if res.code != 2 {
				t.Errorf("exit = %d, want 2 for CLOSED_MAX_AGE_MINUTES=%q.\nSilently ignoring it disables the age gate while the report still looks normal.\nstderr: %s",
					res.code, bad, res.stderr)
			}
			if !strings.Contains(res.stderr, "CLOSED_MAX_AGE_MINUTES") {
				t.Errorf("the error must name the offending knob; got: %s", res.stderr)
			}
		})
	}

	// A valid value still works, so the guard cannot be satisfied by rejecting
	// everything.
	if res := runScript(t, oneStalledPR, "CLOSED_MAX_AGE_MINUTES=60"); res.code != 0 {
		t.Errorf("a valid CLOSED_MAX_AGE_MINUTES must still run; exit=%d stderr=%s", res.code, res.stderr)
	}
}

// TestStalledPRs_SupersededIsCounted: suppressing a row without saying so
// leaves a reader unable to tell "found nothing" from "hid five findings".
func TestStalledPRs_SupersededIsCounted(t *testing.T) {
	cfg := oneStalledPR
	cfg.closedList = fmt.Sprintf("11\tissue/7-x\t%d\ta superseded pr\n", time.Now().Unix()-3600)
	cfg.closedFacts = "present\t101"

	res := runScript(t, cfg)

	if !strings.Contains(res.stdout, "SUPERSEDED") {
		t.Errorf("a suppressed superseded PR must be counted and announced -- every other suppression in this script is.\nstdout:\n%s", res.stdout)
	}
	if got := bucketsIn(closedSection(res.stdout)); len(got) != 0 {
		t.Errorf("it must be counted, not listed as a finding; got rows %v", got)
	}
}

// TestStalledPRs_TruncatedBranchIsMarked: BRANCH is the copy-pasteable field
// and the summary tells the reader to go check it, so a silently shortened ref
// is worse than a long line -- it looks like a branch name and `git fetch`
// fails on it.
func TestStalledPRs_TruncatedBranchIsMarked(t *testing.T) {
	long := "issue/11-" + strings.Repeat("x", 60)
	cfg := oneStalledPR
	cfg.closedList = fmt.Sprintf("11\t%s\t%d\ta closed pr\n", long, time.Now().Unix()-3600)
	cfg.closedFacts = "present\t101"

	res := runScript(t, cfg)
	sec := closedSection(res.stdout)

	if strings.Contains(sec, long) {
		return // not truncated at all: nothing to mark
	}
	if !strings.Contains(sec, "...") {
		t.Errorf("a truncated branch name must be marked; an unmarked one is indistinguishable from a real, shorter ref.\nstdout:\n%s", sec)
	}
}

// TestStalledPRs_EmptyBranchIsNotSuperseded pins the emptiness half of the
// SUPERSEDED guard.
//
// open_branches is built by appending a newline per row and is then fed to
// grep through a herestring, so it ALWAYS carries a trailing empty line -- and
// `grep -Fxq -- ""` matches an empty line. Without the `-n` test, a closed PR
// whose branch_name came through empty matches every time and its LOST-WORK
// finding disappears into the superseded count with no row.
//
// Verified by mutation: removing `[ -n "$branch_name" ] &&` turns the expected
// LOST-WORK row into a bare "1 ... SUPERSEDED" note.
func TestStalledPRs_EmptyBranchIsNotSuperseded(t *testing.T) {
	cfg := oneStalledPR
	// One field only: branch_name, closed_epoch and title all read empty.
	// An empty closed_epoch means an unknown age, which keeps the PR in the
	// sweep, so this drives the guard rather than the age gate.
	cfg.closedList = "11\n"
	cfg.closedFacts = "present\t101"

	res := runScript(t, cfg)

	got := bucketsIn(closedSection(res.stdout))
	if len(got) != 1 || got[0] != "LOST-WORK" {
		t.Errorf("closed-sweep buckets = %v, want [LOST-WORK].\nAn empty branch name must not match the trailing blank line in open_branches and vanish as SUPERSEDED.\nstdout:\n%s", got, res.stdout)
	}
	if strings.Contains(res.stdout, "SUPERSEDED") {
		t.Errorf("an empty branch name was counted as superseded; the guard is not holding.\nstdout:\n%s", res.stdout)
	}
}
