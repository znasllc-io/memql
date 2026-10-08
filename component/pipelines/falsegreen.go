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
//
// What the count settles is whose miss a failed full run is. A step the pull
// request's run planned and skipped as not affected is the selection's miss,
// and that alone is a false green. A step it never planned -- a stage that
// runs on pushes alone, a notification that was not delivered, a step a
// sibling merge added -- was never the selection's to run. A step that ran and
// passed there as a step may still have failed in a package its slice left
// out; the count compares steps, cannot tell that from a tree that differed (a
// sibling merge) or a flake, and files all three alike.

// RunFacts is what the count reads off a v1:pipelines:run row.
//
// Conclusion is the row's own word, empty until the run completed. The count
// reads "success" and "failure": a run with any other word, or none, has said
// nothing about the tree it ran. PullRequest is set on a pull_request run and
// absent on every other event, which is why the pull request a full-mode run
// landed is read off its words (PullRequestOf). HeadBranch and Title are the
// row's: a merge group's head branch, and a commit's first line.
//
// SHA, with Mode and Event, is the key a run was opened under (RunKey; one
// pipeline's runs share a repository): every re-run of a run is another row
// with the same key, which is how the count knows it for another attempt of
// that run. A run with no SHA is a key of its own. A landing is never paired
// with its pull request by commit: a merge writes a new one.
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

// What a pull request's run did with a step the full-mode run failed. Each is
// read off what the run recorded for the step, by lane, and only evidence
// counts.
const (
	// OnPullRequestNotSelected: the pull request's run planned the step and
	// skipped it as not affected (pipeline_not_affected). The selection was
	// asked about the step and left it out: that is its miss, and the only
	// kind that makes a false green.
	OnPullRequestNotSelected = "not selected"
	// OnPullRequestPassed: the pull request's run executed the step and it
	// passed, or carried a pass over from an earlier attempt. That is the
	// step's pass, not each package's: the failing package may not have been
	// in the step's slice.
	OnPullRequestPassed = "passed"
	// OnPullRequestNotPlanned: the pull request's run holds neither of those
	// for the step. It never planned it -- a stage that runs on pushes alone
	// (docs, deploy, verify-rollout), a notification that was not delivered,
	// a step a sibling merge added to the manifest -- or it recorded nothing
	// else usable for it. A selection can only miss a step it was asked
	// about, so this is no miss of it.
	OnPullRequestNotPlanned = "not planned"
)

// FalseGreen is one landing: a pull request whose last run before its commit
// first landed concluded success, and whose full-mode run then failed. The
// list it is in says what the pull request's run did with the failed steps:
// FalseGreenReport.FalseGreens for a selection miss, NotOnPullRequests for a
// step it never planned, SiblingOrFlake for steps that passed.
type FalseGreen struct {
	PullRequest int `json:"pullRequest"`
	// PRRunID is the pull request's run that reported success, FullRunID the
	// full-mode run that failed on the tree that landed: the latest attempt to
	// reach a verdict of the earliest failed run (see CountFalseGreens).
	PRRunID   string `json:"prRunId"`
	FullRunID string `json:"fullRunId"`
	// Steps are the full run's failed steps, each once and by lane (a shard's
	// "#i" suffix removed), in the order the full run holds them, with what
	// the pull request's run did with each: OnPullRequestNotSelected,
	// OnPullRequestPassed or OnPullRequestNotPlanned. Never nil; a run that
	// failed with no failed step of its own holds none.
	Steps []FalseGreenStep `json:"steps"`
}

// FalseGreenStep is one failed step of the full run, by lane, and what the
// pull request's run did with it (OnPullRequestNotSelected,
// OnPullRequestPassed or OnPullRequestNotPlanned).
type FalseGreenStep struct {
	Key           string `json:"key"`
	OnPullRequest string `json:"onPullRequest"`
}

