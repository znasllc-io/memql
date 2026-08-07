package dslconformance

import (
	"fmt"
	"github.com/znasllc-io/memql/dsl"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
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
// 1091 shipped declarations followed it. Nothing noticed, because a convention
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
//
// AND THEN THE TOKEN VERSION WAS NARROWER THAN THE GRAMMAR TOO. Review of the
// rewrite found three more, which is the whole lesson of this file:
//
//  5. The TERSE single-step automation form (#2619) has NO BRACE --
//     `automation X @trigger(...) => logic X` -- and 10 of the tree's 31
//     automations use it. Anchoring on `{` had simply been carried over from
//     the regex without asking whether a declaration must have a body.
//  6. `mutate` was mapped to the single prefix `mutation`, so `mutateArchiveUser`
//     -- the keyword's own name -- passed. A keyword can forbid more than one
//     prefix.
//  7. The word boundary was an ASCII byte range and camelCase only, so a
//     kebab-case prefix (`seed-workbench-baseline`, the spelling 160 of the 185
//     seeds actually use) and a non-ASCII uppercase letter both evaded.
//
// AND THEN ROUND 3 FOUND THREE MORE -- this time in the guardrails around the
// scan rather than the scan itself:
//
//  8. The drift guard was a TAUTOLOGY. It asserted
//     len(declKeywordPrefixes) == len(TopLevelDeclKeywords)+len(rewriterLoweredKeywords)
//     while declKeywordPrefixes is BUILT by iterating exactly those two slices.
//     It could never fail, and had zero signal on the hand-maintained half it
//     existed to guard. Worse, that half did not need to be hand-maintained:
//     parser.StructFormKeywords was exported all along, and the comment saying
//     otherwise was asserted from a reading rather than a probe.
//  9. The DOC gate was inert for one of its five pages. authoring-rules.md had
//     just been ADDED to the list, and not one of the nine banned substrings
//     can occur in that page's shape. It was also blind to the
//     logic*/spec*/trait*/seed* half of the rule on ALL five pages -- a
//     blocklist of nine sentences catches nine sentences.
//  10. The `func (Receiver) NAME` form still LOADED. The rejection guard
//     (parser.RejectLegacyProceduralAuthorForm) was `^func \(` -- column 0,
//     one space -- while the slicer that extracts declarations allows leading
//     whitespace and flexible spacing. An indented `func (Logic) logicFoo(`
//     was rejected by nothing, registered by the loader, and invisible to
//     every gate in the repo. That is defect #2 above, one layer down.
//
// TestNoKindPrefixGateIsLive pins the seven scan shapes;
// TestNamingDocGateIsLive pins the doc gate's claim shapes; and
// parser/legacy_procedural_rejection_test.go pins the spellings from #10. Read
// them before changing anything here: "0 prefixed" is worthless without proof
// the gate can still say anything else.

// rewriterLoweredKeywords are the struct forms the rewriter lowers to the
// internal procedural shape BEFORE the parser's token dispatch ever sees them,
// so they are absent from TopLevelDeclKeywords and must be named here.
//
// Derived from parser.StructFormKeywords -- the rewriter's own list, built
// from structFormSteps so it cannot drift from the actual rewrite chain.
//
// Round 3 of this gate's review found this list hand-pinned, under a comment
// asserting "the parser exports no list for them". That was false, and it was
// asserted from a reading rather than a probe: StructFormKeywords has been
// exported at rewriter.go:557 the whole time, and is already the source of
// truth for the #2124 drift test. Hand-copying it reproduced, inside the drift
// guard itself, the exact defect this file exists to prevent.
var rewriterLoweredKeywords = languageParser.StructFormKeywords

// declKeywordPrefixes maps every declaration keyword to the prefixes now
// forbidden on its declarations.
//
// BOTH halves are derived from the parser: the token-dispatched twelve from
// parser.TopLevelDeclKeywords (its dispatch table) and the four rewriter-lowered
// struct forms from parser.StructFormKeywords (its rewrite chain). Adding a
// construct kind to either extends this gate automatically.
//
// `mutate` carries TWO forbidden prefixes. `mutation` was the documented one,
// but the keyword itself is `mutate`, so `mutateArchiveUser` is the same
// mistake and a map of one prefix per keyword let it through. Every other
// keyword forbids only itself.
var declKeywordPrefixes = func() map[string][]string {
	m := map[string][]string{}
	for _, kw := range languageParser.TopLevelDeclKeywords {
		m[kw] = []string{kw}
	}
	for _, kw := range rewriterLoweredKeywords {
		m[kw] = []string{kw}
	}
	m["mutate"] = []string{"mutate", "mutation"}
	return m
}()

// declKeywordsPinned is the declaration-keyword set this gate covers and
// docs/public/language/naming-conventions.md publishes.
//
// DERIVED vs PINNED, and why both. declKeywordPrefixes DERIVES its coverage
// from the parser, so a new kind is scanned automatically and the gate can
// never cover less than the language has. This list PINS the expected result,
// so the change is still noticed rather than absorbed silently. Deriving alone
// would leave the published count and the docs drifting unchallenged; pinning
// alone is what round 3 caught (a hand-copy masquerading as a source). A rename
// is the case a count alone misses -- `mutation` -> `mutate` actually happened
// (#2036), and would move no total.
var declKeywordsPinned = []string{
	"action", "automation", "builtin", "capability", "concept", "logic",
	"mutate", "policy", "prompt", "provider", "query", "seed", "shape",
	"spec", "tool", "trait",
}

// TestDeclKeywordSetMatchesTheParser is the drift guard on the keyword set.
//
// WHAT THE PREVIOUS VERSION GOT WRONG. It asserted
//
//	len(declKeywordPrefixes) == len(TopLevelDeclKeywords) + len(rewriterLoweredKeywords)
//
// which is a tautology: declKeywordPrefixes is BUILT by iterating exactly those
// two slices, so the equality holds by construction and the assertion could
// never fail. It had zero signal on the only half that was hand-maintained --
// the half it existed to guard. Round 3 review caught it.
//
// Both halves are derived from the parser now, so the remaining drift risk is
// the language gaining a kind neither list reports, or the docs publishing a
// count the gate no longer covers. Pinning the literal is what catches that:
// add a construct kind and this fails, forcing a deliberate doc update instead
// of the gate quietly covering less than it claims.
func TestDeclKeywordSetMatchesTheParser(t *testing.T) {
	for _, kw := range rewriterLoweredKeywords {
		for _, dispatched := range languageParser.TopLevelDeclKeywords {
			if kw == dispatched {
				t.Errorf("%q is in BOTH parser.StructFormKeywords and "+
					"parser.TopLevelDeclKeywords -- the map dedupes them, so the "+
					"covered-keyword count silently drops by one", kw)
			}
		}
	}
	covered := make([]string, 0, len(declKeywordPrefixes))
	for kw := range declKeywordPrefixes {
		covered = append(covered, kw)
	}
	sort.Strings(covered)
	pinned := append([]string(nil), declKeywordsPinned...)
	sort.Strings(pinned)
	if strings.Join(covered, " ") != strings.Join(pinned, " ") {
		t.Errorf("the parser's declaration keywords have changed.\n"+
			"  gate covers (derived): %s\n"+
			"  pinned expectation:    %s\n"+
			"%d dispatched via parser.TopLevelDeclKeywords + %d rewriter-lowered via "+
			"parser.StructFormKeywords. The gate already scans the new set; update "+
			"declKeywordsPinned AND the count published in "+
			"docs/public/language/naming-conventions.md to match.",
			strings.Join(covered, " "), strings.Join(pinned, " "),
			len(languageParser.TopLevelDeclKeywords), len(rewriterLoweredKeywords))
	}
}

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
			name:    "terse automation -- has no brace, so the token rewrite missed it too",
			src:     "automation automationPurgeThings @trigger(schedule=\"0 0 2 * * *\") => logic purgeThings\n",
			want:    "automationPurgeThings",
			wantHit: true,
		},
		{
			name:    "kebab-case prefix -- the spelling 160 of 185 seeds actually use",
			src:     "seed skill seed-workbench-baseline {\n  title: \"x\"\n}\n",
			want:    "seed-workbench-baseline",
			wantHit: true,
		},
		{
			name:    "the keyword itself as prefix -- `mutate`, not just the documented `mutation`",
			src:     "mutate user mutateArchiveUser {\n  update { id: args.id }\n}\n",
			want:    "mutateArchiveUser",
			wantHit: true,
		},
		{
			name:    "non-ASCII uppercase boundary -- the lexer admits unicode identifiers",
			src:     "query user queryÉlève {\n  filter id == args.id\n}\n",
			want:    "queryÉlève",
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
		{
			name: "a terse automation's `=> logic X` tail is a CALL SITE, not a declaration",
			src: "automation purgeThings @trigger(schedule=\"0 0 2 * * *\") => logic logicPurgeThings\n" +
				"query user userById {\n  filter id == args.id\n}\n",
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

// hasKindPrefix reports whether name carries any prefix forbidden for keyword.
//
// A name counts as prefixed only when the prefix ends at a WORD BOUNDARY, so
// `queryable` stays a fine query name while `queryUserById` is not. Two spellings
// of that boundary are recognised, because the tree uses both:
//
//   - camelCase -- `queryUserById`. Tested with unicode.IsUpper on the decoded
//     rune, not an ASCII byte range: the lexer admits unicode identifiers, so a
//     byte test lets a non-ASCII uppercase letter through.
//   - kebab-case -- `seed-workbench-baseline`. 160 of the 185 seeds are
//     kebab-named, so this is the likeliest place a prefixed name would actually
//     land, and a camelCase-only boundary check would scan it and then pass it.
func hasKindPrefix(keyword, name string) bool {
	for _, prefix := range declKeywordPrefixes[keyword] {
		if len(name) <= len(prefix) || !strings.HasPrefix(name, prefix) {
			continue
		}
		r, _ := utf8.DecodeRuneInString(name[len(prefix):])
		if unicode.IsUpper(r) || r == '-' {
			return true
		}
	}
	return false
}

// scanShippedDeclarations returns every top-level declaration in the tree the
// engine actually embeds.
//
// _reference/ is excluded: those are authoring skeletons, not shipped
// constructs, and they are the one place a placeholder name is legitimate.
func scanShippedDeclarations(t *testing.T) []declaration {
	t.Helper()
	tree := dsl.Tree()
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

		// The TERSE single-step automation form (#2619) has NO BRACE at all:
		//
		//	automation purgeExpiredOutputScreenings @trigger(schedule="...") => logic purgeExpiredOutputScreenings
		//
		// 10 of the tree's 31 automations are written this way, and a
		// brace-anchored scan cannot see any of them -- the same "gate narrower
		// than the grammar" defect this file was rewritten to eliminate, missed
		// once more because the rewrite reasoned about identifiers and whitespace
		// and never asked whether a declaration must have a body.
		//
		// The trailing `=> logic <name>` is a CALL SITE, not a declaration, and is
		// correctly not counted: a logic declaration requires a brace, so the
		// `logic` keyword there matches no arm above.
		case tokens[i].Literal == "automation" &&
			i+2 < len(tokens) &&
			tokens[i+1].Type == languageParser.TokenIdentifier &&
			tokens[i+2].Type == languageParser.TokenAt:
			name, skip = tokens[i+1].Literal, 1
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
		"docs/public/language/naming-conventions.md",
		"docs/public/language/functions.md",
		"docs/public/language/memql.md",
		"docs/public/concepts/concept-seeding.md",
		// authoring-rules.md §14 states the rule too, and was missed by the
		// first two versions of this list. It was still teaching the retired
		// prefix AND the retired `mutation` declaration keyword, while claiming
		// "Enforcement: none on the spelling" -- which had stopped being true.
		"docs/public/language/authoring-rules.md",
	} {
		src := readDocForNamingTest(t, doc)
		for _, b := range banned {
			if strings.Contains(src, b.pattern) {
				t.Errorf("%s contains %q -- %s.\n\nThe prefix convention was abandoned in "+
					"memql#2853 after measuring 0 of 1091 shipped declarations following it. If the "+
					"decision is being reversed, that is a corpus rename plus "+
					"TestNoKindPrefixInConstructNames, not a doc edit.", doc, b.pattern, b.why)
			}
		}
	}
}

// readDocForNamingTest reads a REPO-relative doc path. These paths were
// `dsl/`-relative (`../docs/...`) while this suite lived in `dsl/`; memql#3242
// moved the suite to the root module, so they resolve through repoPath now.
func readDocForNamingTest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(repoPath(t, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// namingDocPages are the pages both doc tests cover. Shared so the two cannot
// drift apart -- the substring test above catches a mandate stated in PROSE,
// this list feeds the stronger check below.
var namingDocPages = []string{
	"docs/public/language/naming-conventions.md",
	"docs/public/language/functions.md",
	"docs/public/language/memql.md",
	"docs/public/concepts/concept-seeding.md",
	"docs/public/language/authoring-rules.md",
}

// docImportLine matches a file-top import: `use cognition.queries.{ a, b }`.
var docImportLine = regexp.MustCompile(`(?m)^\s*use\s+[a-zA-Z0-9_.]+\.\{([^}]*)\}`)

// docLocatedClaim matches prose asserting a construct exists at a path:
// "`queryStaleClusterNodes` in `dsl/cluster/queries.memql`".
var docLocatedClaim = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_-]*)`\\s+in\\s+`(dsl/[A-Za-z0-9_/-]+\\.memql)`")

// docsCitingNonexistentConstructs is the deliberate-exception list: names the
// prose cites precisely BECAUSE they do not exist.
//
// naming-conventions.md's whole argument rests on #2806, where the docs named
// traitIsActiveRecord / specIsHumanParticipant and a reader copying them got an
// import that silently failed. Naming them is the point. mutationCreateCanvasState
// is cited as supplied by a product bundle at runtime, not declared in this tree.
var docsCitingNonexistentConstructs = map[string]bool{
	"traitIsActiveRecord":       true,
	"specIsHumanParticipant":    true,
	"mutationCreateCanvasState": true,
	"mutateArchiveUser":         true, // §"7 evasion shapes", an offender by construction
	"queryFoo":                  true, // legacy-procedural-form example
	"queryBar":                  true, // ditto
	"mutationCreateFoo":         true, // concept-seeding's <ConceptName> illustration
	"actionTYPO":                true, // an explicit "this is wrong" example
}

// TestNamingDocsNameOnlyConstructsThatExist is the strong half of the doc gate.
//
// WHY THE SUBSTRING LIST ABOVE IS NOT ENOUGH. Round 3 review found it inert for
// authoring-rules.md -- the page had just been ADDED to the list, and not one of
// its nine banned patterns can occur in that page's shape (a file-map table and
// §14 prose). It was also blind to the logic* / spec* / trait* / seed* half of
// the rule on all five pages. A blocklist of nine sentences only catches the
// nine sentences someone already thought of.
//
// So this checks the thing that actually harms a reader, against ground truth
// instead of a wordlist: a page must not CLAIM a construct exists when it does
// not. That is the #2783 / #2806 failure mode this PR cites as its own
// justification -- copy the name, write the `use` import, and it silently fails
// to resolve until the query first runs. Three claim shapes are checked:
//
//	A. a `use ns.kind.{ name }` import          -- the exact copy-paste path
//	B. "`name` in `dsl/<ns>/<file>.memql`"      -- prose asserting a location
//	C. a declaration header inside a memql fence -- teaching by example
//
// C is what makes the logic/spec/trait/seed half enforceable: a reinstated
// mandate has to show an example, and a kind-prefixed example fails here.
func TestNamingDocsNameOnlyConstructsThatExist(t *testing.T) {
	exists := map[string]bool{}
	for _, d := range scanShippedDeclarations(t) {
		exists[d.name] = true
	}

	for _, doc := range namingDocPages {
		for _, v := range namingDocViolations(doc, readDocForNamingTest(t, doc), exists) {
			t.Error(v)
		}
	}
}

// namingDocViolations is the pure core of the doc gate: it takes one page's
// source plus the set of real declaration names and returns every violation.
//
// Split out from the test so TestNamingDocGateIsLive can drive it with
// synthetic pages. A gate nobody has watched FIRE is indistinguishable from a
// gate that scans nothing -- which is exactly what round 3 found the substring
// list to be for authoring-rules.md.
func namingDocViolations(doc, src string, exists map[string]bool) []string {
	var out []string

	cite := func(name, shape string) {
		if exists[name] || docsCitingNonexistentConstructs[name] {
			return
		}
		// Only KIND-PREFIXED citations are in scope here. A doc may legitimately
		// name a construct this tree does not declare -- `space`, `partition`,
		// `context` and friends are product-DSL concepts mounted at runtime via
		// MEMQL_DSL_PATH, not engine declarations. Those are real doc debt but a
		// different question, tracked in #2914. What THIS PR is responsible for is
		// the prefixed spelling: a name that is prefixed AND absent is a leftover
		// from the retired convention, and it is the shape that silently fails to
		// resolve for a reader who copies it.
		if !hasAnyKindPrefix(name) {
			return
		}
		out = append(out, fmt.Sprintf("%s cites %q (%s), which is not a declaration "+
			"in the tree.\n\nA reader who copies it writes an import that silently fails "+
			"to resolve until the construct is first called -- the #2783 / #2806 failure "+
			"mode memql#2853 exists to stop. Use the real name, or add it to "+
			"docsCitingNonexistentConstructs if the point is that it does NOT exist.",
			doc, name, shape))
	}

	// A. import lists
	for _, m := range docImportLine.FindAllStringSubmatch(src, -1) {
		for _, raw := range strings.Split(m[1], ",") {
			if name := strings.TrimSpace(raw); name != "" {
				cite(name, "a `use` import")
			}
		}
	}

	// B. prose asserting a construct lives at a path
	for _, m := range docLocatedClaim.FindAllStringSubmatch(src, -1) {
		cite(m[1], "claimed live in "+m[2])
	}

	// C. declaration headers inside memql fences. Reuses the corpus scanner, so
	// the doc and the tree are held to ONE definition of what a declaration is
	// and what a forbidden prefix is.
	for _, fence := range memqlFencesIn(src) {
		decls, err := declarationsIn(doc, fence)
		if err != nil {
			continue // prose fences are not required to parse
		}
		for _, d := range decls {
			if hasKindPrefix(d.keyword, d.name) && !docsCitingNonexistentConstructs[d.name] {
				out = append(out, fmt.Sprintf("%s declares %s %q in an example -- a kind "+
					"prefix the corpus abandoned in memql#2853 (0 of 1091 declarations carry "+
					"one). Teaching it here is how the docs and the tree drifted apart for "+
					"months.", doc, d.keyword, d.name))
			}
		}
	}
	return out
}

// TestNamingDocGateIsLive proves the doc gate FIRES on each claim shape.
//
// The substring half of this gate shipped inert for a whole page and nobody
// noticed, because "no violations" reads identically to "scanned nothing". Every
// shape below must produce a violation, and the negative cases must not.
func TestNamingDocGateIsLive(t *testing.T) {
	exists := map[string]bool{"staleClusterNodes": true, "space": true}

	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		// A -- the copy-paste path
		{"prefixed name in a use import", "use cluster.queries.{ queryStaleClusterNodes }", true},
		{"prefixed name among several", "use cluster.queries.{ space, queryStaleClusterNodes }", true},
		// B -- prose asserting a location
		{"prefixed name claimed live at a path",
			"see `queryStaleClusterNodes` in `dsl/cluster/queries.memql` for this", true},
		// C -- teaching by example, across the half the substring list was blind to
		{"prefixed query declaration in a fence",
			"```memql\nquery user queryUserById {\n  filter row.id == args.id\n}\n```", true},
		{"prefixed logic declaration in a fence",
			"```memql\nlogic logicBootstrapSession {\n  body { return true }\n}\n```", true},
		{"prefixed spec declaration in a fence",
			"```memql\nspec participant specIsGuest {\n  return isGuest == true\n}\n```", true},
		{"prefixed trait declaration in a fence",
			"```memql\ntrait traitIsActive {\n  return active == true\n}\n```", true},
		{"prefixed seed declaration in a fence",
			"```memql\nseed skill seedWorkbenchBaseline {\n  name: \"x\"\n}\n```", true},
		{"kebab-prefixed seed declaration in a fence",
			"```memql\nseed skill seed-workbench-baseline {\n  name: \"x\"\n}\n```", true},
		{"terse prefixed automation in a fence",
			"```memql\nautomation automationPurgeExpired @trigger(schedule=\"0 * * * * *\") => logic purgeExpired\n```", true},

		// Negatives -- must NOT fire
		{"real un-prefixed name in an import", "use cluster.queries.{ staleClusterNodes }", false},
		{"real un-prefixed name claimed live",
			"see `staleClusterNodes` in `dsl/cluster/queries.memql`", false},
		{"un-prefixed declaration in a fence",
			"```memql\nquery user userById {\n  filter row.id == args.id\n}\n```", false},
		{"absent but UN-prefixed name (product DSL, out of scope)",
			"use cognition.concepts.{ canvasState }", false},
		{"deliberate does-not-exist citation",
			"use common.traits.{ traitIsActiveRecord }", false},
		{"prefixed name only in prose, no location claim",
			"the retired spelling was `queryStaleClusterNodes` in older docs", false},
		{"a non-memql fence is not scanned",
			"```go\nquery := queryUserById\n```", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := namingDocViolations("synthetic.md", tc.src, exists)
			if tc.want && len(got) == 0 {
				t.Errorf("the doc gate did NOT fire on %q -- it is blind to this shape.\n"+
					"Source:\n%s", tc.name, tc.src)
			}
			if !tc.want && len(got) > 0 {
				t.Errorf("the doc gate fired on %q, which is legitimate.\nSource:\n%s\nGot: %v",
					tc.name, tc.src, got)
			}
		})
	}
}

// hasAnyKindPrefix reports whether a name carries ANY declaration keyword as a
// prefix. Used at citation sites, where the doc names a construct without
// necessarily stating its kind.
func hasAnyKindPrefix(name string) bool {
	for keyword := range declKeywordPrefixes {
		if hasKindPrefix(keyword, name) {
			return true
		}
	}
	return false
}

// memqlFencesIn returns the body of every ```memql fenced block.
func memqlFencesIn(src string) []string {
	var out []string
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```memql" {
			continue
		}
		var body []string
		for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
			body = append(body, lines[i])
		}
		out = append(out, strings.Join(body, "\n"))
	}
	return out
}
