package agent

import (
	"context"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

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
