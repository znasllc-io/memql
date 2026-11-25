//go:build agent

package pipelinesteps

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// Network-aware fleet selection, the executor's half (design record
// docs/superpowers/specs/2026-11-24-network-aware-fleet-selection-design.md,
// sections 5 and 6): the run's window reaches the dispatcher, a fleet step
// (never a pool step) waits for a steady machine, a bounded wait ends on the
// free machine (D11), and the step's log and failure say why.

var lastDropAt = time.Date(2026, 11, 24, 19, 44, 56, 0, time.UTC)

func unsteadyHealth() *worker.ConnectionHealth {
	return &worker.ConnectionHealth{Drops: 2, LastDropAt: lastDropAt, Window: 2 * time.Hour}
}

func withConnectionWindow(req pl.StepRequest, minutes int) pl.StepRequest {
	policy := pl.ComputePlacementPolicy{Retries: pl.DefaultRetryPolicy()}
	if req.ComputePlacement != nil {
		policy = req.ComputePlacement.Clone()
	}
	policy.Connection = pl.ConnectionPolicy{UnsteadyMinutes: minutes}
	req.ComputePlacement = &policy
	return req
}

func okAnswer() worker.Result {
	return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-b", MachineName: "Box B"}
}

func TestTheConnectionWindowReachesTheRequest(t *testing.T) {
	poolStep := func() pl.StepRequest {
		req := exRequest()
		req.Step.Execution, req.Step.Needs, req.Step.Placement = pl.ExecutionContainer, nil, pl.PlacementPool
		req.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 30, CloudFallback: true}
		return req
	}
	for _, tc := range []struct {
		name string
		req  pl.StepRequest
		want worker.ConnectionPreference
	}{
		{"a fleet step waits for a steady machine", withConnectionWindow(fleetReq(), 120), worker.ConnectionPreference{Window: 2 * time.Hour, WaitForSteady: true}},
		{"a pool step only orders (D10)", withConnectionWindow(poolStep(), 120), worker.ConnectionPreference{Window: 2 * time.Hour}},
		{"a window of zero turns it off", withConnectionWindow(fleetReq(), 0), worker.ConnectionPreference{}},
		{"no policy asks for nothing", fleetReq(), worker.ConnectionPreference{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) { return okAnswer(), nil }}
			f, _, _, _ := newTestFleet(t, d)
			run := fleetRun(tc.req)
			run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
			if _, err := f.RunStep(context.Background(), tc.req, run); err != nil {
				t.Fatal(err)
			}
			if len(d.reqs) == 0 || d.reqs[0].Connection != tc.want {
				t.Fatalf("dispatched %+v, want connection %+v", d.reqs, tc.want)
			}
		})
	}
}

func busyPassingOver(name string) worker.Result {
	return worker.Result{ErrorCode: "pipeline_capacity_busy", ErrorMessage: "all slots are taken", RefusedBeforeStart: true,
		WorkerId: "reg-a", MachineName: "Box A",
		PassedOver: []worker.PassedOver{{WorkerId: "reg-b0123456789", MachineName: name, Health: *unsteadyHealth()}}}
}

func TestTheWaitNamesThePassedOverMachineOnce(t *testing.T) {
	calls := 0
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		if calls <= 3 {
			return busyPassingOver("Box B"), nil
		}
		return okAnswer(), nil
	}}
	f, _, _, sink := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	req := withConnectionWindow(fleetReq(), 120)
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	res, err := f.RunStep(context.Background(), req, run)
	if err != nil || res.Status != pl.OutcomeSucceeded || calls != 4 {
		t.Fatalf("result = %+v err = %v calls = %d", res, err, calls)
	}
	// "Passed over", not "free": the machine was not asked, so nothing says it
	// is free.
	want := "memql: waiting for Box A, whose connection is steady; passed over Box B, whose connection dropped 2 times in the last 2 hours (last at 19:44 UTC)"
	if got := strings.Count(strings.Join(sink.messages(), "\n"), want); got != 1 {
		t.Fatalf("the waiting notice appears %d times, want once: %q", got, sink.messages())
	}
}

