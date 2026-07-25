package automations_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memql#2766: a `??` arm that is a bare reference must COMPILE to a
// reference, not to a quoted string literal.
//
// The shorthand's whole premise is that it lowers byte-identically to the
// coalesce() spelling. The value-slot fold briefly broke that in the one
// place a source-text assertion cannot see: parseValue returns a Go
// string for a bare identifier, and wrapping that in a LiteralExpr turned
//
//	summary: body ?? ""      into      coalesce("body", "")
//
// which resolves to the literal text "body" -- so every note artifact got
// summary="body", every registered cluster node got id="node.id", and
// every AI utterance got partitionId="partitionId". That is the memql#580
// render-the-identifier-as-its-own-name class, and it is invisible to
// both the DSL load gate and any grep of the .memql source.
//
// This walks the REAL embedded automation tree through the production
// loader and asserts no compiled step argument contains a coalesce whose
// FIRST arm is a string literal. A leading literal arm is never
// meaningful anyway -- a non-empty literal always wins the selection, so
// the fallback could never fire -- which makes it a clean signal.
func TestAuthoredAutomations_NoQuotedIdentifierCoalesceArgs(t *testing.T) {
	loader := automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()})
	loaded, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("no automations loaded; the walk must not pass vacuously")
	}

	checked := 0
	for _, auto := range loaded {
		for _, step := range auto.Steps {
			blob, merr := json.Marshal(step)
			if merr != nil {
				// Fail rather than skip: a silently-skipped step would
				// not count toward the non-vacuity floor below either.
				t.Fatalf("%s/%s: marshal step: %v", auto.Name, step.ID, merr)
			}
			checked++
			// The serialized form escapes inner quotes, so the bug shows
			// up as coalesce(\" in the JSON text.
			for _, probe := range []string{`coalesce(\"`, `coalesce("`} {
				if idx := strings.Index(string(blob), probe); idx >= 0 {
					t.Errorf("%s/%s: a coalesce arm compiled to a STRING LITERAL rather than a reference "+
						"-- it will resolve to the identifier's own name (memql#580 class). Near: %s",
						auto.Name, step.ID, excerpt(string(blob), idx))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no steps inspected; the walk must not pass vacuously")
	}
	t.Logf("inspected %d steps across %d automations", checked, len(loaded))
}

func excerpt(s string, at int) string {
	start := at - 60
	if start < 0 {
		start = 0
	}
	end := at + 120
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}
