package inference

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixedWorkers(w ...WorkerProcess) func(context.Context) []WorkerProcess {
	return func(context.Context) []WorkerProcess { return w }
}

// TestReadvertiseSignalsEveryWorkerItFound. More than one is unusual and
// not impossible -- a service-managed worker and one somebody started in a
// terminal -- and reloading only the first leaves the other serving from
// the models.allow it read at startup, which is a machine that refuses
// calls for a model the cluster can see it advertising.
func TestReadvertiseSignalsEveryWorkerItFound(t *testing.T) {
	var got []int
	err := readvertise(context.Background(),
		fixedWorkers(WorkerProcess{PID: 41, Via: "systemd"}, WorkerProcess{PID: 42, Via: "the process table"}),
		func(pid int) error { got = append(got, pid); return nil })
	if err != nil {
		t.Fatalf("readvertise: %v", err)
	}
	sort.Ints(got)
	if len(got) != 2 || got[0] != 41 || got[1] != 42 {
		t.Errorf("signalled %v, want both", got)
	}
}

// TestReadvertiseSaysSoWhenNoWorkerIsRunning. A named error rather than
// nil, because "no worker to tell" and "told it" are different things for
// the CLI to print -- and returning nil would make a machine whose worker
// is stopped indistinguishable from one that reloaded, which is the exact
// confusion this whole command exists to remove.
func TestReadvertiseSaysSoWhenNoWorkerIsRunning(t *testing.T) {
	err := readvertise(context.Background(), fixedWorkers(), func(int) error {
		t.Fatal("signalled something when nothing was found")
		return nil
	})
	if !errors.Is(err, ErrNoWorker) {
		t.Fatalf("err = %v, want ErrNoWorker", err)
	}
	// It is printed as-is, so it has to name what was looked at and what
	// the person should do instead.
	for _, want := range []string{"launchd", "systemd", "SIGHUP"} {
		if !strings.Contains(ErrNoWorker.Error(), want) {
			t.Errorf("ErrNoWorker = %q, want %q named in it", ErrNoWorker, want)
		}
	}
}

// TestReadvertiseIgnoresAWorkerThatExitedBetweenTheScanAndTheSignal. The
// process table is a snapshot; a worker restarting while `setup
// --inference` finishes is ordinary, and it has already read the new
// policy.yaml at startup.
func TestReadvertiseIgnoresAWorkerThatExitedBetweenTheScanAndTheSignal(t *testing.T) {
	err := readvertise(context.Background(),
		fixedWorkers(WorkerProcess{PID: 41}, WorkerProcess{PID: 42}),
		func(pid int) error {
			if pid == 41 {
				return os.ErrProcessDone
			}
			return nil
		})
	if err != nil {
		t.Fatalf("readvertise: %v", err)
	}

	// All of them gone is the same outcome as none found.
	err = readvertise(context.Background(),
		fixedWorkers(WorkerProcess{PID: 41}),
		func(int) error { return os.ErrProcessDone })
	if !errors.Is(err, ErrNoWorker) {
		t.Fatalf("err = %v, want ErrNoWorker", err)
	}
}

// TestReadvertiseReportsASignalItCouldNotSend, naming the pid and where it
// came from. A worker running as another user is the case: silence there
// would read as a successful reload.
func TestReadvertiseReportsASignalItCouldNotSend(t *testing.T) {
	err := readvertise(context.Background(),
		fixedWorkers(WorkerProcess{PID: 4242, Via: "the memql-worker.service user unit"}),
		func(int) error { return os.ErrPermission })
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), "memql-worker.service") {
		t.Errorf("err = %q, want the pid and where it was found named", err)
	}
}

// TestLaunchctlPID over launchd's own dictionary output. The trap is
// "LastExitStatus" = 0, which a looser parser reads as a pid of zero and
// then signals init with.
func TestLaunchctlPID(t *testing.T) {
	running := `{
	"LimitLoadToSessionType" = "Aqua";
	"Label" = "com.znasllc.memql-worker";
	"OnDemand" = true;
	"LastExitStatus" = 0;
	"PID" = 4242;
	"Program" = "/usr/local/bin/memql";
};`
	loadedNotRunning := `{
	"Label" = "com.znasllc.memql-worker";
	"OnDemand" = true;
	"LastExitStatus" = 1;
};`
	for out, want := range map[string]int{
		running:          4242,
		loadedNotRunning: 0,
		"":               0,
		"nonsense":       0,
	} {
		if got := launchctlPID(out); got != want {
			t.Errorf("launchctlPID(%q) = %d, want %d", out, got, want)
		}
	}
}

