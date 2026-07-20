package memql

import (
	"log/slog"
	"os"
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

// TestSignatureBindingBeatsSingleUseImport pins the precedence fix the
// #2661 boot-equivalence probe forced: a construct whose binding lives
// in its SIGNATURE must never take the legacy single-use-import
// binding, no matter how many use lines survive the #2617 strip.
// worker/queries.memql keeps exactly one (cross-domain) use line, so
// without the gate every worker query silently bound v1:planner:plan
// -- and ensureBoundConceptFilter would have injected that into their
// row filters. The actions mutations pin the flip side: their
// BoundConcept now matches the signature (main's latent candidate
// label was inert -- the mutation executor reads
// MutationTemplate.Concept, proven identical both sides).
func TestSignatureBindingBeatsSingleUseImport(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	for name, want := range map[string]string{
		"invocationsForPlan":       "v1:worker:invocation",
		"invocationsForUser":       "v1:worker:invocation",
		"expiredWorkerInvocations": "v1:worker:invocation",
		"workerByIdentityId":       "v1:worker:registration",
		"bumpActionVersion":        "v1:actions:action",
		"registerSurface":          "v1:actions:surface",
	} {
		fn, err := registry.Get(name)
		if err != nil {
			t.Errorf("%s: not registered: %v", name, err)
			continue
		}
		if fn.BoundConcept != want {
			t.Errorf("%s: BoundConcept=%q, want %q (signature must beat the single-use import)", name, fn.BoundConcept, want)
		}
	}

	// The legacy heuristic must also never bind a signatureless
	// non-query/mutation construct: the identity sweeps' logic file
	// keeps a single platform use line post-strip and stays unbound.
	fn, err := registry.Get("accountDeletionSweep")
	if err != nil {
		t.Fatalf("accountDeletionSweep: %v", err)
	}
	if fn.BoundConcept != "" {
		t.Errorf("accountDeletionSweep: BoundConcept=%q, want empty (legacy binding is query/mutation-only)", fn.BoundConcept)
	}
}
