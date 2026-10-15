package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql-cockpit/internal/worker/backup"
	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// FleetOptions wires the shared machine-side singletons into one
// supervisor that fans out a Runner per enabled home.
type FleetOptions struct {
	Logger      *slog.Logger
	Workers     WorkersFile
	WorkersPath string
	Policy      *tools.Policy
	PolicyPath  string
	// WorkerPaths are this worker's own files, which no app session may
	// touch (sessionWorkerPaths).
	WorkerPaths []string
	// ToolsFor builds the dispatcher one home's stream serves tool calls
	// through. PER HOME, because the consent gate inside it is per home
	// (memql-cockpit#433): a window opened for one cluster must admit
	// nothing another cluster dispatches.
	ToolsFor func(homeID string) ToolDispatcher
	// AppsFor builds the app inventory one home reports. PER HOME, because
	// app consent is per cluster (policy.yaml apps.homes): the same app is
	// allowed for one cluster and blocked for another. Nil reports no apps.
	AppsFor    func(homeID string) AppInventory
	Models     ModelInventory
	Discoverer *models.Discoverer
	Metrics    *Metrics
	// Limiter is the machine's model concurrency ceiling, shared by every
	// home (memql-cockpit#432). Nil builds one for this fleet.
	Limiter *modelcall.Limiter
}

// Fleet is one supervisor process, N cluster streams.
//
// Shared: metrics, policy, model inventory / discoverer, and the model
// concurrency ceiling -- the ceiling is a claim about this machine's
// hardware, which every cluster was told the same thing about. Per home:
// Runner, appsession.Manager, modelcall.Manager, backup sweeper, consent
// window -- so one home's disconnect must not StopAll another home's
// in-flight work, and one cluster's consent admits nothing for another.
type Fleet struct {
	logger      *slog.Logger
	workers     WorkersFile
	workersPath string
	machineID   string
	policy      *tools.Policy
	policyPath  string
	workerPaths []string
	toolsFor    func(homeID string) ToolDispatcher
	appsFor     func(homeID string) AppInventory
	modelsInv   ModelInventory
	discoverer  *models.Discoverer
	metrics     *Metrics
	limiter     *modelcall.Limiter

	mu      sync.Mutex
	runners []*Runner
	managed map[string]*managedHome
	runHome func(context.Context, *managedHome) error

	// newRunner is NewRunner, and a seam: a test swaps in one that fails
	// for one home, or one that hands the runner a scripted stream.
	newRunner func(Options) (*Runner, error)
}

// NewFleet validates the registry and returns a supervisor. Call Run.
func NewFleet(opts FleetOptions) (*Fleet, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := opts.Workers.ValidateRun(); err != nil {
		return nil, err
	}
	limiter := opts.Limiter
	if limiter == nil {
		limiter = modelcall.NewLimiter()
	}
	return &Fleet{
		logger:      opts.Logger,
		workers:     opts.Workers,
		workersPath: opts.WorkersPath,
		managed:     make(map[string]*managedHome),
		policy:      opts.Policy,
		policyPath:  opts.PolicyPath,
		workerPaths: opts.WorkerPaths,
		toolsFor:    opts.ToolsFor,
		appsFor:     opts.AppsFor,
		modelsInv:   opts.Models,
		discoverer:  opts.Discoverer,
		metrics:     opts.Metrics,
		limiter:     limiter,
		newRunner:   NewRunner,
	}, nil
}

// RequestImmediateReadvertise fans out to every home stream so a
// SIGHUP / model-allow change becomes visible on all clusters.
func (f *Fleet) RequestImmediateReadvertise() {
	if f == nil {
		return
	}
	f.mu.Lock()
	runners := append([]*Runner(nil), f.runners...)
	f.mu.Unlock()
	for _, r := range runners {
		r.RequestImmediateReadvertise()
	}
}

// homeRun is one home, built and not yet started.
type homeRun struct {
	home     Home
	cfg      Config
	logger   *slog.Logger
	runner   *Runner
	sessions *appsession.Manager
	backups  *backup.Manager
}

