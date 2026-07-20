package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// #2617: constructs of a file's own domain are ambient -- in scope with
// no `use` line. The plan concept is the live ambiguity in the tree
// (v1:planner:plan AND v1:harness:plan share the trailing segment), so
// it exercises every branch: the domain hint disambiguates where the
// unhinted resolution errors, and an explicit import still wins.

func TestDomainFromFilePath(t *testing.T) {
	cases := map[string]string{
		"planner/queries.memql":                           "planner",
		"cognition/mutations.memql":                       "cognition",
		"deployment/v1/x.memql":                           "deployment", // legacy version dir skipped
		"queries.memql":                                   "",
		"./identity/mutations.memql":                      "identity",
		"a/b/planner/queries.memql":                       "planner",
		"unified:worker/queries.memql:invocationsForPlan": "worker",
		"dryrun:cognition/automations.memql:bootstrap":    "cognition",
		"worker/queries.memql:invocationsForPlan":         "worker",
	}
	for in, want := range cases {
		if got := DomainFromFilePath(in); got != want {
			t.Errorf("DomainFromFilePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveCanonicalIdConceptRefs_AmbientDomain(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	resolver := NewConceptResolver(memoryNodes.DefaultRegistry())

	src := `mutate plan probe {
  insert {
    id: canonicalId(args.planId, plan)
  }
}`
	// Ambient: the file's own domain resolves the bare name with no import.
	got, err := resolver.ResolveCanonicalIdConceptRefsInDomain(src, "planner")
	if err != nil {
		t.Fatalf("ambient same-domain canonicalId: %v", err)
	}
	if !strings.Contains(got, `canonicalId(args.planId, "v1:planner:plan")`) {
		t.Errorf("ambient resolution: got %q, want the v1:planner:plan string form", got)
	}
	// The same source in the harness domain binds the harness concept.
	got, err = resolver.ResolveCanonicalIdConceptRefsInDomain(src, "harness")
	if err != nil {
		t.Fatalf("ambient harness canonicalId: %v", err)
	}
	if !strings.Contains(got, `"v1:harness:plan"`) {
		t.Errorf("harness ambient resolution: got %q", got)
	}

	// No domain, no import: the pre-#2617 hard error stands.
	if _, err := resolver.ResolveCanonicalIdConceptRefsInDomain(src, ""); err == nil {
		t.Error("no import + no domain: want the not-in-scope error, got nil")
	}

	// Cross-domain without an import still errors -- ambient scope is
	// same-domain ONLY, so the import discipline stays lint-enforceable.
	cross := `mutate widget probe {
  insert {
    id: canonicalId(args.spaceId, space)
  }
}`
	if _, err := resolver.ResolveCanonicalIdConceptRefsInDomain(cross, "planner"); err == nil {
		t.Error("cross-domain concept without import: want error, got nil")
	}

	// An explicit import wins over the ambient domain: the file sits in
	// planner/ but imports the harness plan explicitly.
	imported := "use harness.concepts.{ plan }\n" + src
	got, err = resolver.ResolveCanonicalIdConceptRefsInDomain(imported, "planner")
	if err != nil {
		t.Fatalf("explicit import beside ambient domain: %v", err)
	}
	if !strings.Contains(got, `"v1:harness:plan"`) {
		t.Errorf("explicit import must win over ambient domain: got %q", got)
	}
}

func TestResolveFileWithSignatureConcepts_AmbientDomain(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	resolver := NewConceptResolver(memoryNodes.DefaultRegistry())

	// The resolver consumes only file.Uses + the signature-concept list,
	// so a bare File models "no imports" directly.
	// Without the domain the bare `plan` signature is ambiguous.
	if err := resolver.ResolveFileWithSignatureConceptsInDomain(&languageParser.File{}, "v1", []string{"plan"}, ""); err == nil {
		t.Error("ambiguous signature concept with no domain: want error, got nil")
	}
	// The ambient domain disambiguates.
	if err := resolver.ResolveFileWithSignatureConceptsInDomain(&languageParser.File{}, "v1", []string{"plan"}, "planner"); err != nil {
		t.Errorf("ambient domain must disambiguate the signature concept: %v", err)
	}
}