// FalseGreenReport is the count over the runs it was given.
//
// The units differ, and are named: FullRuns and FullRunsFailed count full-mode
// run keys; AfterGreen and the three lists count pull requests. A pull request
// is one entry in one of the lists, however many full runs failed on it, filed
// by the worst of its failed steps: a selection miss makes it a false green;
// failing that, a step it never planned puts it in NotOnPullRequests;
// otherwise every failed step ran and passed as a step on the pull request.
//
// The comparison is by step, not by package. A step that selects packages runs
// on a pull request over a slice of them, and a failing package outside that
// slice reads as a step that passed: it lands in SiblingOrFlake. The count is
// a floor on the package axis, so no list proves the selection missed nothing,
// and a SiblingOrFlake entry on a lane that selects packages is for a person
// to look at.
//
// While Unread is not empty none of the lists is to be believed.
type FalseGreenReport struct {
	// FullRuns is the full-mode run keys that reached a verdict on the tree
	// they ran, which is a conclusion of success or failure, each run once
	// however often it was re-run. One still going, cancelled or refused says
	// nothing about the tree, and is not counted.
	FullRuns int `json:"fullRuns"`
	// FullRunsFailed is those that failed: run keys, not pull requests, so a
	// merge group and the push of its merge commit that both failed are two.
	FullRunsFailed int `json:"fullRunsFailed"`
	// AfterGreen is the pull requests whose last run before a failed full run
	// of them concluded success: the pull request reported green and the tree
	// then failed. Each is counted once. It is FalseGreens, NotOnPullRequests
	// and SiblingOrFlake together, always.
	AfterGreen int `json:"afterGreen"`
	// FalseGreens are those with a failed step the pull request's run did not
	// run: it planned the step and the selection left it out (skipped as not
	// affected), which is what the affected-subset decision rests on.
	FalseGreens []FalseGreen `json:"falseGreens"`
	// NotOnPullRequests are those with no selection miss and at least one
	// failed step the pull request's run never planned: a stage that runs on
	// pushes alone, a notification that was not delivered, a step a sibling
	// merge added. It is no fault of the selection, and it is kept apart from
	// the false greens so that none of those reads as one.
	NotOnPullRequests []FalseGreen `json:"notOnPullRequests"`
	// SiblingOrFlake are the rest: every failed step ran and passed as a step
	// on the pull request; the failing package may not have been in that
	// step's slice. So the tree the queue built differed from the pull
	// request's -- a sibling merge -- or the step is flaky, or the failing
	// package was one the step did not run on the pull request, which a count
	// by step cannot tell. A failed run with no failed step of its own (a
	// manifest that would not compile, a runner that was not there) is listed
	// here too, with no steps.
	SiblingOrFlake []FalseGreen `json:"siblingOrFlake"`
	// Unread is the runs whose step facts the count needed and was not given:
	// every id RunsNeedingSteps names that has no key in the steps map. A key
	// that is present and empty is the legitimate no-steps state (a run
	// refused before any step began, a pipeline none of whose stages run on
	// pull requests); an absent one is a read that did not happen -- a nil
	// map, a run left out, a map keyed by something other than RunFacts.ID. A
	// landing whose steps were not read is filed as though its run recorded
	// nothing, so a caller treats a non-empty Unread as an error and believes
	// no list. Sorted; never nil.
	Unread []string `json:"unread"`
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

// laneRecord is what a pull request's run recorded for a lane that bears on a
// failure of it. These two are the only evidence: anything else the run
// recorded for the lane, or nothing, is neither.
type laneRecord struct {
	// passed: a step of the lane succeeded, or a failed-only re-run carried
	// one over from an earlier attempt that passed it. One shard passing is
	// the lane having run, which is the granularity the count has.
	passed bool
	// notAffected: a step of the lane was skipped as not affected -- planned,
	// and left out by the selection.
	notAffected bool
}

// skipReading is what a step skipped with a given code is evidence of on a
// pull request's run.
type skipReading int

const (
	// skipSaysNothing: the step did not run and the skip is neither a pass nor
	// the selection's doing. It is the reading of a code that is not stated.
	skipSaysNothing skipReading = iota
	// skipIsAPass: the step passed in an earlier attempt and was carried over.
	skipIsAPass
	// skipIsTheSelections: the step was planned and the selection left it out.
	skipIsTheSelections
)

// skipReadings states, for every skip code of the catalogue (ClassSkip), what
// the count reads a step skipped with it as. A skip code added without a line
// here fails TestEverySkipCodeHasAStatedReading, so a new one is decided on
// and not read quietly as skipSaysNothing.
var skipReadings = map[string]skipReading{
	CodeNotAffected:   skipIsTheSelections,
	CodePassedEarlier: skipIsAPass,
	CodeStageBlocked:  skipSaysNothing,
}

// laneRecords is what a run recorded for each lane it holds steps of.
func laneRecords(facts []StepFacts) map[string]laneRecord {
	records := make(map[string]laneRecord, len(facts))
	for _, f := range facts {
		lane := laneOf(f.Key)
		rec := records[lane]
		switch {
		case f.Status == statusDone || f.Status == statusSucceeded:
			rec.passed = true
		case f.Status == statusSkipped:
			switch skipReadings[f.Code] {
			case skipIsAPass:
				rec.passed = true
			case skipIsTheSelections:
				rec.notAffected = true
			}
		}
		records[lane] = rec
	}
	return records
}

// fullRun is a full-mode run as the count judges it: a run key, by its latest
// attempt that reached a verdict.
type fullRun struct {
	// verdict is that attempt: the latest row of the key concluded success or
	// failure.
	verdict RunFacts
	// queued is when the key was first queued, which is when its commit
	// landed. The pull request's runs are read as of this moment, and not as
	// of a re-run's, which may come after anything.
	queued time.Time
}

// fullRuns is the full-mode runs of runs, each run key once. A run key is the
// commit, mode and event the run was opened for, with the repository left out
// because one pipeline has one (RunKey): every re-run of a run is another row
// sharing it. A key none of whose attempts reached a verdict has said nothing,
// and is left out. The order is not defined.
func fullRuns(runs []RunFacts) []fullRun {
	type attempts struct {
		queued     time.Time
		verdict    RunFacts
		hasVerdict bool
	}
	byKey := map[string]*attempts{}
	for row, r := range runs {
		if r.Mode != ModeFull || r.Event == EventSchedule {
			continue
		}
		key := runKeyOf(r, row)
		a := byKey[key]
		if a == nil {
			a = &attempts{queued: r.QueuedAt}
			byKey[key] = a
		}
		if r.QueuedAt.Before(a.queued) {
			a.queued = r.QueuedAt
		}
		if hasVerdict(r) && (!a.hasVerdict || newestFirst(r, a.verdict) < 0) {
			a.verdict, a.hasVerdict = r, true
		}
	}
	out := make([]fullRun, 0, len(byKey))
	for _, a := range byKey {
		if a.hasVerdict {
			out = append(out, fullRun{verdict: a.verdict, queued: a.queued})
		}
	}
	return out
}

// runKeyOf is the key a run was opened under: RunKey without its repository.
// A run that names no commit has nothing to be grouped by, and is a key of its
// own.
func runKeyOf(r RunFacts, row int) string {
	if strings.TrimSpace(r.SHA) == "" {
		return "row " + strconv.Itoa(row)
	}
	return RunKey("", r.SHA, r.Mode, r.Event)
}

// hasVerdict reports whether a run concluded on the tree it ran.
func hasVerdict(r RunFacts) bool {
	return r.Conclusion == conclusionSuccess || r.Conclusion == conclusionFailure
}

// landing is a pull request that was green and whose commit then failed: the
// failed full run that judges it, when that run's key was first queued, and
// the pull request's run that reported success before then.
type landing struct {
	pullRequest int
	full, pr    RunFacts
	queued      time.Time
}

// landings is the pull requests that were green and whose commits failed, one
// per pull request, newest first. Each failed full run that names a pull
// request is paired with that pull request's newest pull_request run queued
// before its key was first queued, and the pair stands when that run
// concluded success. A pull request that several failed runs landed -- a merge
// group, and the push of its merge commit -- is judged by the earliest of
// those that stand: the first full run to find the failure. Only a pair that
// stands competes, so a failed run that followed a red run of the pull
// request, with nothing green yet, does not hide a later one that followed a
// green. The runs must be distinct (distinctRuns), and full is fullRuns of
// them.
//
// The newest run is the one that stands for the pull request: an older green
// run does not vouch for a newer one that failed, was cancelled or has not
// finished, because the head that landed is the newest.
func landings(runs []RunFacts, full []fullRun) []landing {
	byPullRequest := map[int][]RunFacts{}
	for _, r := range runs {
		if r.Event == EventPullRequest && r.PullRequest > 0 {
			byPullRequest[r.PullRequest] = append(byPullRequest[r.PullRequest], r)
		}
	}
	earliest := map[int]landing{}
	for _, f := range full {
		if f.verdict.Conclusion != conclusionFailure {
			continue
		}
		n, ok := PullRequestOf(f.verdict)
		if !ok {
			continue
		}
		last, ok := newestQueuedBefore(byPullRequest[n], f.queued)
		if !ok || last.Conclusion != conclusionSuccess {
			continue
		}
		l := landing{pullRequest: n, full: f.verdict, pr: last, queued: f.queued}
		if prev, seen := earliest[n]; !seen || firstLanded(l, prev) {
			earliest[n] = l
		}
	}
	out := make([]landing, 0, len(earliest))
	for _, l := range earliest {
		out = append(out, l)
	}
	slices.SortFunc(out, newestLanding)
	return out
}

// firstLanded reports whether a landed before b: its commit was first queued
// earlier, or in the same instant with a run whose id sorts first.
func firstLanded(a, b landing) bool {
	if c := a.queued.Compare(b.queued); c != 0 {
		return c < 0
	}
	return a.full.ID < b.full.ID
}

// newestLanding orders landings newest first, and by the failed run's id
// within one instant, so no order the rows arrive in changes an answer.
func newestLanding(a, b landing) int {
	if c := b.queued.Compare(a.queued); c != 0 {
		return c
	}
	return strings.Compare(a.full.ID, b.full.ID)
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

// distinctRuns drops a row seen twice. A read of a table that is being written
// to, in pages, can return one run on two of them -- once before it concluded
// and once after -- so the copy that has a verdict is kept over one that has
// none, and no order the pages arrive in changes an answer. Of two copies alike
// in that, the first is kept.
func distinctRuns(runs []RunFacts) []RunFacts {
	at := make(map[string]int, len(runs)) // where each run id was first kept
	out := make([]RunFacts, 0, len(runs))
	for _, r := range runs {
		if r.ID != "" {
			if i, seen := at[r.ID]; seen {
				if hasVerdict(r) && !hasVerdict(out[i]) {
					out[i] = r
				}
				continue
			}
			at[r.ID] = len(out)
		}
		out = append(out, r)
	}
	return out
}

// judge says what the pull request's run did with each step the full run
// failed, and whether any was a selection miss or a step it never planned.
func (l landing) judge(steps map[string][]StepFacts) (green FalseGreen, selectionMiss, notPlanned bool) {
	green = FalseGreen{PullRequest: l.pullRequest, PRRunID: l.pr.ID, FullRunID: l.full.ID, Steps: []FalseGreenStep{}}
	records := laneRecords(steps[l.pr.ID])
	for _, lane := range failedLanes(steps[l.full.ID]) {
		var on string
		switch rec := records[lane]; {
		case rec.passed:
			on = OnPullRequestPassed
		case rec.notAffected:
			on, selectionMiss = OnPullRequestNotSelected, true
		default:
			on, notPlanned = OnPullRequestNotPlanned, true
		}
		green.Steps = append(green.Steps, FalseGreenStep{Key: lane, OnPullRequest: on})
	}
	return green, selectionMiss, notPlanned
}

// runsRead is what the count decides from the run rows alone: the full runs it
// counts, and the pull requests that were green and whose commits failed.
// CountFalseGreens and RunsNeedingSteps both read the rows through it, so
// neither can read them another way.
type runsRead struct {
	full     []fullRun
	landings []landing
}

// readRuns reads one pipeline's run rows: the distinct runs, the full runs they
// make, and the landings among those.
func readRuns(runs []RunFacts) runsRead {
	runs = distinctRuns(runs)
	full := fullRuns(runs)
	return runsRead{full: full, landings: landings(runs, full)}
}

// stepsNeeded is the ids of the runs whose step facts the count reads to judge
// these landings: each one's judging full run and its pull request's run,
// sorted. A pull request is one landing and a run belongs to one, so no id is
// named twice.
func stepsNeeded(ls []landing) []string {
	ids := []string{}
	for _, l := range ls {
		ids = append(ids, l.full.ID, l.pr.ID)
	}
	slices.Sort(ids)
	return ids
}

// CountFalseGreens counts the false greens among runs: the measure M1 rests
// on, and the number the affected-subset decision (D1) is held to.
//
// A full-mode run is a run key -- the commit, mode and event it was opened
// for, which every re-run of it shares -- and is judged by its latest attempt
// that reached a verdict, a conclusion of success or failure. A failure that
// passed on a re-run was a flake and is no failure; a failure re-run into
// another is one failure, as the latest attempt saw it. An attempt still going
// or cancelled says nothing, and the one before it stands.
//
// A failed run that names a pull request (PullRequestOf) is paired with that
// pull request's run queued latest before the commit first landed, and counts
// in AfterGreen only when that run concluded success: the pull request
// reported green on content whose full run then failed. A pull request that
// several failed runs landed (a merge group, and the push of its merge commit)
// is one entry, judged by the earliest of them that followed a green run of
// it. A failed run that names no pull request, or whose pull request has no
// run before it in runs, or whose last run did not pass, is counted in
// FullRuns and FullRunsFailed and nowhere else. Those count run keys; the
// entries, AfterGreen and the lists count pull requests.
//
// The entry is filed by what the pull request's run did with the steps the
// full run failed, by lane (shard suffix removed, so tests.go-tests#2 compares
// as tests.go-tests), and by the worst of them:
//
//   - a step it planned and skipped as not affected is a selection miss, and
//     the entry is a false green;
//   - failing that, a step it never planned -- no pass and no such skip --
//     puts it in NotOnPullRequests: the selection was not asked about it;
//   - otherwise every failed step ran and passed as a step on the pull
//     request, or the run failed with no failed step of its own: a sibling
//     merge, a flake, or a failing package outside the step's slice, which a
//     count by step cannot tell apart.
//
// That last is the count's limit. It compares steps, not packages, so it is a
// floor on the package axis: a step that selects packages runs on a pull
// request over a slice of them, and a failing package outside that slice files
// as a sibling or a flake. No list proves the selection missed nothing, and a
// SiblingOrFlake entry on a lane that selects packages is for a person to look
// at.
//
// runs is one pipeline's runs: the window's, and as many earlier pull_request
// runs as the caller wants a landing to be able to pair with, since a pull
// request opened before the window can land inside it. Every full-mode run it
// holds is counted, so the window is the caller's cut of them. A row given
// twice is one run, and the order of runs changes nothing.
//
// steps holds each run's step facts, keyed by RunFacts.ID, for the runs
// RunsNeedingSteps names; only those are read. A caller seeds an empty list
// for each of them before it fills them: present and empty is the legitimate
// no-steps state, a run refused before any step began or one whose pipeline
// has no stage that runs on pull requests. An id RunsNeedingSteps names with
// no key at all is a read that did not happen -- a nil map, a run left out, a
// map keyed by the work run's id -- and is reported in Unread. A landing whose
// steps were not read is filed as though its run recorded nothing (a pull
// request's as not planned, quietly; a full run's as having failed no step),
// so a caller treats a non-empty Unread as an error and believes no list.
//
// The lists come newest landing first, and are never nil.
func CountFalseGreens(runs []RunFacts, steps map[string][]StepFacts) FalseGreenReport {
	read := readRuns(runs)
	report := FalseGreenReport{
		FalseGreens: []FalseGreen{}, NotOnPullRequests: []FalseGreen{}, SiblingOrFlake: []FalseGreen{},
		Unread: []string{},
	}
	for _, f := range read.full {
		report.FullRuns++
		if f.verdict.Conclusion == conclusionFailure {
			report.FullRunsFailed++
		}
	}
	for _, l := range read.landings {
		report.AfterGreen++
		green, selectionMiss, notPlanned := l.judge(steps)
		switch {
		case selectionMiss:
			report.FalseGreens = append(report.FalseGreens, green)
		case notPlanned:
			report.NotOnPullRequests = append(report.NotOnPullRequests, green)
		default:
			report.SiblingOrFlake = append(report.SiblingOrFlake, green)
		}
	}
	for _, id := range stepsNeeded(read.landings) {
		if _, given := steps[id]; !given {
			report.Unread = append(report.Unread, id)
		}
	}
	return report
}

// RunsNeedingSteps names the runs whose step facts CountFalseGreens reads: for
// each pull request that was green and whose commit failed, the failed full
// run that judges it (the latest attempt to reach a verdict of the earliest
// failed run) and the pull request's run it is judged against. Every other
// run is counted from its row alone, so a caller that loads the steps of these
// and of no others -- a dozen runs of a window of hundreds -- has read what
// the count needs, and seeds an empty list under each id before it fills them
// (see CountFalseGreens, and the report's Unread). The ids are sorted and the
// list is never nil; a pull request is one landing and a run belongs to one,
// so no id is named twice.
func RunsNeedingSteps(runs []RunFacts) []string {
	return stepsNeeded(readRuns(runs).landings)
}
