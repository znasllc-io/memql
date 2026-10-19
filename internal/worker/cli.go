package worker

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/znasllc-io/memql-cockpit/internal/config"
	"github.com/znasllc-io/memql-cockpit/internal/crash"
	"github.com/znasllc-io/memql-cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql-cockpit/internal/worker/backup"
	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// HandleCommand dispatches `memql worker <subcommand>`.
// Subcommands:
//
//	memql worker backup            Report (and optionally run) the watched-folder sweep
//	memql worker pair <code>       Redeem a pairing code, upsert a home, run worker
//	memql worker                   Open the pairing wizard (paste a code)
//	memql worker run [flags]       Run all enabled homes from workers.yaml
//	memql worker setup             Re-run TCC permissions check (computeruse builds)
//	memql worker setup --inference Install a model runtime, pull models, allow them
//	memql worker config            Print effective config (all homes)
//	memql worker apps              Print the local apps, which clusters may use them, and each level
//	memql worker apps --allow <id> --home <cluster>   Allow an app for one cluster (--deny withdraws)
//	memql worker unpair --cluster  Remove or disable one home
//
// `pair` is the primary entry: it walks the user from "I have an
// XXXX-XXXX code from CoPresent" through redemption, TCC, an additive
// workers.yaml upsert, and into the running worker -- one command. `run`
// stays as the headless / scripted path for users who want to
// reinvoke an already-configured worker (LaunchAgent uses this).
func HandleCommand(args []string) {
	// Wrap the whole dispatch in crash.Catch so any panic in a
	// subcommand (the wizard, the runner loop, tools.Dispatch, etc.)
	// is caught, sanitized for token-shaped strings, written to a
	// 0600 crash log, and surfaced as a friendly message rather than
	// a raw goroutine dump to stderr. Without this a panic
	// mid-`worker run` killed the process with sensitive locals
	// visible on the terminal.
	if rep := crash.Catch("worker:HandleCommand", func() {
		dispatchHandleCommand(args)
	}); rep != nil {
		fmt.Fprint(os.Stderr, crash.UserMessage(rep))
		os.Exit(1)
	}
}

