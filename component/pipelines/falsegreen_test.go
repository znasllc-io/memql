package pipelines

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
)

// The false-green count is what milestone M1 rests on (design record,
// section 7): a pull request whose run reported success, on a tree whose
// full-mode run then failed. Its fixtures are runs and steps as the rows hold
// them -- the work journal's step words ("done", "failed", "skipped") and the
// engine's own skip codes -- spelled out, because the count compares against
// what the journal writes and a constant read back from the code under test
// would agree with any value.

// fgQueueSHA is the head a merge queue's temporary branch names.
const fgQueueSHA = "b19c6b22ab1f138a3a69ed266fc59fb07c59f5ad"

var fgOrigin = time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

// fgAt is a moment `minutes` after the fixtures' origin; the runs are ordered
// by it.
func fgAt(minutes int) time.Time { return fgOrigin.Add(time.Duration(minutes) * time.Minute) }

func fgPullRequestRun(id string, number, minutes int, conclusion string) RunFacts {
	n := strconv.Itoa(number)
	return RunFacts{
		ID: id, Event: EventPullRequest, Mode: ModeAffected, PullRequest: number,
		HeadBranch: "feature-" + n, Title: "Pull request " + n,
		Conclusion: conclusion, QueuedAt: fgAt(minutes),
	}
}

func fgMergeTitle(number int) string {
	return "Merge pull request #" + strconv.Itoa(number) + " from Acme-Corp/cart-badge"
}

// fgPushRun is a push to the default branch landing pull request `number` by a
// merge commit.
func fgPushRun(id string, number, minutes int, conclusion string) RunFacts {
	return RunFacts{
		ID: id, Event: EventPush, Mode: ModeFull, HeadBranch: "main", Title: fgMergeTitle(number),
		Conclusion: conclusion, QueuedAt: fgAt(minutes),
	}
}

// fgPushOf is a push to the default branch with a commit subject of the
// test's choosing.
func fgPushOf(id, title string, minutes int, conclusion string) RunFacts {
	return RunFacts{
		ID: id, Event: EventPush, Mode: ModeFull, HeadBranch: "main", Title: title,
		Conclusion: conclusion, QueuedAt: fgAt(minutes),
	}
}

func fgMergeGroupRun(id string, number, minutes int, conclusion string) RunFacts {
	return RunFacts{
		ID: id, Event: EventMergeGroup, Mode: ModeFull,
		HeadBranch: "gh-readonly-queue/main/pr-" + strconv.Itoa(number) + "-" + fgQueueSHA,
		Title:      fgMergeTitle(number), Conclusion: conclusion, QueuedAt: fgAt(minutes),
	}
}

func fgDone(key string) StepFacts   { return StepFacts{Key: key, Status: "done"} }
func fgFailed(key string) StepFacts { return StepFacts{Key: key, Status: "failed"} }
func fgNotAffected(key string) StepFacts {
	return StepFacts{Key: key, Status: "skipped", Code: CodeNotAffected}
}
func fgCarried(key string) StepFacts {
	return StepFacts{Key: key, Status: "skipped", Code: CodePassedEarlier}
}

func fgOn(key, onPullRequest string) FalseGreenStep {
	return FalseGreenStep{Key: key, OnPullRequest: onPullRequest}
}

func fgGreen(pullRequest int, prRunID, fullRunID string, steps ...FalseGreenStep) FalseGreen {
	if steps == nil {
		steps = []FalseGreenStep{}
	}
	return FalseGreen{PullRequest: pullRequest, PRRunID: prRunID, FullRunID: fullRunID, Steps: steps}
}

// fgWant is a report whose lists are never nil: the report's contract is that
// they marshal as [], not null.
func fgWant(fullRuns, fullRunsFailed, afterGreen int, falseGreens, siblings []FalseGreen) FalseGreenReport {
	if falseGreens == nil {
		falseGreens = []FalseGreen{}
	}
	if siblings == nil {
		siblings = []FalseGreen{}
	}
	return FalseGreenReport{
		FullRuns: fullRuns, FullRunsFailed: fullRunsFailed, AfterGreen: afterGreen,
		FalseGreens: falseGreens, SiblingOrFlake: siblings,
	}
}

func fgAssertReport(t *testing.T, got, want FalseGreenReport) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(want, "", "  ")
	t.Errorf("report differs\n got: %s\nwant: %s", g, w)
}

