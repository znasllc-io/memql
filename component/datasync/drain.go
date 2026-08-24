package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/core/common"
)

// drain.go -- the outbox drain worker (epic memql#4378, D5).
//
// One worker per node drives every BOUND connector. For each it claims
// the right to drain, reads that connector's pending entries oldest
// first, and delivers each through Connector.Propagate with the entry's
// stored idempotency key.
//
// # Oldest first, and why it is per row rather than global
//
// Entries are delivered in creation order, which for one row means its
// changes arrive at the mirror in the order they were made. Across
// DIFFERENT rows the order does not matter and is not promised -- a
// per-row guarantee is what a receiver needs, and a global one would
// serialize the whole connector behind its slowest delivery.
//
// # Every outcome is terminal-or-scheduled, never silent
//
//	delivered  the receiver has it (including "already had it", which the
//	           idempotency key is for and which counts as success)
//	failed     retry scheduled at nextAttemptAt, backing off
//	dead       attempts exhausted; an operator's problem now, and never
//	           picked up again automatically
//
// There is deliberately no fourth outcome and no path that leaves an
// entry `delivering`. A worker that dies mid-delivery leaves one behind,
// and the next pass reclaims it because the attempt was already counted
// at claim time -- so a crash-looping delivery burns its attempts and
// dead-letters instead of spinning forever.
//
// # A capability a connector does not implement is not a failure
//
// Propagate returning the contract's typed not-implemented error means
// this connector does not do outbound delivery yet. That is a
// configuration fact, not a delivery failure: the entry is left pending,
// the domain's health records it, and nothing is dead-lettered. A
// connector filling in one direction at a time -- which is where Shopify
// starts -- would otherwise dead-letter every entry it was ever handed.

// ExecutionClaimer is the cross-replica claim gate (the automations
// ClusterExecutionGuard satisfies it).
//
// Nil leaves a single-replica deployment correct -- the poll is the only
// driver -- but MUST be wired in the mesh: without it two replicas drain
// the same connector concurrently, and the only thing between that and a
// double delivery is the receiver honouring the idempotency key, which
// is a promise about somebody else's system.
type ExecutionClaimer interface {
	ClaimWithTTL(ctx context.Context, name, dedupKey string, ttl time.Duration) bool
}

// Worker drains v1:platform:outboxEntry rows.
type Worker struct {
	store   *Store
	claimer ExecutionClaimer
	cfg     Config
	logger  *slog.Logger
	now     func() time.Time

	// lookup resolves a connector name to its bound implementation.
	// Injected so tests drive the worker without the process-global
	// registry, and so the registry stays a boot-time concern.
	lookup func(name string) (memqlsync.Connector, bool)
	// bound lists the connectors this build has implementations for.
	bound func() []string

	cancel    context.CancelFunc
	running   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once
	readyCh   chan struct{}
	doneCh    chan struct{}
	mu        sync.Mutex

	deliveredTotal atomic.Int64
	failedTotal    atomic.Int64
	deadTotal      atomic.Int64
}

// NewWorker constructs the drain worker.
func NewWorker(engine Engine, claimer ExecutionClaimer, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		store:   NewStore(engine),
		claimer: claimer,
		cfg:     LoadConfig(),
		logger:  logger.With("component", "datasync.drain"),
		now:     time.Now,
		lookup:  memqlsync.Lookup,
		bound:   memqlsync.BoundNames,
		readyCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start launches the drain loop. Idempotent; a no-op when disabled or
// unwired.
func (w *Worker) Start(_ context.Context) {
	w.startOnce.Do(func() {
		defer close(w.readyCh)
		if !w.cfg.Enabled {
			w.logger.Info("datasync drain: disabled (" + envEnabled + "=false); outbox entries are still APPENDED, just not delivered")
			close(w.doneCh)
			return
		}
		if w.store == nil || w.store.engine == nil {
			w.logger.Warn("datasync drain: no engine wired; not starting")
			close(w.doneCh)
			return
		}
		if w.claimer == nil {
			// Loud, once. A single-replica deployment is correct without
			// it; a mesh is not, and the symptom of the missing wire is a
			// DOUBLE DELIVERY at somebody else's system, which is the
			// hardest kind of bug to see from here.
			w.logger.Warn("datasync drain: no cluster execution claimer wired -- correct on a single replica, " +
				"but in a mesh two replicas will drain the same connector concurrently")
		}
		runCtx, cancel := context.WithCancel(context.Background())
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		w.running.Store(true)
		go w.loop(runCtx)
	})
}

// Stop cancels the loop.
func (w *Worker) Stop(_ context.Context) {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		cancel := w.cancel
		w.cancel = nil
		w.mu.Unlock()
		if cancel != nil {
			cancel()
			<-w.doneCh
		}
		w.running.Store(false)
	})
}

