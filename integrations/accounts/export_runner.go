package accounts

// export_runner.go -- the runner that claims and supervises account exports
// (design record docs/superpowers/specs/2026-11-12-account-export-design.md,
// section F, "The run").
//
// Wired on AGENT nodes only (app/accounts_export_agent.go): the export is long
// work beside the agent's other long work, and the bff that serves the calls
// must not hold a snapshot open for an hour. Any number of agent replicas run
// one runner each; the database decides which of them, if any, runs an export.
//
// # Every tick (every five seconds)
//
//  1. Drafts not started within fifteen minutes are deleted, and the files
//     still recorded on failed or cancelled exports are deleted again
//     (export_store.go, sweepKeptObjects).
//  2. A running export whose heartbeat is two minutes old is queued again (its
//     node stopped), or failed after its third attempt, or cancelled when a
//     person asked to. A file its attempt committed and recorded is deleted
//     first; a run that ends does not wait on that delete, a run that would be
//     queued again does (export_store.go, reclaimStale).
//  3. When this node runs nothing, it claims the oldest queued export, but
//     only when no export is running anywhere: one at a time on the whole
//     installation (claim's advisory lock makes that one decision). A queued
//     row already at the attempt limit is failed instead of claimed.
//
// # While an attempt runs
//
// A heartbeat every twenty seconds renews the lease and reads
// cancel_requested. Each heartbeat statement has its own ten-second deadline,
// so a statement that hangs cannot hold the loop. A cancel request cancels the
// work's context. A heartbeat that renews no row means the lease is gone
// (another node took the run over): the work is cancelled at once and this node
// writes nothing.
//
// THE NODE FENCES ITSELF. A node whose heartbeats keep failing -- the pool is
// exhausted, the database is unreachable from it -- cannot learn that another
// replica has taken its run. Once it has gone exportStaleAfter minus thirty
// seconds without a renewal that landed, it cancels its own work as if the
// lease were lost, whatever the errors said: past exportStaleAfter another
// replica may already be running the same export. The fence runs on its
// own goroutine and timer, so a renewal that hangs past its deadline cannot
// hold it; what it does not cover (a node paused as a whole) is said at beat.
//
// Every write of the attempt is guarded on (node, attempt), so a late answer
// from a node that lost its run cannot overwrite the next attempt.
//
// # Ending
//
//   - the work answers: complete.
//   - the work fails: failed, with its ExportFailure code, or export_failed.
//   - a person cancelled: cancelled, whatever the work answered.
//   - the node is stopping: the attempt is handed back (queued) for another
//     node to start at once, its attempt still counted (the claim refuses a row
//     at the limit, so rolling restarts cannot run one export without end), or
//     ended cancelled when a person asked to cancel it. A run that dies without
//     stopping is caught by the take-over instead.
//
// WHO REMOVES WHAT. The work cleans up an attempt that fails or is cancelled
// before it returns. A result it returned that the runner cannot record (the
// lease was lost, a cancel landed after the work finished, the node is
// stopping, or the final write matched nothing) is removed by the runner
// through the blob store (discard), so no file is left that no record names. A
// final write that errored is ambiguous -- it may have landed -- so its object
// is kept and its key logged at error level instead. A node that dies after the
// commit runs none of this: the run records the key on the row as soon as the
// commit lands, and the take-over, the claim and the hand-back delete a
// recorded file before they requeue or end its row.
//
// Every final state clears the sealed passphrase in the same statement.
//
// Tests: export_db_test.go (TestExportRun*, TestExportStale*,
// TestExportSecondExportWaitsWhileOneRuns, TestExportRunIsTakenOverByAnotherNode)
// and export_record_db_test.go (the fence, the bounded heartbeat, the discard,
// the hand-back, the unsealing failure, two claims at once).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

const (
	exportTickEvery      = 5 * time.Second
	exportHeartbeatEvery = 20 * time.Second
	// exportHeartbeatTimeout bounds one heartbeat statement.
	exportHeartbeatTimeout = 10 * time.Second
	// exportFenceAfter is how long a node keeps working without a renewal that
	// landed: thirty seconds inside the window after which another replica
	// takes the run over.
	exportFenceAfter = exportStaleAfter - 30*time.Second
	// exportFinishTimeout bounds the final write, which runs on a context of
	// its own: the run's context may be the one that ended.
	exportFinishTimeout = 10 * time.Second
)