func dispatchHandleCommand(args []string) {
	if len(args) == 0 {
		// No-args entry: open the wizard with a paste field. On
		// non-TTY callers this falls back to a clear usage hint.
		handlePair(nil)
		return
	}
	switch args[0] {
	case "pair":
		handlePair(args[1:])
	case "run":
		handleRun(args[1:])
	case "setup":
		handleSetup(args[1:])
	case "config":
		handleConfig(args[1:])
	case "unpair":
		handleUnpair(args[1:])
	case "backup":
		handleBackup(args[1:])
	case "models":
		handleModels(args[1:])
	case "apps":
		handleApps(args[1:])
	case "hardware":
		handleHardware(args[1:])
	case "probe":
		handleProbe(args[1:])
	case "control":
		handleControl(args[1:])
	case "consent":
		handleConsentCmd(args[1:])
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown worker subcommand %q\n\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

// handlePair is the front-door wizard entry. The cockpit MUST
// have an authorized cluster before pair runs -- the identity URL
// is read from ~/.memql/clusters.yaml's selected cluster, NOT
// from a flag or interactive prompt. Run `memql cluster add
// <domain>` first.
//
// With a code argument, the wizard skips the paste step. With none,
// it opens to a paste prompt. The advanced --identity / --token
// flags stay as escape hatches for power users who want to pair
// against an explicit identity URL or who already hold a worker
// token.
func handlePair(args []string) {
	fs := flag.NewFlagSet("worker pair", flag.ExitOnError)
	clusterName := fs.String("cluster", "", "cluster NAME from ~/.memql/clusters.yaml (defaults to the active selection)")
	overrideIdentity := fs.String("identity", "", "advanced: override identity service URL (skips active-cluster lookup)")
	token := fs.String("token", "", "advanced: worker token (skip redeem; assumes pre-existing token)")
	homeID := fs.String("home-id", "", "home id in workers.yaml (default: clusters.yaml name or URL host)")
	force := fs.Bool("force", false, "remap a home id onto a different cluster_url (not required for same-URL refresh)")
	logLevel := fs.String("log-level", "info", "log level")
	_ = fs.Parse(args)

	var code string
	if fs.NArg() > 0 {
		code = fs.Arg(0)
	}

	logger := newLogger(*logLevel)

	identityURL := strings.TrimSpace(*overrideIdentity)
	if identityURL == "" {
		resolved, err := resolveActiveClusterIdentityURL(*clusterName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n\n", err)
			fmt.Fprintln(os.Stderr, "Run `memql cluster add <domain>` first to register + log into a cluster.")
			fmt.Fprintln(os.Stderr, "Or supply --identity <url> + --token <mql_wkr_...> to pair against an explicit identity service.")
			os.Exit(1)
		}
		identityURL = resolved
	}

	opts := PairOptions{
		PairingCode: code,
		IdentityURL: identityURL,
		Token:       *token,
		HomeID:      *homeID,
		Force:       *force,
		Logger:      logger,
	}
	if err := RunPairWizard(opts); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

// resolveActiveClusterIdentityURL reads ~/.memql/clusters.yaml and
// returns the IDENTITY service URL (the OIDC issuer) of the named
// cluster (or the SelectedCluster when name is empty). When only
// one cluster is registered, it's treated as active even without an
// explicit SelectedCluster stamp.
//
// Returns the identity URL because pairing redeems are HTTP calls
// to the identity service; the gRPC endpoint is for the worker's
// later WorkerService.Stream connection (returned by the redeem
// reply itself).
func resolveActiveClusterIdentityURL(name string) (string, error) {
	clusters, err := config.LoadClusters()
	if err != nil {
		return "", err
	}
	resolvedName := strings.TrimSpace(name)
	if resolvedName == "" {
		resolvedName = strings.TrimSpace(clusters.SelectedCluster)
	}
	if resolvedName == "" && len(clusters.Clusters) == 1 {
		resolvedName = clusters.Clusters[0].Name
	}
	if resolvedName == "" {
		return "", fmt.Errorf("no active cluster found in ~/.memql/clusters.yaml")
	}
	for _, c := range clusters.Clusters {
		if c.Name == resolvedName {
			issuer := strings.TrimSpace(c.Issuer)
			if issuer == "" {
				return "", fmt.Errorf("cluster %q has no identity issuer configured (re-run `memql cluster add <domain>`)", c.Name)
			}
			return issuer, nil
		}
	}
	return "", fmt.Errorf("cluster %q not found in ~/.memql/clusters.yaml", resolvedName)
}

func handleRun(args []string) {
	fs := flag.NewFlagSet("worker run", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to legacy worker.yaml (workers.yaml is preferred beside it)")
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml multi-home registry")
	cluster := fs.String("cluster", "", "cluster URL (overrides config; forces single-home mode)")
	tokenFile := fs.String("token-file", "", "path to a 0600 file whose contents are the worker token (overrides config)")
	name := fs.String("name", "", "worker name (overrides config)")
	logLevel := fs.String("log-level", "", "log level: debug | info | warn | error")
	metricsPort := fs.Int("metrics-port", 9100, "loopback port for the prometheus metrics endpoint (0 disables)")
	// --token (the literal token on argv) was retained as a hidden
	// alias for backwards-compatibility with legacy LaunchAgent /
	// systemd unit invocations. Tokens on argv leak into `ps`,
	// shell history, and process listings -- we accept the flag but
	// emit a loud WARN every time it's used. Prefer --token-file or
	// MEMQL_WORKER_TOKEN.
	tokenInline := fs.String("token", "", "DEPRECATED: worker token literal. Use --token-file or MEMQL_WORKER_TOKEN to avoid leaking into ps/shell history.")
	fs.Parse(args)

	legacyCfg, err := LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	if *cluster != "" {
		legacyCfg.ClusterURL = *cluster
	}
	// Token resolution order (each step overrides the previous):
	//   1. worker.yaml / workers.yaml (loaded below for multi-home).
	//   2. MEMQL_WORKER_TOKEN env var (in-process memory, not argv).
	//   3. --token-file path (read once, 0600 enforced).
	//   4. --token literal (DEPRECATED; logs a WARN).
	cliToken := ""
	if env := os.Getenv("MEMQL_WORKER_TOKEN"); env != "" {
		cliToken = env
	}
	if *tokenFile != "" {
		if err := config.VerifyCredentialFileMode(*tokenFile); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: --token-file: %v\n", err)
			os.Exit(1)
		}
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: --token-file: %v\n", err)
			os.Exit(1)
		}
		cliToken = strings.TrimSpace(string(raw))
	}
	if *tokenInline != "" {
		fmt.Fprintln(os.Stderr, "WARNING: --token <literal> leaks the token to `ps` and shell history.")
		fmt.Fprintln(os.Stderr, "WARNING: Use --token-file or MEMQL_WORKER_TOKEN instead. Continuing for backwards compatibility.")
		cliToken = *tokenInline
	}
	if cliToken != "" {
		legacyCfg.Token = cliToken
	}
	if *name != "" {
		legacyCfg.Name = *name
	}
	if *logLevel != "" {
		legacyCfg.LogLevel = *logLevel
	}

	// Single-home override: any explicit cluster/token on the CLI keeps
	// the legacy one-stream path (scripts, debugging). Otherwise load
	// the multi-home registry and run every enabled home.
	forceSingle := *cluster != "" || cliToken != ""
	var workers WorkersFile
	if !forceSingle {
		workers, err = LoadWorkers(*workersPath, *configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		if *name != "" {
			workers.WorkerName = *name
		}
		if *logLevel != "" {
			workers.LogLevel = *logLevel
		}
	}
	mode, err := decideRunMode(forceSingle, !legacyOnly(*workersPath), workers, legacyCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Configure the worker with one of:")
		fmt.Fprintln(os.Stderr, "  - ~/.memql/workers.yaml (one or more homes)")
		fmt.Fprintln(os.Stderr, "  - ~/.memql/worker.yaml (legacy single home; auto-migrates)")
		fmt.Fprintln(os.Stderr, "  - --cluster <url> --token-file <path>")
		if forceSingle {
			fmt.Fprintln(os.Stderr, "  - MEMQL_WORKER_TOKEN env var")
		}
		os.Exit(1)
	}
	singleHome := mode == runSingleHome

	logLevelEffective := legacyCfg.LogLevel
	if !singleHome && workers.LogLevel != "" {
		logLevelEffective = workers.LogLevel
	}
	logger := newLogger(logLevelEffective)
	if !singleHome && isMirrorOf(*configPath, *workersPath) {
		// The mirror names an enabled home, with the token it holds now,
		// or it goes -- including a token an earlier unpair left behind
		// (memql-cockpit#429). ONLY the mirror: a --config pointing
		// anywhere but beside workers.yaml is a file a person wrote, and
		// this must not rewrite or delete it.
		switch outcome, home, err := syncLegacyMirror(*configPath, workers); {
		case err != nil:
			logger.Warn("could not bring the legacy worker.yaml mirror in line with workers.yaml", "path", *configPath, "error", err)
		case outcome == MirrorRewritten:
			logger.Info("the legacy worker.yaml mirror named no enabled home; it now names one", "path", *configPath, "home", home)
		case outcome == MirrorDeleted:
			logger.Info("deleted the legacy worker.yaml mirror: it named no enabled home, and none is left", "path", *configPath)
		}
	}
	if mode == runNoHomes {
		interactive := isInteractiveTTY()
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		if code := waitWithoutHomes(logger, os.Stderr, *workersPath, workers, interactive, signals); code != 0 {
			os.Exit(code)
		}
		return
	}

	policyDir := filepath.Dir(*configPath)
	if !singleHome {
		policyDir = filepath.Dir(*workersPath)
	}
	policyPath := filepath.Join(policyDir, "policy.yaml")
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		logger.Warn("policy load failed; using defaults", "error", err)
		policy = tools.DefaultPolicy()
	}
	logLevelProblems(logger, policy)

	// What no app session may touch because it is this worker's own: the
	// tokens, policy.yaml's consent gate, the state (apppaths.go).
	stateDirs := []string{legacyCfg.StateDir}
	if !singleHome {
		stateDirs = append(stateDirs, workers.machineRoot())
	}
	workerPaths := sessionWorkerPaths(*configPath, *workersPath, stateDirs, *tokenFile, consent.DefaultSocketPath())

	// Consent gate (memql-cockpit#64). ONE socket for the whole
	// supervisor, and one window PER CLUSTER behind it (memql-cockpit
	// #433): each home's dispatcher asks its own home's Manager, so a
	// window opened for one cluster admits nothing another dispatches.
	consentHomes := consent.NewPersistentHomes(filepath.Join(policyDir, "consent"))
	consentSrv := consent.NewServer(consentHomes, consent.DefaultSocketPath(), logger)
	consentCtx, consentCancel := context.WithCancel(context.Background())
	if err := consentSrv.Listen(consentCtx); err != nil {
		logger.Warn("consent socket failed to start; running with default-deny gate but no IPC control",
			"error", err)
	}
	defer consentCancel()

	toolsFor := homeDispatchers(logger, policy, consentHomes)

	var metrics *Metrics
	if *metricsPort > 0 {
		metrics = NewMetrics()
		if err := metrics.Listen(*metricsPort); err != nil {
			logger.Warn("metrics endpoint failed to start; continuing without metrics", "error", err)
			metrics = nil
		} else {
			logger.Info("metrics endpoint listening", "addr", metrics.ListenAddr())
		}
	}

	// Before the first lookup of any binary: a LaunchAgent's PATH has no
	// directory an app installer uses (servicepath.go).
	ensureServicePath(logger)

	discoverer := newDiscoverer()
	modelInventory := NewModelInventory(policy, discoverer)
	appInventories := NewAppInventories(policy, nil)

	ctx, cancel := context.WithCancel(context.Background())
	// The worker's own record of the ceiling it advertises, once per served
	// model, at start and after every reload (announceModelCeilings).
	go announceModelCeilings(ctx, logger, modelInventory)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	var (
		fleet  *Fleet
		runner *Runner
		// stopSessions is invoked on SIGTERM so MCP configs die with us.
		stopSessions func(string)
	)

	if singleHome {
		legacyCfg.Home = HomeIDFromURL(legacyCfg.ClusterURL)
		sessions := appsession.NewManager(appsession.Options{
			Logger:     logger,
			StateDir:   legacyCfg.StateDir,
			ClusterURL: legacyCfg.ClusterURL,
			Allowed:    appsAllowedFor(policy, legacyCfg.Home),
			// The owner's apps.levels, read per session so a SIGHUP
			// reaches the next one (memql-cockpit#438).
			Levels:         policy.AppLevels,
			CheckWorkspace: policy.CheckPath,
			// Where a session goes when the engine names no workspace:
			// under fs.workspace_root when the owner set one, filed by
			// this home either way (appsession/workspace.go).
			Home:          legacyCfg.Home,
			WorkspaceRoot: policy.WorkspaceRoot,
			// What a session may neither run in nor let its app read or
			// write (appsession/protected.go).
			WorkerPaths: workerPaths,
			DenyPaths:   policy.DenyPaths,
		})
		stopSessions = sessions.StopAll
		calls := modelcall.NewManager(modelcall.Options{
			Logger:    logger,
			Inventory: modelInventory,
		})
		backups := backup.New(backup.Options{
			Logger:            logger,
			StateDir:          legacyCfg.StateDir,
			BaseURL:           backupBaseURL(legacyCfg.ClusterURL),
			Bearer:            backupBearer(legacyCfg.ClusterURL, logger),
			CheckPath:         policy.CheckBackupPath,
			CheckDownloadPath: policy.CheckBackupDownloadPath,
			HTTPClient:        &http.Client{Timeout: 0},
		})
		runner, err = NewRunner(Options{
			Logger:   logger,
			Config:   legacyCfg,
			Tools:    toolsFor(legacyCfg.Home),
			Apps:     appInventories.For(legacyCfg.Home),
			Models:   modelInventory,
			Calls:    calls,
			Sessions: sessions,
			Metrics:  metrics,

			PipelinesPolicy: policy.Pipelines,
			ModelPull: &ModelPullOptions{
				PolicyPath:   policyPath,
				OllamaBase:   discoverer.ResolvedOllamaBaseURL,
				PullAllowed:  policy.ModelsPullAllowed,
				ReloadPolicy: policy.Reload,
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		go backups.Run(ctx, runner.RegistrationId)
		logger.Info("worker starting (single-home)",
			"cluster_url", legacyCfg.ClusterURL,
			"name", legacyCfg.Name,
			"capabilities", legacyCfg.Capabilities,
		)
	} else {
		fleet, err = NewFleet(FleetOptions{
			Logger:      logger,
			Workers:     workers,
			WorkersPath: *workersPath,
			Policy:      policy,
			PolicyPath:  policyPath,
			WorkerPaths: workerPaths,
			ToolsFor:    toolsFor,
			AppsFor:     appInventories.For,
			Models:      modelInventory,
			Discoverer:  discoverer,
			Metrics:     metrics,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		logger.Info("worker starting (multi-home fleet)",
			"homes", len(workers.EnabledHomes()),
			"name", workers.WorkerName,
			"capabilities", workers.Capabilities,
		)
	}

	if fleet != nil {
		controlListener, err := listenControl(ctx, defaultControlPath(), fleet)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: worker control: %v\n", err)
			cancel()
			os.Exit(1)
		}
		defer controlListener.Close()
	}

	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				if reloadPolicy(logger, policy) {
					// Fan out to every home stream.
					if fleet != nil {
						fleet.RequestImmediateReadvertise()
					} else if runner != nil {
						runner.RequestImmediateReadvertise()
					}
					// After the request above, which drops the cached inventory:
					// the ceilings are then read under the policy just loaded.
					go announceModelCeilings(ctx, logger, modelInventory)
				}
			default:
				logger.Info("worker shutting down", "signal", sig.String())
				if stopSessions != nil {
					stopSessions("the cockpit is shutting down")
				}
				cancel()
				return
			}
		}
	}()

	restoreTermios := enableQuitHotkeys(ctx, cancel, logger)
	defer restoreTermios()
	stopPipelineMaintenance := tools.StartPipelineMaintenance(ctx, logger)
	defer stopPipelineMaintenance()

	var runErr error
	if fleet != nil {
		runErr = fleet.Run(ctx)
	} else {
		runErr = runner.Run(ctx)
	}
	if runErr != nil && ctx.Err() == nil {
		logger.Error("worker exited with error", "error", runErr)
		if metrics != nil {
			metrics.Stop()
		}
		os.Exit(1)
	}
	if metrics != nil {
		metrics.Stop()
	}
}

// setupNonInteractive makes the permission pre-flight report-and-fail
// instead of pausing for approval -- for scripted installs and CI.
var setupNonInteractive bool

// setupFlags is `worker setup`'s command line, parsed.
//
// A struct rather than five locals so the ONE decision this command
// makes -- which of two entirely different things it is -- is assertable
// in a test without a terminal, a machine, or an os.Exit. See
// TestSetupFlags.
type setupFlags struct {
	workbench      bool
	nonInteractive bool
	inference      bool
	configPath     string
	runtimeFlag    string
	modelIDs       []string
}

func parseSetupFlags(args []string) setupFlags {
	fs := flag.NewFlagSet("worker setup", flag.ExitOnError)
	nonInteractive := fs.Bool("non-interactive", false, "never prompt; report what is missing and exit 3 (a question could not be asked), 4 (not granted) or 5 (probe failed)")
	workbench := fs.Bool("workbench", false, "enable isolated Fleet workbenches with resource limits")
	inference := fs.Bool("inference", false, "set this machine up to serve local models: runtime, models, models.allow")
	configPath := fs.String("config", DefaultConfigPath(), "path to worker.yaml (its directory holds policy.yaml)")
	runtimeFlag := fs.String("runtime", "", "with --inference: docker | native, overriding the runtime this platform would choose. On its own: kokoro | image, to install that runtime")
	var modelIDs repeatedFlag
	fs.Var(&modelIDs, "model", "with --inference: a model id to pull; repeatable (default: the recommended set for this machine's class)")
	_ = fs.Parse(args)
	return setupFlags{
		workbench: *workbench, nonInteractive: *nonInteractive,
		inference:   *inference,
		configPath:  *configPath,
		runtimeFlag: *runtimeFlag,
		modelIDs:    modelIDs,
	}
}

// runtimeModeFor decides which of --runtime's two meanings applies.
//
// It returns ("", false) when the flag is absent or names a model
// runtime alongside --inference (the existing path, unchanged); the
// runtime name when this is an install; and (sentence, true) for a
// combination that is refused.
//
// A PURE FUNCTION of the parsed flags, so every combination is
// assertable without a terminal or a machine -- the same reason
// setupFlags is a struct.
func runtimeModeFor(f setupFlags) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(f.runtimeFlag))
	if name == "" {
		return "", false
	}

	installable := false
	for _, r := range inference.InstallableRuntimes() {
		if name == r {
			installable = true
			break
		}
	}

	switch {
	case installable && f.inference:
		return wrapJoin(fmt.Sprintf(
			"--runtime %s installs a speech or image runtime, and --runtime docker or native chooses"+
				" how Ollama runs. They are different questions, so they are different commands: run"+
				" memql worker setup --runtime %s on its own.", name, name)), true

	case installable:
		return name, false

	case f.inference:
		// docker | native alongside --inference: the existing meaning.
		// applyRuntimeFlag validates it and refuses a combination the
		// platform cannot serve, in its own words.
		return "", false

	case name == "docker" || name == "native":
		return wrapJoin(fmt.Sprintf(
			"--runtime %s chooses how Ollama runs, which is a question only --inference asks."+
				" Run memql worker setup --inference --runtime %s, or drop the flag.", name, name)), true

	default:
		return wrapJoin(fmt.Sprintf(
			"--runtime takes docker or native with --inference, or %s on its own. It does not take %q.",
			strings.Join(inference.InstallableRuntimes(), " or "), f.runtimeFlag)), true
	}
}

// wrapJoin renders a refusal at the width the rest of this surface
// wraps to, so a usage error and a plan refusal look like one product.
func wrapJoin(text string) string {
	return strings.Join(wrapText(text, inferenceWrapWidth), "\n")
}

func handleSetup(args []string) {
	f := parseSetupFlags(args)
	setupNonInteractive = f.nonInteractive
	if f.workbench {
		if f.inference || f.runtimeFlag != "" || len(f.modelIDs) > 0 {
			fmt.Fprintln(os.Stderr, "Run --workbench separately from model setup.")
			os.Exit(SetupExitUsage)
		}
		if err := setupWorkbench(context.Background(), f.configPath, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "Fleet workbench setup could not finish:", err)
			os.Exit(1)
		}
		return
	}

	// --inference IS ITS OWN COMMAND, and the branch is here rather than
	// inside the wizard so the computer-use pre-flight is UNTOUCHED when
	// the flag is absent. That pre-flight is reached by `worker pair` on
	// every machine that enrolls; folding a model setup into it would
	// mean an enrollment could now fail for a reason that has nothing to
	// do with the permissions it was asking about -- on the majority of
	// machines, which are below the hardware floor and were never going
	// to serve a model.
	// --runtime MEANS TWO DIFFERENT THINGS, and the ambiguity is
	// resolved by refusing the mix rather than by guessing.
	//
	// Alongside --inference it chooses HOW OLLAMA RUNS (docker or
	// native). On its own it installs an ADDITIONAL runtime (kokoro or
	// image). Those are different questions -- one is about a runtime
	// this machine is getting either way, the other about whether it
	// gets a second one at all -- and a person who typed the wrong
	// combination is better served by a sentence than by whichever
	// meaning happened to win.
	if verdict, refused := runtimeModeFor(f); refused {
		fmt.Fprintln(os.Stdout, verdict)
		os.Exit(SetupExitUsage)
	} else if verdict != "" {
		os.Exit(runRuntimeSetup(context.Background(),
			newRuntimeSetup(f.configPath, verdict, f.nonInteractive)))
	}

	if f.inference {
		os.Exit(runInferenceSetup(context.Background(),
			newInferenceSetup(f.configPath, f.modelIDs, f.nonInteractive, f.runtimeFlag)))
	}

	if err := runSetupWizard(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		// The wizard's own code, not a flat 1: an install script reading
		// `$?` has to be able to tell "a permission has not been granted"
		// (exit 4, the operator has an action) from "the probe itself
		// failed" (exit 5). See setup_exit.go.
		os.Exit(SetupExitCode(err))
	}
}

