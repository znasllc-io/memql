// Contract gate for scripts/dev/stalled-prs.sh (znasllc-io/memql#2833).
package dev

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
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

// classify exercises the script's bucketing function directly, by sourcing the
// script with main() suppressed. The classifier is a pure function of five
// strings, so it can be tested without touching the GitHub API -- which
// matters, because the buckets that are hardest to observe live (DRAFT,
// CONFLICT, and every "could not determine" state) are exactly the ones a
// wrong rule would silently mislabel as STALLED.
func classify(t *testing.T, draft, mergeState, queued, checks, idle string) string {
	t.Helper()
	// `main "$@"` runs on source, so strip the trailing invocation.
	body := strings.Replace(readScript(t), "\nmain \"$@\"\n", "\n", 1)

	script := body + "\nclassify " +
		strings.Join([]string{draft, mergeState, queued, checks, idle}, " ") + "\n"
	out, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("classify(%s %s %s %s %s): %v", draft, mergeState, queued, checks, idle, err)
	}
	return strings.TrimSpace(string(out))
}

// TestStalledPRs_ClassifierBuckets pins the rule.
//
// STALLED is the only finding, and it invites a human to enqueue, so it must
// be reported ONLY when every input is positively known good. The first cut of
// this script made STALLED the fallthrough instead, which meant one transient
// API failure -- or a mergeability GitHub had not finished computing --
// manufactured a finding for a PR whose CI may never have run.
func TestStalledPRs_ClassifierBuckets(t *testing.T) {
	cases := []struct {
		name                                  string
		draft, mergeState, queued, checks, id string
		want                                  string
	}{
		// The finding: green, mergeable, unqueued, idle past the threshold.
		{"green and idle", "false", "CLEAN", "false", "green", "120", "STALLED"},

		// Known-bad states, each reported as itself.
		{"already queued", "false", "CLEAN", "true", "green", "120", "QUEUED"},
		{"draft", "true", "CLEAN", "false", "green", "120", "DRAFT"},
		{"failing checks", "false", "CLEAN", "false", "failing", "120", "RED"},
		{"checks still running", "false", "CLEAN", "false", "pending", "120", "PENDING"},
		{"merge conflict", "false", "DIRTY", "false", "green", "120", "CONFLICT"},
		{"blocked", "false", "BLOCKED", "false", "green", "120", "CONFLICT"},
		{"within the idle window", "false", "CLEAN", "false", "green", "5", "FRESH"},

		// BEHIND is uppercase on the wire. The first cut spelled this rule
		// `behind`, so it could never match and every out-of-date branch fell
		// through to STALLED.
		{"behind base branch", "false", "BEHIND", "false", "green", "120", "CONFLICT"},

		// Default-deny. Every one of these reached STALLED before the fix.
		{"mergeability not yet computed", "false", "UNKNOWN", "false", "green", "120", "UNKNOWN"},
		{"unstable merge state", "false", "UNSTABLE", "false", "green", "120", "UNKNOWN"},
		{"merge-queue lookup failed", "false", "CLEAN", "unknown", "green", "120", "UNKNOWN"},
		{"check lookup failed", "false", "CLEAN", "false", "unknown", "120", "UNKNOWN"},
		{"unreadable timestamp", "false", "CLEAN", "false", "green", "-1", "UNKNOWN"},

		// CI never ran. Distinct from UNKNOWN: the API answered, and the answer
		// was "no check runs" -- which is the state where "ready to merge" is
		// least trustworthy, so it is never a STALLED finding.
		{"no check runs at all", "false", "CLEAN", "false", "none", "120", "NO-CI"},

		// Precedence: a draft that is also red is still reported as a draft --
		// the author has not offered it, so its checks are not the story.
		{"draft beats red", "true", "CLEAN", "false", "failing", "120", "DRAFT"},
		// Queued beats idle: the queue owns it, however long it has sat.
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

func identByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ghInvocations returns the text following EVERY `gh` token on a code line,
// outside quotes.
//
// The previous version tried to recognise "command position" -- line start, or
// after ; & | ( ! -- and skipped every other occurrence as "an argument". That
// made the FINDER fail open even though the classifier failed closed, and 19 of
// 33 probed mutations walked through the gap. The worst was the likeliest edit
// anyone would actually make:
//
//	if [ "$bucket" = "STALLED" ]; then gh pr merge "$num" --squash --auto; fi
//
// `then gh` is not after a metacharacter, so it was never even examined. Also
// missed: backticks (only `$(` was in the set), `command gh`, `env gh`,
// `xargs gh`, and `G=gh; $G pr merge`.
//
// So this no longer tries to decide which occurrences are invocations. EVERY
// `gh` token must justify itself against the allowlist. That is why the script
// probes with `gh --version` instead of `command -v gh`: it removes the one
// legitimate non-invocation occurrence rather than carving out an exception
// that a mutation could hide behind.
func ghInvocations(line string) []string {
	var out []string
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && inDouble {
			i++
			continue
		}
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble {
			continue
		}
		if c != 'g' || i+1 >= len(line) || line[i+1] != 'h' {
			continue
		}
		if i > 0 && identByte(line[i-1]) {
			continue // part of a longer word
		}
		if i+2 < len(line) && identByte(line[i+2]) {
			continue // `ghost`, `ghcr`, ...
		}
		out = append(out, strings.TrimSpace(line[i+2:]))
	}
	return out
}

// readOnlyForms are the only gh invocations this script may make.
//
// `gh api` needs no path constraint: with no field flag and no -X it is a GET
// whatever the path, and writeFlags below is what enforces that. Constraining
// the path instead rejected legitimate reads (`--paginate`, `-H`, `gh api user`)
// while still admitting `gh api repos/... -f body=`, which is backwards.
var readOnlyForms = []*regexp.Regexp{
	regexp.MustCompile(`^--version\b`),
	regexp.MustCompile(`^auth status\b`),
	regexp.MustCompile(`^api\b`),
	regexp.MustCompile(`^(?:pr|issue|repo|label|run|release|cache) (?:list|view|diff|checks|status)\b`),
}

// writeFlagRe matches the flags that turn a `gh api` call into a write.
// -X/--method is the obvious one. The subtle one is that gh sends POST as soon
// as ANY field flag is present, so `gh api repos/o/r/issues/1/comments -f body=hi`
// posts a comment with no -X in sight -- and pflag accepts the shorthand
// ATTACHED (`-fbody=hi`), which a plain `strings.Contains(inv, "-f ")` missed.
var writeFlagRe = regexp.MustCompile(`(^|\s)(-X|--method|--input|--field|--raw-field|-f|-F)`)

// shellIndirection hides a call from any static scan, so it is banned outright
// rather than parsed. A read-only reporter has no use for eval or backticks.
var shellIndirection = []string{"eval ", "`", "xargs ", "command gh", "env gh"}

// TestStalledPRs_IsReadOnly is the load-bearing property, and the reason this
// tool reports instead of sweeping.
//
// Adversarial review of every PR merged on the night this was written found a
// real defect in each one, so "green" is demonstrably not "reviewed" here. A
// sweeper that enqueued automatically would land unreviewed work at exactly
// the moment nobody is watching -- and, being unattended, could also race a
// session that is still working.
//
// This is an ALLOW-list, not a denylist. The first cut denied eleven known-bad
// strings and caught 0 of 10 real mutations probed against it: `gh pr comment`,
// `gh pr review --approve`, `gh api ... -f body=` (POST by default, no -X),
// `gh api graphql -f query='mutation{...}'`, `gh label create`, `curl -X POST`
// and friends all sailed through -- and the last of those is a one-word edit
// from a call this script already makes. An allowlist fails closed: a gh
// invocation this test has not been taught is a failure, whether or not anyone
// thought of it in advance.
func TestStalledPRs_IsReadOnly(t *testing.T) {
	src := readScript(t)

	// Comments discuss mutations on purpose (the paragraph above this test is
	// mirrored in the script header), so judge code lines only.
	var code []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code = append(code, line)
	}
	// Join `\` continuations first: a flag on the second line of a wrapped
	// command is part of that command, and scanning line-by-line would let
	// `gh api "repos/..." \` + `-f body=hi` through with the field flag unseen.
	codeSrc := strings.ReplaceAll(strings.Join(code, "\n"), "\\\n", " ")

	found := 0
	for _, line := range strings.Split(codeSrc, "\n") {
		for _, inv := range ghInvocations(line) {
			found++

			allowed := false
			for _, form := range readOnlyForms {
				if form.MatchString(inv) {
					allowed = true
					break
				}
			}
			if !allowed {
				t.Errorf("unrecognised gh invocation %q\nthis reporter must stay read-only; add it to readOnlyForms only if it is provably a GET (memql#2833)", inv)
				continue
			}

			if strings.HasPrefix(inv, "api graphql") {
				// GraphQL is always POSTed, so the read/write distinction is in
				// the document, and the document must therefore be visible HERE.
				// Requiring an inline literal opening brace rejects both
				// `-f query='mutation{...}'` and the indirection that defeats a
				// substring scan: Q="mutation{...}"; gh api graphql -f query="$Q".
				if !strings.Contains(inv, `query="{`) && !strings.Contains(inv, "query='{") {
					t.Errorf("gh api graphql must pass an inline literal query beginning with `{`, so this test can see it is not a mutation: %q", inv)
				}
				continue
			}
			if m := writeFlagRe.FindString(inv); m != "" {
				t.Errorf("gh invocation %q carries write flag %q; gh sends POST as soon as a field flag is present", inv, strings.TrimSpace(m))
			}
		}
	}

	// The script makes five calls (--version, auth status, pr list, api graphql,
	// pr view, api check-runs). A collapse to zero would make this test vacuous.
	if found < 5 {
		t.Fatalf("found only %d gh invocations; the allowlist scan looks vacuous -- check ghInvocations still matches the script", found)
	}

	// Indirection would hide a call from the scan above, and nothing may reach
	// GitHub except through gh, where the allowlist can see it.
	for _, banned := range append(append([]string{}, shellIndirection...),
		"curl ", "wget ", "git push", "git commit") {
		if strings.Contains(codeSrc, banned) {
			t.Errorf("%s uses %q; it must not, because that hides the call from the allowlist above (memql#2833)", stalledPRsScript, banned)
		}
	}
}

