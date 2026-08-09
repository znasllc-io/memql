package gen

import (
	"strings"
	"testing"
)

// TestResolveConceptId_SameAndCrossNamespace pins the core A3 (#2442)
// resolution: a construct's signature-bound concept SHORT name resolves to its
// canonical id -- same-namespace from its own directory, cross-namespace
// through a file-top `use <dir>.concepts.{ name }` import -- and sub-namespace
// concepts (@namespace carrying a colon) keep their multi-segment id.
func TestResolveConceptId_SameAndCrossNamespace(t *testing.T) {
	root := t.TempDir()
	// Concept definitions: one plain, one sub-namespaced.
	writeFixture(t, root, "cognition/concepts.memql", `
@version("1.0.0")
@namespace("cognition")
concept space {
  ownerUserId string @required
}

@version("1.0.0")
@namespace("cognition")
concept participant {
  spaceId string @required
}

@version("1.0.0")
@namespace("cognition:turn")
concept state {
  turnId string @required
}
`)
	// Same-namespace binding (query in cognition/ binds cognition's space).
	writeFixture(t, root, "cognition/queries.memql", `
@description("Same-namespace bind")
query space querySpaceLocal {
  args { ownerId string @required }
  filter ownerUserId==args.ownerId
  shape spaceCard
}

@description("Sub-namespace bind")
query state queryTurnState {
  args { turnId string @required }
  filter turnId==args.turnId
  shape stateCard
}
`)
	// Cross-namespace binding: a query in common/ binds cognition's space via a
	// file-top `use` import.
	writeFixture(t, root, "common/queries.memql", `
use cognition.concepts.{ space }

@description("Cross-namespace bind")
query space querySpaceCrossNs {
  args { ownerId string @required }
  filter ownerUserId==args.ownerId
  shape spaceCard
}
`)

	constructs, err := CollectAndMerge([]string{root})
	if err != nil {
		t.Fatalf("CollectAndMerge: %v", err)
	}
	idx := buildConceptIndex([]string{root})
	got := map[string]string{}
	for _, c := range constructs {
		got[c.Name] = resolveConceptId(c.Concept, c.Dir, c.Imports, idx)
	}

	want := map[string]string{
		"querySpaceLocal":   "v1:cognition:space",
		"queryTurnState":    "v1:cognition:turn:state",
		"querySpaceCrossNs": "v1:cognition:space",
	}
	for name, wantID := range want {
		if got[name] != wantID {
			t.Errorf("resolveConceptId(%s) = %q, want %q", name, got[name], wantID)
		}
	}
}

// TestGenerate_FailsOnCanonicalArgPattern is contract test (a): generation
// FAILS loudly when a client-facing arg carries a canonical-id `@pattern`
// (belt-and-suspenders with the A1 conformance rule). A bare-slug pattern
// generates cleanly.
func TestGenerate_FailsOnCanonicalArgPattern(t *testing.T) {
	// Bad: a `^v1:` anchored pattern forces the caller to compose a canonical id.
	bad := t.TempDir()
	out := t.TempDir()
	writeFixture(t, bad, "widget/queries.memql", `
@description("Forces canonical")
query widget queryWidgetByCanonical {
  args {
    widgetId string @required @pattern("^v1:[a-z0-9]+:[a-z0-9_]+:[a-zA-Z0-9_-]{1,128}$")
  }
  filter id==args.widgetId
  shape widgetCard
}
`)
	if _, err := Generate(Options{Roots: []string{bad}, GoOut: out}); err == nil {
		t.Fatalf("expected Generate to fail on a canonical-id @pattern, got nil error")
	} else if !strings.Contains(err.Error(), "canonical-id @pattern") || !strings.Contains(err.Error(), "queryWidgetByCanonical.widgetId") {
		t.Errorf("error should name the offending construct.arg + the rule, got: %v", err)
	}

	// Good: a bare-slug pattern is fine.
	good := t.TempDir()
	writeFixture(t, good, "widget/queries.memql", `
@description("Bare slug ok")
query widget queryWidgetBare {
  args {
    widgetId string @required @pattern("^[a-zA-Z0-9_-]{1,128}$")
  }
  filter id==args.widgetId
  shape widgetCard
}
`)
	if _, err := Generate(Options{Roots: []string{good}, GoOut: t.TempDir()}); err != nil {
		t.Errorf("bare-slug @pattern should generate cleanly, got: %v", err)
	}
}

