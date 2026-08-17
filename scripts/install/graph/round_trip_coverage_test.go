// round_trip_coverage_test.go -- znasllc-io/memql#4071.
//
// THE ASSERTION THIS FILE EXISTS FOR: a `--skip` cannot quietly empty out the
// round trip.
//
// graph_test.go holds install and uninstall to each other STRUCTURALLY: every
// receipt has a reversal, every mutation is accounted for, teardown is install
// reversed. What it cannot hold is the thing those invariants assume and none of
// them can check -- that the set of things a step WRITES equals the set its
// reversal REMOVES. Both halves of that sentence are facts about shell scripts,
// not about the graph document, so the only thing that can hold them to account
// is the before/after diff of a real machine: the round-trip E2E lanes.
//
// Which makes the lanes' COVERAGE load-bearing, and coverage is exactly what a
// `--skip=` flag removes without leaving a mark. memql#4071 is the worked
// example: `localCA` writes four things (the CA, its provenance marker, the
// front-door certificate and its private key) and its reversal removed the
// first two, so an uninstall reported OK on all seven steps and left a private
// key at ~/.memql/certs/dev.key. Every graph invariant was green -- the step HAS
// a reversal -- and the round trip that would have seen the leftover file ran
// with `--skip=localCA`, so the artifact was never created there and the diff
// had nothing to see. The defect survived for as long as the only lane that
// could see it was narrowed past it.
//
// So: every install step that leaves an artifact behind must be RUN, unskipped,
// by at least one lane that both installs and uninstalls. Skipping it in one
// lane is fine and often necessary (install-e2e.yml skips `localCA` because
// `mkcert -install` writes the runner's system trust store, which is not one of
// the four surfaces it diffs); skipping it in ALL of them means nothing on earth
// checks that its reversal works.
//
// WHAT THIS GATE CANNOT SEE, said rather than implied:
//
//   - It reads the workflow SOURCE, not a run. A lane behind an `if:` that never
//     fires still counts as coverage here.
//   - It says nothing about whether the diff those lanes take is WIDE enough. A
//     step exercised by a lane whose snapshot does not cover the surface it
//     writes to is invisible to both the lane and this gate; the trust store is
//     exactly that case, and both workflows say so in their own headers.
//   - It is about coverage, not correctness. That a step ran unskipped is not
//     evidence its reversal is complete -- only the diff can be that, which is
//     why this is a gate ON the lanes rather than a replacement for them.
package graph

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// laneGlob matches the round-trip workflow files. Both shipped lanes -- the
// cheap no-cluster one and the full cluster one -- are named this way, and a
// third would be too.
const laneGlob = ".github/workflows/install*e2e*.yml"

var (
	// installInvocationRe finds the start of one `cli.js install` command.
	installInvocationRe = regexp.MustCompile(`cli\.js\s+install\b`)
	// uninstallInvocationRe is what makes a lane a ROUND TRIP rather than an
	// install test: a lane that never uninstalls proves nothing about reversal,
	// so its coverage must not count towards this gate.
	uninstallInvocationRe = regexp.MustCompile(`cli\.js\s+uninstall\b`)
	// skipFlagRe reads a --skip=a,b,c off a command line.
	skipFlagRe = regexp.MustCompile(`--skip=([A-Za-z0-9,]+)`)
)

// laneInvocation is one `cli.js install` command found in a round-trip lane,
// with the steps it declined to run.
type laneInvocation struct {
	lane    string
	line    int
	skipped map[string]bool
}

// commandAt returns the whole shell command starting at line i, following
// backslash continuations. The flags of an install invocation are spread over a
// dozen continued lines in both lanes, so reading one line would find the
// command and none of its arguments.
func commandAt(lines []string, i int) string {
	var b strings.Builder
	for ; i < len(lines); i++ {
		line := lines[i]
		b.WriteString(line)
		b.WriteString(" ")
		if !strings.HasSuffix(strings.TrimRight(line, " \t"), `\`) {
			break
		}
	}
	return b.String()
}

// roundTripInvocations reads every `cli.js install` in every lane that also
// uninstalls, with the skip set each one declares.
func roundTripInvocations(t *testing.T, root string) []laneInvocation {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, laneGlob))
	if err != nil {
		t.Fatalf("glob %s: %v", laneGlob, err)
	}
	var out []laneInvocation
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		body := string(b)
		if !uninstallInvocationRe.MatchString(body) {
			continue // installs but never reverses: not a round trip
		}
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			if !installInvocationRe.MatchString(line) {
				continue
			}
			inv := laneInvocation{lane: filepath.Base(p), line: i + 1, skipped: map[string]bool{}}
			for _, m := range skipFlagRe.FindAllStringSubmatch(commandAt(lines, i), -1) {
				for _, id := range strings.Split(m[1], ",") {
					if id = strings.TrimSpace(id); id != "" {
						inv.skipped[id] = true
					}
				}
			}
			out = append(out, inv)
		}
	}
	return out
}

// TestEveryInstallStepIsExercisedByARoundTrip is the gate. A receipt-writing
// step skipped by every round-trip invocation has no lane anywhere that could
// notice its reversal is incomplete.
func TestEveryInstallStepIsExercisedByARoundTrip(t *testing.T) {
	root := repoRoot(t)
	invocations := roundTripInvocations(t, root)
	// A gate that cannot fire proves nothing. If the lanes are renamed, moved or
	// deleted, this must say so rather than pass by finding nothing to check.
	if len(invocations) == 0 {
		t.Fatalf("no `cli.js install` invocation found in any round-trip lane (%s) -- "+
			"either the lanes are gone or this parser is broken; both mean the install/uninstall "+
			"round trip is no longer checked by anything", laneGlob)
	}

	in, _ := bothGraphs(t)
	for _, inv := range invocations {
		for id := range inv.skipped {
			if in.Step(id) == nil {
				t.Errorf("%s:%d skips %q, which is not a step in the install graph -- "+
					"a skip naming a step that no longer exists is a control that does nothing",
					inv.lane, inv.line, id)
			}
		}
	}

	for i := range in.Steps {
		s := &in.Steps[i]
		if s.Receipt == "" {
			continue // leaves nothing behind, so a round trip has nothing to check
		}
		exercised := false
		var by []string
		for _, inv := range invocations {
			if !inv.skipped[s.ID] {
				exercised = true
				break
			}
			by = append(by, inv.lane)
		}
		if exercised {
			continue
		}
		sort.Strings(by)
		t.Errorf("install step %q writes the %q receipt and EVERY round-trip lane skips it (%s) -- "+
			"so no before/after diff ever creates its artifact, and nothing can notice that its "+
			"reversal is incomplete. That is how memql#4071 survived: the step had a reversal, the "+
			"reversal did not remove the private key it wrote, and the only lane that could have "+
			"seen the leftover ran with --skip=%s. Unskip it in at least one lane.",
			s.ID, s.Receipt, strings.Join(dedupe(by), ", "), s.ID)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
