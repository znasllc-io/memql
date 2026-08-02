package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// canonical_id_ambient_namespace_2976_test.go -- memql#2976.
//
// `canonicalId(value, <concept>)` resolves its concept argument against
// file-top `use` imports, or ambiently when the concept is same-domain
// (#2617). The ambient check tested the assembled id against the containing
// DIRECTORY:
//
//	strings.Contains(id, ":"+domain+":")
//
// For any pack whose directory differs from its `@namespace` that never
// matches. `dsl/deployment` declares `@namespace("cluster")`, so its concepts
// assemble to `v1:cluster:deployment` while the ambient hint is `deployment`,
// and the loader answered:
//
//	canonicalId: concept "deployment" is neither imported nor a same-domain concept
//
// about a concept declared in that very domain. Namespace remapping is a
// supported feature -- `@namespace` exists precisely so a directory need not
// dictate the canonical id -- so this is not an exotic shape.
//
// # The unsatisfiable part
//
// There was no spelling that worked. The error asks for an import; the
// same-domain import it is asking for is the one `TestNoSameDomainUse` bans
// (#2617), and the pinned-namespace import hit #2945 and #2977. The ambient
// rule and the same-domain gate disagreed about what "same domain" means for a
// remapped pack: the loader said "not same-domain, import it", the gate said
// "that is a same-domain import, remove it".
//
// # The fix
//
// Uniqueness, which is what boot already uses.
// `resolveBareConceptNameWithNamespace` returns a unique trailing-segment match
// BEFORE it consults the hint, so signature-concept binding has always accepted
// these names ambiently -- only `canonicalId` carried the extra directory test.
// An AMBIGUOUS name still needs an import unless the directory really does name
// the namespace, because two concepts sharing a trailing segment is exactly
// what #2617's rule protects against.

// remappedPackResolver models a pack whose DIRECTORY differs from its
// @namespace: files live in `zdeploy/`, concepts assemble under `zcluster`.
// Built by construction rather than by pointing at dsl/deployment, so the
// coverage does not depend on that one pack continuing to exist (memql#2976
// definition-of-done item 3).
func remappedPackResolver(extra ...string) *ConceptResolver {
	all := map[string]*memoryNodes.Concept{
		"v1:zcluster:widget": {Name: "v1:zcluster:widget"},
	}
	for _, name := range extra {
		all[name] = &memoryNodes.Concept{Name: name}
	}
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(all)
	return NewConceptResolver(registry)
}

// TestCanonicalId_AmbientResolvesInARemappedPack is the reproduction.
//
// The file sits in `zdeploy/`; its concept assembles under `zcluster`. No
// import, and none should be needed.
func TestCanonicalId_AmbientResolvesInARemappedPack(t *testing.T) {
	got, err := remappedPackResolver().
		ResolveCanonicalIdConceptRefsInDomain(`canonicalId(args.x, widget)`, "zdeploy")
	if err != nil {
		t.Fatalf("canonicalId did not resolve ambiently in a namespace-remapped pack.\n"+
			"The concept is declared in this file's own domain; the ambient check compared the "+
			"assembled id against the DIRECTORY (\"zdeploy\") rather than the namespace it "+
			"actually assembles under (\"zcluster\"), so it never matched and the loader asked "+
			"for an import that is banned by TestNoSameDomainUse (memql#2976).\n  error: %v", err)
	}
	if !strings.Contains(got, `canonicalId(args.x, "v1:zcluster:widget")`) {
		t.Errorf("ambient resolution produced the wrong canonical id.\n  got: %s", got)
	}
}