// Dependency surface.
//
// Order is 13, immediately after the campaigns worker's 12: both are
// drain loops over engine rows and neither can do anything useful until
// the engine and its integrations are up, so they sit together at the
// tail of the startup order rather than being interleaved with the
// components they depend on.
func (w *Worker) IsRunning() bool                     { return w.running.Load() }
func (w *Worker) Order() int                          { return 13 }
func (w *Worker) ComponentName() common.ComponentName { return WorkerComponent }
func (w *Worker) Ready() <-chan struct{}              { return w.readyCh }

// WorkerComponent is the drain worker's component name.
const WorkerComponent = common.ComponentName("datasync.drain")

// Counters returns delivered / failed / dead totals for this process.
func (w *Worker) Counters() (delivered, failed, dead int64) {
	return w.deliveredTotal.Load(), w.failedTotal.Load(), w.deadTotal.Load()
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.doneCh)
	w.logger.Info("datasync drain: started",
		"poll", w.cfg.Poll.String(), "batchSize", w.cfg.BatchSize, "maxAttempts", w.cfg.MaxAttempts)

	startup := time.NewTimer(w.cfg.StartupDelay)
	defer startup.Stop()
	select {
	case <-ctx.Done():
		return
	case <-startup.C:
	}

	ticker := time.NewTicker(w.cfg.Poll)
	defer ticker.Stop()
	for {
		w.DrainOnce(ctx)
		select {
		case <-ctx.Done():
			w.logger.Info("datasync drain: stopped")
			return
		case <-ticker.C:
		}
	}
}

// DrainOnce runs one pass over every bound connector. Exported so a test
// -- and an operator tool -- can drive a single pass deterministically
// instead of racing the ticker.
func (w *Worker) DrainOnce(ctx context.Context) {
	for _, name := range w.bound() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		w.drainConnector(ctx, name)
	}
}

