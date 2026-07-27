package dsl

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// naming_conventions_test.go -- memql#2853.
//
// THE DECISION: constructs are named for what they do, never for what kind
// they are. No query* / mutation* / logic* / spec* / trait* / seed* prefix.
// docs/public/language/naming-conventions.md is the normative statement; this
// is its enforcement.
//
// WHY IT NEEDS ENFORCING AT ALL. The docs mandated the OPPOSITE rule for
// months -- "Use kind-specific prefixes so intent is obvious" -- while 0 of 506
// shipped constructs followed it. Nothing noticed, because a convention stated
// only in prose is checked by nobody. #2806 found the spec/trait half was worse
// than aspirational: its examples named constructs that do not exist
// (traitIsActiveRecord, specIsHumanParticipant), so a reader copying them wrote
// a `use` import that silently fails to resolve until the query first runs
// (#2783).
//
// So the fix for "the docs and the tree disagreed" is not only to correct the
// docs. It is to make the corpus the thing that fails when they diverge again.
//
// The keyword already marks the kind one token before the name -- `query user
// userById { ... }` -- so a prefix restates the grammar. That is the whole
// argument, and it is why abandoning the prefix beat renaming 445 constructs
// plus every call site and the generated SDK surface.

// constructHeader matches a struct-form declaration: `<keyword> [<Concept>]
// <name> {`. The optional middle identifier is the signature-bound concept
// (`query user userById`), so the NAME is the last identifier before the brace.
var constructHeader = regexp.MustCompile(`(?m)^(query|mutate|logic|spec|trait|seed)[ \t]+(?:[A-Za-z_]\w*[ \t]+)?([A-Za-z_]\w*)[ \t]*\{`)

// kindPrefixes maps a declaration keyword to the prefix that is now forbidden.
//
// `mutate` is the keyword but `mutation` was the documented prefix, which is
// exactly the sort of mismatch that makes a prose rule hard to follow.
var kindPrefixes = map[string]string{
	"query":  "query",
	"mutate": "mutation",
	"logic":  "logic",
	"spec":   "spec",
	"trait":  "trait",
	"seed":   "seed",
}

// TestNoKindPrefixInConstructNames is the gate.
func TestNoKindPrefixInConstructNames(t *testing.T) {
	var offenders []string
	total := 0

	walkShippedMemqlFiles(t, func(path, src string) {
		for _, m := range constructHeader.FindAllStringSubmatch(src, -1) {
			keyword, name := m[1], m[2]
			total++
			prefix := kindPrefixes[keyword]
			// A name is prefixed only if the next character is uppercase --
			// `queryable` is a fine query name, `queryUserById` is not.
			if len(name) > len(prefix) && strings.HasPrefix(name, prefix) &&
				name[len(prefix)] >= 'A' && name[len(prefix)] <= 'Z' {
				offenders = append(offenders, fmt.Sprintf("%s: %s %s", path, keyword, name))
			}
		}
	})

	if total == 0 {
		t.Fatal("no constructs were scanned, so this test asserts nothing -- the header regex or " +
			"the file walk has broken")
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

	t.Logf("scanned %d constructs, 0 prefixed", total)
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
					"memql#2853 after measuring 0 of 506 shipped constructs following it. If the "+
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

// walkShippedMemqlFiles visits every .memql file the engine actually embeds.
//
// _reference/ is excluded: those are authoring skeletons, not shipped
// constructs, and they are the one place a placeholder name is legitimate.
func walkShippedMemqlFiles(t *testing.T, visit func(path, src string)) {
	t.Helper()
	// embedFS is what actually ships, so gating on it means the check covers
	// exactly the constructs the engine loads.
	err := fs.WalkDir(embedFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// `path != "."` guards the ROOT: its Name() is ".", which matches
			// the dot-prefix skip and made this walk SkipDir the entire tree.
			// The total==0 assertion below is what caught that.
			if path != "." && (strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		b, err := fs.ReadFile(embedFS, path)
		if err != nil {
			return err
		}
		visit(path, string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the DSL tree: %v", err)
	}
}
