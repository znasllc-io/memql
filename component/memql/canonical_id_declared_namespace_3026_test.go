package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
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

// TestCanonicalId_AmbiguousRemappedPackAgreesWithTheSameDomainGate is DoD item
// 3, in full (memql#3026 landing review).
//
// The item is a conjunction -- "a remapped pack with an ambiguous name has a
// working spelling, AND TestNoSameDomainUse permits it" -- and it was covered
// as two disjoint halves in two files: the ambient half here against the
// two-concept fixture, the gate half in the 2976 file against the
// single-concept fixture that issue #3026 section 4 condemns as unable to tell
// the two rules apart. The conjunction is the thing that was broken, so the
// conjunction is what needs asserting, against the fixture that discriminates.
//
// #2976's consistency requirement is about the two MODES, not a count of
// spellings: "for any file, exactly one of 'ambient works' or 'an import is
// required' should be true, and the gate should permit whichever it is."
// Under uniqueness NEITHER was true here. Note that the cross-namespace import
// `use zcluster.concepts.{ widget }` also works and is NOT stripped -- that is
// deliberate and is asserted in TestCanonicalId_ForeignDomainConceptRequiresAnImport,
// so this test is named for the agreement it checks rather than for a
// uniqueness it does not.
func TestCanonicalId_AmbiguousRemappedPackAgreesWithTheSameDomainGate(t *testing.T) {
	const domain = "zdeploy"

	// 1. The ambient spelling works, so no import is needed.
	if _, err := ambiguousRemappedResolver().ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, domain, "zcluster"); err != nil {
		t.Fatalf("ambient resolution fails for an AMBIGUOUS name in a remapped pack, so an "+
			"import IS required -- and step 2 shows the only same-domain spelling is stripped. "+
			"That is the memql#2976 deadlock, which uniqueness could not close: %v", err)
	}

	// 2. And the SAME-DOMAIN import is unavailable: the gate strips it. (The
	//    cross-namespace `use zcluster.concepts.{ widget }` remains available
	//    and is asserted elsewhere -- this step is about the spelling the gate
	//    bans, not about a count.)
	withImport := []byte("use zdeploy.concepts.{ widget }\n\nmutate widget doThing {\n  insert {\n    id: args.x\n  }\n}\n")
	stripped, err := languageParser.RewriteSameDomainUse(domain, withImport)
	if err != nil {
		t.Fatalf("RewriteSameDomainUse: %v", err)
	}
	if string(stripped) == string(withImport) {
		t.Errorf("the gate no longer strips `use %s.concepts.{ widget }`, so the same-domain "+
			"import and the ambient form both work -- the mirror of the memql#2976 deadlock "+
			"where both failed. The ambient rule and the gate must agree.", domain)
	}

	// 3. And the gate positively PERMITS the working spelling: a file with no
	//    same-domain import passes through byte-identical. A gate that mangled
	//    it would leave the pack with no spelling at all again.
	ambient := []byte("mutate widget doThing {\n  insert {\n    id: canonicalId(args.x, widget)\n  }\n}\n")
	permitted, err := languageParser.RewriteSameDomainUse(domain, ambient)
	if err != nil {
		t.Fatalf("RewriteSameDomainUse on the ambient spelling: %v", err)
	}
	if string(permitted) != string(ambient) {
		t.Errorf("the gate rewrote the ONE working spelling.\n  want: %q\n  got:  %q",
			ambient, permitted)
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
// The fixture is remappedPackResolver, whose `widget` is UNIQUE, and that is
// load-bearing rather than incidental (memql#3026 landing review). This test
// was first written against ambiguousRemappedResolver, where `widget` is
// declared twice -- so the deleted uniqueness rule could not have bound it
// either, and the test passed identically with the fix reverted. A regression
// guard that passes against the regression is not a guard. Only a UNIQUE name
// distinguishes "unique, therefore ambient" from "foreign, therefore an import
// is required", which is the whole of DoD item 2.
func TestCanonicalId_ForeignDomainConceptRequiresAnImport(t *testing.T) {
	r := remappedPackResolver()

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

		// BLOCK comments too (memql#3026 landing review). The lexer has two
		// comment forms -- skipWhitespace dispatches to skipLineComment for
		// `//` and skipBlockComment for `/* */` -- so a scanner that skipped
		// only the line form left the identical defect alive in the other.
		"block comment":  "/* canonicalId(args.x, widget) */\ncanonicalId(args.x, \"v1:zcluster:widget\")",
		"block trailing": "canonicalId(args.x, \"v1:zcluster:widget\") /* canonicalId(args.x, widget) */",

		// The wrapped case specifically: this is what the authoring warning in
		// dsl/deployment/mutations.memql was about ("do not write the call
		// form in a comment across a line break"), and a block comment is the
		// likeliest place for it. Deleting that warning is only honest if this
		// passes.
		"block across a line break": "/*\n canonicalId(args.x,\n   widget)\n*/\ncanonicalId(args.x, \"v1:zcluster:widget\")",

		// An unterminated block runs to end of file, matching skipBlockComment.
		// The parser reports the unterminated comment; this scanner must not
		// rewrite the prose on the way there.
		"unterminated block": "/* canonicalId(args.x, widget)\n",
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

// TestCanonicalId_InDomainComposesTheRealPin is the composition test, and it is
// the one the rest of this file could not substitute for (memql#3026 landing
// review).
//
// Every other assertion here calls the three-argument InNamespace form with a
// hand-supplied declaredNS, which proves the RULE but not the WIRING. Reverting
// the single line that joins them -- InDomain passing declaredNamespaceForDomain
// rather than the bare directory -- left the entire suite green, so the pin
// could have been disconnected from the resolver and nothing would have said
// so. That is the composition this PR exists to create, so it gets an assertion
// through the production entry point, against the REAL dsl/deployment pin.
//
// The registry deliberately declares `deployment` twice. A unique name would
// resolve under the deleted uniqueness rule too, so ambiguity is what makes
// this bite in both directions at once.
func TestCanonicalId_InDomainComposesTheRealPin(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:cluster:deployment":   {Name: "v1:cluster:deployment"},
		"v1:elsewhere:deployment": {Name: "v1:elsewhere:deployment"},
	})

	got, err := NewConceptResolver(registry).
		ResolveCanonicalIdConceptRefsInDomain(`canonicalId(args.x, deployment)`, "deployment")
	if err != nil {
		t.Fatalf("the two-argument entry point -- the only one production calls -- did not "+
			"compose the pin with the ambient rule. dsl/deployment/namespace.pin declares "+
			"\"cluster\", so `deployment` is ambient here; resolving against the DIRECTORY "+
			"instead is exactly the memql#2976 behaviour this PR replaces: %v", err)
	}
	if !strings.Contains(got, `"v1:cluster:deployment"`) {
		t.Errorf("resolved against the wrong namespace.\n  got: %s", got)
	}
}

