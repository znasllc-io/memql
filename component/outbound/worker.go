package outbound

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

const (
	// WorkerComponent is the Dependency ComponentName.
	WorkerComponent = common.ComponentName("outbound.worker")

	// deliveryClaimName is the cross-replica claim namespace: rows land
	// in the same automation_execution_claims ledger the automation
	// cluster guard uses. Keyed per (row id, attempts) so a retry is a
	// fresh claim but two replicas never run the same attempt.
	deliveryClaimName = "outboundDelivery"

	// systemOutboundActor stamps engine roundtrips from this worker
	// (planner systemActorContext precedent).
	systemOutboundActor = "system:outbound"

	// lastErrorCap bounds the persisted lastError (observability
	// errorMessage precedent).
	lastErrorCap = 4096

	// Backoff schedule (ADR 4.1): base 30s, factor 4, cap 1h, +-20%
	// jitter. Attempt 1 -> ~30s, 2 -> ~2m, 3 -> ~8m, 4 -> ~32m, 5+ -> 1h.
	backoffBase   = 30 * time.Second
	backoffFactor = 4
	backoffCap    = time.Hour
)

// Engine is the narrow engine surface the worker needs. Returns any
// (not *memql.ExecuteResult) so tests fake it with flat row envelopes;
// app wiring passes an adapter over MemQLEngine (planner precedent).
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// ExecutionClaimer is the cross-replica claim gate (the automations
// ClusterExecutionGuard satisfies it). Nil leaves single-replica
// deployments unguarded-but-correct: the poll is the only driver.
type ExecutionClaimer interface {
	Claim(ctx context.Context, name, dedupKey string) bool
}

// Worker drains v1:platform:outboundRequest rows and delivers them
// (memql#2521). Fast path: the graph.node.created event for the concept
// wakes the drain immediately on the replica that executed the staging
// mutation. Safety net: the poll picks up cross-replica rows, due
// retries, and dropped events. The ClusterExecutionGuard claim keeps
// delivery at-least-once but single-runner per (row, attempt).
type Worker struct {
	engine     Engine
	claimer    ExecutionClaimer
	bus        *events.Bus
	transports map[string]Transport
	logger     *slog.Logger
	cfg        Config

	now  func() time.Time // test seam
	wake chan struct{}

	cancel      context.CancelFunc
	unsubscribe func()
	running     atomic.Bool
	startOnce   sync.Once
	stopOnce    sync.Once
	readyCh     chan struct{}
	doneCh      chan struct{}
	mu          sync.Mutex
}

// NewWorker constructs the drain worker. transports maps medium ->
// Transport; app wiring passes the email and webhook transports. bus
// and claimer are optional (nil disables the fast path / the
// cross-replica gate respectively).
func NewWorker(engine Engine, claimer ExecutionClaimer, bus *events.Bus, transports map[string]Transport, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		engine:     engine,
		claimer:    claimer,
		bus:        bus,
		transports: transports,
		logger:     logger.With("component", string(WorkerComponent)),
		cfg:        LoadConfig(),
		now:        time.Now,
		wake:       make(chan struct{}, 1),
		readyCh:    make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Start launches the drain loop and the event fast-path subscription.
// Idempotent. A no-op when disabled or the engine is missing.
func (w *Worker) Start(_ context.Context) {
	w.startOnce.Do(func() {
		defer close(w.readyCh)
		if !w.cfg.Enabled {
			w.logger.Info("outbound worker: disabled (MEMQL_OUTBOUND_ENABLED=false)")
			close(w.doneCh)
			return
		}
		if w.engine == nil {
			w.logger.Warn("outbound worker: no engine wired; not starting")
			close(w.doneCh)
			return
		}
		runCtx, cancel := context.WithCancel(context.Background())
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		if w.bus != nil {
			// Concept events for @scope-global platform concepts fire on
			// the canonical id; wake the drain so a staged row on THIS
			// replica delivers without waiting out the poll interval.
			w.unsubscribe = w.bus.Subscribe(
				"graph.node.created.v1:platform:outboundRequest",
				func(events.Event) { w.wakeDrain() },
				events.WithSubscriberName(string(WorkerComponent)),
			)
		}
		w.running.Store(true)
		go w.loop(runCtx)
	})
}

// Stop cancels the loop and the subscription.
func (w *Worker) Stop(_ context.Context) {
	w.stopOnce.Do(func() {
		if w.unsubscribe != nil {
			w.unsubscribe()
		}
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
func (w *Worker) IsRunning() bool                     { return w.running.Load() }
func (w *Worker) Order() int                          { return 11 }
func (w *Worker) ComponentName() common.ComponentName { return WorkerComponent }
func (w *Worker) Ready() <-chan struct{}              { return w.readyCh }

func (w *Worker) wakeDrain() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.doneCh)
	w.logger.Info("outbound worker: started",
		"pollInterval", w.cfg.Poll.String(),
		"emailAllowlist", len(w.cfg.EmailAllowlist),
		"webhookAllowlist", len(w.cfg.WebhookAllowlist))
	startup := time.NewTimer(w.cfg.StartupDelay)
	defer startup.Stop()
	select {
	case <-ctx.Done():
		return
	case <-startup.C:
	}
	ticker := time.NewTicker(w.cfg.Poll)
	defer ticker.Stop()
	w.drainOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("outbound worker: stopped")
			return
		case <-ticker.C:
			w.drainOnce(ctx)
		case <-w.wake:
			w.drainOnce(ctx)
		}
	}
}

