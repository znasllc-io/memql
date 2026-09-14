package memql

import (
	"context"
	"strings"
	"testing"
)

// The probe answers with the executor's own lowering and post-filter: the SQL
// is the pushdown fragment a read uses, and the in-process decision agrees
// with it on a row that matches and on one that does not.
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
	filter := `primaryEmail=="a@example.test" || role=="admin"`

	sql, err := eng.ProbeLowerFilter(ctx, user, filter)
	if err != nil {
		t.Fatalf("ProbeLowerFilter: %v", err)
	}
	for _, want := range []string{"payload #>> '{primaryEmail}' = ?", " OR ", "payload #>> '{role}' = ?"} {
		if !strings.Contains(sql, want) {
			t.Errorf("lowered SQL %q does not contain %q", sql, want)
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
		got, err := eng.ProbeEvaluateFilter(ctx, user, filter, c.row)
		if err != nil {
			t.Fatalf("ProbeEvaluateFilter(%v): %v", c.row, err)
		}
		if got != c.want {
			t.Errorf("ProbeEvaluateFilter(%v) = %v, want %v", c.row, got, c.want)
		}
	}

	if _, err := eng.ProbeLowerFilter(ctx, user, "   "); err == nil {
		t.Error("an empty filter lowered")
	}
}
