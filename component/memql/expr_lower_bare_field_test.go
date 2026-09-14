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
// predicate may read. A bare name that is no field keeps the list. Pinned by
// the conformance corpus too (expr/queryFilter/bare-payload-field.memql).
func TestLowerNamesTheRowSpellingOfABarePayloadField(t *testing.T) {
	concept := lowerTestConcept(t)
	for src, want := range map[string]string{
		`row => status == "open"`:  "write `row.status`",
		`it => status == "open"`:   "write `it.status`",
		`row => unknown == "open"`: "A predicate reads its parameter (row), args, actor, now and config",
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
