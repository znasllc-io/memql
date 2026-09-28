//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/planner"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// cockpitapp.go is the FIRST inhabitant of the container-executor
// seam (memql#4361). The seam has existed and been empty since
// memql#4120: RegisterContainerExecutor was there, Task carried
// executionSurface + executorBackend, and nothing in the tree ever
// called it -- so a containerExecutor Task had nowhere to land.
//
// It lives in this package rather than a sibling one so it shares the
// fleet router, the registry and the APP GATE (app_gate.go) with the
// chat door in app_inference.go. An app run does NOT take the gates a
// shell command through the same machine takes -- per-task approval,
// standing computerUseScope, the classifier -- which was design D4's
// answer and is no longer the owner's: an app session's consent is the
// machine's apps.allow and the owner's policy naming the app, with the
// kill switch able to close it. One gate for both doors means the two
// cannot drift into different consent stories.

// BackendCockpitApp is the registered backend name. Tasks name a
// specific app as "cockpit-app:<appId>"; the base name is what
// registers, so growing the app list is a value change rather than a
// release.
const BackendCockpitApp = "cockpit-app"

// defaultAppSessionMaxDuration bounds a run whose delegation policy
// sets none. Generous, because a headless coding agent legitimately
// runs for a long time -- but not unbounded, because a run nobody
// ends is a machine nobody gets back.
const defaultAppSessionMaxDuration = 4 * time.Hour

// CockpitAppExecutor runs a Task by opening an app session on one of
// the owner's cockpit machines.
type CockpitAppExecutor struct {
	logger   *slog.Logger
	runner   *workerservice.SessionRunner
	router   *Router
	registry *workerservice.Registry
	policies DelegationPolicyReader
	// prefs is where the app gate reads the owner's kill switch. Nil admits,
	// which is what a node with no store can honestly say about a switch it
	// cannot read.
	prefs PreferencesReader
	// ledger records the run's reported spend on v1:router:call. Nil
	// disables the write; the run still happens, because losing a
	// cost row must not lose the work.
	ledger *LedgerWriter
}

// WithLedger installs the AI-ledger writer.
func (e *CockpitAppExecutor) WithLedger(ledger *LedgerWriter) *CockpitAppExecutor {
	if e != nil {
		e.ledger = ledger
	}
	return e
}

// DelegationPolicyReader resolves a user's delegation preference.
// Narrow on purpose so this file does not import the engine.
type DelegationPolicyReader interface {
	DelegationPolicy(ctx context.Context, ownerUserId string) (DelegationPolicy, error)
}

// DelegationPolicy is the subset of v1:worker:delegationPolicy the
// executor reads.
type DelegationPolicy struct {
	Found                  bool
	PreferSubscriptionApps bool
	EligibleKinds          []string
	AppOrder               []string
	MaxConcurrentSessions  int
	WorkspaceRoot          string
	CredentialLifetime     time.Duration
}

