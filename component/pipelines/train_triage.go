package pipelines

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// train_triage.go -- the two judgements a train makes about people's work:
// whether the owner's approval lands this train, and which pull request a
// failed run drops (design record, sections 6 and 7).
//
// Both are deliberately conservative. A train never lands on an approval of
// a commit the owner did not see, and never drops a pull request it cannot
// name: when the evidence does not single one out, the train is HELD and the
// owner decides, because dropping the wrong pull request costs its author a
// day and blames them for someone else's break.

// TrainReview is one GitHub review of a train pull request.
type TrainReview struct {
	Login, State, CommitID string
	SubmittedAt            time.Time
}

// ApprovalLands reports whether the owner's latest deciding review approves
// exactly headSHA. APPROVED and CHANGES_REQUESTED decide; COMMENTED and
// DISMISSED never do. A review is tied to the commit it was given on, so an
// approval followed by a rebuild (a dropped pull request, a moved main) does
// not land the new head: the owner approves what they looked at, nothing
// else.
func ApprovalLands(reviews []TrainReview, ownerLogin, headSHA string) bool {
	if strings.TrimSpace(ownerLogin) == "" || strings.TrimSpace(headSHA) == "" {
		return false
	}
	var latest *TrainReview
	for i := range reviews {
		r := &reviews[i]
		if !strings.EqualFold(r.Login, ownerLogin) {
			continue
		}
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" {
			continue
		}
		if latest == nil || r.SubmittedAt.After(latest.SubmittedAt) {
			latest = r
		}
	}
	return latest != nil && latest.State == "APPROVED" && latest.CommitID == headSHA
}

// EnvironmentalStepCodes are failures about the machinery, not the change: a
// step past its timeout, no runner, a runner that could not report, an image
// that would not pull, a clone that failed. Only these earn the one automatic
// retry (spec section 6, step 1); a test that failed is never retried.
var EnvironmentalStepCodes = []string{CodeStepTimeout, CodeRunnerUnavailable, CodeExecutorError, CodeImagePullFailed, CodeCloneFailed}

// TrainFailedStep is one failed step of a train's run: its key, the code the
// runner gave (empty when a command simply exited non-zero) and the Go
// packages it covered (empty when the step selects none).
type TrainFailedStep struct {
	Key, ErrorCode string
	Packages       []string
}

// TrainReach is what one boarded pull request's change can reach: the Go
// packages the pipeline's own affected analysis would select for the files
// it changed at its boarded commit -- the packages those files are in, and
// every package importing them, over the train head's import graph -- or
// Unbounded, with Why, where that analysis cannot bound the change.
type TrainReach struct {
	Unbounded bool
	Why       string
	Packages  []string
}

// TrainReachOf is the reach of one pull request's changed files in g.
// complete says whether files is the whole list (a comparison stops at 300
// files). It is unbounded exactly where the shipped selection policy
// (dsl/pipelines automation pipelinePackageSelection) runs every package,
// with that policy's reasons -- an unknown or empty change list, an
// incomplete graph, a path outside the repository, the manifest, a module
// graph file, vendored code, a select.full path -- plus no graph at all. An
// installation that replaced that policy is still judged by the shipped one
// here: triage only decides whom to suspect, never what to test.
func TrainReachOf(g *Graph, files []string, complete bool, fullGlobs []string) TrainReach {
	unbounded := func(why string) TrainReach { return TrainReach{Unbounded: true, Why: why} }
	switch {
	case !complete:
		return unbounded("the changed-file comparison is unavailable")
	case len(files) == 0:
		return unbounded("the change list is empty")
	case g == nil:
		return unbounded("the import graph is unavailable")
	}
	facts, err := AnalyzeSelection(g, files, true, fullGlobs)
	if err != nil {
		return unbounded("the change could not be analyzed")
	}
	if !facts.GraphComplete {
		return unbounded("the import graph is incomplete")
	}
	for _, f := range facts.Paths {
		switch {
		case !f.RepositoryPath:
			return unbounded("a changed path is not repository-relative")
		case f.Path == ManifestPath:
			return unbounded("the pipeline manifest changed")
		case f.BaseName == "go.mod" || f.BaseName == "go.sum" || f.BaseName == "go.work" || f.BaseName == "go.work.sum":
			return unbounded("a Go module graph file changed")
		case f.Vendored:
			return unbounded("vendored code changed")
		case f.ConfiguredFull:
			return unbounded("a configured full-selection path changed")
		}
	}
	sel, err := ResolveSelection(g, facts, SelectionDecision{})
	if err != nil {
		return unbounded("the change could not be analyzed")
	}
	return TrainReach{Packages: sel.Packages}
}

