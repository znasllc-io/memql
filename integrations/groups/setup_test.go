package groups

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// Each Integration is a separate replica. Only their graph is shared; no
// in-memory session or ensure cache may be required to retry completion.
type setupGraph struct {
	mu sync.Mutex
	*stubEngine
	failMembership bool
}

func (g *setupGraph) Execute(ctx context.Context, q string) (any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.HasPrefix(q, "mutation writeGroupMembership(") && g.failMembership {
		return nil, errors.New("membership temporarily unavailable")
	}
	res, err := g.stubEngine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.HasPrefix(q, "mutation createClientAccount("):
		g.accounts["self"] = map[string]any{"id": "self", "name": argOf(q, "name"), "status": StatusActive}
	case strings.HasPrefix(q, "mutation configureClusterAccount("):
		g.accounts["self"]["name"] = argOf(q, "name")
		g.accounts["self"]["ownerUserId"] = argOf(q, "ownerUserId")
		g.accounts["self"]["configuredAt"] = "2026-09-22T00:00:00Z"
	case strings.HasPrefix(q, "mutation writeGroup("):
		g.groups[argOf(q, "groupId")] = map[string]any{"id": argOf(q, "groupId"), "name": argOf(q, "name"), "accountId": argOf(q, "accountId"), "kind": argOf(q, "kind"), "status": argOf(q, "status")}
	case strings.HasPrefix(q, "mutation writeGroupMembership("):
		g.members[argOf(q, "groupId")] = []any{map[string]any{"id": argOf(q, "membershipId"), "groupId": argOf(q, "groupId"), "userId": argOf(q, "userId"), "status": argOf(q, "status")}}
	}
	return res, nil
}

func TestOrganizationSetupRetriesAcrossReplicasPreservingSelf(t *testing.T) {
	g := &setupGraph{stubEngine: newStub(), failMembership: true}
	g.users["owner"] = map[string]any{"id": "owner", "role": "owner"}
	g.accounts["self"] = map[string]any{"id": "self", "name": "Your company", "status": StatusActive, "domain": "example.test", "notes": "Keep this"}
	g.groups["acct-self"] = map[string]any{"id": "acct-self", "name": "Your company", "accountId": "self", "kind": KindAccount, "status": StatusActive}
	first := New(g, nil)
	ctx := SystemActorContext(context.Background())
	if err := first.ConfigureSelfAccount(ctx, "Example", "owner"); err == nil {
		t.Fatal("failed membership must prevent completion")
	}
	g.failMembership = false
	second := New(g, nil)
	if err := second.ConfigureSelfAccount(ctx, "Example", "owner"); err != nil {
		t.Fatal(err)
	}
	writes := len(g.writes)
	if err := first.ConfigureSelfAccount(ctx, "Example", "owner"); err != nil {
		t.Fatal(err)
	}
	if len(g.writes) != writes {
		t.Fatal("completed retry wrote duplicate account/group/membership versions")
	}
	if g.accounts["self"]["notes"] != "Keep this" || g.accounts["self"]["domain"] != "example.test" {
		t.Fatal("setup discarded existing company details")
	}
	if g.groups["acct-self"]["name"] != "Example" {
		t.Fatal("group kept boot placeholder")
	}
	if g.accounts["self"]["ownerUserId"] != "owner" {
		t.Fatal("claiming owner was not persisted on self")
	}
	if len(g.groups) != 1 || len(g.members["acct-self"]) != 1 {
		t.Fatal("duplicate organization membership")
	}
	for _, q := range g.writes {
		if strings.Contains(q, "role:") {
			t.Fatalf("organization setup granted a role: %s", q)
		}
	}
}

func TestOrganizationSetupRefusesUntrustedOrNonOwnerBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		internal bool
		role     string
		org      string
	}{
		{"client owner", false, "owner", "Example"}, {"missing owner", true, "", "Example"},
		{"member", true, "reader", "Example"}, {"missing name", true, "owner", " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &setupGraph{stubEngine: newStub()}
			g.users["person"] = map[string]any{"id": "person", "role": tc.role}
			ctx := context.Background()
			if tc.internal {
				ctx = SystemActorContext(ctx)
			}
			if err := New(g, nil).ConfigureSelfAccount(ctx, tc.org, "person"); err == nil {
				t.Fatal("unsafe setup accepted")
			}
			if len(g.writes) != 0 {
				t.Fatal("refused setup wrote data")
			}
		})
	}
}

func TestOperatorMembershipUsesConfiguredSelfWithoutRoleGrants(t *testing.T) {
	for _, role := range []string{"owner", "admin", "developer", "writer", "reader", "unknown"} {
		t.Run(role, func(t *testing.T) {
			g := &setupGraph{stubEngine: newStub()}
			g.users["person"] = map[string]any{"id": "person", "role": role}
			g.accounts["self"] = map[string]any{"id": "self", "name": "Example", "status": StatusActive, "configuredAt": "2026-09-22"}
			i := New(g, nil)
			ctx := SystemActorContext(context.Background())
			if _, err := i.handleGroupEnsureOperatorMembership(ctx, map[string]any{"userId": "person"}, 0); err != nil {
				t.Fatal(err)
			}
			want := auth.IsClusterOperator(auth.Role(role))
			if got := len(g.members["acct-self"]) == 1; got != want {
				t.Fatalf("membership %v want %v", got, want)
			}
			writes := len(g.writes)
			if _, err := New(g, nil).handleGroupEnsureOperatorMembership(ctx, map[string]any{"userId": "person"}, 0); err != nil {
				t.Fatal(err)
			}
			if len(g.writes) != writes {
				t.Fatal("another replica duplicated membership")
			}
		})
	}
}

func TestOrganizationSetupNeverTakesAnotherOwnersSelf(t *testing.T) {
	g := &setupGraph{stubEngine: newStub()}
	g.users["owner"] = map[string]any{"id": "owner", "role": "owner"}
	g.accounts["self"] = map[string]any{"id": "self", "name": "Existing", "status": StatusActive, "ownerUserId": "different-owner"}
	if err := New(g, nil).ConfigureSelfAccount(SystemActorContext(context.Background()), "Replacement", "owner"); err == nil {
		t.Fatal("reassigned organization owner")
	}
	if len(g.writes) != 0 {
		t.Fatal("competing setup changed the existing organization")
	}
}