// TestCanonicalId_NestedFileUsesItsRootDomainsNamespace covers the layout the
// first cut of this fix broke (memql#3026 landing review).
//
// Boot assembles a concept id from the origin's FIRST path segment
// (unified_loader.go pass 1: `dir := firstPathSegment(p)`), so
// beta/sub/concepts.memql declares v1:beta:widget. DomainFromFilePath returns
// the LAST segment, "sub" -- not a namespace at all. Feeding that to the
// ambient rule refuses every same-domain reference in a nested file and then
// demands the same-domain import TestNoSameDomainUse bans: the memql#2976
// deadlock, one directory deeper. The tree has 23 nested .memql files today, so
// this is a live layout even though none of them currently writes canonicalId.
func TestCanonicalId_NestedFileUsesItsRootDomainsNamespace(t *testing.T) {
	// The pin belongs to the ROOT domain, and a nested file must still find it.
	if got := declaredNamespaceForOrigin("deployment/anything/mutations.memql"); got != "cluster" {
		t.Errorf("a nested file under a PINNED domain must declare that domain's namespace. "+
			"Resolving \"anything\" instead finds no pin and yields a directory name that no "+
			"concept id can ever carry. got %q, want \"cluster\"", got)
	}
	if got := declaredNamespaceForOrigin("deployment/mutations.memql"); got != "cluster" {
		t.Errorf("the flat case must be unchanged. got %q, want \"cluster\"", got)
	}
	// An unpinned root domain declares its own directory, nested or not.
	if got := declaredNamespaceForOrigin("agents/skills/categories.memql"); got != "agents" {
		t.Errorf("a nested file under an UNPINNED domain declares that domain. got %q, "+
			"want \"agents\"", got)
	}
	// Loader origin decorations must not leak into the namespace.
	if got := declaredNamespaceForOrigin("unified:deployment/mutations.memql:createDeployment"); got != "cluster" {
		t.Errorf("a unified-loader slice origin must strip to the same namespace. got %q, "+
			"want \"cluster\"", got)
	}

	// And through the LOADER, not just the helper. Asserting only the helper
	// would repeat the gap TestCanonicalId_InDomainComposesTheRealPin exists to
	// close: the rule right, the wiring unpinned. tryParseNewFunctionSyntax is
	// the seam function_loader.go resolves canonicalId in, and it is handed the
	// origin, so this fails if that call site ever goes back to deriving the
	// namespace from DomainFromFilePath's last segment.
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:cluster:deployment": {Name: "v1:cluster:deployment"},
	})
	const src = `@description("A nested file referencing its own domain's concept.")
mutate deployment nestedAmbientRef {
  args {
    x  string  @required
  }
  insert {
    id: canonicalId(args.x, deployment)
  }
}`
	// Asserted POSITIVELY -- `err == nil`, not "err is not the ambient error".
	// The conditional form passes vacuously on any unrelated failure, which is
	// the same "documents but does not pin" defect this round is fixing
	// elsewhere; the loader returns a nil error and a real function for this
	// source today, so the strict form costs nothing.
	fn, err := tryParseNewFunctionSyntax(
		"nestedAmbientRef", "mutation", src, "deployment/anything/mutations.memql", registry)
	if err != nil {
		t.Fatalf("the loader refused a nested file's reference to its OWN domain's concept. If "+
			"this is the \"neither imported nor a same-domain concept\" error, the import it "+
			"demands is the one TestNoSameDomainUse bans -- the memql#2976 deadlock reinstated "+
			"one directory deeper. The loader must derive the ambient namespace from the "+
			"origin's ROOT domain, the way boot derives the id: %v", err)
	}
	if fn == nil {
		t.Fatal("the loader returned no function for the nested origin")
	}

	// The same source under the flat origin must behave identically -- if it
	// does not, the assertion above is passing for some unrelated reason.
	flatFn, ferr := tryParseNewFunctionSyntax(
		"nestedAmbientRef", "mutation", src, "deployment/mutations.memql", registry)
	if ferr != nil || flatFn == nil {
		t.Fatalf("the FLAT case regressed too, so this test is not measuring what it claims: %v", ferr)
	}
}

