package auth

import (
	"context"
	"testing"
)

// THE ORDER IS THE DEFINITION (memql#5581). A connector and an anonymous
// caller both carry a role a later check would also match, and borrowed
// authority carries a real person's id while the cluster's own actors carry
// none -- so the narrow flags are read before the general ones.
func TestCallerKindFromContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no context at all", nil, CallerKindUnattributed},
		{"nobody stamped anything", context.Background(), CallerKindUnattributed},
		{"a signed-in person", ContextWithAccess(context.Background(),
			&AccessContext{UserId: "v1:identity:user:alice", Role: RoleWriter}), CallerKindUser},
		{"borrowed authority is still a person", ContextWithUserActor(context.Background(),
			"v1:identity:user:bob"), CallerKindUser},
		{"a maintenance sweep is the cluster", ContextWithAccess(context.Background(),
			MaintenanceActor("auditEventRetentionSweep")), CallerKindSystem},
		{"a connector", ContextWithConnectorActor(context.Background(), "shopify"), CallerKindConnector},
		{"the anonymous reader", ContextWithAccess(context.Background(), AnonymousActor()), CallerKindAnonymous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CallerKindFromContext(tc.ctx); got != tc.want {
				t.Fatalf("CallerKindFromContext = %q, want %q", got, tc.want)
			}
		})
	}
}

// It never answers "". A blank is exactly the ambiguity the value exists to
// remove -- "a person made this call and nobody wrote down which one" and "no
// person made this call at all" are different answers.
func TestCallerKindIsNeverBlank(t *testing.T) {
	for _, ctx := range []context.Context{
		nil,
		context.Background(),
		ContextWithAccess(context.Background(), &AccessContext{}),
		ContextWithAccess(context.Background(), &AccessContext{Role: RoleOwner}),
		ContextWithInternalOrigin(context.Background()),
	} {
		got := CallerKindFromContext(ctx)
		if got == "" {
			t.Fatal("CallerKindFromContext answered blank")
		}
		if !inCallerKinds(got) {
			t.Fatalf("CallerKindFromContext answered %q, which is outside the closed set %v", got, CallerKinds())
		}
	}
}

// The set is closed and every member is distinct, because the concept declares
// it as an enum: a sixth value would be refused at write time and lose the
// whole row.
func TestCallerKindsAreClosedAndDistinct(t *testing.T) {
	kinds := CallerKinds()
	if len(kinds) != 5 {
		t.Fatalf("CallerKinds() has %d entries, want the five the derivation can answer", len(kinds))
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		if k == "" || seen[k] {
			t.Fatalf("caller kind %q is empty or repeated", k)
		}
		seen[k] = true
	}
}

// IT IS NOT AN AUTHORIZATION INPUT. A synthetic actor's kind says nothing
// about what it may reach: the cluster-owner escape, the rank rules and the
// origin gate are all untouched by this value, and this pins the one property
// a future reader might be tempted to lean on -- that `system` implies power.
func TestCallerKindGrantsNothing(t *testing.T) {
	sweep := MaintenanceActor("auditEventRetentionSweep")
	ctx := ContextWithAccess(context.Background(), sweep)
	if CallerKindFromContext(ctx) != CallerKindSystem {
		t.Fatal("precondition: the sweep is a system caller")
	}
	// The origin bit is the authorization-adjacent one, and the caller kind
	// does not move it.
	if OriginFromContext(ctx) != OriginClient {
		t.Fatal("a system caller kind changed the call origin; the two are independent")
	}
}

func inCallerKinds(kind string) bool {
	for _, k := range CallerKinds() {
		if k == kind {
			return true
		}
	}
	return false
}