var (
	errExportCancelRequested = errors.New("the export was cancelled")
	errExportLeaseLost       = errors.New("another node took the export over")
	errExportFenced          = errors.New("this node could not renew its lease in time")
)

// ExportRunner claims and supervises exports on one node.
type ExportRunner struct {
	*component.Component
	exports *Exports
	nodeID  func() string

	// tickEvery, heartbeatEvery, heartbeatTimeout and fenceAfter are the
	// constants above; fields so a test ticks by hand and heartbeats in
	// milliseconds. work, when set, is used instead of the integration's (a
	// test's fake), and renew instead of Exports.heartbeat.
	tickEvery        time.Duration
	heartbeatEvery   time.Duration
	heartbeatTimeout time.Duration
	fenceAfter       time.Duration
	work             ExportWork
	renew            func(ctx context.Context, x Export, node string) (cancelRequested, ok bool, err error)

	busy    atomic.Bool
	running sync.WaitGroup
	// warnedNoWork keeps the "nothing to run exports with" line to one.
	warnedNoWork atomic.Bool
}

// NewExportRunner builds the runner. nodeID is resolved on every tick: a node
// learns its id in the cluster phase, after the runner is built, and a node
// with no id yet claims nothing.
func NewExportRunner(exports *Exports, nodeID func() string) (*ExportRunner, error) {
	if exports == nil || nodeID == nil {
		return nil, errors.New("account export runner: the exports and the node id are required")
	}
	comp, err := component.New(common.ComponentName("accountExportRunner"))
	if err != nil {
		return nil, err
	}
	r := &ExportRunner{Component: comp, exports: exports, nodeID: nodeID,
		tickEvery: exportTickEvery, heartbeatEvery: exportHeartbeatEvery,
		heartbeatTimeout: exportHeartbeatTimeout, fenceAfter: exportFenceAfter,
		renew: exports.heartbeat}
	if err := r.ConfigureLifecycle(component.WithRunHook(r.run)); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *ExportRunner) run(ctx context.Context, markStarted func()) error {
	markStarted()
	t := time.NewTicker(r.tickEvery)
	defer t.Stop()
	for {
		r.tick(ctx)
		select {
		case <-ctx.Done():
			r.wait()
			return nil
		case <-t.C:
		}
	}
}

// wait blocks until this node's attempt, if any, has ended.
func (r *ExportRunner) wait() { r.running.Wait() }

// tick runs one round. A step that fails is logged and the next step still
// runs: a failed sweep must not stop a claim.
func (r *ExportRunner) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	log := r.exports.logger
	if err := r.exports.sweepDrafts(ctx); err != nil {
		log.Warn("account export: sweep failed", "error", err)
	}
	if err := r.exports.sweepKeptObjects(ctx); err != nil {
		log.Warn("account export: sweep of kept files failed", "error", err)
	}
	if err := r.exports.reclaimStale(ctx); err != nil {
		log.Warn("account export: take-over failed", "error", err)
	}
	node := strings.TrimSpace(r.nodeID())
	if node == "" || r.busy.Load() {
		return
	}
	work := r.work
	if work == nil {
		work = r.exports.exportWork()
	}
	if work == nil {
		if r.warnedNoWork.CompareAndSwap(false, true) {
			log.Warn("account export: this node has nothing to run exports with, so it claims none")
		}
		return
	}
	x, sealed, ok, err := r.exports.claim(ctx, node)
	if err != nil {
		log.Warn("account export: claim failed", "error", err)
		return
	}
	if !ok {
		return
	}
	r.busy.Store(true)
	r.running.Add(1)
	go func() {
		defer r.running.Done()
		defer r.busy.Store(false)
		r.supervise(ctx, x, sealed, node, work)
	}()
}

