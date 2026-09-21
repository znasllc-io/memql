package memql

// run_ceilings.go -- the run's ceilings, enforced (memql#5580).
//
// component/work.CheckCeilings had no production caller. The dollar-versus-loop
// split was written, documented and unit-tested, and a run's costCeiling,
// tokenBudget and maxModelCalls were settable fields nothing read: an operator
// who set one to bound an automation's model spend bounded nothing, and the
// only real stop was the PROCESS-wide rate ceiling and circuit breaker at the
// provider transport (ai_guard.go), which one runaway run shares with every
// other caller on the node.
//
// WHY THIS SEAM AND NOT ANOTHER. Three places could have owned the check:
//
//	the model seam    HERE. It is the one funnel both covered call sites pass
//	                  through (InvokeAI and InvokeAIStructured -- the DSL
//	                  `ai(...)` form is how an automation step, and therefore
//	                  a work run, reaches a model). It reads the run off the
//	                  context, it sits BEFORE the provider call so the call
//	                  can still be refused, and it already knows whether the
//	                  answer came from a provider, a fleet machine or the
//	                  replay journal -- which is exactly the discriminator
//	                  the dollar/loop split needs.
//	the step runner   WRONG GRAIN. A step may make many model calls; a check
//	                  per step cannot refuse the fifth `ai()` inside one step
//	                  against maxModelCalls: 3, which is the cap's whole job.
//	the journal       TOO LATE. component/automations' journal writes a step's
//	                  receipt AFTER the body ran. It is the right place to
//	                  record a breach and it does -- but a record is not a
//	                  refusal.
//
// The router was the fourth candidate and loses for a reason worth stating: a
// journal hit never reaches it (the live closure is not called), so a router-
// level check would be blind to exactly the answers the loop cap exists to
// count. It also already owns a DIFFERENT ceiling -- the process-wide one --
// and two ceilings answering at one site under one refusal code is how they
// stop being distinguishable.
//
// THE DECISION IS NOT MADE HERE, for the reason model_journal.go's header
// gives at length: component/memql is imported by fifteen modules and none of
// them may be made to carry component/work in its go.mod. So this file holds
// the SEAM and the mirror types, and integrations/work holds the guard that
// reads the goal's ceilings, folds the run's spend and calls
// work.CheckCeilings.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// Served values a charge reports. They mirror component/work's set; the two
// that name no v1:work:modelCall row are the ones this package produces
// itself.
const (
	ServedLive         = "live"
	ServedJournal      = "journal"
	ServedLocal        = "local"
	ServedSubscription = "subscription"
	// ServedCache is an in-process response cache hit in the ai() runtime.
	// Nothing is journaled for one and nobody was billed -- but it is still
	// an answered request, so the loop caps count it.
	ServedCache = "cache"
)

// RunCeilingBreach is the ceiling a run reached, in the shape this package can
// speak without importing the package that decided it. It mirrors
// component/work.CeilingBreach field for field.
type RunCeilingBreach struct {
	// Ceiling is the ceiling's name (`tokens`, `cost`, `modelCalls`,
	// `wallClock`, ...), or `unevaluated` when the ceilings could not be
	// read at all.
	Ceiling string
	// Limit is the ceiling's value, rendered.
	Limit string
	// Actual is what the run has spent, rendered.
	Actual string
	// Reason is the breach in a sentence.
	Reason string
}

// RunSpend is the run's spend at the moment it was refused, carried so the
// park can put it on the row. It mirrors component/work.Spent.
type RunSpend struct {
	Tokens             int
	TokensSubscription int
	TokensLocal        int
	Cost               float64
	ModelCalls         int
	WallClockMs        int64
}

// ModelSpend is what one answered request cost, as the seam reports it.
type ModelSpend struct {
	// Served is one of the Served* constants above.
	Served       string
	InputTokens  int
	OutputTokens int
	Cost         float64
}

