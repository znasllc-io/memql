package memql

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

// required_concept_fields_on_insert_3619_test.go -- memql#3619's broader point,
// made into a gate.
//
// A concept field declared `@required @default("x")` reads exactly like one
// whose default is applied. It is not: per memql#2960 a concept @default is
// never applied on insert, so the pair means "must be present" AND "here is a
// value nobody will write". mintAction is what that costs -- four such fields,
// none written, every call failing schema validation into a best-effort log
// line while the action library stayed empty.
//
// # Why not resolve the pairing itself
//
// memql#3619 offers two resolutions and asks for one. Both are OUT OF SCOPE for
// this branch, and deliberately:
//
//   - APPLY concept defaults on insert. That is memql#2960's own subject and it
//     changes the meaning of every insert in the tree at once -- 53 concept
//     fields currently carry the pair, and each becomes newly-populated on rows
//     that today omit it. It needs its own blast-radius measurement, not a
//     ride-along in a mutation-write-path fix.
//   - REFUSE @default on a @required field at load. Same 53 fields, every one
//     needing an author decision (drop @required, drop @default, or write the
//     value), plus every mutation that inserts into them. It also DELETES
//     information: the @default is the documented intent, and the mutations
//     that do the right thing express it as `args.x ?? <that value>`.
//
// What IS in scope is making the trap visible at the point where it bites. This
// test is the memql#3619 sweep made permanent: for every insert-kind mutation,
// every @required field of its bound concept must be written by the template.
// A default does not count, because a default is not applied.
func TestInsertMutationsSupplyEveryRequiredConceptField(t *testing.T) {
	reg := load1633Functions(t)
	registry := concept.DefaultRegistry()

	var offenders []string
	examined := 0
	for _, fn := range reg.List() {
		if fn == nil || fn.MutationTemplate == nil {
			continue
		}
		kind := fn.MutationTemplate.Kind
		if kind != "" && kind != ast.MutationKindInsert {
			continue
		}
		examined++
		if missing := unwrittenRequiredFields(fn, registry); len(missing) > 0 {
			offenders = append(offenders, fn.Name+": "+strings.Join(missing, ", "))
		}
	}
	sort.Strings(offenders)

	// Coverage guard. The assertion below is an emptiness check, so a change
	// that made the walk examine nothing -- a renamed Kind constant, a
	// template shape the static reader stops understanding -- would turn this
	// test green while checking nothing at all.
	require.Greater(t, examined, 50,
		"the sweep examined only %d insert-kind mutations; it is no longer reading the tree",
		examined)

	require.Emptyf(t, offenders,
		"insert-kind mutation(s) leave @required concept field(s) unwritten (memql#3619):\n"+
			"  %s\n\nEvery call fails JSON-schema validation. A concept @default does NOT fill "+
			"them -- it is never applied on insert (memql#2960) -- so the body has to supply "+
			"the value with `??`, which is what the mutations that do this correctly already do.",
		strings.Join(offenders, "\n  "))
}

// unwrittenRequiredFields returns the bound concept's @required fields that a
// mutation template never writes. Statically derived: a field is written when
// the payload template, its overlay, or the row-intrinsic slots name it.
func unwrittenRequiredFields(fn *Function, registry concept.Registry) []string {
	conceptName := strings.TrimSpace(fn.MutationTemplate.Concept)
	if conceptName == "" {
		return nil
	}
	bound, err := registry.Get(conceptName)
	if err != nil || bound == nil {
		return nil
	}

	written := map[string]struct{}{}
	if payload, ok := fn.MutationTemplate.PayloadTemplate.(map[string]any); ok {
		for k := range payload {
			written[k] = struct{}{}
		}
	} else if fn.MutationTemplate.PayloadTemplate != nil {
		// A splat (`insert { args.payload }`) hands the whole object over,
		// so the template cannot say which fields it carries. Nothing to
		// assert; the schema check at write time is the gate there.
		return nil
	}
	for k := range fn.MutationTemplate.PayloadOverlayTemplate {
		written[k] = struct{}{}
	}
	if fn.MutationTemplate.IDTemplate != nil {
		written["id"] = struct{}{}
	}
	if fn.MutationTemplate.CreatedAtTemplate != nil {
		written["createdAt"] = struct{}{}
	}

	var missing []string
	for _, field := range bound.RequiredFields() {
		if _, ok := written[field]; !ok {
			missing = append(missing, field)
		}
	}
	sort.Strings(missing)
	return missing
}