func handleConfig(args []string) {
	fs := flag.NewFlagSet("worker config", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to legacy worker.yaml")
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml")
	fs.Parse(args)

	w, err := LoadWorkers(*workersPath, *configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Workers file: %s\n", *workersPath)
	fmt.Printf("Machine id:   %s\n", machineIDLine(w))
	fmt.Printf("Name:         %s\n", emptyOrValue(w.WorkerName))
	fmt.Printf("Capabilities:%s\n", " "+strings.Join(w.Capabilities, ", "))
	fmt.Printf("Concurrency:  %v\n", w.Concurrency)
	fmt.Printf("State dir:    %s\n", w.StateDir)
	fmt.Printf("Log level:    %s\n", w.LogLevel)
	fmt.Printf("Homes:        %d (%d enabled)\n", len(w.Homes), len(w.EnabledHomes()))
	_, duplicates := w.runnableHomes()
	duplicateOf := make(map[string]string, len(duplicates))
	for _, d := range duplicates {
		duplicateOf[d.Home.ID] = d.Connects
	}
	if len(w.Homes) == 0 {
		fmt.Println("\n(no homes enrolled — run `memql worker pair` or the install script)")
	}
	for _, h := range w.Homes {
		en := "enabled"
		if !h.IsEnabled() {
			en = "disabled"
		}
		fmt.Printf("\n  [%s] %s\n", h.ID, en)
		fmt.Printf("    Cluster URL: %s\n", emptyOrValue(h.ClusterURL))
		fmt.Printf("    Token:       %s\n", maskToken(h.Token))
		stateDir := w.ConfigForHome(h).StateDir
		fmt.Printf("    State dir:   %s\n", stateDir)
		if connects, dup := duplicateOf[h.ID]; dup {
			for i, line := range duplicateHomeLines(h.ID, connects) {
				label := "                 "
				if i == 0 {
					label = "    Status:      "
				}
				fmt.Printf("%s%s\n", label, line)
			}
			continue
		}
		// A refusal the running worker recorded (memql-cockpit#427). The
		// worker is another process; the record is how it tells this one.
		rec, ok := loadRefusal(stateDir)
		for i, line := range describeRefusal(rec, ok, h) {
			label := "                 "
			if i == 0 {
				label = "    Status:      "
			}
			fmt.Printf("%s%s\n", label, line)
		}
	}
	if err := w.Validate(); err != nil {
		fmt.Printf("\nValidation: ERROR -- %v\n", err)
	} else if err := w.ValidateRun(); err != nil {
		fmt.Printf("\nValidation: OK (registry); run: %v\n", err)
	} else {
		fmt.Printf("\nValidation: OK\n")
	}
}

func handleUnpair(args []string) {
	fs := flag.NewFlagSet("worker unpair", flag.ExitOnError)
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml")
	configPath := fs.String("config", "", "path to the legacy worker.yaml mirror (default: the worker.yaml beside --workers)")
	cluster := fs.String("cluster", "", "home id to remove (see `memql worker config`)")
	clusterURL := fs.String("cluster-url", "", "remove all enrollments for this cluster URL")
	dryRun := fs.Bool("dry-run", false, "preview URL-scoped removal without writing")
	jsonOutput := fs.Bool("json", false, "print a token-free URL-scoped result")
	disable := fs.Bool("disable", false, "disable the home instead of removing it")
	fs.Parse(args)
	if strings.TrimSpace(*configPath) == "" {
		*configPath = mirrorPathFor(*workersPath)
	}
	if *clusterURL != "" {
		if *cluster != "" || *disable || fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "ERROR: --cluster-url cannot be combined with a home id or --disable")
			os.Exit(2)
		}
		res, err := RemoveHomesByURL(*workersPath, *configPath, *clusterURL, *dryRun)
		if err != nil {
			// Parser errors can contain configuration values. Keep credentials out
			// of installer logs even when a hand-edited YAML file is malformed.
			fmt.Fprintln(os.Stderr, "ERROR: scoped unpair could not safely read or update enrollment files; files may need repair")
			os.Exit(5)
		}
		if *jsonOutput {
			_ = json.NewEncoder(os.Stdout).Encode(res)
		} else {
			fmt.Printf("Matched %d enrollment(s); %d other enrollment(s) remain (preview: %t).\n", res.Removed, res.Remaining, res.DryRun)
		}
		return
	}
	if *dryRun || *jsonOutput {
		fmt.Fprintln(os.Stderr, "ERROR: --dry-run and --json require --cluster-url")
		os.Exit(2)
	}
	id := strings.TrimSpace(*cluster)
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "ERROR: unpair requires --cluster <home-id>")
		os.Exit(1)
	}
	res, err := RemoveHome(*workersPath, *configPath, id, *disable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		if res.Removed.ID != "" {
			fmt.Fprintf(os.Stderr, "Delete it yourself so nothing can reconnect with that token:\n  rm %s\n", *configPath)
		}
		os.Exit(1)
	}
	for _, line := range unpairSentences(res, *disable, *configPath, runtime.GOOS) {
		fmt.Println(line)
	}
}

