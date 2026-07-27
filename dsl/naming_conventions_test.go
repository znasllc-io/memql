package dsl

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// naming_conventions_test.go -- memql#2853.
//
// THE DECISION: constructs are named for what they do, never for what kind
// they are. No query* / mutation* / logic* / spec* / trait* / seed* prefix.
// docs/public/language/naming-conventions.md is the normative statement; this
// is its enforcement.
//
// WHY IT NEEDS ENFORCING AT ALL. The docs mandated the OPPOSITE rule for
// months -- "Use kind-specific prefixes so intent is obvious" -- while 0 of
// 1081 shipped declarations followed it. Nothing noticed, because a convention
// stated only in prose is checked by nobody. #2806 found the spec/trait half
// was worse than aspirational: its examples named constructs that do not exist
// (traitIsActiveRecord, specIsHumanParticipant), so a reader copying them wrote
// a `use` import that silently fails to resolve until the query first runs
// (#2783).
//
// So the fix for "the docs and the tree disagreed" is not only to correct the
// docs. It is to make the corpus the thing that fails when they diverge again.
//
// The keyword already marks the kind one token before the name -- `query user
// userById { ... }` -- so a prefix restates the grammar. That is the whole
// argument, and it is why abandoning the prefix beat renaming the corpus plus
// every call site and the generated SDK surface.
//
// WHY THIS GATE READS TOKENS AND NOT A REGEX. The first version of this file
// matched declarations with
//
//	^(query|mutate|logic|spec|trait|seed)[ \t]+(?:[A-Za-z_]\w*[ \t]+)?([A-Za-z_]\w*)[ \t]*\{
//
// and review found it was narrower than the grammar in four separate ways --
// the same class of defect the gate exists to prevent, in the gate itself:
//
//  1. `-` is a legal identifier character (parser.isIdentifierCharNoColon
//     returns `... || ch == '-'`), and 160 of the tree's 185 seeds use it
//     (`seed skill workbench-baseline`, pinned deliberately by
//     parser/seed_decl_test.go). `\w` excludes `-`, so those 160 declarations
//     were never scanned -- a prefixed one among them would have passed green,
//     and the `total == 0` backstop cannot see a PARTIAL miss.
//  2. `^` pinned the keyword to column 0, but the parser's own headers allow
//     leading whitespace (rewriter.go queryStructHeader / mutationStructHeader
//     are `^[ \t]*`), and the token-dispatched kinds are whitespace-insensitive
//     entirely. An indented prefixed construct loads for real and was invisible.
//  3. It covered 6 of the 16 declaration keywords while the doc claimed the
//     gate "fails if any construct is named with its own kind as a prefix".
//  4. It scanned raw bytes, so a brace inside a string literal or a
//     commented-out "what NOT to write" example counted as real syntax.
//
// Reading the lexer's token stream removes all four by construction rather than
// by patching the pattern: the lexer is what actually defines an identifier, it
// discards comments, it emits strings as single tokens, and brace depth comes
// from real TokenBraceOpen/Close rather than from character matching. A gate
// that re-implements the grammar will keep drifting from it; one that reuses the
// grammar cannot.

// declKeywordPrefixes maps every declaration keyword to the prefix that is now
// forbidden on its declarations.
//
// Derived from parser.TopLevelDeclKeywords (the parser's own authoritative
// dispatch table, so a new construct keyword extends this gate automatically)
// plus the four struct forms the rewriter lowers before the token dispatch ever
// sees them -- query / mutate / logic / automation.
//
// `mutate` is the keyword but `mutation` was the documented prefix, which is
// exactly the sort of mismatch that makes a prose rule hard to follow.
var declKeywordPrefixes = func() map[string]string {
	m := map[string]string{}
	for _, kw := range languageParser.TopLevelDeclKeywords {
		m[kw] = kw
	}
	for _, kw := range []string{"query", "logic", "automation"} {
		m[kw] = kw
	}
	m["mutate"] = "mutation"
	return m
}()

// declaration is one top-level construct declaration found in the tree.
type declaration struct {
	path    string
	keyword string
	name    string
}

