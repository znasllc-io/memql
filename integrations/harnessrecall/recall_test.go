package harnessrecall

import (
	"context"
	"math"
	"sort"
	"strings"
	"testing"
)

// TestRecallScoreFormula pins the exact scoring formula:
//
//	score = wSem*cosine + wRec*exp(-ln2*age/halfLife)
//
// At age == halfLife the recency term is exactly 0.5; at age 0 it is 1.
func TestRecallScoreFormula(t *testing.T) {
	const halfLife = 3600.0

	// Recency term at age 0 == 1.0.
	if got := recallScore(0, 0, halfLife, 0, 1); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("recency at age 0 = %v, want 1.0", got)
	}
	// Recency term at age == halfLife == 0.5.
	if got := recallScore(0, halfLife, halfLife, 0, 1); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("recency at age==halfLife = %v, want 0.5", got)
	}
	// Recency term at age == 2*halfLife == 0.25.
	if got := recallScore(0, 2*halfLife, halfLife, 0, 1); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("recency at age==2*halfLife = %v, want 0.25", got)
	}
	// Pure semantic: recency weight 0 -> score == wSem*cosine.
	if got := recallScore(0.8, 1234, halfLife, 0.5, 0); math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("pure semantic score = %v, want 0.4", got)
	}
	// Weighted sum.
	want := 0.7*0.9 + 0.3*math.Exp(-ln2*1800/halfLife)
	if got := recallScore(0.9, 1800, halfLife, 0.7, 0.3); math.Abs(got-want) > 1e-9 {
		t.Fatalf("weighted score = %v, want %v", got, want)
	}
	// Non-positive halfLife disables the recency term (no div-by-zero).
	if got := recallScore(0.5, 1000, 0, 1, 1); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("recency with halfLife<=0 = %v, want 0.5 (recency disabled)", got)
	}
	// Negative age clamps to 0 (clock skew safety).
	if got := recallScore(0, -500, halfLife, 0, 1); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("recency at negative age = %v, want 1.0", got)
	}
}

// TestHalfLifeReordersResults is the acceptance-criteria test: changing
// half-life measurably reorders results. Two candidate memories:
//   - A: highly relevant (cosine 0.95) but old (age 2h)
//   - B: less relevant (cosine 0.70) but fresh (age 5min)
//
// With a long half-life (recency barely decays), A's relevance wins.
// With a short half-life (recency decays fast), B's freshness wins.
func TestHalfLifeReordersResults(t *testing.T) {
	type cand struct {
		name   string
		cosine float64
		age    float64
	}
	a := cand{"A_relevant_old", 0.80, 2 * 3600}
	b := cand{"B_fresh_lessrelevant", 0.72, 60}

	const wSem, wRec = 0.5, 0.5

	rank := func(halfLife float64) []string {
		cs := []cand{a, b}
		sort.SliceStable(cs, func(i, j int) bool {
			si := recallScore(cs[i].cosine, cs[i].age, halfLife, wSem, wRec)
			sj := recallScore(cs[j].cosine, cs[j].age, halfLife, wSem, wRec)
			return si > sj
		})
		return []string{cs[0].name, cs[1].name}
	}

	// Long half-life (1 day): recency of both is near-1, relevance dominates -> A first.
	longHL := rank(24 * 3600)
	if longHL[0] != a.name {
		t.Fatalf("long half-life ranking = %v, want %s first (relevance dominates)", longHL, a.name)
	}

	// Short half-life (2 min): A's 2h age decays its recency to ~0, B stays fresh -> B first.
	shortHL := rank(120)
	if shortHL[0] != b.name {
		t.Fatalf("short half-life ranking = %v, want %s first (recency dominates)", shortHL, b.name)
	}

	if longHL[0] == shortHL[0] {
		t.Fatalf("changing half-life did not reorder results: long=%v short=%v", longHL, shortHL)
	}
}

// TestRecallSQLIsSingleStatement guards the "one query, no app-side
// merge" acceptance criterion against the literal SQL the handler
// issues. Exactly one statement (one terminating semicolon's worth --
// we forbid an embedded `;`), one table scan of MemoryNodes, one
// node_vectors join, the score in ORDER BY, and a LIMIT.
func TestRecallSQLIsSingleStatement(t *testing.T) {
	sql := recallSQL

	// No statement separator inside the query body -> single statement.
	if strings.Contains(sql, ";") {
		t.Fatalf("recallSQL contains a ';' -- must be a single statement, got:\n%s", sql)
	}
	// Exactly one FROM "MemoryNodes" (the latest-version CTE scan).
	if got := strings.Count(sql, `FROM "MemoryNodes"`); got != 1 {
		t.Fatalf(`recallSQL scans "MemoryNodes" %d times, want exactly 1`, got)
	}
	// Exactly one node_vectors join (the vector side).
	if got := strings.Count(sql, "JOIN node_vectors"); got != 1 {
		t.Fatalf("recallSQL joins node_vectors %d times, want exactly 1", got)
	}
	// Single SELECT producing the result set (the CTE uses SELECT
	// DISTINCT ON too; allow exactly the two we author: CTE + outer).
	if got := strings.Count(sql, "SELECT"); got != 2 {
		t.Fatalf("recallSQL has %d SELECTs, want 2 (latest CTE + outer scored select)", got)
	}
	// The score must drive the ordering, and there must be a LIMIT.
	if !strings.Contains(sql, "ORDER BY score DESC") {
		t.Fatalf("recallSQL must ORDER BY score DESC (server-side ranking)")
	}
	if !strings.Contains(sql, "LIMIT $8") {
		t.Fatalf("recallSQL must LIMIT $8 (top-k server-side)")
	}
	// The time-window predicate must live in the CTE (so the
	// hypertable can prune chunks before the vector join).
	if !strings.Contains(sql, `"createdAt" >= now() - make_interval`) {
		t.Fatalf("recallSQL must filter createdAt by window in the CTE for hypertable pruning")
	}
	// Owner-scope predicate (partition isolation) must be present.
	if !strings.Contains(sql, "payload->>'ownerUserId'") {
		t.Fatalf("recallSQL must scope by payload->>'ownerUserId' (partition isolation)")
	}
}