// TestCanonicalId_NestedFileDoesNotBindItsSubdirectorysNamespace is the guard
// on which directory the ambient rule is handed (memql#3026 landing review,
// round 3).
//
// Admitting the directory as well as the pin is only correct when `domain` IS
// the directory assembly used -- the FIRST path segment. The loader was still
// passing DomainFromFilePath's LAST segment, so for
// agents/tools/askSpecialist.memql the rule admitted any id whose namespace is
// `tools`: a FOREIGN domain's concepts, bound with no import, on the path that
// derives row ids. That is a widening, not a near miss, and it is the exact
// bind the gate exists to refuse -- introduced by the fix for the deadlock in
// the other direction.
//
// The tree has 23 nested files under dsl/agents/{roles,skills,tools}; none
// writes canonicalId today, so this was latent, and a MEMQL_DSL_PATH bundle
// mounting a domain named `tools`, `roles` or `skills` makes it live.
func TestCanonicalId_NestedFileDoesNotBindItsSubdirectorysNamespace(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		// A foreign domain whose namespace happens to equal the SUBDIRECTORY
		// of the file doing the referencing.
		"v1:tools:widget": {Name: "v1:tools:widget"},
	})
	const origin = "agents/tools/askSpecialist.memql"

	// Through the loader, which is where the wrong segment was being passed.
	src := `@description("A nested file naming a foreign domain's concept.")
mutate widget nestedForeignRef {
  args {
    x  string  @required
  }
  insert {
    id: canonicalId(args.x, widget)
  }
}`
	if _, err := tryParseNewFunctionSyntax(
		"nestedForeignRef", "mutation", src, origin, registry); err == nil {
		t.Error("a nested file bound a FOREIGN domain's concept ambiently, with no import, " +
			"because the ambient rule was handed the file's SUBDIRECTORY (\"tools\") instead of " +
			"the directory its own concepts assemble under (\"agents\"). The two arguments must " +
			"both describe the assembly directory.")
	}

	// And directly on the correct pairing, so a failure names the rule rather
	// than the loader. (The mis-pairing is not asserted here: handing the rule
	// the last segment DOES admit v1:tools:widget, which is precisely why the
	// loader must not do it -- a test asserting the broken call still breaks
	// would be pinning the hazard, not the fix.)
	if got, err := NewConceptResolver(registry).ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, RootDomainFromFilePath(origin), declaredNamespaceForOrigin(origin)); err == nil {
		t.Errorf("the ambient rule admitted a foreign namespace even when handed the assembly "+
			"directory. `tools` is not a namespace agents/tools/askSpecialist.memql can "+
			"assemble under.\n  got: %s", got)
	}

	// The right pairing resolves this file's OWN domain, which is what the
	// directory arm is for -- so the guard above is not just refusing
	// everything.
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:agents:widget": {Name: "v1:agents:widget"},
	})
	got, err := NewConceptResolver(registry).ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, RootDomainFromFilePath(origin), declaredNamespaceForOrigin(origin))
	if err != nil {
		t.Fatalf("a nested file must still reach its own root domain's concept: %v", err)
	}
	if !strings.Contains(got, `"v1:agents:widget"`) {
		t.Errorf("wrong id.\n  got: %s", got)
	}
}

