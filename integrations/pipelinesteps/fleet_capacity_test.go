//go:build agent

package pipelinesteps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

func TestFleetConcurrentStepsShareOneBuildSlot(t *testing.T) {
	var mu sync.Mutex
	busy := false
	executions := map[string]int{}
	refusals := 0
	d := &fakeDispatcher{answer: func(ctx context.Context, req worker.Request) (worker.Result, error) {
		mu.Lock()
		if busy {
			refusals++
			mu.Unlock()
			return worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, nil
		}
		busy = true
		executions[req.StepId]++
		mu.Unlock()
		select {
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
		}
		mu.Lock()
		busy = false
		mu.Unlock()
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":30}`}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	results := make(chan pl.StepResult, 2)
	for _, key := range []string{"checks:first", "checks:second"} {
		go func(key string) {
			req := fleetReq()
			req.StepKey = key
			run := fleetRun(req)
			run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
			res, _ := f.RunStep(context.Background(), req, run)
			results <- res
		}(key)
	}
	for range 2 {
		select {
		case res := <-results:
			if res.Status != pl.OutcomeSucceeded {
				t.Fatalf("parallel step failed on occupied capacity: %+v", res)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("steps did not finish using one slot")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if refusals == 0 || executions["checks:first"] != 1 || executions["checks:second"] != 1 {
		t.Fatalf("executions=%v busy responses=%d", executions, refusals)
	}
}

func TestFleetWaitsForConfirmedCapacityWithoutSpendingCommandBudget(t *testing.T) {
	calls := 0
	var fleet *Fleet
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		calls++
		if calls == 1 {
			return worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, nil
		}
		if got := req.Args["timeoutSec"]; got != 900 {
			t.Fatalf("queue spent command budget: %v", got)
		}
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
	}}
	fleet, _, _, _ = newTestFleet(t, d)
	fleet.capacityRetry = time.Millisecond
	req := fleetReq()
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	res, err := fleet.RunStep(context.Background(), req, run)
	if err != nil || res.Status != pl.OutcomeSucceeded || calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", res, err, calls)
	}
}

func TestFleetDiskCapacityWaitKeepsActionableReasonWithoutRepeatingIt(t *testing.T) {
	calls := 0
	reason := "Build waiting for disk space: 512 MiB available; 20480 MiB required. Free space; no pipeline command started."
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		if calls <= 2 {
			return worker.Result{ErrorCode: "pipeline_capacity_busy", ErrorMessage: reason, RefusedBeforeStart: true}, nil
		}
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
	}}
	f, _, _, sink := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	result, err := f.RunStep(context.Background(), req, run)
	if err != nil || result.Status != pl.OutcomeSucceeded || calls != 3 {
		t.Fatalf("disk wait: %+v %v calls=%d", result, err, calls)
	}
	if got := strings.Count(strings.Join(sink.messages(), "\n"), reason); got != 1 {
		t.Fatalf("actionable reason reported %d times: %v", got, sink.messages())
	}
}

func TestFleetLocalBuildFenceWaitsForOperatorReconciliation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "local-build.json")
	body := []byte("fixture operator-owned record")
	if err := os.WriteFile(marker, body, 0600); err != nil {
		t.Fatal(err)
	}
	reason := "A retained local build needs reconciliation; inspect its record before clearing it."
	fenced := make(chan struct{}, 8)
	var executions atomic.Int32
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		if _, err := os.Lstat(marker); err == nil {
			select {
			case fenced <- struct{}{}:
			default:
			}
			return worker.Result{ErrorCode: "pipeline_capacity_fenced", ErrorMessage: reason, RefusedBeforeStart: true}, nil
		} else if !os.IsNotExist(err) {
			return worker.Result{}, err
		}
		executions.Add(1)
		if req.Args["timeoutSec"] != 900 {
			return worker.Result{}, errors.New("waiting spent the command budget")
		}
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
	}}
	f, _, _, sink := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan pl.StepResult, 1)
	go func() {
		result, _ := f.RunStep(ctx, req, run)
		done <- result
	}()
	for range 2 {
		select {
		case <-fenced:
		case result := <-done:
			t.Fatalf("fence did not keep admission waiting: %+v", result)
		case <-ctx.Done():
			t.Fatal("fenced admission did not return")
		}
	}
	if executions.Load() != 0 {
		t.Fatal("command started while the operator-owned marker remained")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(body) {
		t.Fatalf("waiter changed the operator record: %q %v", got, err)
	}
	// Fixture-only external operator action, never an effect of the waiter.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Status != pl.OutcomeSucceeded || executions.Load() != 1 {
			t.Fatalf("reconciled admission: %+v executions=%d", result, executions.Load())
		}
	case <-ctx.Done():
		t.Fatal("reconciled admission did not finish")
	}
	if got := strings.Count(strings.Join(sink.messages(), "\n"), reason); got != 1 {
		t.Fatalf("recovery reason reported %d times: %v", got, sink.messages())
	}
}

