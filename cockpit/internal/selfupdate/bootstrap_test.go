package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bootstrapFixture(t *testing.T) (*fixture, *int) {
	t.Helper()
	f := setupFixture(t)
	if err := os.WriteFile(f.c.Executable, []byte("memql "+f.c.Version+" (computeruse)\n"), 0755); err != nil {
		t.Fatal(err)
	}
	f.c.bootstrap = &bootstrapOptions{release: "1.1.0", installed: f.c.Version, confirmation: "restart-worker"}
	f.worker.Busy = nil
	f.worker.PID = 2147483647 // An absent PID for journal recovery; never signal a real service.
	f.c.legacyPID = f.worker.PID
	stops := 0
	f.c.stop = func(context.Context, int) error {
		stops++
		body, _ := os.ReadFile(f.c.Executable)
		if string(body) != "memql 1.0.0+gabc (computeruse)\n" {
			t.Fatal("legacy worker stopped after replacement")
		}
		if record, ok := f.c.pendingRecord(); !ok || record.PreviousVersion != "1.0.0" || record.Version != "1.1.0" || !sameHomes(record.Homes, f.worker.Homes) {
			t.Fatalf("missing pre-stop recovery journal: %+v %v", record, ok)
		}
		return nil
	}
	return f, &stops
}

func TestBootstrapLegacyPreservesEnrollmentAndStopsBeforeReplacement(t *testing.T) {
	f, stops := bootstrapFixture(t)
	before, _ := os.ReadFile(f.config)
	r := f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || !r.WorkerReconnected || *stops != 1 || f.prepares != 0 || f.restarts != 1 || f.cancels != 0 {
		t.Fatalf("result %+v stop/prepare/restart/cancel %d/%d/%d/%d", r, *stops, f.prepares, f.restarts, f.cancels)
	}
	after, _ := os.ReadFile(f.config)
	if !bytes.Equal(before, after) || f.worker.Homes[1].Enabled || f.worker.Homes[1].State != "Paused" {
		t.Fatal("enrollment or paused connection changed")
	}
}

func TestBootstrapRefusesWrongTargetVariantWorkAndMissingConfirmation(t *testing.T) {
	for _, kind := range []string{"confirmation", "target", "disk-version", "variant", "worker-version", "busy", "stop-refused"} {
		t.Run(kind, func(t *testing.T) {
			f, stops := bootstrapFixture(t)
			request := "1.1.0"
			switch kind {
			case "confirmation":
				f.c.bootstrap.confirmation = ""
			case "target":
				request = "1.2.0"
			case "disk-version":
				f.c.bootstrap.installed = "1.0.0"
			case "variant":
				f.c.Variant = "headless"
			case "worker-version":
				f.worker.Version = "0.9.0"
			case "busy":
				busy := true
				f.worker.Busy = &busy
			case "stop-refused":
				f.c.stop = func(context.Context, int) error { return errors.New("service PID changed") }
			}
			before, _ := os.ReadFile(f.c.Executable)
			r := f.c.Install(context.Background(), request)
			after, _ := os.ReadFile(f.c.Executable)
			if r.OK || r.Updated || r.Error == "" || *stops != 0 || f.restarts != 0 || !bytes.Equal(before, after) {
				t.Fatalf("refusal changed installation or restarted worker: %+v stops=%d restarts=%d", r, *stops, f.restarts)
			}
		})
	}
}

func TestBootstrapCapableWorkerStillReservesWithoutLegacyStop(t *testing.T) {
	f, stops := bootstrapFixture(t)
	busy := false
	f.worker.Busy = &busy
	f.c.bootstrap.confirmation = ""
	r := f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || *stops != 0 || f.prepares != 1 {
		t.Fatalf("%+v stops %d prepares %d", r, *stops, f.prepares)
	}
}

func TestBootstrapRecoversInterruptedStopBeforeSwap(t *testing.T) {
	f, stops := bootstrapFixture(t)
	if err := os.MkdirAll(f.c.state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.c.savePending("1.1.0", "interrupted", f.worker.Homes); err != nil {
		t.Fatal(err)
	}
	stopped := true
	starts := 0
	f.c.status = func(context.Context) (workerStatus, error) {
		if stopped {
			return workerStatus{}, errors.New("worker stopped")
		}
		return f.worker, nil
	}
	originalRun := f.c.run
	f.c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && strings.Contains(strings.Join(args, " "), "--property=MainPID") {
			return []byte("0"), nil
		}
		if name == "systemctl" && len(args) > 1 && args[1] == "start" {
			starts++
			stopped = false
			return nil, nil
		}
		return originalRun(ctx, name, args...)
	}
	r := f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || starts != 1 || *stops != 1 || f.restarts != 1 {
		t.Fatalf("%+v starts %d stops %d restarts %d", r, starts, *stops, f.restarts)
	}
}

func TestBootstrapRecoversInterruptedSwapWithoutDownloading(t *testing.T) {
	f, stops := bootstrapFixture(t)
	f.c.start = f.c.startService
	if err := os.MkdirAll(f.c.state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.c.savePending("1.1.0", "interrupted", f.worker.Homes); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.c.Executable, f.payload, 0755); err != nil {
		t.Fatal(err)
	}
	f.c.Version = "1.1.0+grelease"
	f.c.bootstrap.installed = f.c.Version
	f.c.downloads = "http://127.0.0.1:1/unavailable/"
	stopped, starts := true, 0
	f.c.status = func(context.Context) (workerStatus, error) {
		if stopped {
			return workerStatus{}, errors.New("worker stopped")
		}
		return f.worker, nil
	}
	originalRun := f.c.run
	f.c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && strings.Contains(strings.Join(args, " "), "--property=MainPID") {
			return []byte("0"), nil
		}
		if name == "systemctl" && len(args) > 1 && args[1] == "start" {
			starts++
			stopped = false
			f.worker.Version = f.c.Version
			return nil, nil
		}
		return originalRun(ctx, name, args...)
	}
	r := f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || r.RestartRequired || starts != 1 || *stops != 0 || f.restarts != 0 {
		t.Fatalf("%+v starts %d stops %d restarts %d", r, starts, *stops, f.restarts)
	}
	r = f.c.Install(context.Background(), "1.1.0")
	if !r.OK || r.Updated || r.Phase != "current" || starts != 1 {
		t.Fatalf("repeated update: %+v starts %d", r, starts)
	}
}

