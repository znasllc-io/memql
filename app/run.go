package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// DefaultRunShutdownTimeout caps how long Run gives the dependency
// graph to drain on shutdown. Matches the value memql/main.go used
// pre-#356 (the dependencyShutdownTimeout package const there); kept
// in the app package so consumers don't have to know about it
// unless they want to override.
const DefaultRunShutdownTimeout = 30 * time.Second

// DefaultShutdownDrainDelay is how long Run keeps serving AFTER a shutdown
// signal before it begins the dependency Stop sweep (#552, graceful
// shutdown). The window lets k8s notice the pod is Terminating + the
// readiness probe flip to 503 (via BeginDrain) and remove this pod from the
// Service endpoints, so NEW traffic stops arriving while in-flight requests
// finish -- instead of new connections racing onto a pod that's tearing
// down. terminationGracePeriodSeconds in the manifests must exceed this plus
// the grace period plus the Stop budget. Override via the carrier binary
// (MEMQL_SHUTDOWN_DRAIN_DELAY).
const DefaultShutdownDrainDelay = 5 * time.Second

// DefaultShutdownGracePeriod bounds how long Run waits, AFTER the drain delay
// (endpoint-removal window) has elapsed, for in-flight work to finish before
// it transitions the node Draining -> Stopped and runs the dependency Stop
// sweep (memql#1269, the graceful-drain BEHAVIOUR on top of the #1268
// lifecycle seams). The node is already de-routed (readiness 503, gossip
// DRAINING) so no NEW work arrives; this grace lets the in-flight user streams
// (server.ActiveStreams via RunConfig.ActiveWork) drain to zero so a rolling
// deploy doesn't drop a turn mid-flight. In-flight that outlives the grace is
// cut off at the deadline (the bounded #1119 GracefulStop in the Stop sweep
// then forces remaining streams closed). Override via the carrier binary
// (MEMQL_SHUTDOWN_GRACE_PERIOD). A zero value falls back to this default; a
// negative value skips the in-flight wait entirely.
const DefaultShutdownGracePeriod = 25 * time.Second

// DefaultStartupReadyWarningInterval reports a slow dependency without treating
// elapsed time as readiness. A shutdown signal can cancel the wait; the platform's
// startup probe owns the maximum startup window.
const DefaultStartupReadyWarningInterval = 30 * time.Second

