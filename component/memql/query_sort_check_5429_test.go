package memql

import (
	"errors"
	"strings"
	"testing"
)

// query_sort_check_5429_test.go -- memql#5429: an authored query's sort clause
// names a declared field or a sortable row intrinsic, in a direction written
// "asc" or "desc", or the load refuses it.
//
// THE DEFECT. A sort key is a string literal, so the grammar could only say
// that it was one. `sort "row.nope"` and `sort "nope"` loaded and ordered on a
// JSONB path no stored row carries -- a silent no-op ordering -- and
// `sort "priority", "sideways"` loaded as two keys, since a string after a key
// that is not a direction starts the next key.

const sortCheckConcepts = `/// A support ticket.
concept sortTicket5429 {
  title     string
  status    string
  priority  int
  tags      []string
  details   object
  routing {
    queue  string
  }
}
`

// sortCheckQuery is a query over sortTicket5429 with one sort clause.
func sortCheckQuery(name, clause string) string {
	return "/// Tickets, ordered.\nquery sortTicket5429 " + name + " {\n  filter row => row.status == \"open\"\n  sort " + clause + "\n  paginate 20\n}\n\n"
}

func TestSortClauseIsHeldToTheBoundConcept(t *testing.T) {
	type tc struct {
		name, clause string
		code         string // "" loads
		fragments    []string
	}
	cases := []tc{
		// Loads: a declared field bare or under payload., a sortable intrinsic
		// under row., a declared field inside a block, a key into an open
		// object or through an untyped field, a list field, both directions.
		{"sortOkBare5429", `"priority", "desc"`, "", nil},
		{"sortOkTwoKeys5429", `"priority", "asc", "row.createdAt", "desc"`, "", nil},
		{"sortOkIntrinsics5429", `"row.id", "asc", "row.concept", "row.type", "row.createdBy", "desc"`, "", nil},
		{"sortOkPayloadPrefix5429", `"payload.title", "asc"`, "", nil},
		{"sortOkBlockField5429", `"routing.queue", "asc"`, "", nil},
		{"sortOkOpenObject5429", `"details.severity.level", "desc"`, "", nil},
		{"sortOkNoDirection5429", `"title"`, "", nil},
		{"sortOkListField5429", `"tags", "asc"`, "", nil},

		// Refused.
		{"sortBadUndeclared5429", `"priorty", "desc"`, SortCodeUnknownKey,
			[]string{`sort key "priorty": v1:sortcheck5429:sortTicket5429 declares no field "priorty"`, `did you mean "priority"?`}},
		{"sortBadUndeclaredNoNear5429", `"zzz", "desc"`, SortCodeUnknownKey,
			[]string{`declares no field "zzz"`, `sort by a field the concept declares, or by a row intrinsic as "row.createdAt"`}},
		{"sortBadSecondKey5429", `"priority", "desc", "nope"`, SortCodeUnknownKey,
			[]string{`sort key "nope": v1:sortcheck5429:sortTicket5429 declares no field "nope"`}},
		{"sortBadDirectionWord5429", `"priority", "sideways"`, SortCodeUnknownKey,
			[]string{`sort key "sideways": it is not a direction, which is "asc" or "desc"`, `write "asc" or "desc" after the key it orders`}},
		{"sortBadDirectionCase5429", `"priority", "ASC"`, SortCodeUnknownDirection,
			[]string{`sort direction "ASC": a direction is "asc" or "desc", written in lower case -- write "asc"`}},
		{"sortBadDirectionMixed5429", `"priority", "Desc"`, SortCodeUnknownDirection,
			[]string{`write "desc"`}},
		{"sortBadRowUnknown5429", `"row.updatedAt", "desc"`, SortCodeUnknownKey,
			[]string{`sort key "row.updatedAt": row.updatedAt is not a sortable row intrinsic`}},
		{"sortBadRowTypo5429", `"row.createAt", "desc"`, SortCodeUnknownKey,
			[]string{`did you mean "row.createdAt"?`}},
		{"sortBadRowPayload5429", `"row.priority", "desc"`, SortCodeUnknownKey,
			[]string{`a payload field is named bare in a sort key: write "priority"`}},
		{"sortBadRowAlone5429", `"row", "desc"`, SortCodeUnknownKey,
			[]string{"`row.` addresses a row intrinsic and needs one"}},
		{"sortBadRowProvenance5429", `"row.provenance.kind", "desc"`, SortCodeUnknownKey,
			[]string{"row.provenance is an object, and an object has no order"}},
		{"sortBadRowPartition5429", `"row.partition", "desc"`, SortCodeUnknownKey,
			[]string{"row.partition is a row intrinsic with no ordering"}},
		{"sortBadReserved5429", `"actor.userId", "desc"`, SortCodeUnknownKey,
			[]string{`"actor" is a reserved engine name, which no concept may declare as a field`}},
		{"sortBadPayloadAlone5429", `"payload", "desc"`, SortCodeUnknownKey,
			[]string{"`payload.` needs a field after it"}},
		{"sortBadPayloadUndeclared5429", `"payload.nope", "desc"`, SortCodeUnknownKey,
			[]string{`declares no field "nope"`}},
		{"sortBadThroughScalar5429", `"priority.x", "desc"`, SortCodeUnknownKey,
			[]string{`"priority" is a number, and a number has no fields -- sort by "priority" itself`}},
		{"sortBadThroughList5429", `"tags.first", "desc"`, SortCodeUnknownKey,
			[]string{`"tags" is a list, and a list has no fields`}},
		{"sortBadBlockUndeclared5429", `"routing.team", "desc"`, SortCodeUnknownKey,
			[]string{`declares no field "routing.team"`}},
		{"sortBadEmptySegment5429", `"routing..queue", "desc"`, SortCodeUnknownKey,
			[]string{"a field path has an empty segment"}},
	}

	var queries strings.Builder
	for _, c := range cases {
		queries.WriteString(sortCheckQuery(c.name, c.clause))
	}
	diags := lint(t, sigTree(map[string]string{
		"sortcheck5429/concepts.memql": sortCheckConcepts,
		"sortcheck5429/queries.memql":  queries.String(),
	}))

	for _, c := range cases {
		var found []LintDiagnostic
		for _, d := range diags {
			if strings.Contains(d.Message, `"`+c.name+`"`) {
				found = append(found, d)
			}
		}
		if c.code == "" {
			for _, d := range found {
				t.Errorf("sort %s: refused, want it to load:\n  %s", c.clause, d.Message)
			}
			continue
		}
		if len(found) != 1 {
			t.Errorf("sort %s: %d diagnostics, want one refusal:\n%s", c.clause, len(found), sigDump(found))
			continue
		}
		d := found[0]
		if d.Code != c.code {
			t.Errorf("sort %s: code = %q, want %q\n  %s", c.clause, d.Code, c.code, d.Message)
		}
		for _, f := range c.fragments {
			if !strings.Contains(d.Message, f) {
				t.Errorf("sort %s: the refusal does not say %q:\n  %s", c.clause, f, d.Message)
			}
		}
		if !strings.HasSuffix(strings.TrimSpace(d.Message), "["+c.code+"]") {
			t.Errorf("sort %s: the code is not last, in brackets:\n  %s", c.clause, d.Message)
		}
	}
}

