package parser

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// sort_direction_text_test.go -- memql#5429. The grammar reads a sort direction
// in any case ("ASC" orders ascending) and records the spelling it read; a key
// written with no direction records none, which is how the loader tells that
// the literal after it sat where a direction goes.
func TestSortFieldRecordsTheDirectionAsWritten(t *testing.T) {
	expr := queryFilterExpr(t, `sort(status == "open", "priority", "ASC", "title", "row.createdAt", "desc")`)
	sortExpr, ok := expr.(*ast.SortExpr)
	if !ok {
		t.Fatalf("parsed to %T, want *ast.SortExpr", expr)
	}
	want := []ast.SortField{
		{Field: "priority", Direction: ast.SortAsc, DirectionText: "ASC"},
		{Field: "title", Direction: ast.SortDesc, DirectionText: ""},
		{Field: "row.createdAt", Direction: ast.SortDesc, DirectionText: "desc"},
	}
	if len(sortExpr.Fields) != len(want) {
		t.Fatalf("parsed %d sort fields, want %d: %+v", len(sortExpr.Fields), len(want), sortExpr.Fields)
	}
	for i, w := range want {
		if got := sortExpr.Fields[i]; got != w {
			t.Errorf("sort field %d = %+v, want %+v", i, got, w)
		}
	}
}