func TestFleetLocalBuildFenceRechecksConsentBeforeAnotherDispatch(t *testing.T) {
	calls := 0
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		if calls == 1 {
			return worker.Result{ErrorCode: "pipeline_capacity_fenced", RefusedBeforeStart: true}, nil
		}
		return worker.Result{ErrorCode: "denied_by_policy", ErrorMessage: "repository consent was withdrawn", RefusedBeforeStart: true}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	result, err := f.RunStep(context.Background(), req, run)
	if err != nil || result.Status != pl.OutcomeRefused || calls != 2 || result.Failure == nil || !strings.Contains(result.Failure.Message, "consent was withdrawn") {
		t.Fatalf("withdrawal did not stop fenced admission: %+v %v calls=%d", result, err, calls)
	}
}

func TestFleetDiskCapacityWaitBoundsLiveMeasurementsAndRetainsArchive(t *testing.T) {
	calls := 0
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		if calls <= 6 {
			return worker.Result{ErrorCode: "pipeline_capacity_busy", ErrorMessage: fmt.Sprintf("Build waiting for disk space: %d MiB available", calls), RefusedBeforeStart: true}, nil
		}
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
	}}
	f, lib, _, sink := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	result, err := f.RunStep(context.Background(), req, run)
	if err != nil || result.Status != pl.OutcomeSucceeded || calls != 7 {
		t.Fatalf("disk wait: %+v %v calls=%d", result, err, calls)
	}
	if got := strings.Count(strings.Join(sink.messages(), "\n"), "Build waiting for disk space:"); got != 4 {
		t.Fatalf("live capacity messages=%d, want bounded four: %v", got, sink.messages())
	}
	log, ok := lib.named(logFileName(req.StepKey))
	if !ok || strings.Count(string(log.Bytes), "Build waiting for disk space:") != 6 {
		t.Fatalf("capacity changes missing from archive: %q", log.Bytes)
	}
}

func TestFleetNeverQueuesUncertainOrPolicyRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, code   string
		before, gate bool
		err          error
	}{
		{"uncertain busy", "pipeline_capacity_busy", false, false, nil},
		{"lost busy reply", "pipeline_capacity_busy", true, false, errors.New("transport lost")},
		{"uncertain fence", "pipeline_capacity_fenced", false, false, nil},
		{"lost fence reply", "pipeline_capacity_fenced", true, false, errors.New("transport lost")},
		{"fence engine gate", "pipeline_capacity_fenced", true, true, nil},
		{"capacity inspection unavailable", "pipeline_capacity_unavailable", true, false, nil},
		{"policy", "denied_by_policy", true, false, nil},
		{"engine gate", "pipeline_capacity_busy", true, true, nil},
		{"disconnected", "worker_disconnected", false, false, nil},
		{"recovery", "pipeline_recovery_required", false, false, nil},
		{"cleanup", "pipeline_cleanup_uncertain", false, false, nil},
		{"offline", "no_worker_available", true, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				calls++
				return worker.Result{ErrorCode: tc.code, RefusedBeforeStart: tc.before, RefusedByGate: tc.gate}, tc.err
			}}
			f, _, _, _ := newTestFleet(t, d)
			f.capacityRetry = time.Millisecond
			req := fleetReq()
			run := fleetRun(req)
			run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
			res, _ := f.RunStep(context.Background(), req, run)
			if calls != 1 || res.Status == pl.OutcomeSucceeded {
				t.Fatalf("replayed uncertain step: calls=%d result=%+v", calls, res)
			}
		})
	}
}

func TestFleetCapacityWaitStopsOnCancellationAndRunCeiling(t *testing.T) {
	for _, code := range []string{"pipeline_capacity_busy", "pipeline_capacity_fenced"} {
		for _, mode := range []string{"cancel", "ceiling", "missing-ceiling"} {
			t.Run(code+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				now := exNow
				calls := 0
				d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
					calls++
					if mode == "cancel" {
						cancel()
					} else if mode == "ceiling" {
						now = now.Add(time.Hour)
					}
					return worker.Result{ErrorCode: code, RefusedBeforeStart: true}, nil
				}}
				f, _, _, _ := newTestFleet(t, d)
				f.now = func() time.Time { return now }
				f.capacityRetry = time.Millisecond
				req := fleetReq()
				run := fleetRun(req)
				if mode != "missing-ceiling" {
					run.RunDeadline = exNow.Add(time.Minute).Format(time.RFC3339)
				}
				res, _ := f.RunStep(ctx, req, run)
				if calls != 1 {
					t.Fatalf("dispatched again after %s: %d", mode, calls)
				}
				if mode == "cancel" {
					if res.Status != pl.OutcomeCancelled {
						t.Fatalf("cancel: %+v", res)
					}
				} else {
					want := pl.CodeRunCeiling
					if mode == "missing-ceiling" {
						want = pl.CodeExecutorError
					}
					if res.Failure == nil || res.Failure.Code != want {
						t.Fatalf("%s: %+v", mode, res)
					}
				}
			})
		}
	}
}

