package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// memql#2801: the context-spec surface must use the SAME actor envelope
// as everything else, including when auth is absent.
//
// buildSpecCtx used to seed `actor := map[string]any{}` and overwrite it
// only when an AccessContext was present. That introduced a THIRD
// representation of "no actor" -- absent keys -- alongside the
// envelope's two, and specEqual short-circuits on nil
// (`a == nil || b == nil` -> `a == b`). So a NEGATED actor predicate
// diverged from the envelope's deny answer:
//
//	isClusterOwner == true    absent-keys: false   envelope: false
//	isClusterOwner != false   absent-keys: TRUE    envelope: false
//
// which is the same fail-open the envelope's own default was fixed for,
// one layer up. No shipped context-spec uses `!=` on an actor field
// today -- every one is a positive `role == "..."` -- so this was
// latent, and this test is what keeps it that way.
func TestBuildSpecCtx_AbsentAuthYieldsTheDenyingEnvelope(t *testing.T) {
	specCtx := buildSpecCtx(context.Background(), nil)

	actor, ok := specCtx["actor"].(map[string]any)
	require.True(t, ok, "actor must be a map")

	// Every envelope key must be PRESENT, not absent -- that is what
	// stops the nil short-circuit from flipping a negated predicate.
	want := auth.ActorEnvelopeMap(nil)
	for key, wantVal := range want {
		if key == "now" {
			continue // timestamp, compared separately below
		}
		got, present := actor[key]
		require.True(t, present,
			"actor.%s absent from the spec envelope; an absent key makes `%s != <v>` short-circuit TRUE", key, key)
		require.Equal(t, wantVal, got, "actor.%s must match the canonical envelope", key)
	}

	require.Equal(t, false, actor["isClusterOwner"],
		"absent auth must deny the admin gate")
	require.NotNil(t, actor["now"], "actor.now must be bound")
}

// With a real context the spec surface must agree with the envelope too
// -- one canonical answer, not two.
func TestBuildSpecCtx_PresentAuthMatchesTheEnvelope(t *testing.T) {
	ac := &auth.AccessContext{UserId: "u1", Role: auth.RoleOwner, IdentityId: "i1"}
	specCtx := buildSpecCtx(auth.ContextWithAccess(context.Background(), ac), nil)

	actor, ok := specCtx["actor"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, actor["isClusterOwner"], "a real owner must pass the gate")
	require.Equal(t, "u1", actor["userId"])
}
