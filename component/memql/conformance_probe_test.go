package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// The probe answers with the executor's own lowering and post-filter: the SQL
// is the pushdown fragment a read uses, the in-process decision agrees with it
// on a row that matches and on one that does not, a plan constant over args is
// folded before the SQL is compiled, and what Lower refuses is refused.
func TestConformanceProbeLowersAndEvaluatesLikeTheExecutor(t *testing.T) {
	registry := loadedConceptRegistry(t)
	eng := newQuietEngine(t)
	if err := eng.Init(registry); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if eng.LoadReport() == nil {
		t.Fatal("LoadReport() is nil after Init")
	}
	ctx := context.Background()
	const user = "v1:identity:user"
	lambda := func(src string) *languageParser.LambdaExpr {
		lam, err := languageParser.ParseV1Lambda(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		return lam
	}

	probe, err := eng.ProbeLower(ctx, tiers.PositionQueryFilter, user, lambda(`row => row.primaryEmail == "a@example.test" || row.role == "admin"`), nil, time.Time{})
	if err != nil {
		t.Fatalf("ProbeLower: %v", err)
	}
	for _, want := range []string{"payload #>> '{primaryEmail}'", " OR ", "payload #>> '{role}'"} {
		if !strings.Contains(probe.SQL, want) {
			t.Errorf("lowered SQL %q does not contain %q", probe.SQL, want)
		}
	}
	for _, c := range []struct {
		row  map[string]any
		want bool
	}{
		{map[string]any{"primaryEmail": "a@example.test", "role": "writer"}, true},
		{map[string]any{"primaryEmail": "b@example.test", "role": "admin"}, true},
		{map[string]any{"primaryEmail": "b@example.test", "role": "writer"}, false},
	} {
		got, err := probe.Matches(c.row)
		if err != nil {
			t.Fatalf("Matches(%v): %v", c.row, err)
		}
		if got != c.want {
			t.Errorf("Matches(%v) = %v, want %v", c.row, got, c.want)
		}
	}

	// The optional-argument guard folds: with the argument left out it is
	// TRUE for every row, with it given it is the one comparison.
	guard := lambda(`row => args.role == nil || row.role == args.role`)
	given, err := eng.ProbeLower(ctx, tiers.PositionQueryFilter, user, guard, map[string]any{"role": "admin"}, time.Time{})
	if err != nil {
		t.Fatalf("ProbeLower with the argument: %v", err)
	}
	if strings.Contains(given.SQL, " OR ") || !strings.Contains(given.SQL, "payload #>> '{role}'") {
		t.Errorf("the guard with its argument lowered to %q, not the one comparison", given.SQL)
	}
	omitted, err := eng.ProbeLower(ctx, tiers.PositionQueryFilter, user, guard, nil, time.Time{})
	if err != nil {
		t.Fatalf("ProbeLower without the argument: %v", err)
	}
	if got, err := omitted.Matches(map[string]any{"role": "writer"}); err != nil || !got {
		t.Errorf("the guard without its argument = %v, %v; want true for every row", got, err)
	}

	_, err = eng.ProbeLower(ctx, tiers.PositionQueryFilter, user, lambda(`row => lower(row.primaryEmail) == "a"`), nil, time.Time{})
	var le *LowerError
	if !errors.As(err, &le) {
		t.Errorf("an in-process function over the row lowered, or failed as %v rather than a LowerError", err)
	}
}
