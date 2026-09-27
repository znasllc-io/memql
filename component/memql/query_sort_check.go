package memql

// query_sort_check.go -- an authored query's sort clause, held to the concept
// the query binds at load (memql#5429).
//
// A sort key is a string literal, so the grammar can only say that it IS one;
// what it names went unchecked. `sort "row.nope"` and `sort "nope"` loaded and
// ordered on a JSONB path no stored row carries -- a silent no-op ordering --
// and `sort "priority", "sideways"` loaded as two keys, because a string after
// a key that is not a direction starts the next key. The runtime compiler
// (compileSortField) refuses the reserved-namespace shapes, but only when the
// query first RUNS, as an error on every call.
//
// So a query's sort clause is checked where the bound concept is known, the
// same place its filter is lowered:
//
//   - a `row.` key names a SORTABLE row intrinsic (id, concept, type,
//     createdAt, createdBy) -- the classification compileSortField makes at
//     run time, and TestSortCheckAgreesWithTheRuntimeCompiler holds the two to
//     one answer;
//   - any other key names a payload field the concept DECLARES, written bare
//     or under the explicit `payload.` prefix, hop by hop down a dotted path
//     the way the filter lowering walks `row.a.b`: every hop is declared until
//     the first hop into a value whose keys the declaration does not close (an
//     open `object`, a map, a union, an untyped field), past which the rest of
//     the path is the author's to answer for; a hop through a scalar or a list
//     names nothing;
//   - a direction is "asc" or "desc". Lower case is the spelling to write,
//     but the grammar reads a direction in any case and "ASC" always ordered
//     ascending, so any case loads -- refusing it would retire a spelling that
//     worked, which the language freeze does through a deprecation window,
//     not a load refusal. What IS refused is a word where a direction goes
//     that is no direction: `sort "priority", "sideways"`, which the grammar
//     reads as a second key, is refused as an unknown direction when the
//     concept declares no field of that name.
//
// A bare row intrinsic (`sort "createdAt"`) is NOT this check's: the
// sort-row-intrinsic contract gate refuses it, by the same detector the editor
// uses, and a second refusal of the same key would say the same thing twice.
//
// WITHOUT A BOUND CONCEPT -- a load with no concept registry, which only an
// offline tool or a test runs -- the concept-independent rules still apply
// (the `row.` namespace and the reserved heads) and a payload key -- a word in
// a direction's place included -- is not checked: there is no declaration to
// hold it to. A query with an
// unresolvable signature concept never gets here; it is refused first
// (memql#5433).

import (
	"fmt"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// The sort clause's refusal codes. Stable: the load report, memqllint and the
// conformance corpus key on them; the wording may be revised around them.
const (
	// SortCodeUnknownKey: a sort key that names neither a declared payload
	// field nor a sortable row intrinsic.
	SortCodeUnknownKey = "sort_key_unknown"
	// SortCodeUnknownDirection: a word written where a direction goes --
	// right after a key written with none -- that is neither "asc" nor "desc"
	// (in any case) nor a field the concept declares.
	SortCodeUnknownDirection = "sort_direction_unknown"
)

// sortableIntrinsics is how a refusal names the row intrinsics a sort key may
// order by -- the set compileSortField has an ORDER BY for.
const sortableIntrinsics = `"row.id", "row.concept", "row.type", "row.createdAt" and "row.createdBy"`

// SortKeyError refuses one key, or one direction, of a query's sort clause:
// the literal as written, why it is refused, and what to write instead, with
// the rule id last in brackets.
type SortKeyError struct {
	Code   string
	Text   string
	Reason string
	Fix    string
}

// Error prints the refusal as one sentence:
//
//	sort key "nope": v1:crm:ticket declares no field "nope" -- did you mean
//	"note"? [sort_key_unknown]
func (e *SortKeyError) Error() string {
	what := "sort key"
	if e.Code == SortCodeUnknownDirection {
		what = "sort direction"
	}
	msg := fmt.Sprintf("%s %q: %s", what, e.Text, e.Reason)
	if e.Fix != "" {
		msg += " -- " + e.Fix
	}
	return msg + " [" + e.Code + "]"
}

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *SortKeyError) RuleCode() string { return e.Code }

// checkQuerySortClause checks the sort clause of an authored query's body --
// the SortExpr the struct-form rewriter wraps the query in -- against the
// concept the query binds (nil when none is known). It returns the first key
// or direction it refuses.
func checkQuerySortClause(body languageParser.ExpressionNode, concept *memoryNodes.Concept) error {
	for _, clause := range authoredSortClauses(body) {
		c, err := newSortChecker(concept)
		if err != nil {
			return err
		}
		for i, f := range clause.Fields {
			// A key right after a key written with no direction sits where a
			// direction could have been: `sort "priority", "sideways"`. The
			// grammar read whatever direction it found -- in any case, as at
			// run time -- so only the literals it read as KEYS are judged.
			directionSlot := i > 0 && clause.Fields[i-1].DirectionText == ""
			if err := c.checkKey(f.Field, directionSlot); err != nil {
				return err
			}
		}
	}
	return nil
}

