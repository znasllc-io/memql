package campaigns

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// digest_sweep_test.go -- when the worker sweeps recipients that carry no address
// digest (memql#6022, F9). What the sweep does to the rows is tested where it
// lives, against a database: component/database/recipient_email_digest_db_test.go.

// idleEngine answers every read with no rows: a worker whose drain has nothing to do.
type idleEngine struct{}

func (idleEngine) Execute(context.Context, string) (any, error) {
	return map[string]any{"output": []any{}}, nil
}
func (idleEngine) OrganizationCapable(context.Context, string, string, string) bool { return false }

// logRecorder keeps what a worker logs, at every level, for a test to read.
type logRecorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *logRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *logRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *logRecorder) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// sweepingWorker is a started-able worker whose sweep is the one given, on a
// schedule short enough to watch.
func sweepingWorker(t *testing.T, first, every time.Duration, sweep RecipientDigestSweep) (*Worker, *logRecorder) {
	t.Helper()
	w := newTestWorker(t, idleEngine{}, &recordingSender{})
	rec := &logRecorder{}
	w.logger = rec.logger()
	w.cfg.Poll = time.Hour // the drain never ticks again within a test
	w.cfg.StartupDelay = 0
	w.digestSweepFirst, w.digestSweepEvery = first, every
	w.SetRecipientDigestSweep(sweep)
	return w, rec
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTheSweepRunsTenMinutesAfterStartAndThenHourly: the schedule is a decision
// about a rollout (see digest_sweep.go), so it is pinned.
func TestTheSweepRunsTenMinutesAfterStartAndThenHourly(t *testing.T) {
	if digestSweepFirst != 10*time.Minute || digestSweepEvery != time.Hour {
		t.Errorf("the sweep runs %s after start and every %s after that, want 10m and 1h: the first pass is meant to land after a rollout "+
			"has finished, and the hourly ones catch what a straggler leaves", digestSweepFirst, digestSweepEvery)
	}
	w := newTestWorker(t, idleEngine{}, &recordingSender{})
	if w.digestSweepFirst != 0 || w.digestSweepEvery != 0 {
		t.Errorf("a worker carries its own schedule %s / %s; zero means the defaults and only a test sets it", w.digestSweepFirst, w.digestSweepEvery)
	}
}

// TestTheSweepWaitsItsFirstDelayThenRepeatsAndStopsWithTheWorker.
func TestTheSweepWaitsItsFirstDelayThenRepeatsAndStopsWithTheWorker(t *testing.T) {
	var calls atomic.Int64
	w, _ := sweepingWorker(t, 400*time.Millisecond, 20*time.Millisecond, func(context.Context) (int, error) {
		calls.Add(1)
		return 0, nil
	})
	started := time.Now()
	w.Start(context.Background())
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Errorf("the sweep ran %d times in the first 100ms, before its first delay of 400ms", n)
	}
	eventually(t, "three sweeps", func() bool { return calls.Load() >= 3 })
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Errorf("the first sweep ran %s after start, before its delay", elapsed)
	}

	w.Stop(context.Background())
	after := calls.Load()
	time.Sleep(150 * time.Millisecond)
	if n := calls.Load(); n != after {
		t.Errorf("the sweep ran %d more times after Stop returned", n-after)
	}
}

// TestAFailedSweepIsWarnedAboutAndTheScheduleGoesOn: no read depends on the sweep
// having run, so a failure costs the next pass and nothing else.
func TestAFailedSweepIsWarnedAboutAndTheScheduleGoesOn(t *testing.T) {
	var calls atomic.Int64
	w, rec := sweepingWorker(t, time.Millisecond, 10*time.Millisecond, func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 3, fmt.Errorf("the batch after %q failed: connection reset", "v1:campaigns:recipient:x")
		}
		return 0, nil
	})
	w.Start(context.Background())
	defer w.Stop(context.Background())
	eventually(t, "a sweep after the failed one", func() bool { return calls.Load() >= 3 })

	out := rec.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "recipient digest sweep failed") || !strings.Contains(out, "connection reset") {
		t.Errorf("the failed sweep was not warned about with its error:\n%s", out)
	}
}

// TestAStampingSweepIsLoggedAndAnIdleOneIsNot: an hourly line that says nothing
// happened is how a log stops being read.
func TestAStampingSweepIsLoggedAndAnIdleOneIsNot(t *testing.T) {
	stamped := 0
	w, rec := sweepingWorker(t, time.Millisecond, time.Hour, func(context.Context) (int, error) { return stamped, nil })

	w.SweepRecipientDigests(context.Background())
	if out := rec.String(); strings.Contains(out, "level=INFO") || strings.Contains(out, "level=WARN") {
		t.Errorf("an idle sweep logged above debug:\n%s", out)
	}

	stamped = 12
	w.SweepRecipientDigests(context.Background())
	if out := rec.String(); !strings.Contains(out, "level=INFO") || !strings.Contains(out, "stamped=12") {
		t.Errorf("a sweep that stamped 12 recipients did not say so:\n%s", out)
	}
}