// TestCanonicalId_AmbientAndSameDomainGateAgree is definition-of-done item 2.
//
// The two must not contradict: for any file, exactly one of "ambient works" and
// "an import is required" should be true, and the gate must permit whichever it
// is. Before the fix BOTH were false for a remapped pack -- ambient failed, and
// the import that would have worked was the one the gate strips.
//
// Asserted as the conjunction rather than as two separate facts, because it is
// the conjunction that was broken.
func TestCanonicalId_AmbientAndSameDomainGateAgree(t *testing.T) {
	const domain = "zdeploy"

	// 1. Ambient resolution works, so no import is needed.
	if _, err := remappedPackResolver().
		ResolveCanonicalIdConceptRefsInDomain(`canonicalId(args.x, widget)`, domain); err != nil {
		t.Fatalf("ambient resolution fails, so an import IS required here: %v", err)
	}

	// 2. And the same-domain import is still stripped by the gate -- which is
	//    only consistent BECAUSE ambient works. If the rewrite ever stopped
	//    stripping it the two rules would have drifted apart again, in the
	//    other direction.
	src := []byte("use zdeploy.concepts.{ widget }\n\nmutate widget doThing {\n  insert {\n    id: args.x\n  }\n}\n")
	rewritten, err := languageParser.RewriteSameDomainUse(domain, src)
	if err != nil {
		t.Fatalf("RewriteSameDomainUse: %v", err)
	}
	if string(rewritten) == string(src) {
		t.Errorf("the same-domain-use gate no longer strips `use %s.concepts.{ widget }`.\n"+
			"The ambient rule and the gate must agree: ambient resolution works for this file, "+
			"so the import is redundant and the gate should remove it. Two rules that both "+
			"accept it is the mirror of the memql#2976 deadlock where both refused.", domain)
	}
}

// TestCanonicalId_AmbiguousNameStillRequiresAnImport is the over-widening guard.
//
// memql#2976 is satisfied by deleting the ambient check entirely, and that
// would be worse: two concepts sharing a trailing segment across namespaces is
// exactly the collision #2617's rule protects against, and binding one of them
// by whichever the hint happened to favour is a silent wrong answer.
func TestCanonicalId_AmbiguousNameStillRequiresAnImport(t *testing.T) {
	r := remappedPackResolver("v1:zother:widget")

	// From a domain that is neither namespace: ambiguous, no directory match,
	// so an import is required.
	_, err := r.ResolveCanonicalIdConceptRefsInDomain(`canonicalId(args.x, widget)`, "zdeploy")
	if err == nil {
		t.Error("an AMBIGUOUS concept name resolved ambiently. `widget` is declared under both " +
			"zcluster and zother, so nothing in the file says which is meant -- this must still " +
			"demand an explicit import (memql#2617, memql#2976).")
	}

	// From the directory that DOES name one of the namespaces, the ambient
	// same-domain rule still applies -- this is the branch the fix preserves.
	got, derr := r.ResolveCanonicalIdConceptRefsInDomain(`canonicalId(args.x, widget)`, "zother")
	if derr != nil {
		t.Fatalf("a directory that names the concept's own namespace must still resolve "+
			"ambiently even when the bare name is ambiguous elsewhere: %v", derr)
	}
	if !strings.Contains(got, `"v1:zother:widget"`) {
		t.Errorf("same-domain disambiguation picked the wrong concept.\n  got: %s", got)
	}
}

// TestCanonicalId_CrossDomainUniqueNameMatchesSignatureBinding pins the other half of
// #2617: a concept from another domain is not in ambient scope just because it
// happens to be unique.
//
// NOTE this is the one place the fix genuinely widens behaviour, and the test
// says so rather than hiding it. A UNIQUE cross-domain name now resolves
// ambiently, because uniqueness is the condition boot itself uses and
// signature-concept binding already accepted it -- `canonicalId` was the only
// construct applying a stricter rule. What is preserved is the protection that
// matters: ambiguity still demands an import.
func TestCanonicalId_CrossDomainUniqueNameMatchesSignatureBinding(t *testing.T) {
	r := remappedPackResolver()

	// A file in an unrelated domain naming the unique concept.
	got, err := r.ResolveCanonicalIdConceptRefsInDomain(`canonicalId(args.x, widget)`, "somewhereElse")
	if err != nil {
		t.Fatalf("a unique concept name should resolve the same way signature binding resolves "+
			"it -- resolveBareConceptNameWithNamespace returns a unique match before it consults "+
			"the hint, so refusing here would keep canonicalId stricter than the rest of the "+
			"language for no stated reason (memql#2976): %v", err)
	}
	if !strings.Contains(got, `"v1:zcluster:widget"`) {
		t.Errorf("wrong canonical id for the unique name.\n  got: %s", got)
	}
}