// fakeGh writes a stub gh onto PATH so the script can be run end to end with
// no network. It answers exactly the six calls the script makes.
func fakeGh(t *testing.T, prListTSV string) string {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  --version) echo 'gh version 2.0.0'; exit 0 ;;\n" +
		"  auth) exit 0 ;;\n" +
		"  pr)\n" +
		"    case \"$2\" in\n" +
		"      list) printf '%s' " + shellQuote(prListTSV) + "; exit 0 ;;\n" +
		"      view) echo 'deadbeef'; exit 0 ;;\n" +
		"    esac ;;\n" +
		"  api)\n" +
		"    case \"$2\" in\n" +
		"      graphql) echo false; exit 0 ;;\n" +
		"      *) echo success; exit 0 ;;\n" +
		"    esac ;;\n" +
		"esac\n" +
		"echo \"unexpected gh call: $*\" >&2; exit 1\n"
	if err := os.WriteFile(dir+"/gh", []byte(stub), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return dir
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runScript executes the real script with a stubbed gh, returning stdout,
// stderr and the exit code separately.
func runScript(t *testing.T, prListTSV string, env ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("bash", stalledPRsScript)
	cmd.Env = append(append(os.Environ(), "PATH="+fakeGh(t, prListTSV)+":"+os.Getenv("PATH")), env...)
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
	return stdout.String(), stderr.String(), code
}

// TestStalledPRs_EndToEnd runs the whole script against a stub. It replaces a
// source-inspecting test that only looked at lines beginning `printf ` -- and
// so passed when the same regression was reintroduced as `echo >&2`.
func TestStalledPRs_EndToEnd(t *testing.T) {
	// updatedAt epoch 1000000000 is 2001, i.e. unambiguously stale.
	rows := "7\tfalse\tCLEAN\t1000000000\tan abandoned green pr\n"
	stdout, _, code := runScript(t, rows)

	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	// The whole report must be on stdout, or `make prs-stalled > file` is empty.
	for _, want := range []string{"PR", "STATE", "#7", "STALLED", "are STALLED"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; the report must go to stdout, not stderr.\ngot:\n%s", want, stdout)
		}
	}
}

