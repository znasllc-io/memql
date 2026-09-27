package memql

// tool_caller_context_test.go -- who a ListToolsMsg / CallToolMsg speaks for
// (memql#5438). CallToolMsg runs on the agent node: the agent kind arrives
// threaded in the envelope's agent_role metadata, and the person as the
// verified forwarded authority the mesh bound onto the stream's context. The
// tool's rank floor must be judged against THAT person -- never against a
// re-resolution of the claims beside it, which can disagree (a role ceiling).

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/component"
)

func toolCallerTestSession() *streamSession {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return &streamSession{service: &service{logger: logger}, logger: logger}
}

func agentRoleEnvelope(role string) *memqlv1.MemqlClientMessage {
	return &memqlv1.MemqlClientMessage{Metadata: map[string]string{"agent_role": role}}
}

func TestToolCallerContextKeepsTheForwardedPerson(t *testing.T) {
	eng, err := memqlengine.New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	floor := &memqlengine.Tool{Name: "validateThings", RequiresRank: "developer"}

	// The forwarded authority proved a WRITER; the claims beside it say
	// owner. The floor is judged against the proved one.
	forwarded := auth.ContextWithClaims(context.Background(), map[string]any{"sub": "v1:identity:user:u1", "role": "owner"})
	forwarded = auth.ContextWithAccess(forwarded, &auth.AccessContext{UserId: "v1:identity:user:u1", Role: auth.RoleWriter})
	ctx := toolCallerTestSession().toolCallerContext(forwarded, agentRoleEnvelope("assistant"))

	if caller := memqlengine.ToolCallerFromContext(ctx); caller.Kind != memqlengine.ToolCallerAgent || caller.AgentRole != "assistant" {
		t.Fatalf("the envelope's agent_role did not make the call an agent's: %+v", caller)
	}
	if ac, _ := auth.AccessFromContext(ctx); ac == nil || ac.Role != auth.RoleWriter {
		t.Fatalf("the forwarded person was replaced: %+v", ac)
	}
	if refusal := eng.ToolCallRefusal(ctx, floor); refusal == nil || !strings.Contains(refusal.Error(), `requires the "developer" role or above`) {
		t.Fatalf("a writer's forwarded call cleared a developer floor: %v", refusal)
	}
}

func TestToolCallerContextResolvesADirectStreamsPerson(t *testing.T) {
	// A direct stream carries only claims; the session resolves them (with no
	// identity resolver wired, from the claims themselves).
	direct := auth.ContextWithClaims(context.Background(), map[string]any{"sub": "v1:identity:user:u2", "role": "developer"})
	ctx := toolCallerTestSession().toolCallerContext(direct, &memqlv1.MemqlClientMessage{})

	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.UserId != "v1:identity:user:u2" || ac.Role != auth.RoleDeveloper {
		t.Fatalf("a direct stream's person = %+v, want the claims' developer", ac)
	}
	if caller := memqlengine.ToolCallerFromContext(ctx); caller.Kind != memqlengine.ToolCallerNone {
		t.Fatalf("an envelope with no agent_role made the call %+v; it names no caller kind", caller)
	}
}
