// Static guard over the coverage that compensates for CodeQL's merge_group
// no-op (znasllc-io/memql#2973).
//
// CURRENT STATE (memql#5482): codeql.yml no longer subscribes to merge_group
// or pull_request, so the no-op below is gone and the two implication tests
// SKIP -- the "provably absent" branch they were written with. What holds
// CodeQL now is the pair of tests at the end of this file: every push to main
// is analysed with no path filter, and the pull-request path stays off as a
// recorded decision. The history below explains why the implication tests
// stay: if merge_group ever comes back WITH a no-op, they fire again.
//
// # The tradeoff being guarded
//
// `.github/workflows/codeql.yml` runs a single `echo` on `merge_group` and
// skips checkout / init / autobuild / analyze. The job reports success and
// satisfies the required `Analyze (go)` status context without analysing
// anything. That is DELIBERATE and documented in the workflow itself
// (.github/workflows/codeql.yml) and in #1539: a SARIF upload keyed to the
// torn-down `gh-readonly-queue` ref wedges the queue.
//
// NOT documented in docs/internal/ops/merge-queue.md, which this comment used
// to cite (memql#2973 landing review). That page says the opposite -- that all
// three required contexts "trigger on merge_group" and the queue "runs the
// full suite on it" -- so citing it as the authority for the no-op pointed a
// reader at a page that contradicts it. The page has been corrected too.
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
		// A job-level `if` gates every step in the job, so a workflow can be
		// split into a merge_group no-op job and a real analyze job with no
		// step-level condition anywhere. Reading only steps made that shape
		// invisible (memql#2973 landing review).
		If    string `yaml:"if"`
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
		jobCond := normalizeWorkflowCond(job.If)
		if strings.Contains(jobCond, "github.event_name=='merge_group'") {
			gatedOnMergeGroup = true
		}
		jobExcludesMergeGroup := strings.Contains(jobCond, "github.event_name!='merge_group'")
		for _, s := range job.Steps {
			cond := normalizeWorkflowCond(s.If)
			switch {
			case strings.Contains(cond, "github.event_name=='merge_group'"):
				gatedOnMergeGroup = true
			case (strings.Contains(cond, "github.event_name!='merge_group'") || jobExcludesMergeGroup) &&
				strings.Contains(s.Uses, "codeql-action/analyze"):
				analysisSkipped = true
			}
		}
	}
	return gatedOnMergeGroup && analysisSkipped
}

// normalizeWorkflowCond strips whitespace and any `${{ }}` wrapper so the
// condition can be matched as text.
func normalizeWorkflowCond(cond string) string {
	c := strings.ReplaceAll(cond, " ", "")
	c = strings.TrimPrefix(c, "${{")
	c = strings.TrimSuffix(c, "}}")
	return c
}