// TestNoKindPrefixInConstructNames is the gate.
func TestNoKindPrefixInConstructNames(t *testing.T) {
	decls := scanShippedDeclarations(t)
	if len(decls) == 0 {
		t.Fatal("no constructs were scanned, so this test asserts nothing -- the token scan or " +
			"the file walk has broken")
	}

	var offenders []string
	perKeyword := map[string]int{}
	for _, d := range decls {
		perKeyword[d.keyword]++
		if hasKindPrefix(d.keyword, d.name) {
			offenders = append(offenders, fmt.Sprintf("%s: %s %s", d.path, d.keyword, d.name))
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d construct(s) carry a kind prefix:\n  %s\n\n"+
			"Constructs are named for what they DO, not what kind they are (memql#2853). The "+
			"keyword already marks the kind one token before the name -- `query user userById` -- "+
			"so the prefix restates the grammar. Drop it: queryActiveSpaces -> activeSpaces, "+
			"mutationCreateSpace -> createSpace.\n\n"+
			"See docs/public/language/naming-conventions.md.",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	kinds := make([]string, 0, len(perKeyword))
	for k := range perKeyword {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", k, perKeyword[k]))
	}
	t.Logf("scanned %d declarations across %d kinds (%s), %d prefixed",
		len(decls), len(perKeyword), strings.Join(parts, " "), len(offenders))
}

// TestNoKindPrefixGateIsLive proves the gate actually fires, on exactly the
// shapes the regex version silently skipped. Without this, "0 prefixed" is
// indistinguishable from "scanned nothing" -- which is what the hyphen and
// indentation blind spots really were.
func TestNoKindPrefixGateIsLive(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    string
		wantHit bool
	}{
		{
			name:    "plain prefixed query",
			src:     "query user queryFooBar {\n  filter id == args.id\n}\n",
			want:    "queryFooBar",
			wantHit: true,
		},
		{
			name:    "hyphenated prefixed seed -- regex version missed this entirely",
			src:     "seed skill seedWorkbench-baseline {\n  title: \"x\"\n}\n",
			want:    "seedWorkbench-baseline",
			wantHit: true,
		},
		{
			name:    "indented prefixed mutation -- regex version anchored at column 0",
			src:     "  mutate user mutationArchiveUser {\n  update { id: args.id }\n}\n",
			want:    "mutationArchiveUser",
			wantHit: true,
		},
		{
			name:    "prefixed shape -- keyword the regex version did not cover",
			src:     "shape space shapeSpaceCard {\n  row.id\n}\n",
			want:    "shapeSpaceCard",
			wantHit: true,
		},
		{
			name:    "unprefixed name is fine",
			src:     "query user userById {\n  filter id == args.id\n}\n",
			wantHit: false,
		},
		{
			name:    "prefix must be followed by uppercase -- `queryable` is a fine name",
			src:     "query user queryable {\n  filter id == args.id\n}\n",
			wantHit: false,
		},
		{
			name:    "commented-out example must not be reported",
			src:     "// query user queryFooBar {\n/// mutate user mutationFooBar {\nquery user userById {\n  filter id == args.id\n}\n",
			wantHit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decls, err := declarationsIn("probe.memql", tc.src)
			if err != nil {
				t.Fatalf("lex probe source: %v", err)
			}
			var hits []string
			for _, d := range decls {
				if hasKindPrefix(d.keyword, d.name) {
					hits = append(hits, d.name)
				}
			}
			if tc.wantHit {
				if len(hits) != 1 || hits[0] != tc.want {
					t.Fatalf("gate must flag %q, got %v (scanned %d declarations)", tc.want, hits, len(decls))
				}
				return
			}
			if len(hits) > 0 {
				t.Fatalf("gate must not flag anything here, got %v", hits)
			}
			if len(decls) == 0 {
				t.Fatal("scanned zero declarations, so this case proves nothing")
			}
		})
	}
}

// hasKindPrefix reports whether name carries keyword's forbidden prefix. A name
// is prefixed only if the next character is uppercase -- `queryable` is a fine
// query name, `queryUserById` is not.
func hasKindPrefix(keyword, name string) bool {
	prefix, ok := declKeywordPrefixes[keyword]
	if !ok {
		return false
	}
	return len(name) > len(prefix) && strings.HasPrefix(name, prefix) &&
		name[len(prefix)] >= 'A' && name[len(prefix)] <= 'Z'
}

