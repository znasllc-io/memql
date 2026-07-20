package parser

import (
	"strings"
	"testing"
)

// forEachIDs parses procedural source and returns its forEach step ids
// in order, mirroring TestParser_ForRange_GeneratesUniqueForEachStepIds.
func forEachIDs(t *testing.T, src string) []string {
	t.Helper()
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	node, err := NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	file, ok := node.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", node)
	}
	var ids []string
	for _, def := range file.Definitions {
		fn, ok := def.(*FunctionDef)
		if !ok {
			continue
		}
		auto, ok := fn.Body.(*AutomationDef)
		if !ok {
			continue
		}
		for _, step := range auto.Steps {
			if step.Type == StepTypeForEach {
				ids = append(ids, step.ID)
			}
		}
	}
	return ids
}

const forEachFixture = `
@enabled
func (Automation) sweepThings(_ any) {
  q := query { concept==v1:test }
  for item := range q.result {
    _ := query { concept==v1:test }
  }
  return q
}`

// TestForEachStepIDsSurviveUpstreamEdits is the #2659 regression. Step
// ids are PERSISTED -- component/automations/checkpoint.go keys step
// results by id with a TTL -- and the old
// `forEach_<var>_<charOffset>_L<line>` form renamed a step whenever
// anything above it changed the character count or line numbering. A
// comment reflow was enough to break resume for an in-flight run. The
// id must depend only on the loop's own construct.
func TestForEachStepIDsSurviveUpstreamEdits(t *testing.T) {
	base := forEachIDs(t, forEachFixture)
	if len(base) != 1 {
		t.Fatalf("fixture must yield exactly one forEach id, got %v", base)
	}

	// Each edit changes the byte count and/or line numbers ABOVE the
	// loop while leaving the loop itself untouched.
	for name, edited := range map[string]string{
		"comment added above":  "// a newly added explanatory comment\n" + forEachFixture,
		"comment lengthened":   strings.Replace(forEachFixture, "@enabled", "// substantially longer prose than was here before\n@enabled", 1),
		"step renamed above":   strings.Replace(forEachFixture, "q := query", "queryResults := query", 1),
		"blank lines inserted": strings.Replace(forEachFixture, "@enabled", "\n\n@enabled", 1),
	} {
		got := forEachIDs(t, strings.Replace(edited, "q.result", replacementFor(edited), 1))
		if len(got) != 1 || got[0] != base[0] {
			t.Errorf("%s: forEach id changed from %v to %v -- an upstream edit must not rename a persisted step id (#2659)", name, base, got)
		}
	}
}

// replacementFor keeps the renamed-step variant self-consistent.
func replacementFor(src string) string {
	if strings.Contains(src, "queryResults := query") {
		return "queryResults.result"
	}
	return "q.result"
}

// Two loops in one construct must stay distinct (the property the
// character offset used to provide), and numbering restarts per
// construct so one construct's loops never depend on another's.
func TestForEachStepIDsAreDistinctAndPerConstruct(t *testing.T) {
	two := `
@enabled
func (Automation) twoLoops(_ any) {
  q := query { concept==v1:test }
  for item := range q.result {
    _ := query { concept==v1:test }
  }
  for item := range q.result {
    _ := query { concept==v1:test }
  }
  return q
}`
	ids := forEachIDs(t, two)
	if len(ids) != 2 {
		t.Fatalf("want two forEach ids, got %v", ids)
	}
	if ids[0] == ids[1] {
		t.Errorf("same-variable loops in one construct must stay distinct, got %v", ids)
	}

	pair := forEachFixture + "\n" + strings.Replace(forEachFixture, "sweepThings", "otherSweep", 1)
	pairIDs := forEachIDs(t, pair)
	if len(pairIDs) != 2 {
		t.Fatalf("want one forEach id per construct, got %v", pairIDs)
	}
	if pairIDs[0] != pairIDs[1] {
		t.Errorf("ordinals must restart per construct, so the first loop of each carries the same id; got %v", pairIDs)
	}
}
