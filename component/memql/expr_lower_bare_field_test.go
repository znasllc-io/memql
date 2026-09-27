package memql

import (
	"errors"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// A bare name that is a declared field of the bound concept is the pre-v1
// filter's payload field (D1), and its fix is mechanical: the refusal names
// the rewritten spelling (D24), `row.status`, rather than listing the roots a
// predicate may read. A bare row intrinsic is the same mistake with the same
// fix, `row.id` (memql#5434: the editor's bare-row-intrinsic warning gives way
// to this refusal, so the fix must not be lost with it). A bare name that is
// neither keeps the list. Pinned by the conformance corpus too
// (expr/queryFilter/bare-payload-field.memql).
func TestLowerNamesTheRowSpellingOfABarePayloadField(t *testing.T) {
	concept := lowerTestConcept(t)
	for src, want := range map[string]string{
		`row => status == "open"`:   "write `row.status`",
		`it => status == "open"`:    "write `it.status`",
		`row => id == "x"`:          "write `row.id`",
		`row => createdAt > "2026"`: "write `row.createdAt`",
		`it => createdBy == "u"`:    "write `it.createdBy`",
		`row => unknown == "open"`:  "A predicate reads its parameter (row), args, actor, now and config",
	} {
		lam, err := languageParser.ParseV1Lambda(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		_, err = Lower(lam.Body, LowerEnv{Position: tiers.PositionQueryFilter, Param: lam.Params[0], Concept: concept, Args: map[string]ArgType{}})
		var le *LowerError
		if !errors.As(err, &le) || !strings.Contains(err.Error(), want) {
			t.Errorf("Lower(%s) = %v; want a refusal carrying %q", src, err, want)
		}
	}
}