// scanShippedDeclarations returns every top-level declaration in the tree the
// engine actually embeds.
//
// _reference/ is excluded: those are authoring skeletons, not shipped
// constructs, and they are the one place a placeholder name is legitimate.
func scanShippedDeclarations(t *testing.T) []declaration {
	t.Helper()
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("walking the DSL tree: %v", err)
	}
	var out []declaration
	for _, p := range paths {
		if strings.HasPrefix(p, "_") || strings.Contains(p, "/_") {
			continue
		}
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		decls, scanErr := declarationsIn(p, string(raw))
		if scanErr != nil {
			t.Fatalf("lex %s: %v", p, scanErr)
		}
		out = append(out, decls...)
	}
	return out
}

// declarationsIn lexes one .memql source and returns its top-level declarations.
//
// A declaration is `<keyword> <name> {` or, for the signature-bound kinds,
// `<keyword> <Concept> <name> {` -- so the NAME is the last identifier before
// the brace. Only depth 0 counts, which is what keeps `args { ... }` blocks and
// nested bodies out.
func declarationsIn(path, src string) ([]declaration, error) {
	tokens, err := languageParser.NewLexer(src).Tokenize()
	if err != nil {
		return nil, err
	}
	var out []declaration
	depth := 0
	for i := 0; i < len(tokens); i++ {
		switch tokens[i].Type {
		case languageParser.TokenBraceOpen:
			depth++
			continue
		case languageParser.TokenBraceClose:
			depth--
			continue
		}
		if depth != 0 || tokens[i].Type != languageParser.TokenIdentifier {
			continue
		}
		if _, ok := declKeywordPrefixes[tokens[i].Literal]; !ok {
			continue
		}
		name, skip := "", 0
		switch {
		case i+2 < len(tokens) &&
			tokens[i+1].Type == languageParser.TokenIdentifier &&
			tokens[i+2].Type == languageParser.TokenBraceOpen:
			name, skip = tokens[i+1].Literal, 1
		case i+3 < len(tokens) &&
			tokens[i+1].Type == languageParser.TokenIdentifier &&
			tokens[i+2].Type == languageParser.TokenIdentifier &&
			tokens[i+3].Type == languageParser.TokenBraceOpen:
			name, skip = tokens[i+2].Literal, 2
		}
		if name == "" {
			continue
		}
		out = append(out, declaration{path: path, keyword: tokens[i].Literal, name: name})
		i += skip
	}
	return out, nil
}

// TestNamingDocsDoNotMandateAPrefix stops the retired rule being reinstated in
// prose while the corpus quietly keeps not following it -- which is the exact
// state #2853 found, sustained for months.
//
// Deliberately narrow: it looks for the specific sentence shapes that MANDATE a
// prefix, not for any mention of one. Both files must be able to discuss the
// history, and naming-conventions.md does.
func TestNamingDocsDoNotMandateAPrefix(t *testing.T) {
	banned := []struct{ pattern, why string }{
		{`Use kind-specific prefixes`, "the retired mandate, verbatim"},
		{`Function names use kind prefixes`, "the retired mandate, verbatim"},
		{"- Queries: `query*`", "a normative bullet requiring the prefix"},
		{"- Query: `query*`", "a normative bullet requiring the prefix"},
		{"- Mutations: `mutation*`", "a normative bullet requiring the prefix"},
		{"- Mutation: `mutation*`", "a normative bullet requiring the prefix"},
		{`The compiler emits naming diagnostics`, "false: the naming lint was retired in epic #2031"},
		{"carries the `query` / `mutation` prefix", "a normative mandate in memql.md"},
		{"->   mutationCreate<ConceptName>", "false: the seed materializer builds create<Concept>, not mutationCreate<Concept>"},
	}

	// Every page that STATES the rule. The first version scanned only the two
	// obvious ones and missed two live mandates -- memql.md's "the declaration
	// name carries the query / mutation prefix" and concept-seeding.md's
	// mutationCreate<ConceptName> -- which an exhaustive sweep found.
	for _, doc := range []string{
		"../docs/public/language/naming-conventions.md",
		"../docs/public/language/functions.md",
		"../docs/public/language/memql.md",
		"../docs/public/concepts/concept-seeding.md",
	} {
		src := readDocForNamingTest(t, doc)
		for _, b := range banned {
			if strings.Contains(src, b.pattern) {
				t.Errorf("%s contains %q -- %s.\n\nThe prefix convention was abandoned in "+
					"memql#2853 after measuring 0 of 1081 shipped declarations following it. If the "+
					"decision is being reversed, that is a corpus rename plus "+
					"TestNoKindPrefixInConstructNames, not a doc edit.", doc, b.pattern, b.why)
			}
		}
	}
}

func readDocForNamingTest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