// Run connects every enabled home in parallel, one stream per cluster
// (runnableHomes). One home failing (or reconnecting forever) does not
// cancel the others; only ctx cancel tears the supervisor down.
//
// EVERY HOME IS BUILT BEFORE ANY IS STARTED (memql-cockpit#433). A home
// that cannot be built is a configuration error, and finding it after
// its siblings were already connected used to return from Run with those
// siblings still running -- streams, backup sweepers and all -- under a
// supervisor that had reported itself finished. Built first, a bad home
// fails the fleet before anything exists to leak. Every goroutine Run
// starts runs under a context Run cancels on its way out, and is joined
// before Run returns; what those start in turn -- a generation, an app
// session's process -- is cancelled with them and ends on its own.
func (f *Fleet) Run(ctx context.Context) error {
	homes, duplicates := f.workers.runnableHomes()
	machineID, err := ResolveMachineID(f.workers.machineRoot())
	if err != nil {
		f.logger.Warn("machine id unavailable; registering without one", "error", err)
	}
	// Preserve upstream's all-or-nothing construction before any connection.
	prepared := make(map[string]*homeRun)
	for _, home := range homes {
		run, err := f.buildHome(home, machineID)
		if err != nil {
			return fmt.Errorf("fleet: home %s: %w", home.ID, err)
		}
		prepared[home.ID] = &run
	}
	suppressed := make(map[string]string)
	for _, d := range duplicates {
		f.logger.Warn(duplicateHomeSentence(d), "home", d.Home.ID)
		suppressed[d.Home.ID] = d.Connects
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.mu.Lock()
	f.machineID = machineID
	var wg sync.WaitGroup
	for _, home := range f.workers.Homes {
		h := &managedHome{home: home, enabled: home.IsEnabled(), wake: make(chan struct{}, 1), prepared: prepared[home.ID], duplicateOf: suppressed[home.ID]}
		f.managed[home.ID] = h
		if h.duplicateOf != "" {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); f.runManagedHome(runCtx, h) }()
	}
	f.mu.Unlock()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
	case <-done:
	}
	cancel()
	<-done
	return ctx.Err()
}

func (f *Fleet) runManagedHome(ctx context.Context, h *managedHome) {
	for ctx.Err() == nil {
		f.mu.Lock()
		if !h.enabled {
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-h.wake:
				continue
			}
		}
		homeCtx, cancel := context.WithCancel(ctx)
		h.cancel = cancel
		h.retrying = false
		f.mu.Unlock()
		run := f.runHome
		if run == nil {
			run = f.runManagedHomeOnce
		}
		err := run(homeCtx, h)
		wasCanceled := homeCtx.Err() != nil
		cancel()
		f.mu.Lock()
		h.cancel = nil
		h.runner = nil
		h.retrying = !wasCanceled
		f.mu.Unlock()
		if !wasCanceled {
			if err != nil {
				f.logger.Error("home stream exited", "home", h.home.ID, "error", err)
			}
			// Keep this home's supervisor controllable when its runner ends.
			// A bounded retry avoids stranding it while siblings keep running.
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
			case <-h.wake:
			case <-timer.C:
			}
			timer.Stop()
		}
	}
}

func (f *Fleet) runManagedHomeOnce(ctx context.Context, h *managedHome) error {
	run := h.prepared
	h.prepared = nil
	if run == nil {
		built, err := f.buildHome(h.home, f.machineID)
		if err != nil {
			return err
		}
		run = &built
	}
	f.mu.Lock()
	h.runner = run.runner
	f.runners = append(f.runners, run.runner)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, r := range f.runners {
			if r == run.runner {
				f.runners = append(f.runners[:i], f.runners[i+1:]...)
				break
			}
		}
	}()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); run.backups.Run(runCtx, run.runner.RegistrationId) }()
	run.logger.Info("home stream starting", "cluster_url", run.cfg.ClusterURL, "name", run.cfg.Name)
	err := run.runner.Run(runCtx)
	cancel()
	run.sessions.StopAll("home stream ended")
	<-done
	return err
}