// RunConfig configures Run. Every field has a sensible default the
// app package supplies when omitted -- the only required fields in
// practice are Logger, Version, and Overrides.
//
// Carrier-binary callers (memql/main.go + the carrier repo's
// main.go) populate Logger via
// mustCreateServiceLogger (binary-side helper) + Version via the
// binary's resolveServiceVersion. Subcommand dispatch + the `.env` override
// stay caller-side because they're binary-specific +
// run BEFORE the logger / overrides exist.
type RunConfig struct {
	// Logger is the root slog.Logger every component derives from.
	// Required.
	Logger *slog.Logger

	// Version is the service version string stamped onto startup
	// events + health probes. Required.
	Version string

	// Overrides bundles the injectable hooks the App needs at
	// build time (FatalWithLogger, LoadServiceEnvOpt). See
	// Overrides for the field-level contract.
	Overrides Overrides

	// ShutdownTimeout caps how long the per-dependency Stop calls
	// have to complete. Zero falls back to DefaultRunShutdownTimeout.
	// Tests pass a shorter value to keep the suite snappy.
	ShutdownTimeout time.Duration

	// BeginDrain is invoked the instant a shutdown signal arrives,
	// BEFORE the drain delay + Stop sweep (#552). The carrier binary
	// wires it to component/server.SetDraining(true) so /healthz starts
	// returning 503 and k8s/LB de-routes the pod. Zero = no-op (the
	// drain delay still applies). Kept as a hook so the app package
	// doesn't import component/server.
	BeginDrain func()

	// DrainDelay is how long to keep serving after the signal before
	// the in-flight wait + Stop sweep, giving endpoint removal time to
	// propagate. Zero falls back to DefaultShutdownDrainDelay; negative
	// disables the delay (tests pass a negative or tiny value).
	DrainDelay time.Duration

	// GracePeriod bounds the in-flight drain (memql#1269): after the
	// DrainDelay window, Run waits up to this long for ActiveWork to
	// report zero before transitioning Draining -> Stopped and running
	// the Stop sweep. Zero falls back to DefaultShutdownGracePeriod;
	// negative skips the in-flight wait (tests pass a tiny/negative
	// value). The carrier binary wires it from MEMQL_SHUTDOWN_GRACE_PERIOD.
	GracePeriod time.Duration

	// ActiveWork reports the count of currently in-flight units of work
	// (the carrier wires it to component/server.ActiveStreams -- open
	// user-facing MemqlService.Stream sessions). Run polls it during the
	// grace window and proceeds to teardown the instant it hits zero.
	// Zero (nil) means "no in-flight accounting" -- Run waits the full
	// grace period unless GracePeriod is negative. Kept as a hook so the
	// app package doesn't import component/server. (memql#1269)
	ActiveWork func() int64

	// WaitForReady blocks until dependencies are ready or ctx is cancelled.
	// Returning an error prevents Ready and system.startup, then drains the app.
	// Overrides must observe ctx so shutdown also works during startup.
	WaitForReady func(ctx context.Context, deps []common.Dependency) error

	// Start is the per-dependency start hook. Zero falls back to
	// the package default, which calls dep.Start(context.Background())
	// in registration order. Tests override to inject failures or
	// capture call order.
	Start func(deps ...common.Dependency)

	// Stop is the per-dependency stop hook. Zero falls back to the
	// package default, which sorts by Order() descending + stops
	// each with a per-dep ShutdownTimeout context. Tests override
	// to capture ordering or inject hangs.
	Stop func(ctx context.Context, deps ...common.Dependency)

	// WaitForSignal optionally replaces the OS signal source. It must return
	// when ctx is cancelled (with nil if no signal arrived), so an operator
	// drain or failed startup can release the waiter. Nil registers SIGINT and
	// SIGTERM synchronously before starting dependencies.
	WaitForSignal func(ctx context.Context) os.Signal

	// SetHealth is invoked with the live dependency slice after the
	// app builds. Zero is a no-op. The standalone helper that mints
	// the health-probe surface lives in component/server, but the
	// app package keeps it pluggable so callers without an HTTP
	// surface (subcommands, tests) don't need to drag the server
	// package into their import graph.
	SetHealth func(deps []common.Dependency)
}

// Run is the canonical lifecycle entry point for a memql carrier
// binary. Bundles Build + Start + EmitSystemStartup + WaitForSignal
// + EmitSystemShutdown + Stop into a single call so consumers
// (memql/main.go, the carrier repo's main.go) don't need to
// duplicate the 30+ lines of orchestration.
//
// Returns nothing; failures during build call cfg.Overrides.FatalWithLogger
// which is expected to os.Exit. The shutdown path is best-effort: a
// hung dependency that exceeds its per-dep ShutdownTimeout context
// will still trigger the next dependency's Stop after its context
// fires. Run blocks until WaitForSignal returns + the Stop sweep
// completes.
//
// memql#356: extracted from memql/main.go's main() body so the
// carrier repo's binary can share the same lifecycle
// without duplicating ~30 LOC. Pre-extraction shape preserved
// bit-exactly so the bff binary's runtime behaviour is identical.
func Run(cfg RunConfig) {
	if cfg.Logger == nil {
		panic("app.Run: Logger is required")
	}
	if cfg.Version == "" {
		panic("app.Run: Version is required")
	}

	runApplication(cfg, Build(cfg.Logger, cfg.Version, cfg.Overrides))
}