// TrainTriageInput is everything the triage reads. Reach is each boarded
// pull request's TrainReach; MainFailed are the steps main's newest
// completed push run failed, with the packages they tested.
type TrainTriageInput struct {
	Failed                []TrainFailedStep
	Retried               bool
	Rebuilds, MaxRebuilds int
	Reach                 map[int]TrainReach
	MainFailed            []TrainFailedStep
}

// TrainVerdict is what a train does about a failed run: "retry" the failed
// steps, "drop" one pull request and rebuild, or "hold" for the owner.
type TrainVerdict struct {
	Action string
	Drop   int
	Reason string
}

// TriageTrainFailure decides a failed train run, in this order:
//
//  1. every failed step environmental and not yet retried: retry;
//  2. main's own newest run failed one of the same steps: hold, because the
//     break is main's and dropping a pull request would blame its author;
//  3. the rebuild cap reached: hold;
//  4. exactly one boarded pull request can reach the failed steps' packages:
//     drop it; otherwise hold.
//
// "The same step" (rule 2) is the same key for an unsharded step, and for a
// step that tested packages, an overlap in those packages: shard numbers
// partition a pull request run's affected packages and main's full set
// differently, so tests.go-unit#2 names different packages in the two.
//
// Rule 4 asks the pipeline's own affected analysis (TrainReachOf): a pull
// request is a suspect for a failed step when its change reaches one of the
// step's packages through the import graph, and for every failed step when
// the analysis cannot bound its change. A pull request is cleared only when
// the graph shows its change cannot reach what failed, so a single suspect is
// dropped only when every other pull request on board is out of reach. What
// the graph cannot see -- a test reading a file at run time -- is the same
// blind spot the pipeline's affected runs already accept.
func TriageTrainFailure(in TrainTriageInput) TrainVerdict {
	keys := failedKeys(in.Failed)
	if !in.Retried && len(in.Failed) > 0 && allEnvironmental(in.Failed) {
		return TrainVerdict{Action: "retry", Reason: "retrying " + keys + " once: the runner reported " + codesOf(in.Failed)}
	}
	for _, f := range in.Failed {
		for _, m := range in.MainFailed {
			if sameStep(f, m) {
				return TrainVerdict{Action: "hold", Reason: "main fails " + f.Key + " too, so no pull request on this train is to blame; fix main first (an express train)"}
			}
		}
	}
	if in.MaxRebuilds > 0 && in.Rebuilds >= in.MaxRebuilds {
		return TrainVerdict{Action: "hold", Reason: fmt.Sprintf("stopped after %d rebuilds; %s still failed", in.Rebuilds, keys)}
	}
	suspects := map[int]bool{}
	for _, f := range in.Failed {
		if len(f.Packages) == 0 {
			continue
		}
		for n, r := range in.Reach {
			if r.Unbounded || overlaps(r.Packages, f.Packages) {
				suspects[n] = true
			}
		}
	}
	if len(suspects) == 1 {
		for n := range suspects {
			reason := fmt.Sprintf("#%d is the only pull request on board whose change reaches what %s tested", n, keys)
			if r := in.Reach[n]; r.Unbounded {
				reason += " (" + r.Why + ")"
			}
			return TrainVerdict{Action: "drop", Drop: n, Reason: reason}
		}
	}
	return TrainVerdict{Action: "hold", Reason: "could not tell which pull request caused " + keys}
}

// sameStep reports whether main's failed step m is the train's failed step
// f: the same key when neither is a shard, or packages in common.
func sameStep(f, m TrainFailedStep) bool {
	if !strings.Contains(f.Key, "#") && !strings.Contains(m.Key, "#") && f.Key == m.Key {
		return true
	}
	return len(f.Packages) > 0 && len(m.Packages) > 0 && overlaps(f.Packages, m.Packages)
}

func overlaps(a, b []string) bool {
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	for _, y := range b {
		if set[y] {
			return true
		}
	}
	return false
}

func allEnvironmental(failed []TrainFailedStep) bool {
	for _, f := range failed {
		env := false
		for _, c := range EnvironmentalStepCodes {
			if f.ErrorCode == c {
				env = true
				break
			}
		}
		if !env {
			return false
		}
	}
	return true
}

func failedKeys(failed []TrainFailedStep) string {
	var keys []string
	for _, f := range failed {
		keys = append(keys, f.Key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func codesOf(failed []TrainFailedStep) string {
	seen := map[string]bool{}
	var codes []string
	for _, f := range failed {
		if f.ErrorCode != "" && !seen[f.ErrorCode] {
			seen[f.ErrorCode] = true
			codes = append(codes, f.ErrorCode)
		}
	}
	sort.Strings(codes)
	return strings.Join(codes, ", ")
}
