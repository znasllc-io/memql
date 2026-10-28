package pipelines

import (
	"fmt"
	"path"
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

// TrainTriageInput is everything the triage reads. Files holds each boarded
// pull request's changed paths, FilesComplete whether that list is whole
// (GitHub stops listing a pull request's files at 3,000; the coordinator at
// 300). MainFailedKeys are the steps main's newest completed push run failed.
type TrainTriageInput struct {
	Failed                []TrainFailedStep
	Retried               bool
	Rebuilds, MaxRebuilds int
	Files                 map[int][]string
	FilesComplete         map[int]bool
	ModulePath            string
	MainFailedKeys        []string
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
//  4. exactly one boarded pull request changed files in the failed steps'
//     packages: drop it; otherwise hold.
//
// Rule 4 maps a changed file to its directory's package and every ancestor
// directory's (dsl/pipelines/x.memql suspects .../dsl/pipelines, .../dsl and
// the module root): a change to embedded data or a parent's files can break
// that package, and over-suspecting only ever turns a drop into a hold.
func TriageTrainFailure(in TrainTriageInput) TrainVerdict {
	keys := failedKeys(in.Failed)
	if !in.Retried && len(in.Failed) > 0 && allEnvironmental(in.Failed) {
		return TrainVerdict{Action: "retry", Reason: "retrying " + keys + " once: the runner reported " + codesOf(in.Failed)}
	}
	for _, f := range in.Failed {
		for _, k := range in.MainFailedKeys {
			if f.Key == k {
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
		failing := map[string]bool{}
		for _, p := range f.Packages {
			failing[p] = true
		}
		for n, paths := range in.Files {
			if !in.FilesComplete[n] {
				suspects[n] = true
				continue
			}
			for _, p := range paths {
				if touches(in.ModulePath, p, failing) {
					suspects[n] = true
					break
				}
			}
		}
	}
	if len(suspects) == 1 {
		for n := range suspects {
			return TrainVerdict{Action: "drop", Drop: n, Reason: fmt.Sprintf("#%d changed files in the packages %s covers", n, keys)}
		}
	}
	return TrainVerdict{Action: "hold", Reason: "could not tell which pull request caused " + keys}
}

func touches(modulePath, file string, failing map[string]bool) bool {
	if modulePath == "" {
		return false
	}
	dir := path.Dir(strings.TrimPrefix(file, "/"))
	for {
		pkg := modulePath
		if dir != "." && dir != "/" && dir != "" {
			pkg = modulePath + "/" + dir
		}
		if failing[pkg] {
			return true
		}
		if dir == "." || dir == "/" || dir == "" {
			return false
		}
		dir = path.Dir(dir)
	}
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
