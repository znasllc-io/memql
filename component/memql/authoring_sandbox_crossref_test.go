package memql_test

// authoring_sandbox_crossref_test.go -- integration coverage for Gate 1
// cross-reference resolution (issue #956, follow-up 3/3).
//
// External test package: needs a fully-initialised engine (so the live
// shape / spec / function registries are populated) plus the automations
// package linked (so the automation-compile hook is registered for the
// event-trigger concept check).

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"

	_ "github.com/znasllc-io/memql/component/automations"
)

var (
	crossRefEngineOnce sync.Once
	crossRefEngine     *memql.MemQLEngine
)

// crossRefTestEngine builds + initialises a single engine over the full
// embedded DSL tree (mirrors TestEngineInitLoadsFullDSL), shared across the
// cross-ref tests. After Init the engine's Shapes / Specs / Functions
// registries are populated, which the cross-ref pass reads.
func crossRefTestEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	crossRefEngineOnce.Do(func() {
		if err := memoryNodes.LoadConcepts(nil); err != nil {
			t.Fatalf("LoadConcepts: %v", err)
		}
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			t.Fatalf("LoadUnifiedConcepts: %v", err)
		}
		eng, err := memql.New(nil)
		if err != nil {
			t.Fatalf("construct engine: %v", err)
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
			t.Fatalf("engine.Init: %v", err)
		}
		crossRefEngine = eng
	})
	return crossRefEngine
}

// TestCrossRef_SelfConsistentBundlePasses: a bundle whose shape binds a
// bundle-defined concept and projects only its declared fields, plus a spec
// that imports that bundle shape, passes cleanly.
func TestCrossRef_SelfConsistentBundlePasses(t *testing.T) {
	eng := crossRefTestEngine(t)

	rep := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "concept",
			Name: "crossWidget",
			Source: `@version("1.0.0")
@namespace("crossns")
@description("widget")
concept crossWidget {
  label  string
  count  int
}`,
		},
		{
			Kind: "shape",
			Name: "crossWidgetCard",
			Source: `use crossns.concepts.{ crossWidget }

@row
shape crossWidget crossWidgetCard {
  row.id
  row.payload.label
  row.payload.count
}`,
		},
	}, eng)

	if !rep.OK {
		t.Fatalf("expected a self-consistent bundle to pass, got: %+v", rep.Diagnostics)
	}
}

// TestCrossRef_DanglingShapeImportFails: a spec that imports a shape neither
// the core registry nor the bundle defines fails with a precise diagnostic.
func TestCrossRef_DanglingShapeImportFails(t *testing.T) {
	eng := crossRefTestEngine(t)

	rep := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "spec",
			Name: "crossDanglingSpec",
			Source: `use crossns.shapes.{ ghostShapeThatDoesNotExist }

@description("references a missing shape")
spec crossDanglingSpec {
  actor.role == "admin"
}`,
		},
	}, eng)

	if rep.OK {
		t.Fatalf("expected bundle FAIL on dangling shape import, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || !strings.Contains(d.Error, "unresolved reference") || !strings.Contains(d.Error, "ghostShapeThatDoesNotExist") {
		t.Errorf("expected an unresolved-reference diagnostic, got %+v", d)
	}
}

// TestCrossRef_BundleSiblingImportResolves: an import that names a sibling
// construct the SAME bundle defines resolves (no live registry entry needed).
func TestCrossRef_BundleSiblingImportResolves(t *testing.T) {
	eng := crossRefTestEngine(t)

	rep := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "concept",
			Name: "crossThing",
			Source: `@version("1.0.0")
@namespace("crossns")
concept crossThing {
  name  string
}`,
		},
		{
			Kind: "shape",
			Name: "crossThingCard",
			Source: `use crossns.concepts.{ crossThing }

@row
shape crossThing crossThingCard {
  row.payload.name
}`,
		},
		{
			Kind: "query",
			Name: "queryCrossThing",
			// Imports the bundle-defined concept + the bundle-defined shape
			// above; both must resolve as bundle siblings.
			Source: `use crossns.concepts.{ crossThing }
use crossns.shapes.{ crossThingCard }

@description("query over the bundle concept via the bundle shape")
query crossThing queryCrossThing {
  filter  payload.name == "x"
  shape   crossThingCard
}`,
		},
	}, eng)

	if !rep.OK {
		t.Fatalf("expected sibling import to resolve, got: %+v", rep.Diagnostics)
	}
}