func TestBootstrapNeverRevivesStoppedWorkerWithoutMatchingJournal(t *testing.T) {
	f, _ := bootstrapFixture(t)
	f.c.status = func(context.Context) (workerStatus, error) { return workerStatus{}, errors.New("unavailable") }
	mutations := 0
	originalRun := f.c.run
	f.c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 && (args[1] == "start" || args[1] == "restart" || args[1] == "stop") {
			mutations++
			return nil, nil
		}
		return originalRun(ctx, name, args...)
	}
	r := f.c.Install(context.Background(), "1.1.0")
	if r.OK || mutations != 0 {
		t.Fatalf("%+v mutations %d", r, mutations)
	}
}

func TestBootstrapDoesNotReviveJobWhilePreviousProcessStillExits(t *testing.T) {
	f, _ := bootstrapFixture(t)
	f.c.legacyPID = os.Getpid()
	if err := os.MkdirAll(f.c.state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.c.savePending("1.1.0", "interrupted", f.worker.Homes); err != nil {
		t.Fatal(err)
	}
	f.c.status = func(context.Context) (workerStatus, error) { return workerStatus{}, errors.New("socket closed") }
	mutations := 0
	originalRun := f.c.run
	f.c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 && args[1] == "start" {
			mutations++
			return nil, nil
		}
		return originalRun(ctx, name, args...)
	}
	r := f.c.Install(context.Background(), "1.1.0")
	if r.OK || mutations != 0 || !strings.Contains(r.Error, "not been confirmed stopped") {
		t.Fatalf("%+v mutations %d", r, mutations)
	}
}

func TestBootstrapManagerPIDMismatchNeverStops(t *testing.T) {
	f, _ := bootstrapFixture(t)
	mutations := 0
	f.c.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && strings.Contains(strings.Join(args, " "), "--property=MainPID") {
			return []byte("999"), nil
		}
		mutations++
		return nil, nil
	}
	if err := f.c.stopBootstrapService(context.Background(), 123); err == nil || mutations != 0 {
		t.Fatalf("error %v mutations %d", err, mutations)
	}
}

func TestMacBootstrapStopsOnlyVerifiedLegacyLaunchAgent(t *testing.T) {
	for _, scenario := range []string{"verified", "changed-pid", "bootout-refused", "changed-executable", "unsafe-plist"} {
		t.Run(scenario, func(t *testing.T) {
			f, _ := bootstrapFixture(t)
			c := f.c
			c.OS = "darwin"
			plist := filepath.Join(c.home, "Library", "LaunchAgents", "com.znasllc.memql-worker.plist")
			if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0600)
			if scenario == "unsafe-plist" {
				mode = 0666
			}
			if err := os.WriteFile(plist, []byte("fixture values supplied through PlistBuddy seam"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(plist, mode); err != nil {
				t.Fatal(err)
			}
			stops := 0
			c.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "/usr/libexec/PlistBuddy" {
					values := map[string]string{"Print :Label": "com.znasllc.memql-worker", "Print :ProgramArguments:0": c.Executable, "Print :ProgramArguments:1": "worker", "Print :ProgramArguments:2": "run"}
					if scenario == "changed-executable" {
						values["Print :ProgramArguments:0"] += "-different"
					}
					return []byte(values[args[1]]), nil
				}
				if name == "/bin/launchctl" && args[0] == "print" {
					pid := f.worker.PID
					if scenario == "changed-pid" {
						pid--
					}
					return []byte(fmt.Sprintf("program = %s\npid = %d\n", c.Executable, pid)), nil
				}
				if name == "/bin/launchctl" && args[0] == "bootout" {
					stops++
					if scenario == "bootout-refused" {
						return nil, errors.New("refused")
					}
					return nil, nil
				}
				t.Fatalf("unexpected service command %s %v", name, args)
				return nil, nil
			}
			err := c.stopBootstrapService(context.Background(), f.worker.PID)
			wantStop := 0
			if scenario == "verified" || scenario == "bootout-refused" {
				wantStop = 1
			}
			if (err == nil) != (scenario == "verified") || stops != wantStop {
				t.Fatalf("scenario %s error %v stops %d", scenario, err, stops)
			}
		})
	}
}

func TestBootstrapConstructorRejectsAmbiguousTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memql")
	if err := os.WriteFile(path, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ helper, installed, old, release, confirm string }{
		{path, path, "1.0.0", "1.1.0", "restart-worker"},
		{"/helper", "relative", "1.0.0", "1.1.0", "restart-worker"},
		{"/helper", path, "", "1.1.0", "restart-worker"},
		{"/helper", path, "1.0.0", "dev", "restart-worker"},
		{"/helper", path, "1.0.0", "1.1.0", "yes"},
	} {
		if _, err := NewBootstrap(item.release, "computeruse", item.helper, item.installed, item.old, item.confirm); err == nil {
			t.Fatal(fmt.Sprintf("accepted %+v", item))
		}
	}
}