// runApplication keeps lifecycle orchestration testable with a dependency graph
// whose readiness and event subscriptions can be delayed independently.
func runApplication(cfg RunConfig, application *App) {
	timeout := cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultRunShutdownTimeout
	}
	start := cfg.Start
	if start == nil {
		start = DefaultStartDependencies
	}
	stop := cfg.Stop
	if stop == nil {
		stop = DefaultStopDependencies
	}
	waitForReady := cfg.WaitForReady
	if waitForReady == nil {
		waitForReady = func(ctx context.Context, deps []common.Dependency) error {
			return waitForDependenciesReady(ctx, deps, cfg.Logger)
		}
	}
	runCtx, shutdown, stopWatching := watchShutdownTrigger(cfg.WaitForSignal, application.OperatorDrainSignal())
	defer stopWatching()

	if cfg.SetHealth != nil {
		cfg.SetHealth(application.Dependencies)
	}

	start(application.Dependencies...)

	// Clean startup (memql#1269, readiness != liveness): block until every
	// dependency has actually signalled Ready (DB, engine, mesh registration
	// up) -- not merely until Start was INVOKED -- before flipping the node to
	// Ready. Otherwise the node would advertise Ready (and readiness 200) the
	// instant Start returned, while deps were still wiring up, and route
	// traffic onto a not-yet-serving pod.
	readyErr := waitForReady(runCtx, application.Dependencies)
	if readyErr == nil {
		readyErr = runCtx.Err()
	}
	if readyErr != nil {
		cfg.Logger.Warn("startup interrupted before dependencies were ready", "error", readyErr)
		application.BeginNodeDrain()
		if cfg.BeginDrain != nil {
			cfg.BeginDrain()
		}
		application.MarkNodeStopped()
		application.releaseDatabasePools(timeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		stop(shutdownCtx, application.Dependencies...)
		return
	}

	// The node's dependencies are now actually serving: flip its lifecycle
	// Starting -> Ready (memql#1268) so the next gossip heartbeat advertises
	// Ready and the readiness probe reports ready. No-op on non-mesh binaries.
	application.MarkNodeReady()

	// MODULE READINESS (design records 2026-09-06-configuration-readiness
	// section 6, and 2026-09-14-readiness-convergence D2): evaluated here,
	// after every dependency is Ready and the plug-ins and providers have
	// materialized, under the same node identity the startup event carries.
	// Earlier would report an unconfigured module for every integration that
	// had not finished wiring up.
	//
	// THE WRITE GOES THROUGH THE RECOMPUTE SUBSCRIBER, which retries a pass
	// that failed or could not evaluate on a jittered backoff until one lands.
	// On 2026-09-13 every pod's boot write hit a saturated database during the
	// rollout (SQLSTATE 53300 and dial i/o timeouts); the direct write that
	// used to sit here had exactly one more chance thirty seconds later, and a
	// node nobody dials (the edge, a product bff) never receives the mesh event
	// that would have prompted a third.
	//
	// AND IT IS JITTERED 0-5 s, because every pod in a rollout reaches this
	// line at the same moment, on top of the seed materializer, the cluster
	// registration and provider resolution -- the peak of the surge.
	//
	// A failure never stops the node. A cluster that will not start because
	// it could not describe its own configuration is a worse outcome than a
	// stale verdict.
	//
	// AND ONE RE-WRITE THIRTY SECONDS LATER (epic memql#5118, D5). It covers a
	// node whose integration materialized LAZILY, after the first write ran,
	// and there is no event for "an integration finished resolving". It goes
	// through the same loop, so it too is retried on failure; after it, the
	// loop's safety net bounds how long any row can lag.
	bootRewrite, cancelBootRewrite := context.WithCancel(context.Background())
	defer cancelBootRewrite()
	if eng := application.Engine(); eng != nil {
		eng.SetReadinessIdentity(application.startupNodeID(), application.startupNodeType())
		// writeOrQueue notifies the subscriber where there is one and writes
		// directly where there is not (a binary with no event bus), because a
		// write that quietly went nowhere is the failure this exists to prevent.
		writeOrQueue := func(reason string) {
			if eng.NotifyReadinessRecompute(reason) {
				cfg.Logger.Info("module readiness: write queued", "reason", reason)
				return
			}
			if n, err := eng.WriteModuleReadiness(bootRewrite); err != nil {
				cfg.Logger.Warn("module readiness: direct write failed; the previous rows stand", "reason", reason, "error", err)
			} else {
				cfg.Logger.Info("module readiness: rows written", "reason", reason, "modules", n)
			}
		}
		go func() {
			if !sleepOrDone(bootRewrite, memql.ReadinessBootJitter()) {
				return
			}
			writeOrQueue("boot")
			if !sleepOrDone(bootRewrite, memql.ReadinessBootRewriteDelay) {
				return
			}
			writeOrQueue("bootRewrite")
		}()
	}

	// Emit system.startup AFTER every dependency has started so the
	// cluster bootstrap automation sees the full dependency surface.
	application.EmitSystemStartup()

	// Block until a shutdown trigger fires. TWO triggers funnel into the
	// SAME drain sequence below (epic memql#1259 decision #4: one drain
	// mechanism, two triggers): the OS shutdown signal (SIGINT/SIGTERM --
	// the deploy path, memql#1269) and an on-demand operator maintenance
	// request (memql#1270, RequestOperatorDrain closes the channel from
	// the owner/admin-gated gRPC handler). Whichever fires first wins; the
	// drain orchestration that follows is identical for both.
	trigger := <-shutdown
	cfg.Logger.Info("shutdown triggered", "trigger", trigger,
		"reason", application.OperatorDrainReason())

	// The delayed readiness re-write above has no business running through a
	// drain: it would write rows describing a node that is going away, and it
	// would do so against a database the Stop sweep is about to close.
	cancelBootRewrite()

	// Flip this node's lifecycle Ready -> Draining (memql#1268) on the
	// shutdown signal so the next gossip heartbeat advertises Draining and
	// peers route around it at once (the lifecycle observer also flips
	// readiness to not-ready). This is the mechanism only; the in-flight
	// finish / flush BEHAVIOUR is memql#1269. Runs before the drain delay so
	// the new state propagates during the DrainDelay window.
	application.BeginNodeDrain()

	// Graceful drain (#552): flip readiness to 503 + keep serving for
	// DrainDelay so k8s removes this pod from the Service endpoints
	// before we tear down -- new traffic stops arriving while in-flight
	// requests finish. Runs BEFORE the in-flight wait + Stop sweep.
	applyShutdownDrain(cfg.Logger, cfg.BeginDrain, cfg.DrainDelay, time.Sleep)

	// Finish in-flight work (memql#1269): the node is now de-routed
	// (readiness 503, gossip DRAINING) so no NEW work arrives. Wait -- bounded
	// by GracePeriod -- for the in-flight user streams to drain to zero before
	// tearing the transport down, so a rolling deploy doesn't drop a turn
	// mid-flight. Work that outlives the grace is cut off at the deadline (the
	// bounded #1119 GracefulStop in the Stop sweep below then forces what
	// remains closed).
	waitForInflightDrain(cfg.Logger, cfg.ActiveWork, cfg.GracePeriod, time.Sleep)

	// Emit system.shutdown BEFORE the dependency Stop sweep so the
	// node-deregistration automation has a chance to run while the
	// engine is still up.
	application.EmitSystemShutdown()

	// In-flight work is done (or the grace deadline fired): flip the node
	// lifecycle Draining -> Stopped (memql#1268/#1269) so it leaves the mesh
	// as cleanly Stopped rather than vanishing mid-Draining, immediately
	// before the Stop sweep closes the transport. No-op on non-mesh binaries.
	application.MarkNodeStopped()

	// Release the database connection pool as an explicit drain phase
	// (memql#1875). The node is already de-routed (readiness 503, gossip
	// DRAINING) and in-flight work has drained, so hand the TigerDB sessions
	// back NOW -- before the (potentially slow, per-dependency-timeout-bounded)
	// Stop sweep below -- rather than holding up to MaxOpenConns connections
	// open the whole time the process lingers in the blue-green scale-down
	// window. This shrinks the deploy-time connection overlap that exhausts the
	// instance ceiling (SQLSTATE 53300). Runs AFTER EmitSystemShutdown +
	// MarkNodeStopped (which still need the DB) and is idempotent with the DB
	// component's own Stop in the sweep.
	application.releaseDatabasePools(timeout)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stop(shutdownCtx, application.Dependencies...)

	cfg.Logger.Info("shutdown complete")
}