// AllowsKind reports whether this policy permits delegating a task of
// the given kind. An empty EligibleKinds list allows NOTHING rather
// than everything: opting into delegation should not silently opt
// every task kind in with it.
func (p DelegationPolicy) AllowsKind(kind string) bool {
	if !p.PreferSubscriptionApps {
		return false
	}
	for _, k := range p.EligibleKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// cockpitAppRegistry holds the process-wide executor so init() can
// register a placeholder that the app wiring later completes. The
// registry maps a NAME to an implementation, and the implementation
// needs a dispatcher and a runner that do not exist at init() time.
var (
	cockpitAppMu       sync.RWMutex
	cockpitAppExecutor *CockpitAppExecutor
)

func init() {
	// Registering at init() is what makes ValidateExecutorBackend
	// accept "cockpit-app:*" on an agent binary and refuse it
	// everywhere else, which is the honest answer: only an agent node
	// holds worker streams, so only an agent node can serve one.
	planner.RegisterContainerExecutor(BackendCockpitApp, cockpitAppDelegate{})
}

// cockpitAppDelegate is the registry entry. It forwards to whatever
// the app wiring installed, and refuses with a NAMED reason when
// nothing did -- rather than nil-panicking or silently succeeding.
type cockpitAppDelegate struct{}

func (cockpitAppDelegate) Backend() string { return BackendCockpitApp }

func (cockpitAppDelegate) Run(ctx context.Context, req planner.ExecutorRequest, progress planner.ProgressCallback) (planner.ExecutorResult, error) {
	cockpitAppMu.RLock()
	exec := cockpitAppExecutor
	cockpitAppMu.RUnlock()
	if exec == nil {
		return planner.ExecutorResult{}, errors.New(
			"cockpit-app: the backend is registered but not wired on this node (no worker service); " +
				"a Task can only reach it on an agent node running WorkerService")
	}
	return exec.Run(ctx, req, progress)
}

// InstallCockpitAppExecutor completes the registration made at
// init(). Called from the agent node's wiring once the worker service
// and the engine exist.
func InstallCockpitAppExecutor(exec *CockpitAppExecutor) {
	cockpitAppMu.Lock()
	cockpitAppExecutor = exec
	cockpitAppMu.Unlock()
}

// NewCockpitAppExecutor builds the executor.
func NewCockpitAppExecutor(logger *slog.Logger, dispatcher *Dispatcher, runner *workerservice.SessionRunner, policies DelegationPolicyReader) (*CockpitAppExecutor, error) {
	if logger == nil {
		return nil, errors.New("cockpit-app: logger required")
	}
	if dispatcher == nil {
		return nil, errors.New("cockpit-app: dispatcher required (it owns the fleet router, the registry and the owner's preferences)")
	}
	if runner == nil {
		return nil, errors.New("cockpit-app: session runner required")
	}
	exec := &CockpitAppExecutor{
		logger: logger,
		runner: runner,
		// The dispatcher's router and registry, not new ones: two routers
		// would read the same owner policy twice, possibly at different
		// moments, and give the two paths different answers to "which
		// machine".
		router:   dispatcher.Router(),
		registry: dispatcher.Registry(),
		policies: policies,
	}
	if dispatcher.store != nil {
		exec.prefs = dispatcher.store
	}
	return exec, nil
}

// Backend implements planner.ContainerExecutor.
func (e *CockpitAppExecutor) Backend() string { return BackendCockpitApp }

// Run opens an app session for the Task and maps its outcome onto an
// ExecutorResult.
//
// The gate order matters and is deliberate: identity, then app,
// then the APP GATE's consent, then the machine. Running the consent
// before selection means a refused run never reveals which machines
// the user has online; running it after would make "you have no
// machine for this" and "you may not do this" both present as the
// same failure.
func (e *CockpitAppExecutor) Run(ctx context.Context, req planner.ExecutorRequest, progress planner.ProgressCallback) (planner.ExecutorResult, error) {
	started := time.Now()

	// An empty owner is a REFUSAL, never a wildcard. Worker
	// registrations are per-user and the back-channel credential is
	// minted with this as its subject; a blank one would either match
	// nobody or, worse, be treated as "any".
	ownerUserId := strings.TrimSpace(req.OwnerUserId)
	if ownerUserId == "" {
		return planner.ExecutorResult{}, errors.New("cockpit-app: task has no owner; a machine-touching backend cannot run unattributed")
	}

	appId := planner.BackendArg(taskBackend(req))
	if appId == "" {
		return planner.ExecutorResult{}, errors.New(
			"cockpit-app: executorBackend must name an app, e.g. \"cockpit-app:claude-code\"")
	}
	if !workerservice.IsKnownAppId(appId) {
		return planner.ExecutorResult{}, fmt.Errorf(
			"cockpit-app: %q is not an app this engine drives (known: %s)",
			appId, strings.Join(workerservice.KnownAppIds(), ", "))
	}

	// THE APP GATE (app_gate.go), the one the chat door asks too. The
	// machine's apps.allow and the owner's policy naming this app are the
	// consent; what is asked here is whether the owner has since switched
	// computer use off. No agent scope is read: an app session is not an
	// agent's shell command, and a cluster where nobody granted one still
	// opens the sessions its owner's policy routes to an app.
	if refusal := appSessionConsent(ctx, e.prefs, e.logger, ownerUserId); refusal != nil {
		return planner.ExecutorResult{}, fmt.Errorf("cockpit-app: %s", refusal)
	}

	policy := DelegationPolicy{}
	if e.policies != nil {
		resolved, err := e.policies.DelegationPolicy(ctx, ownerUserId)
		if err != nil {
			e.logger.Warn("cockpit-app: delegation policy lookup failed; using defaults",
				"owner_user_id", ownerUserId, "error", err)
		} else {
			policy = resolved
		}
	}

	spec := sessionRunSpec(req, appId, ownerUserId, sessionWorkspace(req, policy.WorkspaceRoot), policy.CredentialLifetime)

	// Machine selection goes through the FLEET ROUTER (memql#4350), not
	// through anything this file invents: it applies the owner's routing
	// policy, orders by their chosen strategy, and knows which replica
	// holds each machine's stream. The app requirement is merged into the
	// require-labels the Task already carried, so a policy that narrows
	// still narrows and an app requirement is added on top.
	w, routeErr := e.selectMachine(ctx, ownerUserId, appId, spec.RequireLabels)
	if routeErr != nil {
		return planner.ExecutorResult{}, routeErr
	}

	result, runErr := e.runner.Run(ctx, w, spec, progressBridge(progress))

	// The ledger row is written on EVERY outcome, failures included.
	// A run that burned an hour of somebody's subscription and then
	// crashed still spent it, and a cost surface that only records
	// successes systematically understates what the work cost.
	if e.ledger != nil && result.SessionId != "" {
		if err := e.ledger.RecordAppSession(ctx, result, spec); err != nil {
			e.logger.Warn("cockpit-app: ledger write failed",
				"session_id", result.SessionId, "error", err)
		}
	}

	out := planner.ExecutorResult{
		Output: map[string]interface{}{
			"sessionId":     result.SessionId,
			"workerId":      result.WorkerId,
			"app":           appId,
			"exitCode":      result.ExitCode,
			"appSessionRef": result.AppSessionRef,
			"transcript":    result.Transcript,
			// WHAT THE APP REPORTED, verbatim (design D9). Empty means it did
			// not say, which every reader records as unknown -- never the
			// level it was given, never the app id.
			"model":  result.Model,
			"effort": result.Effort,
			// The structured answer, when the harness produced one -- from
			// the session's end or from a `submit` the app made over MCP.
			// ABSENT rather than empty when there is none: a caller that
			// parses this can tell "no structured answer" from "an empty
			// one", and those are different outcomes.
			"result": resultOrNil(result.Result),
		},
		// TokensSpent is what the APP reported, never a MemQL
		// estimate. An app that reports nothing contributes zero
		// tokens and `unknown` billing, which is visible as silence
		// rather than as a free call.
		TokensSpent: int(result.Usage.InputTokens + result.Usage.OutputTokens),
		DurationMs:  time.Since(started).Milliseconds(),
		Billing:     billingOrUnknown(result.Billing),
		ArtifactIds: result.ProducedArtifactIds,
	}
	if runErr != nil {
		return out, fmt.Errorf("cockpit-app: %s run failed: %w", appId, runErr)
	}
	return out, nil
}

// sessionRunSpec is the session a Task opens, built from the request alone --
// a function rather than a literal inside Run so what reaches AppSessionStart
// can be read without standing up the consent gates in front of it.
func sessionRunSpec(req planner.ExecutorRequest, appId, ownerUserId, workspace string, credentialLifetime time.Duration) workerservice.RunSpec {
	return workerservice.RunSpec{
		SessionId:          "v1:worker:appSession:" + id.NewShortId(),
		OwnerUserId:        ownerUserId,
		App:                appId,
		Kind:               workerservice.AppSessionKindRun,
		Prompt:             promptFromInput(req),
		Workspace:          workspace,
		Inputs:             req.Inputs,
		RunId:              req.RunId,
		StepId:             req.StepId,
		RequireLabels:      mergeRequireLabels(req.RequireLabels, appId),
		CredentialLifetime: credentialLifetime,
		MaxDuration:        defaultAppSessionMaxDuration,
		// The task's output contract, when it declared one (design D7). A
		// harness that supports a structured answer is asked for one; the rest
		// carry it and ignore it, which is why nothing here gates on the
		// descriptor -- the answer is a bonus, not a requirement, on this
		// path.
		ResponseSchema: responseSchemaFromInput(req),
		// The call's LEVEL, when the caller named one (epic memql#5391,
		// design D8). A task that named none leaves it empty and the app runs
		// at its own defaults, which is what every delegated task did before
		// the field existed. The COCKPIT translates it; nothing here maps a
		// level to a model, because the knob names are the app's.
		Level: stringFromInput(req, "level"),
		// The model an `app:<id>:<model>` pin names and a person's explicit
		// effort (epic memql#5414, design D20). Both override the level's
		// knobs on the far side for this session only; empty lets it decide.
		Model:  stringFromInput(req, "model"),
		Effort: stringFromInput(req, "effort"),
		// The subrun the session's actions are recorded into (epic
		// memql#5396). The step-handover path opens it and stamps childRunId
		// on the delegating step; a delegated TASK opens no run at all and
		// leaves this empty, and the recorder opens one for itself.
		RecordingRunId: stringFromInput(req, "recordingRunId"),
	}
}

// sessionWorkspace is the directory an app session runs in.
//
// A Task that named a directory gets it. Otherwise it is one directory per
// unit of work under the owner's workspaceRoot -- the per-plan convention the
// workbench also keeps, so a run cannot read the previous one's leftovers by
// accident. That directory is the run's own, or, when a re-run or a branch
// asked for a FRESH one (epic memql#5414, design D19), the directory that
// request named: the new session must not run inside the tree the previous
// version's later steps changed. A name that is not one plain directory name
// is never joined, since it could place the workspace outside the root the
// owner chose; the delegate refuses such a name before it gets here.
//
// EMPTY IS A VALID ANSWER, and it means the machine chooses
// (AppSessionStart.workspace): with no root, or with a root and no unit of
// work to key a directory by, the session names no workspace and the cockpit
// makes one of its own. The engine never invents a path on somebody's
// machine, and never runs a session in the root itself -- the tree every
// other run's directory lives in.
func sessionWorkspace(req planner.ExecutorRequest, root string) string {
	if w := strings.TrimSpace(req.Workspace); w != "" {
		return w
	}
	dir := strings.TrimSpace(req.RunId)
	if fresh := stringFromInput(req, "freshWorkspace"); isWorkspaceDirName(fresh) {
		dir = fresh
	}
	return workspaceUnderRoot(root, dir)
}

// workspaceUnderRoot joins one directory name onto the owner's
// delegationPolicy.workspaceRoot, or answers "" -- the machine chooses -- when
// either is missing. It is the one place both doors compose a workspace, so
// the chat door and the session door cannot disagree about what a root means.
func workspaceUnderRoot(root, dir string) string {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	dir = strings.TrimSpace(dir)
	if root == "" || dir == "" {
		return ""
	}
	return root + "/" + dir
}

// isWorkspaceDirName reports whether name is one plain directory name, which
// is what a re-run's fresh workspace is: no separator, not "." or "..", no
// surrounding space -- nothing that, joined to a root, lands anywhere but
// directly inside it.
func isWorkspaceDirName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.TrimSpace(name) != name {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

// selectMachine asks the Fleet router for a machine that can run appId.
//
// The `app:<id>` label is what the router matches on -- the engine
// derived it from the machine's own report and persisted it beside the
// inventory. Requiring the label rather than filtering candidates here
// keeps ONE definition of "can run this app": if the router's answer and
// the runner's re-check ever disagreed, the plan would commit to a
// machine that then refused, and the failure would name the router.
func (e *CockpitAppExecutor) selectMachine(ctx context.Context, ownerUserId, appId string, require map[string]string) (*workerservice.Worker, error) {
	if e.router == nil || e.registry == nil {
		return nil, fmt.Errorf("cockpit-app: no fleet router on this node; a Task can only reach one on an agent node running WorkerService")
	}
	// The app is NOT passed as a require-label, and that is deliberate.
	// The derived label's VALUE is the app's version (`app:claude-code`
	// -> "2.1"), and satisfiesLabels compares values with `got != v` --
	// exact equality. Requiring the label with an empty value, the
	// natural spelling of "any version", would match no machine that
	// reports one, which is every real machine. Nothing would fail
	// loudly: the router would return candidates, none would match, and
	// the triage would report `no_machine_with_app_online` --
	// indistinguishable from a laptop being asleep.
	//
	// So the ROUTER applies the owner's policy and ordering over the
	// Task's own requirements, and the app filter is the app gate's
	// appMachineRefusal below -- RunsApp, the same predicate the label
	// derivation uses, so the two cannot disagree, plus the machine being
	// the owner's. TestAppRequirementIsNotAnExactLabelMatch pins the
	// semantics that force this.
	plan, err := e.router.Plan(ctx, ownerUserId, workerservice.CapabilityHeadless, require, nil)
	if err != nil {
		return nil, fmt.Errorf("cockpit-app: routing %s: %w", appId, err)
	}
	considered := make(map[string]string, len(plan.Candidates)+len(plan.Rejected))
	for _, candidate := range plan.Candidates {
		// Only a machine whose stream THIS replica holds can carry a
		// session today: the app-session envelope has no cross-node
		// forward yet (the tool path's WorkerForward does, memql#4352).
		// Skipping rather than failing is what makes a second replica
		// holding the machine a routing outcome rather than an error.
		w := e.registry.WorkerById(candidate.RegistrationId)
		if why := appMachineRefusal(w, ownerUserId, appId); why != "" {
			considered[candidate.RegistrationId] = why
			continue
		}
		return w, nil
	}
	if plan.Total == 0 {
		return nil, fmt.Errorf("cockpit-app: no machines are registered to this user")
	}
	for reg, why := range plan.Rejected {
		considered[reg] = why
	}
	return nil, fmt.Errorf(
		"cockpit-app: none of this user's %d machine(s) can carry a %s session from this replica%s",
		plan.Total, appId, consideredSuffix(considered))
}

// consideredSuffix renders why each machine was passed over, in a stable
// order, as " -- id: reason; id: reason". Empty when nothing was considered.
func consideredSuffix(considered map[string]string) string {
	if len(considered) == 0 {
		return ""
	}
	regs := make([]string, 0, len(considered))
	for reg := range considered {
		regs = append(regs, reg)
	}
	sort.Strings(regs)
	parts := make([]string, 0, len(regs))
	for _, reg := range regs {
		parts = append(parts, reg+": "+considered[reg])
	}
	return " -- " + strings.Join(parts, "; ")
}

// mergeRequireLabels returns the Task's own require-labels with any
// app: entry STRIPPED.
//
// It strips rather than adds: the app filter is RunsApp, not a label
// require (see selectMachine), and a synthesised version pin would
// refuse a machine running a NEWER app for a reason nobody stated. A
// Task that genuinely needs a version floor says so itself, and that
// pin survives here untouched.
func mergeRequireLabels(taskLabels map[string]string, appId string) map[string]string {
	out := make(map[string]string, len(taskLabels))
	for k, v := range taskLabels {
		out[k] = v
	}
	// The app: label's presence is what PickWorkerForApp checks; the
	// VALUE is a version, and pinning one here would refuse a machine
	// running a newer app for no stated reason. A Task that genuinely
	// needs a version floor states it in its own RequireLabels.
	delete(out, workerservice.AppLabelKey(appId))
	if len(out) == 0 {
		return nil
	}
	return out
}

// taskBackend recovers the full executorBackend name for this
// request. The planner passes it on Input under "executorBackend";
// falling back to the bare backend name keeps a Task that named no
// app reaching the explicit error above rather than a nil map read.
func taskBackend(req planner.ExecutorRequest) string {
	if raw, ok := req.Input["executorBackend"].(string); ok && raw != "" {
		return raw
	}
	return BackendCockpitApp
}

// promptFromInput pulls the run's prompt off the Task input.
func promptFromInput(req planner.ExecutorRequest) string {
	for _, key := range []string{"prompt", "goal", "instruction"} {
		if raw, ok := req.Input[key].(string); ok && strings.TrimSpace(raw) != "" {
			return raw
		}
	}
	return ""
}

// stringFromInput reads one string off the Task input. A missing key and a
// non-string value both read as EMPTY, which is the honest answer for every
// field on this path: absent means the caller named none, and the far side
// has a defined behaviour for that.
func stringFromInput(req planner.ExecutorRequest, key string) string {
	raw, _ := req.Input[key].(string)
	return strings.TrimSpace(raw)
}

// billingOrUnknown normalizes a runner billing value for the seam.
func billingOrUnknown(billing string) string {
	switch billing {
	case workerservice.BillingMetered:
		return planner.BillingMetered
	case workerservice.BillingSubscription:
		return planner.BillingSubscription
	}
	return planner.BillingUnknown
}

// progressBridge maps session chunks onto planner ProgressEvents so
// the Tasks page's live view renders a delegated run the same way it
// renders an in-process one.
//
// An "event" chunk carries the app's own structured JSON, which is
// where a `command` or `file` event comes from; plain stdout/stderr
// becomes narration. Guessing structure out of raw stdout would
// produce a live view that is confidently wrong about what the agent
// did.
func progressBridge(progress planner.ProgressCallback) workerservice.ProgressFunc {
	if progress == nil {
		return nil
	}
	return func(chunk workerservice.AppSessionChunk) {
		kind := "narration"
		if chunk.Stream == workerservice.AppSessionStreamEvent {
			kind = "command"
		}
		progress(planner.ProgressEvent{
			Kind: kind,
			Payload: map[string]interface{}{
				"stream": chunk.Stream,
				"seq":    chunk.Seq,
				"text":   string(chunk.Data),
			},
		})
	}
}

// responseSchemaFromInput pulls the task's output contract off its input.
//
// Absent means no structured answer is asked for, which is not the same as
// asking and getting nothing: only a run that asked can be disappointed by a
// session that ends without one.
func responseSchemaFromInput(req planner.ExecutorRequest) string {
	for _, key := range []string{"responseSchema", "outputSchema", "schema"} {
		if raw, ok := req.Input[key].(string); ok && strings.TrimSpace(raw) != "" {
			return raw
		}
	}
	return ""
}

// resultOrNil decodes the harness's structured answer for the executor output,
// or returns nil so the key is absent.
func resultOrNil(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// Not JSON. Handed back as text rather than dropped: the harness
		// answered something, and losing it because it did not parse hides
		// the one piece of evidence that says the schema was not met.
		return string(raw)
	}
	return decoded
}
