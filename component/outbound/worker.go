package outbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/num"
)

const (
	// WorkerComponent is the Dependency ComponentName.
	WorkerComponent = common.ComponentName("outbound.worker")

	// deliveryClaimName is the cross-replica claim namespace: rows land
	// in the same automation_execution_claims ledger the automation
	// cluster guard uses. Keyed per (row id, attempts) so a retry is a
	// fresh claim but two live replicas never run the same attempt. The
	// claim carries an attempt-scoped lease (Config.ClaimTTL, memql#2548):
	// a claim orphaned by a dead claimant expires after the TTL so a peer
	// can re-claim and recover the row.
	deliveryClaimName = "outboundDelivery"

	// lastErrorCap bounds the persisted lastError (observability
	// errorMessage precedent).
	lastErrorCap = 4096

	// Backoff schedule (ADR 4.1): base 30s, factor 4, cap 1h, +-20%
	// jitter. Attempt 1 -> ~30s, 2 -> ~2m, 3 -> ~8m, 4 -> ~32m, 5+ -> 1h.
	backoffBase   = 30 * time.Second
	backoffFactor = 4
	backoffCap    = time.Hour

	// SecretNamePattern is the form a secret target's NAME must take
	// (memql#5480). The engine's resolver interpolates the name into its
	// lookup query, so the worker refuses any other name before it resolves
	// anything. stageOutboundRequestToSecret's targetSecret arg carries the
	// same @pattern, and TestOutboundSecretNamePatternMatchesTheDSL at the
	// repository root holds the two together.
	SecretNamePattern = `^[A-Z][A-Z0-9_]{0,63}$`
)

// Engine is the narrow engine surface the worker needs. Returns any
// (not *memql.ExecuteResult) so tests fake it with flat row envelopes;
// app wiring passes an adapter over MemQLEngine (planner precedent).
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// ExecutionClaimer is the cross-replica claim gate (the automations
// ClusterExecutionGuard satisfies it). Nil leaves single-replica
// deployments unguarded-but-correct: the poll is the only driver. The
// worker uses the attempt-scoped-lease variant (ClaimWithTTL, memql#2548)
// so a claim orphaned by a replica that died before stamping becomes
// re-winnable after cfg.ClaimTTL, instead of wedging the row until the
// guard's 1h retention prune. Automation/planner callers keep the
// permanent-within-retention Claim; only this worker opts into the lease.
type ExecutionClaimer interface {
	ClaimWithTTL(ctx context.Context, name, dedupKey string, ttl time.Duration) bool
}

