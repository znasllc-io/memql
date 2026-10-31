package gamespack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// The runtime contract's own vectors for the score.
func TestRecallScoreIsTheContractFormula(t *testing.T) {
	w := defaultRecallWeights()
	cases := []struct {
		name                        string
		relevance, ageH, importance float64
		want                        float64
	}{
		{"relevant, new, important", 1, 0, 10, 1.0},
		{"irrelevant, one half-life old, importance 0", 0, 72, 0, 0.15},
		{"negative cosine counts as zero", -0.7, 0, 0, 0.3},
		{"future episode counts as new", 0, -5, 0, 0.3},
	}
	for _, c := range cases {
		if got := recallScore(c.relevance, c.ageH, c.importance, w); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: recallScore = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRecallStatementHasNoRelevanceWithoutAVectorTable(t *testing.T) {
	plain := recallStatement("")
	if strings.Contains(plain, "$10") || strings.Contains(plain, "JOIN") {
		t.Fatalf("the no-relevance statement must not bind a vector or join a table:\n%s", plain)
	}
	joined := recallStatement("node_vectors_8_0011223344556677")
	if !strings.Contains(joined, "LEFT JOIN node_vectors_8_0011223344556677 nv") || !strings.Contains(joined, "$10::vector") {
		t.Fatalf("the joined statement must LEFT JOIN the table and bind the query vector:\n%s", joined)
	}
}

// A recall with no person behind it is refused before anything is read, and one
// that does not say who is recalling, where, or what about is refused before the
// database is touched. Each refusal leads with its stable code. The provider is
// the zero value on purpose: reaching its store, its space or its database would
// panic, so a refusal here also proves the check runs first.
func TestRecallRefusesACallItCannotAnswer(t *testing.T) {
	whole := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "anything"}
	with := func(key string, value any) map[string]any {
		args := map[string]any{}
		for k, v := range whole {
			args[k] = v
		}
		if value == nil {
			delete(args, key)
		} else {
			args[key] = value
		}
		return args
	}
	recall := func(ctx context.Context, p *Provider, args map[string]any) (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("the call got past the check and reached the provider's store, space or database: %v", r)
			}
		}()
		_, err = p.recall(ctx, args, 0)
		return err
	}

	for label, ctx := range map[string]context.Context{
		"no actor in the context":         context.Background(),
		"an actor whose user id is blank": auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "   "}),
	} {
		if err := recall(ctx, &Provider{}, whole); err == nil || !strings.HasPrefix(err.Error(), "games_unauthenticated: ") {
			t.Errorf("%s: error = %v, want games_unauthenticated", label, err)
		}
	}
	owner := asOwner("user-games-recall-refusals")
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"no characterId", with("characterId", nil)},
		{"no worldId", with("worldId", nil)},
		{"no text", with("text", nil)},
		{"blank text", with("text", "   ")},
	} {
		if err := recall(owner, &Provider{}, tc.args); err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: ") {
			t.Errorf("%s: error = %v, want games_invalid_argument", tc.name, err)
		}
	}

	// The control: with everything said the call gets past both checks and stops
	// at the missing database handle, so the refusals above are the checks' doing.
	noDatabase := &Provider{db: func() *sql.DB { return nil }}
	if err := recall(owner, noDatabase, whole); err == nil || strings.HasPrefix(err.Error(), "games_") {
		t.Errorf("a whole call as an owner = %v, want it past both checks and stopped at the database handle", err)
	}
}