// authoredSortClauses finds the sort clauses in a query body: the SortExpr
// under the directive wrappers the struct-form rewriter stacks around the
// filter (asOf, sort, paginate, refine, then shape or count).
func authoredSortClauses(expr languageParser.ExpressionNode) []*languageParser.SortExpr {
	var out []*languageParser.SortExpr
	for expr != nil {
		switch n := expr.(type) {
		case *languageParser.SortExpr:
			out = append(out, n)
			expr = n.Target
		case *languageParser.ShapeExpr:
			expr = n.Target
		case *languageParser.CountExpr:
			expr = n.Target
		case *languageParser.RefineExpr:
			expr = n.Target
		case *languageParser.PaginateExpr:
			expr = n.Target
		case *languageParser.SelectExpr:
			expr = n.Target
		case *languageParser.DepthExpr:
			expr = n.Target
		case *languageParser.TimestampExpr:
			expr = n.Target
		default:
			return out
		}
	}
	return out
}

// sortChecker holds a sort key to one concept's declarations.
type sortChecker struct {
	concept *memoryNodes.Concept
	fields  map[string]conceptFieldShape
	closed  map[string]bool
}

func newSortChecker(concept *memoryNodes.Concept) (*sortChecker, error) {
	c := &sortChecker{concept: concept}
	if concept == nil {
		return c, nil
	}
	fields, err := flattenConceptFields(concept)
	if err != nil {
		return nil, fmt.Errorf("read the declared fields of %s: %w", concept.Name, err)
	}
	closed, err := closedObjectPaths(concept)
	if err != nil {
		return nil, fmt.Errorf("read the declared blocks of %s: %w", concept.Name, err)
	}
	c.fields, c.closed = fields, closed
	return c, nil
}

// checkKey classifies one key the way compileSortField does -- `row.` first,
// then a bare intrinsic, then a reserved head, then a payload path -- and
// refuses what names nothing.
func (c *sortChecker) checkKey(key string, directionSlot bool) error {
	key = strings.TrimSpace(key)
	refuse := func(reason, fix string) error {
		return &SortKeyError{Code: SortCodeUnknownKey, Text: key, Reason: reason, Fix: fix}
	}

	head, rest, dotted := strings.Cut(key, ".")
	if strings.EqualFold(head, rowIntrinsicNamespace) {
		return c.checkRowKey(key, strings.TrimSpace(rest), dotted, refuse)
	}
	if !dotted {
		if info, ok := resolveIntrinsicField(key); ok && info.kind != intrinsicFieldProvenance {
			// A bare scalar intrinsic: the sort-row-intrinsic contract gate
			// refuses it, naming "row.<name>".
			return nil
		}
	}

	path := key
	if strings.EqualFold(head, "payload") {
		if !dotted || strings.TrimSpace(rest) == "" {
			return refuse("`payload.` needs a field after it",
				"name the payload field bare, as \"priority\"")
		}
		path = rest
	} else if reservedFilterHead(head) {
		return refuse(fmt.Sprintf("%q is a reserved engine name, which no concept may declare as a field", head),
			"sort by a declared payload field, or by a row intrinsic: one of "+sortableIntrinsics)
	}
	return c.checkPayloadPath(key, path, directionSlot, refuse)
}

// checkRowKey checks a `row.`-namespaced key: it names a sortable row
// intrinsic, or nothing.
func (c *sortChecker) checkRowKey(key, leaf string, dotted bool, refuse func(reason, fix string) error) error {
	if !dotted || leaf == "" {
		return refuse("`row.` addresses a row intrinsic and needs one", "write one of "+sortableIntrinsics)
	}
	info, ok := resolveIntrinsicField(leaf)
	switch {
	case ok && info.kind == intrinsicFieldProvenance, strings.HasPrefix(strings.ToLower(leaf), "provenance."):
		return refuse("row.provenance is an object, and an object has no order", "sort by one of "+sortableIntrinsics)
	case ok && !strings.Contains(leaf, "."):
		return nil
	case isUnfilterableRowIntrinsic(leaf):
		return refuse(fmt.Sprintf("row.%s is a row intrinsic with no ordering", leaf), "sort by one of "+sortableIntrinsics)
	}
	if _, declared := c.fields[leaf]; declared {
		return refuse(fmt.Sprintf("%s is a payload field of %s, not a row intrinsic", leaf, c.concept.Name),
			fmt.Sprintf("a payload field is named bare in a sort key: write %q", leaf))
	}
	fix := "the sortable intrinsics are " + sortableIntrinsics
	if near := nearestIntrinsic(leaf); near != "" {
		fix = fmt.Sprintf("did you mean %q? The sortable intrinsics are %s", "row."+near, sortableIntrinsics)
	}
	return refuse(fmt.Sprintf("%s is not a sortable row intrinsic", key), fix)
}

