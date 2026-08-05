package parser

import (
	"strings"
	"testing"
)

// asof_caller_instant_3028_test.go -- memql#3028.
//
// `asOf args.X` WITHOUT the `?? latest` fallback is not legal. It is rejected
// at parse, with a message naming the fix.
//
// #3025 made the fallback optional deliberately, and #2992's headline ruling
// ("`asOf args.X` becomes legal, spelled with a coalesce default") read both
// ways. The recorded rationale did not: "one clause covers both callers" and
// "omit the arg and behaviour is byte-identical to today" are properties of the
// COALESCE form specifically, and neither holds for the bare one. That is what
// settled it (#3028 ruling).
//
// # Why rejecting costs nothing
//
// A mandatory instant is still expressible: declare the arg `@required` and
// write `asOf args.at ?? latest`. The fallback is then unreachable and the
// failure lands at the ARG BOUNDARY with a usable message, instead of inside
// temporal resolution.
//
// # Why the bare form had to go
//
// Its failure was discoverable nowhere before production -- not at load, not at
// lint, and not in a test unless someone wrote one that omits the argument. And
// omitting the argument is the COMMON path for this construct, which is the
// entire reason the coalesce spelling exists. A query authored the bare way
// works in its author's test and fails for its ordinary callers.

func TestAsOfCallerInstant_BareArgIsRejectedAtParse(t *testing.T) {
	for name, clause := range map[string]string{
		"simple":      "args.at",
		"dotted":      "args.window.start",
		"extra space": "args.at   ",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseAsOfClause(t, clause)
			if err == nil {
				t.Fatalf("`asOf %s` parsed. Without a fallback every caller who OMITS the "+
					"argument gets a run-time error from resolveAsOfArg -- and omitting it is the "+
					"common path for this construct, so the query works in its author's test and "+
					"fails for its ordinary callers. It must be refused at parse (memql#3028).",
					clause)
			}
			// The diagnostic has to name the fix, or the author is told the
			// form is wrong without being told what to write.
			if !strings.Contains(err.Error(), "?? latest") {
				t.Errorf("the rejection must name `?? latest` as the fix, got: %v", err)
			}
			if !strings.Contains(err.Error(), "3028") {
				t.Errorf("the rejection should cite the issue so the reason is findable, got: %v", err)
			}
		})
	}
}

// TestAsOfCallerInstant_FallbackFormStillParses is the counterpart: the legal
// spelling, and the two forms that never involved an arg at all, must be
// untouched. The single live usage in the tree
// (dsl/deployment/queries.memql: `asOf args.asOf ?? latest`) is this shape.
func TestAsOfCallerInstant_FallbackFormStillParses(t *testing.T) {
	for name, tc := range map[string]struct {
		clause  string
		wantArg string
	}{
		"caller arg with fallback": {"args.asOf ?? latest", "asOf"},
		"dotted arg with fallback": {"args.window.start ?? latest", "window.start"},
	} {
		t.Run(name, func(t *testing.T) {
			te, err := parseAsOfClause(t, tc.clause)
			if err != nil {
				t.Fatalf("`asOf %s` is the LEGAL spelling and must parse: %v", tc.clause, err)
			}
			if te.ArgPath != tc.wantArg {
				t.Errorf("ArgPath = %q, want %q", te.ArgPath, tc.wantArg)
			}
			if !te.FallbackLatest {
				t.Error("FallbackLatest must be set for the coalesce form")
			}
		})
	}
}

// TestAsOfCallerInstant_GrammarSummaryDoesNotAdvertiseTheBareForm pins the
// OTHER diagnostic in this function -- the one an author actually hits first
// (memql#3028 landing review).
//
// parseAsOfFunction has two messages: the targeted rejection above, and the
// `default:` grammar summary shown when the second argument is not recognised
// at all. That summary read "or `args.<name>` (optionally `?? latest`)", so
// mistyping the clause told the author, in the parser's own voice, to write
// the exact form the same function rejects fifteen lines earlier -- and then
// rejected that too. Shipping a rule whose own diagnostic teaches the opposite
// is the discoverability failure this issue exists to fix, reproduced one
// message over.
//
// Asserted as "does not advertise the retired form" rather than as an exact
// string, so rewording the summary stays free while re-optionalising it does
// not.
func TestAsOfCallerInstant_GrammarSummaryDoesNotAdvertiseTheBareForm(t *testing.T) {
	_, err := parseAsOfClause(t, "bogus")
	if err == nil {
		t.Fatal("an unrecognised asOf second argument must be a parse error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "RFC3339") {
		t.Fatalf("expected the grammar summary, got: %v", err)
	}
	if strings.Contains(msg, "optionally") {
		t.Errorf("the grammar summary still calls the `?? latest` fallback optional. It is "+
			"required (memql#3028), so this message hands the author a form the parser refuses.\n"+
			"  got: %s", msg)
	}
	if !strings.Contains(msg, "?? latest") {
		t.Errorf("the grammar summary must still show the caller-arg form WITH its fallback, or "+
			"an author who mistypes the clause is not told the caller-instant form exists at "+
			"all.\n  got: %s", msg)
	}
}