// Whether relevance was available is said per row, from the similarity the row
// has. With no vector table no row has one; with one, a row has one exactly when
// it has a vector there whose cosine to the query is a number, so an episode still
// waiting for its vector, or holding one that cannot be compared, does not claim
// a relevance it was not given.
func TestRecallStatementSaysRelevanceAvailablePerRow(t *testing.T) {
	plain := recallStatement("")
	if !strings.Contains(plain, "NULL::float8 AS similarity") {
		t.Fatalf("the no-relevance statement must give no row a similarity:\n%s", plain)
	}
	joined := recallStatement("node_vectors_8_0011223344556677")
	if !strings.Contains(joined, "1 - (nv.embedding <=> $10::vector) AS similarity") {
		t.Fatalf("the joined statement must give a row a similarity exactly when it has a vector:\n%s", joined)
	}
	for name, statement := range map[string]string{"no-relevance": plain, "joined": joined} {
		if !strings.Contains(statement, "AS available") || !strings.Contains(statement, "BETWEEN -1e308 AND 1e308") {
			t.Errorf("the %s statement must say per row whether a similarity that is a number was found:\n%s", name, statement)
		}
	}
}

// The similarity is read in several places (the relevance, whether it was
// available, and the relevance again in the score), and the planner inlines a
// common table expression that is used once, so left alone it would compute the
// vector distance again at each of them: eight times a row, a cost that grows with
// the width of the vectors. MATERIALIZED keeps it to one.
func TestRecallStatementComputesEachVectorDistanceOnce(t *testing.T) {
	for name, statement := range map[string]string{"no-relevance": recallStatement(""), "joined": recallStatement("node_vectors_8_0011223344556677")} {
		if !strings.Contains(statement, "scored AS MATERIALIZED (") {
			t.Errorf("the %s statement must materialize the similarity, or the planner computes the distance at every use:\n%s", name, statement)
		}
	}
}

// recallStatement puts the name of a vector table into the statement, so it takes
// the name of a vector table and nothing else, however it was reached: anything
// that is not one of the names a vector table is given gets the no-relevance
// statement, which has no table in it.
func TestRecallStatementTakesOnlyTheNameOfAVectorTable(t *testing.T) {
	plain := recallStatement("")
	for _, hostile := range notVectorTables {
		if got := recallStatement(hostile); got != plain {
			t.Errorf("a table named %q must get the no-relevance statement, got:\n%s", hostile, got)
		}
	}
	for _, table := range vectorTables {
		if got := recallStatement(table); !strings.Contains(got, "LEFT JOIN "+table+" nv ON") {
			t.Errorf("the vector table %q must be joined, got:\n%s", table, got)
		}
	}
}

// The score is a weighted sum computed in float8, so a weight or a half-life that
// no sane use needs and no float8 can take is refused before the statement runs,
// with the stable code, instead of coming back as a raw overflow from the
// database. A non-finite number and a value that is no number are refused too,
// where they used to become the default without a word. The edges themselves are
// fine, and a half-life of zero or less is still recency off.
func TestRecallRefusesTunablesTheStatementCannotTake(t *testing.T) {
	owner := asOwner("user-games-tunables-refusals")
	noDatabase := &Provider{db: func() *sql.DB { return nil }}
	recall := func(key string, value any) (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s %#v: the call reached the provider's store or space: %v", key, value, r)
			}
		}()
		args := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "anything"}
		if value != nil {
			args[key] = value
		}
		_, err = noDatabase.recall(owner, args, 0)
		return err
	}

	type tunable struct {
		key   string
		value any
	}
	var refused, accepted []tunable
	for _, key := range []string{"wRelevance", "wRecency", "wImportance"} {
		for _, v := range []any{-0.001, -1.0, 1000.001, 1001, 1e305, math.MaxFloat64, math.NaN(), math.Inf(1), math.Inf(-1), "heavy", true, []any{1.0},
			json.Number("-0.5"), json.Number("1001"), json.Number("1e999"), json.Number("NaN"), json.Number("Inf"), json.Number("-Inf"), json.Number("Infinity"), json.Number("-Infinity"), json.Number("heavy")} {
			refused = append(refused, tunable{key, v})
		}
		for _, v := range []any{nil, 0, 0.0, 0.5, 1, 999.999, 1000, 1000.0, json.Number("0"), json.Number("0.5"), json.Number("1000")} {
			accepted = append(accepted, tunable{key, v})
		}
	}
	for _, v := range []any{1e305, math.MaxFloat64, 87600.001, 87601, 1e-7, 0.0000009, math.NaN(), math.Inf(1), math.Inf(-1), "long", true,
		json.Number("87601"), json.Number("0.0000001"), json.Number("1e999"), json.Number("-1e999"), json.Number("NaN"), json.Number("Inf"), json.Number("+Inf"),
		json.Number("-Inf"), json.Number("Infinity"), json.Number("-Infinity"), json.Number("long")} {
		refused = append(refused, tunable{"halfLifeHours", v})
	}
	for _, v := range []any{nil, 0, 0.0, -72, -1e308, -math.MaxFloat64, 0.000001, 0.5, 72, 87600, 87600.0,
		json.Number("0"), json.Number("-72"), json.Number("72"), json.Number("0.000001"), json.Number("87600")} {
		accepted = append(accepted, tunable{"halfLifeHours", v})
	}

	for _, tc := range refused {
		if err := recall(tc.key, tc.value); err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: "+tc.key+" ") {
			t.Errorf("%s %#v: error = %v, want games_invalid_argument: %s ...", tc.key, tc.value, err, tc.key)
		}
	}
	for _, tc := range accepted {
		if err := recall(tc.key, tc.value); err == nil || !strings.Contains(err.Error(), "no database handle") {
			t.Errorf("%s %#v: error = %v, want it past the check and stopped at the missing database handle", tc.key, tc.value, err)
		}
	}
}