// nearestIntrinsic is the sortable intrinsic a misspelled `row.` leaf is
// closest to, or "". Close means at most two edits, and no more than one edit
// per three letters: "createAt" is createdAt, "nope" is not type.
func nearestIntrinsic(leaf string) string {
	best, bestDist := "", 3
	for _, name := range []string{"id", "concept", "type", "createdAt", "createdBy"} {
		d := levenshtein(strings.ToLower(leaf), strings.ToLower(name))
		if d < bestDist && d*3 <= len(leaf) {
			best, bestDist = name, d
		}
	}
	return best
}

// checkPayloadPath holds a payload key to the concept's declarations, hop by
// hop, as the filter lowering holds `row.a.b` (expr_lower.go): every hop is
// declared, down to the first hop into a value whose keys the declaration
// does not close.
func (c *sortChecker) checkPayloadPath(key, path string, directionSlot bool, refuse func(reason, fix string) error) error {
	if c.fields == nil {
		return nil // no bound concept: no declaration to hold the key to
	}
	segs := strings.Split(path, ".")
	walked := ""
	for i, seg := range segs {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return refuse("a field path has an empty segment", "write each field name between the dots")
		}
		if walked == "" {
			walked = seg
		} else {
			walked += "." + seg
		}
		s, ok := c.fields[walked]
		if !ok {
			return c.undeclared(walked, directionSlot && len(segs) == 1, refuse)
		}
		if i == len(segs)-1 {
			return nil
		}
		switch {
		case s.Type == "any" || s.Type == "":
			return nil // untyped: its members are the author's to answer for
		case strings.HasPrefix(s.Type, "[]"):
			return refuse(fmt.Sprintf("%q is a list, and a list has no fields", walked),
				"a sort key orders by one value per row; sort by a scalar field")
		case !objectLikeType(s.Type):
			return refuse(fmt.Sprintf("%q is a %s, and a %s has no fields", walked, declTypeWord(s.Type), declTypeWord(s.Type)),
				fmt.Sprintf("sort by %q itself", walked))
		case s.Type != "object" || !c.closed[walked]:
			return nil // an open object, a map or a union: its keys are not all declared
		}
	}
	return nil
}

// undeclared refuses a payload path the concept does not declare, suggesting
// the nearest declared name at the same depth. A single word written where a
// direction goes is refused as an unknown DIRECTION: it is no direction, and
// no field either, so the direction is what the author was most likely
// writing -- and the refusal names the field it may have meant all the same.
func (c *sortChecker) undeclared(path string, directionSlot bool, refuse func(reason, fix string) error) error {
	prefix := ""
	if i := strings.LastIndex(path, "."); i >= 0 {
		prefix = path[:i+1]
	}
	var near []string
	for declared := range c.fields {
		if !strings.HasPrefix(declared, prefix) || strings.Contains(strings.TrimPrefix(declared, prefix), ".") {
			continue
		}
		if levenshtein(declared, path) <= 2 {
			near = append(near, declared)
		}
	}
	sort.Strings(near)
	reason := fmt.Sprintf("%s declares no field %q", c.concept.Name, path)
	fix := `sort by a field the concept declares, or by a row intrinsic as "row.createdAt"`
	if len(near) > 0 {
		fix = fmt.Sprintf("did you mean %q?", near[0])
	}
	if directionSlot {
		return &SortKeyError{
			Code:   SortCodeUnknownDirection,
			Text:   path,
			Reason: fmt.Sprintf(`a direction is "asc" or "desc", and as the next key it names no field of %s`, c.concept.Name),
			Fix:    directionFix(path, near),
		}
	}
	return refuse(reason, fix)
}

// directionFix is what to write for a word in a direction's place: the
// direction it spells out, when it spells one, and otherwise either direction
// -- or the declared field it is closest to, if it was meant as a key.
func directionFix(word string, nearFields []string) string {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "ascending", "ascend", "up":
		return `write "asc"`
	case "descending", "descend", "down":
		return `write "desc"`
	}
	fix := `write "asc" or "desc" after the key it orders`
	if len(nearFields) > 0 {
		fix += fmt.Sprintf(", or, for a second key, %q", nearFields[0])
	}
	return fix
}