// unpairSentences is what `worker unpair` prints: what changed in both
// files, and what the running worker still holds. A pure function of the
// result, so every path is asserted on its exact words.
func unpairSentences(res RemoveHomeResult, disabled bool, mirrorPath, goos string) []string {
	verb := "Removed"
	if disabled {
		verb = "Disabled"
	}
	remain := len(res.Workers.Homes)
	enabled := len(res.Workers.EnabledHomes())
	lines := []string{fmt.Sprintf("%s home %q (%s left, %d enabled).", verb, res.Removed.ID, plural(remain, "home"), enabled)}
	if remain == 0 {
		lines[0] = fmt.Sprintf("%s home %q. It was the last one.", verb, res.Removed.ID)
	}
	switch res.Mirror {
	case MirrorRewritten:
		lines = append(lines, fmt.Sprintf("The legacy mirror %s now names %q.", mirrorPath, res.MirrorHome))
	case MirrorDeleted:
		lines = append(lines, fmt.Sprintf("Deleted the legacy mirror %s: no enabled home is left for it to name, so no token stays behind in it.", mirrorPath))
	}
	lines = append(lines, fmt.Sprintf("The running worker keeps its stream to %s until it restarts:", res.Removed.ID))
	lines = append(lines, "  "+restartWorkerCommand(goos))
	if enabled == 0 {
		lines = append(lines, "With no cluster enabled it then waits, connected to nothing. After you pair one (memql worker pair <code>), restart it the same way.")
	}
	return lines
}