// beat starts the two halves of the lease for one attempt and answers a
// channel closed when the FENCE has stopped. The renewal loop is not waited
// for: a renewal that ignores its deadline must not hold the attempt's end,
// and it exits by itself once its statement returns (every write it makes is
// guarded on the attempt, so a late one changes nothing).
//
//   - The renewal loop renews the lease every heartbeatEvery, each statement
//     under heartbeatTimeout. It records the time of each renewal that landed,
//     cancels the work with the lease-lost cause when a renewal matched no row,
//     and with the cancel cause when a person asked.
//   - The fence runs on its own goroutine and its own timer, which the renewal
//     cannot block. It cancels the work as if the lease were lost once the
//     last renewal that LANDED is fenceAfter old, whatever the renewals' errors
//     said and even while a renewal is still waiting on the database.
//
// WHAT THE FENCE DOES NOT COVER. It measures time on this process's monotonic
// clock while the process runs. A node paused as a whole for longer than the
// margin (a stopped virtual machine, a long stop-the-world pause) wakes after
// another replica may have taken the run over, and works beside it until the
// fence's next check, at most the fence's remaining time. The guarded final
// writes keep the record right, and the runner removes the file it could not
// record (discard); what this cannot prevent is the second read and stream
// for that while.
func (r *ExportRunner) beat(ctx context.Context, cancel context.CancelCauseFunc, x Export, node string, log *slog.Logger) <-chan struct{} {
	var renewed atomic.Int64
	renewed.Store(time.Now().UnixNano())
	since := func() time.Duration { return time.Since(time.Unix(0, renewed.Load())) }

	go func() {
		t := time.NewTicker(r.heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			beatCtx, cancelBeat := context.WithTimeout(ctx, r.heartbeatTimeout)
			cancelRequested, ok, err := r.renew(beatCtx, x, node)
			cancelBeat()
			if ctx.Err() != nil {
				return
			}
			switch {
			case err != nil:
				log.Warn("account export: heartbeat failed", "error", err)
			case !ok:
				log.Warn("account export: the lease is gone; another node holds this run")
				cancel(errExportLeaseLost)
				return
			case cancelRequested:
				cancel(errExportCancelRequested)
				return
			default:
				renewed.Store(time.Now().UnixNano())
			}
		}
	}()

	fenceDone := make(chan struct{})
	go func() {
		defer close(fenceDone)
		fence := time.NewTimer(r.fenceAfter)
		defer fence.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-fence.C:
			}
			if left := r.fenceAfter - since(); left > 0 {
				fence.Reset(left)
				continue
			}
			log.Warn("account export: no lease renewal landed in time; this node stops its work so it cannot run beside the node that takes the run over",
				"since", since().String())
			cancel(errExportFenced)
			return
		}
	}()
	return fenceDone
}

