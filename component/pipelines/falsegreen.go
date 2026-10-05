package pipelines

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// falsegreen.go -- the count milestone M1 is measured in (epic memql#5480,
// issue memql#5506, design record section 7): a pull-request run that
// reported success on content whose full-mode run then failed, and not a
// sibling merge changing the tree under it.
//
// The affected-subset decision (D1) -- a pull request runs only what it
// affects, and the whole suite runs once on the tree that lands -- is right
// exactly as long as that is rare, and the run rows make it countable. What
// follows is that count as a function over values: the engine side reads the
// run rows, and the work steps of the runs that need them (RunsNeedingSteps),
// and hands them over as RunFacts and StepFacts.

// RunFacts is what the count reads off a v1:pipelines:run row.
//
// Conclusion is the row's own word, empty until the run completed. The count
// reads "success" and "failure": a run with any other word, or none, has said
// nothing about the tree it ran. PullRequest is set on a pull_request run and
// absent on every other event, which is why the pull request a full-mode run
// landed is read off its words (PullRequestOf). HeadBranch and Title are the
// row's: a merge group's head branch, and a commit's first line. SHA is not
// read: a merge writes a new commit, so a landing is never paired with its
// pull request by commit.
type RunFacts struct {
	ID, SHA, HeadBranch, Title, Conclusion string
	Event                                  Event
	Mode                                   Mode
	PullRequest                            int
	QueuedAt                               time.Time
}

// StepFacts is one work step of a run: its key and how it ended.
//
// Status is read in either vocabulary a caller may hold for the same fact:
// the work journal's ("done" for a step that passed) and the check run's and
// the executor's ("succeeded"). A step failed when it is "failed", or
// "refused" -- the runner would not start it, and the check run counts it a
// failure too. Any other status is neither a failure nor, on a pull request's
// run, a pass: a step still pending or running, one cancelled, one in a state
// this package does not know. Code matters only on a skipped step, where it
// says why.
type StepFacts struct{ Key, Status, Code string }

// What a pull request's run did with a step the full-mode run failed.
const (
	// OnPullRequestNotSelected: the pull request's run did not execute the
	// step. The affected selection left it out, the pull request's plan never
	// held it (a stage that runs on a push alone), or the run recorded no
	// pass for it -- the pull request gave no evidence about this step.
	OnPullRequestNotSelected = "not selected"
	// OnPullRequestPassed: the pull request's run executed the step and it
	// passed, or carried a pass over from an earlier attempt.
	OnPullRequestPassed = "passed"
)

// FalseGreen is one landing: a pull request whose last run before it landed
// concluded success, and whose full-mode run then failed. It is a false green
// proper when a step reads OnPullRequestNotSelected, which is what keeps it in
// FalseGreenReport.FalseGreens; the rest are in SiblingOrFlake.
type FalseGreen struct {
	PullRequest int `json:"pullRequest"`
	// PRRunID is the pull request's run that reported success, FullRunID the
	// full-mode run that failed on the tree that landed.
	PRRunID   string `json:"prRunId"`
	FullRunID string `json:"fullRunId"`
	// Steps are the full run's failed steps, each once and by lane (a shard's
	// "#i" suffix removed), in the order the full run holds them, with what
	// the pull request's run did with each: OnPullRequestNotSelected or
	// OnPullRequestPassed. Never nil; a run that failed with no failed step
	// of its own holds none.
	Steps []FalseGreenStep `json:"steps"`
}

// FalseGreenStep is one failed step of the full run, by lane, and what the
// pull request's run did with it (OnPullRequestNotSelected or
// OnPullRequestPassed).
type FalseGreenStep struct {
	Key           string `json:"key"`
	OnPullRequest string `json:"onPullRequest"`
}

