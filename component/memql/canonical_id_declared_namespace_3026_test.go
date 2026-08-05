package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// canonical_id_declared_namespace_3026_test.go -- memql#3026.
//
// #2976 asked for the ambient check to test the concept's DECLARED NAMESPACE
// rather than its containing directory. What shipped in #3017 tested global
// UNIQUENESS instead, because the pin-aware namespace was believed unreachable
// from the loader. It is reachable -- component/memql already imports the dsl
// package -- so this replaces uniqueness with the rule that was asked for.
//
// The two rules are not interchangeable, and these tests are built to tell
// them apart. Under uniqueness:
//
//   - a concept declared ONLY in a foreign domain binds with no import, on the
//     path that derives row ids, which is what #2617's import discipline
//     existed to prevent;
//   - a namespace-remapped pack whose name is AMBIGUOUS tree-wide is still
//     deadlocked -- ambient refuses AND the import the error asks for is the
//     one TestNoSameDomainUse bans;
//   - a product bundle mounted at MEMQL_DSL_PATH can retroactively make this
//     tree's ambient reference ambiguous, so an unrelated repository's DSL
//     causes a boot failure here.
//
// The declared-namespace rule closes all three by construction.
//
// # Why these fixtures carry TWO concepts
//
// The #2976 fixture held a single concept, so every assertion resolved by
// uniqueness and a flat non-remapped fixture would have passed identically --
// nothing pinned namespace-awareness (annotated in that file during its own
// landing review). Ambiguity is exactly where the rules diverge, so the
// fixture has to contain a collision to prove anything.

// ambiguousRemappedResolver models a pack whose DIRECTORY (`zdeploy`) differs
// from its declared namespace (`zcluster`), where the concept short-name
// COLLIDES with one in another namespace.
//
// `widget` is ambiguous tree-wide, so uniqueness cannot resolve it and the
// directory hint filters every candidate to zero. Only the declared namespace
// disambiguates.
func ambiguousRemappedResolver() *ConceptResolver {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:zcluster:widget":  {Name: "v1:zcluster:widget"},
		"v1:elsewhere:widget": {Name: "v1:elsewhere:widget"},
	})
	return NewConceptResolver(registry)
}

// TestCanonicalId_RemappedAmbiguousNameResolvesByDeclaredNamespace is DoD
// item 4, and the headline: the fixture FAILS under the uniqueness rule and
// PASSES under the declared-namespace rule.
//
// This is the deadlock #2976's consistency requirement names -- "for any file,
// exactly one of 'ambient works' or 'an import is required' should be true".
// Under uniqueness NEITHER was: ambient refused, and both import spellings
// were unavailable.
func TestCanonicalId_RemappedAmbiguousNameResolvesByDeclaredNamespace(t *testing.T) {
	got, err := ambiguousRemappedResolver().ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, "zdeploy", "zcluster")
	if err != nil {
		t.Fatalf("an ambiguous name in a namespace-remapped pack did not resolve ambiently.\n"+
			"`widget` is ambiguous tree-wide, so the uniqueness rule cannot resolve it, and the "+
			"DIRECTORY hint (\"zdeploy\") filters every candidate to zero -- while the import the "+
			"resulting error asks for is the same-domain one TestNoSameDomainUse strips. That is "+
			"the deadlock memql#3026 exists to close: the DECLARED namespace (\"zcluster\") "+
			"disambiguates it, and uniqueness cannot by construction.\n  error: %v", err)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("resolved to the wrong concept -- the declared namespace must pick this pack's "+
			"own `widget`, not the colliding one in another namespace.\n  got: %s", got)
	}
	if strings.Contains(got, `"v1:elsewhere:widget"`) {
		t.Errorf("resolved to the FOREIGN concept. On the path that derives row ids, binding a "+
			"same-named concept from another namespace is a silent data defect.\n  got: %s", got)
	}
}

// TestCanonicalId_ForeignDomainConceptRequiresAnImport is DoD item 2.
//
// This INVERTS TestCanonicalId_CrossDomainUniqueNameMatchesSignatureBinding,
// deliberately and by the issue's own definition of done. That test documented
// the widening honestly rather than hiding it; #3026 is the decision to take
// it back.
//
// The reason is the blast radius, not tidiness: `canonicalId` derives row ids,
// and #2617's import discipline existed precisely so a cross-domain reference
// had to be written down. A bundle author writing `canonicalId(x, user)`
// meaning THEIR `user` should get a loud failure, not a silent bind to the
// engine's.
func TestCanonicalId_ForeignDomainConceptRequiresAnImport(t *testing.T) {
	r := ambiguousRemappedResolver()

	// A file in an unrelated domain naming a concept declared only elsewhere.
	// Unique or not, it is not in ambient scope here.
	_, err := r.ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, "somewhereElse", "somewhereElse")
	if err == nil {
		t.Fatal("a concept declared only in a FOREIGN namespace bound ambiently, with no import. " +
			"This is the path that derives row ids, so a silent cross-domain bind is a data " +
			"defect rather than a convenience -- #2617's import discipline exists so the " +
			"reference has to be written down (memql#3026 DoD item 2).")
	}
	if !strings.Contains(err.Error(), "widget") {
		t.Errorf("the error should name the offending concept so the fix is obvious, got: %v", err)
	}

	// ...and the explicit import is the working spelling.
	got, ierr := r.ResolveCanonicalIdConceptRefsInNamespace(
		"use zcluster.concepts.{ widget }\ncanonicalId(args.x, widget)", "somewhereElse", "somewhereElse")
	if ierr != nil {
		t.Fatalf("the explicit cross-namespace import must be the working spelling, or there is "+
			"no way to reference a foreign concept at all: %v", ierr)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("the import should bind the named namespace's concept.\n  got: %s", got)
	}
}

