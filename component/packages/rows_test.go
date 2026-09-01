package packages

import "testing"

// memqlRows has to read BOTH shapes the engine returns, and only one of them
// can be built by a fake: ExecuteResult.output -- what a SHAPED query fills --
// is unexported with no setter, so every fake in this tree produces the Bundle
// branch. That is exactly the asymmetry memql#4794 inherited from the Files
// epic: a shaped query nils the Bundle, so a reader covered only against a
// fake reads an empty result from a perfectly good production query.
//
// So the shaped branches are covered here, directly, against the values the
// engine actually produces.
func TestMemqlRowsReadsEveryShapeTheEngineReturns(t *testing.T) {
	t.Run("a shaped query's []map rows", func(t *testing.T) {
		rows := memqlRows([]map[string]any{{"id": "a"}, {"id": "b"}})
		if len(rows) != 2 || rows[0]["id"] != "a" {
			t.Fatalf("got %+v", rows)
		}
	})

	t.Run("a shaped query's []any rows", func(t *testing.T) {
		rows := memqlRows([]any{map[string]any{"id": "a"}, "not a row"})
		if len(rows) != 1 || rows[0]["id"] != "a" {
			t.Fatalf("got %+v", rows)
		}
	})

	t.Run("a single-object return", func(t *testing.T) {
		rows := memqlRows(map[string]any{"id": "a"})
		if len(rows) != 1 || rows[0]["id"] != "a" {
			t.Fatalf("got %+v", rows)
		}
	})

	t.Run("an object carrying rows", func(t *testing.T) {
		rows := memqlRows(map[string]any{"rows": []any{map[string]any{"id": "a"}}})
		if len(rows) != 1 || rows[0]["id"] != "a" {
			t.Fatalf("got %+v", rows)
		}
	})

	t.Run("nothing", func(t *testing.T) {
		if rows := memqlRows(nil); rows != nil {
			t.Fatalf("got %+v", rows)
		}
	})

	t.Run("a bundle", func(t *testing.T) {
		rows := memqlRows(asBundle([]map[string]any{{"id": "a", "concept": "v1:platform:package", "name": "acme"}}))
		if len(rows) != 1 || rows[0]["id"] != "a" || rows[0]["name"] != "acme" {
			t.Fatalf("got %+v", rows)
		}
	})
}