// Worker drains v1:platform:outboundRequest rows and delivers them
// (memql#2521). Fast path: the graph.node.created event for the concept
// wakes the drain immediately on the replica that executed the staging
// mutation. Safety net: the poll picks up cross-replica rows, due
// retries, and dropped events. The ClusterExecutionGuard claim keeps
// delivery at-least-once but single-runner per (row, attempt) for the life
// of the claim's attempt-scoped lease (Config.ClaimTTL); past it a claim
// orphaned by a dead claimant is re-winnable so the row is never wedged
// (memql#2548).
type Worker struct {
	// Secrets resolves a v1:platform:globalSecret by NAME to its plaintext
	// value (memql#5480). A webhook row whose URL is a credential -- a
	// Discord webhook carries its token in the path -- names the secret in
	// targetSecret instead of carrying the URL, and the worker reads the
	// value here at send time. Set by app wiring after NewWorker; nil on a
	// node that wired none, where such a row fails naming its secret.
	Secrets func(ctx context.Context, name string) (string, error)

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
	sysCtx := SystemActorContext(ctx)
	for _, status := range []string{"pending", "retrying"} {
		res, err := w.engine.Execute(sysCtx, fmt.Sprintf(`query outboundRequestsByStatus(status: %s)`, langparser.QuoteString(status)))
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
	// a later retry (attempts incremented) claims fresh. The claim carries
	// an attempt-scoped lease (cfg.ClaimTTL, memql#2548): if this replica
	// dies after winning the claim but before stamping a terminal status,
	// the row stays pending and the persisted claim expires after the TTL
	// so a peer re-claims and recovers it -- rather than every peer
	// re-claiming-and-losing the key until the guard's 1h retention prune.
	if w.claimer != nil {
		if !w.claimer.ClaimWithTTL(ctx, deliveryClaimName, fmt.Sprintf("%s:%d", req.ID, req.Attempts), w.cfg.ClaimTTL) {
			return
		}
	}
	transport, delivery, admitErr := w.admit(ctx, req)
	if admitErr != nil {
		if isRetryLater(admitErr) {
			// Nothing was sent, but the attempt is spent: the row takes the
			// delivery backoff and counts against MaxAttempts, so a failure
			// that never clears still ends the row (ADR 4.1).
			w.recordFailure(ctx, req, admitErr, now)
			return
		}
		w.stampFailed(ctx, req, admitErr)
		return
	}
	w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "sending")`, langparser.QuoteString(req.ID)))
	// delivery goes to the transport and nowhere else: for a secret target
	// (memql#5480) it is the one value holding the resolved URL. Every stamp
	// and log line in this function reads req, whose Target is the
	// descriptor secret:<NAME>.
	err := transport.Deliver(ctx, delivery)
	if err == nil {
		w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "sent", lastError: "", sentAt: %s)`,
			langparser.QuoteString(req.ID), langparser.QuoteString(now.Format(time.RFC3339))))
		w.logger.Info("outbound worker: delivered", "id", req.ID, "medium", req.Medium, "attempt", req.Attempts+1)
		return
	}
	w.recordFailure(ctx, req, err, now)
}