func TestFleetQueuedTokenRefreshIsMaskedAndTimeoutShrinksAtCeiling(t *testing.T) {
	now := exNow
	calls := 0
	var tokens *fakeTokens
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		calls++
		if calls == 1 {
			now = now.Add(31 * time.Minute)
			tokens.token = "refreshed-installation-token"
			return worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, nil
		}
		if req.Args["token"] != "refreshed-installation-token" || req.Args["timeoutSec"] != 60 {
			t.Fatalf("stale queued dispatch: timeout=%v", req.Args["timeoutSec"])
		}
		req.OnStreamChunk(chunk("stdout", "refreshed-installation-token\n"))
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
	}}
	f, lib, minted, _ := newTestFleet(t, d)
	tokens = minted
	f.now = func() time.Time { return now }
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(32 * time.Minute).Format(time.RFC3339)
	res, err := f.RunStep(context.Background(), req, run)
	if err != nil || res.Status != pl.OutcomeSucceeded || calls != 2 {
		t.Fatalf("result: %+v %v calls=%d", res, err, calls)
	}
	for _, file := range lib.files {
		if strings.Contains(string(file.Bytes), "refreshed-installation-token") {
			t.Fatal("refreshed credential reached archived log")
		}
	}
	if len(tokens.asked) != 2 {
		t.Fatalf("token mints=%d", len(tokens.asked))
	}
}

func TestFleetWaitsThroughReconnectAndRechecksConsent(t *testing.T) {
	for _, mode := range []string{"reconnect", "policy-withdrawn", "cancel", "ceiling", "missing-ceiling", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, executions := 0, 0
			now := exNow
			d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
				calls++
				if calls == 1 {
					return worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, nil
				}
				if calls == 2 {
					if mode == "cancel" {
						cancel()
					}
					if mode == "ceiling" {
						now = now.Add(time.Hour)
					}
					return worker.Result{ErrorCode: "no_worker_available", RefusedBeforeStart: mode != "uncertain", WaitForConnection: true}, nil
				}
				if mode == "policy-withdrawn" {
					return worker.Result{ErrorCode: "no_worker_available", RefusedBeforeStart: true}, nil
				}
				executions++
				if req.Args["timeoutSec"] != 900 {
					t.Fatal("queue consumed command timeout")
				}
				return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
			}}
			f, _, _, _ := newTestFleet(t, d)
			f.now = func() time.Time { return now }
			f.capacityRetry = time.Millisecond
			req := fleetReq()
			run := fleetRun(req)
			if mode != "missing-ceiling" {
				run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
			}
			res, err := f.RunStep(ctx, req, run)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "reconnect" {
				if calls != 3 || executions != 1 || res.Status != pl.OutcomeSucceeded {
					t.Fatalf("result=%+v calls=%d executions=%d", res, calls, executions)
				}
			} else if executions != 0 || res.Status == pl.OutcomeSucceeded || calls > 3 {
				t.Fatalf("unexpected execution: result=%+v calls=%d executions=%d", res, calls, executions)
			}
			if mode == "cancel" && res.Status != pl.OutcomeCancelled {
				t.Fatalf("cancel: %+v", res)
			}
			if mode == "ceiling" && (res.Failure == nil || res.Failure.Code != pl.CodeRunCeiling) {
				t.Fatalf("ceiling: %+v", res)
			}
		})
	}
}

func TestFleetWaitsForPreDispatchConnectionLossOnly(t *testing.T) {
	for _, code := range []string{"worker_disconnected", "worker_unreachable"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				calls++
				if calls == 1 {
					return worker.Result{ErrorCode: code, RefusedBeforeStart: true}, nil
				}
				return worker.Result{OK: true, OutputJSON: `{"exitCode":0}`}, nil
			}}
			f, _, _, _ := newTestFleet(t, d)
			f.capacityRetry = time.Millisecond
			req := fleetReq()
			run := fleetRun(req)
			run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
			res, err := f.RunStep(context.Background(), req, run)
			if err != nil || calls != 2 || res.Status != pl.OutcomeSucceeded {
				t.Fatalf("result=%+v calls=%d err=%v", res, calls, err)
			}
		})
	}
}