func TestPullRequestOfNamesTheLandedPullRequest(t *testing.T) {
	queue := func(base string, n int) string {
		return "gh-readonly-queue/" + base + "/pr-" + strconv.Itoa(n) + "-" + fgQueueSHA
	}
	cases := []struct {
		name string
		run  RunFacts
		want int // 0: the run names no pull request
	}{
		// A merge group is named by the branch the queue made for it.
		{"a merge group's head branch", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: queue("main", 42)}, 42},
		{"a head ref still carrying refs/heads/", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: "refs/heads/" + queue("main", 42)}, 42},
		{"a base branch with slashes in it", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: queue("release/1.2", 7)}, 7},
		{"a base branch that itself reads like a queue entry", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: "gh-readonly-queue/pr-3-feedface/pr-9-" + fgQueueSHA}, 9},
		{"a head branch with no SHA after the number", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: "gh-readonly-queue/main/pr-42"}, 42},
		{"the branch outranks the commit's subject", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: queue("main", 42), Title: fgMergeTitle(99)}, 42},
		{"a merge group whose branch names nothing, by its merge commit", RunFacts{Event: EventMergeGroup, Mode: ModeFull, Title: fgMergeTitle(42)}, 42},
		{"a merge group whose branch names nothing, by its squash commit", RunFacts{Event: EventMergeGroup, Mode: ModeFull, Title: "Show the cart count on the badge (#43)"}, 43},

		// A push is named by its head commit's subject.
		{"a push of a merge commit", RunFacts{Event: EventPush, Mode: ModeFull, HeadBranch: "main", Title: "Merge pull request #5830 from znasllc-io/chore/pin-toolchain-digest"}, 5830},
		{"a push of a squash commit", RunFacts{Event: EventPush, Mode: ModeFull, HeadBranch: "main", Title: "Show the cart count on the badge (#42)"}, 42},
		{"a squash of a revert names the revert's own pull request", RunFacts{Event: EventPush, Mode: ModeFull, HeadBranch: "main", Title: `Revert "Show the cart count (#42)" (#57)`}, 57},
		{"only the first line of a merge message is read", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Merge pull request #42 from Acme-Corp/cart-badge\n\nCloses #7 (#9)"}, 42},
		{"only the first line of a squash message is read", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Show the cart count (#42)\n\nSee also (#9)"}, 42},
		{"a carriage return after the number", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Show the cart count (#42)\r\nbody"}, 42},
		{"a merge subject outranks a trailing reference", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Merge pull request #42 from Acme-Corp/cart-badge (#7)"}, 42},

		// A push that landed no pull request names none.
		{"a plain commit", RunFacts{Event: EventPush, Mode: ModeFull, HeadBranch: "main", Title: "Fix the cart badge"}, 0},
		{"a branch merge", RunFacts{Event: EventPush, Mode: ModeFull, HeadBranch: "main", Title: "Merge branch 'main' into cart-badge"}, 0},
		{"an issue reference is not a pull request", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Issue #5506: count false greens from run rows"}, 0},
		{"a reference that does not end the subject", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Fix the cart badge (#42) again"}, 0},
		{"a reference that is two numbers", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Fix the cart badge (#42, #43)"}, 0},
		{"a reference that is a number and words", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Fix the cart badge (#42 and #43)"}, 0},
		{"a reference with no number", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Fix the cart badge (#)"}, 0},
		{"a reference to nothing", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Fix the cart badge (#0)"}, 0},
		{"a signed number is not a number", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Fix the cart badge (#+42)"}, 0},
		{"a merge subject with no number", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Merge pull request #x from Acme-Corp/cart-badge"}, 0},
		{"a merge subject naming no branch", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Merge pull request #42 is blocked on review"}, 0},
		{"a number too large to be a pull request", RunFacts{Event: EventPush, Mode: ModeFull, Title: "Fix the cart badge (#99999999999999999999999)"}, 0},
		{"a push with no subject", RunFacts{Event: EventPush, Mode: ModeFull, HeadBranch: "main"}, 0},

		// A merge group's branch that is not a queue entry names none.
		{"a queue ref with no pull request segment", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: "gh-readonly-queue/main"}, 0},
		{"a pull request segment with no number", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: "gh-readonly-queue/main/pr-x-" + fgQueueSHA}, 0},
		{"a number that runs into letters", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: "gh-readonly-queue/main/pr-42abc"}, 0},
		{"a queue entry for no pull request", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: queue("main", 0)}, 0},
		{"a branch that only looks like a queue entry", RunFacts{Event: EventMergeGroup, Mode: ModeFull, HeadBranch: "feature/pr-42-" + fgQueueSHA}, 0},

		// What a release and a pull request run are.
		{"a release names none, whatever its name says", RunFacts{Event: EventRelease, Mode: ModeFull, Title: fgMergeTitle(42)}, 0},
		{"a release named like a squash commit", RunFacts{Event: EventRelease, Mode: ModeFull, Title: "Release 0.25.0 (#42)"}, 0},
		{"a pull request run is its own number", RunFacts{Event: EventPullRequest, Mode: ModeAffected, PullRequest: 9}, 9},
		{"a pull request run with no number names none, whatever its title says", RunFacts{Event: EventPullRequest, Mode: ModeAffected, Title: "Show the cart count (#42)"}, 0},
		{"a number the row carries outranks its words", RunFacts{Event: EventPush, Mode: ModeFull, PullRequest: 9, Title: "Show the cart count (#42)"}, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := PullRequestOf(c.run)
			if c.want == 0 {
				if ok || got != 0 {
					t.Fatalf("PullRequestOf = (%d, %v), want it to name no pull request", got, ok)
				}
				return
			}
			if !ok || got != c.want {
				t.Fatalf("PullRequestOf = (%d, %v), want (%d, true)", got, ok, c.want)
			}
		})
	}
}

func TestLaneOfStripsTheShardSuffixAndNothingElse(t *testing.T) {
	for key, want := range map[string]string{
		"tests.go-tests#2":   "tests.go-tests",
		"tests.go-tests#12":  "tests.go-tests",
		"tests.go-tests":     "tests.go-tests",
		"tests.go-tests#":    "tests.go-tests#",
		"tests.go-tests#x":   "tests.go-tests#x",
		"tests.go-tests#1x":  "tests.go-tests#1x",
		"tests.go-tests#+1":  "tests.go-tests#+1",
		" tests.go-tests#2 ": "tests.go-tests",
		"":                   "",
	} {
		if got := laneOf(key); got != want {
			t.Errorf("laneOf(%q) = %q, want %q", key, got, want)
		}
	}
}

// The pull request's affected selection did not run a step the whole tree's
// run then failed: the case the affected-subset decision rests on.
func TestASelectionMissIsAFalseGreen(t *testing.T) {
	runs := []RunFacts{
		fgPullRequestRun("pr-a", 42, 0, "success"),
		fgPushRun("main-a", 42, 90, "failure"),
	}
	steps := map[string][]StepFacts{
		"pr-a": {
			fgDone("checks.build-vet"), fgDone("tests.go-tests#1"), fgDone("tests.go-tests#2"),
			fgNotAffected("tests.os-checks"),
		},
		"main-a": {
			fgDone("checks.build-vet"), fgDone("tests.go-tests#1"), fgDone("tests.go-tests#2"),
			fgFailed("tests.os-checks"),
		},
	}
	fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1,
		[]FalseGreen{fgGreen(42, "pr-a", "main-a", fgOn("tests.os-checks", "not selected"))}, nil))
}

// A stage that does not run on pull requests is absent from their plan, and
// a step of it that fails on the tree was never one the pull request ran.
func TestAStepThePullRequestNeverPlannedIsNotSelected(t *testing.T) {
	runs := []RunFacts{
		fgPullRequestRun("pr-a", 17, 0, "success"),
		fgMergeGroupRun("queue-a", 17, 40, "failure"),
	}
	steps := map[string][]StepFacts{
		"pr-a":    {fgDone("checks.build-vet")},
		"queue-a": {fgDone("checks.build-vet"), fgFailed("docs.bundle")},
	}
	fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1,
		[]FalseGreen{fgGreen(17, "pr-a", "queue-a", fgOn("docs.bundle", "not selected"))}, nil))
}

// Every failed step ran and passed on the pull request, so the content
// passed: the tree the queue built was another one, or the step is flaky.
func TestAStepThatPassedOnThePullRequestIsASiblingOrAFlake(t *testing.T) {
	runs := []RunFacts{
		fgPullRequestRun("pr-a", 31, 0, "success"),
		fgMergeGroupRun("queue-a", 31, 45, "failure"),
	}
	steps := map[string][]StepFacts{
		"pr-a":    {fgDone("checks.build-vet"), fgDone("tests.db-tests")},
		"queue-a": {fgDone("checks.build-vet"), fgFailed("tests.db-tests")},
	}
	fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1, nil,
		[]FalseGreen{fgGreen(31, "pr-a", "queue-a", fgOn("tests.db-tests", "passed"))}))
}

// tests.go-tests#2 compares as tests.go-tests, whatever shards either run
// split the step into.
func TestShardsAreComparedAsOneLane(t *testing.T) {
	t.Run("the pull request ran fewer shards than the tree did", func(t *testing.T) {
		runs := []RunFacts{fgPullRequestRun("pr-a", 8, 0, "success"), fgPushRun("main-a", 8, 50, "failure")}
		steps := map[string][]StepFacts{
			"pr-a":   {fgDone("tests.db-tests#1"), fgDone("tests.db-tests#2")},
			"main-a": {fgDone("tests.db-tests#1"), fgDone("tests.db-tests#2"), fgFailed("tests.db-tests#3"), fgDone("tests.db-tests#4")},
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1, nil,
			[]FalseGreen{fgGreen(8, "pr-a", "main-a", fgOn("tests.db-tests", "passed"))}))
	})
	t.Run("the pull request skipped the whole lane under its plain key", func(t *testing.T) {
		// A step with nothing selected compiles to ONE skipped step under the
		// plain key, never to shards; the tree's failing shard is its lane.
		runs := []RunFacts{fgPullRequestRun("pr-a", 9, 0, "success"), fgPushRun("main-a", 9, 50, "failure")}
		steps := map[string][]StepFacts{
			"pr-a":   {fgNotAffected("tests.go-tests")},
			"main-a": {fgDone("tests.go-tests#1"), fgFailed("tests.go-tests#2")},
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1,
			[]FalseGreen{fgGreen(9, "pr-a", "main-a", fgOn("tests.go-tests", "not selected"))}, nil))
	})
	t.Run("several failed shards of one lane are one step", func(t *testing.T) {
		runs := []RunFacts{fgPullRequestRun("pr-a", 10, 0, "success"), fgPushRun("main-a", 10, 50, "failure")}
		steps := map[string][]StepFacts{
			"pr-a":   {fgNotAffected("tests.go-tests")},
			"main-a": {fgFailed("tests.go-tests#1"), fgFailed("tests.go-tests#4"), fgFailed("tests.go-tests#2")},
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1,
			[]FalseGreen{fgGreen(10, "pr-a", "main-a", fgOn("tests.go-tests", "not selected"))}, nil))
	})
	t.Run("one shard running is the lane running", func(t *testing.T) {
		// An affected selection that leaves a shard with nothing skips that
		// shard; the lane still ran, which is the granularity the count has.
		runs := []RunFacts{fgPullRequestRun("pr-a", 11, 0, "success"), fgPushRun("main-a", 11, 50, "failure")}
		steps := map[string][]StepFacts{
			"pr-a":   {fgNotAffected("tests.go-tests#1"), fgDone("tests.go-tests#2")},
			"main-a": {fgFailed("tests.go-tests#1")},
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1, nil,
			[]FalseGreen{fgGreen(11, "pr-a", "main-a", fgOn("tests.go-tests", "passed"))}))
	})
}

// A false green lists every failed step with what the pull request did with
// it, in the order the tree's run holds them -- not sorted, not one only.
func TestEveryFailedStepIsListedOnceInTheFullRunsOrder(t *testing.T) {
	runs := []RunFacts{fgPullRequestRun("pr-a", 12, 0, "success"), fgPushRun("main-a", 12, 50, "failure")}
	steps := map[string][]StepFacts{
		"pr-a": {
			fgDone("checks.generated"), fgDone("tests.go-tests#1"), fgNotAffected("tests.os-checks"),
		},
		"main-a": {
			fgDone("checks.build-vet"),
			fgFailed("tests.os-checks"), fgFailed("checks.generated"), fgFailed("tests.go-tests#2"),
			fgFailed("tests.go-tests#1"),
		},
	}
	fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1,
		[]FalseGreen{fgGreen(12, "pr-a", "main-a",
			fgOn("tests.os-checks", "not selected"),
			fgOn("checks.generated", "passed"),
			fgOn("tests.go-tests", "passed"),
		)}, nil))
}

// A step passed on the pull request only on evidence that it passed: a step
// the selection skipped, never planned, cancelled, still pending or in a
// state this code does not know is not one.
func TestAStepPassedOnThePullRequestOnlyOnEvidence(t *testing.T) {
	for _, c := range []struct {
		name string
		step StepFacts // what the pull request's run recorded for tests.lane
		want string
	}{
		{"done, the journal's word for a pass", StepFacts{Key: "tests.lane", Status: "done"}, "passed"},
		{"succeeded, the check run's word for a pass", StepFacts{Key: "tests.lane", Status: "succeeded"}, "passed"},
		{"carried from an earlier attempt that passed it", fgCarried("tests.lane"), "passed"},
		{"skipped because nothing it covers changed", fgNotAffected("tests.lane"), "not selected"},
		{"skipped because an earlier stage failed", StepFacts{Key: "tests.lane", Status: "skipped", Code: CodeStageBlocked}, "not selected"},
		{"skipped for no stated reason", StepFacts{Key: "tests.lane", Status: "skipped"}, "not selected"},
		{"cancelled", StepFacts{Key: "tests.lane", Status: "cancelled"}, "not selected"},
		{"failed, which a run that concluded success cannot hold", StepFacts{Key: "tests.lane", Status: "failed"}, "not selected"},
		{"still pending", StepFacts{Key: "tests.lane", Status: "pending"}, "not selected"},
		{"still running", StepFacts{Key: "tests.lane", Status: "running"}, "not selected"},
		{"no status", StepFacts{Key: "tests.lane"}, "not selected"},
		{"a status nobody writes", StepFacts{Key: "tests.lane", Status: "completed"}, "not selected"},
		{"a pass under another code is still a pass", StepFacts{Key: "tests.lane", Status: "done", Code: CodeLogCapped}, "passed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runs := []RunFacts{fgPullRequestRun("pr-a", 20, 0, "success"), fgPushRun("main-a", 20, 50, "failure")}
			steps := map[string][]StepFacts{
				"pr-a":   {c.step},
				"main-a": {fgFailed("tests.lane")},
			}
			got := CountFalseGreens(runs, steps)
			green := fgGreen(20, "pr-a", "main-a", fgOn("tests.lane", c.want))
			if c.want == "passed" {
				fgAssertReport(t, got, fgWant(1, 1, 1, nil, []FalseGreen{green}))
				return
			}
			fgAssertReport(t, got, fgWant(1, 1, 1, []FalseGreen{green}, nil))
		})
	}
}

// What makes a step of the tree's run a failure of it, in either vocabulary a
// caller may hold the statuses in.
func TestWhatFailedOnTheFullRun(t *testing.T) {
	runs := []RunFacts{fgPullRequestRun("pr-a", 21, 0, "success"), fgPushRun("main-a", 21, 50, "failure")}
	steps := map[string][]StepFacts{
		"pr-a": {fgDone("a.passed")},
		"main-a": {
			{Key: "a.failed", Status: "failed", Code: CodeStepTimeout},
			{Key: "b.refused", Status: "refused"},
			{Key: "c.cancelled", Status: "cancelled"},
			{Key: "d.done", Status: "done"},
			{Key: "e.succeeded", Status: "succeeded"},
			{Key: "f.blocked", Status: "skipped", Code: CodeStageBlocked},
			{Key: "g.pending", Status: "pending"},
			{Key: "h.running", Status: "running"},
			{Key: "", Status: "failed"},
		},
	}
	fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1,
		[]FalseGreen{fgGreen(21, "pr-a", "main-a", fgOn("a.failed", "not selected"), fgOn("b.refused", "not selected"))}, nil))
}