// TestCanonicalId_AmbientNamespaceTestIsAnchored guards the substring hazard
// (memql#3026 landing review).
//
// Namespaces are colon-separated and multi-segment ones are live -- dsl/cognition
// declares @namespace("cognition:client:tool"). The first cut tested
// strings.Contains(id, ":"+declaredNS+":"), which matches an INTERIOR segment,
// so a domain named "tool" or "client" bound v1:cognition:client:tool:* with no
// import. Once uniqueness was deleted this condition became the ONLY thing
// refusing a foreign concept, so an unanchored match silently reopened the
// cross-namespace bind that DoD item 2 closes -- reachable from any bundle
// mounted at MEMQL_DSL_PATH that happens to name a domain "tool".
func TestCanonicalId_AmbientNamespaceTestIsAnchored(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:cognition:client:tool:gadget": {Name: "v1:cognition:client:tool:gadget"},
	})
	r := NewConceptResolver(registry)

	for _, foreign := range []string{"tool", "client"} {
		if got, err := r.ResolveCanonicalIdConceptRefsInNamespace(
			`canonicalId(args.x, gadget)`, foreign, foreign); err == nil {
			t.Errorf("declaredNS %q bound a concept in the `cognition:client:tool` namespace "+
				"ambiently -- %q is an INTERIOR segment of that id, not its namespace. On the "+
				"path that derives row ids this is the silent cross-namespace bind DoD item 2 "+
				"exists to refuse.\n  got: %s", foreign, foreign, got)
		}
	}

	// The colon EXTENSION is admitted deliberately, and must stay admitted:
	// AssembleConceptIdFromDeclInDir accepts an @namespace that colon-extends
	// the directory, so v1:cognition:client:tool:gadget IS a concept of domain
	// cognition and is in its ambient scope.
	got, err := r.ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, gadget)`, "cognition", "cognition")
	if err != nil {
		t.Fatalf("a colon-extended sub-namespace must stay in its root domain's ambient scope, "+
			"or the anchor has over-tightened into the mirror of the bug it fixes: %v", err)
	}
	if !strings.Contains(got, `"v1:cognition:client:tool:gadget"`) {
		t.Errorf("wrong id.\n  got: %s", got)
	}

	// A namespace that merely PREFIXES another segment-wise is not a match:
	// "cog" must not bind "cognition:...".
	if got, err := r.ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, gadget)`, "cog", "cog"); err == nil {
		t.Errorf("declaredNS \"cog\" bound a `cognition:...` concept -- the anchor must be at a "+
			"segment boundary, not a raw prefix.\n  got: %s", got)
	}
}