// TestCanonicalId_SameNamespaceStillResolvesAmbiently guards the direction
// this change could over-tighten.
//
// Narrowing the ambient rule risks the mirror of the defect it fixes: a rule
// too strict refuses shipped DSL and breaks the load. The remapped pack's own
// concept must still need no import.
func TestCanonicalId_SameNamespaceStillResolvesAmbiently(t *testing.T) {
	got, err := ambiguousRemappedResolver().ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, "zdeploy", "zcluster")
	if err != nil {
		t.Fatalf("a pack's OWN concept must resolve with no import -- requiring one would ask for "+
			"the same-domain spelling TestNoSameDomainUse bans, recreating the memql#2976 "+
			"deadlock from the other side: %v", err)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("wrong id.\n  got: %s", got)
	}
}

// TestCanonicalId_ScannerSkipsComments is DoD item 5.
//
// The scanner skips string literals but not comments, so `canonicalId(...)`
// written in a `//` comment was treated as a real call and the file's COMMENT
// TEXT was rewritten. PR #3017 papered over this with an authoring warning in
// dsl/deployment/mutations.memql ("Do NOT write the call form in a comment
// across a line break") instead of fixing the scanner.
//
// It matters more under any ambient rule, because a comment naming a
// resolvable concept silently mutates file text -- and it matters most for a
// comment naming an UNKNOWN concept, which turns prose into a hard load error.
func TestCanonicalId_ScannerSkipsComments(t *testing.T) {
	for name, src := range map[string]string{
		"line comment": "// canonicalId(args.x, widget)\ncanonicalId(args.x, \"v1:zcluster:widget\")",
		"doc comment":  "/// canonicalId(args.x, widget)\ncanonicalId(args.x, \"v1:zcluster:widget\")",
		"trailing":     "canonicalId(args.x, \"v1:zcluster:widget\") // canonicalId(args.x, widget)",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ambiguousRemappedResolver().
				ResolveCanonicalIdConceptRefsInNamespace(src, "zdeploy", "zcluster")
			if err != nil {
				t.Fatalf("a commented-out canonicalId must not be resolved at all: %v", err)
			}
			if got != src {
				t.Errorf("the scanner rewrote COMMENT TEXT. A comment is prose, not a call -- "+
					"rewriting it edits what the author wrote (memql#3026 DoD item 5).\n"+
					"  want: %q\n  got:  %q", src, got)
			}
		})
	}

	// The sharpest case: a comment naming a concept that does not exist must
	// stay prose rather than becoming a hard load error.
	unknown := "// canonicalId(args.x, thisConceptDoesNotExist)\n"
	got, err := ambiguousRemappedResolver().
		ResolveCanonicalIdConceptRefsInNamespace(unknown, "zdeploy", "zcluster")
	if err != nil {
		t.Fatalf("a comment naming an unknown concept became a LOAD ERROR -- prose cannot be "+
			"allowed to fail the build (memql#3026): %v", err)
	}
	if got != unknown {
		t.Errorf("comment text was rewritten.\n  want: %q\n  got:  %q", unknown, got)
	}

	// And the guard that keeps this honest: a `//` inside a STRING is not a
	// comment, so a real call after it must still resolve.
	withURL := "@description(\"see https://example.com\")\ncanonicalId(args.x, widget)"
	got, err = ambiguousRemappedResolver().
		ResolveCanonicalIdConceptRefsInNamespace(withURL, "zdeploy", "zcluster")
	if err != nil {
		t.Fatalf("a `//` inside a string literal must not start a comment -- treating it as one "+
			"would swallow the rest of the file, including real calls: %v", err)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("the real call after a string containing `//` must still resolve.\n  got: %s", got)
	}
}

// TestCanonicalId_DeclaredNamespaceForDomainReadsThePin pins the plumbing the
// issue called "the actual work".
//
// dsl/deployment is the live divergence: directory `deployment`, namespace.pin
// `cluster`, concepts assembling as v1:cluster:*. If this lookup regresses to
// returning the directory, the ambient rule silently reverts to the behaviour
// #2976 reported -- resolving against the wrong namespace -- so it is worth an
// assertion of its own rather than only being covered through the resolver.
func TestCanonicalId_DeclaredNamespaceForDomainReadsThePin(t *testing.T) {
	if got := declaredNamespaceForDomain("deployment"); got != "cluster" {
		t.Errorf("dsl/deployment/namespace.pin declares \"cluster\"; the declared namespace must "+
			"come from the pin, not the directory (#2614). got %q", got)
	}
	// A domain with no pin declares its own directory name.
	if got := declaredNamespaceForDomain("cognition"); got != "cognition" {
		t.Errorf("an unpinned domain's declared namespace is its directory. got %q", got)
	}
	// An unknown domain must not invent one.
	if got := declaredNamespaceForDomain("noSuchDomain"); got != "noSuchDomain" {
		t.Errorf("an unknown domain falls back to itself. got %q", got)
	}
}