// A failed run that has no failed step failed as a run: a manifest that did
// not compile, a runner that was not there. The pull request's selection did
// not miss anything, and the partition of AfterGreen stays whole.
func TestAFailedFullRunWithNoFailedStepIsNotASelectionMiss(t *testing.T) {
	runs := []RunFacts{
		fgPullRequestRun("pr-a", 22, 0, "success"),
		fgPushRun("main-a", 22, 50, "failure"),
		fgPullRequestRun("pr-b", 23, 5, "success"),
		fgPushRun("main-b", 23, 55, "failure"),
	}
	steps := map[string][]StepFacts{
		"pr-a":   {fgDone("checks.build-vet")},
		"main-a": {fgDone("checks.build-vet")}, // every step passed, and the run failed
		"pr-b":   {fgDone("checks.build-vet")},
		// no work run at all: the run was refused before any step began
	}
	got := CountFalseGreens(runs, steps)
	fgAssertReport(t, got, fgWant(2, 2, 2, nil, []FalseGreen{
		fgGreen(23, "pr-b", "main-b"),
		fgGreen(22, "pr-a", "main-a"),
	}))
	if got.AfterGreen != len(got.FalseGreens)+len(got.SiblingOrFlake) {
		t.Errorf("AfterGreen = %d, but the two lists hold %d", got.AfterGreen, len(got.FalseGreens)+len(got.SiblingOrFlake))
	}
}

