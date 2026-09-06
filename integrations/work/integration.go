// Package work is the Go half of the work spine (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, epic A2). It backs
// the seven builtins declared in dsl/work/builtins.memql:
//
//	integration.work.createGoal      -- open a goal and its first run
//	integration.work.cancelGoal      -- ask every run of a goal to stop
//	integration.work.forkRun         -- derive a run that diverges at a step
//	integration.work.replayRun       -- derive a run served from the journal
//	integration.work.decideApproval  -- decide a human gate and resume
//	integration.work.sweepWaiting    -- resume due timers, close dead runs
//	integration.work.retentionSweep  -- archive then delete journal detail
//
// THE DIVISION OF LABOUR IS THE DESIGN. Every DECISION is a pure function in
// component/work -- the compile order, the symptom table, the replay verdict,
// the ceilings, the artifact-hash rule -- so the spec's headline claims are
// properties of values and are provable with no engine, no provider and no
// database. This package is only responsible for OBEYING those decisions and
// for the two things a pure package cannot do: reaching the engine, and
// getting the actor right (see store.go's header, which is the file to read
// before changing anything here).
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/num"
)

// integrationName is the plug-in name and the middle segment of every
// capability FQN. Spelled as a STRING LITERAL in RegisterPlugin below as well,
// because the module-taxonomy gate finds registrations by scanning source for
// the literal; TestRegistrationNameIsTheLiteral asserts the two agree.
const integrationName = "work"

// resultConcept is the synthetic concept a capability reply rides on. It is
// never persisted -- the reply is a value the caller reads, the same shape
// every other integration answers with.
const resultConcept = "v1:work:result"

// Integration exposes the work capabilities.
type Integration struct {
	engine Engine
	logger *slog.Logger

	// bunDB is the raw handle the two sweeps need. Both ask questions the
	// work namespace has no query for -- "every run in flight, whoever owns
	// it" and "every journal row past its window" -- and a hand-rolled read
	// is the only way to ask them today (see sweep.go's header, which also
	// records the DSL queries that would replace it).
	bunDB func() *bun.DB

	// admitRow is the per-row authorization gate applied to the rows the
	// sweeps fetch THEMSELVES. A hand-rolled SELECT passes through neither
	// the parser nor the filter path, so nothing is injected into it and
	// this callback is the whole of the enforcement there. Nil is REFUSED at
	// construction rather than treated as "admit everything".
	admitRow func(ctx context.Context, node memorynodes.MemoryNode) bool

	// archiver is object storage for the retention sweep. NIL IS AN ANSWER:
	// no archive means no delete, so a cluster with no archive container
	// keeps its journal and the sweep says so every night.
	archiver Archiver

	// compiler is the compile seam (design section B, "Compile"). Set by the
	// node that runs compile; nil everywhere else, and a nil one leaves a
	// freshly opened run in `compiling` rather than inventing a plan.
	compiler Compiler

	// rowsInFlight is the source of the abandoned sweep's rows. It is a
	// FIELD rather than a method call so the sweep's per-row decisions --
	// which run is parked, which is dead, whose authority each write borrows
	// -- are testable without a database, while the production path stays the
	// hand-rolled read in sweep.go. Set in New; a nil one is a programmer
	// error the sweep reports rather than reading nothing.
	rowsInFlight func(ctx context.Context) ([]map[string]any, error)

	now func() time.Time

	mu sync.RWMutex
}

// Compiler is what createGoal dispatches to once the goal and its first run
// exist. It is a SEAM rather than an implementation because compile is the
// other half of epic A2: the catalog lookup, the three prompts and the Gate-1
// sandbox compile-and-bind. The ORDER it must follow is already pure and
// already tested -- component/work.Decide -- so an implementation of this
// interface is responsible for obeying that decision and nothing more.
type Compiler interface {
	// Compile runs the compile order for one run. It is called on a
	// DETACHED goroutine, so it must not assume the caller's context is
	// still live and must journal its own failures onto the run.
	Compile(ctx context.Context, req CompileRequest)
}

// CompileRequest is everything compile needs that createGoal already has in
// hand. The owner rides along so the compiler can borrow the same authority
// without re-reading the goal.
type CompileRequest struct {
	GoalId      string
	RunId       string
	OwnerUserId string
	Statement   string
	Input       map[string]any
	Ceilings    map[string]any
}

