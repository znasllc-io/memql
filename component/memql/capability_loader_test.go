package memql

import (
	"strings"
	"testing"
)

// TestDeclaredCapabilityNames_EmbeddedTree confirms the capability loader
// picks up the authored example capabilities from the embedded DSL tree
// (dsl/capabilities/capabilities.memql). Story 5 / memql#2325.
func TestDeclaredCapabilityNames_EmbeddedTree(t *testing.T) {
	names, err := declaredCapabilityNames()
	if err != nil {
		t.Fatalf("declaredCapabilityNames: %v", err)
	}
	for _, want := range []string{"integration.github.tagRelease", "fs.readFile"} {
		if !names[want] {
			t.Errorf("declared capability %q not loaded from the embedded tree (got %v)", want, names)
		}
	}
}

// TestExtractCapabilitySlices covers the dot-aware slice extractor: it
// pulls each top-level `capability NAME { ... }` block (preamble + body)
// and is not fooled by a concept/builtin field literally named
// `capability`.
func TestExtractCapabilitySlices(t *testing.T) {
	src := `// header comment
@sideEffect("write")
@description("tag + release")
capability integration.github.tagRelease {
  args { repo string @required }
}

concept widget {
  capability string @required
}

@sideEffect("read")
capability fs.readFile {
  args { path string @required }
}`
	slices := extractCapabilitySlices(src)
	if len(slices) != 2 {
		t.Fatalf("extractCapabilitySlices = %d slices, want 2 (got %q)", len(slices), slices)
	}
	// The `capability string @required` concept field must NOT be sliced.
	for _, s := range slices {
		if strings.Contains(s, "concept widget") {
			t.Errorf("slice leaked into the concept body: %q", s)
		}
	}
}

// TestCheckUseImports_CapabilityResolves drives the cross-reference pass:
// a `use capabilities.<ns>.{ verb }` import of a DECLARED capability
// resolves, an undeclared one fails with a precise diagnostic.
func TestCheckUseImports_CapabilityResolves(t *testing.T) {
	r := &crossRefResolver{bundleByKind: map[string]map[string]bool{}}

	// Declared in the embedded tree -> resolves.
	ok := SandboxConstruct{
		Kind:   "logic",
		Name:   "usesTagRelease",
		Source: "use capabilities.integration.github.{ tagRelease }\n",
	}
	if msg := r.checkUseImports(ok, "origin"); msg != "" {
		t.Errorf("declared capability import should resolve, got: %s", msg)
	}

	// Not declared anywhere -> fails.
	bad := SandboxConstruct{
		Kind:   "logic",
		Name:   "usesGhost",
		Source: "use capabilities.integration.github.{ ghostVerb }\n",
	}
	msg := r.checkUseImports(bad, "origin")
	if msg == "" {
		t.Fatal("undeclared capability import should fail cross-ref")
	}
	if !strings.Contains(msg, "unresolved capability") || !strings.Contains(msg, "integration.github.ghostVerb") {
		t.Errorf("diagnostic %q is missing the unresolved-capability detail", msg)
	}
}

// TestCheckUseImports_BundleSiblingCapabilityResolves: a capability the
// SAME bundle declares resolves even though it is not in the embedded
// registry.
func TestCheckUseImports_BundleSiblingCapabilityResolves(t *testing.T) {
	r := &crossRefResolver{
		bundleByKind: map[string]map[string]bool{
			"capability": {"integration.acme.doThing": true},
		},
	}
	c := SandboxConstruct{
		Kind:   "logic",
		Name:   "usesAcme",
		Source: "use capabilities.integration.acme.{ doThing }\n",
	}
	if msg := r.checkUseImports(c, "origin"); msg != "" {
		t.Errorf("bundle-sibling capability import should resolve, got: %s", msg)
	}
}