// The pull request's run is its newest one queued before the tree's run, and
// the pair counts only when that one passed.
func TestOnlyThePullRequestsNewestRunBeforeTheFullRunIsItsPair(t *testing.T) {
	steps := map[string][]StepFacts{
		"a": {fgDone("tests.lane")}, "b": {fgDone("tests.lane")}, "c": {fgDone("tests.lane")},
		"main": {fgFailed("tests.lane")},
	}
	t.Run("a pull request run that failed is not a false green", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("a", 5, 0, "success"),
			fgPullRequestRun("b", 5, 30, "failure"), // the newest before the push: red
			fgPushRun("main", 5, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 0, nil, nil))
	})
	t.Run("a green run after a red one is the pair", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("a", 6, 0, "failure"),
			fgPullRequestRun("b", 6, 30, "success"),
			fgPushRun("main", 6, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1, nil,
			[]FalseGreen{fgGreen(6, "b", "main", fgOn("tests.lane", "passed"))}))
	})
	t.Run("an older green run does not stand for a newer red one", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("a", 7, 0, "success"),
			fgPullRequestRun("b", 7, 30, "cancelled"),
			fgPushRun("main", 7, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 0, nil, nil))
	})
	t.Run("a run still in progress is the newest, and green by nothing", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("a", 8, 0, "success"),
			fgPullRequestRun("b", 8, 30, ""),
			fgPushRun("main", 8, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 0, nil, nil))
	})
	t.Run("a run queued after the full run is not its pair, and the earlier one still is", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("a", 9, 0, "success"),
			fgPullRequestRun("c", 9, 200, "failure"),
			fgPushRun("main", 9, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1, nil,
			[]FalseGreen{fgGreen(9, "a", "main", fgOn("tests.lane", "passed"))}))
	})
	t.Run("a pull request run queued only after the full run pairs with nothing", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("c", 10, 200, "success"),
			fgPushRun("main", 10, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 0, nil, nil))
	})
	t.Run("a run queued in the very instant of the full run is not before it", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("c", 14, 100, "success"),
			fgPushRun("main", 14, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 0, nil, nil))
	})
	t.Run("another pull request's runs are nobody's pair", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("a", 11, 0, "success"),
			fgPushRun("main", 12, 100, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 0, nil, nil))
	})
}

