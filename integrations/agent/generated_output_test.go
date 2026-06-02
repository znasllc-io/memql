package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestDeriveGeneratedOutputId_Deterministic(t *testing.T) {
	a := deriveGeneratedOutputId("agent_generated", "user-1", "space-1:Title")
	b := deriveGeneratedOutputId("agent_generated", "user-1", "space-1:Title")
	if a != b {
		t.Fatalf("same inputs must yield same id: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "genout-") || len(a) != len("genout-")+16 {
		t.Fatalf("malformed id %q", a)
	}
	if deriveGeneratedOutputId("agent_generated", "user-2", "space-1:Title") == a {
		t.Errorf("different owner must yield distinct id")
	}
	if deriveGeneratedOutputId("agent_generated", "user-1", "space-1:Other") == a {
		t.Errorf("different stableKey must yield distinct id")
	}
}

func TestStringField(t *testing.T) {
	m := map[string]any{"title": "Hi", "n": 3}
	if got := stringField(m, "title"); got != "Hi" {
		t.Errorf("stringField title = %q, want Hi", got)
	}
	if got := stringField(m, "n"); got != "" {
		t.Errorf("non-string field should yield empty, got %q", got)
	}
	if got := stringField(m, "missing"); got != "" {
		t.Errorf("missing field should yield empty, got %q", got)
	}
	if got := stringField(nil, "x"); got != "" {
		t.Errorf("nil map should yield empty, got %q", got)
	}
}

// captureEngine is a minimal MemQLEngine that records Execute queries.
// Only Execute is exercised by promoteCanvasOutput; the rest of the
// interface is a nil embed.
type captureEngine struct {
	MemQLEngine
	queries []string
}

func (c *captureEngine) Execute(_ context.Context, q string) (any, error) {
	c.queries = append(c.queries, q)
	return nil, nil
}

func newTestReplier(e MemQLEngine) *Replier {
	return &Replier{engine: e, logger: slog.New(slog.NewTextHandler(discard{}, nil))}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestPromoteCanvasOutput_SkipsEmptyCard(t *testing.T) {
	ce := &captureEngine{}
	r := newTestReplier(ce)
	tc := turnContext{OwnerUserId: "user-1", AgentId: "agent-1", SpaceId: "space-1"}

	// No data at all.
	r.promoteCanvasOutput(context.Background(), tc, map[string]any{})
	// data present but empty body (source).
	r.promoteCanvasOutput(context.Background(), tc, map[string]any{
		"data": map[string]any{"title": "Heads up"},
	})
	// body present but whitespace only.
	r.promoteCanvasOutput(context.Background(), tc, map[string]any{
		"data": map[string]any{"title": "Heads up", "source": "   "},
	})

	if len(ce.queries) != 0 {
		t.Fatalf("empty/ambient cards must not promote, got %v", ce.queries)
	}
}

func TestPromoteCanvasOutput_SkipsNoOwner(t *testing.T) {
	ce := &captureEngine{}
	r := newTestReplier(ce)
	tc := turnContext{SpaceId: "space-1"} // no OwnerUserId
	r.promoteCanvasOutput(context.Background(), tc, map[string]any{
		"data": map[string]any{"title": "T", "source": "real body"},
	})
	if len(ce.queries) != 0 {
		t.Fatalf("missing owner must skip promotion, got %v", ce.queries)
	}
}

func TestPromoteCanvasOutput_PromotesRealCard(t *testing.T) {
	ce := &captureEngine{}
	r := newTestReplier(ce)
	tc := turnContext{OwnerUserId: "user-1", AgentId: "agent-1", SpaceId: "space-1", PlanId: "plan-1"}
	r.promoteCanvasOutput(context.Background(), tc, map[string]any{
		"data": map[string]any{"title": "Report", "source": "# Heading\nbody"},
	})
	if len(ce.queries) != 1 {
		t.Fatalf("expected one insert, got %d: %v", len(ce.queries), ce.queries)
	}
	q := ce.queries[0]
	for _, want := range []string{
		"mutationCreateGeneratedOutput(",
		`source:"agent_generated"`,
		`ownerUserId:"user-1"`,
		`title:"Report"`,
		`producedByAgentId:"agent-1"`,
		`producedByPlanId:"plan-1"`,
		`spaceId:"space-1"`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("insert query missing %q\n  got: %s", want, q)
		}
	}
}

func TestPromoteCanvasOutput_NilEngineNoPanic(t *testing.T) {
	r := &Replier{logger: slog.New(slog.NewTextHandler(discard{}, nil))}
	r.promoteCanvasOutput(context.Background(), turnContext{OwnerUserId: "u"}, map[string]any{
		"data": map[string]any{"source": "body"},
	})
}