// recordFailure stamps a failed attempt: failed when err is permanent or the
// attempts are spent, retrying on the bounded backoff otherwise (ADR 4.1).
func (w *Worker) recordFailure(ctx context.Context, req Request, err error, now time.Time) {
	attempts := req.Attempts + 1
	if IsPermanent(err) || attempts >= w.cfg.MaxAttempts {
		// lastError is rendered with langparser.QuoteString, NOT %q
		// (memql#3035). This text can carry bytes from a remote server -- a
		// webhook response excerpt, a TLS or DNS error naming a hostile
		// hostname -- and %q emits `\x00`, `\a` and `\v`, which the MemQL
		// lexer refuses outright rather than falling back. A single control
		// byte made this statement unparseable, stamp() logged a warning and
		// returned, and the row kept its previous status: a request mid-
		// delivery stayed `sending` forever, never retried and never reported
		// failed, with no error recorded because recording it was what failed.
		w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "failed", attempts: %d, lastError: %s)`,
			langparser.QuoteString(req.ID), attempts, langparser.QuoteString(truncateError(err))))
		w.logger.Warn("outbound worker: delivery failed permanently",
			"id", req.ID, "medium", req.Medium, "attempts", attempts, "error", err)
		return
	}
	next := now.Add(backoffFor(attempts))
	// lastError via QuoteString, same reason as the failed stamp above
	// (memql#3035). This one is worse if it breaks: the row never reaches
	// `retrying`, so it is never picked up again.
	w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "retrying", attempts: %d, lastError: %s, nextAttemptAt: %s)`,
		langparser.QuoteString(req.ID), attempts, langparser.QuoteString(truncateError(err)), langparser.QuoteString(next.Format(time.RFC3339))))
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
//
// It also returns the request the transport is to deliver. That is req
// itself, except for a secret target (memql#5480), where it is a copy whose
// Target is the URL the named secret holds. The caller keeps req for every
// stamp and log line, so the row, its audit and its errors carry the
// descriptor secret:<NAME> and never the URL.
func (w *Worker) admit(ctx context.Context, req Request) (Transport, Request, error) {
	transport, ok := w.transports[req.Medium]
	if !ok || transport == nil {
		return nil, req, fmt.Errorf("medium %q has no transport wired", req.Medium)
	}
	if len(req.Payload) > w.cfg.MaxPayloadBytes {
		return nil, req, fmt.Errorf("payload %d bytes exceeds cap %d", len(req.Payload), w.cfg.MaxPayloadBytes)
	}
	if req.TargetSecret != "" && !secretNameRe.MatchString(req.TargetSecret) {
		// First, so that every later message naming the secret names one
		// that passed. The name is not repeated: it failed the one rule that
		// makes it safe to put in the resolver's query, and the row already
		// shows it to anyone who needs to look.
		return nil, req, fmt.Errorf("webhook: target secret refused: its name does not match %s", SecretNamePattern)
	}
	if req.TargetSecret != "" && req.Medium != "webhook" {
		// Refused before the secret is read. Its value is a webhook URL, and
		// nothing another medium could do with it would be a delivery.
		return nil, req, fmt.Errorf("target secret %s names a webhook URL, but the row's medium is %q", req.TargetSecret, req.Medium)
	}
	switch req.Medium {
	case "email":
		if len(w.cfg.EmailAllowlist) == 0 {
			return nil, req, fmt.Errorf("medium disabled by deployment config (MEMQL_OUTBOUND_EMAIL_ALLOWLIST empty)")
		}
		if !emailAllowed(req.Target, w.cfg.EmailAllowlist) {
			return nil, req, fmt.Errorf("target %q not in email allowlist", req.Target)
		}
	case "webhook":
		if len(w.cfg.WebhookAllowlist) == 0 {
			return nil, req, fmt.Errorf("medium disabled by deployment config (MEMQL_OUTBOUND_WEBHOOK_ALLOWLIST empty)")
		}
		if req.TargetSecret != "" {
			delivery, err := w.resolveSecretTarget(ctx, req)
			if err != nil {
				return nil, req, err
			}
			return transport, delivery, nil
		}
		if !webhookAllowed(req.Target, w.cfg.WebhookAllowlist) {
			return nil, req, fmt.Errorf("target %q not in webhook allowlist", req.Target)
		}
	default:
		return nil, req, fmt.Errorf("unknown medium %q", req.Medium)
	}
	return transport, req, nil
}

// resolveSecretTarget reads the URL a secret target names (memql#5480) and
// returns the copy of req the transport delivers. Every refusal names the
// secret and never the value, because whatever this returns is stamped into
// lastError and logged, and the value is a credential. That is also why the
// allowlist refusal does not quote its target the way a plain row's does.
//
// A secret that does not resolve fails the row rather than scheduling a
// retry. A secret nobody stored, one that cannot be decrypted, an empty one
// and a node with no resolver all wait on an operator, not on the backoff,
// and the stager learns of it now instead of after
// MEMQL_OUTBOUND_MAX_ATTEMPTS. The resolver's misses and decrypt failures are
// told apart by the engine's predicates, never by their text. Any other
// lookup error -- a timeout, a dropped connection, an engine still booting --
// may pass on its own, and is retried like a refused connection (ADR 4.1).
func (w *Worker) resolveSecretTarget(ctx context.Context, req Request) (Request, error) {
	if w.Secrets == nil {
		return req, fmt.Errorf("webhook: target secret %s did not resolve: no secret resolver is wired on this node", req.TargetSecret)
	}
	value, err := w.Secrets(ctx, req.TargetSecret)
	if err != nil {
		if memql.IsVariableNotFound(err) || memql.IsSecretUndecryptable(err) {
			return req, fmt.Errorf("webhook: target secret %s did not resolve: %w", req.TargetSecret, err)
		}
		return req, retryLater(fmt.Errorf("webhook: target secret %s could not be read: %w", req.TargetSecret, err))
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return req, fmt.Errorf("webhook: target secret %s did not resolve: its value is empty", req.TargetSecret)
	}
	if !webhookAllowed(value, w.cfg.WebhookAllowlist) {
		return req, errors.New("webhook: target not in allowlist")
	}
	delivery := req
	delivery.Target = value
	return delivery, nil
}

