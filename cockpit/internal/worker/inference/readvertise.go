package inference

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// readvertise.go tells a running worker to re-read policy.yaml.
//
// WHAT A SIGHUP ACHIEVES, AND WHAT IT DOES NOT. The worker's signal
// handler calls Policy.Reload and nothing else, so the machine's own
// answer to "may I serve this model" changes at once. THE CLUSTER DOES NOT
// SEE THE MODEL YET. The engine binds `model:<id>` labels at Register and
// Heartbeat carries none of them (CLAUDE.md, local models rule 4), so a
// newly allowed model becomes visible only after a RECONNECT -- which the
// worker takes for itself when the advertised label set changes, only
// while nothing is in flight, and never twice inside two minutes. Between
// the reload and that reconnect sit the inventory's own 90-second cache
// and the 60-second refresh ticker that drives it.
//
// So the sentence to print after this returns is "the cluster will see it
// within a minute or two", never "it is available now". A caller that
// promised the second sends a person to the Fleet page to watch for a row
// that is not due yet, and they read the delay as a failure -- which is
// exactly the confusion `memql worker models` was written to end.
//
// The function keeps the name the design record gave it. It is a slight
// overstatement of what one signal does, and this comment is the correction.

// The service identities, COPIED from the two places that own them:
// internal/worker/launchagent_darwin.go (which this package must not
// import -- cli.go imports this one, so the dependency runs the other way)
// and scripts/install/install-linux.sh. A test asserts both files still
// contain these strings, because a rename that missed this copy would
// leave Readvertise reporting "no worker is running" on a machine whose
// worker is running perfectly well.
const (
	launchdLabel = "com.znasllc.memql-worker"
	systemdUnit  = "memql-worker.service"
)

// ErrNoWorker is returned when nothing was signalled. It is a SENTENCE the
// CLI prints rather than a failure it reports: a paired machine whose
// worker is stopped is an ordinary state, and `setup --inference` has
// already done the part that lasts -- policy.yaml is on disk, and the next
// worker to start reads it.
var ErrNoWorker = errors.New("no running worker was found through launchd, systemd or this machine's process table, so nothing was reloaded; a worker started from here on reads policy.yaml at startup, and one already running in a terminal takes a SIGHUP you send it yourself")

// WorkerProcess is a running worker this machine found, and how.
//
// Via is carried because it is what an operator needs when a signal is
// refused: "the memql-worker.service user unit" and "this machine's
// process table" send them to completely different places.
type WorkerProcess struct {
	PID int
	Via string
}

// Readvertise makes the running worker re-read policy.yaml. See the file
// comment for what that does and does not make visible to the cluster.
func Readvertise(ctx context.Context) error {
	return readvertise(ctx, FindWorkers, sendHUP)
}

// readvertise is the seam: the finder and the signal are both injected so
// the whole decision is testable without a worker, a service manager or a
// real signal delivered to a real pid.
func readvertise(ctx context.Context, find func(context.Context) []WorkerProcess, hup func(int) error) error {
	found := find(ctx)
	if len(found) == 0 {
		return ErrNoWorker
	}
	var signalled int
	var firstErr error
	for _, w := range found {
		err := hup(w.PID)
		switch {
		case err == nil:
			signalled++
		case isGone(err):
			// It exited between the scan and the signal. A worker
			// restarting while `setup --inference` finishes has already
			// read the new policy.yaml at startup, so this is not a miss.
		case firstErr == nil:
			firstErr = fmt.Errorf("sending SIGHUP to the worker at pid %d, found through %s: %w", w.PID, w.Via, err)
		}
	}
	// An error only when NOTHING was reloaded. The caller's next sentence
	// is about whether the reload happened at all, and on the one machine
	// in a thousand that runs two workers, one of them reloading is the
	// answer to that question.
	if signalled == 0 {
		if firstErr != nil {
			return firstErr
		}
		return ErrNoWorker
	}
	return nil
}

// FindWorkers lists the running workers on this machine.
//
// THE SERVICE MANAGERS ARE ASKED FIRST because they do not guess: the pid
// systemd or launchd reports IS that unit's main process, which is the
// state both installers leave a machine in. The process-table scan behind
// them is for the worker somebody started in a terminal, which no service
// manager knows about.
//
// Nothing here fails: a machine with no systemd, no launchd and no /proc
// simply reports nothing found, which is a sentence the caller prints
// rather than an error it handles.
func FindWorkers(ctx context.Context) []WorkerProcess {
	var out []WorkerProcess
	seen := map[int]bool{}
	add := func(pid int, via string) {
		// 0 is systemd's "not running" and 1 is init. Signalling either
		// because a parser returned a zero value is the failure this
		// whole file is written against.
		if pid <= 1 || seen[pid] {
			return
		}
		seen[pid] = true
		out = append(out, WorkerProcess{PID: pid, Via: via})
	}

	if s, err := runProbe(ctx, "systemctl", "--user", "show", "--property=MainPID", "--value", systemdUnit); err == nil {
		add(systemctlMainPID(s), "the "+systemdUnit+" user unit")
	}
	if s, err := runProbe(ctx, "launchctl", "list", launchdLabel); err == nil {
		add(launchctlPID(s), "the "+launchdLabel+" LaunchAgent")
	}
	for _, pid := range defaultProcScan().find() {
		add(pid, "this machine's process table")
	}
	return out
}