func TestABoundedWaitEndsOnTheFreeMachine(t *testing.T) {
	var mu sync.Mutex
	now := exNow
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		if req.Connection.WaitForSteady {
			return busyPassingOver("Box B"), nil
		}
		return okAnswer(), nil
	}}
	f, _, _, sink := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	f.now = func() time.Time { mu.Lock(); defer mu.Unlock(); now = now.Add(10 * time.Second); return now }
	req := withConnectionWindow(fleetReq(), 120)
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	run.MaxWaitSeconds = 30 // an attempt the retry driver placed, with its own bound
	res, err := f.RunStep(context.Background(), req, run)
	if err != nil || res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v err = %v, want the step run on the free machine when the bounded wait ran out", res, err)
	}
	last := d.reqs[len(d.reqs)-1]
	if last.Connection.WaitForSteady || last.Connection.Window != 2*time.Hour {
		t.Fatalf("the last dispatch asked %+v, want the window kept and no wait for a steady machine", last.Connection)
	}
	if want := "memql: the wait for a steady machine ran out; trying the machines it passed over"; !strings.Contains(strings.Join(sink.messages(), "\n"), want) {
		t.Fatalf("notices = %q, want %q", sink.messages(), want)
	}
}

func TestAStepOnAnUnsteadyMachineSaysSo(t *testing.T) {
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		res := okAnswer()
		res.Connection = unsteadyHealth()
		return res, nil
	}}
	f, _, _, sink := newTestFleet(t, d)
	req := withConnectionWindow(fleetReq(), 120)
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	if res, err := f.RunStep(context.Background(), req, run); err != nil || res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v err = %v", res, err)
	}
	want := "memql: the step was placed on Box B, whose connection dropped 2 times in the last 2 hours (last at 19:44 UTC); no machine with a steady connection could take it"
	if !strings.Contains(strings.Join(sink.messages(), "\n"), want) {
		t.Fatalf("notices = %q, want %q", sink.messages(), want)
	}

	// A retry pinned to its machine (D8) had no other machine to try, steady or
	// not: the notice says so instead of claiming none could take it.
	f, _, _, sink = newTestFleet(t, d)
	run.PinWorker = "reg-b"
	if res, err := f.RunStep(context.Background(), req, run); err != nil || res.Status != pl.OutcomeSucceeded {
		t.Fatalf("pinned result = %+v err = %v", res, err)
	}
	pinned := "memql: the step was placed on Box B, whose connection dropped 2 times in the last 2 hours (last at 19:44 UTC); this retry was pinned to it"
	if all := strings.Join(sink.messages(), "\n"); !strings.Contains(all, pinned) || strings.Contains(all, "could take it") {
		t.Fatalf("pinned notices = %q, want %q", sink.messages(), pinned)
	}
}

// When the machine's output says where the step started (Request.OnStarted),
// the notice is written then: once, above the step's own lines, where a person
// watching the step live reads it first. TestAStepOnAnUnsteadyMachineSaysSo is
// the step that wrote nothing until it ended, whose notice comes after.
func TestAStepOnAnUnsteadyMachineSaysSoBeforeItsOutput(t *testing.T) {
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		if req.OnStarted == nil || req.OnStreamChunk == nil {
			return worker.Result{}, nil
		}
		req.OnStarted(worker.AttemptStarted{WorkerId: "reg-b", MachineName: "Box B", Connection: unsteadyHealth()})
		req.OnStreamChunk(&nodev1.WorkerForwardStream{Payload: &nodev1.WorkerForwardStream_StdoutChunk{StdoutChunk: []byte("cloning the repository\n")}})
		res := okAnswer()
		res.Connection = unsteadyHealth()
		return res, nil
	}}
	f, _, _, sink := newTestFleet(t, d)
	req := withConnectionWindow(fleetReq(), 120)
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	if res, err := f.RunStep(context.Background(), req, run); err != nil || res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v err = %v", res, err)
	}
	all := strings.Join(sink.messages(), "\n")
	notice := "memql: the step was placed on Box B, whose connection dropped 2 times in the last 2 hours (last at 19:44 UTC); no machine with a steady connection could take it"
	at, output := strings.Index(all, notice), strings.Index(all, "cloning the repository")
	if at < 0 || output < 0 || at > output || strings.Count(all, notice) != 1 {
		t.Fatalf("messages = %q, want the notice once, above the step's output", sink.messages())
	}
}

