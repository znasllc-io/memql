package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/service"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

const versionFilePath = "VERSION"

var (
	fatalWithLoggerFn       = logger.FatalWithLogger
	fatalFn                 = logger.Fatal
	loadServiceEnvOptionsFn = service.LoadDefaultServiceEnvOptions
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

	// Cloud A2 secrets model: when MEMQL_GENESIS_AUTOLOAD=true, decrypt
	// the genesis envelope IN-PROCESS (from MEMQL_GENESIS_B64 or a
	// sealed file) and apply each entry SET-IF-ABSENT, so Container App
	// overrides that are already set in the environment win. This is the
	// base config layer and must run before any component reads its
	// config. Fail closed: a misconfigured auto-load is fatal -- booting
	// on a half-applied/wrong config is worse than not booting. When the
	// flag is unset this is a no-op (local dev's env_file path is
	// untouched).
	if res, err := genesis.AutoloadFromEnv(); err != nil {
		fatalWithLoggerFn(serviceLogger, "genesis envelope auto-load failed", "err", err)
	} else if res.Enabled {
		serviceLogger.Info("genesis envelope auto-loaded",
			"source", res.Source,
			"applied", len(res.Applied),
			"skipped", len(res.Skipped))
	}

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

	// Epic 7.3 (memql#2106): the owned env vars were renamed to the
	// MEMQL_ convention. Bridge any pre-7.3 LEGACY names an operator's
	// shell / .env / sealed envelope still carries onto their new names
	// (set-if-absent, new wins), emitting one deprecation warning per
	// bridged var. Runs after the envelope + .env layers are painted so
	// a legacy value from any of them is honored, and before config is
	// read so every consumer sees the new name.
	genesis.ApplyLegacyEnvAliases(serviceLogger)

	app.Run(app.RunConfig{
		Logger:  serviceLogger,
		Version: resolveVersionFn(),
		Overrides: app.Overrides{
			FatalWithLogger:   fatalWithLoggerFn,
			LoadServiceEnvOpt: loadServiceEnvOptionsFn,
		},
		// Wire the health-dependency surface so /healthz reports
		// per-component readiness. Lives in component/server so
		// non-server consumers (subcommands, tests) don't drag it
		// in by default; the carrier binary explicitly opts in.
		SetHealth: func(deps []common.Dependency) {
			server.SetHealthDependencies(deps)
		},
		// Graceful shutdown (#552 + memql#1269): on SIGTERM, flip /healthz
		// + /readyz to 503 (BeginDrain) so k8s/LB de-route this pod, then
		// app.Run keeps serving for the drain delay (endpoint-removal
		// window), waits up to GracePeriod for in-flight user streams
		// (ActiveWork) to finish, marks the node lifecycle Stopped, and
		// finally stops. terminationGracePeriodSeconds in deploy/k8s must
		// exceed DrainDelay + GracePeriod + the Stop budget.
		BeginDrain:  func() { server.SetDraining(true) },
		DrainDelay:  resolveShutdownDrainDelay(serviceLogger),
		GracePeriod: resolveShutdownGracePeriod(serviceLogger),
		// In-flight accounting for the drain wait: the user-facing
		// MemqlService.Stream session count (memql#1269). Mesh / worker /
		// voice streams ride other services and are infra, not user work.
		ActiveWork: func() int64 { return server.ActiveStreams() },
	})
}

// resolveShutdownDrainDelay reads MEMQL_SHUTDOWN_DRAIN_DELAY (a Go duration
// string, e.g. "5s") and falls back to app.DefaultShutdownDrainDelay. An
// unparseable value logs a warning and uses the default.
func resolveShutdownDrainDelay(logger *slog.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv("MEMQL_SHUTDOWN_DRAIN_DELAY"))
	if raw == "" {
		return app.DefaultShutdownDrainDelay
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if logger != nil {
			logger.Warn("invalid MEMQL_SHUTDOWN_DRAIN_DELAY; using default",
				"value", raw, "default", app.DefaultShutdownDrainDelay.String())
		}
		return app.DefaultShutdownDrainDelay
	}
	return d
}

// resolveShutdownGracePeriod reads MEMQL_SHUTDOWN_GRACE_PERIOD (a Go duration
// string, e.g. "25s") -- the bound on how long the graceful drain (memql#1269)
// waits for in-flight user streams to finish after the node is de-routed --
// and falls back to app.DefaultShutdownGracePeriod. An unparseable value logs a
// warning and uses the default.
func resolveShutdownGracePeriod(logger *slog.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv("MEMQL_SHUTDOWN_GRACE_PERIOD"))
	if raw == "" {
		return app.DefaultShutdownGracePeriod
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if logger != nil {
			logger.Warn("invalid MEMQL_SHUTDOWN_GRACE_PERIOD; using default",
				"value", raw, "default", app.DefaultShutdownGracePeriod.String())
		}
		return app.DefaultShutdownGracePeriod
	}
	return d
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
// voice-agent-token mint) print the minted bearer to stdout so a
// `bearer=$(kubectl exec deploy/identity -- /app/memql ... mint)`
// shell capture (e.g. `make node-token` / `make voice-agent-token`)
// can pull it out. Under the previous
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
// the container log capture (kubectl/ArgoCD, cloud log ingestion) that
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