// A bare row intrinsic is the sort-row-intrinsic contract gate's to refuse,
// naming "row.createdAt". The clause check stays out of its way, so the author
// reads the refusal once.
func TestSortClauseLeavesABareIntrinsicToItsGate(t *testing.T) {
	diags := lint(t, sigTree(map[string]string{
		"sortcheck5429/concepts.memql": sortCheckConcepts,
		"sortcheck5429/queries.memql":  sortCheckQuery("sortBareIntrinsic5429", `"createdAt", "desc"`),
	}))
	var gate, clause int
	for _, d := range diags {
		switch {
		case strings.Contains(d.Message, `sort key names the row intrinsic "createdAt" bare`):
			gate++
		case strings.Contains(d.Message, "sortBareIntrinsic5429"):
			clause++
			t.Errorf("the clause check refused a bare intrinsic the gate already refuses:\n  %s", d.Message)
		}
	}
	if gate != 1 {
		t.Errorf("the sort-row-intrinsic gate refused %d time(s), want once:\n%s", gate, sigDump(diags))
	}
}

// With no bound concept -- a load with no registry -- a payload key has no
// declaration to be held to, and loads; the concept-independent rules still
// refuse.
func TestSortClauseWithoutAConceptChecksOnlyWhatNeedsNone(t *testing.T) {
	c, err := newSortChecker(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.checkKey("anything.at.all", false); err != nil {
		t.Errorf("a payload key with no bound concept was refused: %v", err)
	}
	for _, key := range []string{"row.nope", "row", "provenance", "actor.userId", "row.provenance"} {
		if err := c.checkKey(key, false); err == nil {
			t.Errorf("sort key %q loaded with no bound concept; it names nothing whatever the concept", key)
		}
	}
}

// TestSortCheckAgreesWithTheRuntimeCompiler holds the load check to the
// run-time compiler (compileSortField) on every key whose answer needs no
// concept: what the load refuses the runtime would refuse on every call, and
// what the load accepts the runtime compiles. Two classifiers of one key that
// disagreed would refuse a query that runs, or load one that fails.
func TestSortCheckAgreesWithTheRuntimeCompiler(t *testing.T) {
	c, err := newSortChecker(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"row.id", "row.concept", "row.type", "row.createdAt", "row.createdBy", "ROW.createdAt",
		"row.nope", "row", "row.", "row.provenance", "row.provenance.kind", "row.partition", "row.schema",
		"row.actor", "row.row", "provenance", "provenance.kind", "actor.userId", "now", "config.x",
		"meta", "schema", "partition", "trace", "args.x", "payload", "payload.title", "payload.a.b",
		"title", "a.b.c",
	} {
		loadErr := c.checkKey(key, false)
		_, runErr := compileSortField(SortField{Field: key, Direction: SortDirectionDesc})
		if (loadErr == nil) != (runErr == nil) {
			t.Errorf("sort key %q: load says %v, runtime says %v -- the two classifiers disagree", key, loadErr, runErr)
		}
		var sk *SortKeyError
		if loadErr != nil && !errors.As(loadErr, &sk) {
			t.Errorf("sort key %q: the refusal is a %T, not a coded SortKeyError", key, loadErr)
		}
	}
}
