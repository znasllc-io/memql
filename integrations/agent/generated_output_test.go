package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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
//
// It records the ACTOR alongside each query: since memql#2989
// createGeneratedOutput stamps `ownerUserId: actor.userId` instead of
// taking it as an argument, the owner is no longer visible in the query
// string, so asserting on the string alone would no longer prove
// anything about ownership.
type captureEngine struct {
	MemQLEngine
	queries []string
	actors  []string
}

func (c *captureEngine) Execute(ctx context.Context, q string) (any, error) {
	c.queries = append(c.queries, q)
	c.actors = append(c.actors, actorUserId(ctx))
	return nil, nil
}

// actorUserId reads the acting user off the context the way the engine
// resolves `actor.userId` -- from the claims withUserActor stamps.
func actorUserId(ctx context.Context) string {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return ""
	}
	sub, _ := claims["sub"].(string)
	return sub
}

func newTestReplier(e MemQLEngine) *Replier {
	return &Replier{engine: e, logger: slog.New(slog.NewTextHandler(discard{}, nil))}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestPromoteCanvasOutput_SkipsEmptyCard(t *testing.T) {
	ce := &captureEngine{}
	r := newTestReplier(ce)
	tc := turnContext{OwnerUserId: "user-1", AgentId: "agent-1", PartitionId: "space-1"}

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
	tc := turnContext{PartitionId: "space-1"} // no OwnerUserId
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
	tc := turnContext{OwnerUserId: "user-1", AgentId: "agent-1", PartitionId: "space-1", PlanId: "plan-1"}
	r.promoteCanvasOutput(context.Background(), tc, map[string]any{
		"data": map[string]any{"title": "Report", "source": "# Heading\nbody"},
	})
	if len(ce.queries) != 1 {
		t.Fatalf("expected one insert, got %d: %v", len(ce.queries), ce.queries)
	}
	// The owner arrives on the ACTOR, not in the query (memql#2989).
	if ce.actors[0] != "user-1" {
		t.Errorf("promotion ran under actor %q, want user-1 -- createGeneratedOutput stamps "+
			"ownerUserId from actor.userId, so the wrong actor silently misattributes the row",
			ce.actors[0])
	}
	q := ce.queries[0]
	if strings.Contains(q, "ownerUserId:") {
		t.Errorf("query still passes ownerUserId as an argument; it is stamped from actor.userId "+
			"(memql#2989)\n  got: %s", q)
	}
	for _, want := range []string{
		"createGeneratedOutput(",
		`source:"agent_generated"`,
		`title:"Report"`,
		`producedByAgentId:"agent-1"`,
		`producedByPlanId:"plan-1"`,
		`partitionId:"space-1"`,
		// memql#1207: the summary is derived from the markdown heading.
		`summary:"Heading"`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("insert query missing %q\n  got: %s", want, q)
		}
	}
}

// TestPromoteCanvasOutput_EmitsDerivedSummary verifies the promotion path
// stamps a derived summary (memql#1207) under each derivation branch.
func TestPromoteCanvasOutput_EmitsDerivedSummary(t *testing.T) {
	tc := turnContext{OwnerUserId: "user-1", PartitionId: "space-1"}
	cases := []struct {
		name string
		data map[string]any
		want string
	}{
		{
			name: "explicit summary wins over body",
			data: map[string]any{"title": "T", "source": "# Heading\nbody", "summary": "Explicit intent line"},
			want: `summary:"Explicit intent line"`,
		},
		{
			name: "intent used when no summary",
			data: map[string]any{"title": "T", "source": "# Heading\nbody", "intent": "Make a budget"},
			want: `summary:"Make a budget"`,
		},
		{
			name: "first heading when no explicit",
			data: map[string]any{"title": "T", "source": "## Quarterly Report\nlots of text"},
			want: `summary:"Quarterly Report"`,
		},
		{
			name: "first sentence when no heading",
			data: map[string]any{"title": "T", "source": "This is the first sentence. And a second one."},
			want: `summary:"This is the first sentence."`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ce := &captureEngine{}
			r := newTestReplier(ce)
			r.promoteCanvasOutput(context.Background(), tc, map[string]any{"data": c.data})
			if len(ce.queries) != 1 {
				t.Fatalf("expected one insert, got %d: %v", len(ce.queries), ce.queries)
			}
			if !strings.Contains(ce.queries[0], c.want) {
				t.Errorf("insert query missing %q\n  got: %s", c.want, ce.queries[0])
			}
		})
	}
}

// TestDeriveOutputSummary covers the pure derivation helper directly,
// including truncation + whitespace collapse (memql#1207).
func TestDeriveOutputSummary(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		body string
		want string
	}{
		{"explicit summary", map[string]any{"summary": "  intent line  "}, "# H\nbody", "intent line"},
		{"intent fallback", map[string]any{"intent": "do the thing"}, "# H\nbody", "do the thing"},
		{"markdown heading", nil, "# Project Plan\nbody text", "Project Plan"},
		{"heading strips multiple hashes", nil, "### Deep Heading", "Deep Heading"},
		{"first sentence", nil, "Hello world. Next.", "Hello world."},
		{"question terminator", nil, "What now? More.", "What now?"},
		{"no terminator -> first line", nil, "just a phrase with no period\nsecond line", "just a phrase with no period"},
		{"collapses newlines in explicit", map[string]any{"summary": "a\n\nb   c"}, "", "a b c"},
		{"empty body and data", nil, "   ", ""},
		{"skips blank lines before heading", nil, "\n\n# Real Heading\nbody", "Real Heading"},
		{"non-heading first line falls to sentence", nil, "Intro line. Body.", "Intro line."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveOutputSummary(c.data, c.body); got != c.want {
				t.Errorf("deriveOutputSummary = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTruncateSummary verifies the ~200-char cap with an ellipsis,
// operating on runes (memql#1207).
func TestTruncateSummary(t *testing.T) {
	long := strings.Repeat("a", summaryMaxLen+50)
	got := truncateSummary(long)
	gotRunes := []rune(got)
	if gotRunes[len(gotRunes)-1] != '…' {
		t.Errorf("truncated summary should end with ellipsis, got %q", got)
	}
	if n := len(gotRunes); n != summaryMaxLen+1 {
		t.Errorf("truncated rune length = %d, want %d", n, summaryMaxLen+1)
	}
	short := "fits fine"
	if got := truncateSummary(short); got != short {
		t.Errorf("short string should pass through, got %q", got)
	}
}

func TestPromoteCanvasOutput_NilEngineNoPanic(t *testing.T) {
	r := &Replier{logger: slog.New(slog.NewTextHandler(discard{}, nil))}
	r.promoteCanvasOutput(context.Background(), turnContext{OwnerUserId: "u"}, map[string]any{
		"data": map[string]any{"source": "body"},
	})
}
