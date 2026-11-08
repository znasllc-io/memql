//go:build darwin || linux

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/cockpit/internal/worker/workbench"
)

// One build reservation per operating-system user, shared by every worker
// process and every cluster enrollment. It deliberately does not derive from
// MEMQL_HOME, worker configuration or workspace_root: those vary per cluster.
// A closed laptop retains the kernel lock. Process death releases the lock,
// but an unfinished record still refuses reuse until its effects reconcile.
var pipelineCapacityRoot = func() (string, error) {
	owner, err := user.Current()
	if err != nil || owner.HomeDir == "" {
		return "", errors.New("cannot resolve the worker account's home for capacity admission")
	}
	return filepath.Join(owner.HomeDir, ".memql", "worker-capacity"), nil
}

// THE HOST BUILD LOCK AND ITS SLOTS (policy pipelines.max_concurrent_steps).
//
// pipeline.lock is the host build lock. A pipeline step holds it EXCLUSIVELY, a
// Fleet workbench holds it shared, and a guarded local build
// (scripts/lib/host_build_guard.py) holds it exclusively, so each keeps the
// others out. A machine that allows several steps keeps that contract by
// sharing ONE exclusive hold among the steps of this process: the first step
// takes it, later steps join under it, and the last to leave releases it
// (pipelineHost). A workbench or a local build therefore still waits until every
// step has ended, and an older guard or Cockpit sees the same lock it always
// did. The cost is that a second worker process on the same account (a legacy
// single-home worker run beside the supervisor) is refused while this one runs
// any step: the steps of one process share the machine, two processes do not.
// Sharing the lock across processes would need a mode flock does not offer
// (shared among steps, exclusive against workbenches and local builds), and the
// supervisor runs every enrollment in one process (fleet.go).
//
// Each slot has its own lock and attempt record (capacity.PipelineSlotFile).
// Slot 0 is pipeline.lock itself: the lock this process holds, and the record
// of the step in slot 0, written exactly as a machine with one slot always
// wrote it. Slot n is pipeline-slot-n.lock, locked exclusively by its step under
// the host lock. A record outlives its process as before. A slot whose record
// has content and that no step of this process holds is an interrupted attempt:
// it is reconciled (or keeps the machine from admitting a step) as the single
// record always was, while the steps running in other slots are left alone.
//
// TWO ATTEMPTS OF ONE STEP NEVER HOLD SLOTS AT ONCE. The engine retries a step
// on the machine it last ran on trusting that "a machine still holding the old
// command refuses the next one" (component/pipelinerun/attempts.go): with one
// slot the lock did it, and with several the dispatch's run and step
// (pipelineSlotUse.step) do. A dispatch that names neither runs only alone.
//
// TestPipelineSlotsShareTheHostLockAcrossOneProcess,
// TestPipelineSlotsRefuseASecondAttemptOfTheSameStep,
// TestAnInterruptedSlotIsReconciledWhileAnotherSlotRuns and
// TestPipelineSlotsOutliveTheirProcess cover it.

type pipelineReservation struct {
	file  *os.File
	dirty bool
	slot  int
	host  *pipelineHost
	// use is what the step in this slot asked for, charged by every other
	// step's admission; reconcile marks a slot held only to reconcile an
	// interrupted attempt, which no step runs in.
	use       pipelineSlotUse
	reconcile bool
	// closed refuses every record write after close: slot 0's file is the
	// host lock's descriptor, which stays open while other slots run.
	closed atomic.Bool
}

type pipelineAttemptRecord struct {
	Workspace string   `json:"workspace"`
	Execution string   `json:"execution"`
	Container string   `json:"container,omitempty"`
	Services  []string `json:"services,omitempty"`
	Network   string   `json:"network,omitempty"`
	// Identity of the Docker endpoint is needed before another worker may
	// reconcile it; it must not remove a namesake on a different daemon.
	DockerID       string `json:"dockerID,omitempty"`
	DockerEndpoint string `json:"dockerEndpoint,omitempty"`
}

var errPipelineBusy = errors.New("this machine's build capacity is occupied")
var errPipelineUnreconciled = errors.New("an interrupted build needs reconciliation before this machine can accept another build")

// pipelineHost is this process's share of one capacity root: the host build
// lock, held while any slot is, and the slots held. Every field is guarded by
// pipelineHosts' mutex. Tests point pipelineCapacityRoot at a directory of
// their own, so the shares are kept per root.
type pipelineHost struct {
	root  string
	fence *os.File
	held  map[int]*pipelineReservation
}

var pipelineHosts = struct {
	sync.Mutex
	byRoot map[string]*pipelineHost
}{byRoot: map[string]*pipelineHost{}}

