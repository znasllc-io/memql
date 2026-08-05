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

// TestCanonicalId_AmbiguousRemappedPackHasExactlyOneSpelling is DoD item 3, in
// full (memql#3026 landing review).
//
// The item is a conjunction -- "a remapped pack with an ambiguous name has
// exactly ONE working spelling, AND TestNoSameDomainUse permits it" -- and it
// was covered as two disjoint halves in two files: the ambient half here
// against the two-concept fixture, the gate half in the 2976 file against the
// single-concept fixture that issue #3026 section 4 condemns as unable to tell
// the two rules apart. The conjunction is the thing that was broken, so the
// conjunction is what needs asserting, against the fixture that discriminates.
//
// #2976's consistency requirement: "for any file, exactly one of 'ambient
// works' or 'an import is required' should be true, and the gate should permit
// whichever it is." Under uniqueness NEITHER was true here.
func TestCanonicalId_AmbiguousRemappedPackHasExactlyOneSpelling(t *testing.T) {
	const domain = "zdeploy"

	// 1. The ambient spelling works, so no import is needed.
	if _, err := ambiguousRemappedResolver().ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, domain, "zcluster"); err != nil {
		t.Fatalf("ambient resolution fails for an AMBIGUOUS name in a remapped pack, so an "+
			"import IS required -- and step 2 shows the only same-domain spelling is stripped. "+
			"That is the memql#2976 deadlock, which uniqueness could not close: %v", err)
	}

	// 2. And the same-domain import is unavailable: the gate strips it. So
	//    ambient is not merely A working spelling, it is the ONLY one.
	withImport := []byte("use zdeploy.concepts.{ widget }\n\nmutate widget doThing {\n  insert {\n    id: args.x\n  }\n}\n")
	stripped, err := languageParser.RewriteSameDomainUse(domain, withImport)
	if err != nil {
		t.Fatalf("RewriteSameDomainUse: %v", err)
	}
	if string(stripped) == string(withImport) {
		t.Errorf("the gate no longer strips `use %s.concepts.{ widget }`, so BOTH spellings now "+
			"work -- the mirror of the memql#2976 deadlock where both failed. The ambient rule "+
			"and the gate must agree on which single spelling is correct.", domain)
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
	_, err := tryParseNewFunctionSyntax(
		"nestedAmbientRef", "mutation", src, "deployment/anything/mutations.memql", registry)
	if err != nil && strings.Contains(err.Error(), "neither imported nor a same-domain concept") {
		t.Fatalf("the loader refused a nested file's reference to its OWN domain's concept, and "+
			"the import its error demands is the one TestNoSameDomainUse bans -- the memql#2976 "+
			"deadlock reinstated one directory deeper. The loader must derive the ambient "+
			"namespace from the origin's ROOT domain, the way boot derives the id: %v", err)
	}

	// The same source under the flat origin must behave identically -- if it
	// does not, the assertion above is passing for some unrelated reason.
	if _, ferr := tryParseNewFunctionSyntax(
		"nestedAmbientRef", "mutation", src, "deployment/mutations.memql", registry); ferr != nil &&
		strings.Contains(ferr.Error(), "neither imported nor a same-domain concept") {
		t.Fatalf("the FLAT case regressed too, so this test is not measuring what it claims: %v", ferr)
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

// TestCanonicalId_EmptyDeclaredNamespaceFallsBackToTheDomain covers the
// exported three-argument form's defensive branch, which nothing else reaches:
// production always calls InDomain, which supplies a namespace. A caller that
// knows only the directory should get the directory's behaviour rather than
// silently losing ambient scope altogether.
func TestCanonicalId_EmptyDeclaredNamespaceFallsBackToTheDomain(t *testing.T) {
	got, err := remappedPackResolver().ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, "zcluster", "  ")
	if err != nil {
		t.Fatalf("an empty declaredNS must fall back to the domain, not disable ambient "+
			"resolution: %v", err)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("wrong id.\n  got: %s", got)
	}
}
