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
	"time"

	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// compileEngine is the composite seam compile needs off the engine. Both
// halves are optional and each degrades on its own.
type compileEngine interface {
	authoringNearMatcher
	authoringSandbox
}

// WorkCompiler satisfies workintegration.Compiler.
type WorkCompiler struct {
	loop *PlannerAgentLoop
}

// NewWorkCompiler returns nil when there is no loop, so app wiring can call
// SetCompiler unconditionally and a node that runs no planner installs
// nothing.
func NewWorkCompiler(loop *PlannerAgentLoop) *WorkCompiler {
	if loop == nil {
		return nil
	}
	return &WorkCompiler{loop: loop}
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

	out, err := c.loop.CompileGoalForRun(ctx, CompileRequest{
		GoalId:      req.GoalId,
		RunId:       req.RunId,
		OwnerUserId: req.OwnerUserId,
		Statement:   req.Statement,
		Input:       req.Input,
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
	c.write(ctx, req.OwnerUserId, "updateWorkRun", args)

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
	c.write(ctx, req.OwnerUserId, "updateWorkRun", map[string]any{
		"runId":        req.RunId,
		"status":       "failed",
		"errorCode":    "compile_failed",
		"errorMessage": err.Error(),
		"finishedAt":   time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// write renders a @serverOnly work mutation under the goal owner's borrowed
// authority. The owner arrives on the request, copied from a goal row the
// caller had already read under their own actor, so it can never name a user
// the caller could not act as.
func (c *WorkCompiler) write(ctx context.Context, ownerUserId, name string, args map[string]any) {
	if c.loop.engine == nil {
		return
	}
	call := name + "(" + encodeArgs(args) + ")"
	if _, err := c.loop.engine.Execute(ownerActorContext(ctx, ownerUserId), call); err != nil && c.loop.logger != nil {
		c.loop.logger.Warn("work compile: journal write failed; the run continues", "mutation", name, "error", err)
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
func (p *PlannerIntegration) WorkCompiler() *WorkCompiler {
	if p == nil {
		return nil
	}
	return NewWorkCompiler(p.agentLoop)
}