func TestAFullRunWithNoPullRequestRunIsNotCounted(t *testing.T) {
	runs := []RunFacts{
		fgPullRequestRun("pr-a", 42, 0, "success"),
		fgPushRun("main-a", 99, 50, "failure"),                  // lands #99, which this pipeline never ran
		fgPushOf("main-b", "Fix the cart badge", 60, "failure"), // lands no pull request
		fgPushOf("main-c", "Merge branch 'main' into x", 70, "failure"),
		{ID: "release-a", Event: EventRelease, Mode: ModeFull, Title: "Release 0.25.0", Conclusion: "failure", QueuedAt: fgAt(80)},
	}
	steps := map[string][]StepFacts{"pr-a": {fgDone("tests.lane")}}
	fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(4, 4, 0, nil, nil))
}

// A full run with no queue time cannot be placed after anything.
func TestAFullRunQueuedAtNoTimePairsWithNothing(t *testing.T) {
	full := fgPushRun("main-a", 42, 0, "failure")
	full.QueuedAt = time.Time{}
	runs := []RunFacts{fgPullRequestRun("pr-a", 42, 0, "success"), full}
	fgAssertReport(t, CountFalseGreens(runs, nil), fgWant(1, 1, 0, nil, nil))
}

// A tree that passed is not the pull request's run: the event says what a
// pull request's run is, whatever number a row carries.
func TestAFullRunIsNeverThePullRequestsRun(t *testing.T) {
	t.Run("a merge group that passed", func(t *testing.T) {
		runs := []RunFacts{
			fgMergeGroupRun("queue-a", 42, 40, "success"),
			fgPushRun("main-a", 42, 60, "failure"),
		}
		fgAssertReport(t, CountFalseGreens(runs, nil), fgWant(2, 1, 0, nil, nil))
	})
	t.Run("even when its row carries the pull request's number", func(t *testing.T) {
		queue := fgMergeGroupRun("queue-a", 42, 40, "success")
		queue.PullRequest = 42
		push := fgPushRun("main-a", 42, 60, "failure") // names #42 by its subject alone
		fgAssertReport(t, CountFalseGreens([]RunFacts{queue, push}, nil), fgWant(2, 1, 0, nil, nil))
	})
}