// TestRecallSQLArgsBinding verifies the resolved params bind in the
// exact positional order recallSQL expects ($1..$8), and that a
// non-positive half-life is guarded against division by zero.
func TestRecallSQLArgsBinding(t *testing.T) {
	p := recallParams{
		concept:       "v1:harness:observation",
		ownerUserId:   "v1:identity:user:alice",
		k:             7,
		windowSeconds: 86400,
		halfLife:      3600,
		wSem:          0.7,
		wRec:          0.3,
	}
	args := recallSQLArgs(p, "[0.1,0.2]")
	if len(args) != 8 {
		t.Fatalf("recallSQLArgs returned %d args, want 8", len(args))
	}
	if args[0] != "[0.1,0.2]" {
		t.Fatalf("$1 = %v, want vector literal", args[0])
	}
	if args[1] != "v1:harness:observation" {
		t.Fatalf("$2 = %v, want concept", args[1])
	}
	if args[2] != "v1:identity:user:alice" {
		t.Fatalf("$3 = %v, want owner id", args[2])
	}
	if args[3] != 86400.0 {
		t.Fatalf("$4 = %v, want window seconds", args[3])
	}
	if args[4] != 3600.0 {
		t.Fatalf("$5 = %v, want half-life", args[4])
	}
	if args[5] != 0.7 || args[6] != 0.3 {
		t.Fatalf("$6/$7 = %v/%v, want weights 0.7/0.3", args[5], args[6])
	}
	if args[7] != 7 {
		t.Fatalf("$8 = %v, want k", args[7])
	}

	// Non-positive half-life is replaced with a huge value so the SQL
	// divisor is never zero.
	pZero := p
	pZero.halfLife = 0
	zeroArgs := recallSQLArgs(pZero, "[0]")
	if hl, ok := zeroArgs[4].(float64); !ok || hl <= p.halfLife {
		t.Fatalf("$5 with halfLife<=0 = %v, want a large positive guard value", zeroArgs[4])
	}
}

// TestResolveParamsDefaults verifies defaulting + owner-scope
// resolution. With no auth context, ownerUserId is empty (dev-mode
// unscoped read); required text is enforced.
func TestResolveParamsDefaults(t *testing.T) {
	i := New(nil)

	if _, err := i.resolveParams(context.Background(), map[string]any{}, 0); err == nil {
		t.Fatalf("resolveParams with no text should error")
	}

	p, err := i.resolveParams(context.Background(), map[string]any{"text": "hello"}, 0)
	if err != nil {
		t.Fatalf("resolveParams: %v", err)
	}
	if p.concept != defaultConcept {
		t.Fatalf("default concept = %q, want %q", p.concept, defaultConcept)
	}
	if p.provider != defaultProvider {
		t.Fatalf("default provider = %q, want %q", p.provider, defaultProvider)
	}
	if p.k != defaultK {
		t.Fatalf("default k = %d, want %d", p.k, defaultK)
	}
	if p.halfLife != defaultHalfLifeSeconds {
		t.Fatalf("default halfLife = %v, want %v", p.halfLife, defaultHalfLifeSeconds)
	}
	if p.wSem != defaultWSem || p.wRec != defaultWRec {
		t.Fatalf("default weights = %v/%v, want %v/%v", p.wSem, p.wRec, defaultWSem, defaultWRec)
	}
	if p.windowSeconds != defaultWindowSeconds {
		t.Fatalf("default window = %v, want %v (all history)", p.windowSeconds, defaultWindowSeconds)
	}

	// target param supplies k when caller omits it.
	pt, _ := i.resolveParams(context.Background(), map[string]any{"text": "x"}, 25)
	if pt.k != 25 {
		t.Fatalf("k from target = %d, want 25", pt.k)
	}

	// Explicit overrides win; numeric args accept int64 (DSL parser) +
	// float64 (JSON) forms.
	po, _ := i.resolveParams(context.Background(), map[string]any{
		"text":     "x",
		"concept":  "v1:cognition:utterance",
		"k":        int64(3),
		"window":   float64(600),
		"halfLife": int64(120),
		"wSem":     0.2,
		"wRec":     0.8,
		"provider": "embedding3Large",
	}, 0)
	if po.concept != "v1:cognition:utterance" || po.k != 3 || po.windowSeconds != 600 ||
		po.halfLife != 120 || po.wSem != 0.2 || po.wRec != 0.8 || po.provider != "embedding3Large" {
		t.Fatalf("override resolution wrong: %+v", po)
	}
}