// releaseDatabasePools closes the node's database connection pool as an
// explicit drain phase, BEFORE the dependency Stop sweep, so a draining
// blue-green color hands its TigerDB connections back promptly instead of
// holding them until process exit (memql#1875). No-op on a build without a
// database handle. The released-connection counts are logged so a deploy can
// observe the drop in active connections per node.
func (a *App) releaseDatabasePools(timeout time.Duration) {
	if a == nil || a.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stats := a.db.ClosePool(ctx)

	a.Logger.Info("released database connection pool on drain",
		"openConnections", stats.OpenConnections,
		"inUse", stats.InUse,
		"idle", stats.Idle,
		"maxOpenConnections", stats.MaxOpenConnections,
		"waitCount", stats.WaitCount,
	)
}

// applyShutdownDrain runs the graceful-drain step (#552): mark the node
// draining (beginDrain, typically server.SetDraining(true) so /healthz ->
// 503), then keep serving for `delay` via the injected sleep so endpoint
// removal propagates before the Stop sweep. A zero delay falls back to
// DefaultShutdownDrainDelay; a negative delay skips the wait entirely.
// `sleep` is injected so tests don't park on the wall clock.
func applyShutdownDrain(logger *slog.Logger, beginDrain func(), delay time.Duration, sleep func(time.Duration)) {
	if beginDrain != nil {
		beginDrain()
	}
	if delay == 0 {
		delay = DefaultShutdownDrainDelay
	}
	if delay <= 0 {
		return
	}
	if logger != nil {
		logger.Info("draining before shutdown", "delay", delay.String())
	}
	sleep(delay)
}