// A failed-only re-run carries what an earlier attempt passed: the content
// passed, so the step is not one the selection missed.
func TestAStepCarriedFromAnEarlierAttemptPassedOnThePullRequest(t *testing.T) {
	runs := []RunFacts{fgPullRequestRun("pr-a", 25, 0, "success"), fgPushRun("main-a", 25, 50, "failure")}
	steps := map[string][]StepFacts{
		"pr-a":   {fgCarried("tests.os-checks"), fgDone("tests.go-tests#1")},
		"main-a": {fgFailed("tests.os-checks")},
	}
	fgAssertReport(t, CountFalseGreens(runs, steps), fgWant(1, 1, 1, nil,
		[]FalseGreen{fgGreen(25, "pr-a", "main-a", fgOn("tests.os-checks", "passed"))}))
}

// Only a run with a verdict on the content is a full run of the window: one
// still running, cancelled or refused says nothing about the tree.
func TestTheWindowCountsEveryFullRunWithAVerdict(t *testing.T) {
	runs, steps := fgWindow()
	got := CountFalseGreens(runs, steps)
	fgAssertReport(t, got, fgWant(7, 5, 2,
		[]FalseGreen{fgGreen(3, "pr3-a", "main-3", fgOn("tests.os-checks", "not selected"))},
		[]FalseGreen{fgGreen(4, "pr4-a", "queue-4", fgOn("tests.db-tests", "passed"))}))

	// Pull request runs and unfinished runs are no full run.
	only := []RunFacts{
		fgPullRequestRun("pr-a", 1, 0, "success"),
		fgPushRun("main-a", 1, 10, ""),
		fgPushRun("main-b", 2, 20, "cancelled"),
		fgPushRun("main-c", 3, 30, "refused"),
	}
	fgAssertReport(t, CountFalseGreens(only, nil), fgWant(0, 0, 0, nil, nil))
}

