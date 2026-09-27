//go:build agent

package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_app_fallback.go -- a goal a replay cannot serve goes back to the
// APP (epic memql#5408, D16): its preconditions did not hold, a parameter
// could not be bound, or a step diverged. The app gets the goal statement and
// the guidance -- the steps that already ran and must not run again, and what
// diverged -- and repairs rather than resamples.
//
// THROUGH THE ROUTER'S SESSION DOOR, AND ONLY IT. The handover is a
// tools-modality request carrying the replay's run and CANONICAL step, which
// the router turns into an app SESSION: the whole step goes to the app, which
// drives its own loop, and the step then points at the session's subrun
// (childRunId) -- the repaired run the learner reads as a new recording. It is
// the one seam every model call goes through (design D2), so the handover is
// on the decision record like any other.
//
// THE DOOR IS PINNED TO AN APP. The request names the app the procedure was
// recorded from (`app:claude-code`), or any signed-in app when that is
// unknown, as its explicit provider -- so the router answers with an app
// session or refuses in its own words, and never with a model MemQL would
// drive, which is a different thing from "the app takes over". The answer is
// still checked: anything but a session door is refused here, before the call.
//
// THE CONSENT IS THE APP SESSION'S OWN: the delegate runs it through the same
// executor, and the same per-run and standing-scope gates, as a delegated
// task -- under the owner's reasoning agent, which is why the request carries
// it. A session runs only on the replica holding the machine's stream; a
// replay on the other replica is refused by the router, Handover returns that
// refusal as its error, and the runner fails the goal with
// procedure_fallback_failed rather than serving it somewhere else.
// (procedure_fallback_unavailable is the other answer: no fallback installed
// on the node at all.)

// procedureSessionResolver resolves a tools-modality request through the
// router this node already uses.
type procedureSessionResolver func(ctx context.Context, req airoute.ResolveRequest) (common.ToolCallingChatAIProvider, airoute.Resolution, error)

// procedureSessionReporter is what the router's tools client says about the
// app session its call ran as.
type procedureSessionReporter interface {
	LastSession() (memql.AppSessionOutcome, bool)
}

// procedureFallbackTag marks the handover on the decision record, so the
// ledger can say which session calls were a replay handing a goal back.
const procedureFallbackTag = "procedureFallback"

// procedureAppFallback is the procedure.AppFallback.
type procedureAppFallback struct {
	resolve procedureSessionResolver
	agents  procedureAgentResolver
}

var _ procedure.AppFallback = (*procedureAppFallback)(nil)

// newProcedureAppFallback builds the fallback over the engine's AI resolver
// -- the router -- and the planner's reasoning-agent rule.
func newProcedureAppFallback(engine *memql.MemQLEngine) *procedureAppFallback {
	return &procedureAppFallback{
		resolve: func(ctx context.Context, req airoute.ResolveRequest) (common.ToolCallingChatAIProvider, airoute.Resolution, error) {
			return memql.ResolveAITyped[common.ToolCallingChatAIProvider](ctx, engine, req)
		},
		agents: procedureReasoningAgents(engine),
	}
}

// Handover hands the goal to the app and waits for its session.
func (f *procedureAppFallback) Handover(ctx context.Context, req procedure.FallbackRequest) (procedure.FallbackOutcome, error) {
	owner := strings.TrimSpace(req.OwnerUserId)
	if owner == "" {
		return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: the goal names no owner, and an app session cannot run unattributed")
	}
	runId, stepId := strings.TrimSpace(req.RunId), strings.TrimSpace(req.StepId)
	if runId == "" || stepId == "" {
		return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: an app takes a goal back as a session opened from a step, and this handover names no run and step to open it from")
	}
	level := airoute.LevelReasoning
	if s := strings.TrimSpace(req.Level); s != "" {
		parsed, err := airoute.ParseLevel(s)
		if err != nil {
			return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: %w", err)
		}
		level = parsed
	}
	agentId := strings.TrimSpace(req.AgentId)
	if agentId == "" {
		agent, err := f.agents(ctx, owner)
		if err != nil {
			return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: an app session runs under the owner's reasoning agent, whose standing scope is its consent, and none resolves: %w", err)
		}
		agentId = agent.Id
	}
	prompt := procedureFallbackPrompt(req.Statement, req.Guidance.Prompt)
	if prompt == "" {
		return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: the handover carries neither a goal statement nor guidance, so there is nothing to hand the app")
	}

	door := memql.AppWildcard
	if app := strings.TrimSpace(req.App); airoute.IsRunnableApp(app) {
		door = memql.AppReferencePrefix + app
	}
	callCtx := auth.ContextWithUserActor(ctx, owner)
	request := airoute.ResolveRequest{
		Level:    level,
		Modality: airoute.ModalityTools,
		Needs: airoute.Needs{
			Tools:            true,
			MinContextTokens: airoute.EstimateMinContextTokens(prompt, 0),
		},
		ExplicitProvider: door,
		Tags:             []string{procedureFallbackTag},
		UserId:           owner,
		AgentId:          agentId,
		RunId:            runId,
		StepId:           stepId,
	}
	// A PERSON'S KNOBS BIND HERE (epic memql#5414, D20). When the step being
	// handed over is one a person re-ran or branched, its level, the app they
	// named and its effort are theirs -- applied by the one function every
	// model seam uses. A step nobody re-ran comes back unchanged.
	request, err := memql.ApplyStepOverride(callCtx, request)
	if err != nil {
		return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: %w", err)
	}
	client, resolution, err := f.resolve(callCtx, request)
	if err != nil {
		return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: the router would not hand the goal to %s: %w", door, err)
	}
	if resolution.Decision.Door != airoute.DoorSession || client == nil {
		return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: the router resolved %q on the %q door, not an app session -- a diverged goal goes back to the app, never to a model MemQL would drive",
			resolution.ProviderName, resolution.Decision.Door)
	}

	result, err := client.CallChatWithTools(callCtx, []common.ChatMessage{{Role: "user", Content: prompt}}, nil)
	if err != nil {
		return procedure.FallbackOutcome{}, fmt.Errorf("procedure fallback: the app session did not take the goal: %w", err)
	}
	out := procedure.FallbackOutcome{}
	if result != nil {
		out.Content = result.AssistantText
	}
	if reporter, ok := client.(procedureSessionReporter); ok {
		if session, ok := reporter.LastSession(); ok {
			out.ChildRunId = session.ChildRunId
			out.SessionId = session.SessionId
			if session.Content != "" {
				out.Content = session.Content
			}
		}
	}
	return out, nil
}

// procedureFallbackPrompt is the goal statement with the guidance after it.
// Guidance that already opens with the statement is used as it is: the app is
// told the goal once.
func procedureFallbackPrompt(statement, guidance string) string {
	statement, guidance = strings.TrimSpace(statement), strings.TrimSpace(guidance)
	switch {
	case guidance == "":
		return statement
	case statement == "" || strings.HasPrefix(guidance, statement):
		return guidance
	}
	return statement + "\n\n" + guidance
}
