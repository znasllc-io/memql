package memql

import (
	"strings"
	"testing"
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
	eng := sharedDblessEngine(t)

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
	// THREE NAMES LEFT THIS LIST IN EPIC memql#5166 -- `requiresOwnerOrAdmin`,
	// `requiresDeveloperOrAbove` and `requiresAdmin`. They compared the actor's
	// role STRING against literals, which cannot see a custom role at all, and
	// dsl/common/specs.memql no longer declares them: a role question is
	// `@requiresRank` (a floor) or `@requiresCapability` (a grant), neither of
	// which is a spec. What is left here is every context-spec the tree still
	// declares.
	for _, name := range []string{"requiresOwner", "forgeDeveloper", "forgeApprover"} {
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
