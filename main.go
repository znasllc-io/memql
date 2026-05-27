package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/service"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

const (
	dependencyShutdownTimeout = 30 * time.Second
	versionFilePath           = "VERSION"
)

var (
	fatalWithLoggerFn       = logger.FatalWithLogger
	fatalFn                 = logger.Fatal
	loadServiceEnvOptionsFn = service.LoadDefaultServiceEnvOptions
	startDependenciesFn     = startDependencies
	waitForShutdownSignalFn = waitForShutdownSignal
	stopDependenciesFn      = stopDependencies
	resolveVersionFn        = resolveServiceVersion
)

func main() {
	// Subcommand dispatch -- intercept before bootstrapping the full
	// service stack. Subcommands handle their own lifecycle (build
	// dependencies, do the work, exit). The dispatch table is
	// build-tag-gated; binaries that lack the relevant deps surface
	// "subcommand X is not available on this binary" at runtime.
	if len(os.Args) > 1 {
		if handled, code := dispatchSubcommand(os.Args[1:]); handled {
			os.Exit(code)
		}
	}

	serviceLogger := mustCreateServiceLogger()

	// Layer repo-root /.env on top of whatever the host shell + genesis
	// envelope already painted into the process environment. Local
	// dev relies on this to flip knobs (verbose observability, debug
	// flags, ...) without re-sealing the envelope; production simply
	// has no .env file and the call is a no-op.
	if overridden, err := genesis.ApplyLocalOverride("."); err != nil {
		serviceLogger.Warn("local .env override failed -- continuing with envelope values", "err", err)
	} else if len(overridden) > 0 {
		serviceLogger.Info("local .env override applied", "vars", overridden)
	}

	version := resolveVersionFn()

	application := app.Build(serviceLogger, version, app.Overrides{
		FatalWithLogger:   fatalWithLoggerFn,
		LoadServiceEnvOpt: loadServiceEnvOptionsFn,
	})

	server.SetHealthDependencies(application.Dependencies)

	startDependenciesFn(application.Dependencies...)

	// Emit system.startup event for cluster bootstrap and node self-registration automations.
	application.EmitSystemStartup()

	sig := waitForShutdownSignalFn()
	serviceLogger.Info("shutdown signal received", "signal", sig.String())

	// Emit system.shutdown event for node deregistration automation.
	application.EmitSystemShutdown()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), dependencyShutdownTimeout)
	defer cancel()

	stopDependenciesFn(shutdownCtx, application.Dependencies...)

	serviceLogger.Info("shutdown complete")
}

func startDependencies(dependencies ...common.Dependency) {
	for _, dependency := range dependencies {
		dependency.Start(context.Background())
	}
}

func stopDependencies(ctx context.Context, dependencies ...common.Dependency) {
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

	for _, dependency := range ordered {
		stopCtx := ctx

		if stopCtx == nil {
			stopCtx = context.Background()
		}

		var cancel context.CancelFunc

		if _, hasDeadline := stopCtx.Deadline(); !hasDeadline {
			stopCtx, cancel = context.WithTimeout(context.Background(), dependencyShutdownTimeout)
		}

		dependency.Stop(stopCtx)

		if cancel != nil {
			cancel()
		}
	}
}

func waitForShutdownSignal() os.Signal {
	signals := make(chan os.Signal, 1)

	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	defer signal.Stop(signals)

	return <-signals
}

func resolveServiceVersion() string {
	value := strings.TrimSpace(os.Getenv("VERSION"))

	if value != "" {
		return value
	}

	data, err := os.ReadFile(versionFilePath)

	if err == nil {
		if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
			return trimmed
		}
	}

	return "dev"
}

func mustCreateServiceLogger() *slog.Logger {
	return mustCreateLoggerWithWriter(os.Stdout)
}

// mustCreateCLILogger builds the same logger mustCreateServiceLogger
// builds but writes JSON to os.Stderr instead of os.Stdout.
//
// Why this exists (memql#353): CLI subcommands (node-token mint /
// voice-agent-token mint) print the minted bearer to stdout so the
// `bearer=$(docker exec memql ... mint)` shell capture in
// scripts/dev/mint-node-tokens.sh + scripts/dev/lib.sh's
// mint_voice_agent_token can pull it out. Under the previous
// stdout-bound service logger, every app.Build() + dep.Start()
// startup INFO log landed on stdout BEFORE the bearer, so the shell
// capture ended up with a multi-line "bearer" of JSON log lines +
// the JWT. That value got stamped into .env.local.node-tokens as a
// MEMQL_<TYPE>_NODE_TOKEN= value; the next `source` of the file
// failed at line 5 with `time:2026-05-27T01:15:00...: command not
// found` because the JSON shaped like a shell statement that bash
// couldn't parse.
//
// Switching the CLI's logger to stderr keeps the contract clean:
// stdout = data (the bearer), stderr = diagnostics (slog JSON + the
// "minted node=X type=Y" summary fmt.Fprintf already writes there).
// The server boot path (main.go's serviceLogger) stays on stdout so
// the docker compose log capture / Cloud Run log ingestion that
// already consume container stdout aren't affected.
func mustCreateCLILogger() *slog.Logger {
	return mustCreateLoggerWithWriter(os.Stderr)
}

// mustCreateLoggerWithWriter is the shared body of
// mustCreateServiceLogger + mustCreateCLILogger; only the writer
// differs. Loads serviceOpts the same way (LoggerLevel honoured)
// and routes through core/logger.New so the redaction handler +
// ordered JSON writer + component-name field land identically on
// both paths.
func mustCreateLoggerWithWriter(w io.Writer) *slog.Logger {
	serviceOpts, err := loadServiceEnvOptionsFn()

	if err != nil {
		fatalFn("failed to load service environment options", "error", err)
	}

	level := slog.LevelInfo

	if strings.TrimSpace(serviceOpts.LoggerLevel) != "" {
		var parsedLevel slog.Level

		if err := parsedLevel.UnmarshalText([]byte(strings.ToLower(serviceOpts.LoggerLevel))); err != nil {
			fatalFn("invalid service log level", "error", err)
		}

		level = parsedLevel
	}

	return logger.New(common.ComponentName(serviceOpts.Name), w, level)
}
