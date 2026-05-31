package main

import (
	"io"
	"log/slog"
	"os"
	"strings"

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
	})
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
