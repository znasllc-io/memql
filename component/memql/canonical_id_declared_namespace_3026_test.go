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

// TestCanonicalId_NestedFileUsesItsOwnNamespace pins the rule memql#3898
// established, and REPLACES the root-domain rule this test asserted for
// memql#3026.
//
// # What changed, and why the reversal is not a regression
//
// memql#3026 made a nested file resolve against its ROOT domain, because that
// was the directory boot assembled ids from (`dir := firstPathSegment(p)`).
// Ambient scope and assembly had to agree, and root-domain assembly was the
// fixed point, so ambient scope followed it there.
//
// memql#3898 moved the OTHER one. A directory is a namespace and a SUBDIRECTORY
// IS A DIFFERENT NAMESPACE -- `dsl/agents/roles/` is `agents/roles`, not
// `agents` -- so assembly now uses the whole directory path and ambient scope
// follows it there instead. The invariant memql#3026 was protecting is intact:
// exactly one function answers both questions (dslfs.NamespaceFromFilePath),
// which is why the deadlock it fixed cannot come back.
//
// # The model is Go's
//
// A directory is a package; a subdirectory is a different, unrelated package
// with no privileged access to its parent; and a symbol's global identity is
// the full IMPORT PATH plus the name. So a concept in a subdirectory assembles
// as `v1:agents/tools:widget` -- the path inside the domain segment, which
// keeps core/id.ParseNodeId's version:domain:entity arity intact.
//
// # The corpus already agreed
//
// 17 of the 23 nested files carry `use agents.concepts.{ agentRole }` for a
// concept in their PARENT directory -- an import the old engine did not require
// and which would have been redundant under it. Authors had been writing the
// boundary the engine did not enforce. The other 6 (dsl/agents/tools/*.memql)
// reference nothing ambient at all; their `@handler(name=...)` targets are
// string literals, resolved against the function registry rather than the
// namespace.
func TestCanonicalId_NestedFileUsesItsOwnNamespace(t *testing.T) {
	// A subdirectory is its own namespace, and the PARENT'S PIN DOES NOT REACH
	// IT. Go again: `package cluster` in directory `deployment` does not name
	// the package in `deployment/anything`, which declares its own. A pin is
	// per-directory, so a subdirectory that wants one carries its own file.
	if got := declaredNamespaceForOrigin("deployment/anything/mutations.memql"); got != "deployment/anything" {
		t.Errorf("a nested file must declare its OWN namespace, not its parent's. "+
			"got %q, want \"deployment/anything\"", got)
	}
	// THE FLAT CASE IS UNCHANGED, pin and all. This is the assertion that keeps
	// the change scoped: every non-nested file in the tree -- which is every
	// file that declares a concept -- resolves exactly as it did.
	if got := declaredNamespaceForOrigin("deployment/mutations.memql"); got != "cluster" {
		t.Errorf("the flat case must be unchanged. got %q, want \"cluster\"", got)
	}
	if got := declaredNamespaceForOrigin("agents/skills/categories.memql"); got != "agents/skills" {
		t.Errorf("a nested file under an unpinned domain declares its own path. got %q, "+
			"want \"agents/skills\"", got)
	}
	// Loader origin decorations must not leak into the namespace.
	if got := declaredNamespaceForOrigin("unified:deployment/mutations.memql:createDeployment"); got != "cluster" {
		t.Errorf("a unified-loader slice origin must strip to the same namespace. got %q, "+
			"want \"cluster\"", got)
	}
}

// TestCanonicalId_NestedFileMustImportItsParentsConcept is memql#3898's second
// acceptance bullet, and the behaviour change an author will actually meet.
//
// Under memql#3026 this resolved ambiently and silently. It is now REFUSED,
// naming the import -- which is the whole point of a namespace boundary: the
// dependency is declared rather than inferred from adjacency.
//
// THE IMPORT IT DEMANDS IS NOT THE ONE memql#2617 BANS, and that is what makes
// this safe rather than a new deadlock. TestNoSameDomainUse refuses an import
// whose namespace equals the file's OWN; here the file's namespace is
// `deployment/anything` and the import names `deployment`, which is a different
// one. The two rules finally agree about what "same domain" means -- the drift
// core/dslfs/domain.go's header measured ("boot -> tools, lint -> agents") is
// what memql#3898 closes.
func TestCanonicalId_NestedFileMustImportItsParentsConcept(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:cluster:deployment": {Name: "v1:cluster:deployment"},
	})
	const src = `@description("A nested file referencing its PARENT domain's concept.")
mutate deployment nestedAmbientRef {
  args {
    x  string  @required
  }
  insert {
    id: canonicalId(args.x, deployment)
  }
}`
	if _, err := tryParseNewFunctionSyntax(
		"nestedAmbientRef", "mutation", src, "deployment/anything/mutations.memql", registry); err == nil {
		t.Error("a nested file reached its PARENT directory's concept with no import. A " +
			"subdirectory is a different namespace (memql#3898), so the dependency must be " +
			"declared -- otherwise the boundary is documentation the engine does not enforce, " +
			"which is the state memql#3898 exists to end.")
	}

	// The FLAT file in the same domain still resolves ambiently, which is what
	// makes the assertion above about NESTING rather than about the concept.
	flatFn, ferr := tryParseNewFunctionSyntax(
		"nestedAmbientRef", "mutation", src, "deployment/mutations.memql", registry)
	if ferr != nil || flatFn == nil {
		t.Fatalf("the FLAT case regressed, so the refusal above is not measuring what it claims: %v", ferr)
	}
}

// TestCanonicalId_NestedFileDoesNotBindItsSubdirectorysBareName is the guard on
// what a nested file must NOT reach.
//
// Under memql#3026's rule the hazard was the LAST segment: a file in
// agents/tools resolving against a foreign domain literally named `tools`. The
// namespace is now the full path `agents/tools`, so that collision is gone by
// construction -- a top-level domain cannot be named `agents/tools`.
//
// The guard still matters in the other direction: a nested file must not reach
// a foreign namespace ambiently, whatever it is called.
func TestCanonicalId_NestedFileDoesNotBindItsSubdirectorysBareName(t *testing.T) {
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		// A foreign domain whose namespace is the file's SUBDIRECTORY name.
		"v1:tools:widget": {Name: "v1:tools:widget"},
	})
	const origin = "agents/tools/askSpecialist.memql"

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
		t.Error("a nested file bound a FOREIGN domain's concept ambiently, with no import.")
	}

	// And its OWN namespace resolves, so the guard above is not refusing
	// everything. `v1:agents/tools:widget` is what a concept declared in this
	// file's directory assembles as -- the Go-faithful path-in-domain form.
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:agents/tools:widget": {Name: "v1:agents/tools:widget"},
	})
	got, err := NewConceptResolver(registry).ResolveCanonicalIdConceptRefsInNamespace(
		`canonicalId(args.x, widget)`, NamespaceFromFilePath(origin), declaredNamespaceForOrigin(origin))
	if err != nil {
		t.Fatalf("a nested file must reach a concept declared in its OWN directory: %v", err)
	}
	if !strings.Contains(got, `"v1:agents/tools:widget"`) {
		t.Errorf("wrong id.\n  got: %s", got)
	}
}
