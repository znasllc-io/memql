// Package procedure is the wiring half of procedure learning and
// certification: epic memql#5402 (design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// section 4 epic C; decisions D6, D13, D14, D24) and epic memql#5408 (epic D;
// D3, D4, D15, D16). It backs the builtins declared in
// dsl/procedure/builtins.memql:
//
//	integration.procedure.learnFromRun    -- mine the corpus a finished run belongs to, and lift it
//	integration.procedure.mineCorpus      -- the scheduled sweep, per owner and goal signature
//	integration.procedure.step            -- the statement every rendered step is
//	integration.procedure.replay          -- serve a goal from a learned procedure
//	integration.procedure.ladderSweep     -- the ladder's demotion and retirement sweeps
//	integration.procedure.decidePromotion -- apply a person's promotion decision
//
// THE DIVISION OF LABOUR IS THE DESIGN, and it is the same one the work spine
// draws. Every DECISION is a pure function in component/procedure or
// component/work -- what a symbol is, what recurs, what a hole means, what an
// abstraction is worth, where a procedure stands on the ladder -- so the
// epic's headline claims are properties of values, provable with no engine and
// no database. This package is responsible only for what a pure module cannot
// do: reading rows under the right actor, rendering the winner as MemQL,
// writing it, and making the ONE bounded model call D6 allows -- whose answer
// it hands straight back to the module to check.
package procedure

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// integrationName is the plug-in name and the middle segment of every
// capability FQN. Spelled as a STRING LITERAL in RegisterPlugin below as well,
// because the module-taxonomy gate finds registrations by scanning source for
// the literal.
const integrationName = "procedure"

// resultConcept is the synthetic concept a capability reply rides on. Never
// persisted.
const resultConcept = "v1:procedure:result"

// Deriver makes the one bounded model call of D6. It is an interface so that
// the pipeline can be driven with NO deriver at all -- which is the ordinary
// case, and the case the zero-provider-call test asserts.
type Deriver interface {
	// ProposeDerivation returns a derivation expression for one unexplained
	// hole, or "" when it has nothing to propose. Whatever it returns is
	// handed straight to component/procedure.CheckDerivation: this seam
	// cannot decide that a proposal is good, only that one was made.
	ProposeDerivation(ctx context.Context, h proc.Hole, instances [][]proc.Action) (string, error)
}

// CompileGate is Gate 1: the isolated compile + bind of a candidate bundle
// (gap G7). A learned procedure whose source this does not compile is not
// re-runnable, keeps no goal signature, and enters the ladder as a candidate.
//
// It is a SEAM installed from app/ rather than something the plug-in finds on
// its engine, because the engine it is handed (*memql.MemQLEngine) has no such
// method -- the compile is a function of the engine that app's
// CognitionEngineAdapter wraps. A type assertion on the engine was how this
// used to find it, and it failed on every node: Gate 1 never ran, no procedure
// was ever recorded as re-runnable, and none was ever findable by its goal.
type CompileGate interface {
	CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport
}

// Integration exposes the procedure capabilities.
type Integration struct {
	store    *store
	logger   *slog.Logger
	now      func() time.Time
	deriver  Deriver
	params   proc.Params
	compiler CompileGate
	// seams are what a replay needs from the node it runs on (seams.go):
	// the dispatchers, the prober and the app fallback. app/ installs them.
	seams seams
	// lockDB is the handle the ladder's per-construct advisory lock is taken
	// on (ladder.go) -- the DIRECT endpoint, because a transaction pooler
	// recycles the server backend between statements and would drop a held
	// session lock. Nil, or answering nil, runs every ladder move unlocked:
	// a test over a fake engine, or a node with no database, which could not
	// write the ladder anyway.
	lockDB func() *bun.DB
	// heartbeatEvery is how often a replay renews its run's heartbeat while
	// a step is in flight (replay.go); zero is the automation runtime's own
	// interval.
	heartbeatEvery time.Duration
}

// New builds the integration.
func New(engine Engine, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		store:  &store{engine: engine},
		logger: logger,
		now:    time.Now,
		params: proc.DefaultParams(),
	}
}