// FalseGreenReport is the count over the runs it was given.
//
// The comparison is by step, not by package. A failing package that a lane
// which did run left out of its selection reads as passed, and lands in
// SiblingOrFlake: the count is a floor under the selection's misses, never a
// ceiling.
type FalseGreenReport struct {
	// FullRuns is the full-mode runs that reached a verdict on the tree they
	// ran, which is a conclusion of success or failure. One still going,
	// cancelled or refused says nothing about the tree, and is not counted.
	FullRuns int `json:"fullRuns"`
	// FullRunsFailed is those that failed.
	FullRunsFailed int `json:"fullRunsFailed"`
	// AfterGreen is the failed full runs whose pull request's last run before
	// them concluded success: the pull request reported green and the tree
	// then failed. It is FalseGreens and SiblingOrFlake together, always.
	AfterGreen int `json:"afterGreen"`
	// FalseGreens are those with a failed step the pull request's run did not
	// execute: the affected selection missed it, which is what the
	// affected-subset decision rests on.
	FalseGreens []FalseGreen `json:"falseGreens"`
	// SiblingOrFlake are the rest: every failed step ran and passed on the
	// pull request, so the content passed there and the tree the queue built
	// differed -- a sibling merge -- or the step is flaky. A failed run with
	// no failed step of its own (a manifest that would not compile, a runner
	// that was not there) is listed here too, with no steps.
	SiblingOrFlake []FalseGreen `json:"siblingOrFlake"`
}

// The words the count compares against. The work journal's status for a pass
// is "done" and the executor's outcome "succeeded"; "skipped" is the journal's
// and the check run's alike.
const (
	statusDone      = "done"
	statusSucceeded = string(OutcomeSucceeded)
	statusFailed    = string(OutcomeFailed)
	statusRefused   = string(OutcomeRefused)
	statusSkipped   = "skipped"

	conclusionSuccess = "success"
	conclusionFailure = "failure"
)

// How GitHub names what a pull request left on the default branch, and the
// branch a merge queue builds its group on.
const (
	// mergeSubjectPrefix starts the subject of a merge commit: "Merge pull
	// request #<n> from <head>".
	mergeSubjectPrefix = "Merge pull request #"
	// queueBranchPrefix starts the temporary branch of a merge group:
	// gh-readonly-queue/<base>/pr-<n>-<sha>.
	queueBranchPrefix = "gh-readonly-queue/"
)

// PullRequestOf names the pull request a full-mode run landed: a merge
// group's head branch gh-readonly-queue/<base>/pr-<n>-<sha>, or a push whose
// head commit is "Merge pull request #<n> from ..." or ends "(#<n>)".
//
// A row that carries its pull request's number names it. Otherwise a merge
// group is read from its head branch, which the queue wrote, and then from
// its head commit's subject, where a merge or squash commit puts the number
// as it does on a push. The branch's <base> may hold slashes, and its <sha>
// is not needed to name the pull request, so a branch without one still
// names it. A release lands no pull request, and a pull_request run with no
// number names none, whatever its title says.
//
// A subject ending "(#<n>)" is how GitHub names a squash merge; an issue
// reference spelled the same way would be read as a pull request, and one
// this pipeline never ran pairs with nothing. A merge group of several pull
// requests carries one number in its branch, so only that pull request is
// judged.
func PullRequestOf(r RunFacts) (int, bool) {
	if r.PullRequest > 0 {
		return r.PullRequest, true
	}
	if r.Event == EventPullRequest || r.Event == EventRelease {
		return 0, false
	}
	if n, ok := queuedPullRequest(r.HeadBranch); ok {
		return n, true
	}
	return landedPullRequest(r.Title)
}

// queuedPullRequest reads the pull request off a merge group's head branch.
// The base branch may hold slashes itself, so the entry is the last segment.
func queuedPullRequest(branch string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/"), queueBranchPrefix)
	if !ok {
		return 0, false
	}
	slash := strings.LastIndexByte(rest, '/')
	if slash <= 0 { // no base branch, or no entry after it
		return 0, false
	}
	entry, ok := strings.CutPrefix(rest[slash+1:], "pr-")
	if !ok {
		return 0, false
	}
	digits, tail := splitDigits(entry)
	if tail != "" && !strings.HasPrefix(tail, "-") { // pr-42abc is no number
		return 0, false
	}
	return pullRequestNumber(digits)
}

// landedPullRequest reads the pull request off a commit's subject: the merge
// commit's own words first, then the squash commit's trailing "(#<n>)".
func landedPullRequest(title string) (int, bool) {
	subject := firstLine(title)
	if rest, ok := strings.CutPrefix(subject, mergeSubjectPrefix); ok {
		if digits, tail := splitDigits(rest); strings.HasPrefix(tail, " from ") {
			return pullRequestNumber(digits)
		}
	}
	if inner, ok := strings.CutSuffix(subject, ")"); ok {
		if i := strings.LastIndex(inner, "(#"); i >= 0 {
			return pullRequestNumber(inner[i+len("(#"):])
		}
	}
	return 0, false
}