// drainOnce scans pending + due retrying rows and processes each. The
// synchronously testable unit (cron-leader test precedent).
func (w *Worker) drainOnce(ctx context.Context) {
	sysCtx := systemActorContext(ctx)
	for _, status := range []string{"pending", "retrying"} {
		res, err := w.engine.Execute(sysCtx, fmt.Sprintf(`query outboundRequestsByStatus(status: %q)`, status))
		if err != nil {
			w.logger.Debug("outbound worker: scan failed (engine likely not ready)", "status", status, "error", err)
			return
		}
		for _, row := range memql.MaterializeRows(res) {
			w.processRow(sysCtx, row, status)
		}
	}
}

// processRow validates, claims, and delivers one row, stamping the
// resulting status transition.
func (w *Worker) processRow(ctx context.Context, row map[string]any, scanStatus string) {
	req := requestFromRow(row)
	if req.ID == "" {
		return
	}
	now := w.now().UTC()
	if scanStatus == "retrying" {
		due := parseTimeOrZero(getString(row, "nextAttemptAt"))
		// An unparseable/missing due-time is treated as NOT due: leave
		// the row for inspection rather than hot-looping deliveries.
		if due.IsZero() || now.Before(due) {
			return
		}
	}
	// Per-node egress gate BEFORE the claim (memql#2540). The worker runs
	// on every node, but the per-medium allowlist is per-deployment env on
	// specific node types. A node not configured for the row's medium must
	// leave the row pending -- no claim, no stamp -- so it cannot win the
	// claim race and stamp failed a row a configured peer could deliver.
	// Only configured nodes contend; the guard elects one that can actually
	// deliver. Target-allowlist and payload-cap checks stay POST-claim (in
	// admit) so a configured node still fails a genuinely bad row loudly.
	if !w.mediumEnabledHere(req.Medium) {
		return
	}
	// Cross-replica claim BEFORE any side effect. Keyed per (row,
	// attempts) so the winning replica of THIS attempt is unique, while
	// a later retry (attempts incremented) claims fresh.
	if w.claimer != nil {
		if !w.claimer.Claim(ctx, deliveryClaimName, fmt.Sprintf("%s:%d", req.ID, req.Attempts)) {
			return
		}
	}
	transport, policyErr := w.admit(req)
	if policyErr != nil {
		w.stampFailed(ctx, req, policyErr)
		return
	}
	w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %q, status: "sending")`, req.ID))
	err := transport.Deliver(ctx, req)
	if err == nil {
		w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %q, status: "sent", lastError: "", sentAt: %q)`,
			req.ID, now.Format(time.RFC3339)))
		w.logger.Info("outbound worker: delivered", "id", req.ID, "medium", req.Medium, "attempt", req.Attempts+1)
		return
	}
	attempts := req.Attempts + 1
	if IsPermanent(err) || attempts >= w.cfg.MaxAttempts {
		w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %q, status: "failed", attempts: %d, lastError: %q)`,
			req.ID, attempts, truncateError(err)))
		w.logger.Warn("outbound worker: delivery failed permanently",
			"id", req.ID, "medium", req.Medium, "attempts", attempts, "error", err)
		return
	}
	next := now.Add(backoffFor(attempts))
	w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %q, status: "retrying", attempts: %d, lastError: %q, nextAttemptAt: %q)`,
		req.ID, attempts, truncateError(err), next.Format(time.RFC3339)))
	w.logger.Warn("outbound worker: delivery failed; will retry",
		"id", req.ID, "medium", req.Medium, "attempts", attempts, "nextAttemptAt", next.Format(time.RFC3339), "error", err)
}