// systemctlMainPID reads `systemctl --user show --property=MainPID
// --value`. Anything that is not a bare number is read as NONE, including
// `MainPID=4242` -- that is what the output looks like when --value was
// dropped, and a parser that coped with it would hide the mistake until
// the day systemd changed the other format too.
func systemctlMainPID(out string) int {
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return pid
}

// launchctlPID reads the dictionary `launchctl list <label>` prints.
//
// The key is matched with its quotes and its `=`, because the same
// dictionary carries `"LastExitStatus" = 0;` and a looser match on the
// digits would return a pid of zero for an agent that is loaded and not
// running.
func launchctlPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, `"PID"`)
		if !ok {
			continue
		}
		rest, ok = strings.CutPrefix(strings.TrimSpace(rest), "=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), ";")))
		if err != nil {
			continue
		}
		return pid
	}
	return 0
}

// procScan finds a worker nobody's service manager knows about.
//
// IT MATCHES ON THREE FACTS, NOT ON A NAME, and that is the whole reason
// it is written out rather than shelled out to `pgrep -f memql`. SIGHUP's
// default disposition is TERMINATE, so a wrong pid does not fail quietly
// -- it kills somebody's editor, shell or build. argv[0] is whatever a
// process felt like calling itself, so the executable behind
// /proc/<pid>/exe, the `worker run` argv and the owning uid are checked
// together: the first two are facts about the program, and the third keeps
// this out of another user's processes entirely.
type procScan struct {
	// root is /proc. A field so the tests can build one.
	root string
	// self is this process, which is never the answer.
	self int
	// uid is who we are. A worker we may signal is one we own.
	uid int
	// name is the basename of this executable -- "memql" on an installed
	// machine. Compared against the real target of /proc/<pid>/exe, which
	// a process cannot choose for itself.
	name string
}

func defaultProcScan() procScan {
	name := "memql"
	if exe, err := os.Executable(); err == nil {
		name = filepath.Base(exe)
	}
	return procScan{root: "/proc", self: os.Getpid(), uid: os.Getuid(), name: name}
}

// find returns the pids, lowest first. Every read here is best-effort: a
// process that exits mid-scan takes its whole directory with it, which is
// ordinary rather than an error.
func (s procScan) find() []int {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == s.self {
			continue
		}
		if !s.isWorker(pid) {
			continue
		}
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func (s procScan) isWorker(pid int) bool {
	dir := filepath.Join(s.root, strconv.Itoa(pid))

	raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(raw) == 0 {
		return false
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if !hasSubcommand(argv, "worker", "run") {
		return false
	}

	// The executable, not the name it printed. Unreadable is a no: on
	// Linux only the owner (or root) can read another process's exe link,
	// so a refusal here is usually a process we would have rejected on
	// the uid anyway.
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil || filepath.Base(strings.TrimSuffix(exe, " (deleted)")) != s.name {
		return false
	}
	return statusUID(filepath.Join(dir, "status")) == s.uid
}

// hasSubcommand reports whether argv[1:] carries `first second` next to
// each other. Adjacency matters: `memql worker models --run` is not a
// worker, and a flag that happened to contain "run" is not either.
func hasSubcommand(argv []string, first, second string) bool {
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == first && argv[i+1] == second {
			return true
		}
	}
	return false
}

// statusUID reads the real uid out of /proc/<pid>/status, or -1.
//
// Parsed from status rather than stat'ing the directory because
// syscall.Stat_t is not portable, and this file has to compile everywhere
// the cockpit does even though only Linux has a /proc to walk.
func statusUID(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(body), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return -1
		}
		uid, err := strconv.Atoi(fields[0])
		if err != nil {
			return -1
		}
		return uid
	}
	return -1
}

// sendHUP delivers the signal. Never to a process group: the worker is one
// process, and a group signal from here would reach whatever else shares
// its group -- under a terminal, that is the shell the person is typing in.
func sendHUP(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGHUP)
}

// isGone reports that the pid was already finished. Both spellings,
// because os.Process answers with its own sentinel once it has reaped and
// with the kernel's errno before that.
func isGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}
