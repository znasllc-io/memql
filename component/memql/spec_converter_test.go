package memql

import (
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
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
	src := `@enabled
@description("Caller must hold owner or admin role.")
spec actorEnvelope requiresOwnerOrAdminFixture {
  return role == "admin" || role == "owner"
}`
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
// or misplaced annotation is a hard error rather than a silent drop. Since
// memql#2395 the PARSER already rejects it (validateDeclAnnotations against
// the same registry); the converter check remains as defense-in-depth for
// programmatically-built decls that never pass through the parser.
func TestSpecDeclToSpec_RejectsUnknownAnnotation(t *testing.T) {
	// @public is a valid annotation for Query/Mutation but not for Spec.
	src := `@public
spec actorEnvelope specWithMisplacedAnnotation {
  return role == "admin"
}`
	if _, err := languageParser.ParseSpecDecl(src); err == nil {
		t.Fatal("ParseSpecDecl accepted @public on a spec; want a parse-time rejection (memql#2395)")
	}

	// Converter layer: a decl built directly (bypassing the parser) still
	// rejects the misplaced annotation.
	decl := &ast.SpecDecl{
		Name:      "specWithMisplacedAnnotation",
		BoundName: "actorEnvelope",
		Attributes: []*ast.Attribute{
			{Name: "public"},
		},
		Body: &ast.ComparisonExpr{},
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