// inflightPollInterval is how often waitForInflightDrain re-checks the
// in-flight count while waiting for it to reach zero. Small enough that a drain
// that finishes early proceeds promptly; large enough not to busy-spin.
const inflightPollInterval = 250 * time.Millisecond

// waitForInflightDrain implements the memql#1269 in-flight finish: after the
// node is de-routed (readiness 503, gossip DRAINING), wait -- bounded by
// `grace` -- for `activeWork` to report zero before returning so the caller can
// transition Stopped + run the Stop sweep. Polls on a fixed interval and
// returns the instant the count hits zero. A zero `grace` falls back to
// DefaultShutdownGracePeriod; a negative `grace` skips the wait entirely
// (work is cut off immediately, relying on the bounded GracefulStop). A nil
// `activeWork` means there is no in-flight accounting, so there is nothing to
// wait ON -- return immediately rather than blocking the full grace for no
// reason. `sleep` is injected so tests don't park on the wall clock.
func waitForInflightDrain(logger *slog.Logger, activeWork func() int64, grace time.Duration, sleep func(time.Duration)) {
	if grace == 0 {
		grace = DefaultShutdownGracePeriod
	}
	if grace < 0 || activeWork == nil {
		return
	}

	if n := activeWork(); n <= 0 {
		return
	}

	if logger != nil {
		logger.Info("waiting for in-flight work to drain",
			"grace", grace.String(), "active", activeWork())
	}

	deadline := grace
	for deadline > 0 {
		step := inflightPollInterval
		if step > deadline {
			step = deadline
		}
		sleep(step)
		deadline -= step

		if n := activeWork(); n <= 0 {
			if logger != nil {
				logger.Info("in-flight work drained; proceeding to shutdown")
			}
			return
		}
	}

	if logger != nil {
		logger.Warn("in-flight grace period elapsed; proceeding to shutdown with work still active",
			"grace", grace.String(), "active", activeWork())
	}
}