// TestSystemctlMainPID. `--value` prints the number alone, and 0 is
// systemd's way of saying the unit is not running -- not a process.
func TestSystemctlMainPID(t *testing.T) {
	for out, want := range map[string]int{
		"4242":         4242,
		"0":            0,
		" 4242 ":       4242,
		"":             0,
		"MainPID=4242": 0, // --value was not passed; refuse rather than guess
	} {
		if got := systemctlMainPID(out); got != want {
			t.Errorf("systemctlMainPID(%q) = %d, want %d", out, got, want)
		}
	}
}

// fakeProc builds one /proc/<pid> entry.
func fakeProc(t *testing.T, root string, pid int, argv []string, exe string, uid string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if argv != nil {
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	if uid != "" {
		body := "Name:\tmemql\nState:\tS (sleeping)\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestProcScanMatchesOnMoreThanAName.
//
// This is the test the whole scan exists for. SIGHUP is cheap and a wrong
// pid is not: the signal's default disposition is TERMINATE, so signalling
// somebody's editor, shell or build kills it. argv[0] is whatever a
// process felt like calling itself, so a match on the name alone is a
// match on a claim -- the executable behind /proc/<pid>/exe, the `worker
// run` argv and the owning uid are three facts a process cannot simply
// assert about itself.
func TestProcScanMatchesOnMoreThanAName(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "memql")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "memql-lookalike")
	if err := os.WriteFile(other, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeProc(t, root, 100, []string{"memql", "worker", "run"}, bin, "1000") // the worker
	fakeProc(t, root, 101, []string{"/usr/local/bin/memql", "worker", "run", "--log-level=debug"}, bin, "1000")
	fakeProc(t, root, 200, []string{"memql", "worker", "models"}, bin, "1000") // a different command
	fakeProc(t, root, 201, []string{"memql", "setup", "project"}, bin, "1000") // ditto
	fakeProc(t, root, 300, []string{"memql", "worker", "run"}, other, "1000")  // calls itself memql; is not
	fakeProc(t, root, 400, []string{"memql", "worker", "run"}, bin, "0")       // another user's
	fakeProc(t, root, 500, []string{"memql", "worker", "run"}, bin, "1000")    // this process itself
	fakeProc(t, root, 600, nil, bin, "1000")                                   // exited mid-scan
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {    // not a pid
		t.Fatal(err)
	}

	got := procScan{root: root, self: 500, uid: 1000, name: "memql"}.find()
	want := []int{100, 101}
	if len(got) != len(want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("found %v, want %v", got, want)
		}
	}
}

// TestFindWorkersRunsOnThisMachine is the only test here that touches the
// real service managers, and it asserts what holds on every runner rather
// than anything about what is installed: that it answers at all, quickly,
// and never with a pid that could be signalled by mistake.
func TestFindWorkersRunsOnThisMachine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, w := range FindWorkers(ctx) {
		if w.PID <= 1 {
			t.Errorf("found pid %d via %q; 0 and 1 are never this worker and signalling either is a bug", w.PID, w.Via)
		}
		if w.PID == os.Getpid() {
			t.Errorf("found this very process (pid %d) via %q", w.PID, w.Via)
		}
		if strings.TrimSpace(w.Via) == "" {
			t.Errorf("pid %d was found through nothing; the caller prints Via in the error when a signal fails", w.PID)
		}
	}
}

// TestServiceNamesMatchTheOnesTheInstallersUse. These two constants are
// COPIES -- the originals are in internal/worker/launchagent_darwin.go and
// in the installers, and this package cannot import the first (cli.go
// imports this one, so the dependency runs the other way) or execute the
// second. A rename that missed this file would leave Readvertise looking
// for a service that no longer exists and reporting "no worker is running"
// on a machine whose worker is running fine.
func TestServiceNamesMatchTheOnesTheInstallersUse(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{filepath.Join("..", "launchagent_darwin.go"), launchdLabel},
		{filepath.Join("..", "..", "..", "scripts", "install", "install-linux.sh"), systemdUnit},
		{filepath.Join("..", "..", "..", "deploy", "systemd", systemdUnit), "worker run"},
	} {
		body, err := os.ReadFile(tc.path)
		if err != nil {
			t.Skipf("%s is not where it was; re-point this test rather than deleting it: %v", tc.path, err)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s no longer contains %q", tc.path, tc.want)
		}
	}
}
