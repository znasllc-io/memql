package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// DefaultRunShutdownTimeout caps how long Run gives the dependency
// graph to drain on shutdown. Matches the value memql/main.go used
// pre-#356 (the dependencyShutdownTimeout package const there); kept
// in the app package so consumers don't have to know about it
// unless they want to override.
const DefaultRunShutdownTimeout = 30 * time.Second

// RunConfig configures Run. Every field has a sensible default the
// app package supplies when omitted -- the only required fields in
// practice are Logger, Version, and Overrides.
//
// Carrier-binary callers (memql/main.go + future
// memql-bff-copresent/main.go) populate Logger via
// mustCreateServiceLogger (binary-side helper) + Version via the
// binary's resolveServiceVersion. Subcommand dispatch + the genesis
// .env override stay caller-side because they're binary-specific +
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

	// WaitForSignal blocks until a shutdown signal arrives + returns
	// the signal that fired. Zero falls back to the package default
	// (SIGINT + SIGTERM). Tests override to fire a synthetic signal
	// immediately so Run's body exercises end-to-end without parking
	// on a real syscall.
	WaitForSignal func() os.Signal

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
// (memql/main.go, memql-bff-copresent/main.go) don't need to
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
// memql-bff-copresent carrier binary can share the same lifecycle
// without duplicating ~30 LOC. Pre-extraction shape preserved
// bit-exactly so the bff binary's runtime behaviour is identical.
func Run(cfg RunConfig) {
	if cfg.Logger == nil {
		panic("app.Run: Logger is required")
	}
	if cfg.Version == "" {
		panic("app.Run: Version is required")
	}

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
	wait := cfg.WaitForSignal
	if wait == nil {
		wait = DefaultWaitForShutdownSignal
	}

	application := Build(cfg.Logger, cfg.Version, cfg.Overrides)

	if cfg.SetHealth != nil {
		cfg.SetHealth(application.Dependencies)
	}

	start(application.Dependencies...)

	// Emit system.startup AFTER every dependency has started so the
	// cluster bootstrap automation sees the full dependency surface.
	application.EmitSystemStartup()

	sig := wait()
	cfg.Logger.Info("shutdown signal received", "signal", sig.String())

	// Emit system.shutdown BEFORE the dependency Stop sweep so the
	// node-deregistration automation has a chance to run while the
	// engine is still up.
	application.EmitSystemShutdown()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stop(shutdownCtx, application.Dependencies...)

	cfg.Logger.Info("shutdown complete")
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

// DefaultWaitForShutdownSignal is the package default for
// RunConfig.WaitForSignal. Blocks on SIGINT + SIGTERM + returns the
// signal that fired. Tests pass a custom WaitForSignal to fire
// immediately so Run's body exercises end-to-end without parking on
// a real syscall.
func DefaultWaitForShutdownSignal() os.Signal {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	return <-signals
}
