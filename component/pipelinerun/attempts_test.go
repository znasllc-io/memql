package pipelinerun

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

// attempts_test.go -- where a step's next attempt goes (spec 2026-10-28
// fleet-first retries, section 5, as the controller's rulings amend it) and the
// record each attempt leaves in the step's binding (section 3).

// The steps the table retries: one per placement, as the compiler leaves them.
var (
	retryFleetStep = pipelines.Step{Key: "build.mac", Stage: "build", Name: "mac", Kind: pipelines.StepCommand,
		Placement: pipelines.PlacementFleet, Execution: pipelines.ExecutionNative, Platform: "darwin/arm64"}
	retryPoolStep = pipelines.Step{Key: "checks.vet", Stage: "checks", Name: "vet", Kind: pipelines.StepCommand,
		Placement: pipelines.PlacementPool, Execution: pipelines.ExecutionContainer, Platform: "linux/amd64"}
	retryClusterStep = pipelines.Step{Key: "checks.lint", Stage: "checks", Name: "lint", Kind: pipelines.StepCommand,
		Placement: pipelines.PlacementCluster, Execution: pipelines.ExecutionContainer}
)

func repeatableStep(s pipelines.Step) pipelines.Step {
	s.Repeatable = true
	return s
}

// The records the table is written in. An attempt on a machine names it; the
// fleet answering for an unpinned failover that it found no machine for names
// none; a cluster attempt names the cluster.
func on(machine string, class pipelines.AttemptClass) attemptRecord {
	return attemptRecord{Class: class, Code: "pipeline_clone_failed", Surface: pipelines.PlacementFleet, WorkerID: machine}
}

func notRetried(r attemptRecord) attemptRecord {
	r.NotRetriedHere = true
	return r
}

func onCluster(class pipelines.AttemptClass) attemptRecord {
	return attemptRecord{Class: class, Code: "pipeline_image_pull_failed", Surface: pipelines.PlacementCluster, JobName: "job"}
}

var noMachine = attemptRecord{Class: pipelines.AttemptNothingRan, Code: pipelines.CodeNoMachineForNeed, Surface: pipelines.PlacementFleet}

// numbered is records as a step's history: attempt 1, 2, ... in order.
func numbered(records ...attemptRecord) []attemptRecord {
	out := make([]attemptRecord, len(records))
	for i, r := range records {
		r.Attempt = i + 1
		out[i] = r
	}
	return out
}

func repeatRecord(r attemptRecord, n int) []attemptRecord {
	out := make([]attemptRecord, n)
	for i := range out {
		out[i] = r
	}
	return out
}

const (
	nothingRan     = pipelines.AttemptNothingRan
	retryRequested = pipelines.AttemptRetryRequested
	unknownOutcome = pipelines.AttemptOutcomeUnknown
)

func pinnedTo(machine string, wait int) *pipelines.AttemptPlacement {
	return &pipelines.AttemptPlacement{Surface: pipelines.PlacementFleet, PinWorker: machine, MaxWaitSeconds: wait}
}

func failoverFrom(wait int, tried ...string) *pipelines.AttemptPlacement {
	return &pipelines.AttemptPlacement{Surface: pipelines.PlacementFleet, ExcludeWorkers: tried, MaxWaitSeconds: wait}
}

var toCluster = &pipelines.AttemptPlacement{Surface: pipelines.PlacementCluster}

