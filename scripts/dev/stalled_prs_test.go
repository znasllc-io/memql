// Contract gate for scripts/dev/stalled-prs.sh (znasllc-io/memql#2833).
package dev

import (
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

// ghInvocations returns the argument text of every `gh` command in line.
//
// Two subtleties, both of which produced a wrong answer on the first attempt:
//
//   - `gh` inside a quoted string is prose, not a call. The script's own
//     "not authenticated (gh auth login)" error message tripped a naive scan.
//   - `gh` as an ARGUMENT is not an invocation either: `command -v gh` is
//     preceded by whitespace just like a real call, so command position has to
//     mean "line start, or right after ; & | ( or !", not merely "after a space".
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
		if c != 'g' || i+2 >= len(line) || line[i+1] != 'h' || (line[i+2] != ' ' && line[i+2] != '\t') {
			continue
		}
		j := i - 1
		for j >= 0 && (line[j] == ' ' || line[j] == '\t') {
			j--
		}
		if j >= 0 && !strings.ContainsRune(";&|(!", rune(line[j])) {
			continue // an argument (e.g. `command -v gh`), not a command
		}
		out = append(out, strings.TrimSpace(line[i+2:]))
	}
	return out
}

// readOnlyForms are the only gh invocations this script may make. Each is a
// GET (or a GraphQL query, checked separately below).
var readOnlyForms = []*regexp.Regexp{
	regexp.MustCompile(`^auth status\b`),
	regexp.MustCompile(`^pr list\b`),
	regexp.MustCompile(`^pr view\b`),
	regexp.MustCompile(`^api graphql\b`),
	regexp.MustCompile(`^api "?repos/`),
}

// writeFlags turn a `gh api` call into a write. -X/--method is the obvious
// one; the subtle one is that gh defaults to POST as soon as ANY field flag is
// present, so `gh api repos/o/r/issues/1/comments -f body=hi` posts a comment
// with no -X anywhere in sight.
var writeFlags = []string{"-X", "--method", "-f ", "--field", "-F ", "--raw-field", "--input"}

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
				// the document: a query is read-only, a mutation is not.
				if strings.Contains(inv, "mutation") {
					t.Errorf("gh api graphql invocation carries a mutation: %q", inv)
				}
				continue
			}
			for _, flag := range writeFlags {
				if strings.Contains(inv, flag) {
					t.Errorf("gh invocation %q carries write flag %q; gh sends POST as soon as a field flag is present", inv, flag)
				}
			}
		}
	}

	if found == 0 {
		t.Fatal("no gh invocations found; the allowlist scan is vacuous -- check ghInvocation still matches the script")
	}

	// Nothing may reach GitHub except through gh, where the allowlist can see it.
	for _, backdoor := range []string{"curl ", "wget ", "git push", "git commit"} {
		if strings.Contains(codeSrc, backdoor) {
			t.Errorf("%s uses %q; every GitHub call must go through gh so the allowlist above can vet it", stalledPRsScript, backdoor)
		}
	}
}

// TestStalledPRs_ReportGoesToStdout keeps `make prs-stalled > report.txt`
// working. The first cut printed the whole table to stderr, so redirecting
// produced an empty file.
func TestStalledPRs_ReportGoesToStdout(t *testing.T) {
	for _, line := range strings.Split(readScript(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "printf ") {
			continue
		}
		if strings.Contains(trimmed, ">&2") {
			t.Errorf("report line writes to stderr, so `make prs-stalled > file` yields an empty file: %s", trimmed)
		}
	}
}

// TestStalledPRs_SkippedChecksCountAsGreen pins one classification detail that
// is easy to get wrong and would make the report useless: this repo
// path-filters several CI lanes, so a healthy PR routinely carries `skipped`
// runs. Treating those as non-green would mark every PR RED.
func TestStalledPRs_SkippedChecksCountAsGreen(t *testing.T) {
	if !strings.Contains(readScript(t), "skipped") {
		t.Error("check_state does not mention `skipped`; path-filtered lanes are normal here, so a healthy PR would classify as RED")
	}
}
