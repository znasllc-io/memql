package dslfs

import "testing"

// A directory is a namespace, and a SUBDIRECTORY IS A DIFFERENT NAMESPACE
// (memql#3898). This pins the three path functions apart, because the whole
// defect class here is two of them being confused for each other -- which
// core/dslfs exists to prevent and has now been the subject of three issues
// (memql#2852, memql#3026, memql#3898).
//
// The three answer three different questions and this test states each one:
//
//	NamespaceFromFilePath   the whole directory PATH -- the namespace. Both the
//	                        ambient-resolution scope and the domain a concept's
//	                        canonical id assembles under. The one that matters.
//	RootDomainFromFilePath  the FIRST segment -- which mounted TREE this file
//	                        belongs to (RegisterTree, MEMQL_DSL_PATH). Nothing
//	                        else.
//	DomainFromFilePath      the LAST segment -- a namespace HINT, used by the
//	                        import-integrity lane only.
func TestTheThreePathFunctionsAnswerDifferentQuestions(t *testing.T) {
	cases := []struct {
		path      string
		namespace string
		root      string
		last      string
		why       string
	}{
		{
			path: "cognition/queries.memql", namespace: "cognition", root: "cognition", last: "cognition",
			why: "a FLAT file: all three agree, which is why the confusion stayed latent for so long -- every file that declares a concept in this tree is flat",
		},
		{
			path: "agents/tools/askSpecialist.memql", namespace: "agents/tools", root: "agents", last: "tools",
			why: "the nested case, where all three differ. `tools` is the trap: a top-level domain could be called that, and binding to it would reach a FOREIGN namespace with no import",
		},
		{
			path: "agents/roles/legal.memql", namespace: "agents/roles", root: "agents", last: "roles",
			why: "one of the 23 nested files live in the tree today",
		},
		{
			path: "unified:deployment/mutations.memql", namespace: "deployment", root: "deployment", last: "deployment",
			why: "a unified-loader slice origin: the `unified:` decoration must not pollute any of the three",
		},
		{
			path: "mutations/v1/cognition/join.memql", namespace: "mutations/cognition", root: "mutations", last: "cognition",
			why: "the legacy v<digits> layout segment is dropped by all three rather than treated as a namespace segment",
		},
		{
			path: "bare.memql", namespace: "", root: "", last: "",
			why: "a bare filename has no directory, so it has no namespace -- and \"\" must not read as some default domain",
		},
	}

	for _, tc := range cases {
		if got := NamespaceFromFilePath(tc.path); got != tc.namespace {
			t.Errorf("NamespaceFromFilePath(%q) = %q, want %q\n  %s", tc.path, got, tc.namespace, tc.why)
		}
		if got := RootDomainFromFilePath(tc.path); got != tc.root {
			t.Errorf("RootDomainFromFilePath(%q) = %q, want %q\n  %s", tc.path, got, tc.root, tc.why)
		}
		if got := DomainFromFilePath(tc.path); got != tc.last {
			t.Errorf("DomainFromFilePath(%q) = %q, want %q\n  %s", tc.path, got, tc.last, tc.why)
		}
	}
}

// The change is SCOPED: for every flat file -- which is every file in the tree
// that declares a concept -- the namespace is exactly what the root domain was,
// so not one canonical id moves.
//
// This is the assertion that makes memql#3898 cheap to land now and expensive
// later, which is the issue's own argument for deciding it before a concept
// lands in a subdirectory.
func TestFlatPathsAreUnchangedByTheNamespaceRule(t *testing.T) {
	flat := []string{
		"cognition/concepts.memql",
		"identity/concepts.memql",
		"agents/concepts.memql",
		"deployment/mutations.memql",
		"platform/seeds.memql",
	}
	for _, path := range flat {
		ns := NamespaceFromFilePath(path)
		root := RootDomainFromFilePath(path)
		if ns != root {
			t.Errorf("a flat file's namespace must equal what its root domain was, or an existing "+
				"canonical id moves: NamespaceFromFilePath(%q) = %q, RootDomainFromFilePath = %q",
				path, ns, root)
		}
	}
}

// A namespace is a PATH, spelled the way Go spells an import path -- one
// slash-separated string, with the identity being the whole thing rather than
// its last element.
//
// The alternative spelling, an extra colon segment (`v1:agents:tools:widget`),
// is the same idea and breaks the id contract: core/id.ParseNodeId defines a
// concept as the version segment plus EXACTLY two more, and that arity is
// unrecoverable from the string. Keeping the path INSIDE the domain segment is
// what leaves version:domain:entity intact.
func TestNamespaceIsAPathNotAnExtraIdSegment(t *testing.T) {
	ns := NamespaceFromFilePath("agents/tools/concepts.memql")
	if ns != "agents/tools" {
		t.Fatalf("namespace = %q, want %q", ns, "agents/tools")
	}
	// The composed concept id a caller builds from this stays 3 segments.
	id := "v1:" + ns + ":widget"
	segments := 1
	for _, c := range id {
		if c == ':' {
			segments++
		}
	}
	if segments != 3 {
		t.Errorf("composed concept id %q has %d colon-separated segments, want 3 -- "+
			"core/id.ParseNodeId reads version + exactly two more, and a 4th would be "+
			"indistinguishable from a shortId containing a colon", id, segments)
	}
}