func TestANodeLossOnAnUnsteadyMachineSaysItsConnectionHadDropped(t *testing.T) {
	lost := func(health *worker.ConnectionHealth) string {
		t.Helper()
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			return worker.Result{ErrorCode: "worker_disconnected", ErrorMessage: "stream lost", WorkerId: "reg-b", MachineName: "Box B", Connection: health}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		req := withConnectionWindow(fleetReq(), 120)
		run := fleetRun(req)
		run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
		res, err := f.RunStep(context.Background(), req, run)
		if err != nil || res.Failure == nil || res.Failure.Code != pl.CodeNodeLost {
			t.Fatalf("result = %+v err = %v, want pipeline_node_lost", res, err)
		}
		return res.Failure.Message
	}
	plain := lost(nil)
	if steady := lost(&worker.ConnectionHealth{Window: 2 * time.Hour}); steady != plain {
		t.Fatalf("a steady machine's node loss reads %q, want today's %q", steady, plain)
	}
	got := lost(unsteadyHealth())
	if !strings.HasSuffix(got, "Its connection had dropped 2 times in the 2 hours before the step started.") || !strings.HasPrefix(got, plain) {
		t.Fatalf("an unsteady machine's node loss reads %q, want today's sentence and the connection's", got)
	}
}

// The words for a count: a read that hit the query's page of 100 is a floor,
// and says so, and one drop is one time, in a window said as a person says it.
func TestDropCountsAreWordedAsTheyWereRead(t *testing.T) {
	capped := worker.ConnectionHealth{Drops: 100, Capped: true, LastDropAt: lastDropAt, Window: 2 * time.Hour}
	if got, want := droppedWords(capped), "dropped 100 or more times in the last 2 hours (last at 19:44 UTC)"; got != want {
		t.Fatalf("capped = %q, want %q", got, want)
	}
	if got, want := nodeLossConnectionSentence(capped), " Its connection had dropped 100 or more times in the 2 hours before the step started."; got != want {
		t.Fatalf("capped node loss = %q, want %q", got, want)
	}
	one := worker.ConnectionHealth{Drops: 1, LastDropAt: lastDropAt, Window: 90 * time.Minute}
	if got, want := droppedWords(one), "dropped 1 time in the last 90 minutes (last at 19:44 UTC)"; got != want {
		t.Fatalf("one = %q, want %q", got, want)
	}
	for window, want := range map[time.Duration]string{time.Hour: "1 hour", time.Minute: "1 minute", 24 * time.Hour: "24 hours"} {
		if got := windowWords(window); got != want {
			t.Fatalf("windowWords(%s) = %q, want %q", window, got, want)
		}
	}
}

func TestUnsteadyNoticesNameAnUnnamedMachineByItsId(t *testing.T) {
	calls := 0
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		if calls == 1 {
			busy := busyPassingOver("")
			busy.MachineName = ""
			return busy, nil
		}
		res := okAnswer()
		res.MachineName = ""
		res.Connection = unsteadyHealth()
		return res, nil
	}}
	f, _, _, sink := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	req := withConnectionWindow(fleetReq(), 120)
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	if res, err := f.RunStep(context.Background(), req, run); err != nil || res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v err = %v", res, err)
	}
	all := strings.Join(sink.messages(), "\n")
	for _, want := range []string{pl.MachineWords("", "reg-a"), pl.MachineWords("", "reg-b0123456789"), pl.MachineWords("", "reg-b")} {
		if !strings.Contains(all, want) {
			t.Fatalf("notices = %q, want the unnamed machine called %q", sink.messages(), want)
		}
	}
	if strings.Contains(all, "Box ") {
		t.Fatalf("notices name a machine its owner did not name: %q", all)
	}
}
