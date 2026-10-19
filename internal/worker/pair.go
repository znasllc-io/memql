package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
	"github.com/znasllc-io/memql/component/identity/workerpairing"
)

// PairOptions configures the pairing wizard's entry conditions.
//
// Three supported invocation shapes:
//
//  1. PairingCode != ""         -- code came from the command line.
//     Wizard runs paste -> redeem ->
//     yaml -> tcc -> run.
//  2. IdentityURL + Token != "" -- advanced "I already have a token,
//     skip redemption" path. Wizard
//     needs ClusterURL too (advanced
//     callers supply both).
//  3. all empty                 -- no-args entry. Wizard prompts for
//     the code interactively.
//
// IdentityURL is the public origin of the memQL identity service
// (e.g. https://identity.acme.com). The pairing redeem POSTs to
// `${IdentityURL}/pair/redeem`. ClusterURL is the gRPC dial address
// the worker uses for WorkerService.Stream after redemption -- it
// comes back from the redeem response, NOT from the cockpit's
// configuration.
type PairOptions struct {
	PairingCode string
	IdentityURL string
	ClusterURL  string
	Token       string
	HomeID      string // optional workers.yaml home id
	Force       bool   // replace matched home only
	Logger      *slog.Logger
}

// RunPairWizard is the front-door entry for `memql worker
// pair` and `memql worker` (no args). Coordinates the
// pairing-code redeem, yaml write-out, TCC pre-flight, and
// transition into the worker run loop.
//
// On TCC restart needs (post-grant probe still denied), the wizard
// spills its state to ~/.memql/wizard-state.json and `syscall.Exec`s
// itself with the same args. The fresh process picks up the state
// and resumes at the same step with re-evaluated TCC -- same shell,
// no Terminal restart needed.
func RunPairWizard(opts PairOptions) error {
	logger := opts.Logger
	if logger == nil {
		logger = newLogger("info")
	}

	// Resume from a state-file spill if the previous process
	// self-exec'd to pick up TCC grants. Do this BEFORE consulting
	// command-line args so the resume takes precedence over a
	// re-pass of the original code.
	if state := LoadWizardState(); state != nil {
		logger.Info("resuming wizard after self-exec",
			"step", state.Step,
			"reason", state.Reason,
		)
		opts.PairingCode = state.PairingCode
		opts.IdentityURL = state.IdentityURL
		opts.ClusterURL = state.ClusterURL
		opts.Token = state.Token
	}

	// Resolve the pairing code if needed (interactive paste).
	if opts.Token == "" {
		code := strings.TrimSpace(opts.PairingCode)
		if code == "" {
			pasted, err := promptForPairingCode()
			if err != nil {
				return err
			}
			code = pasted
		}
		canonical := workerpairing.Canonicalize(code)
		if canonical == "" {
			return fmt.Errorf("pair: %q is not a valid pairing code (8 chars from A-Z + 2-9, no I/O/0/1)", code)
		}

		// Resolve identity URL: either supplied via --identity or
		// MEMQL_IDENTITY_URL, or asked interactively.
		identityURL := strings.TrimSpace(opts.IdentityURL)
		if identityURL == "" {
			fmt.Println("Identity service URL (e.g. http://localhost:8081):")
			if env := os.Getenv("MEMQL_IDENTITY_URL"); env != "" {
				identityURL = env
			} else {
				prompted, err := promptLine("Identity URL: ")
				if err != nil {
					return err
				}
				identityURL = strings.TrimSpace(prompted)
			}
		}
		if identityURL == "" {
			return errors.New("pair: identity URL required (run `memql cluster add <domain>` first or set MEMQL_IDENTITY_URL)")
		}

		fmt.Printf("Redeeming pairing code at %s ...\n", identityURL)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hostname, _ := os.Hostname()
		result, err := RedeemPairingCode(ctx, identityURL, canonical, hostname, logger)
		if err != nil {
			return fmt.Errorf("pair: redeem failed: %w", err)
		}
		opts.Token = result.PlainToken
		opts.IdentityURL = identityURL
		// The server tells us where workers should dial (the gRPC
		// host:port for WorkerService.Stream on the agent node) --
		// the cockpit never decides this on its own.
		opts.ClusterURL = result.ClusterURL
		fmt.Printf("Pairing code redeemed; worker token bound to user %s.\n", maskId(result.OwnerUserId))
	}

	if opts.Token == "" || opts.ClusterURL == "" {
		return errors.New("pair: empty token or cluster_url after redeem")
	}

	// Upsert this cluster as one home in workers.yaml (additive —
	// sibling homes are preserved). Legacy worker.yaml is mirrored
	// for transition. Same single LaunchAgent serves every home.
	hostname, _ := os.Hostname()
	if _, err := UpsertHome(UpsertHomeOptions{
		ClusterURL:   opts.ClusterURL,
		Token:        opts.Token,
		Name:         hostname,
		ID:           opts.HomeID,
		Force:        opts.Force,
		Capabilities: capabilitiesForBuildTag(),
	}); err != nil {
		return fmt.Errorf("pair: write workers.yaml: %w", err)
	}
	fmt.Printf("Enrolled home in %s (mirrored %s).\n", DefaultWorkersPath(), DefaultConfigPath())

	// TCC pre-flight (computeruse builds only). On the headless build,
	// runSetupWizard is a no-op-with-print; we skip it entirely so
	// the user doesn't see a confusing "no computer-use required" splash.
	if isInteractiveTTY() && BuildHasComputerUse() {
		fmt.Println()
		fmt.Println("Running computer-use permissions check...")
		if err := runSetupWizard(); err != nil {
			// runSetupWizard writes its own diagnostics; the wizard
			// continues into running the worker either way. A
			// non-zero exit from setup is treated as "computer-use tools may
			// be unavailable; HEADLESS still works."
			logger.Warn("permissions wizard reported issues; proceeding HEADLESS-only", "error", err)
		}
	}

	// Clear the state file once we're past the TCC step -- if a
	// self-exec was going to happen, it would have happened during
	// runSetupWizard. Anything from here on is a fresh run.
	ClearWizardState()

	// Transition into the worker run loop. The user runs ONE
	// command and ends up serving dispatched tool calls -- no
	// "now run memql worker run" homework.
	fmt.Println()
	fmt.Println("Starting worker. Press Ctrl+Q (or Ctrl+C) to stop and (optionally) install auto-start.")
	fmt.Println()
	if err := runConfiguredWorker(opts.ClusterURL, opts.Token, hostname, logger); err != nil {
		return err
	}

	// Worker stopped (user pressed Ctrl+C / Ctrl+Q). Offer to
	// install the LaunchAgent so future logins auto-start.
	if isInteractiveTTY() {
		offerLaunchAgentInstall(logger)
	}
	return nil
}