// TestCrossRef_FieldExistenceFails: a @row shape projecting a payload field
// the bound concept does not declare fails with a precise diagnostic.
func TestCrossRef_FieldExistenceFails(t *testing.T) {
	eng := crossRefTestEngine(t)

	rep := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "concept",
			Name: "crossFieldThing",
			Source: `@version("1.0.0")
@namespace("crossns")
concept crossFieldThing {
  label  string
}`,
		},
		{
			Kind: "shape",
			Name: "crossFieldCard",
			Source: `use crossns.concepts.{ crossFieldThing }

@row
shape crossFieldThing crossFieldCard {
  row.payload.label
  row.payload.doesNotExist
}`,
		},
	}, eng)

	if rep.OK {
		t.Fatalf("expected bundle FAIL on non-existent field, got OK: %+v", rep.Diagnostics)
	}
	var shapeDiag *memql.SandboxDiagnostic
	for i := range rep.Diagnostics {
		if rep.Diagnostics[i].Name == "crossFieldCard" {
			shapeDiag = &rep.Diagnostics[i]
		}
	}
	if shapeDiag == nil || shapeDiag.OK || !strings.Contains(shapeDiag.Error, "doesNotExist") {
		t.Errorf("expected a field-existence diagnostic naming doesNotExist, got %+v", shapeDiag)
	}
}

// TestCrossRef_FieldExistenceAgainstCoreConceptPasses: a @row shape over a
// REAL core concept that projects a declared field passes; projecting a
// bogus field fails.
func TestCrossRef_FieldExistenceAgainstCoreConceptPasses(t *testing.T) {
	eng := crossRefTestEngine(t)

	good := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "shape",
			Name: "crossUserCard",
			Source: `use identity.concepts.{ user }

@row
shape user crossUserCard {
  row.id
  row.createdAt
}`,
		},
	}, eng)
	if !good.OK {
		t.Fatalf("expected a row-intrinsic-only shape over a core concept to pass, got: %+v", good.Diagnostics)
	}

	bad := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "shape",
			Name: "crossUserBogus",
			Source: `use identity.concepts.{ user }

@row
shape user crossUserBogus {
  row.payload.thisFieldIsNotOnUser
}`,
		},
	}, eng)
	if bad.OK {
		t.Fatalf("expected FAIL projecting a bogus field on a core concept, got OK: %+v", bad.Diagnostics)
	}
}

// TestCrossRef_AutomationTriggerConceptExists: an automation triggering on a
// real concept passes; one triggering on a non-existent concept fails.
func TestCrossRef_AutomationTriggerConceptExists(t *testing.T) {
	eng := crossRefTestEngine(t)

	good := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "crossOnUser",
			Source: `@trigger(event="node.created", concept="v1:identity:user")
automation crossOnUser {
  step run {
    logic crossNoop { event: event }
  }
}`,
		},
	}, eng)
	if !good.OK {
		t.Fatalf("expected automation on a real concept to pass, got: %+v", good.Diagnostics)
	}

	bad := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "crossOnGhost",
			Source: `@trigger(event="node.created", concept="v1:ghostns:ghostConcept")
automation crossOnGhost {
  step run {
    logic crossNoop { event: event }
  }
}`,
		},
	}, eng)
	if bad.OK {
		t.Fatalf("expected FAIL on automation triggering on a non-existent concept, got OK: %+v", bad.Diagnostics)
	}
	d := bad.Diagnostics[0]
	if d.OK || !strings.Contains(d.Error, "ghostConcept") {
		t.Errorf("expected a trigger-concept diagnostic naming the ghost concept, got %+v", d)
	}
}

// TestCrossRef_AutomationTriggerOnBundleConcept: an automation may trigger on
// a concept the SAME bundle defines (overlaid onto the clone).
func TestCrossRef_AutomationTriggerOnBundleConcept(t *testing.T) {
	eng := crossRefTestEngine(t)

	rep := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "concept",
			Name: "crossEvented",
			Source: `@version("1.0.0")
@namespace("crossns")
concept crossEvented {
  name  string
}`,
		},
		{
			Kind: "automation",
			Name: "crossOnEvented",
			Source: `@trigger(event="node.created", concept="v1:crossns:crossEvented")
automation crossOnEvented {
  step run {
    logic crossNoop { event: event }
  }
}`,
		},
	}, eng)
	if !rep.OK {
		t.Fatalf("expected automation triggering on a bundle-defined concept to pass, got: %+v", rep.Diagnostics)
	}
}

// TestCrossRef_NoMutationOfConceptRegistry: the cross-ref pass must not
// mutate the live concept registry.
func TestCrossRef_NoMutationOfConceptRegistry(t *testing.T) {
	eng := crossRefTestEngine(t)

	before := len(memoryNodes.List())
	_ = memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{
		{
			Kind: "concept",
			Name: "crossEphemeral",
			Source: `@version("1.0.0")
@namespace("crossns")
concept crossEphemeral {
  name  string
}`,
		},
		{
			Kind: "automation",
			Name: "crossOnEphemeral",
			Source: `@trigger(event="node.created", concept="v1:crossns:crossEphemeral")
automation crossOnEphemeral {
  step run {
    logic crossNoop { event: event }
  }
}`,
		},
	}, eng)
	if after := len(memoryNodes.List()); after != before {
		t.Fatalf("cross-ref pass mutated the live concept registry: before=%d after=%d", before, after)
	}
}