// hasPipelineAttempt reports, without taking any lock, whether a slot holds a
// record. The idle maintenance reads it before it competes for the host lock.
func hasPipelineAttempt() (bool, error) {
	root, err := pipelineCapacityRoot()
	if err != nil {
		return false, err
	}
	stat, err := os.Lstat(capacity.PipelineSlotFile(root, 0))
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil {
		if !stat.Mode().IsRegular() {
			return false, errPipelineUnreconciled
		}
		if stat.Size() != 0 {
			return true, nil
		}
	}
	return capacity.PipelineSlotsPending(root)
}

// acquirePipelineReservation takes a slot on a machine that runs one step at a
// time: the door the single-slot callers and their tests use.
func acquirePipelineReservation() (*pipelineReservation, error) {
	return acquirePipelineSlot(1, pipelineSlotUse{})
}

// acquirePipelineSlot takes the lowest free slot below slots for a step that
// asked for use. It refuses with errPipelineBusy when every one is held, when an
// attempt of the same step holds one, while an interrupted attempt is being
// reconciled, and when another process holds the host lock. A slot whose record
// outlived its step is handed over dirty, for the caller to reconcile.
func acquirePipelineSlot(slots int, use pipelineSlotUse) (*pipelineReservation, error) {
	if slots < 1 || slots > capacity.PipelineSlots {
		return nil, fmt.Errorf("pipelines.max_concurrent_steps must be between 1 and %d", capacity.PipelineSlots)
	}
	root, err := pipelineCapacityRoot()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	pipelineHosts.Lock()
	defer pipelineHosts.Unlock()
	host, err := pipelineHostLocked(root)
	if err != nil {
		return nil, err
	}
	defer host.releaseIfIdleLocked()
	if err := host.admitLocked(); err != nil {
		return nil, err
	}
	for _, held := range host.held {
		if held.reconcile {
			return nil, fmt.Errorf("%w: an interrupted build is being reconciled", errPipelineBusy)
		}
	}
	// Counted, not only looked up: an owner who lowers max_concurrent_steps
	// leaves steps running in slots above the new count until they end.
	if len(host.held) >= slots {
		return nil, errPipelineBusy
	}
	free := -1
	for slot := 0; slot < slots; slot++ {
		if host.held[slot] == nil {
			free = slot
			break
		}
	}
	if free < 0 {
		return nil, errPipelineBusy
	}
	for _, held := range host.held {
		if held.use.step != "" && held.use.step == use.step {
			return nil, fmt.Errorf("%w: an earlier attempt of this step is still running here", errPipelineBusy)
		}
		if held.use.step == "" || use.step == "" {
			return nil, fmt.Errorf("%w: a step the cluster did not identify runs only alone", errPipelineBusy)
		}
	}
	reservation, err := host.holdLocked(free)
	if err != nil {
		return nil, err
	}
	reservation.use = use
	return reservation, nil
}

// pipelineHostLocked is this process's share of root, taking the host build
// lock when this process holds none of its slots. The caller holds
// pipelineHosts' mutex and releases an idle share with releaseIfIdleLocked.
func pipelineHostLocked(root string) (*pipelineHost, error) {
	host := pipelineHosts.byRoot[root]
	if host != nil && host.fence != nil {
		return host, nil
	}
	fence, err := lockPipelineFile(capacity.PipelineSlotFile(root, 0))
	if err != nil {
		return nil, err
	}
	host = &pipelineHost{root: root, fence: fence, held: map[int]*pipelineReservation{}}
	pipelineHosts.byRoot[root] = host
	return host, nil
}

// lockPipelineFile opens a slot's file and locks it exclusively. Never unlink
// this file: removing a locked inode would let a newcomer acquire a different
// inode and run concurrently with the existing owner.
func lockPipelineFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "pipeline capacity")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errPipelineBusy
		}
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("capacity record is not a regular file")
	}
	return file, nil
}

// admitLocked is what every slot's admission checks under the host lock,
// whichever step took it.
func (h *pipelineHost) admitLocked() error {
	// A local build uses this same lock but a separate durable record. Its
	// supervisor may have died while Docker still has effects in progress.
	// Never interpret it as a pipeline checkout or clear it based on PID age.
	localBuild := filepath.Join(h.root, "local-build.json")
	if _, err := os.Lstat(localBuild); err == nil {
		return fmt.Errorf("%w: inspect %s with host_build_guard.py --inspect from the local build checkout; reconcile its processes and image operations before clearing that record", errPipelineLocalBuildFenced, localBuild)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect retained local build record: %w", err)
	}
	if interrupted, err := workbench.HasInterrupted(h.root); err != nil || interrupted {
		return errPipelineUnreconciled
	}
	return nil
}