// New constructs the integration. Tests call this with a stub engine.
func New(engine Engine, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	i := &Integration{engine: engine, logger: logger, now: time.Now}
	i.rowsInFlight = i.runsInFlight
	return i
}

func init() {
	memql.RegisterPlugin("work", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// REFUSE rather than default. A nil AdmitSourceRow means this node
		// cannot tell whether a caller may see a row, and "cannot tell" must
		// never resolve to "everyone may" -- the sweeps read every owner's
		// runs by construction, which is exactly the read that must not be
		// hand-waved (component/memql/plugins.go states the rule).
		if pctx.AdmitSourceRow == nil {
			return nil, fmt.Errorf("work plug-in: no AdmitSourceRow in plugin context")
		}
		i := New(pctx.Engine, pctx.Logger)
		i.bunDB = pctx.BunDB
		i.admitRow = pctx.AdmitSourceRow
		return i, nil
	})
}

// SetCompiler installs the compile surface. Called once, from the node that
// runs compile, before the first goal. A later call is ignored for the reason
// packages' SetWorkbench states: two halves of one run must not disagree about
// where the work went.
func (i *Integration) SetCompiler(c Compiler) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.compiler == nil {
		i.compiler = c
	}
}

// SetArchiver installs object storage for the retention sweep.
func (i *Integration) SetArchiver(a Archiver) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.archiver = a
}

// SetNow injects a clock. Tests only.
func (i *Integration) SetNow(f func() time.Time) {
	if f != nil {
		i.now = f
	}
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

func (i *Integration) store() *store {
	return &store{engine: i.engine}
}

func (i *Integration) compilerRef() Compiler {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.compiler
}

func (i *Integration) archiverRef() Archiver {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.archiver
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider. Each Name is the last
// segment of the @executor FQN its builtin declares; the registry namespaces
// them as integration.work.<name>. TestCapabilityNamesMatchTheDSL asserts the
// set against dsl/work/builtins.memql, because a capability the DSL names and
// the registry lacks is a BOOT failure on every node type.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "createGoal",
			Description: "Accept a goal and start work on it: opens a v1:work:goal owned by the caller and its first v1:work:run in `compiling`, then dispatches compile. Returns {goalId, runId, compileDispatched}.",
			Handler:     i.handleCreateGoal,
			ArgsSchema: map[string]string{
				"statement":    "string (required) -- the goal in the person's own words",
				"input":        "object -- the typed input object the chosen template's args declare",
				"accountIds":   "[]string -- account tags; a record of who the work is for, never a visibility scope",
				"ceilings":     "object -- {tokenBudget, costCeiling, wallClockMs, maxRetries, maxModelCalls, maxEvents}; a zero is 'unset', never 'nothing allowed'",
				"requestedVia": "string -- api | ask | nexus | responsibility | library | materializer; empty when unknown",
			},
		},
		{
			Name:        "cancelGoal",
			Description: "Ask every live run of one of the caller's goals to stop. Cancellation is REQUESTED rather than done: a run notices at its next step boundary, so a step already in flight finishes and is journaled. Returns {goalId, runsAsked, goalClosed}.",
			Handler:     i.handleCancelGoal,
			ArgsSchema: map[string]string{
				"goalId": "string (required) -- the v1:work:goal to close",
				"reason": "string -- why it closed, in words a person can read",
			},
		},
		{
			Name:        "forkRun",
			Description: "Fork one of the caller's runs at a step: a NEW run that serves the shared prefix from the journal and runs live from the fork step on. The source run is untouched. Returns {runId, forkedFromRunId, atStepKey}.",
			Handler:     i.handleForkRun,
			ArgsSchema: map[string]string{
				"runId":     "string (required) -- the run to fork",
				"atStepKey": "string (required) -- the step key to diverge at",
				"variables": "object -- variable overrides for the fork",
			},
		},
		{
			Name:        "replayRun",
			Description: "Replay one of the caller's runs: a NEW run that serves EVERY model call from the journal, so it reaches no provider. Returns {runId, replayOfRunId, policy}.",
			Handler:     i.handleReplayRun,
			ArgsSchema: map[string]string{
				"runId":  "string (required) -- the run to replay",
				"policy": "string -- strict (default) raises a divergence on a journal miss; permissive makes a fresh call and journals it",
			},
		},
		{
			Name:        "decideApproval",
			Description: "Decide one of the caller's pending approvals and resume the run parked on it. Refused when the artifact changed since it was approved. Returns {approvalId, runId, decision, runResumed}.",
			Handler:     i.handleDecideApproval,
			ArgsSchema: map[string]string{
				"approvalId": "string (required) -- the v1:work:approval to decide",
				"decision":   "string (required) -- approved, rejected, or answered",
				"answer":     "object -- the person's answer, for a feedback approval",
			},
		},
		{
			Name:        "sweepWaiting",
			Description: "Resume every run whose timer wait is due and close every run whose node stopped answering. Cluster-owner floored; the scheduled automation runs under the cluster's maintenance principal. Returns {checked, resumed, abandoned}.",
			Handler:     i.handleSweepWaiting,
			ArgsSchema: map[string]string{
				"olderThanSeconds": "int -- how long a run must have been silent before it counts as abandoned; defaults to twice the heartbeat window",
			},
		},
		{
			Name:        "retentionSweep",
			Description: "Fold each affected run's summary, archive expired journal rows to blob storage, then delete them. No archive means no delete. Returns {boundaryModelCall, boundaryObservation, runsSummarized, rowsArchived, rowsDeleted, objects, refused}.",
			Handler:     i.handleRetentionSweep,
			ArgsSchema: map[string]string{
				"dryRun": "boolean -- report what would be archived and deleted without doing either",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Replies
// ---------------------------------------------------------------------------

// resultNode wraps a capability's answer as the single node the engine hands
// back to the caller.
func (i *Integration) resultNode(payload map[string]any) []memorynodes.MemoryNode {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	at := i.clock().UTC()
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("work:%d", at.UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: at,
		Payload:   raw,
	}}
}

// ---------------------------------------------------------------------------
// Argument reading
// ---------------------------------------------------------------------------

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if s, ok := args[key].(string); ok {
		return trim(s)
	}
	return ""
}

func argMap(args map[string]any, key string) map[string]any {
	if args == nil {
		return nil
	}
	if m, ok := args[key].(map[string]any); ok {
		return m
	}
	return nil
}

func argStrings(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && trim(s) != "" {
				out = append(out, trim(s))
			}
		}
		return out
	}
	return nil
}

