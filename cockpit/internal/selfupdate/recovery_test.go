package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func interruptedFixture(t *testing.T) *fixture {
	t.Helper()
	f := setupFixture(t)
	if err := os.MkdirAll(f.c.state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.c.savePending("1.1.0", "interrupted-operation", f.worker.Homes); err != nil {
		t.Fatal(err)
	}
	// The replacement succeeded, but the process/service still runs the old
	// worker. A new invocation reads its build identity from the new executable.
	if err := os.WriteFile(f.c.Executable, f.payload, 0755); err != nil {
		t.Fatal(err)
	}
	f.c.Version = "1.1.0+grelease"
	f.c.catalog = "http://127.0.0.1:1/offline"
	return f
}

func TestInterruptedSwapOffersReadOnlyRecoveryThenRestartsWithoutDownload(t *testing.T) {
	f := interruptedFixture(t)
	before, _ := os.ReadFile(filepath.Join(f.c.state, "pending.json"))
	r := f.c.Check(context.Background())
	if !r.OK || !r.RestartRequired || !r.CanInstall || r.Available || r.LatestVersion != "1.1.0" {
		t.Fatalf("recovery was not offered while offline: %+v", r)
	}
	after, _ := os.ReadFile(filepath.Join(f.c.state, "pending.json"))
	if string(before) != string(after) || f.prepares != 0 || f.restarts != 0 {
		t.Fatal("background recovery check changed files or services")
	}
	r = f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || r.RestartRequired || !r.WorkerReconnected || f.prepares != 1 || f.restarts != 1 || f.progressChecked {
		t.Fatalf("recovery redownloaded or did not safely resume: %+v prepare=%d restart=%d", r, f.prepares, f.restarts)
	}
	if _, err := os.Stat(filepath.Join(f.c.state, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("completed recovery left an active journal")
	}
	if contents, _ := os.ReadFile(f.config); string(contents) != "keep identity, credentials, homes and paused policy exactly" || f.worker.Homes[1].Enabled {
		t.Fatal("recovery altered saved enrollment or paused connections")
	}
}

func TestRecoveryRefusesBusyChangedHomesAndUnverifiableRunningWorker(t *testing.T) {
	for _, kind := range []string{"busy", "changed-homes", "unavailable-running"} {
		t.Run(kind, func(t *testing.T) {
			f := interruptedFixture(t)
			switch kind {
			case "busy":
				*f.worker.Busy = true
			case "changed-homes":
				f.worker.Homes = append(f.worker.Homes, homeStatus{ID: "another", Enabled: true})
			case "unavailable-running":
				f.c.status = func(context.Context) (workerStatus, error) { return workerStatus{}, errors.New("socket unavailable") }
				run := f.c.run
				f.c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
					if strings.Contains(strings.Join(args, " "), "MainPID") {
						return []byte("123\n"), nil
					}
					return run(ctx, name, args...)
				}
			}
			r := f.c.Install(context.Background(), "1.1.0")
			if r.OK || r.Error == "" || f.prepares != 0 || f.restarts != 0 {
				t.Fatalf("unsafe recovery accepted: %+v", r)
			}
			if _, ok := f.c.pending(); !ok {
				t.Fatal("refusal discarded recovery information")
			}
		})
	}
}

func TestStoppedRecoveryUsesNonDestructiveStart(t *testing.T) {
	f := interruptedFixture(t)
	f.c.start = f.c.startService
	running, starts := false, 0
	f.c.status = func(context.Context) (workerStatus, error) {
		if !running {
			return workerStatus{}, errors.New("worker stopped")
		}
		return f.worker, nil
	}
	run := f.c.run
	f.c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "MainPID") {
			return []byte("0\n"), nil
		}
		if name == "systemctl" && joined == "--user start memql-worker.service" {
			starts++
			running = true
			f.worker.Version = "1.1.0+grelease"
			f.worker.PID++
			return nil, nil
		}
		return run(ctx, name, args...)
	}
	r := f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || starts != 1 || f.restarts != 0 || f.prepares != 0 {
		t.Fatalf("stopped recovery used destructive restart: %+v starts=%d restarts=%d", r, starts, f.restarts)
	}
}

func TestRecoveryJournalMustMatchInstalledTarget(t *testing.T) {
	f := interruptedFixture(t)
	f.c.Version = "1.0.0"
	if _, ok := f.c.pending(); ok {
		t.Fatal("pre-swap journal was treated as an installed update")
	}
	f.c.Version = "1.1.0"
	f.c.Variant = "headless"
	if _, ok := f.c.pending(); ok {
		t.Fatal("journal for another variant was accepted")
	}
}

func TestConnectionsNeverSucceedForReplacementProcessOrChangedHomes(t *testing.T) {
	for _, kind := range []string{"process", "version", "homes", "path", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t)
			initial := f.worker
			initial.Homes = append([]homeStatus(nil), f.worker.Homes...)
			initial.Homes[0].State = "Connecting"
			f.c.reconnectTimeout = 1500 * time.Millisecond
			f.c.status = func(context.Context) (workerStatus, error) {
				fresh := f.worker
				switch kind {
				case "process":
					fresh.PID++
				case "version":
					fresh.Version = "9.0.0"
				case "homes":
					fresh.Homes = nil
				case "path":
					fresh.Executable = "/different/memql"
				case "unavailable":
					return workerStatus{}, errors.New("socket unavailable")
				}
				return fresh, nil
			}
			got, err := f.c.waitConnected(context.Background(), initial)
			connected, _ := connectionCounts(got.Homes)
			if err == nil || connected != 0 || len(got.Homes) != len(initial.Homes) {
				t.Fatalf("unrelated/stale connection accepted: %+v, %v", got, err)
			}
		})
	}
}

func TestReadyRequiresTheInstalledWorkerExecutable(t *testing.T) {
	f := setupFixture(t)
	f.worker.Executable = filepath.Join(t.TempDir(), "other-memql")
	if _, err := f.c.waitReady(context.Background(), f.worker.Version, f.worker.Homes); err == nil {
		t.Fatal("another installation satisfied worker readiness")
	}
}
