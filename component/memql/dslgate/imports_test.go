package dslgate

import (
	"sort"
	"strings"
	"testing"
)

// imports_test.go -- znasllc-io/memql#3803.
//
// `use` had two jobs and only one was enforced: load-bearing for `concept`,
// pure documentation for the twelve flat kinds. This gate makes it mean one
// thing -- import what you reference from another namespace -- and these tests
// pin both the rule and the two mistakes that made the FIRST measurement of its
// cost wrong by a factor of 300.

// gateOn runs the gate over an in-memory corpus.
//
// Paths are SORTED before the scan, matching what every real caller supplies:
// both entry points source their file set from dslfs.WalkMemqlFiles, which
// returns sorted paths. Pass 1 of the gate resolves a duplicate declaration
// first-wins, so an unordered corpus would make a duplicate's attributed
// namespace depend on Go's map iteration order and flake this suite.
func gateOn(t *testing.T, files map[string]string) []Violation {
	t.Helper()
	return gateOnWith(t, files, Options{})
}

// gateOnWith is gateOn with a verdict on which domains are core (memql#4882).
func gateOnWith(t *testing.T, files map[string]string, opts Options) []Violation {
	t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	corpus := make([]SourceFile, 0, len(paths))
	for _, p := range paths {
		corpus = append(corpus, SourceFile{Path: p, Content: files[p]})
	}
	return scanCrossNamespaceImports(corpus, opts)
}

// TestCrossNamespaceReferenceNeedsAnImport is the rule.
func TestCrossNamespaceReferenceNeedsAnImport(t *testing.T) {
	got := gateOn(t, map[string]string{
		"common/builtins.memql": "builtin trackPresence {\n  x string\n}\n",
		"cognition/automations.memql": `automation a {
  step s {
    builtin trackPresence(x: "1")
  }
}
`,
	})
	if len(got) != 1 {
		t.Fatalf("violations = %v, want 1: a call into another namespace with no import is "+
			"exactly what enforcing `use` means", got)
	}
	if !strings.Contains(got[0].Detail, "use common.builtins.{ trackPresence }") {
		t.Errorf("the violation does not spell the import that fixes it, so a reader has the "+
			"finding and no remedy:\n%s", got[0].Detail)
	}
}

// TestImportedCrossNamespaceReferenceIsFine is the other direction, and the one
// that keeps the gate from being a corpus-wide migration: the tree was already
// 113/114 compliant by habit before this rule existed.
func TestImportedCrossNamespaceReferenceIsFine(t *testing.T) {
	got := gateOn(t, map[string]string{
		"common/traits.memql": "trait isActiveRecord {\n  return active == true\n}\n",
		"cognition/queries.memql": `use common.traits.{ isActiveRecord }

query participant q {
  filter  isActiveRecord
}
`,
	})
	if len(got) != 0 {
		t.Errorf("an IMPORTED cross-namespace reference was flagged: %v", got)
	}
}

// TestSameNamespaceNeedsNoImport is the uniform exception -- and the thing
// memql#2617 was right about for these kinds.
func TestSameNamespaceNeedsNoImport(t *testing.T) {
	got := gateOn(t, map[string]string{
		"common/traits.memql": "trait isActiveRecord {\n  return active == true\n}\n",
		"common/queries.memql": `query thing q {
  filter  isActiveRecord
}
`,
	})
	if len(got) != 0 {
		t.Errorf("a SAME-namespace reference was flagged, which would make every file in a "+
			"domain import its own siblings: %v", got)
	}
}

// TestNestedDirectoryIsItsOwnNamespace.
//
// Each directory is a namespace, including a nested one: dsl/agents/roles/ is
// `agents/roles`, not `agents`. That is the authoring model the corpus already
// follows -- every seed file under agents/roles/ carries
// `use agents.concepts.{ agentRole }` for a concept in its PARENT directory,
// an import nothing required and which would be redundant if the subdirectory
// were part of the same namespace.
func TestNestedDirectoryIsItsOwnNamespace(t *testing.T) {
	got := gateOn(t, map[string]string{
		"agents/builtins.memql": "builtin mintSkill {\n  x string\n}\n",
		"agents/roles/farming.memql": `seed agentRole rowCrop {
  body: mintSkill(x: "1")
}
`,
	})
	if len(got) != 1 {
		t.Fatalf("a nested directory referencing its PARENT with no import was not flagged "+
			"(%v). Treating agents/roles as part of agents makes the subdirectory boundary "+
			"invisible, which is the opposite of what the corpus already writes.", got)
	}
}