// TestCanonicalId_PinnedDomainAdmitsItsDirectoryToo is the second-round
// correction to the ambient rule, and it guards a DEADLOCK rather than a
// widening (memql#3026 landing review).
//
// AssembleConceptIdFromDeclInDir admits three namespaces for a file in a
// pinned domain -- the directory, a colon-extension of the directory, and the
// pin (concept_id.go's #2614 rule, `if !hasNamespace { namespace = dir }`
// first among them). Testing only the PIN refused a concept declared in the
// file's own domain that simply carries no @namespace, while the same-domain
// import its error demanded is the one TestNoSameDomainUse bans: no working
// spelling, which is the memql#2976 deadlock re-entered from the other side,
// and a regression against main.
//
// Aggravated by TestNoRedundantNamespace, which STRIPS a @namespace equal to
// the directory -- so the tree's own gates push a concept in a pinned domain
// towards exactly the shape that was refused.
func TestCanonicalId_PinnedDomainAdmitsItsDirectoryToo(t *testing.T) {
	// A pinned domain (directory "zdeploy", pin "zcluster") whose concepts are
	// declared under BOTH namespaces -- annotated and not.
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:zcluster:widget":     {Name: "v1:zcluster:widget"},     // @namespace("zcluster")
		"v1:zdeploy:gadget":      {Name: "v1:zdeploy:gadget"},      // no @namespace: derives the directory
		"v1:zdeploy:sub:sprover": {Name: "v1:zdeploy:sub:sprover"}, // @namespace("zdeploy:sub")
		"v1:elsewhere:foreign":   {Name: "v1:elsewhere:foreign"},   // another namespace entirely
	})
	r := NewConceptResolver(registry)

	for name, want := range map[string]string{
		"widget":  `"v1:zcluster:widget"`,     // via the pin
		"gadget":  `"v1:zdeploy:gadget"`,      // via the directory
		"sprover": `"v1:zdeploy:sub:sprover"`, // via a colon-extension of the directory
	} {
		t.Run(name, func(t *testing.T) {
			got, err := r.ResolveCanonicalIdConceptRefsInNamespace(
				"canonicalId(args.x, "+name+")", "zdeploy", "zcluster")
			if err != nil {
				t.Fatalf("a concept this domain could have DECLARED was refused ambient scope. "+
					"The ambient test must mirror AssembleConceptIdFromDeclInDir -- directory, "+
					"colon-extension of the directory, or pin -- or a concept in the file's own "+
					"domain has no working spelling at all: %v", err)
			}
			if !strings.Contains(got, want) {
				t.Errorf("wrong id.\n  want to contain: %s\n  got: %s", want, got)
			}
		})
	}

	// And the refusal still holds where it must: another namespace entirely is
	// NOT in scope, however unique its name.
	if got, err := r.ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, foreign)`, "zdeploy", "zcluster"); err == nil {
		t.Errorf("admitting the directory must not re-open cross-namespace binding -- "+
			"v1:elsewhere:foreign is a namespace this domain cannot assemble.\n  got: %s", got)
	}
}

// TestIdNamespace_StripsOnlyARealVersionSegment pins the assumption underneath
// the anchor (memql#3026 landing review).
//
// idNamespace drops the leading `v<major>:` so the namespace can be anchored at
// its own first character. Dropping "everything before the first colon"
// unconditionally reproduces the exact interior-segment bind the anchor exists
// to close the moment it meets an id without a version prefix: "client" would
// match "cognition:client:tool:gadget". Every id AssembleConceptId emits does
// carry a version, which is the reason to CHECK the assumption rather than to
// rely on it -- an unchecked one is invisible until something else changes.
func TestIdNamespace_StripsOnlyARealVersionSegment(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"v1:cognition:space", "cognition"},
		{"v1:cognition:client:tool:request", "cognition:client:tool"},
		{"v12:cluster:node", "cluster"},
		// No version segment: nothing is stripped, so the FIRST segment is
		// part of the namespace and cannot be matched as an interior one.
		{"cognition:client:tool:gadget", "cognition:client:tool"},
		{"voice:thing", "voice"}, // "v" followed by a non-digit is not a version
		{"v1x:thing", "v1x"},
		// Degenerate: no name segment means it is not a concept id.
		{"v1:cluster", ""},
		{"cluster", ""},
		{"", ""},
	} {
		if got := idNamespace(tc.id); got != tc.want {
			t.Errorf("idNamespace(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}

	// The consequence, stated as the assertion that matters: an id with no
	// version prefix must not be matchable by one of its interior segments.
	if idIsInDomainAmbientScope("cognition:client:tool:gadget", "client", "client") {
		t.Error("an id without a version prefix was matched by its INTERIOR segment \"client\" -- " +
			"the version strip must verify it is stripping a version, or the anchor is not an " +
			"anchor at all.")
	}
}

// TestCanonicalId_EscapedQuoteDoesNotDesyncTheScanner guards the interaction
// between the string tracker and the comment branches (memql#3026 landing
// review).
//
// The tracker toggles inStr on every `"`. Before the comment branches existed
// an escaped quote's desync was harmless; afterwards it is not, because the
// next `/*` is then read as a real block comment, finds no closer, and copies
// THE REST OF THE FILE through verbatim -- so every genuine canonicalId after
// it is silently left un-rewritten and the bare name reaches evaluation
// unresolved. Silent, on the path that derives row ids. Escaped quotes are
// already in the corpus (dsl/todos/tools.memql, dsl/forge/tools.memql).
func TestCanonicalId_EscapedQuoteDoesNotDesyncTheScanner(t *testing.T) {
	for name, src := range map[string]string{
		"escaped quote then block comment": "@description(\"a \\\"/* b\")\ncanonicalId(args.x, widget)",
		"escaped quote then line comment":  "@description(\"a \\\" // b\")\ncanonicalId(args.x, widget)",
		"escaped backslash before quote":   "@description(\"a \\\\\")\ncanonicalId(args.x, widget)",
		"two escaped quotes":               "@description(\"\\\"x\\\"\")\ncanonicalId(args.x, widget)",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := remappedPackResolver().ResolveCanonicalIdConceptRefsInNamespace(
				src, "zdeploy", "zcluster")
			if err != nil {
				t.Fatalf("resolve failed: %v", err)
			}
			if !strings.Contains(got, `"v1:zcluster:widget"`) {
				t.Errorf("the real canonicalId call after an escaped quote was NOT rewritten -- "+
					"the string tracker desynced and the scanner swallowed the rest of the file "+
					"as a comment. This fails silently at load and resolves to nil at "+
					"evaluation.\n  src: %q\n  got: %q", src, got)
			}
		})
	}
}

