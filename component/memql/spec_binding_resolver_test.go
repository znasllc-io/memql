package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestSpecBindingsResolveAcrossFullTree is the binding-redesign companion
// to TestEngineInitLoadsFullDSL (epic #2281). The per-slice loader
// silently skips a spec it cannot parse, so a green smoke test alone is
// not proof the migrated tree is correct. This test builds the engine
// over the full embedded DSL tree and asserts:
//
//   - every registered spec/trait resolved (non-empty Kind);
//   - the migrated deployment/common context-specs classify as context
//     (an @actor-shape binding) with the body rewritten to actor.*;
//   - a payload trait classifies as row with the body rewritten to
//     payload.*.
func TestSpecBindingsResolveAcrossFullTree(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after LoadUnifiedConcepts")
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}

	specs := eng.Specs()
	if specs == nil {
		t.Fatal("engine has no spec registry")
	}
	all := specs.List()
	if len(all) == 0 {
		t.Fatal("no specs/traits registered after Init -- migration likely dropped the tree")
	}

	for _, s := range all {
		if s == nil {
			continue
		}
		if s.Kind != SpecKindRow && s.Kind != SpecKindContext {
			t.Errorf("spec %q (%s) has unresolved Kind %q -- binding resolution failed (likely silently skipped)", s.Name, s.Origin, s.Kind)
		}
	}

	// Context-specs bound to the @actor envelope: classify as context and
	// the body is rewritten from the bare `role` to `actor.role`.
	for _, name := range []string{"requiresOwner", "requiresOwnerOrAdmin", "requiresDeveloperOrAbove", "requiresAdmin", "forgeDeveloper", "forgeApprover"} {
		s, err := specs.Get(name)
		if err != nil || s == nil {
			t.Errorf("expected context-spec %q to be registered: %v", name, err)
			continue
		}
		if s.Kind != SpecKindContext {
			t.Errorf("spec %q: Kind = %q, want context (it binds an @actor shape)", name, s.Kind)
		}
		if s.IsTrait {
			t.Errorf("spec %q must not be a trait (it is a signature-bound caller predicate)", name)
		}
		if !strings.Contains(s.ExprSource, "actor.role") {
			t.Errorf("spec %q body not rewritten to actor.role: %q", name, s.ExprSource)
		}
	}

	// A payload trait: row-spec, IsTrait, body rewritten bare->payload.
	if s, err := specs.Get("isActiveRecord"); err != nil || s == nil {
		t.Errorf("expected trait isActiveRecord registered: %v", err)
	} else {
		if s.Kind != SpecKindRow {
			t.Errorf("trait isActiveRecord: Kind = %q, want row", s.Kind)
		}
		if !s.IsTrait {
			t.Errorf("isActiveRecord should be a trait")
		}
		if !strings.Contains(s.ExprSource, "payload.active") {
			t.Errorf("trait isActiveRecord body not rewritten to payload.active: %q", s.ExprSource)
		}
	}
}
