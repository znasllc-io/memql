package sense

import (
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// clause_opener_separators_test.go -- memql#2863.
//
// isFilterClauseOpener required the keyword be followed by an ASCII space or
// tab, so four spellings the engine accepts scored ZERO hits and walked past
// both row-intrinsic gates -- TestFilterIntrinsicsUseRowNamespace (#2779) and
// TestSortKeysUseRowNamespace (#2786). The consequence is the same as #2817's:
// a bare filter intrinsic compiles to a different predicate than the author
// wrote, silently.
//
// # Why the fixtures are validated against the parser in the test itself
//
// Two ways to get this wrong bit me while writing it, and both are the reason
// clauseSeparators is probed rather than asserted from memory:
//
//  1. PROBING THE WRONG ENTRY POINT. `ParseFile` alone REJECTS every one of
//     these (`unexpected character ' '`), which reads as "the grammar
//     refuses them, so the scanner needs no change". It does not: a struct
//     query goes through `NormaliseQuerySource` FIRST, which rewrites them into
//     a parseable form. The pairing is what makes the verdict real.
//
//  2. FIXTURES THAT LOST THEIR NON-ASCII BYTES. My first pass wrote the
//     separators as literal characters through a shell heredoc, which silently
//     replaced them with ASCII spaces -- so the "NBSP" case was a duplicate
//     SPACE case and reported a hit, i.e. "already fixed". They are Go escape
//     sequences here for that reason, and TestClauseSeparatorFixturesAreNotAscii
//     asserts it.
//
// So each separator below is asserted to be (a) genuinely non-ASCII where it
// claims to be, (b) accepted by the engine, and only then (c) seen by the
// scanner. A separator that stops being legal DSL fails loudly instead of
// quietly widening the guard.

// clauseSeparators is every separator the engine admits between a clause keyword
// and its body -- NOT every separator the scanner recognises. `(` is admitted by
// the engine and deliberately not by the opener; see scannerSees.
var clauseSeparators = []struct {
	name string
	sep  string
	// sortLegal records whether the SORT clause admits this separator.
	sortLegal bool
	// scannerSees records whether the OPENER is expected to recognise it.
	//
	// `(` is the asymmetry, and it is deliberate. The engine accepts
	// `filter(...)`, but FOUR openers walk .memql text line by line -- this
	// one, dsl/conformance_test.go, component/language/pagination/checker.go,
	// and dslclause -- and teaching only some of them `(` puts them in
	// conflict: a compliant `filter(row.id==args.x && ownerUserId==actor.userId)`
	// passes this scanner and FAILS TestPerRowAuthzClassification, whose opener
	// does not see the clause and so cannot see the caller-check inside it.
	//
	// So `(` is banned from authored .memql by the tree-convention gate
	// dsl.TestClauseOpenerUsesAPlainSpace instead -- the same way #2817 closed
	// its sibling shape -- and every opener stays correct simultaneously.
	scannerSees bool
}{
	{"space", " ", true, true},
	{"tab", "\t", true, true},
	{"nbsp", " ", true, true},
	{"em space", " ", true, true},
	{"ideographic space", "　", true, true},
	{"open paren", "(", false, false},
}

// TestClauseSeparatorFixturesAreNotAscii guards the fixtures themselves.
//
// A separator that silently degrades to " " turns its case into a duplicate of
// the space case -- which PASSES, and reports the hole as closed. That is
// exactly how my first attempt concluded the unicode half was already fixed.
func TestClauseSeparatorFixturesAreNotAscii(t *testing.T) {
	for _, tc := range clauseSeparators {
		// Derived from the data, not the name: renaming a fixture used to send
		// the ASCII cases into the multi-byte assertion with a misleading
		// message about collapsed bytes.
		if len(tc.sep) == 1 {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.sep) < 2 {
				t.Fatalf("separator %q is %d byte(s) -- a multi-byte separator was expected. "+
					"If this collapsed to ASCII the case is a duplicate of the space case and "+
					"proves nothing (memql#2863).", tc.sep, len(tc.sep))
			}
			for _, r := range tc.sep {
				if r < 128 {
					t.Fatalf("separator %q contains the ASCII rune %q", tc.sep, r)
				}
			}
		})
	}
}

// engineAcceptsQuery reports whether the engine accepts `src` through the path a
// struct query really takes: NormaliseQuerySource, then ParseFile.
func engineAcceptsQuery(t *testing.T, src string) bool {
	t.Helper()
	normalised, err := languageParser.NormaliseQuerySource(src)
	if err != nil {
		return false
	}
	_, err = languageParser.ParseFile(normalised)
	return err == nil
}