func init() {
	memql.RegisterPlugin("procedure", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.Engine == nil {
			return nil, fmt.Errorf("procedure plug-in: no engine in plugin context")
		}
		i := New(pctx.Engine, pctx.Logger)
		// The ladder's lock is SESSION-scoped, so it is taken on the direct
		// endpoint (PluginContext.DirectBunDB, which itself falls back to the
		// main pool when no direct DSN is configured). A context built before
		// that getter existed offers only the pool.
		if pctx.DirectBunDB != nil {
			i.SetLadderLockDB(pctx.DirectBunDB)
		} else {
			i.SetLadderLockDB(pctx.BunDB)
		}
		return i, nil
	})
}

// SetLadderLockDB installs the database handle the ladder's per-construct
// advisory lock is taken on. The plug-in factory installs the direct endpoint
// on every node; a test installs its own. A nil getter is ignored, for
// SetCompiler's reason.
func (i *Integration) SetLadderLockDB(db func() *bun.DB) {
	if i != nil && db != nil {
		i.lockDB = db
	}
}

// LadderLockInstalled reports whether the ladder's lock has a database handle
// to be taken on, for the wiring test that holds the node to it: with none,
// every ladder move runs unlocked, green in every test and racy in the
// cluster.
func (i *Integration) LadderLockInstalled() bool { return i != nil && i.lockDB != nil }

// SetDeriver installs the one bounded model call. Called once, from the node
// that holds a router. Absent, every unexplained hole simply stays free --
// which is a correct procedure with one more parameter, never a failure.
func (i *Integration) SetDeriver(d Deriver) {
	if i.deriver == nil {
		i.deriver = d
	}
}

// SetCompiler installs Gate 1 (gap G7). app/ calls it on the REGISTERED
// instance on every node type, because the plug-in registers everywhere and a
// lift can run wherever a run succeeds. A nil gate is ignored rather than
// installed: "no gate" must stay distinguishable from "a gate that refuses".
func (i *Integration) SetCompiler(c CompileGate) {
	if c != nil {
		i.compiler = c
	}
}

// CompileGateInstalled reports whether Gate 1 is wired, for the wiring test
// that holds app/ to it -- a plug-in with an unwired setter is green in every
// test and inert in the cluster.
func (i *Integration) CompileGateInstalled() bool { return i != nil && i.compiler != nil }

// SetDispatcher installs the dispatcher for one replay target. app/ installs
// the workbench's on every node that can reach a workbench, and the machine's
// on agent nodes only (the worker stream lives there). A nil dispatcher is
// ignored, for SetCompiler's reason.
func (i *Integration) SetDispatcher(target work.ReplayTarget, d Dispatcher) {
	if i == nil || d == nil {
		return
	}
	i.seams.mu.Lock()
	defer i.seams.mu.Unlock()
	if i.seams.dispatchers == nil {
		i.seams.dispatchers = map[work.ReplayTarget]Dispatcher{}
	}
	i.seams.dispatchers[target] = d
}

// SetProber installs the environment prober the preconditions are checked
// against. Absent, a procedure that learned any precondition cannot start:
// an unmeasured precondition does not hold (D16).
func (i *Integration) SetProber(p Prober) {
	if i == nil || p == nil {
		return
	}
	i.seams.mu.Lock()
	defer i.seams.mu.Unlock()
	i.seams.prober = p
}

// SetAppFallback installs the hand-back to the app. Absent, a replay that
// cannot serve its goal fails the run with procedure_fallback_unavailable,
// naming why -- never a silent success.
func (i *Integration) SetAppFallback(f AppFallback) {
	if i == nil || f == nil {
		return
	}
	i.seams.mu.Lock()
	defer i.seams.mu.Unlock()
	i.seams.fallback = f
}

// dispatcherFor, prober and appFallback read what app/ installed.
func (i *Integration) dispatcherFor(target work.ReplayTarget) Dispatcher {
	i.seams.mu.RLock()
	defer i.seams.mu.RUnlock()
	return i.seams.dispatchers[target]
}

