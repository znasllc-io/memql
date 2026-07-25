package auth

import "testing"

// memql#2801: the actor envelope must not resolve `isClusterOwner` to
// TRUE when there is no AccessContext.
//
// `actor.isClusterOwner==true` is the ENTIRE access gate for the
// admin-tier constructs -- it compiles to a bound-parameter `? = ?`
// fragment against the resolved actor value, and nothing else is
// appended to the WHERE clause. So on any path where AccessContext is
// absent, a permissive default makes those queries return EVERYTHING
// rather than nothing: full call-detail-record and DID inventory dumps
// from the telephony admin reads, and the authoring admin read.
//
// The old default justified itself as "no auth context (dev mode)".
// That is not what dev mode does. With MEMQL_IDENTITY_ENABLED=false the
// local-dev interceptor injects a SYNTHETIC AccessContext (subject
// "local-dev", role "owner") onto every stream, so `ac` is non-nil and
// resolves to owner through the normal path. Nil does not mean dev mode;
// it means the context genuinely never arrived -- and "no auth context"
// and "caller is the cluster owner" are very different facts.
//
// The codebase's own canonical helper already agrees: AccessContext.
// IsClusterOwner() is `ac != nil && ac.Role == RoleOwner`, i.e. nil is
// NOT owner. The envelope was the one place contradicting it.

// The path resolver -- what `actor.isClusterOwner` reads in a filter.
func TestActorEnvelopeValue_NilContextIsNotClusterOwner(t *testing.T) {
	got, ok := ActorEnvelopeValue(nil, "isClusterOwner")
	if !ok {
		t.Fatal("isClusterOwner must resolve (present in the closed envelope set)")
	}
	if got != false {
		t.Errorf("nil AccessContext resolved isClusterOwner=%v; the admin-tier gate is `actor.isClusterOwner==true` "+
			"and nothing else bounds those queries, so a permissive default returns every row (memql#2801)", got)
	}
}

// The map surface -- the same envelope for logic bodies and
// context-specs. Both must agree, or the gate depends on which surface
// evaluated it.
func TestActorEnvelopeMap_NilContextIsNotClusterOwner(t *testing.T) {
	env := ActorEnvelopeMap(nil)
	for _, key := range []string{"isClusterOwner", "isOwner"} {
		if env[key] != false {
			t.Errorf("nil AccessContext yielded %s=%v; must be false (memql#2801)", key, env[key])
		}
	}
}

// The two surfaces must not drift: whatever the envelope says about a
// nil context, it must say identically in both.
func TestActorEnvelope_NilDefaultsAgreeAcrossSurfaces(t *testing.T) {
	env := ActorEnvelopeMap(nil)
	for _, key := range []string{"userId", "role", "identityId", "primaryEmail", "isClusterOwner"} {
		pathValue, ok := ActorEnvelopeValue(nil, key)
		if !ok {
			t.Errorf("%s: not resolvable via the path surface", key)
			continue
		}
		if pathValue != env[key] {
			t.Errorf("%s: path surface = %v, map surface = %v -- the two envelope surfaces disagree "+
				"about a nil context, so the gate depends on which one evaluated it", key, pathValue, env[key])
		}
	}
}

// Nil must be the DENY default, not a special case: a real non-owner
// context and an absent one should both fail an owner gate, and only a
// genuine owner should pass it.
func TestActorEnvelope_OnlyARealOwnerPassesTheGate(t *testing.T) {
	cases := []struct {
		name string
		ac   *AccessContext
		want bool
	}{
		{"nil context", nil, false},
		{"reader", &AccessContext{Role: RoleReader}, false},
		{"writer", &AccessContext{Role: RoleWriter}, false},
		{"admin is not cluster owner", &AccessContext{Role: RoleAdmin}, false},
		{"developer is not cluster owner", &AccessContext{Role: RoleDeveloper}, false},
		{"owner", &AccessContext{Role: RoleOwner}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ActorEnvelopeValue(tc.ac, "isClusterOwner")
			if !ok {
				t.Fatal("isClusterOwner must resolve")
			}
			if got != tc.want {
				t.Errorf("isClusterOwner = %v, want %v", got, tc.want)
			}
			// The envelope must agree with the canonical helper.
			if got != tc.ac.IsClusterOwner() {
				t.Errorf("envelope (%v) disagrees with AccessContext.IsClusterOwner() (%v)",
					got, tc.ac.IsClusterOwner())
			}
		})
	}
}
