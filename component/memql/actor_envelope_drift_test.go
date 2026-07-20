package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/dslspec"
)

// TestActorEnvelopeSpecDrift pins the dslspec actor property table to
// the engine's canonical envelope (auth.ActorEnvelopeFields), both
// directions (#2623) -- the editor stories consume the dslspec table,
// the four runtime resolvers consume the auth table, and this test is
// what keeps them the same table.
func TestActorEnvelopeSpecDrift(t *testing.T) {
	spec := dslspec.Build()
	var specProps []dslspec.KeywordProperty
	for _, k := range spec.Keywords {
		if k.Kind == "reserved" && k.Name == "actor" {
			specProps = k.Properties
		}
	}
	if len(specProps) == 0 {
		t.Fatal("dslspec actor keyword carries no property table")
	}

	engine := map[string]auth.ActorField{}
	for _, f := range auth.ActorEnvelopeFields {
		engine[f.Name] = f
	}
	specSeen := map[string]bool{}
	for _, p := range specProps {
		specSeen[p.Name] = true
		ef, ok := engine[p.Name]
		if !ok {
			t.Errorf("dslspec property %q has no engine counterpart", p.Name)
			continue
		}
		if ef.AliasOf != p.AliasOf {
			t.Errorf("property %q alias mismatch: engine %q vs spec %q", p.Name, ef.AliasOf, p.AliasOf)
		}
		if ef.Doc != p.Doc {
			t.Errorf("property %q doc drift:\n engine %q\n spec   %q", p.Name, ef.Doc, p.Doc)
		}
	}
	for name := range engine {
		if !specSeen[name] {
			t.Errorf("engine field %q missing from the dslspec table", name)
		}
	}
}

// TestBuildSpecCtxActorEnvelope pins the seeded spec/shape surface:
// the canonical actorEnvelope @actor shape (dsl/common/shapes.memql)
// projects actor.now, which resolved nil before #2623 unified the
// envelope -- the live bug this story fixes.
func TestBuildSpecCtxActorEnvelope(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "u1", PrimaryEmail: "u@x.io", Role: auth.RoleOwner, IdentityId: "i1",
	})
	out := buildSpecCtx(ctx, nil)
	actor, ok := out["actor"].(map[string]any)
	if !ok {
		t.Fatalf("spec ctx actor missing: %+v", out)
	}
	for _, want := range []string{"userId", "role", "identityId", "isClusterOwner", "primaryEmail", "now", "isOwner"} {
		if v, ok := actor[want]; !ok || v == "" && want != "userId" {
			if !ok {
				t.Errorf("spec-ctx actor missing %q", want)
			}
		}
	}
	if actor["now"] == nil || actor["now"] == "" {
		t.Error("actor.now must resolve in the spec/shape surface (the pre-#2623 nil bug)")
	}
}