func argBool(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	b, _ := args[key].(bool)
	return b
}

// argInt narrows a decoded payload number to an int with the CALLER-DEFAULT
// answer (core/num's third named answer, num.Int64Or / num.Float64Or).
//
// The default is the right answer here rather than saturation or zero, and
// both alternatives are actively wrong for this field. Saturating
// olderThanSeconds at MaxInt would make no run ever old enough to sweep;
// zeroing it would make EVERY run in the cluster read as abandoned on the next
// pass. A bare int(v) is worse than either: out of range it is
// implementation-defined and answers with the integer indefinite value, which
// is hugely negative and inverts the > 0 guard below (memql#4779).
//
// A non-positive value also takes the default: "zero seconds of silence" is
// not a window an operator can have meant.
func argInt(args map[string]any, key string, fallback int) int {
	if args == nil {
		return fallback
	}
	var n int
	switch v := args[key].(type) {
	case int:
		n = v
	case int64:
		n = num.Int64Or(v, fallback)
	case float64:
		n = num.Float64Or(v, fallback)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return fallback
		}
		n = num.Int64Or(parsed, fallback)
	default:
		return fallback
	}
	if n <= 0 {
		return fallback
	}
	return n
}

// ---------------------------------------------------------------------------
// The compile writer's surface
// ---------------------------------------------------------------------------
//
// Compile runs in integrations/planner, because everything it does the work
// with -- the design pass, the emit-and-repair loop, the triage classifier,
// the catalog read -- is unexported there. But its WRITES land on
// @serverOnly work constructs, and `auth.OriginFromContext` returns
// OriginClient for any context nobody stamped, so an unstamped write is
// REFUSED with one WARN and nothing above it hears: the run never leaves
// `compiling`, which reads as a goal accepted and then ignored.
//
// The stamp cannot simply move to the planner. Stamping is allowlisted per
// package (call_origin_conformance_test.go) and integrations/planner is
// large and request-derived -- admitting it would put every @serverOnly
// construct in the tree behind one of its many request paths. So the write
// stays HERE, where the allowlist entry and the single stamping site already
// are, and the planner delegates through these two methods.

