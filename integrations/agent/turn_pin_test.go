package agent

import (
	"context"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// A reply turn's pin carries WHO MADE IT (ResolveRequest.PinnedBy), because
// the app gate admits a pinned app door only when the pin is the session
// owner's own: a pin skips every routing rule, so nothing else says the owner
// chose the app.

// agentOwnerEngine answers the agentOwner query with one owner.
type agentOwnerEngine struct {
	MemQLEngine
	owner   string
	queries []string
}

func (e *agentOwnerEngine) Execute(_ context.Context, q string) (any, error) {
	e.queries = append(e.queries, q)
	return map[string]any{"data": []any{map[string]any{"ownerUserId": e.owner}}}, nil
}

// agentPinData is prompt data whose agent record stores provider as its pin.
func agentPinData(provider string) map[string]any {
	return map[string]any{"assistant": map[string]any{
		"providerConfig": map[string]any{"llm": map[string]any{"provider": provider}},
	}}
}

// An agent's stored pin is its OWNER's choice. Here the agent is user-2's and
// the turn is user-1's: the pin must say user-2 made it, so the app gate can
// refuse to open user-1's machine for it.
func TestAnAgentsStoredPinIsItsOwners(t *testing.T) {
	engine := &agentOwnerEngine{owner: "v1:identity:user:user-2"}
	r := newTestReplier(engine)
	ctx := auth.ContextWithUserActor(context.Background(), "v1:identity:user:user-1")
	msg := &memqlv1.AgentGenerateTurnMsg{AgentId: "v1:agents:agent:user-2s-agent"}

	provider, pinnedBy := r.turnPin(ctx, msg, agentPinData("app:claude-code"))
	if provider != "app:claude-code" || pinnedBy != "v1:identity:user:user-2" {
		t.Fatalf("pin = %q by %q, want app:claude-code by the agent's owner", provider, pinnedBy)
	}

	// The stored MODEL is promoted to the pin when no provider is stored, and
	// it is the owner's choice all the same.
	data := agentPinData("")
	data["assistant"].(map[string]any)["providerConfig"].(map[string]any)["llm"] = map[string]any{"model": "app:claude-code"}
	provider, pinnedBy = r.turnPin(ctx, msg, data)
	if provider != "app:claude-code" || pinnedBy != "v1:identity:user:user-2" {
		t.Fatalf("model pin = %q by %q, want app:claude-code by the agent's owner", provider, pinnedBy)
	}

	// No pin reads nothing and names nobody.
	engine.queries = nil
	if provider, pinnedBy := r.turnPin(ctx, msg, map[string]any{}); provider != "" || pinnedBy != "" || len(engine.queries) != 0 {
		t.Fatalf("an unpinned turn pinned %q by %q after %d read(s)", provider, pinnedBy, len(engine.queries))
	}
}

// A per-turn hint is the CALLER's own pin, and an unpinned turn names nobody.
func TestATurnsHintedPinIsTheCallers(t *testing.T) {
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{"composeFile": true}}, prompts: registry}
	r := newTestReplier(engine)
	caller := "v1:identity:user:hint-caller"
	turn := func(hints map[string]string, ov *common.StepOverride) (*preparedTurn, error) {
		ctx := auth.ContextWithUserActor(context.Background(), caller)
		ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", StepKey: "draft", OwnerUserId: caller, Override: ov})
		msg := &memqlv1.AgentGenerateTurnMsg{
			AgentId:     "assistant",
			ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Name: "Ada", Role: "assistant"},
			History:     []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Draft the report"}},
			Hints:       hints,
		}
		return r.prepareTurn(ctx, msg, time.Now())
	}

	prepared, err := turn(map[string]string{"provider": "app:claude-code"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.routerReq; got.ExplicitProvider != "app:claude-code" || got.PinnedBy != caller {
		t.Fatalf("hinted turn pins %q by %q, want app:claude-code by the caller", got.ExplicitProvider, got.PinnedBy)
	}

	// A person's step override names the person who asked for the re-run.
	prepared, err = turn(map[string]string{"provider": "app:claude-code"}, &common.StepOverride{Model: "app:codex", RequestedBy: "v1:identity:user:rerunner"})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.routerReq; got.ExplicitProvider != "app:codex" || got.PinnedBy != "v1:identity:user:rerunner" {
		t.Fatalf("overridden turn pins %q by %q, want app:codex by the person who asked", got.ExplicitProvider, got.PinnedBy)
	}

	prepared, err = turn(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.routerReq; got.ExplicitProvider != "" || strings.TrimSpace(got.PinnedBy) != "" {
		t.Fatalf("an unpinned turn pins %q by %q, want nothing by nobody", got.ExplicitProvider, got.PinnedBy)
	}
}
