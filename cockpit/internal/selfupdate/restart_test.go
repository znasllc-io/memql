package selfupdate

import (
	"context"
	"errors"
	"testing"
)

func TestReservedRestartFinishesOnlyOldPIDThenStartsAndReconnects(t *testing.T) {
	f := setupFixture(t)
	before := f.worker
	f.worker.UpdatePending = true
	alive, running := true, true
	finishes, starts := 0, 0
	f.c.status = func(context.Context) (workerStatus, error) {
		if !running {
			return workerStatus{}, errors.New("stopped")
		}
		return f.worker, nil
	}
	f.c.alive = func(pid int) bool {
		if pid != before.PID {
			t.Fatalf("waited on another process: %d", pid)
		}
		return alive
	}
	f.c.finish = func(_ context.Context, pid int) error {
		finishes++
		if pid != before.PID {
			t.Fatal("finish targeted a replacement")
		}
		alive, running = false, false
		return nil
	}
	f.c.start = func(context.Context) error {
		starts++
		if alive {
			t.Fatal("started replacement before old worker exited")
		}
		running = true
		f.worker.PID++
		f.worker.Version = "1.1.0"
		f.worker.UpdatePending = false
		return nil
	}
	ready, err := f.c.restartReserved(t.Context(), before, "1.1.0")
	if err != nil || ready.PID == before.PID || finishes != 1 || starts != 1 {
		t.Fatalf("handoff: %+v %v finishes=%d starts=%d", ready, err, finishes, starts)
	}
	connected, err := f.c.waitConnected(t.Context(), ready)
	if have, total := connectionCounts(connected.Homes); err != nil || have != 1 || total != 1 {
		t.Fatalf("saved enabled connections did not resume: %+v %v", connected, err)
	}
	if connected.Homes[1].Enabled || connected.Homes[1].State != "Paused" {
		t.Fatal("handoff changed a paused home")
	}
}

func TestReservedRestartNeverStopsReplacementOrUnreservedWork(t *testing.T) {
	for _, kind := range []string{"already-updated-busy", "changed-pid", "changed-homes", "changed-path", "expired-lease", "busy", "missing-status"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t)
			before := f.worker
			f.worker.UpdatePending = true
			f.c.alive = func(int) bool { return true }
			f.c.finish = func(context.Context, int) error { t.Fatal("unsafe finish was attempted"); return nil }
			f.c.start = func(context.Context) error { t.Fatal("unsafe service start was attempted"); return nil }
			switch kind {
			case "already-updated-busy":
				f.worker.PID++
				f.worker.Version = "1.1.0"
				*f.worker.Busy = true
				f.worker.UpdatePending = false
			case "changed-pid":
				f.worker.PID++
			case "changed-homes":
				f.worker.Homes = nil
			case "changed-path":
				f.worker.Executable += "-other"
			case "expired-lease":
				f.worker.UpdatePending = false
			case "busy":
				*f.worker.Busy = true
			case "missing-status":
				f.c.status = func(context.Context) (workerStatus, error) { return workerStatus{}, errors.New("unavailable") }
			}
			ready, err := f.c.restartReserved(t.Context(), before, "1.1.0")
			if kind == "already-updated-busy" {
				if err != nil || ready.PID != f.worker.PID || !*ready.Busy {
					t.Fatalf("an already-ready busy replacement was not adopted: %+v %v", ready, err)
				}
			} else {
				var recovery *restartRecoveryError
				if !errors.As(err, &recovery) {
					t.Fatalf("unsafe handoff did not preserve installed recovery: %v", err)
				}
			}
		})
	}
}

func TestReservedRestartHandlesReplacementDuringFinishAndLostReply(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost reply after exit", true: "replacement rejected old pid"}[replaced], func(t *testing.T) {
			f := setupFixture(t)
			before := f.worker
			f.worker.UpdatePending = true
			running, alive, starts := true, true, 0
			f.c.status = func(context.Context) (workerStatus, error) {
				if !running {
					return workerStatus{}, errors.New("stopped")
				}
				return f.worker, nil
			}
			f.c.alive = func(int) bool { return alive }
			ready := func() {
				f.worker.PID++
				f.worker.Version = "1.1.0"
				f.worker.UpdatePending = false
				*f.worker.Busy = true // Never stop newly admitted replacement work.
			}
			f.c.finish = func(_ context.Context, pid int) error {
				if pid != before.PID {
					t.Fatal("finish targeted another process")
				}
				alive = false
				if replaced {
					ready()
				} else {
					running = false
				}
				return errors.New("reply lost or old pid rejected")
			}
			f.c.start = func(context.Context) error { starts++; running = true; ready(); return nil }
			got, err := f.c.restartReserved(t.Context(), before, "1.1.0")
			wantStarts := 1
			if replaced {
				wantStarts = 0
			}
			if err != nil || got.PID == before.PID || starts != wantStarts || !*got.Busy {
				t.Fatalf("uncertain finish killed or failed to recover replacement: %+v %v starts=%d", got, err, starts)
			}
		})
	}
}

func TestReservedRestartDoesNotForceAStuckOrUnverifiableWorker(t *testing.T) {
	for _, kind := range []string{"finish-refused", "still-exiting", "start-failed", "wrong-ready-homes"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t)
			before := f.worker
			f.worker.UpdatePending = true
			running, alive, starts := true, true, 0
			f.c.status = func(context.Context) (workerStatus, error) {
				if !running {
					return workerStatus{}, errors.New("stopped")
				}
				return f.worker, nil
			}
			f.c.alive = func(int) bool { return alive }
			f.c.finish = func(context.Context, int) error {
				if kind == "finish-refused" {
					return errors.New("lease expired")
				}
				if kind != "still-exiting" {
					running, alive = false, false
				}
				return nil
			}
			f.c.start = func(context.Context) error {
				starts++
				if kind == "start-failed" {
					return errors.New("service unavailable")
				}
				running = true
				f.worker.PID++
				f.worker.Version = "1.1.0"
				f.worker.Homes = nil
				return nil
			}
			_, err := f.c.restartReserved(t.Context(), before, "1.1.0")
			var recovery *restartRecoveryError
			if !errors.As(err, &recovery) || starts > 1 || (kind == "still-exiting" || kind == "finish-refused") && starts != 0 {
				t.Fatalf("uncertain handoff did not stop safely: %v starts=%d", err, starts)
			}
		})
	}
}