// DefaultWaitForDependenciesReady waits for actual readiness, never for a
// timeout that impersonates success. Cancellation names the unfinished dependency.
func DefaultWaitForDependenciesReady(ctx context.Context, deps []common.Dependency) error {
	return waitForDependenciesReady(ctx, deps, slog.Default())
}

func waitForDependenciesReady(ctx context.Context, deps []common.Dependency, logger *slog.Logger) error {
	warning := time.NewTicker(DefaultStartupReadyWarningInterval)
	defer warning.Stop()
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		ready := dep.Ready()
		waiting := true
		for waiting {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("waiting for %s: %w", dep.ComponentName(), err)
			}
			select {
			case <-ready:
				waiting = false
			case <-ctx.Done():
				return fmt.Errorf("waiting for %s: %w", dep.ComponentName(), ctx.Err())
			case <-warning.C:
				logger.Warn("startup is waiting for a dependency", "dependency", dep.ComponentName())
			}
		}
	}
	return ctx.Err()
}

// DefaultStartDependencies is the package default for RunConfig.Start.
// Calls dep.Start(context.Background()) in registration order; never
// errors (Start is expected to be best-effort + log internally).
//
// Exported so callers can compose with their own start hooks (e.g.
// tests that count + delegate).
func DefaultStartDependencies(dependencies ...common.Dependency) {
	for _, dep := range dependencies {
		dep.Start(context.Background())
	}
}

// DefaultStopDependencies is the package default for RunConfig.Stop.
// Sorts by Order() descending (highest-order = top of the stack;
// stops first), then calls dep.Stop with the caller's ctx OR a fresh
// per-dep timeout context when the caller didn't provide a deadline.
//
// Stable tie-break keeps deterministic shutdown order across runs
// (mainly for log readability + test pinning).
func DefaultStopDependencies(ctx context.Context, dependencies ...common.Dependency) {
	if len(dependencies) == 0 {
		return
	}

	ordered := append([]common.Dependency(nil), dependencies...)

	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Order() == ordered[j].Order() {
			return i > j
		}
		return ordered[i].Order() > ordered[j].Order()
	})

	for _, dep := range ordered {
		stopCtx := ctx
		if stopCtx == nil {
			stopCtx = context.Background()
		}

		var cancel context.CancelFunc
		if _, hasDeadline := stopCtx.Deadline(); !hasDeadline {
			stopCtx, cancel = context.WithTimeout(context.Background(), DefaultRunShutdownTimeout)
		}

		dep.Stop(stopCtx)

		if cancel != nil {
			cancel()
		}
	}
}

// DefaultWaitForShutdownSignal waits for an OS signal or context cancellation.
func DefaultWaitForShutdownSignal(ctx context.Context) os.Signal {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case sig := <-signals:
		return sig
	case <-ctx.Done():
		return nil
	}
}

// watchShutdownTrigger registers the signal source before Start, cancels the
// readiness wait when shutdown begins, and joins its goroutines on cleanup.
// Both a signal and an operator request still enter the same drain sequence.
func watchShutdownTrigger(wait func(context.Context) os.Signal, opDrain <-chan struct{}) (context.Context, <-chan string, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	waitDone := make(chan struct{})
	if wait == nil {
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		close(waitDone)
	} else {
		go func() {
			defer close(waitDone)
			if sig := wait(ctx); sig != nil {
				signals <- sig
			}
		}()
	}
	trigger := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(trigger)
		label := "cancelled"
		select {
		case sig := <-signals:
			label = "signal:" + sig.String()
		case <-opDrain:
			label = "operator"
		case <-ctx.Done():
		}
		cancel()
		if wait == nil {
			signal.Stop(signals)
		}
		<-waitDone
		trigger <- label
	}()
	return ctx, trigger, func() {
		cancel()
		<-done
	}
}

// sleepOrDone waits d, or answers false when ctx ends first -- the boot
// readiness writes' one wait primitive, so neither can outlive a drain.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