// runConfiguredWorker spins up the metrics endpoint + the fleet
// supervisor after a successful pair. Prefers workers.yaml (all
// enabled homes) so pairing a second cluster does not hide the
// first for the rest of this process; falls back to a single-home
// runner when the registry is unavailable.
func runConfiguredWorker(clusterURL, token, name string, logger *slog.Logger) error {
	policyPath := DefaultPolicyPath()
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		logger.Warn("policy load failed; using defaults", "error", err)
		policy = tools.DefaultPolicy()
	}
	logLevelProblems(logger, policy)

	// One consent window per cluster, as `worker run` has (memql-cockpit
	// #433).
	consentHomes := consent.NewPersistentHomes(filepath.Join(filepath.Dir(policyPath), "consent"))
	consentSrv := consent.NewServer(consentHomes, consent.DefaultSocketPath(), logger)
	consentCtx, consentCancel := context.WithCancel(context.Background())
	if err := consentSrv.Listen(consentCtx); err != nil {
		logger.Warn("consent socket failed to start; running with default-deny gate but no IPC control",
			"error", err)
	}
	defer consentCancel()

	toolsFor := homeDispatchers(logger, policy, consentHomes)

	metrics := NewMetrics()
	if err := metrics.Listen(9100); err != nil {
		logger.Warn("metrics endpoint failed to start; continuing without metrics", "error", err)
		metrics = nil
	} else {
		logger.Info("metrics endpoint listening", "addr", metrics.ListenAddr())
	}
	defer func() {
		if metrics != nil {
			metrics.Stop()
		}
	}()

	// As `worker run` does, before anything resolves a binary.
	ensureServicePath(logger)

	discoverer := newDiscoverer()
	modelInventory := NewModelInventory(policy, discoverer)
	appInventories := NewAppInventories(policy, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The worker's own record of the ceiling it advertises, once per served
	// model, at start and after every reload (announceModelCeilings).
	go announceModelCeilings(ctx, logger, modelInventory)

	workers, werr := LoadWorkers(DefaultWorkersPath(), DefaultConfigPath())
	useFleet := werr == nil && workers.ValidateRun() == nil

	var fleet *Fleet
	var runner *Runner
	if useFleet {
		if name != "" {
			workers.WorkerName = name
		}
		fleet, err = NewFleet(FleetOptions{
			Logger:     logger,
			Workers:    workers,
			Policy:     policy,
			PolicyPath: policyPath,
			ToolsFor:   toolsFor,
			AppsFor:    appInventories.For,
			Models:     modelInventory,
			Discoverer: discoverer,
			Metrics:    metrics,
		})
		if err != nil {
			return err
		}
	} else {
		cfg := Defaults()
		cfg.ClusterURL = clusterURL
		cfg.Token = token
		if name != "" {
			cfg.Name = name
		}
		cfg.Capabilities = capabilitiesForBuildTag()
		cfg.Home = HomeIDFromURL(clusterURL)
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("pair-run: %w", err)
		}
		runner, err = NewRunner(Options{
			Logger:  logger,
			Config:  cfg,
			Tools:   toolsFor(cfg.Home),
			Apps:    appInventories.For(cfg.Home),
			Models:  modelInventory,
			Metrics: metrics,

			PipelinesPolicy: policy.Pipelines,
		})
		if err != nil {
			return err
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				if reloadPolicy(logger, policy) {
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
		return fmt.Errorf("pair-run: %w", runErr)
	}
	return nil
}

// offerLaunchAgentInstall asks the user (Y/n) whether to install
// the LaunchAgent so the worker auto-starts at login. macOS only
// today; non-darwin platforms print a guidance message and bow out.
func offerLaunchAgentInstall(logger *slog.Logger) {
	fmt.Println()
	fmt.Print("Auto-start this worker at login? [Y/n] ")
	r := bufio.NewReader(os.Stdin)
	answer, _ := r.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "n" || answer == "no" {
		fmt.Println("Skipping auto-start install. Re-run with `memql worker pair` any time.")
		return
	}

	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: couldn't resolve binary path for LaunchAgent: %v\n", err)
		return
	}
	if err := InstallLaunchAgent(binPath); err != nil {
		if errors.Is(err, ErrNoLaunchAgent) {
			fmt.Println("Auto-start install isn't supported on this platform yet. Use scripts/install/install-linux.sh.")
			return
		}
		logger.Warn("LaunchAgent install failed", "error", err)
		fmt.Fprintf(os.Stderr, "WARN: LaunchAgent install failed: %v\n", err)
		return
	}
	fmt.Println()
	fmt.Println("Auto-start installed. The worker will run after every login.")
	fmt.Println("If macOS prompts you for Accessibility / Screen Recording on the next login,")
	fmt.Println("approve the memql-cockpit-computeruse binary -- the LaunchAgent process is a separate TCC")
	fmt.Println("entry from this Terminal session.")
}