// RecordCompileOutcome advances a run with what compile decided: the
// template it chose, the variables it bound, and the status to move to.
// Fields not named keep their prior value.
func (i *Integration) RecordCompileOutcome(ctx context.Context, ownerUserId, runId string, fields map[string]any) error {
	if i == nil {
		return fmt.Errorf("work: no integration")
	}
	if strings.TrimSpace(runId) == "" {
		return fmt.Errorf("work: RecordCompileOutcome needs a run id")
	}
	// The run is the goal owner's, so the write borrows their authority --
	// the owner arrives from a goal row the caller already read under their
	// own actor, so it can never name somebody they could not act as.
	return i.store().updateRun(ownerActor(ctx, ownerUserId), runId, fields)
}

// RaiseApproval writes one human gate and returns its id. Used by the
// healing subscriber, whose typed patches must reach a person as a
// planReview approval rather than being applied (design decision D5: never a
// silent edit, even to the run's own draft template).
func (i *Integration) RaiseApproval(ctx context.Context, ownerUserId string, a ApprovalSeed) (string, error) {
	if i == nil {
		return "", fmt.Errorf("work: no integration")
	}
	if strings.TrimSpace(a.RunId) == "" {
		// An approval with no run is a question nobody can answer.
		return "", fmt.Errorf("work: RaiseApproval needs a run id")
	}
	if strings.TrimSpace(a.ApprovalId) == "" {
		a.ApprovalId = newRowId(approvalConcept)
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = i.clock().UTC()
	}
	seed := approvalSeed(a)
	if err := i.store().createApprovalRow(ownerActor(ctx, ownerUserId), seed); err != nil {
		return "", err
	}
	return a.ApprovalId, nil
}

// ApprovalSeed is the exported shape of one human gate. It mirrors the
// unexported seed exactly; the conversion in RaiseApproval is what keeps the
// two from drifting, because a field added to one and not the other will not
// compile.
type ApprovalSeed struct {
	ApprovalId   string
	RunId        string
	StepKey      string
	Kind         string
	Subject      map[string]any
	ArtifactHash string
	Question     string
	Options      []map[string]any
	Evidence     map[string]any
	RequestedAt  time.Time
	ExpiresAt    time.Time
}

// RunBudget reports the ceilings a run inherits from its goal.
//
// WHY THE CEILINGS AND NOT THE SPEND. v1:work:run.spent is written by exactly
// one thing today -- component/workjournal stamps wallClockMs when a run
// closes -- so its tokens, cost and modelCalls are absent on every live run.
// A cumulative check against them would compare a real ceiling to a zero that
// means "nobody measured", conclude there is budget left, and never block:
// the same fail-open shape this method exists to close, moved one layer down.
// So the caller supplies what it has actually spent, which for a compile is a
// number it counted itself.
//
// Read under the OWNER's borrowed authority, like every other read here: the
// goal is owner-tiered, and an unstamped read answers zero rows and no error,
// which would present as "this goal has no ceilings" -- unbounded.
func (i *Integration) RunBudget(ctx context.Context, ownerUserId, runId string) (work.Ceilings, error) {
	var out work.Ceilings
	if i == nil {
		return out, fmt.Errorf("work: no integration")
	}
	if strings.TrimSpace(runId) == "" {
		return out, fmt.Errorf("work: RunBudget needs a run id")
	}
	scoped := ownerActor(ctx, ownerUserId)
	run, err := i.store().runForOwner(scoped, runId)
	if err != nil {
		return out, err
	}
	if run == nil {
		return out, fmt.Errorf("work: run %s is not readable as %s", runId, ownerUserId)
	}
	goalId := rowString(run, "goalId")
	if goalId == "" {
		return out, nil
	}
	goal, err := i.store().goalForOwner(scoped, goalId)
	if err != nil || goal == nil {
		return out, err
	}
	raw := rowMap(goal, "ceilings")
	if len(raw) == 0 {
		// ABSENT, not zero. A goal with no ceilings declared is unbounded by
		// this gate, which is the deployment's default -- and the caller's
		// own attempt cap is what still bounds it.
		return out, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return out, fmt.Errorf("work: goal %s ceilings: %w", goalId, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("work: goal %s ceilings: %w", goalId, err)
	}
	return out, nil
}
