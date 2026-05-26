package parser

import (
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// TestParseSeedDecl_GoldenPathAgent locks the canonical agent-seed
// shape: file-top Form B import + @scope + @templateFile +
// @description, two-identifier `seed <Concept> <name>` signature,
// nested provider-config block, kebab-case identifier in a string
// value.
func TestParseSeedDecl_GoldenPathAgent(t *testing.T) {
	source := `use agents.concepts.{ agent }

@scope("perUser")
@templateFile("templates/assistant.tmpl")
@description("Per-user Assistant baseline.")
seed agent assistant {
  name:        "Assistant"
  description: "Designated fallback."
  role:        "assistant"
  providerConfig {
    llm {
      policyName: "balancedChat"
      temperature: 0.7
      maxTokens: 4000
    }
  }
}`
	decl, err := ParseSeedDecl(source)
	if err != nil {
		t.Fatalf("ParseSeedDecl: %v", err)
	}
	if decl.Name != "assistant" {
		t.Errorf("Name = %q, want assistant", decl.Name)
	}
	if decl.SignatureConcept != "agent" {
		t.Errorf("SignatureConcept = %q, want agent", decl.SignatureConcept)
	}
	if decl.Scope != "perUser" {
		t.Errorf("Scope = %q, want perUser", decl.Scope)
	}
	if decl.TemplateFile != "templates/assistant.tmpl" {
		t.Errorf("TemplateFile = %q, want templates/assistant.tmpl", decl.TemplateFile)
	}
	if decl.Description != "Per-user Assistant baseline." {
		t.Errorf("Description = %q", decl.Description)
	}
	if decl.Body == nil {
		t.Fatal("Body is nil")
	}
	if got := decl.Body.Fields["name"]; got == nil || got.Kind != ast.SeedValueString || got.String != "Assistant" {
		t.Errorf("Body.name = %+v, want SeedValueString:\"Assistant\"", got)
	}
	pc, ok := decl.Body.Nested["providerConfig"]
	if !ok || pc == nil {
		t.Fatalf("Body.providerConfig missing")
	}
	llm, ok := pc.Nested["llm"]
	if !ok || llm == nil {
		t.Fatalf("Body.providerConfig.llm missing")
	}
	if v := llm.Fields["policyName"]; v == nil || v.String != "balancedChat" {
		t.Errorf("llm.policyName = %+v, want balancedChat", v)
	}
	if v := llm.Fields["temperature"]; v == nil || v.Kind != ast.SeedValueFloat || v.Float != 0.7 {
		t.Errorf("llm.temperature = %+v, want SeedValueFloat:0.7", v)
	}
	if v := llm.Fields["maxTokens"]; v == nil || v.Kind != ast.SeedValueInt || v.Int != 4000 {
		t.Errorf("llm.maxTokens = %+v, want SeedValueInt:4000", v)
	}
}

// TestParseSeedDecl_KebabCaseSeedName covers the role catalog
// pattern: `seed agentRole graphic-designer { ... }` -- the seed
// name is kebab-case, accepted because the lexer treats `-` as an
// identifier-continuation character.
func TestParseSeedDecl_KebabCaseSeedName(t *testing.T) {
	source := `use agents.concepts.{ agentRole }

@description("Visual identity, layout, typography, color, and brand-system design.")
seed agentRole graphic-designer {
  slug:                  "graphic-designer"
  name:                  "Graphic Designer"
  category:              "creative"
  predefined:            true
}`
	decl, err := ParseSeedDecl(source)
	if err != nil {
		t.Fatalf("ParseSeedDecl: %v", err)
	}
	if decl.Name != "graphic-designer" {
		t.Errorf("Name = %q, want graphic-designer", decl.Name)
	}
	if decl.SignatureConcept != "agentRole" {
		t.Errorf("SignatureConcept = %q, want agentRole", decl.SignatureConcept)
	}
	if v := decl.Body.Fields["predefined"]; v == nil || v.Kind != ast.SeedValueBool || !v.Bool {
		t.Errorf("predefined = %+v, want SeedValueBool:true", v)
	}
}

// TestParseSeedDecl_StringArrayValue locks the array-of-strings
// value form: `lockedSkillIds: ["workbench-baseline", "creative-baseline"]`.
// Empty arrays (`defaultSkillIds: []`) also need to parse.
func TestParseSeedDecl_StringArrayValue(t *testing.T) {
	source := `use agents.concepts.{ agentRole }

seed agentRole tester {
  lockedSkillIds:  ["workbench-baseline", "creative-baseline"]
  defaultSkillIds: []
}`
	decl, err := ParseSeedDecl(source)
	if err != nil {
		t.Fatalf("ParseSeedDecl: %v", err)
	}
	locked := decl.Body.Fields["lockedSkillIds"]
	if locked == nil || locked.Kind != ast.SeedValueStringArray {
		t.Fatalf("lockedSkillIds = %+v, want SeedValueStringArray", locked)
	}
	want := []string{"workbench-baseline", "creative-baseline"}
	if !reflect.DeepEqual(locked.StringArray, want) {
		t.Errorf("lockedSkillIds.StringArray = %v, want %v", locked.StringArray, want)
	}
	def := decl.Body.Fields["defaultSkillIds"]
	if def == nil || def.Kind != ast.SeedValueStringArray {
		t.Fatalf("defaultSkillIds = %+v, want SeedValueStringArray (empty)", def)
	}
	if len(def.StringArray) != 0 {
		t.Errorf("defaultSkillIds.StringArray = %v, want []", def.StringArray)
	}
}

// TestParseSeedDecl_GlobalScopeSimpleSeed covers the simple global
// seed shape used by platform / cluster bootstrap seeds: no
// nested blocks, just scalar field assignments and the
// single-identifier `seed <name>` form gated by a file-top
// `use <ns>.<concept>` clause (legacy authoring shape still on
// disk for some platform seeds).
func TestParseSeedDecl_GlobalScopeSimpleSeed(t *testing.T) {
	source := `use agents.concepts.{ skill }

@scope("global")
@description("Workbench baseline skill -- shell + fs + http on the sandboxed Linux workbench.")
seed skill workbench-baseline {
  slug:        "workbench-baseline"
  name:        "Workbench Baseline"
  category:    "baseline"
  tier:        "A"
  predefined:  true
  maxAgents:   0
}`
	decl, err := ParseSeedDecl(source)
	if err != nil {
		t.Fatalf("ParseSeedDecl: %v", err)
	}
	if decl.Scope != "global" {
		t.Errorf("Scope = %q, want global", decl.Scope)
	}
	if decl.Name != "workbench-baseline" {
		t.Errorf("Name = %q, want workbench-baseline", decl.Name)
	}
	if v := decl.Body.Fields["maxAgents"]; v == nil || v.Kind != ast.SeedValueInt || v.Int != 0 {
		t.Errorf("maxAgents = %+v, want SeedValueInt:0", v)
	}
}

// TestParseSeedDecl_InsertionOrderPreserved verifies that Keys
// preserves the author's declaration order (Fields + Nested are
// not, by themselves, ordered maps). This matters for the
// materialiser's downstream rendering and for deterministic
// diagnostics.
func TestParseSeedDecl_InsertionOrderPreserved(t *testing.T) {
	source := `use agents.concepts.{ agent }

seed agent ordered {
  delta:   "d"
  alpha:   "a"
  gamma {
    nestedFirst: "1"
  }
  beta:    "b"
}`
	decl, err := ParseSeedDecl(source)
	if err != nil {
		t.Fatalf("ParseSeedDecl: %v", err)
	}
	got := decl.Body.Keys
	want := []string{"delta", "alpha", "gamma", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Body.Keys = %v, want %v", got, want)
	}
}

// TestParseSeedDecl_UnknownAnnotationRejected mirrors
// parseSeedMemQL's hard-fail default branch: any annotation
// outside the seed allow-list errors at parse time. This is the
// behavior parity test that catches a sibling PR's drift.
func TestParseSeedDecl_UnknownAnnotationRejected(t *testing.T) {
	source := `use agents.concepts.{ agent }

@nope("something")
seed agent oops { }`
	_, err := ParseSeedDecl(source)
	if err == nil {
		t.Fatal("ParseSeedDecl: expected error for unknown annotation, got nil")
	}
	if !strings.Contains(err.Error(), "@nope") && !strings.Contains(err.Error(), "unknown seed annotation") {
		t.Errorf("error %q does not mention the unknown annotation", err.Error())
	}
}

// TestParseSeedDecl_ScopeEnumGuard checks @scope only accepts
// "global" / "perUser". Any other value fails at parse time,
// matching parseSeedMemQL's enum guard.
func TestParseSeedDecl_ScopeEnumGuard(t *testing.T) {
	source := `use agents.concepts.{ agent }

@scope("clusterWide")
seed agent wrongScope { }`
	_, err := ParseSeedDecl(source)
	if err == nil {
		t.Fatal("expected error for invalid @scope, got nil")
	}
	if !strings.Contains(err.Error(), "@scope") {
		t.Errorf("error %q does not mention @scope", err.Error())
	}
}

// TestParseSeedDecl_NumericLiteralForms covers the int / float
// discrimination in parseSeedValue. `0` / `0.0` / negative ints
// / scientific notation all need to land in the right
// SeedValueKind.
func TestParseSeedDecl_NumericLiteralForms(t *testing.T) {
	source := `use agents.concepts.{ agent }

seed agent nums {
  zero:        0
  intVal:      42
  negInt:      -7
  floatVal:    1.5
  zeroFloat:   0.0
}`
	decl, err := ParseSeedDecl(source)
	if err != nil {
		t.Fatalf("ParseSeedDecl: %v", err)
	}
	cases := []struct {
		key      string
		wantKind ast.SeedValueKind
		wantInt  int64
		wantF    float64
	}{
		{"zero", ast.SeedValueInt, 0, 0},
		{"intVal", ast.SeedValueInt, 42, 0},
		{"negInt", ast.SeedValueInt, -7, 0},
		{"floatVal", ast.SeedValueFloat, 0, 1.5},
		{"zeroFloat", ast.SeedValueFloat, 0, 0.0},
	}
	for _, tc := range cases {
		v := decl.Body.Fields[tc.key]
		if v == nil {
			t.Errorf("%s: missing", tc.key)
			continue
		}
		if v.Kind != tc.wantKind {
			t.Errorf("%s: Kind = %d, want %d", tc.key, v.Kind, tc.wantKind)
		}
		if v.Kind == ast.SeedValueInt && v.Int != tc.wantInt {
			t.Errorf("%s: Int = %d, want %d", tc.key, v.Int, tc.wantInt)
		}
		if v.Kind == ast.SeedValueFloat && v.Float != tc.wantF {
			t.Errorf("%s: Float = %f, want %f", tc.key, v.Float, tc.wantF)
		}
	}
}