// holdLocked takes slot for this process. Slot 0's record is the host lock's
// own file; slot n's is its own file, locked under the host lock.
func (h *pipelineHost) holdLocked(slot int) (*pipelineReservation, error) {
	file := h.fence
	if slot > 0 {
		var err error
		if file, err = lockPipelineFile(capacity.PipelineSlotFile(h.root, slot)); err != nil {
			return nil, err
		}
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		if slot > 0 {
			file.Close()
		}
		return nil, errors.New("capacity record is not a regular file")
	}
	reservation := &pipelineReservation{file: file, dirty: stat.Size() != 0, slot: slot, host: h}
	h.held[slot] = reservation
	return reservation, nil
}

// releaseIfIdleLocked releases the host build lock once no slot is held.
func (h *pipelineHost) releaseIfIdleLocked() {
	if len(h.held) != 0 || h.fence == nil {
		return
	}
	h.fence.Close()
	h.fence = nil
	if pipelineHosts.byRoot[h.root] == h {
		delete(pipelineHosts.byRoot, h.root)
	}
}

// claimInterruptedPipelineSlots holds every slot whose record outlived its
// step: a record with content in a slot no step of this process holds. The host
// lock keeps every other process's steps out, so nothing can be writing such a
// record. owner is the step about to run, which reconciles its own slot; nil is
// the idle maintenance, which takes the host lock for the claim when this
// process holds no slot (errPipelineBusy when another process holds it). A name
// no Cockpit writes, or a slot file that is not a regular file, is never
// interpreted: errPipelineUnreconciled, for an operator.
func claimInterruptedPipelineSlots(owner *pipelineReservation) ([]*pipelineReservation, error) {
	pipelineHosts.Lock()
	defer pipelineHosts.Unlock()
	var host *pipelineHost
	if owner != nil {
		if owner.closed.Load() || owner.host.held[owner.slot] != owner {
			return nil, errPipelineUnreconciled
		}
		host = owner.host
	} else {
		root, err := pipelineCapacityRoot()
		if err != nil {
			return nil, err
		}
		if err = os.MkdirAll(root, 0700); err != nil {
			return nil, err
		}
		if host, err = pipelineHostLocked(root); err != nil {
			return nil, err
		}
		defer host.releaseIfIdleLocked()
	}
	for _, held := range host.held {
		if held.reconcile {
			return nil, fmt.Errorf("%w: an interrupted build is being reconciled", errPipelineBusy)
		}
	}
	var claims []*pipelineReservation
	fail := func(err error) ([]*pipelineReservation, error) {
		for _, claim := range claims {
			claim.closeLocked()
		}
		return nil, err
	}
	claim := func(slot int) error {
		reservation, err := host.holdLocked(slot)
		if err != nil {
			return err
		}
		reservation.reconcile = true
		if !reservation.dirty {
			reservation.closeLocked()
			return nil
		}
		claims = append(claims, reservation)
		return nil
	}
	if host.held[0] == nil {
		stat, err := host.fence.Stat()
		if err != nil {
			return fail(err)
		}
		if stat.Size() != 0 {
			if err := claim(0); err != nil {
				return fail(err)
			}
		}
	}
	extra, err := capacity.PipelineSlotFiles(host.root)
	if err != nil {
		return fail(err)
	}
	for _, slot := range extra {
		info, err := os.Lstat(slot.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		if !info.Mode().IsRegular() {
			return fail(fmt.Errorf("%w: %s is not a regular file", errPipelineUnreconciled, slot.Path))
		}
		if info.Size() == 0 || (slot.Slot != 0 && host.held[slot.Slot] != nil) {
			continue
		}
		if slot.Slot == 0 {
			return fail(fmt.Errorf("%w: %s is not a slot this worker writes", errPipelineUnreconciled, slot.Path))
		}
		if err := claim(slot.Slot); err != nil {
			return fail(err)
		}
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].slot < claims[j].slot })
	return claims, nil
}

// others is what every other step holding a slot on this machine asked for,
// in slot order. Every admission charges all of it (checkPipelineHeadroom).
func (r *pipelineReservation) others() []pipelineSlotUse {
	pipelineHosts.Lock()
	defer pipelineHosts.Unlock()
	slots := make([]int, 0, len(r.host.held))
	for slot, held := range r.host.held {
		if held != r && !held.reconcile {
			slots = append(slots, slot)
		}
	}
	sort.Ints(slots)
	uses := make([]pipelineSlotUse, 0, len(slots))
	for _, slot := range slots {
		uses = append(uses, r.host.held[slot].use)
	}
	return uses
}