// RunCeilingGuard is the seam, implemented in integrations/work.
//
// AN INTERFACE FOR ModelCallJournal'S REASONS, both of them: the ceilings live
// on v1:work:goal and the spend on v1:work:modelCall, which this package may
// not read, and work.CheckCeilings may not be imported here.
type RunCeilingGuard interface {
	// Admit reports the ceiling this run has reached, or nil to proceed.
	// estimatedTokens is the cost of the call about to be made, so the check
	// happens BEFORE the spend rather than after it.
	//
	// A run the guard cannot evaluate answers with a breach naming
	// `unevaluated` rather than nil: a ceiling that could not be read must
	// not read like a ceiling nobody set.
	Admit(ctx context.Context, run common.RunContext, estimatedTokens int) *RunCeilingBreach
	// Charge records one answered request against the run. It is called once
	// per answer, whoever answered -- a provider, a fleet machine, the replay
	// journal or an in-process cache.
	Charge(ctx context.Context, run common.RunContext, spend ModelSpend)
	// Spent reports the run's spend so far, for a refusal to carry.
	Spent(ctx context.Context, run common.RunContext) RunSpend
}

// SetRunCeilingGuard installs the guard. Wired from app/ beside the model-call
// journal, on any node that hosts the work integration.
//
// NIL IS A WORKING CONFIGURATION IN ONE SENSE ONLY: a node that hosts no work
// integration runs no work runs, so there are no run ceilings to enforce
// there. It is NOT a licence to run goal-backed work without the guard, which
// is why app/ logs which of the two it wired.
func (e *MemQLEngine) SetRunCeilingGuard(g RunCeilingGuard) {
	if e == nil || e.modelSeam == nil {
		return
	}
	e.modelSeam.ceilings = g
}

// RunCeilingError is a model call refused because its run reached a ceiling.
//
// IT IS TYPED because the decision made from it -- park the run on a `budget`
// approval rather than fail it -- needs the breach's FIGURES, and reading them
// back out of a rendered sentence would be a parser of our own output. The
// same argument the door report already won.
//
// Its message deliberately does NOT contain any component/work refusal code:
// work.DoorsFrom falls back to a substring match over those codes, and a run
// ceiling is not the process cost ceiling the router refuses with. Two
// ceilings that read as one are two ceilings an operator cannot tell apart.
type RunCeilingError struct {
	RunId   string
	StepKey string
	Breach  RunCeilingBreach
	Spent   RunSpend
}

func (e *RunCeilingError) Error() string {
	step := e.StepKey
	if step == "" {
		step = "<unnamed step>"
	}
	return fmt.Sprintf("run %s reached its %s ceiling at step %s (limit %s, %s): %s",
		e.RunId, e.Breach.Ceiling, step, e.Breach.Limit, e.Breach.Actual, e.Breach.Reason)
}

// admit asks the guard whether this run may make another model call.
//
// A RETURN OF nil IS THE ONLY WAY THROUGH, and the three ways to get one are
// all honest: there is no run on the context (most calls in the product), the
// run names no goal and therefore inherits no ceilings, or the guard looked
// and found none reached. "No guard wired" is a fourth and is logged by the
// wiring rather than swallowed here -- a node with no work integration has no
// work runs.
func (s *modelSeam) admit(ctx context.Context, estimatedTokens int) error {
	if s == nil || s.ceilings == nil {
		return nil
	}
	rc, inRun := common.RunFromContext(ctx)
	if !inRun {
		return nil
	}
	breach := s.ceilings.Admit(ctx, rc, estimatedTokens)
	if breach == nil {
		return nil
	}
	return &RunCeilingError{
		RunId:   rc.RunId,
		StepKey: rc.StepKey,
		Breach:  *breach,
		Spent:   s.ceilings.Spent(ctx, rc),
	}
}

// charge records one answered request. A call outside a run charges nothing.
func (s *modelSeam) charge(ctx context.Context, spend ModelSpend) {
	if s == nil || s.ceilings == nil {
		return
	}
	rc, inRun := common.RunFromContext(ctx)
	if !inRun {
		return
	}
	s.ceilings.Charge(ctx, rc, spend)
}

// estimateRequestTokens is the size of the call about to be made, for the
// token budget's before-the-spend check.
//
// It reuses core/airoute's estimator rather than a second one: the router
// already sizes every request this way to pick a context window, and two
// estimators disagreeing about one request is how a budget and a routing
// decision stop describing the same call.
func estimateRequestTokens(req common.ModelRequest) int {
	parts := make([]string, 0, len(req.Messages)+1)
	for _, m := range req.Messages {
		parts = append(parts, m.Content)
	}
	if len(req.Schema.Schema) > 0 {
		parts = append(parts, string(req.Schema.Schema))
	}
	return airoute.EstimateMinContextTokensFor(0, parts...)
}