// buildHome makes one home's runner and the per-home machinery around
// it, starting nothing.
func (f *Fleet) buildHome(home Home, machineID string) (homeRun, error) {
	cfg := f.workers.ConfigForHome(home)
	cfg.MachineID = machineID
	homeLogger := f.logger.With("home", home.ID)

	sessions := appsession.NewManager(appsession.Options{
		Logger:     homeLogger,
		StateDir:   cfg.StateDir,
		ClusterURL: cfg.ClusterURL,
		// This cluster's own consent, never another's (apps.homes).
		Allowed: appsAllowedFor(f.policy, home.ID),
		// The owner's apps.levels (memql-cockpit#438). The method value is
		// safe on a nil policy: AppLevels answers "no entries", which is
		// the built-in table.
		Levels:         f.policy.AppLevels,
		CheckWorkspace: f.policyCheckPath(),
		// Where a session goes when the engine names no workspace: under
		// fs.workspace_root when the owner set one, and under this home
		// either way, so two clusters never share a directory
		// (appsession/workspace.go). Safe on a nil policy for AppLevels'
		// reason: WorkspaceRoot answers "", the platform's own directory.
		Home:          home.ID,
		WorkspaceRoot: f.policy.WorkspaceRoot,
		// What a session may neither run in nor let its app read or write
		// (appsession/protected.go). DenyPaths is nil-safe for AppLevels'
		// reason: a nil policy denies nothing beyond the worker's own.
		WorkerPaths: f.workerPaths,
		DenyPaths:   f.policy.DenyPaths,
	})
	// Per-home Calls so a disconnect on home A does not StopAll
	// generations serving home B on the shared GPU -- over one Limiter,
	// so the two together cannot take more of that GPU than it has.
	calls := modelcall.NewManager(modelcall.Options{
		Logger:    homeLogger,
		Inventory: f.modelsInv,
		Limiter:   f.limiter,
	})
	var pull *ModelPullOptions
	if f.modelsInv != nil && f.policy != nil {
		ollamaBase := func() string { return "" }
		if f.discoverer != nil {
			ollamaBase = f.discoverer.ResolvedOllamaBaseURL
		}
		pull = &ModelPullOptions{
			PolicyPath:   f.policyPath,
			OllamaBase:   ollamaBase,
			PullAllowed:  f.policy.ModelsPullAllowed,
			ReloadPolicy: f.policy.Reload,
		}
	}
	var dispatcher ToolDispatcher
	if f.toolsFor != nil {
		dispatcher = f.toolsFor(home.ID)
	}
	var appInventory AppInventory
	if f.appsFor != nil {
		appInventory = f.appsFor(home.ID)
	}
	newRunner := f.newRunner
	if newRunner == nil {
		newRunner = NewRunner
	}
	runner, err := newRunner(Options{
		Logger:   homeLogger,
		Config:   cfg,
		Tools:    dispatcher,
		Apps:     appInventory,
		Models:   f.modelsInv,
		Calls:    calls,
		Sessions: sessions,
		Metrics:  f.metrics,

		PipelinesPolicy: f.pipelinesPolicy(),
		ModelPull:       pull,
	})
	if err != nil {
		return homeRun{}, err
	}
	backups := backup.New(backup.Options{
		Logger:            homeLogger,
		StateDir:          cfg.StateDir,
		BaseURL:           backupBaseURL(cfg.ClusterURL),
		Bearer:            backupBearer(cfg.ClusterURL, f.logger),
		CheckPath:         f.policyBackupCheck(),
		CheckDownloadPath: f.policyDownloadCheck(),
		HTTPClient:        &http.Client{Timeout: 0},
	})
	return homeRun{
		home:     home,
		cfg:      cfg,
		logger:   homeLogger,
		runner:   runner,
		sessions: sessions,
		backups:  backups,
	}, nil
}

// pipelinesPolicy is the live pipelines consent, shared by every home: the
// machine's owner allows CI steps on the machine, whichever cluster sends
// them (memql#5494). Nil, read as false, when no policy is loaded.
func (f *Fleet) pipelinesPolicy() func() tools.PipelinesPolicy {
	if f.policy == nil {
		return nil
	}
	return f.policy.Pipelines
}

func (f *Fleet) policyCheckPath() func(string) error {
	if f.policy == nil {
		return func(string) error { return nil }
	}
	return f.policy.CheckPath
}

func (f *Fleet) policyBackupCheck() func(string) error {
	if f.policy == nil {
		return func(string) error { return fmt.Errorf("backup reads are not allowed on this machine") }
	}
	return f.policy.CheckBackupPath
}

func (f *Fleet) policyDownloadCheck() func(string) error {
	if f.policy == nil {
		return nil
	}
	return f.policy.CheckBackupDownloadPath
}
