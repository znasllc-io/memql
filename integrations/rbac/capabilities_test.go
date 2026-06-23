package rbac

import (
	"context"
	"encoding/json"
	"testing"
)

func allowed(t *testing.T, payload []byte) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal decision payload: %v", err)
	}
	b, ok := m["allowed"].(bool)
	if !ok {
		t.Fatalf("decision payload missing bool `allowed`: %v", m)
	}
	return b
}

// TestGovernPrincipalCapability exercises the DSL-facing governPrincipal
// handler end to end: arg coercion (DSL int64 ranks) -> auth core -> decision
// node. Pins the relational rule + the owner carve-out at the integration
// boundary.
func TestGovernPrincipalCapability(t *testing.T) {
	i := New()
	ctx := context.Background()

	// admin (200) updates member (100): allowed.
	nodes, err := i.handleGovernPrincipal(ctx, map[string]any{
		"actorUserId": "a", "actorRank": int64(200), "actorIsOwner": false,
		"targetUserId": "m", "targetRank": int64(100), "targetRoleSlug": "user", "verb": "update",
	}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("handleGovernPrincipal: err=%v nodes=%d", err, len(nodes))
	}
	if !allowed(t, nodes[0].Payload) {
		t.Error("admin should be able to update a member")
	}

	// admin (200) updates developer (300): refused (admin is lower-ranked).
	nodes, _ = i.handleGovernPrincipal(ctx, map[string]any{
		"actorUserId": "a", "actorRank": int64(200), "actorIsOwner": false,
		"targetUserId": "d", "targetRank": int64(300), "targetRoleSlug": "developer", "verb": "update",
	}, 0)
	if allowed(t, nodes[0].Payload) {
		t.Error("admin should NOT be able to update a higher-ranked developer")
	}

	// developer (300) updates an owner: refused (owner-only-by-owner).
	nodes, _ = i.handleGovernPrincipal(ctx, map[string]any{
		"actorUserId": "d", "actorRank": int64(300), "actorIsOwner": false,
		"targetUserId": "o", "targetRank": int64(400), "targetRoleSlug": "owner", "verb": "delete",
	}, 0)
	if allowed(t, nodes[0].Payload) {
		t.Error("a non-owner must NOT be able to delete an owner")
	}

	// self-edit: a member updates themselves -> allowed.
	nodes, _ = i.handleGovernPrincipal(ctx, map[string]any{
		"actorUserId": "m", "actorRank": int64(100), "actorIsOwner": false,
		"targetUserId": "m", "targetRank": int64(100), "targetRoleSlug": "user", "verb": "update",
	}, 0)
	if !allowed(t, nodes[0].Payload) {
		t.Error("a member must be able to update themselves")
	}
}

// TestCanCreatePrincipalCapability pins the create != edit split at the
// integration boundary.
func TestCanCreatePrincipalCapability(t *testing.T) {
	i := New()
	ctx := context.Background()

	// admin (200) creates a member (100): allowed (strictly below).
	nodes, err := i.handleCanCreatePrincipal(ctx, map[string]any{
		"actorRank": int64(200), "actorIsOwner": false, "newRank": int64(100),
	}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("handleCanCreatePrincipal: err=%v nodes=%d", err, len(nodes))
	}
	if !allowed(t, nodes[0].Payload) {
		t.Error("admin should be able to create a member")
	}

	// admin (200) creates a developer (300): refused (at/above own rank).
	nodes, _ = i.handleCanCreatePrincipal(ctx, map[string]any{
		"actorRank": int64(200), "actorIsOwner": false, "newRank": int64(300),
	}, 0)
	if allowed(t, nodes[0].Payload) {
		t.Error("admin must NOT be able to create a developer (above own rank)")
	}
}
