package logstore

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// renderDB is a bun handle that never connects: String() on a query formats
// it against the dialect alone, which is all the SQL-composition tests need.
func renderDB() *bun.DB {
	return bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN("postgres://render:render@127.0.0.1:1/render?sslmode=disable"))), pgdialect.New())
}

func TestSearchSQLComposesTheScopeAsOrAndTheRestAsAnd(t *testing.T) {
	db := renderDB()
	var rows []Row
	q := Query{
		WindowStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		Apps:        []string{"files"},
		SubjectConcepts: []string{
			"v1:library:file", "v1:library:artifact",
		},
		Levels:     []string{"warn", "error"},
		Components: []string{"packages.pipeline"},
		Text:       "100%_done\\",
		Limit:      0,
		BeforeAt:   time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		BeforeId:   "row-9",
	}
	sqlText := searchQuery(db, q, &rows).String()

	// ONE scope predicate, ORed, inside its own parentheses.
	scopeStart := strings.Index(sqlText, "(app = ANY(")
	if scopeStart < 0 {
		t.Fatalf("no scope predicate:\n%s", sqlText)
	}
	scope := sqlText[scopeStart:]
	if end := strings.Index(scope, "))"); end > 0 {
		scope = scope[:end+2]
	}
	if !strings.Contains(scope, " OR subject_concept = ANY(") {
		t.Errorf("apps and subjectConcepts must be ORed inside one clause, got %s", scope)
	}
	if strings.Contains(scope, " AND ") {
		t.Errorf("the scope clause must not AND its two halves: %s", scope)
	}
	// Every other facet ANDs with it.
	for _, want := range []string{
		"occurred_at >= '2026-09-01 00:00:00+00:00'",
		"occurred_at < '2026-09-02 00:00:00+00:00'",
		"level = ANY(",
		"component = ANY(",
		"(occurred_at, id) < ('2026-09-01 12:00:00+00:00', 'row-9')",
		"ORDER BY occurred_at DESC, id DESC",
		"LIMIT 200",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("missing %q in:\n%s", want, sqlText)
		}
	}
	if strings.Count(sqlText, " AND ") < 5 {
		t.Errorf("the non-scope facets must be ANDed; only %d ANDs in:\n%s", strings.Count(sqlText, " AND "), sqlText)
	}
	// The array values are bound, not concatenated: they appear as a typed
	// array literal, and the ILIKE metacharacters are escaped.
	if !strings.Contains(sqlText, `'{"files"}'::text[]`) && !strings.Contains(sqlText, `'{files}'::text[]`) {
		t.Errorf("apps must render as a text[] array parameter:\n%s", sqlText)
	}
	if !strings.Contains(sqlText, `message ILIKE '%100\%\_done\\\\%' ESCAPE '\'`) && !strings.Contains(sqlText, `ILIKE '%100\%\_done\\%'`) {
		t.Errorf("text must be escaped for ILIKE:\n%s", sqlText)
	}
}

func TestSearchSQLWithOnlyOneScopeHalfHasNoOr(t *testing.T) {
	db := renderDB()
	var rows []Row
	only := searchQuery(db, Query{Apps: []string{"files"}, Limit: 9999}, &rows).String()
	if strings.Contains(only, " OR ") {
		t.Errorf("a lone apps facet must not render an OR:\n%s", only)
	}
	if !strings.Contains(only, "LIMIT 500") {
		t.Errorf("limit must be capped at 500:\n%s", only)
	}
	none := searchQuery(db, Query{}, &rows).String()
	if strings.Contains(none, "WHERE") {
		t.Errorf("no facets must mean no WHERE:\n%s", none)
	}
}

func TestTailSQLCursorAscendsAndBaselineReverses(t *testing.T) {
	db := renderDB()
	var rows []Row
	withCursor, reverse := tailQuery(db, Query{AfterAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), AfterId: "r1", Limit: 50}, &rows)
	s := withCursor.String()
	if reverse {
		t.Errorf("a cursor tail must not be reversed")
	}
	if !strings.Contains(s, "(occurred_at, id) > ('2026-09-01 00:00:00+00:00', 'r1')") || !strings.Contains(s, "ORDER BY occurred_at ASC, id ASC") || !strings.Contains(s, "LIMIT 50") {
		t.Errorf("tail with a cursor:\n%s", s)
	}
	baseline, reverse := tailQuery(db, Query{}, &rows)
	b := baseline.String()
	if !reverse {
		t.Errorf("the baseline must be fetched newest-first and reversed")
	}
	if !strings.Contains(b, "ORDER BY occurred_at DESC, id DESC") || !strings.Contains(b, "LIMIT 200") {
		t.Errorf("baseline tail:\n%s", b)
	}
	// A cursor with no id is not a cursor.
	half, _ := tailQuery(db, Query{AfterAt: time.Now()}, &rows)
	if strings.Contains(half.String(), "(occurred_at, id) >") {
		t.Errorf("afterAt without afterId must not form a cursor")
	}
}

func TestIlikePattern(t *testing.T) {
	if got := ilikePattern(`a%b_c\d`); got != `%a\%b\_c\\d%` {
		t.Errorf("ilikePattern = %q", got)
	}
}
