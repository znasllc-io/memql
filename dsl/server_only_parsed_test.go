package dsl

import (
	"regexp"
	"sync"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// server_only_parsed_test.go -- memql#2875.
//
// The audit gates used to decide `@serverOnly` by REGEX over source, while the
// LOADER decides by PARSING and sets Function.ServerOnly. Those two can
// disagree, and the disagreement is FAIL-OPEN in the audit: a line beginning
// `@serverOnly` inside a multi-line annotation string, or inside a block
// comment opened on an `@`-line, satisfies the regex -- so the audit EXEMPTS
// the construct from per-row-authz classification and sdk/gen drops it from the
// client surface, while Function.ServerOnly stays false and NOTHING is enforced
// at runtime. #2861 (block comments in the DSL) made that more reachable.
//
// It is not attacker-reachable -- it requires authoring the construct in-repo.
// The reason to close it is that the classification test is the backstop that
// hard-fails on any new unclassified construct, and a construct that can slip
// out of that gate while ALSO having no runtime enforcement is the one shape
// the gate exists to make impossible.
//
// # This reads the PARSED tree, which is what the issue asked for
//
// An earlier attempt argued the audit could not read a parsed verdict from
// package `dsl` because `component/memql` imports `dsl`, so `dsl` cannot import
// back to load a function registry. The registry half of that is true and the
// conclusion was wrong: `component/memql/dslimports` is importable from here
// and ALREADY IS (dsl/filter_field_coverage_test.go, dsl/embed_test.go), and
// `dslimports.Load` hands back `Files map[path]*ast.File` -- the parsed AST for
// every file in the tree, in ~0.2s.
//
// The loader's rule is `hasFlagAttribute(def.Attributes, "serverOnly")`
// (component/memql/function_loader.go). serverOnlyConstructs applies exactly
// that rule to exactly the same parse, so "the gate exempted it" and "the
// runtime enforces it" cannot diverge by construction rather than merely
// failing a comparison. That is the difference between this and a parity test.

// serverOnlyKey identifies a construct for audit purposes. Name alone is not
// enough: two domains may declare the same construct name.
type serverOnlyKey struct {
	Path string
	Name string
}

var (
	serverOnlyOnce sync.Once
	serverOnlySet  map[serverOnlyKey]bool
	serverOnlyErr  error
	serverOnlySeen int
)

// serverOnlyConstructs returns the set of constructs carrying a REAL
// `@serverOnly` annotation, derived from the parsed AST rather than from source
// text.
//
// Cached: the parse is ~0.2s and several gates consult it.
func serverOnlyConstructs(t *testing.T) map[serverOnlyKey]bool {
	t.Helper()
	serverOnlyOnce.Do(func() {
		tree, err := dslimports.Load(Tree())
		if err != nil {
			// A tree that does not load is a different failure than a
			// divergence, and callers should say so rather than reporting an
			// empty @serverOnly set as "nothing is annotated".
			serverOnlyErr = err
			return
		}
		out := map[serverOnlyKey]bool{}
		seen := 0
		for path, file := range tree.Files {
			if file == nil {
				continue
			}
			for _, def := range file.Definitions {
				name, attrs, ok := declNameAndAttributes(def)
				if !ok {
					continue
				}
				seen++
				for _, a := range attrs {
					// Same rule as the loader's hasFlagAttribute: presence of
					// the attribute NAME. An `@serverOnly` inside a string
					// literal or a comment is not an attribute, so it never
					// appears here -- which is the whole point.
					if a != nil && a.Name == "serverOnly" {
						out[serverOnlyKey{Path: path, Name: name}] = true
					}
				}
			}
		}
		serverOnlySet = out
		serverOnlySeen = seen
	})
	if serverOnlyErr != nil {
		t.Fatalf("dslimports.Load: %v -- the tree did not parse, so the @serverOnly set cannot "+
			"be derived. Fix the parse failure; do not read this as 'no construct is annotated'.",
			serverOnlyErr)
	}
	if serverOnlySeen == 0 {
		t.Fatal("walked 0 named declarations -- every gate keyed on this set would then exempt " +
			"nothing and pass vacuously, which is the failure mode that made the previous " +
			"user-scope detector report a meaningless zero (#2799). Check declNameAndAttributes " +
			"against the AST definition types.")
	}
	return serverOnlySet
}

// declNameAndAttributes pulls the name + annotations off any named top-level
// declaration.
//
// Every kind is listed explicitly rather than reflected over, so a NEW
// declaration type is a compile-time hole that TestServerOnlyParsedSetCoversEveryDeclKind
// reports, instead of silently returning ok=false and quietly dropping a whole
// construct kind out of the audit.
func declNameAndAttributes(def languageAst.Node) (string, []*languageAst.Attribute, bool) {
	switch d := def.(type) {
	case *languageAst.FunctionDef:
		return d.Name, d.Attributes, true
	case *languageAst.ConceptDecl:
		return d.Name, d.Attributes, true
	case *languageAst.BuiltinDecl:
		return d.Name, d.Attributes, true
	default:
		return "", nil, false
	}
}

// TestServerOnlyParsedSetMatchesTheTree pins the derived set against the tree,
// so a change that empties or inflates it is visible.
func TestServerOnlyParsedSetMatchesTheTree(t *testing.T) {
	set := serverOnlyConstructs(t)
	if len(set) == 0 {
		t.Fatal("the parsed tree reports ZERO @serverOnly constructs. Either the annotation has " +
			"no live users -- in which case check whether the enforcement in " +
			"component/memql/function_validator.go is still exercised by anything -- or the " +
			"attribute name changed, which would silently exempt nothing and gate nothing.")
	}
	names := map[string]bool{}
	for k := range set {
		names[k.Name] = true
	}
	// These six are the live set as of memql#2883. The assertion is one-way:
	// each MUST be present. A new one appearing is fine and needs no edit here;
	// one DISAPPEARING means a construct silently lost its origin gate.
	for _, want := range []string{
		"activeUsers", "userByEmail", "userByIdSystem",
		"usersInDeletionCooldown", "usersScheduledForDeletion", "runningPlansForUser",
	} {
		if !names[want] {
			t.Errorf("%q no longer carries @serverOnly in the PARSED tree. If that is deliberate, "+
				"it needs a caller-scope filter instead -- each of these is server-only because "+
				"caller-scoping it is circular or because it is a sweep (#2800 / #2883).", want)
		}
	}
	t.Logf("parsed tree: %d @serverOnly construct(s) across %d named declarations", len(set), serverOnlySeen)
}

// TestServerOnlyParsedSetIgnoresSmuggledAnnotations is the failing-first half.
//
// It asserts the property that makes the parsed verdict better than the regex:
// an `@serverOnly` that is not an ANNOTATION -- because it sits inside a
// multi-line string, or inside a block comment -- must not appear in the set.
//
// Both fixtures are the REACHABLE spellings, verified by parsing them rather
// than by hand-reasoning. An earlier version of this test used
// "/*\n@serverOnly\n*/\nquery ..." which does not reach the audit at all: the
// preamble walk breaks on `*/`, so no gate ever saw it. The reachable
// block-comment form opens the comment on an `@`-line so it lands IN the
// preamble.
func TestServerOnlyParsedSetIgnoresSmuggledAnnotations(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		// POSITIVE CONTROL first. Without it every case below passes on a
		// parser that ignores annotations entirely, which is the tautology this
		// test would otherwise be.
		{
			"a real annotation IS honoured",
			"@serverOnly\nconcept probeRealAnnotation {\n  a string\n}\n",
			true,
		},
		{
			"inside a multi-line annotation string",
			"@description(\"note\n@serverOnly\")\nconcept probeSmuggledViaString {\n  a string\n}\n",
			false,
		},
		{
			"inside a block comment opened on an @-line",
			"@description(\"a\") /*\n@serverOnly */\nconcept probeSmuggledViaComment {\n  a string\n}\n",
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parseOneFileForAudit(tc.src)
			if err != nil {
				t.Fatalf("fixture did not parse (%v).\nA fixture that does not parse cannot "+
					"register, so it proves nothing about the audit -- fix the fixture rather "+
					"than deleting the case.", err)
			}

			var got bool
			var decls int
			for _, def := range file.Definitions {
				_, attrs, ok := declNameAndAttributes(def)
				if !ok {
					continue
				}
				decls++
				for _, a := range attrs {
					if a != nil && a.Name == "serverOnly" {
						got = true
					}
				}
			}
			if decls != 1 {
				t.Fatalf("fixture yielded %d named declarations, want 1 -- the case is not "+
					"measuring what it says", decls)
			}
			if got != tc.want {
				t.Errorf("parsed @serverOnly = %v, want %v (%s)", got, tc.want, tc.name)
			}

			// The CONTRAST that justifies the parsed verdict: for the smuggled
			// shapes the old source regex says YES where the parser says no.
			// That gap is the fail-open the issue reported -- the audit exempts
			// the construct while nothing enforces it at runtime.
			byRegex := smuggledServerOnlyRe.MatchString(tc.src)
			if !tc.want && !byRegex {
				t.Errorf("the source regex does NOT match %s either, so this case demonstrates "+
					"no divergence and guards nothing. Either the shape stopped being reachable "+
					"or the fixture is wrong -- check before deleting the case.", tc.name)
			}
		})
	}
}

// smuggledServerOnlyRe is the pattern the audit gates used before this change.
// It exists only so the tests above can show the DIVERGENCE the parsed verdict
// removes; nothing in the audit path consults it.
var smuggledServerOnlyRe = regexp.MustCompile(`(?m)^@serverOnly\b`)

// parseOneFileForAudit parses a fixture through the same entry point the tree
// load uses, so a fixture cannot pass on a laxer path than production.
func parseOneFileForAudit(src string) (*languageAst.File, error) {
	return languageParser.ParseFile(src)
}