func TestChooseNextFollowsTheRetryOrder(t *testing.T) {
	// The spec's example installation (section 6): the portable retries, a
	// minute's Fleet preference, cloud allowed.
	standard := pipelines.ComputePlacementPolicy{FleetWaitSeconds: 60, CloudFallback: true, Retries: pipelines.DefaultRetryPolicy()}
	noCloud := standard
	noCloud.CloudFallback = false
	noFleetWait := standard
	noFleetWait.FleetWaitSeconds = 0
	noOtherMachines := standard
	noOtherMachines.Retries.OtherMachines = false
	oneDelay := standard
	oneDelay.Retries = pipelines.RetryPolicy{SameMachine: 3, DelaysSeconds: []int{10}, OtherMachines: true}
	// A policy built by hand: its zero retries retry nothing, and its delays are
	// an EMPTY list, which must not be indexed.
	handBuilt := pipelines.ComputePlacementPolicy{Retries: pipelines.RetryPolicy{SameMachine: 1}}

	type want struct {
		placement *pipelines.AttemptPlacement
		delay     time.Duration
		ok        bool
	}
	next := func(p *pipelines.AttemptPlacement, delay time.Duration) want { return want{p, delay, true} }
	stop := want{}

	cases := []struct {
		name    string
		step    pipelines.Step
		policy  pipelines.ComputePlacementPolicy
		history []attemptRecord
		want    want
	}{
		// Rule 1: the same machine, after its delays, then rule 2 at once. Every
		// pinned attempt waits for its machine at most the largest delay.
		{"nothing ran on M once: M again after 30 s", retryFleetStep, standard,
			numbered(on("M", nothingRan)), next(pinnedTo("M", 120), 30*time.Second)},
		{"twice: M again after 120 s", retryFleetStep, standard,
			numbered(on("M", nothingRan), on("M", nothingRan)), next(pinnedTo("M", 120), 120*time.Second)},
		{"three times: any machine but M, at once", retryFleetStep, standard,
			numbered(on("M", nothingRan), on("M", nothingRan), on("M", nothingRan)), next(failoverFrom(0, "M"), 0)},
		{"M not retried here: any machine but M, at once", retryFleetStep, standard,
			numbered(notRetried(on("M", nothingRan))), next(failoverFrom(0, "M"), 0)},
		{"the wait for pinned M ran out: any machine but M, at once", retryFleetStep, standard,
			numbered(on("M", nothingRan), notRetried(on("M", nothingRan))), next(failoverFrom(0, "M"), 0)},
		{"each new machine has its own count", retryFleetStep, standard,
			numbered(on("M", nothingRan), on("M", nothingRan), on("M", nothingRan), on("N", nothingRan)),
			next(pinnedTo("N", 120), 30*time.Second)},
		{"the last delay repeats", retryFleetStep, oneDelay,
			numbered(on("M", nothingRan), on("M", nothingRan), on("M", nothingRan)), next(pinnedTo("M", 10), 10*time.Second)},
		{"no other machines once M is spent", retryFleetStep, noOtherMachines,
			numbered(on("M", nothingRan), on("M", nothingRan), on("M", nothingRan)), stop},

		// Ruling 9: an unpinned failover that comes back nothing ran and not
		// retried here, or naming no machine at all, found the fleet exhausted.
		// A fleet step stops there; the machine a spent wait names is never
		// pinned, because it was only the last one busy.
		{"fleet step, every eligible machine tried", retryFleetStep, standard,
			numbered(notRetried(on("M", nothingRan)), noMachine), stop},
		{"fleet step, the failover found only a machine not retried here", retryFleetStep, standard,
			numbered(notRetried(on("M", nothingRan)), notRetried(on("N", nothingRan))), stop},
		{"pool step, the failover's wait ran out on a busy machine: never pin it", retryPoolStep, noCloud,
			numbered(notRetried(on("M", nothingRan)), notRetried(on("N", nothingRan))), stop},
		{"fleet step, never the cluster", retryFleetStep, standard,
			numbered(on("M", nothingRan), on("M", nothingRan), on("M", nothingRan), noMachine), stop},

		// Rule 3: a pool step's machines exhausted, the cluster once.
		{"pool step, machines exhausted, cloud allowed: the cluster", retryPoolStep, standard,
			numbered(notRetried(on("M", nothingRan)), noMachine), next(toCluster, 0)},
		{"pool step, machines exhausted after their same-machine retries", retryPoolStep, standard,
			numbered(on("M", nothingRan), on("M", nothingRan), on("M", nothingRan), noMachine), next(toCluster, 0)},
		{"pool step, machines exhausted, no cloud", retryPoolStep, noCloud,
			numbered(notRetried(on("M", nothingRan)), noMachine), stop},
		{"pool step, the cluster already a place", retryPoolStep, standard,
			numbered(notRetried(on("M", nothingRan)), noMachine, onCluster(nothingRan)), stop},
		{"pool step, the cluster asked for a retry and was already a place", retryPoolStep, standard,
			numbered(notRetried(on("M", nothingRan)), noMachine, onCluster(retryRequested)), stop},
		// A pool step reaches the cluster once, whatever ended the attempt there
		// (the owner's cost bound). Its first attempt can end on the cluster inside
		// itself (the Fleet preference ran out): that was its one cluster attempt,
		// and its retries go to the machines it never tried, then stop.
		{"pool step, its first attempt fell back to the cluster: the machines, not the cluster", retryPoolStep, standard,
			numbered(onCluster(nothingRan)), next(failoverFrom(60), 0)},
		{"pool step, exit 75 on the cluster: the machines, not the cluster", retryPoolStep, standard,
			numbered(onCluster(retryRequested)), next(failoverFrom(60), 0)},
		{"pool step, its one cluster attempt spent and the machines exhausted", retryPoolStep, standard,
			numbered(onCluster(nothingRan), noMachine), stop},

		// Ruling 8: a pool step's failover to another machine waits at most
		// fleetWaitSeconds for a busy one, and at least a second (0 is no bound).
		{"pool step's failover waits fleetWaitSeconds", retryPoolStep, standard,
			numbered(notRetried(on("M", nothingRan))), next(failoverFrom(60, "M"), 0)},
		{"pool step's failover waits a second at least", retryPoolStep, noFleetWait,
			numbered(notRetried(on("M", nothingRan))), next(failoverFrom(1, "M"), 0)},
		{"pool step's pinned retry waits the largest delay", retryPoolStep, standard,
			numbered(on("M", nothingRan)), next(pinnedTo("M", 120), 30*time.Second)},

		// A cluster step's retries stay on the cluster, under sameMachine.
		{"cluster step, nothing ran once", retryClusterStep, standard,
			numbered(onCluster(nothingRan)), next(toCluster, 30*time.Second)},
		{"cluster step, nothing ran twice", retryClusterStep, standard,
			numbered(onCluster(nothingRan), onCluster(nothingRan)), next(toCluster, 120*time.Second)},
		{"cluster step, sameMachine spent", retryClusterStep, standard,
			numbered(onCluster(nothingRan), onCluster(nothingRan), onCluster(nothingRan)), stop},
		{"cluster step, retry requested", retryClusterStep, standard,
			numbered(onCluster(retryRequested)), next(toCluster, 30*time.Second)},

		// Exit 75 needs no repeatable: the command said another run is safe.
		{"retry requested on a step that is not repeatable", retryFleetStep, standard,
			numbered(on("M", retryRequested)), next(pinnedTo("M", 120), 30*time.Second)},

		// An unknown outcome is retried only for a repeatable step: another
		// machine first, then the same machine once, then the cluster for pool.
		{"outcome unknown, not repeatable", retryFleetStep, standard,
			numbered(on("M", unknownOutcome)), stop},
		{"outcome unknown, repeatable: another machine", repeatableStep(retryFleetStep), standard,
			numbered(on("M", unknownOutcome)), next(failoverFrom(0, "M"), 0)},
		{"outcome unknown, repeatable, pool: another machine within the Fleet wait", repeatableStep(retryPoolStep), standard,
			numbered(on("M", unknownOutcome)), next(failoverFrom(60, "M"), 0)},
		{"after the others: M once", repeatableStep(retryFleetStep), standard,
			numbered(on("M", unknownOutcome), noMachine), next(pinnedTo("M", 120), 0)},
		{"after the others and their own retries: M once", repeatableStep(retryFleetStep), standard,
			numbered(on("M", unknownOutcome), on("N", nothingRan), on("N", nothingRan), on("N", nothingRan), noMachine),
			next(pinnedTo("M", 120), 0)},
		{"no other machines: M once at once", repeatableStep(retryFleetStep), noOtherMachines,
			numbered(on("M", unknownOutcome)), next(pinnedTo("M", 120), 0)},
		{"then the cluster, for pool", repeatableStep(retryPoolStep), standard,
			numbered(on("M", unknownOutcome), noMachine, notRetried(on("M", nothingRan))), next(toCluster, 0)},
		{"then nothing, for fleet", repeatableStep(retryFleetStep), standard,
			numbered(on("M", unknownOutcome), noMachine, notRetried(on("M", nothingRan))), stop},
		{"M's one retry is spent though it was unknown again", repeatableStep(retryPoolStep), standard,
			numbered(on("M", unknownOutcome), noMachine, on("M", unknownOutcome)), next(toCluster, 0)},
		{"M's one retry asked for another: M under its own count", repeatableStep(retryFleetStep), standard,
			numbered(on("M", unknownOutcome), noMachine, on("M", retryRequested)), next(pinnedTo("M", 120), 120*time.Second)},
		{"a machine that needs recovering is not retried after its unknown outcome", repeatableStep(retryPoolStep), standard,
			numbered(notRetried(on("M", unknownOutcome)), noMachine), next(toCluster, 0)},
		// Ruling 18 (as amended): an unknown outcome on the cluster is retried
		// there, up to sameMachine with the same delays, for a repeatable step
		// whose placement IS the cluster -- it has no other place -- each only once
		// the previous attempt's Job is confirmed gone, so it cannot overlap
		// itself. Almost every cluster failure is an unknown outcome (rulings 14
		// and 15), so without this such a step would have no retries left.
		{"a repeatable cluster step's unknown outcome: the cluster again after 30 s", repeatableStep(retryClusterStep), standard,
			numbered(onCluster(unknownOutcome)), next(toCluster, 30*time.Second)},
		{"twice unknown on the cluster: again after 120 s", repeatableStep(retryClusterStep), standard,
			numbered(onCluster(unknownOutcome), onCluster(unknownOutcome)), next(toCluster, 120*time.Second)},
		{"three times unknown on the cluster: sameMachine spent", repeatableStep(retryClusterStep), standard,
			numbered(onCluster(unknownOutcome), onCluster(unknownOutcome), onCluster(unknownOutcome)), stop},
		{"the cluster's count is one count, whatever each attempt proved", repeatableStep(retryClusterStep), standard,
			numbered(onCluster(nothingRan), onCluster(unknownOutcome)), next(toCluster, 120*time.Second)},
		{"a cluster step's unknown outcome, not repeatable", retryClusterStep, standard,
			numbered(onCluster(unknownOutcome)), stop},
		{"the zero policy retries no unknown outcome on the cluster", repeatableStep(retryClusterStep), pipelines.ComputePlacementPolicy{},
			numbered(onCluster(unknownOutcome)), stop},
		// A repeatable pool step whose unknown outcome was on the cluster tries
		// the machines it has not tried, and then stops: it never goes back to
		// the cluster.
		{"a repeatable pool step's unknown outcome on the cluster: an untried machine", repeatableStep(retryPoolStep), standard,
			numbered(onCluster(unknownOutcome)), next(failoverFrom(60), 0)},
		{"then, the machines exhausted, nothing: never a second cluster attempt", repeatableStep(retryPoolStep), standard,
			numbered(onCluster(unknownOutcome), noMachine), stop},
		{"nor after an untried machine's own retries", repeatableStep(retryPoolStep), standard,
			numbered(onCluster(unknownOutcome), on("N", nothingRan), on("N", nothingRan), on("N", nothingRan), noMachine), stop},
		{"a machine owed its once is still paid", repeatableStep(retryPoolStep), standard,
			numbered(onCluster(unknownOutcome), on("M", unknownOutcome), noMachine), next(pinnedTo("M", 120), 0)},
		{"and then nothing, not the cluster", repeatableStep(retryPoolStep), standard,
			numbered(onCluster(unknownOutcome), on("M", unknownOutcome), noMachine, notRetried(on("M", nothingRan))), stop},
		{"no other machines: nothing, not the cluster", repeatableStep(retryPoolStep), noOtherMachines,
			numbered(onCluster(unknownOutcome)), stop},

		// The engine's cap: no step makes more than eight attempts.
		{"seven attempts: one more", retryFleetStep, standard,
			numbered(append(append(repeatRecord(on("m1", retryRequested), 3), repeatRecord(on("m2", retryRequested), 3)...),
				on("m3", retryRequested))...),
			next(pinnedTo("m3", 120), 30*time.Second)},
		{"eight attempts: no more", retryFleetStep, standard,
			numbered(append(append(repeatRecord(on("m1", retryRequested), 3), repeatRecord(on("m2", retryRequested), 3)...),
				repeatRecord(on("m3", retryRequested), 2)...)...),
			stop},

		// Only a class the executor set and this engine knows is retried.
		{"final", retryFleetStep, standard, numbered(on("M", pipelines.AttemptFinal)), stop},
		{"no class reads as final", retryFleetStep, standard, numbered(on("M", "")), stop},
		{"a class this engine does not know reads as final", retryFleetStep, standard, numbered(on("M", "try_harder")), stop},
		{"no history", retryFleetStep, standard, nil, stop},

		// A policy built by hand: no retries, and an empty delay list is no
		// delay and a one-second wait for a pinned machine, never a panic.
		{"the zero policy retries nothing", retryFleetStep, pipelines.ComputePlacementPolicy{},
			numbered(on("M", nothingRan)), stop},
		{"the zero policy retries no unknown outcome either", repeatableStep(retryFleetStep), pipelines.ComputePlacementPolicy{},
			numbered(on("M", unknownOutcome)), stop},
		{"a hand-built policy with no delays", retryFleetStep, handBuilt,
			numbered(on("M", nothingRan)), next(pinnedTo("M", 1), 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			placement, delay, ok := chooseNext(c.step, c.policy, c.history)
			if ok != c.want.ok || delay != c.want.delay || !reflect.DeepEqual(placement, c.want.placement) {
				t.Fatalf("chooseNext = %s, %s, %v; want %s, %s, %v",
					describe(placement), delay, ok, describe(c.want.placement), c.want.delay, c.want.ok)
			}
		})
	}
}