// supervise runs one attempt to its end.
func (r *ExportRunner) supervise(parent context.Context, x Export, sealed, node string, work ExportWork) {
	s := r.exports
	log := s.logger.With("exportId", x.ID, "attempt", x.Attempt)
	log.Info("account export: running", "accountId", x.AccountID)

	finishCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(parent), exportFinishTimeout)
	}
	// end writes a final state. recorded reports whether the row now says what
	// this attempt did; a write that matched nothing is not recorded.
	end := func(write func(context.Context) (Export, bool, error)) (recorded, ambiguous bool) {
		ctx, cancel := finishCtx()
		defer cancel()
		done, ok, err := write(ctx)
		switch {
		case err != nil:
			log.Error("account export: the final state could not be written; the take-over will end it", "error", err)
			return false, true
		case !ok:
			log.Warn("account export: this node no longer holds the run, so it writes nothing")
			return false, false
		default:
			s.finished(ctx, done)
			return true, false
		}
	}

	passphrase := ""
	if sealed != "" {
		p, err := s.open(sealed)
		if err != nil {
			// The opening error names the key's state, never the plaintext.
			end(func(ctx context.Context) (Export, bool, error) {
				return s.finishFailed(ctx, x, node, FailureFailed, "The passphrase could not be opened on this node.")
			})
			return
		}
		passphrase = p
	}

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	beatDone := r.beat(ctx, cancel, x, node, log)

	var progressMu sync.Mutex
	var lastWrite time.Time
	lastPhase := ""
	progress := func(p ExportProgress) {
		// At most a write a second within a phase, since a work function may
		// report per row; a new phase is written at once.
		progressMu.Lock()
		if p.Phase == lastPhase && time.Since(lastWrite) < time.Second {
			progressMu.Unlock()
			return
		}
		lastWrite, lastPhase = time.Now(), p.Phase
		progressMu.Unlock()
		if err := s.writeProgress(ctx, x, node, p); err != nil && ctx.Err() == nil {
			log.Warn("account export: progress not recorded", "error", err)
		}
	}

	result, err := work(ctx, x, passphrase, progress)
	cause := context.Cause(ctx)
	cancel(nil)
	<-beatDone

	// A result the work returned is the runner's to record or to remove.
	produced := err == nil && result.ObjectKey != ""
	discardUnless := func(recorded, ambiguous bool) {
		if !produced || recorded {
			return
		}
		if ambiguous {
			log.Error("account export: the file may not be recorded; it is kept so a record that did land does not lose it",
				"objectKey", result.ObjectKey)
			return
		}
		r.discard(parent, x, result.ObjectKey, log)
	}

	switch {
	case errors.Is(cause, errExportLeaseLost) || errors.Is(cause, errExportFenced):
		log.Warn("account export: this node no longer holds the run; this attempt ends here")
		discardUnless(false, false)
	case errors.Is(cause, errExportCancelRequested) || r.cancelRequested(x, node):
		_, ambiguous := end(func(ctx context.Context) (Export, bool, error) { return s.finishCancelled(ctx, x, node) })
		discardUnless(false, ambiguous)
	case parent.Err() != nil:
		// The node is stopping. Hand the attempt back for another node.
		ctx, cancelFinish := finishCtx()
		defer cancelFinish()
		if ok, err := s.requeue(ctx, x, node); err != nil {
			log.Warn("account export: could not hand the run back; the take-over will", "error", err)
		} else if ok {
			log.Info("account export: the node is stopping; the run is handed back")
		}
		discardUnless(false, false)
	case err == nil:
		discardUnless(end(func(ctx context.Context) (Export, bool, error) { return s.finishComplete(ctx, x, node, result) }))
	default:
		code, detail := FailureFailed, err.Error()
		var failure *ExportFailure
		if errors.As(err, &failure) && knownFailure(failure.Code) {
			code, detail = failure.Code, failure.Detail
		}
		detail = scrub(detail, passphrase)
		log.Warn("account export: failed", "failureCode", code, "error", detail)
		end(func(ctx context.Context) (Export, bool, error) { return s.finishFailed(ctx, x, node, code, detail) })
	}
}

// discard removes the object of a result nothing records, through the blob
// store Remove uses. A delete that fails is logged at error level with the
// export, the attempt and the key, so the file can be found and removed.
func (r *ExportRunner) discard(parent context.Context, x Export, key string, log interface {
	Error(string, ...any)
	Info(string, ...any)
}) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), exportFinishTimeout)
	defer cancel()
	blobs, err := r.exports.blobStore()
	if err == nil {
		err = blobs.Delete(ctx, key)
	}
	if err != nil {
		log.Error("account export: a file this node could not record is left in storage; remove it by hand",
			"objectKey", key, "error", err)
		return
	}
	log.Info("account export: removed a file this node could not record", "objectKey", key)
}

// cancelRequested asks the record once more, for a cancel that landed after
// the last heartbeat while the work was finishing on its own.
func (r *ExportRunner) cancelRequested(x Export, node string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), exportFinishTimeout)
	defer cancel()
	requested, ok, err := r.exports.heartbeat(ctx, x, node)
	return err == nil && ok && requested
}

func knownFailure(code string) bool {
	switch code {
	case FailureNotReady, FailureRowsWaiting, FailureCensusMismatch, FailureVerifyFailed, FailureRowsTooSlow, FailureFailed:
		return true
	}
	return false
}

// scrub takes the passphrase out of a work function's error text before it is
// logged or stored. The work function is not supposed to put it there; this
// is what makes "never in a log line or the record" a property of the runner
// rather than of every work function.
func scrub(text, passphrase string) string {
	if passphrase == "" {
		return text
	}
	return strings.ReplaceAll(text, passphrase, "[passphrase]")
}