func TestFilterClauseOpenerSeesEveryUnicodeSpaceSeparator(t *testing.T) {
	for _, tc := range clauseSeparators {
		t.Run(tc.name, func(t *testing.T) {
			src := "query w q {\n  filter" + tc.sep + "id == args.x"
			if tc.sep == "(" {
				src += ")"
			}
			src += "\n  shape s\n}\n"

			// The engine-acceptance probe uses the `row.`-namespaced form so it
			// tests the SEPARATOR, not the gate; `src` above is the BARE form,
			// which is what the scanner must flag. Both parse.
			legal := "query w q {\n  filter" + tc.sep + "row.id == args.x"
			if tc.sep == "(" {
				legal += ")"
			}
			legal += "\n  shape s\n}\n"
			if !engineAcceptsQuery(t, legal) {
				t.Fatalf("the engine no longer accepts `filter%q`. If that separator became "+
					"illegal DSL the scanner needs no case for it -- but confirm which, because "+
					"the other reading is that NormaliseQuerySource regressed.", tc.sep)
			}

			hits := len(ScanBareRowIntrinsics(src))
			switch {
			case tc.scannerSees && hits == 0:
				t.Errorf("`filter%q` scored ZERO hits on a BARE row intrinsic the engine accepts, "+
					"so it walks past TestFilterIntrinsicsUseRowNamespace (#2779) and "+
					"TestSortKeysUseRowNamespace (#2786) -- a bare filter intrinsic compiles to a "+
					"different predicate than the author wrote (memql#2863).", tc.sep)
			case !tc.scannerSees && hits != 0:
				t.Errorf("`filter%q` scored %d hit(s), but this separator is deliberately NOT "+
					"recognised by the opener -- it is banned from authored .memql by "+
					"dsl.TestClauseOpenerUsesAPlainSpace instead, so that all four openers stay "+
					"consistent. Recognising it here re-creates the conflict described on "+
					"clauseSeparators.", tc.sep, hits)
			}
		})
	}
}

// TestFilterClauseOpenerRejectsNonSeparators is the over-correction guard.
//
// Widening to "any unicode space" must not turn a longer identifier that merely
// STARTS with `filter` into a clause opener -- an args field named `filterMode`,
// or a payload property `filterable`, would otherwise have its line scanned as a
// predicate. That boundary is the documented reason dslclause.StartsWith exists
// rather than a bare HasPrefix: the parser itself would read `sortOrder ...` as a
// `sort` directive, a quirk deliberately not copied into the gates.
func TestFilterClauseOpenerRejectsNonSeparators(t *testing.T) {
	for _, trimmed := range []string{
		"filterMode string!",
		"filterable boolean",
		"filter_by string",
		"filters",
		"filtered==true",
		"notfilter x",
	} {
		if isFilterClauseOpener(trimmed) {
			t.Errorf("isFilterClauseOpener(%q) = true; only a unicode space or `(` may follow "+
				"the keyword, or an ordinary field whose name starts with `filter` is scanned "+
				"as a predicate", trimmed)
		}
	}
	// The bare keyword alone still matches, and that is deliberate even though
	// the ENGINE does not support it: `filter` with nothing after it normalises
	// to a body with no filter clause at all, silently dropping it. The opener
	// keeps matching so the scanner sees a construct an author plainly INTENDED
	// as a filter; scoring zero there would be the same blindness this issue is
	// about. (An earlier version of this comment claimed the body may continue on
	// the following line -- it may not, and the file header says so.)
	if !isFilterClauseOpener("filter") {
		t.Error(`isFilterClauseOpener("filter") = false; the bare keyword must still match`)
	}
}

// TestSortClauseOpenerDoesNotAcceptParen records the deliberate asymmetry, with
// the evidence, so nobody "fixes" the inconsistency later.
//
// #2863 asked whether `filter(` is legal DSL and proposed accepting `(` in both
// openers. Probed: `filter(...)` parses, `sort(...)` does not --
//
//	sort field must be a string literal (e.g. "createdAt") (got "(")
//
// -- so accepting `(` in the sort opener would make it match a shape the engine
// refuses, which is a false-positive generator, not a closed hole.
func TestSortClauseOpenerDoesNotAcceptParen(t *testing.T) {
	src := "query w q {\n  filter row.id == args.x\n  sort(\"createdAt\", \"desc\")\n  shape s\n}\n"
	if engineAcceptsQuery(t, src) {
		t.Fatal("the engine now ACCEPTS `sort(...)`. If that is deliberate, isSortClauseOpener " +
			"must learn `(` too -- this test is the record that it was rejected when the " +
			"asymmetry was introduced (memql#2863).")
	}
	if isSortClauseOpener(`sort("createdAt", "desc")`) {
		t.Error("isSortClauseOpener accepts `(`, which the engine refuses -- that generates " +
			"false positives on a shape no author can ship")
	}
}

// TestSortClauseOpenerSeesUnicodeSeparators pins what is ALREADY true, so the
// sort half cannot regress to ASCII while the filter half stays fixed. It was
// made unicode-aware by #2786's TrimLeftFunc(unicode.IsSpace).
func TestSortClauseOpenerSeesUnicodeSeparators(t *testing.T) {
	for _, tc := range clauseSeparators {
		if !tc.sortLegal {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			src := "query w q {\n  sort" + tc.sep + "\"createdAt\", \"desc\"\n  shape s\n}\n"
			if len(ScanBareRowIntrinsicSortKeys(src)) == 0 {
				t.Errorf("`sort%q` scored zero hits on a bare sort key -- a bare key orders on a "+
					"JSONB path no row carries, which is a silent no-op sort rather than an "+
					"error (#2786)", tc.sep)
			}
		})
	}
}