// fgWindow is a pipeline's two weeks, in minutes: nine full runs and the pull
// request runs they landed. The steps are given for only the four runs a
// pairing reads; the count must not need another.
func fgWindow() ([]RunFacts, map[string][]StepFacts) {
	runs := []RunFacts{
		fgPullRequestRun("pr1-a", 1, 0, "success"),
		fgPullRequestRun("pr3-a", 3, 10, "success"),
		fgPullRequestRun("pr4-a", 4, 20, "success"),
		fgPullRequestRun("pr5-a", 5, 25, "success"),
		fgPullRequestRun("pr5-b", 5, 35, "failure"),

		fgPushRun("main-1", 1, 5, "success"),
		fgMergeGroupRun("queue-2", 2, 15, "success"),
		fgPushRun("main-3", 3, 50, "failure"),
		fgMergeGroupRun("queue-4", 4, 60, "failure"),
		fgPushRun("main-5", 5, 100, "failure"),
		fgPushOf("main-6", "Fix the cart badge", 110, "failure"),
		fgPushRun("main-7", 7, 120, "cancelled"),
		fgPushRun("main-8", 8, 130, ""),
		{ID: "release-9", Event: EventRelease, Mode: ModeFull, Title: "Release 0.25.0", Conclusion: "failure", QueuedAt: fgAt(140)},
	}
	steps := map[string][]StepFacts{
		"pr3-a": {
			fgDone("checks.build-vet"), fgDone("tests.go-tests#1"), fgDone("tests.go-tests#2"),
			fgNotAffected("tests.os-checks"),
		},
		"main-3": {
			fgDone("checks.build-vet"), fgDone("tests.go-tests#1"), fgDone("tests.go-tests#2"),
			fgFailed("tests.os-checks"),
		},
		"pr4-a":   {fgDone("checks.build-vet"), fgDone("tests.db-tests#1"), fgDone("tests.db-tests#2")},
		"queue-4": {fgDone("tests.db-tests#1"), fgDone("tests.db-tests#2"), fgFailed("tests.db-tests#3")},
	}
	return runs, steps
}

// The same row twice is one run: an offset-paged read of a table that is
// being written to can return it on two pages.
func TestARowGivenTwiceIsOneRun(t *testing.T) {
	runs, steps := fgWindow()
	once := CountFalseGreens(runs, steps)
	twice := CountFalseGreens(append(slices.Clone(runs), runs...), steps)
	fgAssertReport(t, twice, once)
}

// The report does not depend on the order the runs arrive in, ties between
// runs queued in the same instant included.
func TestTheCountDoesNotDependOnTheOrderOfItsInput(t *testing.T) {
	runs, steps := fgWindow()
	// Two runs of one pull request queued in the same instant, one green and
	// one red, with a failing tree after them.
	runs = append(runs,
		fgPullRequestRun("pr6-a", 6, 150, "failure"),
		fgPullRequestRun("pr6-b", 6, 150, "success"),
		fgPushRun("main-10", 6, 170, "failure"),
		fgPullRequestRun("pr12-a", 12, 160, "success"),
		fgPushRun("main-11", 12, 180, "failure"),
	)
	steps["pr6-a"] = []StepFacts{fgDone("tests.lane")}
	steps["pr6-b"] = []StepFacts{fgDone("tests.lane")}
	steps["main-10"] = []StepFacts{fgFailed("tests.lane")}
	steps["pr12-a"] = []StepFacts{fgNotAffected("tests.lane")}
	steps["main-11"] = []StepFacts{fgFailed("tests.lane")}
	want := CountFalseGreens(runs, steps)
	// main-3, queue-4 and main-11 pair whatever the tie decides; main-10 is
	// the landing the tie is about, and the shuffles are what show it settled.
	if want.AfterGreen < 3 {
		t.Fatalf("the fixture should pair at least three failed runs, paired %d: %+v", want.AfterGreen, want)
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 50; i++ {
		shuffled := slices.Clone(runs)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := CountFalseGreens(shuffled, steps); !reflect.DeepEqual(got, want) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			t.Fatalf("shuffle %d changed the report\n got: %s\nwant: %s", i, g, w)
		}
	}
}