// chooseNext hands back a placement the caller owns: an executor that edits
// what it was handed cannot change what the next decision reads.
func TestChooseNextSharesNothingWithItsHistory(t *testing.T) {
	policy := pipelines.ComputePlacementPolicy{Retries: pipelines.DefaultRetryPolicy()}
	history := numbered(on("M", nothingRan), on("M", nothingRan), on("M", nothingRan))
	first, _, ok := chooseNext(retryFleetStep, policy, history)
	if !ok || len(first.ExcludeWorkers) != 1 {
		t.Fatalf("want a failover from M, got %s %v", describe(first), ok)
	}
	first.ExcludeWorkers[0] = "edited"
	again, _, _ := chooseNext(retryFleetStep, policy, history)
	if again.ExcludeWorkers[0] != "M" || history[0].WorkerID != "M" {
		t.Fatalf("an edit to one answer reached the next: %s", describe(again))
	}
}

// An attempt's record is what a replica that takes the run over reads back
// from the step's binding (section 3), through the parse the journal's engine
// gives it: JSON numbers, a list of objects, and the next attempt's time.
func TestAttemptRecordsRoundTripThroughTheBinding(t *testing.T) {
	records := []attemptRecord{
		{Attempt: 1, Class: nothingRan, Code: "pipeline_clone_failed", Message: "The fetch failed.", Surface: "fleet",
			WorkerID: "reg-msi", MachineName: "MSI Raider", NodeID: "agent-0", StartedAt: "2026-10-03T12:00:00Z", FinishedAt: "2026-10-03T12:00:05Z",
			LogFileID: "file-log-1", NotRetriedHere: true},
		{Attempt: 2, Surface: "cluster", NodeID: "workbench-0", JobName: "job-2"},
	}
	next := testNow.Add(30 * time.Second)
	binding := attemptsBinding(map[string]any{"surface": "cluster"}, records, next)
	if binding["surface"] != "cluster" {
		t.Errorf("the binding lost what it was built on: %v", binding)
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	got, gotNext := attemptsFromBinding(decoded)
	if !reflect.DeepEqual(got, records) || !gotNext.Equal(next) {
		t.Fatalf("read back %+v, next %s; want %+v, next %s", got, gotNext, records, next)
	}

	// The second record wrote no blank: a field nobody reported is left out.
	second := records[1].asMap()
	for _, key := range []string{"class", "code", "message", "workerId", "machineName", "notRetriedHere", "logFileId", "startedAt"} {
		if _, written := second[key]; written {
			t.Errorf("%s written blank: %v", key, second)
		}
	}

	// Without a next attempt, the binding names none, and an intent replaces
	// one a Retrying wrote (the binding is replaced whole).
	stale := map[string]any{bindingAttempts: []any{}, bindingNextAttemptAt: next.Format(time.RFC3339)}
	if b := attemptsBinding(stale, records, time.Time{}); b[bindingNextAttemptAt] != nil {
		t.Errorf("an intent kept a stale next attempt: %v", b)
	}
	if b := attemptsBinding(nil, nil, time.Time{}); b != nil {
		t.Errorf("a first attempt's intent must name no binding at all, as Step always wrote: %v", b)
	}
	if got, gotNext := attemptsFromBinding(nil); got != nil || !gotNext.IsZero() {
		t.Errorf("an empty binding read as %v %s", got, gotNext)
	}
}

// describe is a placement as a failure message reads it.
func describe(p *pipelines.AttemptPlacement) string {
	if p == nil {
		return "<none>"
	}
	var b strings.Builder
	b.WriteString("{" + p.Surface)
	if p.PinWorker != "" {
		b.WriteString(" pin " + p.PinWorker)
	}
	if len(p.ExcludeWorkers) > 0 {
		b.WriteString(" excluding " + strings.Join(p.ExcludeWorkers, ","))
	}
	if p.MaxWaitSeconds != 0 {
		b.WriteString(" waiting " + (time.Duration(p.MaxWaitSeconds) * time.Second).String())
	}
	return b.String() + "}"
}

// ---------------------------------------------------------------------------
// What the check run is told of a step's attempts
// ---------------------------------------------------------------------------

// The check run reads a step through StepState.Report: its history becomes the
// attempts the run says, each with where it ran in words. The place is all the
// record knows of it -- the machine's registration id, not its name, which only
// the OS's machines feed holds -- so a machine is its id's first eight
// characters and the cluster is the cluster.
func TestAStepReportCarriesItsAttempts(t *testing.T) {
	const machine, other = "3f9c2ab0-1d2e-4f3a-8b5c-6d7e8f9a0b1c", "7a1b2c3d-1d2e-4f3a-8b5c-6d7e8f9a0b1c"
	history := []attemptRecord{
		// The machine's display name, which its owner chose and the dispatcher
		// recorded with the id: the name is what the check run says.
		{Attempt: 1, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeCloneFailed, Surface: pipelines.PlacementFleet, WorkerID: machine, MachineName: "MSI Raider"},
		{Attempt: 2, Class: pipelines.AttemptOutcomeUnknown, Code: pipelines.CodeNodeLost, Surface: pipelines.PlacementCluster, JobName: "job-1"},
		// The fleet named no machine.
		{Attempt: 3, Class: pipelines.AttemptNothingRan, Surface: pipelines.PlacementFleet},
		// A machine no attempt of the step recorded a name for -- a row written before
		// names were recorded -- is called by the first eight characters of its id,
		// and only then; a canonical id is the same machine as its bare one.
		{Attempt: 4, Class: pipelines.AttemptRetryRequested, Surface: pipelines.PlacementFleet, WorkerID: "v1:worker:registration:" + other},
		// A name with only whitespace is no name.
		{Attempt: 5, Class: pipelines.AttemptRetryRequested, Surface: pipelines.PlacementFleet, WorkerID: other, MachineName: "  "},
		// Nothing recorded of where it ran, and the attempt that passed.
		{Attempt: 6},
	}
	got := StepState{Key: "build.mac", Stage: "build", Name: "mac", Status: StepSucceeded, Attempts: history}.Report()
	want := []pipelines.AttemptReport{
		{Attempt: 1, Code: pipelines.CodeCloneFailed, Where: "MSI Raider"},
		{Attempt: 2, Code: pipelines.CodeNodeLost, Where: "the cluster"},
		{Attempt: 3, Where: "your fleet"},
		{Attempt: 4, Where: "machine 7a1b2c3d"},
		{Attempt: 5, Where: "machine 7a1b2c3d"},
		{Attempt: 6},
	}
	if !reflect.DeepEqual(got.Attempts, want) {
		t.Errorf("attempts\n got %+v\nwant %+v", got.Attempts, want)
	}
	if got.Retrying || got.RetryAfter != 0 {
		t.Errorf("a step that passed is not retrying: %+v", got)
	}
	if rep := (StepState{Key: "a.b", Status: StepSucceeded}).Report(); rep.Attempts != nil {
		t.Errorf("a step that never reached the executor has no attempts: %+v", rep.Attempts)
	}
}

// A step reads as retrying only while it is running and the driver said another
// attempt follows: not before a receipt it is about to write (its last record is
// already in the history), and not after it.
func TestAStepReportSaysRetryingOnlyWhileItIs(t *testing.T) {
	history := []attemptRecord{{Attempt: 1, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeCloneFailed, Surface: pipelines.PlacementCluster}}
	for _, c := range []struct {
		name  string
		state StepState
		want  bool
	}{
		{"running and recorded as going again", StepState{Status: StepRunning, Attempts: history, Retrying: true, RetryAfter: 30 * time.Second}, true},
		{"running with a record and no word of another attempt", StepState{Status: StepRunning, Attempts: history}, false},
		{"settled", StepState{Status: StepFailed, Attempts: history, Retrying: true, RetryAfter: 30 * time.Second}, false},
		{"not yet begun", StepState{Status: StepPending, Attempts: history, Retrying: true}, false},
	} {
		got := c.state.Report()
		if got.Retrying != c.want {
			t.Errorf("%s: retrying %v, want %v", c.name, got.Retrying, c.want)
		}
		if c.want && got.RetryAfter != 30*time.Second {
			t.Errorf("%s: the scheduled wait is %s, want 30s", c.name, got.RetryAfter)
		}
		if !c.want && got.RetryAfter != 0 {
			t.Errorf("%s: a step that is not retrying carries a wait of %s", c.name, got.RetryAfter)
		}
	}
}

// The word that another attempt follows belongs to the outcome it was set for.
// runStep sets it once attempt N's outcome is durable and leaves it up while
// attempt N+1 runs (nothing publishes again when that attempt begins); the
// moment attempt N+1 ends, its record supersedes the outcome the word was about,
// and a progress another step publishes before the receipt or the next outcome
// is written must not still say attempt N+1 failed and attempt N+2 follows --
// least of all for a step that is about to pass. Driven through the track, which
// is what runStep calls, because the static Report test cannot see the order.
func TestARecordedAttemptEndsTheWordThatAnotherFollows(t *testing.T) {
	first := attemptRecord{Attempt: 1, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeCloneFailed, Surface: pipelines.PlacementCluster}
	second := attemptRecord{Attempt: 2, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeNodeLost, Surface: pipelines.PlacementCluster}
	track := &stepTrack{state: StepState{Key: "build.mac", Stage: "build", Name: "mac", Status: StepRunning}}
	summaryOf := func() string {
		_, summary, _ := pipelines.CheckOutput(pipelines.RunReport{
			Status: "in_progress", Steps: []pipelines.StepReport{track.snapshot().Report()},
		})
		return summary
	}

	track.setAttempts([]attemptRecord{first})
	track.setRetrying(30 * time.Second)
	if got := track.snapshot().Report(); !got.Retrying || got.RetryAfter != 30*time.Second {
		t.Fatalf("after attempt 1's outcome is durable the step is retrying in 30s: %+v", got)
	}
	if !strings.Contains(summaryOf(), "Retrying: attempt 1 failed") {
		t.Fatalf("the check run says so while attempt 2 runs:\n%s", summaryOf())
	}

	// Attempt 2 ended: its record arrives before its receipt (a step that passed)
	// or before the next outcome is durable (a step that goes again).
	track.setAttempts([]attemptRecord{first, second})
	if got := track.snapshot().Report(); got.Retrying || got.RetryAfter != 0 {
		t.Errorf("a step whose latest attempt has ended and whose next is not yet recorded is retrying: %+v", got)
	}
	if summary := summaryOf(); strings.Contains(summary, "Retrying") {
		t.Errorf("the check run still says the step is retrying:\n%s", summary)
	}

	// The next durable outcome says it again, about the attempt it is for.
	track.setRetrying(2 * time.Minute)
	got := track.snapshot().Report()
	if !got.Retrying || got.RetryAfter != 2*time.Minute || len(got.Attempts) != 2 {
		t.Errorf("after attempt 2's outcome is durable the step is retrying in 2m with both attempts: %+v", got)
	}
	if !strings.Contains(summaryOf(), "Retrying: attempt 2 failed") {
		t.Errorf("the check run names the attempt that just failed:\n%s", summaryOf())
	}
}

// A check run is read on GitHub, so the driver masks what it quotes with every
// secret it resolved: an attempt's text as the step's message and log are, even
// though each record was masked once when it was made (a value another step
// resolved later could not have been).
func TestTheCheckRunMasksAnAttemptsText(t *testing.T) {
	dr := &runDriver{}
	dr.remember("hunter2-token")
	dr.tracks = []*stepTrack{{state: StepState{Key: "a.b", Stage: "a", Name: "b", Status: StepSucceeded, Attempts: []attemptRecord{
		{Attempt: 1, Code: "failed-hunter2-token", Surface: pipelines.PlacementFleet, WorkerID: "hunter2-token-machine"},
		{Attempt: 2, Surface: pipelines.PlacementFleet, WorkerID: "m-2", MachineName: "box-hunter2-token"},
		{Attempt: 3},
	}}}}
	got := dr.report()[0].Attempts
	for _, a := range got {
		if strings.Contains(a.Code+a.Where, "hunter2-token") {
			t.Errorf("attempt %d reaches the check run with a resolved secret: %+v", a.Attempt, a)
		}
	}
	if got[0].Code != "failed-***" {
		t.Errorf("code %q, want the secret masked", got[0].Code)
	}
	if got[1].Where != "box-***" {
		t.Errorf("a machine's name %q, want the secret masked", got[1].Where)
	}
}

// One machine is not called two things in one list. An attempt that no machine
// answered is attributed to the machine it was pinned to, and carries no name of
// its own, so the report gives it the name another attempt of the step recorded
// for that machine; only a machine with no name anywhere in the step falls back
// to the start of its id.
func TestAMachineIsNotCalledTwoThingsInOneList(t *testing.T) {
	const raider, other = "3f9c2ab0-1d2e-4f3a-8b5c-6d7e8f9a0b1c", "7a1b2c3d-1d2e-4f3a-8b5c-6d7e8f9a0b1c"
	got := StepState{Key: "build.mac", Status: StepFailed, Attempts: []attemptRecord{
		{Attempt: 1, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeCloneFailed, Surface: pipelines.PlacementFleet, WorkerID: raider, MachineName: "MSI Raider"},
		{Attempt: 2, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeNoMachineForNeed, Surface: pipelines.PlacementFleet, WorkerID: "v1:worker:registration:" + raider, NotRetriedHere: true},
		{Attempt: 3, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeCloneFailed, Surface: pipelines.PlacementFleet, WorkerID: other},
	}}.Report().Attempts
	want := []pipelines.AttemptReport{
		{Attempt: 1, Code: pipelines.CodeCloneFailed, Where: "MSI Raider"},
		{Attempt: 2, Code: pipelines.CodeNoMachineForNeed, Where: "MSI Raider"},
		{Attempt: 3, Code: pipelines.CodeCloneFailed, Where: "machine 7a1b2c3d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attempts\n got %+v\nwant %+v", got, want)
	}
}

// The record keeps the name of the machine that ANSWERED, masked as every string
// of a record is. An attempt attributed to its pin because no machine answered
// has none to keep, and a timeout, an error or a cancel names no place at all.
func TestAnAttemptRecordKeepsTheNameOfTheMachineThatAnswered(t *testing.T) {
	dr := &runDriver{d: Deps{Now: func() time.Time { return testNow }}}
	dr.remember("hunter2-token")
	req := pipelines.StepRequest{
		Step:      retryFleetStep,
		Attempt:   2,
		Placement: pinnedTo("pinned-machine", 120),
	}
	answered := stepEnd{kind: endResult, result: pipelines.StepResult{
		Status: pipelines.OutcomeFailed, AttemptClass: pipelines.AttemptNothingRan,
		Where: pipelines.Where{Surface: pipelines.PlacementFleet, WorkerID: "m-1", MachineName: "box-hunter2-token"},
	}}
	got := dr.attemptOf(req, answered, failReceipt(pipelines.CodeCloneFailed, "The fetch failed."), testNow)
	if got.WorkerID != "m-1" || got.MachineName != "box-***" {
		t.Errorf("record names %q (%q); want the machine that answered, by id and by masked name", got.WorkerID, got.MachineName)
	}

	// No machine answered: the pin is credited, and it has no name here.
	unanswered := stepEnd{kind: endResult, result: pipelines.StepResult{
		Status: pipelines.OutcomeRefused, AttemptClass: pipelines.AttemptNothingRan, NotRetriedHere: true,
		Where: pipelines.Where{Surface: pipelines.PlacementFleet},
	}}
	got = dr.attemptOf(req, unanswered, failReceipt(pipelines.CodeNoMachineForNeed, "No machine."), testNow)
	if got.WorkerID != "pinned-machine" || got.MachineName != "" {
		t.Errorf("record names %q (%q); want the pinned machine and no name", got.WorkerID, got.MachineName)
	}

	// A timeout reports no answer to read a place from.
	got = dr.attemptOf(req, stepEnd{kind: endTimeout}, failReceipt(pipelines.CodeRunCeiling, "Timed out."), testNow)
	if got.MachineName != "" {
		t.Errorf("a timeout's record carries the name %q", got.MachineName)
	}
}