// TestAShutdownDuringASweepIsNotAFailure: the sweep returns the context's error
// when the node is going down, and that is not a warning.
func TestAShutdownDuringASweepIsNotAFailure(t *testing.T) {
	running := make(chan struct{})
	w, rec := sweepingWorker(t, time.Millisecond, time.Hour, func(ctx context.Context) (int, error) {
		close(running)
		<-ctx.Done()
		return 7, fmt.Errorf("recipient email digest sweep: stopped after 7 recipients stamped: %w", ctx.Err())
	})
	w.Start(context.Background())
	<-running
	w.Stop(context.Background())

	out := rec.String()
	for _, line := range strings.Split(out, "\n") {
		// The drain it ran beside is cancelled by the same shutdown and says so;
		// that is not this sweep's line.
		if strings.Contains(line, "digest sweep") && strings.Contains(line, "level=WARN") {
			t.Errorf("a shutdown was reported as a failure: %s", line)
		}
	}
	if !strings.Contains(out, "stopped by shutdown") {
		t.Errorf("a sweep cut short by shutdown did not say so:\n%s", out)
	}
}

// TestStopWaitsForASweepInFlight: a batch in flight runs to the end on a context
// of its own, so a node shutting down must not return from Stop (and have its
// database pool closed) under one.
func TestStopWaitsForASweepInFlight(t *testing.T) {
	inFlight, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	w, _ := sweepingWorker(t, time.Millisecond, time.Hour, func(context.Context) (int, error) {
		close(inFlight)
		<-release
		finished.Store(true)
		return 0, nil
	})
	w.Start(context.Background())
	<-inFlight

	stopped := make(chan struct{})
	go func() {
		w.Stop(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a sweep was still running")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return once the sweep finished")
	}
	if !finished.Load() {
		t.Error("Stop returned before the sweep finished")
	}
}

// TestAWorkerWithoutASweepStartsNoSweepGoroutine: the sweep is installed by the
// application, and a worker built without one (every test, a node whose wiring
// omits it) neither sweeps nor waits for one.
func TestAWorkerWithoutASweepStartsNoSweepGoroutine(t *testing.T) {
	w, _ := sweepingWorker(t, time.Millisecond, time.Millisecond, nil)
	w.Start(context.Background())
	w.mu.Lock()
	done := w.sweepDoneCh
	w.mu.Unlock()
	if done != nil {
		t.Error("a sweep goroutine was started although no sweep was installed")
	}
	w.SweepRecipientDigests(context.Background()) // a no-op, not a nil call
	w.Stop(context.Background())
}

// TestTheSweepRunsWhereverRecipientsCanBeWritten: signups and email rules write
// recipients -- digest-less ones, during a rollout -- on a node whether or not the
// campaign drain is on there, so the sweep that fills them in does not depend on
// MEMQL_CAMPAIGNS_ENABLED, nor on the drain's engine being wired.
func TestTheSweepRunsWhereverRecipientsCanBeWritten(t *testing.T) {
	for name, set := range map[string]func(w *Worker){
		"campaigns disabled": func(w *Worker) { w.cfg.Enabled = false },
		"no engine wired":    func(w *Worker) { w.store = NewStore(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			w, rec := sweepingWorker(t, time.Millisecond, 10*time.Millisecond, func(context.Context) (int, error) {
				calls.Add(1)
				return 0, nil
			})
			set(w)
			w.Start(context.Background())
			eventually(t, "a sweep", func() bool { return calls.Load() >= 2 })
			w.Stop(context.Background())

			after := calls.Load()
			time.Sleep(60 * time.Millisecond)
			if n := calls.Load(); n != after {
				t.Errorf("the sweep ran %d more times after Stop returned", n-after)
			}
			for _, line := range strings.Split(rec.String(), "\n") {
				if strings.Contains(line, "digest sweep") && strings.Contains(line, "level=WARN") {
					t.Errorf("the sweep was warned about: %s", line)
				}
			}
			if w.IsRunning() {
				t.Error("the worker reports it is running after Stop")
			}
		})
	}
}

// TestADisabledDrainDoesNotStartTheDrain: only the sweep runs on such a node. No
// drain tick reads the send jobs.
func TestADisabledDrainDoesNotStartTheDrain(t *testing.T) {
	var reads atomic.Int64
	engine := readCountingEngine{reads: &reads}
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.Enabled = false
	w.cfg.Poll = 5 * time.Millisecond
	w.cfg.StartupDelay = 0
	var sweeps atomic.Int64
	w.digestSweepFirst, w.digestSweepEvery = time.Millisecond, 10*time.Millisecond
	w.SetRecipientDigestSweep(func(context.Context) (int, error) { sweeps.Add(1); return 0, nil })
	w.Start(context.Background())
	eventually(t, "a sweep", func() bool { return sweeps.Load() >= 1 })
	time.Sleep(60 * time.Millisecond)
	w.Stop(context.Background())
	if n := reads.Load(); n != 0 {
		t.Errorf("a worker with campaigns disabled read %d times: its drain ran", n)
	}
}

// readCountingEngine counts every read it is asked.
type readCountingEngine struct{ reads *atomic.Int64 }

func (e readCountingEngine) Execute(context.Context, string) (any, error) {
	e.reads.Add(1)
	return map[string]any{"output": []any{}}, nil
}
func (readCountingEngine) OrganizationCapable(context.Context, string, string, string) bool {
	return false
}
