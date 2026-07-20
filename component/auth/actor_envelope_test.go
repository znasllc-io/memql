package auth

import (
	"strings"
	"testing"
	"time"
)

// #2623: the canonical envelope, uniform across every surface.
func TestActorEnvelope(t *testing.T) {
	ac := &AccessContext{UserId: "u1", PrimaryEmail: "u@x.io", Role: RoleOwner, IdentityId: "i1"}

	m := ActorEnvelopeMap(ac)
	for _, want := range []string{"userId", "role", "identityId", "isClusterOwner", "primaryEmail", "now", "isOwner"} {
		if _, ok := m[want]; !ok {
			t.Errorf("seeded map missing %q", want)
		}
	}
	if m["isOwner"] != m["isClusterOwner"] {
		t.Error("alias key must mirror the canonical owner bit")
	}
	if _, err := time.Parse(time.RFC3339Nano, m["now"].(string)); err != nil {
		t.Errorf("now must be RFC3339Nano: %v", err)
	}

	for path, want := range map[string]any{
		"userId":         "u1",
		"role":           "owner",
		"identityId":     "i1",
		"primaryEmail":   "u@x.io",
		"isClusterOwner": true,
		"isOwner":        true,
	} {
		got, ok := ActorEnvelopeValue(ac, path)
		if !ok || got != want {
			t.Errorf("ActorEnvelopeValue(%q) = (%v, %v), want (%v, true)", path, got, ok, want)
		}
	}
	if v, ok := ActorEnvelopeValue(ac, "now"); !ok || v == "" {
		t.Error("actor.now must resolve on the path surface")
	}

	// Dropped-by-decision paths stay outside the envelope.
	for _, bad := range []string{"config", "partitions", "rank", "userid"} {
		if _, ok := ActorEnvelopeValue(ac, bad); ok {
			t.Errorf("path %q must be outside the envelope", bad)
		}
	}

	// Nil context: dev-mode owner semantics, matching the historical
	// path-resolver behavior.
	if v, ok := ActorEnvelopeValue(nil, "isClusterOwner"); !ok || v != true {
		t.Errorf("nil context owner bit = (%v, %v), want (true, true)", v, ok)
	}
	nm := ActorEnvelopeMap(nil)
	if nm["isOwner"] != true || nm["userId"] != "" {
		t.Errorf("nil-context map wrong: %+v", nm)
	}

	valid := ActorEnvelopeValidNames()
	if !strings.Contains(valid, "isOwner (alias of isClusterOwner)") || !strings.Contains(valid, "now") {
		t.Errorf("valid-names rendering: %q", valid)
	}
}
