package planner

// work_compile_adapter.go -- binds the planner's compile pass to the work
// integration's Compiler seam (epic memql#4966).
//
// WITHOUT THIS FILE THE FEATURE DOES NOT RUN. integrations/work declares
// `Compiler` as a seam and, with none installed, createGoal opens the goal
// and its first run and returns compileDispatched:false -- the run sits in
// `compiling` forever, which from the outside looks exactly like a goal
// that was accepted and then ignored. The two halves were written on
// either side of the seam and nothing joined them.
//
// It lives here rather than in integrations/work because compile needs the
// unexported authoring pipeline on *PlannerAgentLoop, and here rather than
// in app/ because the near-match and sandbox seams are resolved by type
// assertion off the engine -- the same assertion the capture dispatcher
// makes, and the same graceful degradation: a build without them compiles
// goals by the cheap tiers and refuses to author, rather than refusing to
// start.

import (
	"context"
	"errors"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// compileEngine is the composite seam compile needs off the engine. Both
// halves are optional and each degrades on its own.
type compileEngine interface {
	authoringNearMatcher
	authoringSandbox
}

// runWriter is how compile records its outcome. The planner does NOT write
// the work rows itself, and that is not a style choice: the writes land on
// @serverOnly constructs, `auth.OriginFromContext` returns OriginClient for
// any context nobody stamped, and an unstamped write is REFUSED with one WARN
// that nothing above it hears -- the run would never leave `compiling`, which
// reads as a goal accepted and then ignored.
//
// The stamp cannot move here. Stamping is allowlisted per PACKAGE
// (call_origin_conformance_test.go) and integrations/planner is large and
// request-derived; admitting it would put every @serverOnly construct in the
// tree behind one of its many request paths. So the write stays in
// integrations/work, which already holds the allowlist entry and exactly one
// stamping site, and this delegates.
type runWriter interface {
	RecordCompileOutcome(ctx context.Context, ownerUserId, runId string, fields map[string]any) error
	// RunBudget reports the ceilings this run inherits from its goal.
	RunBudget(ctx context.Context, ownerUserId, runId string) (work.Ceilings, error)
}

// WorkCompiler satisfies workintegration.Compiler.
type WorkCompiler struct {
	loop   *PlannerAgentLoop
	writer runWriter
}

// NewWorkCompiler returns nil without a loop OR without a writer, so app
// wiring can call SetCompiler unconditionally and a node that runs no planner
// installs nothing. A nil writer is refused rather than defaulted: a compiler
// that decides correctly and cannot record the decision is worse than none,
// because the run still moves to `running` in the log and never in the graph.
func NewWorkCompiler(loop *PlannerAgentLoop, writer runWriter) *WorkCompiler {
	if loop == nil || writer == nil {
		return nil
	}
	return &WorkCompiler{loop: loop, writer: writer}
}

// Compile runs the compile order and records what it chose on the run.
//
// It is called on a DETACHED goroutine, so it does not assume the caller's
// context is live and it journals its own failure onto the run rather than
// returning it to nobody.
func (c *WorkCompiler) Compile(ctx context.Context, req workintegration.CompileRequest) {
	if c == nil || c.loop == nil {
		return
	}
	near, sandbox := c.seams()

	// THE CEILING IS READ BEFORE THE COMPILE, not during it. A read failure
	// leaves it UNSET -- unbounded by this gate -- rather than blocking the
	// compile, which matches every other ceiling here and keeps a transient
	// database blip from making goals unrunnable. The attempt cap still
	// bounds the loop, and the failure is logged rather than swallowed.
	var maxCalls int
	if ceilings, cerr := c.writer.RunBudget(ctx, req.OwnerUserId, req.RunId); cerr != nil {
		if c.loop.logger != nil {
			c.loop.logger.Warn("work compile: could not read the run's ceilings; the authoring repair loop is bounded by its attempt cap alone",
				"runId", req.RunId, "error", cerr)
		}
	} else {
		maxCalls = ceilings.MaxModelCalls
	}

	out, err := c.loop.CompileGoalForRun(ctx, CompileRequest{
		GoalId:        req.GoalId,
		RunId:         req.RunId,
		OwnerUserId:   req.OwnerUserId,
		Statement:     req.Statement,
		Input:         req.Input,
		MaxModelCalls: maxCalls,
	}, near, sandbox)
	if err != nil {
		c.failRun(ctx, req, err)
		return
	}

	// Record the template compile chose. This is why updateWorkRun accepts
	// automationName, templateConstructId and variables: the run is opened
	// BEFORE the template is known -- that ordering is the design, so the
	// model calls compilation makes have a home from the first one -- and
	// without this write the choice exists only in a log line.
	args := map[string]any{
		"runId":          req.RunId,
		"status":         "running",
		"automationName": out.AutomationName,
	}
	if out.ConstructId != "" {
		args["templateConstructId"] = out.ConstructId
	}
	if len(req.Input) > 0 {
		args["variables"] = req.Input
	}
	c.record(ctx, req.OwnerUserId, req.RunId, args)

	if c.loop.logger != nil {
		c.loop.logger.Info("work compile decided",
			"goalId", req.GoalId, "runId", req.RunId,
			"route", string(out.Route), "modelCalls", out.ModelCalls,
			"automation", out.AutomationName, "gaps", len(out.Gaps))
	}
}

// failRun marks the run failed with the compile error. A goal whose compile
// failed must not sit in `compiling`: the sweep deliberately does not touch
// a run in that state, so nothing else would ever move it.
func (c *WorkCompiler) failRun(ctx context.Context, req workintegration.CompileRequest, err error) {
	if c.loop.logger != nil {
		c.loop.logger.Warn("work compile failed", "goalId", req.GoalId, "runId", req.RunId, "error", err)
	}
	// A DIVERGENCE IS NOT A COMPILE FAILURE (memql#4999). A strict replay
	// whose journal has no match for a request stops on purpose -- the
	// compile ran, the model seam refused to substitute a live call, and the
	// run has a step key naming exactly where the two runs parted. Recording
	// that as `compile_failed` would send the reader at the compiler, and the
	// Nexus run page could not tell the two apart to say otherwise.
	code := "compile_failed"
	var diverged *memqlengine.DivergenceError
	if errors.As(err, &diverged) {
		code = memqlengine.ErrorCodeReplayDiverged
	}
	c.record(ctx, req.OwnerUserId, req.RunId, map[string]any{
		"status":       "failed",
		"errorCode":    code,
		"errorMessage": err.Error(),
		"finishedAt":   time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// record hands the outcome to the work integration, which stamps internal
// origin at its one site and borrows the owner's authority.
func (c *WorkCompiler) record(ctx context.Context, ownerUserId, runId string, fields map[string]any) {
	delete(fields, "runId")
	if err := c.writer.RecordCompileOutcome(ctx, ownerUserId, runId, fields); err != nil && c.loop.logger != nil {
		c.loop.logger.Warn("work compile: recording the outcome failed; the run keeps its previous status",
			"runId", runId, "error", err)
	}
}

// seams resolves the near-matcher and the sandbox off the engine. Each is
// independently optional: with no near-matcher compile skips that tier, and
// with no sandbox it can still serve every catalogued and triaged route and
// refuses only to author.
func (c *WorkCompiler) seams() (authoringNearMatcher, authoringSandbox) {
	ce, ok := c.loop.engine.(compileEngine)
	if !ok {
		return nil, nil
	}
	return ce, ce
}

// WorkCompiler returns the compile surface this integration offers the work
// spine, or nil on an integration with no agent loop. app wiring installs it
// on the work integration with SetCompiler.
func (p *PlannerIntegration) WorkCompiler(writer runWriter) *WorkCompiler {
	if p == nil {
		return nil
	}
	return NewWorkCompiler(p.agentLoop, writer)
}