// A recall waits two seconds for the vector of its query unless a test shortens
// it on the provider.
func TestRecallWaitsTwoSecondsForItsQueryVectorByDefault(t *testing.T) {
	if recallEmbedTimeout != 2*time.Second {
		t.Fatalf("recallEmbedTimeout = %v, want 2s", recallEmbedTimeout)
	}
	if got := (&Provider{}).recallEmbedDeadline(); got != recallEmbedTimeout {
		t.Fatalf("a provider that sets no timeout waits %v, want %v", got, recallEmbedTimeout)
	}
	if got := (&Provider{embedTimeout: 150 * time.Millisecond}).recallEmbedDeadline(); got != 150*time.Millisecond {
		t.Fatalf("a provider with its own timeout waits %v, want 150ms", got)
	}
}

// asOf is the game's present moment, which every episode's age is measured from.
// One that cannot be read as a moment is refused before anything is queried,
// with the stable code, instead of quietly measuring from the server's clock; one
// that can is read as the instant it names. No asOf, or an empty one, is the
// server's clock. The provider has no database on purpose: a call that is not
// refused stops at the missing handle, so the two outcomes tell apart.
func TestRecallRefusesAnAsOfItCannotMeasureFrom(t *testing.T) {
	owner := asOwner("user-games-asof-refusals")
	noDatabase := &Provider{db: func() *sql.DB { return nil }}
	recall := func(asOf any, given bool) (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("asOf %#v: the call reached the provider's store or space: %v", asOf, r)
			}
		}()
		args := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "anything"}
		if given {
			args["asOf"] = asOf
		}
		_, err = noDatabase.recall(owner, args, 0)
		return err
	}

	for _, tc := range []struct {
		name string
		asOf any
	}{
		{"words", "not a time"},
		{"a space where the T belongs", "2026-10-18 12:00:00"},
		{"a date with no time", "2026-10-18"},
		{"a number", 12345},
		{"a number with a fraction", 12345.5},
		{"a boolean", true},
		{"year 0000", "0000-01-01T00:00:00Z"},
		{"year 0000 once moved to UTC", "0001-01-01T00:00:00+05:30"},
		{"year 10000 once moved to UTC", "9999-12-31T23:59:59-23:59"},
		{"a year of five digits", "10000-01-01T00:00:00Z"},
	} {
		if err := recall(tc.asOf, true); err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: asOf ") {
			t.Errorf("asOf as %s (%#v): error = %v, want games_invalid_argument: asOf ...", tc.name, tc.asOf, err)
		}
	}

	for _, tc := range []struct {
		name  string
		asOf  any
		given bool
	}{
		{"absent", nil, false},
		{"null", nil, true},
		{"empty", "", true},
		{"blank", "   ", true},
		{"a zero time.Time", time.Time{}, true},
		{"on the second", "2026-10-18T12:00:00Z", true},
		{"the stored form", "2026-10-18T12:00:00.000Z", true},
		{"a trimmed fraction", "2026-10-18T12:00:00.5Z", true},
		{"an offset", "2026-10-18T10:00:00+05:30", true},
		{"padded with spaces", " 2026-10-18T12:00:00Z ", true},
		{"the first instant of year 1", "0001-01-01T00:00:00Z", true},
		{"the last instant of year 9999", "9999-12-31T23:59:59.999Z", true},
		{"a time.Time", time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), true},
	} {
		if err := recall(tc.asOf, tc.given); err == nil || !strings.Contains(err.Error(), "no database handle") {
			t.Errorf("asOf as %s (%#v): error = %v, want it past the asOf check and stopped at the missing database handle", tc.name, tc.asOf, err)
		}
	}
}