func (i *Integration) prober() Prober {
	i.seams.mu.RLock()
	defer i.seams.mu.RUnlock()
	return i.seams.prober
}

func (i *Integration) appFallback() AppFallback {
	i.seams.mu.RLock()
	defer i.seams.mu.RUnlock()
	return i.seams.fallback
}

// ReplaySeamsInstalled reports which seams app/ wired, for the wiring tests.
func (i *Integration) ReplaySeamsInstalled() (workbench, machine, prober, fallback bool) {
	if i == nil {
		return
	}
	i.seams.mu.RLock()
	defer i.seams.mu.RUnlock()
	return i.seams.dispatchers[work.TargetWorkbench] != nil, i.seams.dispatchers[work.TargetMachine] != nil,
		i.seams.prober != nil, i.seams.fallback != nil
}

// SetParams overrides the pipeline knobs. The design record's cross-cutting
// rule makes the budget, support, gap and argument ceiling values rather than
// constants; this is where an operator's row reaches them.
func (i *Integration) SetParams(p proc.Params) { i.params = p }

// SetNow injects a clock. Tests only.
func (i *Integration) SetNow(f func() time.Time) {
	if f != nil {
		i.now = f
	}
}

// replayHeartbeatEvery is the automation runtime's heartbeat interval
// (component/automations/heartbeat.go), which the abandoned-run sweep's
// window is set against.
const replayHeartbeatEvery = 15 * time.Second

func (i *Integration) heartbeatInterval() time.Duration {
	if i == nil || i.heartbeatEvery <= 0 {
		return replayHeartbeatEvery
	}
	return i.heartbeatEvery
}

func (i *Integration) clock() time.Time {
	if i == nil || i.now == nil {
		return time.Now()
	}
	return i.now()
}

func (i *Integration) log() *slog.Logger {
	if i == nil || i.logger == nil {
		return slog.Default()
	}
	return i.logger
}

