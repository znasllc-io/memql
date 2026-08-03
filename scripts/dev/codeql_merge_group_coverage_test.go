// Static guard over the coverage that compensates for CodeQL's merge_group
// no-op (znasllc-io/memql#2973).
//
// # The tradeoff being guarded
//
// `.github/workflows/codeql.yml` runs a single `echo` on `merge_group` and
// skips checkout / init / autobuild / analyze. The job reports success and
// satisfies the required `Analyze (go)` status context without analysing
// anything. That is DELIBERATE and documented (#1539,
// docs/internal/ops/merge-queue.md): a SARIF upload keyed to the torn-down
// `gh-readonly-queue` ref wedges the queue.
//
// It is a DEFERRAL rather than a hole only because three other triggers cover
// the same code: `pull_request`, `push: [main]`, and the weekly `schedule`.
// Measured on live CI for #2973: of 15 merge-queue candidates in the retained
// window, 7 had already been analysed pre-merge (the PR-merge ref recomputes
// against current main, so the tree is frequently byte-identical), and for the
// 14 whose tree became a push tip the identical tree was analysed on
// `refs/heads/main` within 11.8 / 13.9 / 14.3 minutes (min / median / max).
// `push: main` is therefore the actual recovery path; the weekly cron was not
// the recovery path for any observed candidate.
//
// # What this guard is for
//
// Nothing in the repo noticed that dependency. Remove `push: main` -- a
// plausible cost-saving edit, since PRs are already analysed -- and the
// merge_group no-op silently stops being a deferral and starts being a hole:
// every queue candidate merges to main with a green `Analyze (go)` that
// analysed nothing, and the only remaining coverage is a weekly cron.
//
// So: while the no-op exists, the compensating triggers must exist too. This
// is #2973's option 1. The #1539 workaround itself is deliberately left alone.
//
// # Why it is written this way
//
// Parsed with yaml.v3 rather than scanned line by line, following this
// package's vscode_lane_scope_test.go -- whose header records that an
// adversarial review defeated a line-scanning version twice, by putting the
// expected pattern in a step's `name:` and in a trailing comment. Reading
// structure makes both unrepresentable rather than merely tested for.
//
// Asserted as an IMPLICATION, not as a fixed trigger list: if the no-op is
// removed (say #1539 is fixed properly, option 2), the guard stops demanding
// anything. It constrains the combination, which is the thing that is actually
// unsafe -- not the presence of any one trigger.
package dev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// codeqlWorkflow is the subset of codeql.yml this guard reads.
//
// `on` is `map[string]any` because its members have three different shapes --
// `merge_group:` is null, `push:` is a mapping, `schedule:` is a sequence --
// and the question here is only which keys are present.
type codeqlWorkflow struct {
	On   map[string]any `yaml:"on"`
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
			If   string `yaml:"if"`
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func codeqlYAML(t *testing.T) codeqlWorkflow {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".github", "workflows", "codeql.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf codeqlWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse .github/workflows/codeql.yml: %v", err)
	}
	if len(wf.On) == 0 {
		t.Fatal("codeql.yml parsed with no `on:` triggers at all -- the parse is wrong, not the " +
			"workflow. Note YAML 1.1 reads a bare `on` key as the boolean true; if that ever " +
			"starts biting, quote it in the workflow rather than weakening this guard (#2973)")
	}
	return wf
}

// hasMergeGroupNoOp reports whether the analyze job still short-circuits on
// merge_group: some step gated on the merge_group event, while the steps that
// do the actual analysis are gated on NOT being merge_group.
//
// Both halves are required. A step merely mentioning merge_group is not the
// no-op; what makes it a no-op is that `codeql-action/analyze` does not run.
func hasMergeGroupNoOp(wf codeqlWorkflow) bool {
	var gatedOnMergeGroup, analysisSkipped bool
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			cond := strings.ReplaceAll(s.If, " ", "")
			switch {
			case strings.Contains(cond, "github.event_name=='merge_group'"):
				gatedOnMergeGroup = true
			case strings.Contains(cond, "github.event_name!='merge_group'") &&
				strings.Contains(s.Uses, "codeql-action/analyze"):
				analysisSkipped = true
			}
		}
	}
	return gatedOnMergeGroup && analysisSkipped
}