// TestPoseIsNotAReference is the first of the two mistakes that made the
// initial measurement of this rule's cost wrong.
//
// A word-boundary match over the raw source reported 345 unimported references.
// Most were English: `builtin agent`, `builtin error`, `builtin tools`,
// `builtin help` and `builtin concepts` are ordinary words that appear in doc
// comments and @description text. A gate built that way would refuse boot over
// a sentence.
func TestProseIsNotAReference(t *testing.T) {
	got := gateOn(t, map[string]string{
		"common/builtins.memql": "builtin recall {\n  x string\n}\n",
		"planner/concepts.memql": `// The planner can recall (x) earlier context.
concept plan {
  note string @description("Queries filter rows out via recall (payload.x!=true).")
}
`,
	})
	if len(got) != 0 {
		t.Errorf("a construct name inside a COMMENT or a @description string was read as a "+
			"reference: %v.\nA gate that refuses a boot over prose is worse than no gate.", got)
	}
}

// TestStringsAreStrippedBeforeComments is the second mistake, and the subtler
// one: it is an ORDERING bug, not a missing rule.
//
// Stripping comments first removes a `//` that lives INSIDE a string -- a URL
// in a @description -- which eats that string's closing quote and
// desynchronises every string match after it in the file. Measured: a sentence
// in dsl/knowledge/concepts.memql then read as a call to the builtin it was
// describing, and survived a fix that had already added string-stripping.
func TestStringsAreStrippedBeforeComments(t *testing.T) {
	got := gateOn(t, map[string]string{
		"common/builtins.memql": "builtin seedDomainContent {\n  x string\n}\n",
		"knowledge/concepts.memql": `concept doc {
  a string @description("See https://example.test/docs for details.")
  b string @description("Baseline content from seedDomainContent (catalog bodies).")
}
`,
	})
	if len(got) != 0 {
		t.Errorf("a name inside a @description was read as a reference: %v.\n"+
			"The URL in the FIRST description is the trigger -- its `//` is removed as a "+
			"comment, taking the closing quote with it, so every string after it stops being "+
			"recognised as a string. Strings must be stripped BEFORE comments.", got)
	}
}

// TestAnnotationIsNotACall.
//
// `@enum("a","b")` is an annotation, not a call to a construct named `enum`.
// Missing this cost 45 phantom findings, because a concept FIELD spelled
// `provider enum("heygen", ...)` also registered a bogus `provider enum`
// declaration for them to point at.
func TestAnnotationIsNotACall(t *testing.T) {
	got := gateOn(t, map[string]string{
		"agents/builtins.memql": "builtin enum {\n  x string\n}\n",
		"cognition/concepts.memql": `concept space {
  kind string @enum("a", "b")
}
`,
	})
	if len(got) != 0 {
		t.Errorf("an @annotation was read as a call: %v", got)
	}
}

// TestConceptFieldIsNotADeclaration is the other half of that same 45.
//
// declLineRe is anchored at column 0 because `^\s*` also matches an indented
// concept FIELD. `provider enum("heygen", ...)` is a field named `provider`
// whose type is an enum -- not a `provider` construct named `enum`.
func TestConceptFieldIsNotADeclaration(t *testing.T) {
	got := gateOn(t, map[string]string{
		"agents/concepts.memql": `concept persona {
  provider   enum("heygen", "d-id")
}
`,
		"cognition/concepts.memql": `concept space {
  kind string @enum("a", "b")
}
`,
	})
	if len(got) != 0 {
		t.Errorf("an indented concept FIELD was read as a top-level declaration, which makes "+
			"every @enum() in the tree a cross-namespace call: %v", got)
	}
}

// TestConceptsAreNotReportedHere.
//
// Concepts already enforce their own imports through signature resolution, and
// the resolver's refusal names the import better than this gate could. Two
// checks reporting one rule is how they drift.
func TestConceptsAreNotReportedHere(t *testing.T) {
	got := gateOn(t, map[string]string{
		"planner/concepts.memql": "concept plan {\n  x string\n}\n",
		"agents/queries.memql": `query plan q {
  filter  row.id != ""
}
`,
	})
	if len(got) != 0 {
		t.Errorf("a CONCEPT reference was reported by the flat-kind gate: %v", got)
	}
}

// -- memql#4882: the core tree's late-bound calls -------------------------------
//
// dsl/cognition/logic.memql calls `mutationCreateCanvasState`, which the engine
// documents as "supplied by a product bundle at runtime". On the merged tree a
// bundle that declares it made pass 1 find the declaration in the product
// namespace, and the gate asked the CORE file to import it -- an import the
// engine cannot write. These four cases pin the exemption to exactly that
// direction.