// promptForPairingCode reads a code interactively. Returns the
// canonicalized form so the caller can hash + redeem directly.
func promptForPairingCode() (string, error) {
	if !isInteractiveTTY() {
		return "", errors.New("pair: no pairing code supplied and stdin is not a TTY -- pass the code as an argument: memql worker pair XXXX-XXXX")
	}
	fmt.Println("Enter the pairing code from CoPresent's Settings -> Computer Use card.")
	fmt.Println("Format: XXXX-XXXX (8 chars; A-Z + 2-9, no I / O / 0 / 1).")
	fmt.Println()
	for {
		raw, err := promptLine("Code: ")
		if err != nil {
			return "", err
		}
		canon := workerpairing.Canonicalize(raw)
		if canon != "" {
			return canon, nil
		}
		fmt.Println("That doesn't look right -- try again, or Ctrl+C to abort.")
	}
}

// promptLine reads one line from stdin with the supplied prompt.
func promptLine(prompt string) (string, error) {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// maskId masks the bulk of an opaque id for display in user-facing
// logs. Keeps the prefix + last 4 chars.
func maskId(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 8 {
		return id
	}
	return id[:4] + "..." + id[len(id)-4:]
}

// DefaultPolicyPath returns ~/.memql/policy.yaml.
func DefaultPolicyPath() string {
	dir := homeDirOrDefault()
	if dir == "" {
		return "policy.yaml"
	}
	return dir + "/.memql/policy.yaml"
}

func homeDirOrDefault() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return ""
}

// SelfExecForTCCReload spills the supplied state to disk and
// re-execs the cockpit binary with the same args. Returns only on
// failure -- on success the caller's process is replaced.
//
// Used by the TCC wizard when a probe shows denied AFTER the user
// said they granted: macOS won't reload TCC for the running
// process, so we spawn a fresh one.
func SelfExecForTCCReload(state WizardState) error {
	if err := SaveWizardState(state); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("self-exec: executable path: %w", err)
	}
	args := os.Args
	env := os.Environ()
	if err := syscall.Exec(exe, args, env); err != nil {
		return fmt.Errorf("self-exec: %w", err)
	}
	// Unreachable: syscall.Exec replaces the process on success.
	return nil
}