// The list a person reads starts with the newest landing.
func TestFalseGreensAreListedNewestFirst(t *testing.T) {
	runs := []RunFacts{
		fgPushRun("main-b", 11, 400, "failure"),
		fgPullRequestRun("pr-a", 10, 0, "success"),
		fgPushRun("main-a", 10, 200, "failure"),
		fgPullRequestRun("pr-c", 12, 20, "success"),
		fgPullRequestRun("pr-b", 11, 10, "success"),
		fgPushRun("main-c", 12, 300, "failure"),
	}
	steps := map[string][]StepFacts{
		"pr-a": {fgNotAffected("tests.lane")}, "pr-b": {fgNotAffected("tests.lane")}, "pr-c": {fgNotAffected("tests.lane")},
		"main-a": {fgFailed("tests.lane")}, "main-b": {fgFailed("tests.lane")}, "main-c": {fgFailed("tests.lane")},
	}
	got := CountFalseGreens(runs, steps)
	var order []string
	for _, g := range got.FalseGreens {
		order = append(order, g.FullRunID)
	}
	if want := []string{"main-b", "main-c", "main-a"}; !slices.Equal(order, want) {
		t.Fatalf("false greens listed %v, want %v", order, want)
	}
}

// The report is what the builtin answers with, so its JSON names are a wire:
// pinned here as the client reads them, and a list with nothing in it is [].
func TestTheReportIsTheWire(t *testing.T) {
	runs, steps := fgWindow()
	got, err := json.Marshal(CountFalseGreens(runs, steps))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fullRuns":7,"fullRunsFailed":5,"afterGreen":2,` +
		`"falseGreens":[{"pullRequest":3,"prRunId":"pr3-a","fullRunId":"main-3","steps":[{"key":"tests.os-checks","onPullRequest":"not selected"}]}],` +
		`"siblingOrFlake":[{"pullRequest":4,"prRunId":"pr4-a","fullRunId":"queue-4","steps":[{"key":"tests.db-tests","onPullRequest":"passed"}]}]}`
	if string(got) != want {
		t.Errorf("wire form\n got: %s\nwant: %s", got, want)
	}

	empty, err := json.Marshal(CountFalseGreens(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"fullRuns":0,"fullRunsFailed":0,"afterGreen":0,"falseGreens":[],"siblingOrFlake":[]}`; string(empty) != want {
		t.Errorf("an empty report\n got: %s\nwant: %s", empty, want)
	}
}

// The two words a step's disposition is spelled in are what the client shows.
func TestTheDispositionWordsAreThePullRequestsOwn(t *testing.T) {
	if OnPullRequestNotSelected != "not selected" || OnPullRequestPassed != "passed" {
		t.Errorf("dispositions are %q and %q", OnPullRequestNotSelected, OnPullRequestPassed)
	}
}

// RunsNeedingSteps is the pairing, told to the caller before it reads a
// single step: the runs a landing is judged from, and no other.
func TestRunsNeedingStepsAreTheRunsTheCountReads(t *testing.T) {
	runs, steps := fgWindow()
	got := RunsNeedingSteps(runs)
	if want := []string{"main-3", "pr3-a", "pr4-a", "queue-4"}; !slices.Equal(got, want) {
		t.Fatalf("RunsNeedingSteps = %v, want %v", got, want)
	}
	// Reading exactly those runs' steps is enough: fgWindow holds no others.
	for _, id := range got {
		if len(steps[id]) == 0 {
			t.Errorf("fixture holds no steps for %s", id)
		}
	}

	t.Run("two failed runs of one pull request name its run once", func(t *testing.T) {
		runs := []RunFacts{
			fgPullRequestRun("pr-a", 5, 0, "success"),
			fgMergeGroupRun("queue-a", 5, 40, "failure"),
			fgPushRun("main-a", 5, 60, "failure"),
		}
		if got, want := RunsNeedingSteps(runs), []string{"main-a", "pr-a", "queue-a"}; !slices.Equal(got, want) {
			t.Fatalf("RunsNeedingSteps = %v, want %v", got, want)
		}
	})
	t.Run("a pull request that did not pass needs nothing read", func(t *testing.T) {
		runs := []RunFacts{fgPullRequestRun("pr-a", 5, 0, "failure"), fgPushRun("main-a", 5, 60, "failure")}
		if got := RunsNeedingSteps(runs); len(got) != 0 {
			t.Fatalf("RunsNeedingSteps = %v, want none", got)
		}
	})
	t.Run("nothing is an empty list, not nil", func(t *testing.T) {
		if got := RunsNeedingSteps(nil); got == nil || len(got) != 0 {
			t.Fatalf("RunsNeedingSteps(nil) = %#v, want an empty list", got)
		}
	})
}

func TestCountFalseGreensOfNothing(t *testing.T) {
	fgAssertReport(t, CountFalseGreens(nil, nil), fgWant(0, 0, 0, nil, nil))
	fgAssertReport(t, CountFalseGreens([]RunFacts{}, map[string][]StepFacts{}), fgWant(0, 0, 0, nil, nil))
}