// Capabilities are the builtins dsl/procedure/builtins.memql declares. A
// capability the DSL names and the registry lacks is a BOOT failure on every
// node type, so every one is registered here -- and every one is a real
// executor: a capability that merely resolved and refused by name would pass
// the boot audit with the certification ladder inert.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "learnFromRun",
			Description: "Mine the corpus a finished run belongs to and, when an abstraction clears D14's floor, " +
				"lift it into a validated v1:authoring:bundle with its procedure, preconditions, version hash and " +
				"provenance, and put it on the certification ladder. Nothing auto-activates: the construct enters as " +
				"a candidate or in shadow. Returns {runId, ownerUserId, goalSignature, level, sequences, candidates, " +
				"constructId, accepted, reason, lift, procedureHash, rung}.",
			Handler: i.handleLearnFromRun,
			ArgsSchema: map[string]string{
				"runId":       "string (required) -- the v1:work:run that just succeeded",
				"ownerUserId": "string -- owner hint used only by the trusted completion trigger",
				"level":       "integer -- 1 mines the actions inside sessions (default), 2 mines automation invocations",
			},
		},
		{
			Name: "mineCorpus",
			Description: "The scheduled sweep: mine one owner's recorded corpus for one goal signature at one " +
				"level, or -- for the cluster's maintenance principal with a blank owner -- every owner's. " +
				"Disliked recordings are excluded. Returns {ownerUserId, goalSignature, level, signatures, " +
				"sequences, candidates, constructId, constructIds, accepted, reason}.",
			Handler: i.handleMineCorpus,
			ArgsSchema: map[string]string{
				"ownerUserId":   "string (required unless the caller is the cluster's maintenance principal) -- whose corpus to mine; every read runs as this person",
				"goalSignature": "string -- restrict to one goal's runs; empty mines every signature the owner has",
				"level":         "integer -- 1 actions (default), 2 automation invocations",
			},
		},
		// REUSE LABELS (epic memql#5414, design D24). Both handlers live in
		// their own files (reuse.go, reuse_sweep.go) and refuse by name until
		// the reuse stream replaces them.
		{
			Name: "setReuse",
			Description: "Label one of the caller's constructs reusable, for one goal or for one account, or hand the " +
				"label back to the evidence. The override is a VERSION and the evidence keeps counting underneath. " +
				"Returns {constructId, reuse, override}.",
			Handler: i.handleSetReuse,
			ArgsSchema: map[string]string{
				"constructId": "string (required) -- the caller's construct",
				"label":       "string (required) -- reusable, goalSpecific, accountSpecific, or evidence to clear the override",
			},
		},
		{
			Name: "reuseSweep",
			Description: "Decide every construct's reuse label from the evidence: the distinct goal signatures and " +
				"account ties of the runs that used it, per owner under that owner's actor. Maintenance principal " +
				"only. Returns {owners, constructs, changed, dryRun}.",
			Handler: i.handleReuseSweep,
			ArgsSchema: map[string]string{
				"dryRun": "boolean -- report what would change without writing it",
			},
		},
		{
			Name: "catalogSections",
			Description: "Catalogue a succeeded goal run's sections that were worked out live (design D24): every section " +
				"automation of the run's draft whose current version stands -- not overridden, not disliked, in a run " +
				"neither stale nor disliked -- is written into its owner's catalog as a construct of its own, keyed by the " +
				"section's signature, so the next goal whose section asks for the same thing is served it instead of " +
				"planning it live. An already-catalogued section writes nothing. Runs only as the catalogSucceededSections " +
				"automation, borrowing the owner its event names. Returns {runId, catalogued, skipped, notCatalogued}.",
			Handler: i.handleCatalogSections,
			ArgsSchema: map[string]string{
				"runId":       "string (required) -- the v1:work:run that just succeeded",
				"ownerUserId": "string (required) -- the owner the completion event carries, re-verified by an owner-filtered read",
			},
		},
		{
			Name: "step",
			Description: "The statement every step of a learned procedure's rendered SOURCE is. It always refuses (" +
				"\"" + stepRefusal + "\"): a learned procedure is served through the certification ladder by its " +
				"stored template, and its source is the artifact a person reads and approves, never an automation " +
				"that runs.",
			Handler: i.handleProcedureStep,
			ArgsSchema: map[string]string{
				"step": "integer (required)", "tool": "string (required)", "args": "object",
			},
		},
		{
			Name: "replay",
			Description: "Serve the current work run from one of its owner's learned procedures -- the one " +
				"statement of replayLearnedProcedure. Refuses outside a work run and on another person's " +
				"construct; re-decides the rung now, replays a trusted or canary procedure's template step by " +
				"step on the target its footprint names, and hands the goal to the app with the partial trace " +
				"when it cannot start or diverges. Returns {servedBy, constructId, rung, diverged, " +
				"startRefused, repairRunId, replayRunId, reason}; a goal neither served fails the run.",
			Handler:    i.handleProcedureReplay,
			ArgsSchema: map[string]string{"constructId": "string (required) -- the learned procedure to replay"},
		},
		{
			Name: "ladderSweep",
			Description: "The certification ladder's demotion or retirement sweep over every owner's learned " +
				"procedures, each read and written as its owner; only a changed procedure is written. Runs " +
				"only as the cluster's maintenance principal. Returns {sweep, owners, procedures, examined, " +
				"demoted, retired, changed, errors}.",
			Handler:    i.handleLadderSweep,
			ArgsSchema: map[string]string{"sweep": "string (required) -- demotion or retirement"},
		},
		{
			Name: "decidePromotion",
			Description: "Apply a decided procedurePromotion approval to the ladder: approved moves the " +
				"procedure from shadow to canary, rejected keeps it in shadow to re-earn the proposal. " +
				"Idempotent -- a construct no longer waiting on this approval, no longer in shadow, or no " +
				"longer the approved version is left as it is. Runs only as the cluster's maintenance " +
				"principal. Returns {approvalId, applied, from, to, reason}.",
			Handler:    i.handleDecidePromotion,
			ArgsSchema: map[string]string{"approvalId": "string (required)"},
		},
	}
}

// IntegrationName is the plug-in name.
func (i *Integration) IntegrationName() string { return integrationName }