// pipelineInterruptedAttempt reports whether any slot holds a record no
// running step of this process holds, including one being reconciled right
// now: an attempt that may still have a container writing into a cache. Cache
// retention does not run then. A measurement it cannot make reads as an
// interrupted attempt.
func pipelineInterruptedAttempt() bool {
	root, err := pipelineCapacityRoot()
	if err != nil {
		return true
	}
	pipelineHosts.Lock()
	defer pipelineHosts.Unlock()
	host := pipelineHosts.byRoot[root]
	unheld := func(slot int) bool {
		return host == nil || host.held[slot] == nil || host.held[slot].reconcile
	}
	if stat, err := os.Lstat(capacity.PipelineSlotFile(root, 0)); err == nil {
		if !stat.Mode().IsRegular() || (stat.Size() != 0 && unheld(0)) {
			return true
		}
	} else if !os.IsNotExist(err) {
		return true
	}
	extra, err := capacity.PipelineSlotFiles(root)
	if err != nil {
		return true
	}
	for _, slot := range extra {
		info, err := os.Lstat(slot.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return true
		}
		if info.Size() != 0 && (slot.Slot == 0 || unheld(slot.Slot)) {
			return true
		}
	}
	return false
}

func (r *pipelineReservation) read() (pipelineAttemptRecord, error) {
	var record pipelineAttemptRecord
	if r.closed.Load() {
		return record, errPipelineUnreconciled
	}
	_, err := r.file.Seek(0, io.SeekStart)
	if err != nil {
		return record, err
	}
	err = json.NewDecoder(io.LimitReader(r.file, 4096)).Decode(&record)
	if err != nil {
		return record, fmt.Errorf("read interrupted build record: %w", err)
	}
	return record, nil
}
func (r *pipelineReservation) begin(record pipelineAttemptRecord) error {
	if r.closed.Load() {
		return errPipelineUnreconciled
	}
	if r.dirty {
		previous, err := r.read()
		if err != nil || previous.Execution != "cleanup" || previous.Workspace != record.Workspace || previous.Container != "" || previous.DockerID != "" || previous.DockerEndpoint != "" || previous.Network != "" || len(previous.Services) != 0 {
			return errPipelineUnreconciled
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	r.dirty = true
	if _, err = r.file.WriteAt(data, 0); err != nil {
		return err
	}
	if err = r.file.Truncate(int64(len(data))); err != nil {
		return err
	}
	return r.file.Sync()
}

// cleanupReady is written only after runtime teardown is confirmed. A
// replacement worker may then remove scratch data without Docker or replaying
// the command. Keep the same locked inode throughout this transition.
func (r *pipelineReservation) cleanupReady(workspace string) error {
	if r.closed.Load() {
		return errPipelineUnreconciled
	}
	if !filepath.IsAbs(workspace) || !strings.HasPrefix(filepath.Base(workspace), "step-") {
		return errPipelineUnreconciled
	}
	if r.dirty {
		record, err := r.read()
		if err != nil {
			return err
		}
		if record.Workspace != workspace {
			return errPipelineUnreconciled
		}
	}
	data, err := json.Marshal(pipelineAttemptRecord{Execution: "cleanup", Workspace: workspace})
	if err != nil {
		return err
	}
	r.dirty = true
	if _, err = r.file.WriteAt(data, 0); err != nil {
		return err
	}
	if err = r.file.Truncate(int64(len(data))); err != nil {
		return err
	}
	return r.file.Sync()
}

func (r *pipelineReservation) clear() error {
	if r.closed.Load() {
		return errPipelineUnreconciled
	}
	if err := r.file.Truncate(0); err != nil {
		return err
	}
	if err := r.file.Sync(); err != nil {
		return err
	}
	r.dirty = false
	return nil
}

// close gives the slot back, with its record as it stands: a record with
// content stays an interrupted attempt for the next admission or the idle
// maintenance to reconcile. The last slot to go releases the host lock. Safe
// to call more than once.
func (r *pipelineReservation) close() {
	if r == nil || r.closed.Load() {
		return
	}
	pipelineHosts.Lock()
	defer pipelineHosts.Unlock()
	r.closeLocked()
}

func (r *pipelineReservation) closeLocked() {
	if r.closed.Swap(true) {
		return
	}
	if r.host.held[r.slot] == r {
		delete(r.host.held, r.slot)
	}
	if r.slot > 0 {
		r.file.Close()
	}
	r.host.releaseIfIdleLocked()
}