// retryLaterError marks a failure admit met before anything was sent that may
// pass on its own: a secret target whose lookup failed rather than missed
// (memql#5480). processRow gives it the delivery backoff instead of stamping
// it failed.
type retryLaterError struct{ err error }

func (e *retryLaterError) Error() string { return e.err.Error() }
func (e *retryLaterError) Unwrap() error { return e.err }

func retryLater(err error) error { return &retryLaterError{err: err} }

func isRetryLater(err error) bool {
	var r *retryLaterError
	return errors.As(err, &r)
}

// secretNameRe compiles SecretNamePattern once.
var secretNameRe = regexp.MustCompile(SecretNamePattern)

func (w *Worker) stampFailed(ctx context.Context, req Request, policyErr error) {
	// lastError via QuoteString (memql#3035). A policy refusal names the
	// offending target, which is caller-supplied.
	w.stamp(ctx, fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "failed", attempts: %d, lastError: %s)`,
		langparser.QuoteString(req.ID), req.Attempts, langparser.QuoteString(truncateError(policyErr))))
	w.logger.Warn("outbound worker: row refused by policy", "id", req.ID, "medium", req.Medium, "error", policyErr)
}

// stamp writes one delivery-state transition, and is the only place the worker
// stamps internal origin (memql#5480). A row naming a secret target is
// server-written end to end -- the engine refuses any write to it without
// internal origin, because the pipelines notify stage reports a delivery when
// its row reads `sent` -- so the worker's own transitions must carry it. The
// stamp is INLINE on this one Execute, where the trust dies: the drain scan
// and the secret's lookup keep the plain system actor, and the statement is
// always updateOutboundRequestStatus, composed by this file with every string
// argument passed through QuoteString.
func (w *Worker) stamp(ctx context.Context, mutation string) {
	if _, err := w.engine.Execute(auth.ContextWithInternalOrigin(ctx), mutation); err != nil {
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
		ID:           getString(row, "id"),
		Medium:       getString(row, "medium"),
		Target:       getString(row, "target"),
		Subject:      getString(row, "subject"),
		Payload:      getString(row, "body"),
		TargetSecret: getString(row, "targetSecret"),
		DedupeKey:    getString(row, "dedupeKey"),
		Attempts:     getInt(row, "attempts"),
	}
}

// truncateError renders err for the lastError argument: NUL-free, then capped.
//
// # Why NUL is removed, when the lexer accepts it
//
// langparser.QuoteString renders a NUL as `\u0000`, and that is correct at its
// own layer -- it is exactly what readString accepts, and the lexer decodes it
// back to rune 0. The statement parses. It then fails one layer down, at
// storage: the stamped payload is marshalled to JSON and bound to a JSONB
// column (component/database/memory-nodes/concept.go, models.go `Payload
// json.RawMessage bun:"type:JSONB,notnull"`, `payload JSONB NOT NULL` in
// migrations/20260324000000_initial_setup.up.sql), and PostgreSQL's jsonb type
// cannot represent U+0000. Measured against postgres:16:
//
//	select '{"lastError":"boom \u0000 nul"}'::jsonb;
//	ERROR:  unsupported Unicode escape sequence
//	DETAIL:  \u0000 cannot be converted to text.
//
// The insert therefore errors, stamp() logs a warning and returns, and the row
// keeps whatever status it had -- which is the identical stuck row memql#3035
// is about, reached through Postgres instead of through the lexer. Fixing the
// escape set alone moved that failure rather than removing it.
//
// NUL is the only escape THIS RENDERER CAN EMIT that jsonb rejects. That is a
// narrower claim than "the only escape jsonb rejects", and only the narrow one
// is true: jsonb ALSO rejects a lone surrogate escape ("Unicode low surrogate
// must follow a high surrogate").
//
// The narrow claim is checkable because the output alphabet is small, and it was
// enumerated rather than recalled -- over every byte 0x00-0xFF, every rune to
// U+2FFF, the surrogate range and invalid UTF-8, QuoteString emits exactly 37
// distinct escapes:
//
//	\" and \\	the two mandatory ones
//	\b \f \n \r \t	the short forms
//	\u0000-\u001f	the remaining control bytes
//	\u2028 \u2029	which encoding/json escapes UNCONDITIONALLY for
//		JSONP safety -- unrelated to the SetEscapeHTML
//		choice QuoteString documents
//	\ufffd	for invalid UTF-8
//
// Every one of those except \u0000 was fed to jsonb and stored fine. A
// surrogate half is not in the set and cannot be, because invalid UTF-8 becomes
// � first. So the wider gap is unreachable from here.
//
// Measure rather than recall if this changes. Three separate claims in this
// change's own review were wrong precisely because they described the escape set
// from memory instead of enumerating it -- including an earlier version of this
// very sentence, which said "\u00XX only for bytes below 0x20" and missed
// the short forms, \u2028/\u2029 and \ufffd.
//
// # Why U+FFFD rather than dropping the byte
//
// The error text is what an operator reads to find out what the remote sent.
// Silently deleting bytes from it is the "lexes fine while mangling what the
// operator reads" hazard this file's own tests warn about; U+FFFD keeps the
// fact that something was there, and it is the convention json.Marshal already
// applies to invalid UTF-8 on this same path.
//
// # Why here, and why lastError only
//
// Not in QuoteString: its contract is "what the MemQL lexer accepts", it is
// shared with component/inbound, and jsonb storability is a different concern
// with different callers. Not for requestId either, and that one is deliberate
// rather than an oversight. A NUL there is unreachable: the id is also a text
// primary key, and PostgreSQL rejects a NUL in text outright ("null character
// not permitted"), so no such row can exist for the worker to read back.
// Rewriting an id would also aim the update at a DIFFERENT row, which is worse
// than failing loudly.
//
// This is a per-BYTE split rather than a blanket exemption, and the two arguments
// in this file are narrower than they look side by side. A bell or a vtab in a
// requestId IS reachable -- text and jsonb both store them happily -- which is
// why every requestId is still quoted. Only NUL is unreachable there.
//
// Substitution runs BEFORE the cap, and the cap bounds THIS FUNCTION'S RETURN.
// It does not bound the stored value exactly, and the difference is worth being
// honest about rather than rounding off: the cut is at a byte boundary, so it
// can split a replacement rune, and encoding/json then repairs each orphaned
// byte into a fresh 3-byte � -- measured at 4100 bytes stored from a
// 4096-byte return. A handful of bytes over, never unbounded.
//
// The ordering is still the right way round. Substituting AFTER the cap lets a
// NUL-dense error expand to 3x lastErrorCap (12288 bytes from 4096, measured),
// which is precisely what the cap exists to prevent.
func truncateError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\x00", "�")
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

// getInt reads an outbound delivery row's numeric field.
//
// SATURATES out of range (memql#4779). Its one caller is `Attempts`, which
// feeds the retry backoff: a wrapped negative reads as a delivery that has
// never been tried and retries it forever.
func getInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	case float64:
		return num.ClampFloat64(v)
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

// SystemActorContext stamps engine roundtrips from the worker with a
// system identity (planner precedent): every drain scan, every status stamp
// and every secret target's lookup runs under it. It carries no internal
// origin; stamp adds that inline to each status stamp, and nothing else gets
// it.
//
// Exported for app's database test (memql#5480), which reads a sealed
// globalSecret through the engine's real resolver under exactly this
// identity: a row tier added to globalSecret that shut this actor out would
// otherwise fail every secret target at run time, with nothing red before.
func SystemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithSystemActor(ctx, "outbound")
}