// TestCodeQLMergeGroupNoOpKeepsItsCompensatingTriggers is the guard.
func TestCodeQLMergeGroupNoOpKeepsItsCompensatingTriggers(t *testing.T) {
	wf := codeqlYAML(t)

	if _, ok := wf.On["merge_group"]; !ok {
		t.Skip("codeql.yml no longer subscribes to merge_group, so there is no no-op to " +
			"compensate for and nothing for this guard to require (#2973)")
	}
	if !hasMergeGroupNoOp(wf) {
		// FAIL, not skip. The workflow still subscribes to merge_group, so
		// either the candidate is genuinely analysed now -- in which case
		// delete this guard and say so -- or the no-op moved into a shape this
		// detector does not read, and skipping would disarm the guard silently
		// while the thing it protects against is intact.
		//
		// That is not hypothetical: the detector originally read only
		// STEP-level `if`, so splitting the workflow into a merge_group no-op
		// job and a real analyze job -- a behaviour-preserving refactor that
		// keeps the required context green -- made this branch fire and the
		// whole guard reported ok with both compensating triggers deleted
		// (memql#2973 landing review). Failing on unrecognised, and skipping
		// only on provably absent, is what makes the guard hard to disarm by
		// accident.
		t.Fatalf("codeql.yml still subscribes to merge_group, but this guard cannot find the "+
			"no-op shape it compensates for. Either the merge_group candidate is now analysed "+
			"for real -- in which case remove this file and the workflow comment that cites it "+
			"-- or the no-op was restructured and hasMergeGroupNoOp needs teaching about the new "+
			"shape. Do not leave it in this state: the compensating triggers (%s) are "+
			"unguarded while it holds.", "push: main, schedule, pull_request")
	}

	// The no-op is present. Every compensating trigger must be too.
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
		{"pull_request",
			"the LARGEST source of the compensating coverage -- 43 of the 58 go analyses in " +
				"#2973's window are refs/pull/*/merge -- and the one the no-op step's own name " +
				"claims: \"merge-queue no-op (CodeQL already ran on the PR)\". Remove this and " +
				"that sentence becomes false while the guard stays green. This file's header " +
				"named all three compensating triggers and the gate covered two of them " +
				"(memql#2973 landing review)"},
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
		// A `paths` / `paths-ignore` filter removes the coverage exactly as
		// completely as narrowing the branch list, and satisfies both the
		// presence check and the branch check. It is the same class the branch
		// check exists for (memql#2973 landing review).
		for _, k := range []string{"paths", "paths-ignore"} {
			if v, has := push[k]; has {
				t.Errorf("codeql.yml's `push` trigger carries a `%s` filter (%v).\n"+
					"The merge_group no-op's recovery path is the analysis of the candidate's "+
					"tree when it lands on main, and a path filter means some trees never get "+
					"one -- the required context stays green having analysed nothing, with no "+
					"recovery. Narrowing by path removes the coverage as completely as "+
					"narrowing by branch (#2973).", k, v)
			}
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

// TestCodeQLAnalysesEveryPushToMain holds the coverage CodeQL keeps once it is
// off the pull-request path (epic memql#5476, memql#5482): every push to main
// -- the tree that lands -- and the weekly scan. With pull_request gone this is
// no longer a compensating trigger behind a no-op; it is THE coverage, so it is
// asserted unconditionally rather than as an implication. A branch narrowed
// away from main, or a path filter, leaves some landed trees never analysed
// with nothing going red.
func TestCodeQLAnalysesEveryPushToMain(t *testing.T) {
	wf := codeqlYAML(t)
	push, ok := wf.On["push"].(map[string]any)
	if !ok {
		t.Fatalf("codeql.yml has no `push` trigger mapping (got %T): the tree that lands is analysed by "+
			"nothing but the weekly scan (memql#5482)", wf.On["push"])
	}
	branches, _ := push["branches"].([]any)
	covers := false
	for _, b := range branches {
		if s, _ := b.(string); s == "main" || s == "**" || s == "*" {
			covers = true
		}
	}
	if !covers {
		t.Errorf("codeql.yml's push trigger does not cover main (branches: %v)", branches)
	}
	for _, k := range []string{"paths", "paths-ignore"} {
		if v, has := push[k]; has {
			t.Errorf("codeql.yml's push trigger carries a `%s` filter (%v): a landed tree the filter "+
				"excludes is never analysed, and nothing says so", k, v)
		}
	}
	if _, ok := wf.On["schedule"]; !ok {
		t.Error("codeql.yml dropped its weekly `schedule`: the only trigger that fires with no push at all")
	}
}

// TestCodeQLIsOffThePullRequestPath records the decision (memql#5482) as an
// assertion, the way ruleset-baseline.md records ruleset decisions: CodeQL does
// not run on pull requests or on the merge queue. It gated no merge -- the
// ruleset requires `ci-required` alone -- cost three runners per pull-request
// push, and each run's ~1 GB dependency cache held the repository's cache store
// at its limit, evicting the caches the pull-request lanes read. Re-adding
// either trigger is a new decision, not a fix: change this test with it, and
// the design record's section 1.
func TestCodeQLIsOffThePullRequestPath(t *testing.T) {
	wf := codeqlYAML(t)
	for _, trig := range []string{"pull_request", "pull_request_target", "merge_group"} {
		if _, ok := wf.On[trig]; ok {
			t.Errorf("codeql.yml runs on %s again. It was taken off that path deliberately "+
				"(epic memql#5476, memql#5482): no merge waits on it, and its per-run cache evicts the "+
				"ones the lanes read. If that decision is being reversed, reverse this test with it.", trig)
		}
	}
}