// TestStalledPRs_BadThresholdIsRejected covers the knob that made the
// manufactured-finding bug reachable in production: `[ "$idle" -lt "$IDLE_MINUTES" ]`
// ERRORS on a non-integer instead of evaluating false, and an errored test falls
// through to the next line -- which was `echo "STALLED"`. A mistyped
// IDLE_MINUTES=45m therefore reported every PR, including one-minute-old ones,
// as stalled and exited 0.
func TestStalledPRs_BadThresholdIsRejected(t *testing.T) {
	rows := "7\tfalse\tCLEAN\t1000000000\ta pr\n"
	stdout, stderr, code := runScript(t, rows, "IDLE_MINUTES=45m")

	if code != 2 {
		t.Errorf("exit = %d, want 2 (bad param) for IDLE_MINUTES=45m", code)
	}
	if strings.Contains(stdout, "STALLED") {
		t.Errorf("a malformed IDLE_MINUTES produced a STALLED finding:\n%s", stdout)
	}
	if !strings.Contains(stderr, "IDLE_MINUTES") {
		t.Errorf("stderr should name the offending variable; got:\n%s", stderr)
	}
}

// TestStalledPRs_CollapseRuns exercises the check-run reducer directly.
//
// Its predecessor asserted only that the script CONTAINED the word "skipped".
// That passed even after the logic was gutted to `^(success)$`, because the
// word survived in a comment -- it tested prose, not behaviour.
func TestStalledPRs_CollapseRuns(t *testing.T) {
	body := strings.Replace(readScript(t), "\nmain \"$@\"\n", "\n", 1)
	call := func(runs string) string {
		out, err := exec.Command("bash", "-c", body+"\ncollapse_runs "+shellQuote(runs)+"\n").Output()
		if err != nil {
			t.Fatalf("collapse_runs(%q): %v", runs, err)
		}
		return strings.TrimSpace(string(out))
	}

	cases := []struct{ name, runs, want string }{
		// This repo path-filters CI lanes, so a healthy PR routinely carries
		// skipped runs. Treating those as non-green would mark every PR RED and
		// make the whole report useless.
		{"skipped counts as green", "success\nskipped\nsuccess", "green"},
		{"neutral counts as green", "success\nneutral", "green"},
		{"all success", "success\nsuccess", "green"},
		// A build that has already failed is failing, whatever is still running.
		{"failure outranks pending", "failure\nin_progress", "failing"},
		{"cancelled is failing", "cancelled\nsuccess", "failing"},
		{"timed out is failing", "timed_out", "failing"},
		{"still running", "in_progress\nsuccess", "pending"},
		{"queued", "queued", "pending"},
		// Never seen before: not green.
		{"unrecognised conclusion", "success\nsomething_new", "unknown"},
		// CI never triggered -- the least trustworthy "ready to merge" there is.
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