// TestCanonicalId_DisambiguatorIsAnchoredToo is the other half of the
// anchoring fix (memql#3026 landing review).
//
// idIsInDomainAmbientScope was anchored while resolveBareConceptNameWithNamespace's
// own hint filter still used the `":"+hint+":"` substring. For a hint that is
// an INTERIOR segment of a live multi-segment namespace -- "tool" against
// v1:cognition:client:tool:request, the exact bundle scenario this rule is
// for -- the filter kept BOTH candidates, so resolution failed as "ambiguous"
// before the anchored gate ever ran. The gate cannot refuse what never reaches
// it, and the caller was left demanding a same-domain import the gate bans.
func TestCanonicalId_DisambiguatorIsAnchoredToo(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:tool:request":                  {Name: "v1:tool:request"},                  // a bundle's own
		"v1:cognition:client:tool:request": {Name: "v1:cognition:client:tool:request"}, // real, dsl/cognition
	})
	r := NewConceptResolver(registry)

	got, err := r.ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, request)`, "tool", "tool")
	if err != nil {
		t.Fatalf("a bundle domain named \"tool\" could not reference its OWN v1:tool:request. "+
			"The hint filter must anchor at the namespace: \"tool\" is an interior segment of "+
			"v1:cognition:client:tool:request, so an unanchored filter keeps both candidates and "+
			"the name stays ambiguous -- while the import the error asks for is stripped by the "+
			"same-domain gate: %v", err)
	}
	if !strings.Contains(got, `"v1:tool:request"`) {
		t.Errorf("bound the FOREIGN concept.\n  got: %s", got)
	}

	// The colon-extended namespace is still reachable from its own root domain.
	cog, cerr := r.ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, request)`, "cognition", "cognition")
	if cerr != nil {
		t.Fatalf("domain cognition must still reach its own colon-extended sub-namespace: %v", cerr)
	}
	if !strings.Contains(cog, `"v1:cognition:client:tool:request"`) {
		t.Errorf("wrong id from domain cognition.\n  got: %s", cog)
	}
}