// restartWorkerCommand is how a person restarts the installed worker on
// this platform -- the LaunchAgent on macOS, the user unit on Linux.
func restartWorkerCommand(goos string) string {
	if goos == "darwin" {
		return "launchctl kickstart -k gui/$(id -u)/com.znasllc.memql-worker"
	}
	return "systemctl --user restart memql-worker"
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// runMode is what `worker run` does with the configuration it found.
type runMode int

const (
	// runFleet: every enabled home in workers.yaml, one stream each.
	runFleet runMode = iota
	// runSingleHome: one stream, from worker.yaml and the flags.
	runSingleHome
	// runNoHomes: a good registry with nothing enabled -- see
	// waitWithoutHomes.
	runNoHomes
)

// decideRunMode is `worker run`'s one decision, as a pure function of
// what it read -- so the path that brought an unpaired cluster back is
// pinned by a test rather than by care (memql-cockpit#429).
//
// forceSingle is --cluster or a token on the command line.
// workersExists is whether workers.yaml exists AFTER LoadWorkers, which
// migrates a legacy-only machine by writing one.
//
// THE ONLY FALLBACK to worker.yaml is a machine that has never had a
// workers.yaml. Once workers.yaml exists it is the whole truth. It used to
// be enough for workers.yaml to have nothing ENABLED -- which is exactly
// the state `worker unpair` leaves -- and the worker.yaml mirror then
// brought the unpaired cluster straight back on the next start.
func decideRunMode(forceSingle, workersExists bool, workers WorkersFile, legacy Config) (runMode, error) {
	if forceSingle {
		return runSingleHome, legacy.Validate()
	}
	runErr := workers.ValidateRun()
	switch {
	case runErr == nil:
		return runFleet, nil
	case !workersExists && legacy.Validate() == nil:
		return runSingleHome, nil
	case workers.Validate() == nil:
		return runNoHomes, nil
	default:
		return runFleet, runErr
	}
}

// legacyOnly reports whether this machine has never had a workers.yaml --
// the one situation `worker run` still reads worker.yaml as a home.
func legacyOnly(workersPath string) bool {
	_, err := os.Stat(workersPath)
	return errors.Is(err, os.ErrNotExist)
}

// waitWithoutHomes is `worker run` with nothing to run: a registry with no
// homes left -- the state `worker unpair` leaves behind. A registry with
// paused homes runs the fleet so its local controls can resume them.
//
// At a terminal it says so and exits 1, because a person is waiting for
// the prompt. Under the LaunchAgent or the user unit it says so ONCE and
// WAITS: both restart a worker that exits (KeepAlive, Restart=on-failure),
// and a worker that exits for want of a home would be restarted into the
// same sentence every few seconds, forever. Connected to nothing and quiet
// is the honest state for a machine that was unpaired on purpose, until it
// is restarted after the next pairing (the macOS pair flow reinstalls the
// LaunchAgent, which does that; elsewhere a person restarts the unit).
//
// SIGHUP DOES NOT END THE WAIT. `worker models --allow` and `setup
// --inference` send it to every running worker to reload its policy, and
// a worker that died of one would not come back under systemd, which
// counts death by SIGHUP as a clean exit.
func waitWithoutHomes(logger *slog.Logger, stderr io.Writer, workersPath string, w WorkersFile, interactive bool, signals <-chan os.Signal) int {
	sentence := noEnabledHomeSentence(workersPath, w)
	if interactive {
		fmt.Fprintln(stderr, "ERROR: "+sentence)
		return 1
	}
	logger.Warn("no cluster is enabled on this machine; this worker waits, connected to nothing, until it is restarted",
		"why", sentence)
	for sig := range signals {
		if sig == syscall.SIGHUP {
			logger.Info("policy reload requested; nothing to reload while no cluster is enabled")
			continue
		}
		logger.Info("worker shutting down", "signal", sig.String())
		return 0
	}
	return 0
}

// noEnabledHomeSentence names the empty state precisely: never paired,
// or paired with everything switched off.
func noEnabledHomeSentence(workersPath string, w WorkersFile) string {
	if len(w.Homes) == 0 {
		return fmt.Sprintf("No cluster is paired with this machine (%s has no homes). Pair one with a code from the portal: memql worker pair <code>", workersPath)
	}
	ids := make([]string, 0, len(w.Homes))
	for _, h := range w.Homes {
		ids = append(ids, h.ID)
	}
	return fmt.Sprintf("Every cluster in %s is disabled (%s). Set enabled: true on one, or pair another with memql worker pair <code>, then restart the worker.", workersPath, strings.Join(ids, ", "))
}

// duplicateHomeLines is the block `worker config` prints under an enabled
// home that does not connect, because a later one points at the same
// cluster (runnableHomes).
func duplicateHomeLines(id, connects string) []string {
	return []string{
		fmt.Sprintf("NOT CONNECTED -- %q points at the same cluster, and it connects instead.", connects),
		"One machine holds one stream per cluster. Remove this home, or set enabled: false on it:",
		"  memql worker unpair --cluster " + id,
	}
}

// machineIDLine is what `worker config` prints for this machine's id: the
// id every cluster sees it as (memql-cockpit#430), read without minting
// one -- printing a config must not be what creates an identity.
func machineIDLine(w WorkersFile) string {
	if id := peekMachineID(w.machineRoot()); id != "" {
		return id
	}
	return "(none yet; the worker creates one when it first connects)"
}

// homeDispatchers builds each home's tool dispatcher over its own consent
// window. The dispatcher is cheap -- a logger, the policy, a gate -- so a
// home gets its own rather than a shared one that would have to be told
// which cluster every call came from.
func homeDispatchers(logger *slog.Logger, policy *tools.Policy, homes *consent.Homes) func(string) ToolDispatcher {
	return func(id string) ToolDispatcher {
		return tools.NewHomeDispatcher(logger.With("home", id), policy, homes.For(id), id)
	}
}

func printUsage() {
	fmt.Println("memql worker -- run cockpit as a memql worker")
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  memql worker pair <code>   Redeem an XXXX-XXXX pairing code from")
	fmt.Println("                                     CoPresent's Settings -> Computer Use card,")
	fmt.Println("                                     upsert a home in workers.yaml, run worker.")
	fmt.Println("                                     Additive: sibling homes are kept.")
	fmt.Println("  memql worker               Same flow with a paste prompt (no code arg).")
	fmt.Println("  memql worker run           Run an already-configured worker (used by")
	fmt.Println("                                     LaunchAgent / scripts).")
	fmt.Println("  memql worker setup         Re-run TCC permissions check (computeruse builds only).")
	fmt.Println("  memql worker setup --inference")
	fmt.Println("                                     Set this machine up to serve local models:")
	fmt.Println("                                     the runtime, the models, and models.allow.")
	fmt.Println("  memql worker setup --runtime kokoro")
	fmt.Println("                                     Install the speech runtime, so this machine")
	fmt.Println("                                     can serve text to speech. --runtime image")
	fmt.Println("                                     does the same for image generation.")
	fmt.Println("  memql worker config        Print the effective config (all homes).")
	fmt.Println("  memql worker unpair --cluster <id>")
	fmt.Println("                                     Remove (or --disable) one home; siblings stay.")
	fmt.Println("  memql worker models        Print the local models this machine would offer,")
	fmt.Println("                                     or the reason it offers none. --pull <id>")
	fmt.Println("                                     pulls one; --allow <id> offers one.")
	fmt.Println("  memql worker apps          Print the local apps (Claude Code, Codex), which")
	fmt.Println("                                     clusters can use them here and why not, and")
	fmt.Println("                                     the model and effort each level runs them at.")
	fmt.Println("                                     --allow <id> [--home <cluster>] allows one for")
	fmt.Println("                                     one cluster; --deny <id> withdraws it.")
	fmt.Println("  memql worker hardware      Print what this machine reports about itself:")
	fmt.Println("                                     chip, memory, GPU, runtimes, and the class")
	fmt.Println("                                     that decides which models are recommended.")
	fmt.Println("  memql worker probe         Measure a local model against the probe suite:")
	fmt.Println("                                     structured output, tool calls, throughput.")
	fmt.Println("  memql worker backup        Print the folders this machine backs up into the")
	fmt.Println("                                     Library, or the reason it backs up none.")
	fmt.Println("                                     --once runs one sweep now.")
	fmt.Println("  memql worker control       Local status/logs and per-server pause/resume (current OS user).")
	fmt.Println("  memql worker consent <op>  Manage the consent gate, one window per cluster")
	fmt.Println("                                     (grant/revoke/status/watch; --cluster <id>).")
	fmt.Println("")
	fmt.Println("PAIR FLAGS")
	fmt.Println("  --cluster <name>     Cluster NAME from clusters.yaml (identity lookup).")
	fmt.Println("  --home-id <id>       Home id in workers.yaml (default: URL host / cluster name).")
	fmt.Println("  --force              Remap a home id onto a different cluster_url (siblings kept).")
	fmt.Println("  --identity <url>     Advanced: override identity service URL.")
	fmt.Println("  --token <token>      Advanced: worker token (skip redeem).")
	fmt.Println("  --log-level <l>      Log level: debug | info | warn | error")
	fmt.Println("")
	fmt.Println("RUN FLAGS")
	fmt.Println("  --workers <path>     Path to workers.yaml (default ~/.memql/workers.yaml)")
	fmt.Println("  --config <path>      Legacy worker.yaml (default ~/.memql/worker.yaml)")
	fmt.Println("  --cluster <url>      Cluster URL (forces single-home mode)")
	fmt.Println("  --token <token>      Worker token (mql_wkr_...; overrides config + MEMQL_WORKER_TOKEN)")
	fmt.Println("  --name <name>        Worker name (overrides config)")
	fmt.Println("  --log-level <l>      Log level: debug | info | warn | error")
	fmt.Println("  --metrics-port <p>   Loopback port for prometheus metrics (default 9100; 0 disables)")
	fmt.Println("")
	fmt.Println("SETUP --INFERENCE FLAGS")
	fmt.Println("  --model <id>         Model to pull; repeatable. The default is the set")
	fmt.Println("                       recommended for this machine's class; run")
	fmt.Println("                       memql worker hardware to see it.")
	fmt.Println("  --runtime <r>        docker | native, overriding the runtime this platform")
	fmt.Println("                       would choose. A combination the platform cannot serve")
	fmt.Println("                       is refused rather than ignored. Without --inference the")
	fmt.Println("                       flag takes kokoro | image instead and installs that")
	fmt.Println("                       runtime; the two meanings are never mixed silently.")
	fmt.Println("  --non-interactive    Never ask. A runtime install it would have asked about")
	fmt.Println("                       is refused with exit 3 and nothing is installed.")
}

// logLevelProblems logs, each time policy.yaml is read -- at start, and on
// every SIGHUP through reloadPolicy -- what is wrong with the blocks whose
// mistakes would otherwise do nothing and say nothing. Three lists, each a
// sentence an owner can act on:
//
//   - the apps consent entries that allow nothing, including the retired
//     machine-wide apps.allow. A consent that silently did nothing looks,
//     from the cluster, exactly like a machine with no app;
//   - the apps.levels entries the app would misread (memql-cockpit#438). A
//     refused entry also says so on every session it refuses, on the End the
//     person who asked will read, but an entry for an app or a level no
//     session names says so NOWHERE else. `memql worker apps` prints the same
//     list for an owner who is not reading this log;
//   - the models.parallel and models.resident entries that cannot stand, and
//     any key under models: that nothing reads (docs/local-models.md, "Keeping
//     a model loaded"). `memql worker models` prints this list too.
//
// An owner's entry that silently did nothing reads exactly like one that
// worked, which is why every list is logged and not only the one that fails
// loudest.
func logLevelProblems(logger *slog.Logger, policy *tools.Policy) {
	for _, problem := range policy.AppConsentProblems() {
		logger.Warn("policy.yaml app consent has a problem", "problem", problem)
	}
	for _, problem := range policy.AppLevelProblems() {
		logger.Warn("policy.yaml apps.levels has a problem", "problem", problem)
	}
	for _, problem := range policy.ModelsTuningProblems() {
		logger.Warn("policy.yaml models has a problem", "problem", problem)
	}
}

// reloadPolicy re-reads policy.yaml on SIGHUP and says what changed that the
// cluster has to be told about: the consents that ride Register. It reports
// whether the policy reloaded; the caller then asks every stream to
// re-advertise.
//
// EVERY RUN PATH CALLS THIS (TestEverySIGHUPReloadsThroughOneFunction). A
// path that reloaded by hand announced nothing -- `worker pair`'s run said
// nothing about repository scopes -- and the announcement is how a person
// reading the log learns why the worker is about to reconnect.
func reloadPolicy(logger *slog.Logger, policy *tools.Policy) bool {
	pipelinesBefore := policy.Pipelines().AdvertisementFingerprint()
	if err := policy.Reload(); err != nil {
		logger.Warn("policy reload failed", "error", err)
		return false
	}
	logger.Info("policy reloaded")
	logLevelProblems(logger, policy)
	if after := policy.Pipelines().AdvertisementFingerprint(); after != pipelinesBefore {
		// So does the pipelines label (memql#5494). A step already routed
		// here on the old answer meets the new one when it arrives: the
		// policy is read live.
		logger.Info("pipeline repository policy changed; every cluster stream re-registers to carry it")
	}
	return true
}

func newLogger(level string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func emptyOrValue(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func maskToken(t string) string {
	if t == "" {
		return "(unset)"
	}
	if len(t) <= 16 {
		return "(set)"
	}
	return t[:12] + "..." + t[len(t)-4:]
}
