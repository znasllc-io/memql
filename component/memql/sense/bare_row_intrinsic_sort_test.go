package sense

import (
	"strings"
	"testing"
)

// TestScanBareRowIntrinsicSortKeys is the ordering half of memql#2786, the
// sibling of TestBareRowIntrinsicRuleFlagsFilters. The interesting cases are
// the ones the filter scanner never sees: keys live INSIDE string literals,
// and a literal is a key or a direction depending on its position.
func TestScanBareRowIntrinsicSortKeys(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // suggested `row.<Name>` replacements, in source order
	}{
		{"bare createdAt", "query widget q {\n  sort \"createdAt\", \"desc\"\n}\n", []string{"createdAt"}},
		{"bare id no direction", "query widget q {\n  sort \"id\"\n}\n", []string{"id"}},
		{"bare concept", "query widget q {\n  sort \"concept\", \"asc\"\n}\n", []string{"concept"}},
		{"case-insensitive", "query widget q {\n  sort \"CreatedAt\", \"desc\"\n}\n", []string{"createdAt"}},

		// Already namespaced -- the canonical form.
		{"row.createdAt", "query widget q {\n  sort \"row.createdAt\", \"desc\"\n}\n", nil},
		{"row.id", "query widget q {\n  sort \"row.id\"\n}\n", nil},

		// Payload properties are legitimately bare and must never flag --
		// dsl/ carries `sort "version", "desc"` today.
		{"payload version", "query widget q {\n  sort \"version\", \"desc\"\n}\n", nil},
		{"payload title", "query widget q {\n  sort \"title\"\n}\n", nil},
		{"payload-prefixed", "query widget q {\n  sort \"payload.title\", \"desc\"\n}\n", nil},
		{"payload prop ending in id", "query widget q {\n  sort \"threadId\", \"desc\"\n}\n", nil},

		// Key-vs-direction classification (mirrors parseSortFunction).
		{"multi-field both bare", "query widget q {\n  sort \"createdAt\", \"asc\", \"id\", \"desc\"\n}\n", []string{"createdAt", "id"}},
		{"direction is not a key", "query widget q {\n  sort \"version\", \"desc\", \"createdAt\", \"asc\"\n}\n", []string{"createdAt"}},
		{"omitted direction still advances", "query widget q {\n  sort \"version\", \"createdAt\"\n}\n", []string{"createdAt"}},

		// Not a sort clause.
		{"filter clause untouched", "query widget q {\n  filter name==\"createdAt\"\n}\n", nil},
		{"sortOrder does not open a clause", "query widget q {\n  sortOrder \"createdAt\", \"desc\"\n}\n", nil},

		// A construct FIELD named `sort` is an ordinary shape (it lets a
		// caller pick an ordering). Because this scanner reads literal
		// contents, a boundary-only opener check read its annotation literals
		// as sort keys and failed CI pointing inside an @enum. Requiring the
		// string literal the grammar demands is what excludes it.
		{"args field named sort", "query widget q {\n  args {\n    sort  string  @enum(\"createdAt\", \"title\")\n  }\n  filter status==\"a\"\n}\n", nil},
		{"concept field named sort", "concept widget {\n  sort  string  @default(\"createdAt\")\n}\n", nil},
		{"tool field named sort", "tool t {\n  sort  string  @enum(\"createdAt\", \"desc\")\n}\n", nil},

		// The rewriter opens the clause with a bare HasPrefix and no boundary,
		// so this IS a sort clause to the engine and must not slip the gate.
		{"no space before the literal", "query widget q {\n  sort\"createdAt\", \"desc\"\n}\n", []string{"createdAt"}},
		{"tab before the literal", "query widget q {\n  sort\t\"createdAt\", \"desc\"\n}\n", []string{"createdAt"}},
		// The rewriter separates keyword from args with unicode TrimSpace, so
		// these rewrite byte-identically to the ASCII-space form and must not
		// bypass the gate.
		{"nbsp before the literal", "query widget q {\n  sort \"createdAt\", \"desc\"\n}\n", []string{"createdAt"}},
		{"ideographic space before the literal", "query widget q {\n  sort　\"createdAt\", \"desc\"\n}\n", []string{"createdAt"}},
		{"narrow nbsp before the literal", "query widget q {\n  sort \"createdAt\", \"desc\"\n}\n", []string{"createdAt"}},

		// Comments are not authored code.
		{"line comment", "query widget q {\n  // sort \"createdAt\", \"desc\"\n}\n", nil},
		{"block comment", "query widget q {\n  /* sort \"createdAt\", \"desc\" */\n}\n", nil},
		{"trailing comment after a real clause", "query widget q {\n  sort \"createdAt\", \"desc\" // newest first\n}\n", []string{"createdAt"}},
		{"url literal does not truncate", "query widget q {\n  sort \"createdAt\", \"desc\" // see http://x\n}\n", []string{"createdAt"}},

		// An unbalanced quote does not parse, so there is no authored key.
		{"unterminated literal", "query widget q {\n  sort \"createdAt\n}\n", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := ScanBareRowIntrinsicSortKeys(tc.src)
			var got []string
			for _, h := range hits {
				got = append(got, h.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ScanBareRowIntrinsicSortKeys(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// TestScanBareRowIntrinsicSortKeysPosition pins the reported position at the
// key text inside the literal, not the quote and not the clause keyword, so a
// Cockpit diagnostic underlines exactly the word the author must change.
func TestScanBareRowIntrinsicSortKeysPosition(t *testing.T) {
	src := "query widget q {\n  sort \"createdAt\", \"desc\"\n}\n"
	hits := ScanBareRowIntrinsicSortKeys(src)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	// Line 2 is `  sort "createdAt", "desc"`; the key starts at rune column 9.
	if hits[0].Line != 2 || hits[0].Column != 9 {
		t.Errorf("hit at %d:%d, want 2:9", hits[0].Line, hits[0].Column)
	}
	if hits[0].Text != "createdAt" {
		t.Errorf("Text = %q, want %q", hits[0].Text, "createdAt")
	}
}

// TestBareRowIntrinsicSortKeyRule locks the edit-time half: the diagnostic
// names the replacement, anchors on the key so a quick-fix has something to
// replace, and stays a Warning because compileSortField still resolves bare
// keys.
func TestBareRowIntrinsicSortKeyRule(t *testing.T) {
	src := "query widget q {\n  sort \"createdAt\", \"desc\"\n}\n"
	got := bareRowIntrinsicSortKeyRule(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(got))
	}
	d := got[0]
	if !strings.Contains(d.Message, `"row.createdAt"`) {
		t.Errorf("message must name the replacement \"row.createdAt\", got: %s", d.Message)
	}
	if d.Severity != SeverityWarning {
		t.Errorf("Severity = %v, want Warning (the engine still resolves bare sort keys)", d.Severity)
	}
	if d.Code != "bare-row-intrinsic-sort-key" {
		t.Errorf("Code = %q, want bare-row-intrinsic-sort-key", d.Code)
	}
	if d.Range.Start.Line != 2 {
		t.Errorf("Line = %d, want 2", d.Range.Start.Line)
	}
	line := strings.Split(src, "\n")[1]
	if covered := line[d.Range.Start.Column-1 : d.Range.End.Column-1]; covered != "createdAt" {
		t.Errorf("range covers %q, want \"createdAt\"", covered)
	}
}

// TestBareRowIntrinsicSortKeyRuleRangeIsRunes -- Column is a RUNE column per
// the Sense contract, so Range.End must advance by runes. The key lookup
// trims like the parser does, and unicode.IsSpace covers NBSP, so a key
// spelled "<NBSP>createdAt" resolves as the intrinsic while Text keeps the
// NBSP: 11 bytes, 10 runes. A byte length there overruns the key and swallows
// the closing quote.
func TestBareRowIntrinsicSortKeyRuleRangeIsRunes(t *testing.T) {
	src := "query widget q {\n  sort \" createdAt\", \"desc\"\n}\n"
	got := bareRowIntrinsicSortKeyRule(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(got))
	}
	d := got[0]
	line := []rune(strings.Split(src, "\n")[1])
	covered := string(line[d.Range.Start.Column-1 : d.Range.End.Column-1])
	if covered != " createdAt" {
		t.Errorf("range covers %q, want the key exactly (no trailing quote)", covered)
	}
}

// TestBareRowIntrinsicSortKeyRuleQuietOnCanonical -- the migrated tree must
// produce no diagnostics, or the rule would flag its own canonical form.
func TestBareRowIntrinsicSortKeyRuleQuietOnCanonical(t *testing.T) {
	for _, src := range []string{
		"query widget q {\n  sort \"row.createdAt\", \"desc\"\n}\n",
		"query widget q {\n  sort \"version\", \"desc\"\n}\n",
	} {
		if got := bareRowIntrinsicSortKeyRule(src); len(got) != 0 {
			t.Errorf("expected no diagnostic for %q, got: %s", src, got[0].Message)
		}
	}
}

// TestScanBareRowIntrinsicSortKeysMultibyteColumn locks the rune-column
// contract against a multi-byte rune earlier on the line -- the same hazard
// the filter scanner documents, reachable here through a trailing comment or
// an adjacent key.
func TestScanBareRowIntrinsicSortKeysMultibyteColumn(t *testing.T) {
	// "prioritét" is 9 runes / 10 bytes; the second key must be reported in
	// RUNE columns.
	src := "query widget q {\n  sort \"prioritét\", \"desc\", \"createdAt\", \"desc\"\n}\n"
	hits := ScanBareRowIntrinsicSortKeys(src)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d (%+v)", len(hits), hits)
	}
	line := strings.Split(src, "\n")[1]
	wantCol := len([]rune(line[:strings.Index(line, "createdAt")])) + 1
	if hits[0].Column != wantCol {
		t.Errorf("Column = %d, want %d (rune column of the key)", hits[0].Column, wantCol)
	}
}