// panickingSpace is an embedder with a bug in it.
type panickingSpace struct{ testSpace }

func (panickingSpace) embedQuery(context.Context, string) ([]float32, error) {
	panic("the embedder has a bug")
}

// The vector of a query is waited for at most the provider's bound, whether or
// not the embedder watches the context it is given, and a recall abandoned by its
// caller stops waiting too. The embedding runs on a goroutine of its own so that
// the wait can be bounded, which puts it out of the reach of any recover above
// the handler: a panic in it must come back as an error, not end the node.
func TestRecallEmbedsItsQueryWithinABound(t *testing.T) {
	const bound = 100 * time.Millisecond
	owner := asOwner("user-games-embed-bound")
	p := &Provider{space: testSpace{}, embedTimeout: bound}

	if vec, err := p.embedRecallQuery(owner, "a cave"); err != nil || len(vec) != testDims {
		t.Fatalf("an embedder that answers: vector of %d, error %v, want a vector of %d and no error", len(vec), err, testDims)
	}
	p.space = failingSpace{testSpace{}}
	if _, err := p.embedRecallQuery(owner, "a cave"); !errors.Is(err, errModelAway) {
		t.Fatalf("an embedder that fails: error = %v, want its own error back", err)
	}

	for _, ignoresContext := range []bool{false, true} {
		p.space = slowSpace{testSpace: testSpace{}, ignoresContext: ignoresContext}
		start := time.Now()
		_, err := p.embedRecallQuery(owner, "a cave")
		if elapsed := time.Since(start); elapsed > 5*bound {
			t.Errorf("an embedder that ignores its context (%v) held the recall for %v, want about %v", ignoresContext, elapsed, bound)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a slow embedder (ignores its context: %v): error = %v, want the deadline", ignoresContext, err)
		}
	}

	caller, cancel := context.WithCancel(owner)
	time.AfterFunc(bound/4, cancel)
	p.space, p.embedTimeout = slowSpace{testSpace: testSpace{}}, time.Minute
	start := time.Now()
	if _, err := p.embedRecallQuery(caller, "a cave"); !errors.Is(err, context.Canceled) || time.Since(start) > 5*bound {
		t.Errorf("a caller that gave up: error = %v after %v, want context.Canceled at once", err, time.Since(start))
	}

	p.space, p.embedTimeout = panickingSpace{}, bound
	_, err := p.embedRecallQuery(owner, "a cave")
	var panicked *embedPanic
	if !errors.As(err, &panicked) || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("an embedder that panics: error = %v, want it reported as an error that carries the panic", err)
	}
	if !strings.Contains(string(panicked.stack), "panickingSpace") {
		t.Errorf("the stack of the panic must show where it came from, got:\n%s", panicked.stack)
	}
}
