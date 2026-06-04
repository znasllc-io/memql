package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/taskstamp"
)

// roleCapturingExecutor records the acting-agent role visible on ctx each
// time a tool is dispatched -- exactly what the engine's ExecuteTool gate
// reads. Stands in for the engine so a test can assert the role survives the
// hop from the lane caller's ctx, through runNonStreamingToolLoop, to the
// tool executor.
type roleCapturingExecutor struct{ rolesSeen []string }

func (e *roleCapturingExecutor) ExecuteToolByName(ctx context.Context, _ string, _ map[string]any) (string, error) {
	e.rolesSeen = append(e.rolesSeen, memql.ActingAgentRoleFromContext(ctx))
	return "ok", nil
}
func (e *roleCapturingExecutor) Execute(_ context.Context, _ string) (any, error) { return nil, nil }

// TestStampActingAgentRoleIfMissing_PlannerPath is the memql#938 regression
// guard. On a planner-driven produceArtifact / post-approval turn the
// incoming AgentGenerateTurnMsg arrives with a nil ActingAgent (the planner
// can't load v1:agents:agent), so agent_turn_handlers.go stamps NO acting-
// agent role on ctx at dispatch. fillActingAgentIfEmpty then self-resolves
// ActingAgent on the agent node for prompt rendering; this helper must mirror
// the resolved role + id onto ctx so ExecuteTool's universal agent-only gate
// admits the agent's tools (workbenchHost / canvasPublish) instead of
// hard-rejecting with "tools are agent-only -- no acting agent on context".
func TestStampActingAgentRoleIfMissing_PlannerPath(t *testing.T) {
	msg := &memqlv1.AgentGenerateTurnMsg{
		AgentId: "v1:agents:agent:abc",
		ActingAgent: &memqlv1.ActingAgentIdentity{
			Id:   "v1:agents:agent:abc",
			Role: "assistant",
		},
	}

	ctx := stampActingAgentRoleIfMissing(context.Background(), msg)

	if got := memql.ActingAgentRoleFromContext(ctx); got != "assistant" {
		t.Fatalf("expected ctx acting-agent role %q, got %q", "assistant", got)
	}
	if got := memql.ActingAgentIdFromContext(ctx); got != "v1:agents:agent:abc" {
		t.Fatalf("expected ctx acting-agent id %q, got %q", "v1:agents:agent:abc", got)
	}
}

// TestStampActingAgentRoleIfMissing_DoesNotClobber asserts the normal
// cognition chat path -- where a role is already stamped on ctx at dispatch
// from a pre-filled ActingAgent -- is left untouched, even if msg carries a
// different role. The helper only fills a gap; it never overrides upstream.
func TestStampActingAgentRoleIfMissing_DoesNotClobber(t *testing.T) {
	base := memql.WithActingAgentRole(context.Background(), "specialist")
	msg := &memqlv1.AgentGenerateTurnMsg{
		ActingAgent: &memqlv1.ActingAgentIdentity{Role: "assistant"},
	}

	ctx := stampActingAgentRoleIfMissing(base, msg)

	if got := memql.ActingAgentRoleFromContext(ctx); got != "specialist" {
		t.Fatalf("expected existing role %q preserved, got %q", "specialist", got)
	}
}

// TestStampActingAgentRoleIfMissing_NilActingAgent is a no-op: a minimal-
// routing turn (or a failed self-resolve) leaves ActingAgent nil, so no role
// is stamped and nothing panics.
func TestStampActingAgentRoleIfMissing_NilActingAgent(t *testing.T) {
	ctx := stampActingAgentRoleIfMissing(
		context.Background(),
		&memqlv1.AgentGenerateTurnMsg{AgentId: "x"},
	)
	if got := memql.ActingAgentRoleFromContext(ctx); got != "" {
		t.Fatalf("expected no role for nil ActingAgent, got %q", got)
	}
}

// TestStampActingAgentRoleIfMissing_EmptyRole guards against stamping an
// empty/whitespace role (which WithActingAgentRole would no-op anyway, but
// the helper should not stamp the id either when there is no usable role).
func TestStampActingAgentRoleIfMissing_EmptyRole(t *testing.T) {
	msg := &memqlv1.AgentGenerateTurnMsg{
		ActingAgent: &memqlv1.ActingAgentIdentity{Id: "v1:agents:agent:abc", Role: "  "},
	}

	ctx := stampActingAgentRoleIfMissing(context.Background(), msg)

	if got := memql.ActingAgentRoleFromContext(ctx); got != "" {
		t.Fatalf("expected no role stamped for blank role, got %q", got)
	}
	if got := memql.ActingAgentIdFromContext(ctx); got != "" {
		t.Fatalf("expected no id stamped when role is blank, got %q", got)
	}
}

// TestStampedCtxReachesToolExecutor_BackgroundLoop is the integration guard
// for the memql#938 follow-up. The first fix stamped the role inside
// prepareTurn, whose local ctx was discarded -- the tool loop ran with the
// caller's un-stamped ctx and every workbenchHost call was still rejected on
// the planner-driven background lane. This asserts the chain the lane callers
// now rely on: a ctx stamped via stampActingAgentRoleIfMissing (as
// handleBackground / handleStreaming do right before their loops) carries the
// role all the way to the tool executor that the engine's ExecuteTool gate
// reads. A loop driven with the caller's ctx is what makes this load-bearing.
func TestStampedCtxReachesToolExecutor_BackgroundLoop(t *testing.T) {
	exec := &roleCapturingExecutor{}
	r := &Replier{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.stamper = taskstamp.New(exec, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The provider dispatches a real (non-sentinel) tool, then ends the turn
	// with a respondToUser envelope so the loop terminates.
	prov := &scriptedToolProvider{steps: []scriptStep{
		toolCallStep("workbenchHost"),
		respondToUserStep("done"),
	}}

	// Mirror exactly what handleBackground does before its tool loop: stamp
	// the role onto the ctx that is then handed to runNonStreamingToolLoop.
	msg := &memqlv1.AgentGenerateTurnMsg{
		AgentId:     "v1:agents:agent:abc",
		ActingAgent: &memqlv1.ActingAgentIdentity{Id: "v1:agents:agent:abc", Role: "assistant"},
	}
	ctx := stampActingAgentRoleIfMissing(context.Background(), msg)

	if _, err := r.runNonStreamingToolLoop(ctx, prov, nil, nil, nil,
		&captureSink{}, time.Now(), "req-938", turnContext{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(exec.rolesSeen) == 0 {
		t.Fatalf("expected workbenchHost to be dispatched to the executor, saw none")
	}
	for i, role := range exec.rolesSeen {
		if role != "assistant" {
			t.Fatalf("tool dispatch #%d saw acting-agent role %q on ctx, want %q -- "+
				"ExecuteTool's agent-only gate would reject this call", i, role, "assistant")
		}
	}
}
