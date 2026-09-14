package memql

import (
	"log/slog"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestSpecDeclToSpec_AcceptsBindingAndEnabled guards #1031 under the epic
// #2281 binding model: a spec carrying @enabled plus a signature binding
// (e.g. dsl/deployment/specs.memql's requiresOwnerOrAdmin) must convert
// cleanly. @enabled is in the "Spec" receiver surface in
// component/language/annotations; the bound name moved from the retired
// @shape pin to the signature. Classification (row vs context) is deferred
// to the engine-bootstrap binding resolver, so Kind is empty at conversion.
func TestSpecDeclToSpec_AcceptsBindingAndEnabled(t *testing.T) {
	src := `
@description("Caller must hold owner or admin role.")
spec actorEnvelope requiresOwnerOrAdminFixture = actor => actor.role == "admin" || actor.role == "owner"`
	decl, err := languageParser.ParseSpecDecl(src)
	if err != nil {
		t.Fatalf("ParseSpecDecl: %v", err)
	}
	spec, err := specDeclToSpec(decl, "test:spec")
	if err != nil {
		t.Fatalf("specDeclToSpec rejected a valid bound @enabled spec: %v", err)
	}
	if spec.Name != "requiresOwnerOrAdminFixture" {
		t.Errorf("Name = %q, want requiresOwnerOrAdminFixture", spec.Name)
	}
	if spec.Description != "Caller must hold owner or admin role." {
		t.Errorf("Description = %q, want the @description body", spec.Description)
	}
	if spec.BoundName != "actorEnvelope" {
		t.Errorf("BoundName = %q, want actorEnvelope", spec.BoundName)
	}
}

// TestSpecDeclToSpec_RejectsUnknownAnnotation confirms the surface still
// rejects an annotation that is not in the Spec receiver set, so a typo'd
// or misplaced annotation is a hard error rather than a silent drop. The
// PARSER is the one gate (memql#2395, and since memql#5359 the annotation
// registry's check every construct runs); the converter's second copy of the
// list is gone with the rest of the duplicate tables.
func TestSpecDeclToSpec_RejectsUnknownAnnotation(t *testing.T) {
	// @public is a valid annotation for Query/Mutation but not for Spec.
	src := `@public
spec actorEnvelope specWithMisplacedAnnotation = actor => actor.role == "admin"`
	_, err := languageParser.ParseSpecDecl(src)
	if err == nil {
		t.Fatal("ParseSpecDecl accepted @public on a spec; want a parse-time rejection (memql#2395)")
	}
	if !strings.Contains(err.Error(), "@public is not valid on a spec or trait") || !strings.HasSuffix(err.Error(), "[annotation_misplaced]") {
		t.Fatalf("want the registry's misplaced refusal, got: %v", err)
	}
}

// TestUnifiedSpecs_RegistersShapeAnnotatedSpec is the end-to-end regression
// for #1031: a real annotated context-spec from the tree must register, not be
// silently skipped by the load gate.
//
// IT NAMED `requiresOwnerOrAdmin` UNTIL EPIC memql#5166, which deleted that
// spec along with the two other role-comparing ones -- a slug comparison cannot
// see a custom role, so a role question is `@requiresRank` or
// `@requiresCapability` now. `requiresOwner` is its surviving sibling in the
// same file, carries the same annotation shape, and asks about the ACTOR rather
// than about a rung, which is what a context-spec is still for.
func TestUnifiedSpecs_RegistersShapeAnnotatedSpec(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	reg := newSpecRegistry()
	if _, err := LoadUnifiedSpecs(logger, reg); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}
	if !reg.Has("requiresOwner") {
		t.Error("requiresOwner not registered: a @shape/@enabled-bearing spec was silently dropped at load (#1031)")
	}
}
