package memql

import (
	"errors"
	"strings"
	"testing"
)

// sort_path_segment_test.go -- a sort key's payload path is rendered into the
// ORDER BY, so every segment of it is held to the field-name rule the filter's
// path builders already apply (isSafePathSegment in buildJSONPathExpression
// and jsonTextPathOn): letters, digits, `_` and `-`.

// A key whose segment is not a field name is refused when the sort compiles,
// and the ORDER BY renderer never renders it.
func TestSortKeyPathSegmentsAreFieldNames(t *testing.T) {
	for _, key := range []string{
		"a'b", "a}b", "a{b", "a)b", "a(b", "a,b", `a"b`, "a;b", "a b", "a?b", `a\b`,
		"title.a'b", "payload.a}b", "details.x)y",
	} {
		_, err := compileSortField(SortField{Field: key, Direction: SortDirectionDesc})
		if err == nil {
			t.Errorf("sort key %q compiled; a segment that is not a field name must be refused", key)
			continue
		}
		if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "is not a field name") {
			t.Errorf("sort key %q: refused with %v, want an invalid-argument refusal naming the segment", key, err)
		}
		// The load check, which holds an authored clause, refuses it too, so
		// the two cannot disagree about a key.
		c, cerr := newSortChecker(nil)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if lerr := c.checkKey(key, false); lerr == nil {
			t.Errorf("sort key %q: the load check admitted a key the runtime refuses", key)
		}
	}

	// The renderer is the second line: a sort field that reached it with a
	// segment that is not a field name -- which compileSortField no longer
	// builds -- renders no path at all.
	for _, segment := range []string{"a'b", "a}b", "a)b", "a,b"} {
		f := compiledSortField{kind: sortFieldPayload, direction: SortDirectionAsc, payloadPath: []string{segment}}
		expr := f.sqlOrderExpr()
		if strings.Contains(expr, segment) || strings.Contains(expr, "payload") {
			t.Errorf("segment %q rendered into the ORDER BY: %s", segment, expr)
		}
	}
}

// The keys the tree, the corpus and the SDK sort by still compile, to the path
// expression the filter builders render.
func TestSortKeysThatAreFieldNamesStillCompile(t *testing.T) {
	for key, want := range map[string]string{
		"priority":              `(payload #>> '{priority}') DESC`,
		"routing.queue":         `(payload #>> '{routing,queue}') DESC`,
		"details.severity":      `(payload #>> '{details,severity}') DESC`,
		"payload.title":         `(payload #>> '{title}') DESC`,
		"due_date":              `(payload #>> '{due_date}') DESC`,
		"metadata.tags":         `(payload #>> '{metadata,tags}') DESC`,
		"sort-order":            `(payload #>> '{sort-order}') DESC`,
		"monthlyPriceUsd":       `(payload #>> '{monthlyPriceUsd}') DESC`,
		"row.createdAt":         `"createdAt" DESC`,
		"createdAt":             `"createdAt" DESC`,
		"row.id":                `id DESC`,
		" lineage.originRunId ": `(payload #>> '{lineage,originRunId}') DESC`,
	} {
		f, err := compileSortField(SortField{Field: key, Direction: SortDirectionDesc})
		if err != nil {
			t.Errorf("sort key %q refused: %v", key, err)
			continue
		}
		if got := f.sqlOrderExpr(); got != want {
			t.Errorf("sort key %q renders %s, want %s", key, got, want)
		}
	}
}