// TestCodeQLMergeGroupNoOpKeepsItsCompensatingTriggers is the guard.
func TestCodeQLMergeGroupNoOpKeepsItsCompensatingTriggers(t *testing.T) {
	wf := codeqlYAML(t)

	if _, ok := wf.On["merge_group"]; !ok {
		t.Skip("codeql.yml no longer subscribes to merge_group, so there is no no-op to " +
			"compensate for and nothing for this guard to require (#2973)")
	}
	if !hasMergeGroupNoOp(wf) {
		t.Skip("codeql.yml subscribes to merge_group but no longer short-circuits on it -- the " +
			"candidate is presumably analysed for real now (#2973 option 2), so the " +
			"compensating triggers are no longer load-bearing")
	}

	// The no-op is present. Both compensating triggers must be too.
	for _, trig := range []struct{ key, why string }{
		{"push",
			"the MEASURED recovery path. For the 14 merge-queue candidates in #2973's window " +
				"whose tree became a push tip, the identical tree was analysed on refs/heads/main " +
				"within 11.8-14.3 minutes. Remove this and a queue candidate can reach main with " +
				"a green `Analyze (go)` that analysed nothing, recovered only by the weekly cron"},
		{"schedule",
			"the backstop for anything the push lane misses. It was not the recovery path for " +
				"any candidate #2973 measured, but it is the only trigger that fires without a " +
				"PR or a push at all"},
	} {
		if _, ok := wf.On[trig.key]; !ok {
			t.Errorf("codeql.yml keeps the merge_group no-op but has dropped its `%s` trigger.\n"+
				"The no-op reports the required `Analyze (go)` context green WITHOUT analysing "+
				"anything (deliberate, #1539). That is a deferral only while other triggers "+
				"cover the same code; drop one and it becomes a silent hole -- the check stays "+
				"green and nothing says otherwise.\n  %s: %s\n"+
				"If this removal is intended, the merge_group no-op has to go with it (#2973).",
				trig.key, trig.key, trig.why)
		}
	}

	// `push` must still cover main specifically -- narrowing it to some other
	// branch would satisfy a presence check while removing the coverage.
	if push, ok := wf.On["push"].(map[string]any); ok {
		branches, _ := push["branches"].([]any)
		var found bool
		for _, b := range branches {
			if s, isStr := b.(string); isStr && (s == "main" || s == "**" || s == "*") {
				found = true
			}
		}
		if !found {
			t.Errorf("codeql.yml's `push` trigger no longer covers main (branches: %v).\n"+
				"The merge_group no-op's recovery path is specifically the analysis that runs "+
				"when the candidate's tree lands on main; a push trigger on another branch "+
				"does not provide it (#2973).", branches)
		}
	}
}

// TestCodeQLNoOpStepStatesItsReason keeps the tradeoff self-documenting.
//
// The guard above enforces the mechanism; this keeps the WHY next to it. A
// future reader hitting the five-second green job should find the reason in the
// job log rather than having to find #1539.
func TestCodeQLNoOpStepStatesItsReason(t *testing.T) {
	wf := codeqlYAML(t)
	if !hasMergeGroupNoOp(wf) {
		t.Skip("no merge_group no-op to document (#2973)")
	}

	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			if !strings.Contains(strings.ReplaceAll(s.If, " ", ""), "github.event_name=='merge_group'") {
				continue
			}
			blob := s.Name + " " + s.Run
			if !strings.Contains(blob, "1539") {
				t.Errorf("the merge_group no-op step does not cite memql#1539 in its name or "+
					"output.\nIt reports a required security check green in five seconds without "+
					"analysing anything; whoever finds that in a job log needs the reason there, "+
					"not in a workflow comment they have to go looking for (#2973).\n"+
					"  name: %q\n  run:  %q", s.Name, s.Run)
			}
			return
		}
	}
}
