package memql

import (
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// concept_ssot_test.go covers the engine-side half of C5 (memql#2035):
//   - an empty-body, signature-bound shape becomes a DefaultProjection
//     marker that expandDefaultShapeProjections fills with the concept's
//     projectable (non-@internal) fields.
//   - validateMutationCallerArgs rejects writing an @internal/@serverSet
//     field from caller args, but leaves public fields (and the
//     unannotated tree) alone.

func mustConcept(t *testing.T, src, path string) *memoryNodes.Concept {
	t.Helper()
	c, err := memoryNodes.ParseConceptMemQL([]byte(src), path)
	if err != nil {
		t.Fatalf("ParseConceptMemQL(%s): %v", path, err)
	}
	return c
}

func conceptRegistry(concepts ...*memoryNodes.Concept) *memoryNodes.MemoryRegistry {
	m := map[string]*memoryNodes.Concept{}
	for _, c := range concepts {
		m[c.Name] = c
	}
	r := &memoryNodes.MemoryRegistry{}
	r.ReplaceAll(m)
	return r
}

// --- Shape default projection ----------------------------------------

func TestShapeConverter_EmptyBody_EmitsDefaultProjection(t *testing.T) {
	decl, err := languageParser.ParseShapeDecl("@row\nshape space spaceCard {\n}\n")
	if err != nil {
		t.Fatalf("ParseShapeDecl: %v", err)
	}
	shape, err := shapeDeclToShapeDefinition(decl, "test:spaceCard")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !shape.DefaultProjection {
		t.Fatalf("empty-body signature shape should be DefaultProjection; got %+v", shape)
	}
	if len(shape.Template) != 0 {
		t.Fatalf("DefaultProjection shape should start with an empty template; got %v", shape.Template)
	}
	if len(shape.UseConcepts) == 0 || shape.UseConcepts[0] != "space" {
		t.Fatalf("signature concept should be recorded; got %v", shape.UseConcepts)
	}
}

func TestShapeConverter_EmptyBody_NoSignature_Errors(t *testing.T) {
	// An empty @actor shape has no concept to derive a default from.
	decl, err := languageParser.ParseShapeDecl("@actor\nshape actorEnvelope {\n}\n")
	if err != nil {
		t.Fatalf("ParseShapeDecl: %v", err)
	}
	if _, err := shapeDeclToShapeDefinition(decl, "test:actorEnvelope"); err == nil {
		t.Fatal("expected an error: empty body with no signature concept")
	}
}

func TestExpandDefaultShapeProjections_ProjectsPublicNotInternal(t *testing.T) {
	concept := mustConcept(t, `
concept Space {
  name        string  @required
  description string
  status      string  @serverSet
  secretSalt  string  @internal
}
`, "v1/cognition/space")

	// Build a DefaultProjection shape via the converter.
	decl, err := languageParser.ParseShapeDecl("@row\nshape space spaceCard {\n}\n")
	if err != nil {
		t.Fatalf("ParseShapeDecl: %v", err)
	}
	shape, err := shapeDeclToShapeDefinition(decl, "test:spaceCard")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	shapes := newShapeRegistry()
	if err := shapes.Upsert(shape); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	n := expandDefaultShapeProjections(nil, shapes, conceptRegistry(concept))
	if n != 1 {
		t.Fatalf("expected 1 shape expanded, got %d", n)
	}

	got, _ := shapes.Get("spaceCard")
	keys := make([]string, 0, len(got.Template))
	for k := range got.Template {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Projectable = public + serverSet, minus @internal.
	want := []string{"description", "name", "status"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("projected keys = %v, want %v", keys, want)
	}
	if _, bad := got.Template["secretSalt"]; bad {
		t.Fatal("@internal field secretSalt must NEVER be auto-projected")
	}
	// The template value must be the canonical node("payload.X") form.
	if v, _ := got.Template["name"].(string); !strings.Contains(v, "payload.name") {
		t.Fatalf("template value should reference payload.name; got %q", v)
	}
}

// --- Mutation caller-arg gate ----------------------------------------

func TestValidateMutationCallerArgs_RejectsSensitiveFromArgs(t *testing.T) {
	concept := mustConcept(t, `
concept Space {
  name       string  @required
  status     string  @serverSet
  secretSalt string  @internal
}
`, "v1/cognition/space")
	reg := conceptRegistry(concept)

	// @serverSet field written from args -> rejected.
	err := validateMutationCallerArgs(reg, "v1:cognition:space", "mutBad", map[string]any{
		"name":   "args.name",
		"status": "args.status",
	})
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("expected rejection naming @serverSet field status; got %v", err)
	}

	// @internal field written from args -> rejected.
	err = validateMutationCallerArgs(reg, "v1:cognition:space", "mutBad2", map[string]any{
		"secretSalt": "args.secretSalt",
	})
	if err == nil || !strings.Contains(err.Error(), "secretSalt") {
		t.Fatalf("expected rejection naming @internal field secretSalt; got %v", err)
	}
}

func TestValidateMutationCallerArgs_AllowsPublicAndStamps(t *testing.T) {
	concept := mustConcept(t, `
concept Space {
  name       string  @required
  status     string  @serverSet
  secretSalt string  @internal
}
`, "v1/cognition/space")
	reg := conceptRegistry(concept)

	// Public field from args + server-stamped (non-args) sensitive
	// fields -> allowed. id is a row intrinsic (not a concept field).
	err := validateMutationCallerArgs(reg, "v1:cognition:space", "mutGood", map[string]any{
		"id":         "args.id",
		"name":       "args.name",
		"status":     `"active"`,     // literal, server-stamped -- OK
		"secretSalt": "actor.userId", // server context, not args -- OK
	})
	if err != nil {
		t.Fatalf("expected no error for public-from-args + server-stamped sensitive; got %v", err)
	}
}

func TestValidateMutationCallerArgs_UnannotatedConceptNoOp(t *testing.T) {
	concept := mustConcept(t, `
concept Widget {
  label string @required
  count int
}
`, "v1/test/widget")
	reg := conceptRegistry(concept)
	// No annotations anywhere -> binding any field from args is fine.
	err := validateMutationCallerArgs(reg, "v1:test:widget", "mut", map[string]any{
		"label": "args.label",
		"count": "args.count",
	})
	if err != nil {
		t.Fatalf("unannotated concept must be a no-op; got %v", err)
	}
}