// TestCanonicalId_EmptyDeclaredNamespaceStillResolvesViaTheDirectory covers a
// caller that knows only the directory -- an unpinned domain, or the plain
// ResolveCanonicalIdConceptRefsInDomain path.
//
// It is named for what it asserts. An earlier version claimed to pin a
// `declaredNS = domain` FALLBACK, and did not: with the directory now an
// ambient namespace in its own right, deleting that fallback changed neither
// behaviour nor any test, so the branch was dead and the test was pinning
// nothing. The branch is gone; this asserts the behaviour that remains.
// (Same "passes against the regression" flaw this file calls out twice in
// other people's tests -- it was in mine too. Landing review, round 3.)
//
// The fixture is the AMBIGUOUS one so the hint genuinely has work to do.
func TestCanonicalId_EmptyDeclaredNamespaceStillResolvesViaTheDirectory(t *testing.T) {
	got, err := ambiguousRemappedResolver().ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, "zcluster", "  ")
	if err != nil {
		t.Fatalf("a caller supplying only the directory must still get ambient resolution -- "+
			"with an ambiguous name the directory hint is the only thing that can "+
			"disambiguate: %v", err)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("wrong id.\n  got: %s", got)
	}
}

// TestCanonicalId_AmbientHintPrefersThePin pins ambientHints' ORDER, which
// decides which canonical id is emitted and was pinned by nothing (landing
// review, round 3).
//
// A pinned domain assembles under two namespaces, so it can declare the same
// short name twice -- once annotated (the pin), once not (the directory). Both
// are legitimately its own and both pass idIsInDomainAmbientScope, so the gate
// cannot choose between them: precedence alone decides, and it decides a ROW
// ID. Pin first, because the pin is the deliberate declaration (#2614's escape
// hatch is written down on purpose) while the directory is the default.
func TestCanonicalId_AmbientHintPrefersThePin(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:zcluster:widget": {Name: "v1:zcluster:widget"}, // annotated: the pin
		"v1:zdeploy:widget":  {Name: "v1:zdeploy:widget"},  // unannotated: the directory
	})

	got, err := NewConceptResolver(registry).ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, "zdeploy", "zcluster")
	if err != nil {
		t.Fatalf("a name declared under BOTH of a pinned domain's namespaces must still "+
			"resolve -- both are this domain's own: %v", err)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("the PIN must win when a short name is declared under both the pin and the "+
			"directory. Swapping the hint order silently changes the derived row id, and both "+
			"ids pass the ambient gate, so nothing else catches it.\n"+
			"  want to contain: \"v1:zcluster:widget\"\n  got: %s", got)
	}
}
