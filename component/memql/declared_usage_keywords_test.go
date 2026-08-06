package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// declared_usage_keywords_test.go -- memql#3105.
//
// precededByBodyOpener's struct-form arm carried the RETIRED `mutation`
// keyword (renamed to `mutate` in memql#2041) and omitted `logic` entirely,
// while its own doc comment four lines above said `mutate`. It sat there
// unnoticed because the arm is UNREACHABLE: every caller is fed
// `rawSourceForUsage`, which function_loader.go assigns AFTER NormaliseAll,
// so the snapshot is post-rewrite text and every struct form has already
// become `func (Receiver) ...{`.
//
// That is the whole reason it needs a test. An arm that matches nothing has
// no failing case to notice, so the only thing that can keep it honest is a
// gate on its DEFINITION rather than on its behaviour.

// The keyword set must come from the rewriter, not a hardcoded list.
//
// This is the single-sourcing memql#3094 applied to the call-graph checker,
// for the same reason: a second, unsynchronised statement of the construct
// vocabulary is exactly how `mutation` survived a rename.
func TestBodyOpenersComeFromTheRewriter(t *testing.T) {
	if len(structFormBodyOpeners) == 0 {
		t.Fatal("no struct-form body openers; the arm cannot match anything at all")
	}
	if len(structFormBodyOpeners) != len(languageParser.StructFormKeywords) {
		t.Fatalf("openers %v do not correspond 1:1 with the rewriter's StructFormKeywords %v",
			structFormBodyOpeners, languageParser.StructFormKeywords)
	}
	for i, kw := range languageParser.StructFormKeywords {
		if want := kw + " "; structFormBodyOpeners[i] != want {
			t.Errorf("opener[%d] = %q, want %q -- derived from the rewriter, in its order",
				i, structFormBodyOpeners[i], want)
		}
	}
}

// The retired spelling must be gone and the live one present. Asserted
// explicitly rather than left to the derivation above, because this is the
// specific defect the issue reports and it should fail by name.
func TestBodyOpenersUseTheLiveMutationKeyword(t *testing.T) {
	joined := strings.Join(structFormBodyOpeners, "|")
	if strings.Contains(joined, "mutation ") {
		t.Errorf("the RETIRED `mutation` keyword is back in the body openers (%s).\n"+
			"The write function was renamed to `mutate` in memql#2041; `mutation` is the "+
			"invocation-step prefix only (memql#3105).", joined)
	}
	for _, want := range []string{"query ", "mutate ", "logic ", "automation "} {
		if !strings.Contains(joined, want) {
			t.Errorf("body openers are missing %q; got %s", want, joined)
		}
	}
	// `spec ` / `trait ` are NOT struct-form keywords -- they are native
	// parser constructs producing SpecDef rather than a *FunctionDef, so they
	// cannot reach this validator. Listing them implied coverage it never had.
	for _, unwanted := range []string{"spec ", "trait "} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("%q is not a struct-form keyword and cannot reach this validator; "+
				"listing it implies coverage that does not exist", unwanted)
		}
	}
}

// The arm still WORKS if it ever goes live. Unreachable is not the same as
// broken, and the point of keeping it is that a future refactor which moves
// `rawSourceForUsage` above the rewriter degrades safely rather than failing
// open -- a header miss makes extractFunctionBody return "", and every caller
// then silently SKIPS validation.
func TestBodyOpenerMatchesEachStructFormHeader(t *testing.T) {
	for _, kw := range languageParser.StructFormKeywords {
		t.Run(kw, func(t *testing.T) {
			// The prefix as it would appear immediately before the body `{`.
			if !precededByBodyOpener(kw + " someName ") {
				t.Errorf("precededByBodyOpener did not recognise a %q header. If "+
					"rawSourceForUsage ever moves above the rewriter this arm goes live, and a "+
					"miss makes extractFunctionBody return \"\" -- silently disabling "+
					"validateDeclaredUsage, validateLogicEventBinding, validateActorBinding and "+
					"validateLogicEventFields at once (memql#3105).", kw)
			}
		})
	}
	// The procedural form is the one that actually reaches here, so it is
	// asserted alongside rather than assumed.
	if !precededByBodyOpener("func (Query) someName(ctx any) (any, error) ") {
		t.Error("precededByBodyOpener no longer recognises the procedural form -- that IS the " +
			"reachable path, and missing it disables validation for every construct")
	}
}

// Negative control. Without this the two tests above pass just as well against
// a function that returns true for everything, which would make
// extractFunctionBody open on the first `{` it sees -- an args block or an
// `@enum("a","b")` annotation.
func TestBodyOpenerRejectsNonHeaders(t *testing.T) {
	for _, line := range []string{
		"args ",
		"@enum(\"a\", \"b\") ",
		"  filter status==args.s ",
		"mutationHelper ", // a longer identifier that merely STARTS with a keyword
		"queryish ",
		"",
	} {
		if precededByBodyOpener(line) {
			t.Errorf("precededByBodyOpener accepted %q as a body opener; it must not", line)
		}
	}
}
