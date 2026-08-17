package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/service"
	"github.com/znasllc-io/memql/core/buildinfo"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

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

	// CONFIG HAS ONE DELIVERY PATH (epic memql#3958). A sealed genesis
	// envelope used to be decrypted in-process here and applied
	// set-if-absent, as a second source beside the k8s Secret every node
	// envFroms. It bought nothing the Secret did not already deliver --
	// seeding works locally via seed-secrets.sh and in the cloud via ESO --
	// and it cost a sealing CLI, a decrypt tool, this boot hook, an
	// eleven-Deployment kustomize patch and a .znas file format. Its own
	// tooling also wrote `export MEMQL_MASTER_KEY=` into a world-readable
	// ~/.bashrc, which is the defect memql#3519 named while that key still
	// doubled as an authenticator.
	//
	// What replaced its one irreplaceable job -- getting an owner back into a
	// cluster they are locked out of -- is the recovery key (memql#3964),
	// which authenticates and decrypts nothing.

	// Layer repo-root /.env on top of whatever the host shell already
	// painted into the process environment. Local
	// dev relies on this to flip knobs (verbose observability, debug
	// flags, ...) without re-sealing the envelope; production simply
	// has no .env file and the call is a no-op.
	if overridden, err := envregistry.ApplyLocalOverride("."); err != nil {
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
	envregistry.ApplyLegacyEnvAliases(serviceLogger)

	// One domain in, every domain-shaped env var out (memql#3593). Runs after
	// the alias shim so a bridged MEMQL_DOMAIN is already on its new name, and
	// before app.Run so every component reads the derived values. Set-if-absent,
	// so a deployment that configures these explicitly is untouched.
	envregistry.ApplyDomainDerivations(serviceLogger)

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

// resolveServiceVersion answers what this binary states about itself. It is
// stamped onto startup events, health output and the engine's memqlVersion()
// builtin (via app.RunConfig.Version), and it is what a client asks for when it
// wants to know which engine it is talking to.
//
// There is exactly one source: the link-time release stamp in core/buildinfo
// (memql#3998). It used to read `VERSION` from the environment and then a
// `VERSION` FILE on disk, and both were removed rather than demoted:
//
//   - The file said `0.15.0` at every tag from v0.16.1 onward, and the image
//     build overwrote it with `0.15.0-<epoch>` -- a stamp VERSIONING.md
//     explicitly forbids ("No -<epoch> suffixes"). So the answer was
//     release-SHAPED and wrong, which reads as authoritative to anything
//     comparing versions.
//   - The env var let a deployment assert any version it liked over a build
//     that already knew the truth. A version a running process can be told is
//     not a version, and the reason the engine could not state its release
//     honestly is that it had two ways to be told what to say and no way to
//     know.
//
// A build that was not cut from a release now says "dev", which no release
// parser accepts -- the client gets "cannot compare" instead of a confident
// wrong answer.
func resolveServiceVersion() string {
	return buildinfo.Version()
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