// TestEmitConcepts_ShapeAndTopics verifies the generated concept-metadata
// artifact carries the machine-readable maps a consumer needs: the concept
// registry, per-construct bound concepts, and the CDC topic + filter forms in
// the exact `graph.node.<action>.<concept>` / `node.<action>.<concept>` grammar
// the event bus emits + subscriptions filter on.
func TestEmitConcepts_ShapeAndTopics(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "cognition/concepts.memql", `
@version("1.0.0")
@namespace("cognition")
concept participant {
  spaceId string @required
}
`)
	writeFixture(t, root, "cognition/queries.memql", `
@description("List participants")
query participant spaceParticipants {
  args { spaceId string @required }
  filter spaceId==args.spaceId
  shape participantCard
}
`)
	constructs, err := CollectAndMerge([]string{root})
	if err != nil {
		t.Fatalf("CollectAndMerge: %v", err)
	}
	idx := buildConceptIndex([]string{root})
	for i := range constructs {
		constructs[i].ConceptId = resolveConceptId(constructs[i].Concept, constructs[i].Dir, constructs[i].Imports, idx)
	}
	registry := conceptRegistry(idx)

	goOut := string(emitConceptsGo(constructs, registry))
	tsOut := string(emitConceptsTS(constructs, registry))

	for _, want := range []string{
		`"COGNITION_PARTICIPANT": "v1:cognition:participant"`,
		`"spaceParticipants": "v1:cognition:participant"`,
		`"COGNITION_PARTICIPANT_CREATED": "graph.node.created.v1:cognition:participant"`,
		`"COGNITION_PARTICIPANT_UPDATED": "graph.node.updated.v1:cognition:participant"`,
		`"COGNITION_PARTICIPANT_DELETED": "graph.node.deleted.v1:cognition:participant"`,
	} {
		if !strings.Contains(goOut, want) {
			t.Errorf("Go concepts output missing %q\n%s", want, goOut)
		}
	}
	// CDCFilters (no graph. prefix) lives in the same Go file.
	if !strings.Contains(goOut, `"COGNITION_PARTICIPANT_CREATED": "node.created.v1:cognition:participant"`) {
		t.Errorf("Go concepts output missing the CDCFilters (graph.-less) form\n%s", goOut)
	}
	for _, want := range []string{
		`COGNITION_PARTICIPANT: "v1:cognition:participant"`,
		`spaceParticipants: "v1:cognition:participant"`,
		`COGNITION_PARTICIPANT_CREATED: "graph.node.created.v1:cognition:participant"`,
		`export const topicFor =`,
		`export const filterFor =`,
		`export type ConceptId =`,
	} {
		if !strings.Contains(tsOut, want) {
			t.Errorf("TS concepts output missing %q\n%s", want, tsOut)
		}
	}
}

// TestEmitConcepts_DeterministicEmission is contract test (c): the concept /
// topic constant emission is byte-stable across runs and independent of map
// iteration order (Go maps randomize; the emitter must sort).
func TestEmitConcepts_DeterministicEmission(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "cognition/concepts.memql", `
@version("1.0.0")
@namespace("cognition")
concept participant { spaceId string @required }

@version("1.0.0")
@namespace("cognition")
concept space { ownerUserId string @required }

@version("1.0.0")
@namespace("agents")
concept agent { ownerUserId string @required }
`)
	writeFixture(t, root, "cognition/queries.memql", `
@description("q1")
query participant qParticipants { args { spaceId string @required } filter spaceId==args.spaceId shape c }
@description("q2")
query space qSpaces { args { ownerId string @required } filter ownerUserId==args.ownerId shape c }
`)

	build := func() (string, string) {
		constructs, err := CollectAndMerge([]string{root})
		if err != nil {
			t.Fatalf("CollectAndMerge: %v", err)
		}
		idx := buildConceptIndex([]string{root})
		for i := range constructs {
			constructs[i].ConceptId = resolveConceptId(constructs[i].Concept, constructs[i].Dir, constructs[i].Imports, idx)
		}
		reg := conceptRegistry(idx)
		return string(emitConceptsGo(constructs, reg)), string(emitConceptsTS(constructs, reg))
	}

	// Ten runs must all be byte-identical (guards against map-iteration
	// nondeterminism leaking into the output).
	go1, ts1 := build()
	for i := 0; i < 10; i++ {
		go2, ts2 := build()
		if go1 != go2 {
			t.Fatalf("Go concept emission is non-deterministic across runs")
		}
		if ts1 != ts2 {
			t.Fatalf("TS concept emission is non-deterministic across runs")
		}
	}
	// The registry must be emitted in sorted order (agents before cognition).
	if strings.Index(go1, "v1:agents:agent") > strings.Index(go1, "v1:cognition:participant") {
		t.Errorf("concept registry not sorted: agents:agent should precede cognition:participant")
	}
}

// TestEmitOutput_NoCanonicalCallerInstructions is contract test (b): the
// generator's OWN scaffolding never injects a caller instruction to compose /
// send canonical ids. Emit over a clean fixture and assert the emitted strings
// carry none of the known-bad instruction phrases. (Bad phrases that originate
// in an authored @description are a DSL-source concern -- carrier A3 fixes the
// carrier ones; this guards the emitter templates so a future refactor can't
// reintroduce the coupling into generated boilerplate.)
func TestEmitOutput_NoCanonicalCallerInstructions(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "cognition/concepts.memql", `
@version("1.0.0")
@namespace("cognition")
concept participant { spaceId string @required }
`)
	writeFixture(t, root, "cognition/queries.memql", `
@description("List participants for a space.")
query participant spaceParticipants {
  args { spaceId string @required }
  filter spaceId==args.spaceId
  shape participantCard
}
`)
	constructs, err := CollectAndMerge([]string{root})
	if err != nil {
		t.Fatalf("CollectAndMerge: %v", err)
	}
	idx := buildConceptIndex([]string{root})
	for i := range constructs {
		constructs[i].ConceptId = resolveConceptId(constructs[i].Concept, constructs[i].Dir, constructs[i].Imports, idx)
	}
	reg := conceptRegistry(idx)

	emitted := strings.ToLower(strings.Join([]string{
		string(emitMethods(constructs, "Query")),
		string(emitTSMethods(constructs, "query", "")),
		string(emitConceptsGo(constructs, reg)),
		string(emitConceptsTS(constructs, reg)),
	}, "\n"))

	badPhrases := []string{
		"pass the canonical",
		"pass a canonical",
		"must pass the canonical",
		"must be a canonical",
		"must be the canonical",
		"compose the canonical",
		"compose a canonical",
		"use the full v1:",
		"use the canonical id",
		"supply the canonical",
		"provide the canonical",
	}
	for _, p := range badPhrases {
		if strings.Contains(emitted, p) {
			t.Errorf("generated output injects a canonical-id caller instruction %q -- clients pass BARE ids (#2438)", p)
		}
	}
}
