package memql

import (
	"log/slog"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestSpecDeclToSpec_AcceptsShapeAndEnabled guards #1031: a spec carrying
// @enabled + @shape (e.g. dsl/deployment/specs.memql's requiresOwnerOrAdmin)
// must convert cleanly. Both annotations are in the "Spec" receiver surface
// in component/language/annotations; before the fix specDeclToSpec rejected
// them and LoadUnifiedSpecs silently dropped the spec at boot.
func TestSpecDeclToSpec_AcceptsShapeAndEnabled(t *testing.T) {
	src := `@enabled
@description("Caller must hold owner or admin role.")
@shape("actorEnvelope")
spec requiresOwnerOrAdminFixture {
  actor.role == "admin" || actor.role == "owner"
}`
	decl, err := languageParser.ParseSpecDecl(src)
	if err != nil {
		t.Fatalf("ParseSpecDecl: %v", err)
	}
	spec, err := specDeclToSpec(decl, "test:spec")
	if err != nil {
		t.Fatalf("specDeclToSpec rejected a valid @shape/@enabled spec: %v", err)
	}
	if spec.Name != "requiresOwnerOrAdminFixture" {
		t.Errorf("Name = %q, want requiresOwnerOrAdminFixture", spec.Name)
	}
	if spec.Description != "Caller must hold owner or admin role." {
		t.Errorf("Description = %q, want the @description body", spec.Description)
	}
	if spec.Kind != SpecKindContext {
		t.Errorf("Kind = %q, want %q (actor-only body)", spec.Kind, SpecKindContext)
	}
}

// TestSpecDeclToSpec_RejectsUnknownAnnotation confirms the surface still
// rejects an annotation that is not in the Spec receiver set, so a typo'd
// or misplaced annotation is a hard error rather than a silent drop.
func TestSpecDeclToSpec_RejectsUnknownAnnotation(t *testing.T) {
	// @public is a valid annotation for Query/Mutation but not for Spec.
	src := `@public
spec specWithMisplacedAnnotation {
  actor.role == "admin"
}`
	decl, err := languageParser.ParseSpecDecl(src)
	if err != nil {
		t.Fatalf("ParseSpecDecl: %v", err)
	}
	if _, err := specDeclToSpec(decl, "test:spec"); err == nil {
		t.Fatal("specDeclToSpec accepted @public on a spec; want a rejection")
	}
}

// TestUnifiedSpecs_RegistersShapeAnnotatedSpec is the end-to-end regression
// for #1031: the real dsl/deployment/specs.memql requiresOwnerOrAdmin (which
// carries @enabled + @shape) must register, not be silently skipped by the
// load gate.
func TestUnifiedSpecs_RegistersShapeAnnotatedSpec(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	reg := newSpecRegistry()
	if _, err := LoadUnifiedSpecs(logger, reg); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}
	if !reg.Has("requiresOwnerOrAdmin") {
		t.Error("requiresOwnerOrAdmin not registered: a @shape/@enabled-bearing spec was silently dropped at load (#1031)")
	}
}