// coreIs marks the given top-level directories as the engine's own.
func coreIs(domains ...string) Options {
	set := map[string]bool{}
	for _, d := range domains {
		set[d] = true
	}
	return Options{CoreDomain: func(d string) bool { return set[d] }}
}

const lateBoundCaller = `logic onSecondActiveHuman {
  body {
    return mutation mutationCreateCanvasState(stateId: "voice-migrated", space: "s1")
  }
}
`

const lateBoundDeclaration = `mutate canvasState mutationCreateCanvasState {
  args {
    stateId  string
    space    string
  }
  insert {
    id: args.stateId
    args.space
  }
}
`

// TestCoreReferenceToARuntimeDeclaredNameIsTheLateBindingSeam is the fix: a
// core file calling a name that only a runtime domain declares is not a
// missing import, because the import it would need cannot be written.
func TestCoreReferenceToARuntimeDeclaredNameIsTheLateBindingSeam(t *testing.T) {
	files := map[string]string{
		"cognition/logic.memql": lateBoundCaller,
		"znas/mutations.memql":  lateBoundDeclaration,
	}
	if got := gateOnWith(t, files, coreIs("cognition")); len(got) != 0 {
		t.Fatalf("violations = %v, want none: the core tree cannot import a product "+
			"namespace, so this is the documented late-binding seam, not a missing `use`", got)
	}
	// Without the verdict the rule is what it was: reported. nil is the
	// fail-closed direction, so a caller that has the verdict must pass it.
	if got := gateOn(t, files); len(got) != 1 {
		t.Fatalf("violations without a core-domain verdict = %v, want 1: nil CoreDomain "+
			"must keep the pre-#4882 rule rather than exempt silently", got)
	}
}

// TestRuntimeReferenceToACoreNameStillNeedsAnImport is the direction that must
// stay refused: a product file CAN write `use cognition.mutations.{ ... }`, so
// it must.
func TestRuntimeReferenceToACoreNameStillNeedsAnImport(t *testing.T) {
	got := gateOnWith(t, map[string]string{
		"cognition/mutations.memql": lateBoundDeclaration,
		"znas/logic.memql":          lateBoundCaller,
	}, coreIs("cognition"))
	if len(got) != 1 {
		t.Fatalf("violations = %v, want 1: a runtime file referencing a core name with no "+
			"import is the ordinary rule, and the exemption must not reach it", got)
	}
	if !strings.Contains(got[0].Detail, "use cognition.mutations.{ mutationCreateCanvasState }") {
		t.Errorf("the violation must still spell the remedy:\n%s", got[0].Detail)
	}
}

// TestRuntimeReferenceToAnotherRuntimeNamespaceStillNeedsAnImport: two product
// domains are two namespaces like any other.
func TestRuntimeReferenceToAnotherRuntimeNamespaceStillNeedsAnImport(t *testing.T) {
	got := gateOnWith(t, map[string]string{
		"acme/mutations.memql": lateBoundDeclaration,
		"znas/logic.memql":     lateBoundCaller,
	}, coreIs("cognition"))
	if len(got) != 1 {
		t.Fatalf("violations = %v, want 1: runtime -> runtime is not the seam", got)
	}
}

// TestCoreReferenceToAnotherCoreNamespaceStillNeedsAnImport: the engine's own
// tree keeps the whole rule -- this is the case the gate was written for.
func TestCoreReferenceToAnotherCoreNamespaceStillNeedsAnImport(t *testing.T) {
	got := gateOnWith(t, map[string]string{
		"common/builtins.memql": "builtin trackPresence {\n  x string\n}\n",
		"cognition/automations.memql": `automation a {
  step s {
    builtin trackPresence(x: "1")
  }
}
`,
	}, coreIs("common", "cognition"))
	if len(got) != 1 {
		t.Fatalf("violations = %v, want 1: core -> core with no import is exactly what "+
			"enforcing `use` means, and #4882 must not widen the exemption to it", got)
	}
}

// TestNestedCoreNamespaceIsCore: agents/roles is `agents/roles` to namespaceOf
// (a nested directory is its own namespace) and is still the core tree.
func TestNestedCoreNamespaceIsCore(t *testing.T) {
	got := gateOnWith(t, map[string]string{
		"agents/roles/professional.memql": lateBoundCaller,
		"znas/mutations.memql":            lateBoundDeclaration,
	}, coreIs("agents"))
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none: a nested directory of a core domain is core", got)
	}
}