// splitDigits cuts the ASCII digits a string starts with from the rest of it.
func splitDigits(s string) (digits, rest string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i], s[i:]
}

// pullRequestNumber reads s as a pull request's number: decimal digits and
// nothing else, above zero, and small enough to be an int.
func pullRequestNumber(s string) (int, bool) {
	digits, rest := splitDigits(s)
	if digits == "" || rest != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// laneOf is a step's key with its shard suffix removed: tests.go-tests#2 is
// the lane tests.go-tests. A step's name holds no "#", so one is a shard's,
// and only when digits follow it.
func laneOf(key string) string {
	key = strings.TrimSpace(key)
	if i := strings.LastIndexByte(key, '#'); i >= 0 {
		if digits, rest := splitDigits(key[i+1:]); digits != "" && rest == "" {
			return key[:i]
		}
	}
	return key
}

// failedLanes is the lane of every step that failed, once each, in the order
// the steps were given. A step with no key names no lane.
func failedLanes(facts []StepFacts) []string {
	var lanes []string
	for _, f := range facts {
		if f.Status != statusFailed && f.Status != statusRefused {
			continue
		}
		if lane := laneOf(f.Key); lane != "" && !slices.Contains(lanes, lane) {
			lanes = append(lanes, lane)
		}
	}
	return lanes
}

// passedLanes is the lanes a run recorded a pass for. A pass takes evidence:
// a step that succeeded, or one a failed-only re-run carried over from an
// earlier attempt that passed it. One shard passing is the lane having run,
// which is the granularity the count has.
func passedLanes(facts []StepFacts) map[string]bool {
	passed := make(map[string]bool, len(facts))
	for _, f := range facts {
		switch {
		case f.Status == statusDone || f.Status == statusSucceeded:
			passed[laneOf(f.Key)] = true
		case f.Status == statusSkipped && f.Code == CodePassedEarlier:
			passed[laneOf(f.Key)] = true
		}
	}
	return passed
}

// landing is a failed full-mode run and the pull-request run that reported
// success before it: a pull request that was green and a tree that was not.
type landing struct {
	pullRequest int
	full, pr    RunFacts
}

// landings pairs each failed full-mode run that names a pull request with
// that pull request's newest pull_request run queued before it, and keeps the
// pairs whose pull-request run concluded success. Newest full run first. The
// runs must be distinct (distinctRuns).
//
// The newest run is the one that stands for the pull request: an older green
// run does not vouch for a newer one that failed, was cancelled or has not
// finished, because the head that landed is the newest.
func landings(runs []RunFacts) []landing {
	byPullRequest := map[int][]RunFacts{}
	var failed []RunFacts
	for _, r := range runs {
		switch {
		case r.Event == EventPullRequest && r.PullRequest > 0:
			byPullRequest[r.PullRequest] = append(byPullRequest[r.PullRequest], r)
		case r.Mode == ModeFull && r.Conclusion == conclusionFailure:
			failed = append(failed, r)
		}
	}
	slices.SortFunc(failed, newestFirst)

	var out []landing
	for _, full := range failed {
		n, ok := PullRequestOf(full)
		if !ok {
			continue
		}
		last, ok := newestQueuedBefore(byPullRequest[n], full.QueuedAt)
		if !ok || last.Conclusion != conclusionSuccess {
			continue
		}
		out = append(out, landing{pullRequest: n, full: full, pr: last})
	}
	return out
}

// newestFirst orders runs by when they were queued, newest first, and by id
// within one instant, so no order the rows arrive in changes an answer.
func newestFirst(a, b RunFacts) int {
	if c := b.QueuedAt.Compare(a.QueuedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// newestQueuedBefore is the newest of runs queued strictly before t. Nothing
// is before a t that is no time at all, so a full run with no queue time
// pairs with nothing.
func newestQueuedBefore(runs []RunFacts, t time.Time) (RunFacts, bool) {
	var newest RunFacts
	found := false
	for _, r := range runs {
		if !r.QueuedAt.Before(t) {
			continue
		}
		if !found || newestFirst(r, newest) < 0 {
			newest, found = r, true
		}
	}
	return newest, found
}

// distinctRuns drops a row seen twice, keeping the first. A read of a table
// that is being written to, in pages, can return one run on two of them.
func distinctRuns(runs []RunFacts) []RunFacts {
	seen := make(map[string]bool, len(runs))
	out := make([]RunFacts, 0, len(runs))
	for _, r := range runs {
		if r.ID != "" {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
		}
		out = append(out, r)
	}
	return out
}

// judge says what the pull request's run did with each step the full run
// failed, and whether any was a step it did not execute.
func (l landing) judge(steps map[string][]StepFacts) (green FalseGreen, missed bool) {
	green = FalseGreen{PullRequest: l.pullRequest, PRRunID: l.pr.ID, FullRunID: l.full.ID, Steps: []FalseGreenStep{}}
	passed := passedLanes(steps[l.pr.ID])
	for _, lane := range failedLanes(steps[l.full.ID]) {
		on := OnPullRequestPassed
		if !passed[lane] {
			on, missed = OnPullRequestNotSelected, true
		}
		green.Steps = append(green.Steps, FalseGreenStep{Key: lane, OnPullRequest: on})
	}
	return green, missed
}

// CountFalseGreens counts the false greens among runs: the measure M1 rests
// on, and the number the affected-subset decision (D1) is held to.
//
// A full-mode run counts once it has a verdict on the tree it ran, which is a
// conclusion of success or failure. Each failed one is paired with the
// pull-request run of the pull request it landed (PullRequestOf) that was
// queued latest before it, and the pair counts, in AfterGreen, only when that
// run concluded success: the pull request reported green on content whose
// full run then failed. It is a FALSE GREEN when at least one failed step of
// the full run -- shard suffix removed, so tests.go-tests#2 compares as
// tests.go-tests -- was not executed by the pull-request run. When every
// failed step ran and passed there, the content passed and the tree the queue
// built differed from it: a sibling merge or a flake, listed apart. A full
// run that names no pull request, or one whose pull request has no run
// before it in runs, or whose last run did not pass, is counted in FullRuns
// and FullRunsFailed and nowhere else.
//
// runs is one pipeline's runs: the window's, and as many earlier pull_request
// runs as the caller wants a landing to be able to pair with, since a pull
// request opened before the window can land inside it. Every full-mode run it
// holds is counted, so the window is the caller's cut of them. A row given
// twice is one run, and the order of runs changes nothing.
//
// steps holds each run's step facts, keyed by RunFacts.ID, and only for the
// runs RunsNeedingSteps names are they read. A run with none reads as having
// executed nothing, so a pull-request run whose steps were not loaded makes
// every failed step of its landing "not selected": a caller loads them.
//
// Both lists come newest landing first, and are never nil.
func CountFalseGreens(runs []RunFacts, steps map[string][]StepFacts) FalseGreenReport {
	runs = distinctRuns(runs)
	report := FalseGreenReport{FalseGreens: []FalseGreen{}, SiblingOrFlake: []FalseGreen{}}
	for _, r := range runs {
		if r.Mode != ModeFull {
			continue
		}
		switch r.Conclusion {
		case conclusionSuccess:
			report.FullRuns++
		case conclusionFailure:
			report.FullRuns++
			report.FullRunsFailed++
		}
	}
	for _, l := range landings(runs) {
		report.AfterGreen++
		if green, missed := l.judge(steps); missed {
			report.FalseGreens = append(report.FalseGreens, green)
		} else {
			report.SiblingOrFlake = append(report.SiblingOrFlake, green)
		}
	}
	return report
}

// RunsNeedingSteps names the runs whose step facts CountFalseGreens reads: the
// failed full-mode run of each landing, and the pull-request run it is judged
// against. Every other run is counted from its row alone, so a caller that
// loads the steps of these and of no others -- a few runs of a window of
// hundreds -- has read what the count needs. The ids are sorted, each once,
// and the list is never nil.
func RunsNeedingSteps(runs []RunFacts) []string {
	ids := []string{}
	for _, l := range landings(distinctRuns(runs)) {
		ids = append(ids, l.full.ID, l.pr.ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