// mediumEnabledHere reports whether THIS node is configured to egress the
// row's medium: a non-empty per-deployment allowlist for it. This is the
// pre-claim gate (memql#2540): the worker is registered on every node, but
// the allowlist env (MEMQL_OUTBOUND_*_ALLOWLIST) is set only on the node
// types that own egress. A node with no allowlist for the medium skips the
// row entirely (leaves it pending), so it never wins the claim and stamps
// failed a row a configured peer could deliver. Trade-off: a medium
// configured on NO node leaves its rows pending rather than failing loud;
// the operator resolves it by setting the allowlist (docs:
// operate/outbound-delivery.md). An unknown medium is enabled nowhere and
// so cannot be config-fixed; it falls through to the post-claim admit gate,
// where exactly one node stamps it failed loudly.
func (w *Worker) mediumEnabledHere(medium string) bool {
	switch medium {
	case "email":
		return len(w.cfg.EmailAllowlist) > 0
	case "webhook":
		return len(w.cfg.WebhookAllowlist) > 0
	default:
		return true
	}
}

// admit applies the deploy-layer policy (ADR 4.3): known medium with a
// wired transport, allowlisted target, bounded payload. A policy miss
// is a permanent failure -- loud, never a silent backlog.
func (w *Worker) admit(req Request) (Transport, error) {
	transport, ok := w.transports[req.Medium]
	if !ok || transport == nil {
		return nil, fmt.Errorf("medium %q has no transport wired", req.Medium)
	}
	if len(req.Payload) > w.cfg.MaxPayloadBytes {
		return nil, fmt.Errorf("payload %d bytes exceeds cap %d", len(req.Payload), w.cfg.MaxPayloadBytes)
	}
	switch req.Medium {
	case "email":
		if len(w.cfg.EmailAllowlist) == 0 {
			return nil, fmt.Errorf("medium disabled by deployment config (MEMQL_OUTBOUND_EMAIL_ALLOWLIST empty)")
		}
		if !emailAllowed(req.Target, w.cfg.EmailAllowlist) {
			return nil, fmt.Errorf("target %q not in email allowlist", req.Target)
		}
	case "webhook":
		if len(w.cfg.WebhookAllowlist) == 0 {
			return nil, fmt.Errorf("medium disabled by deployment config (MEMQL_OUTBOUND_WEBHOOK_ALLOWLIST empty)")
		}
		if !webhookAllowed(req.Target, w.cfg.WebhookAllowlist) {
			return nil, fmt.Errorf("target %q not in webhook allowlist", req.Target)
		}
	default:
		return nil, fmt.Errorf("unknown medium %q", req.Medium)
	}
	return transport, nil
}

func (w *Worker) stampFailed(ctx context.Context, req Request, policyErr error) {
	w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %q, status: "failed", attempts: %d, lastError: %q)`,
		req.ID, req.Attempts, truncateError(policyErr)))
	w.logger.Warn("outbound worker: row refused by policy", "id", req.ID, "medium", req.Medium, "error", policyErr)
}

func (w *Worker) stamp(ctx context.Context, mutation string) {
	if _, err := w.engine.Execute(ctx, mutation); err != nil {
		w.logger.Warn("outbound worker: status stamp failed", "error", err)
	}
}

// backoffFor returns the delay before the given (1-based) attempt's
// retry: bounded exponential with +-20% jitter (ADR 4.1).
func backoffFor(attempt int) time.Duration {
	d := backoffBase
	for i := 1; i < attempt; i++ {
		d *= backoffFactor
		if d >= backoffCap {
			d = backoffCap
			break
		}
	}
	jitter := 1 + (rand.Float64()*0.4 - 0.2)
	j := time.Duration(float64(d) * jitter)
	if j > backoffCap {
		j = backoffCap
	}
	return j
}

func requestFromRow(row map[string]any) Request {
	return Request{
		ID:        getString(row, "id"),
		Medium:    getString(row, "medium"),
		Target:    getString(row, "target"),
		Subject:   getString(row, "subject"),
		Payload:   getString(row, "body"),
		DedupeKey: getString(row, "dedupeKey"),
		Attempts:  getInt(row, "attempts"),
	}
}

func truncateError(err error) string {
	msg := err.Error()
	if len(msg) > lastErrorCap {
		msg = msg[:lastErrorCap]
	}
	return msg
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	default:
		return 0
	}
}

func parseTimeOrZero(s string) time.Time {
	if strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999 -0700 MST"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// systemActorContext stamps engine roundtrips from the worker with a
// system identity (planner precedent).
func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: systemOutboundActor,
		Claims: map[string]any{
			"sub":  systemOutboundActor,
			"role": "system",
		},
	})
}