func (w *Worker) drainConnector(ctx context.Context, name string) {
	connector, ok := w.lookup(name)
	if !ok || connector == nil {
		return
	}
	// One replica per connector. The claim is per NAME rather than per
	// entry: claiming per entry would let two replicas interleave a row's
	// changes and deliver them out of order, which is the one ordering
	// guarantee this worker makes.
	if w.claimer != nil && !w.claimer.ClaimWithTTL(ctx, "datasync.drain", name, w.cfg.ClaimTTL) {
		return
	}

	opCtx := OperatorContext(ctx)
	entries, err := w.store.PendingOutbox(opCtx, name)
	if err != nil {
		w.logger.Warn("datasync drain: reading the queue failed", "connector", name, "error", err)
		return
	}

	now := w.now().UTC()
	attempted := 0
	for _, entry := range entries {
		if attempted >= w.cfg.BatchSize {
			break
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !entry.Due(now) {
			continue
		}
		attempted++
		w.deliver(ctx, opCtx, connector, entry)
	}
}

// deliver makes one delivery attempt and records its outcome.
func (w *Worker) deliver(ctx context.Context, opCtx context.Context, connector memqlsync.Connector, entry OutboxEntry) {
	attempts := entry.Attempts + 1

	// Counted BEFORE the attempt. See the file header: a worker that dies
	// mid-delivery must still spend one of the entry's lives, or a
	// crash-looping delivery never reaches the ceiling.
	if err := w.store.MarkDelivering(opCtx, entry.ID, attempts); err != nil {
		w.logger.Warn("datasync drain: claiming the entry failed", "entry", entry.ID, "error", err)
		return
	}

	result, err := connector.Propagate(ctx, entry.toContractEntry())
	switch {
	case err == nil:
		if markErr := w.store.MarkDelivered(opCtx, entry.ID, w.now().UTC()); markErr != nil {
			w.logger.Warn("datasync drain: recording the delivery failed", "entry", entry.ID, "error", markErr)
			return
		}
		w.deliveredTotal.Add(1)
		w.logger.Info("datasync drain: delivered",
			"connector", connector.Name(), "concept", entry.ConceptID, "row", entry.RowRef,
			"action", entry.Action, "attempts", attempts,
			"alreadyDelivered", result.AlreadyDelivered, "externalId", result.ExternalId)

	case memqlsync.IsNotImplemented(err):
		// NOT a delivery failure. The connector does not do outbound work
		// yet, so the entry is parked AND ITS ATTEMPT COUNT IS PUT BACK.
		//
		// Restoring the count is the half that is easy to leave out and
		// expensive to leave out. The claim above already incremented it,
		// and without this the counter creeps by one per poll against a
		// capability nobody has written -- so an entry sits at the ceiling
		// by the time the connector finally implements Propagate, and the
		// first transient failure after that dead-letters it immediately.
		// The ceiling is meant to bound FAILING DELIVERIES, not waiting.
		if restoreErr := w.store.MarkDelivering(opCtx, entry.ID, entry.Attempts); restoreErr != nil {
			w.logger.Warn("datasync drain: restoring the attempt count failed", "entry", entry.ID, "error", restoreErr)
		}
		if markErr := w.store.MarkFailed(opCtx, entry.ID, w.now().UTC().Add(w.cfg.BackoffMax), err.Error()); markErr != nil {
			w.logger.Warn("datasync drain: parking the entry failed", "entry", entry.ID, "error", markErr)
		}
		w.logger.Info("datasync drain: connector does not implement outbound delivery; entries are parked, not dead-lettered",
			"connector", connector.Name(), "entry", entry.ID)

	case memqlsync.IsPermanent(err):
		// A failure no retry can fix -- a receiver's validation refusal,
		// which arrives identically every time. Dead-lettered NOW rather
		// than at the attempt ceiling.
		//
		// Spending the budget on it is not merely wasteful: the entries
		// behind it wait through every backoff, so one malformed payload
		// slows the whole queue for a connector, and the dead-letter that
		// eventually lands says "exhausted attempts" rather than what the
		// receiver actually objected to. The connector knows which it is
		// and says so; the drain honours it.
		if markErr := w.store.MarkDead(opCtx, entry.ID, err.Error()); markErr != nil {
			w.logger.Warn("datasync drain: dead-lettering failed", "entry", entry.ID, "error", markErr)
			return
		}
		w.deadTotal.Add(1)
		w.logger.Warn("datasync drain: DEAD-LETTERED on a permanent failure -- an operator has to decide",
			"connector", connector.Name(), "concept", entry.ConceptID, "row", entry.RowRef,
			"attempts", attempts, "error", err)

	case attempts >= w.cfg.MaxAttempts:
		if markErr := w.store.MarkDead(opCtx, entry.ID, err.Error()); markErr != nil {
			w.logger.Warn("datasync drain: dead-lettering failed", "entry", entry.ID, "error", markErr)
			return
		}
		w.deadTotal.Add(1)
		w.logger.Warn("datasync drain: DEAD-LETTERED after exhausting attempts -- an operator has to decide",
			"connector", connector.Name(), "concept", entry.ConceptID, "row", entry.RowRef,
			"attempts", attempts, "max", w.cfg.MaxAttempts, "error", err)

	default:
		wait := w.cfg.backoffFor(attempts)
		if result.RetryAfter > 0 {
			// The receiver told us when to come back. Honour it over our
			// own schedule when it asks for longer: a 429 with a
			// Retry-After is a rate limit, and retrying sooner than asked
			// is how a temporary limit becomes a permanent one.
			if result.RetryAfter > wait {
				wait = result.RetryAfter
			}
		}
		if markErr := w.store.MarkFailed(opCtx, entry.ID, w.now().UTC().Add(wait), err.Error()); markErr != nil {
			w.logger.Warn("datasync drain: recording the failure failed", "entry", entry.ID, "error", markErr)
			return
		}
		w.failedTotal.Add(1)
		w.logger.Warn("datasync drain: delivery failed; retry scheduled",
			"connector", connector.Name(), "concept", entry.ConceptID, "row", entry.RowRef,
			"attempts", attempts, "retryIn", wait.String(), "error", err)
	}
}

// toContractEntry projects a stored row onto the value a Connector sees.
//
// The payload is deliberately NOT carried: the drain worker reads the
// queue, not the row, and a payload snapshotted at append time would be
// stale by the time a retried delivery went out. A connector that needs
// the current row reads it -- under its own actor, which row admission
// admits to exactly the concepts that name it.
func (e OutboxEntry) toContractEntry() memqlsync.OutboxEntry {
	return memqlsync.OutboxEntry{
		Id:             e.ID,
		Concept:        e.ConceptID,
		RowId:          e.RowRef,
		Action:         memqlsync.OutboxAction(e.Action),
		Version:        e.Version,
		Target:         e.Target,
		IdempotencyKey: e.IdempotencyKey,
		Attempts:       e.Attempts,
	}
}

// DecodePayload is a convenience for a connector that stores a payload
// snapshot of its own; the runtime does not use it.
func DecodePayload(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("datasync: decoding a payload: %w", err)
	}
	return out, nil
}

// ConnectorNames returns the bound connector names, for a status
// surface.
func (w *Worker) ConnectorNames() []string {
	if w == nil || w.bound == nil {
		return nil
	}
	names := w.bound()
	out := make([]string, 0, len(names))
	for _, n := range names {
		if s := strings.TrimSpace(n); s != "" {
			out = append(out, s)
		}
	}
	return out
}
