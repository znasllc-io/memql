package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type myAccessUserLookup func(context.Context, string) (any, error)

func (lookup myAccessUserLookup) ExecuteShaped(ctx context.Context, query string) (any, error) {
	return lookup(ctx, query)
}

// The stream authenticates claims before ensureAccess resolves the actor. Its
// original context need not contain that actor, even after the session caches
// it. Exercise the actual identity resolver and engine grant resolver across
// this seam; testing applyAccessGrant alone cannot catch a missing context.
func TestMyAccessResolvesGrantFromSessionActor(t *testing.T) {
	eng := gateTestEngine(t)
	for _, tc := range []struct {
		name         string
		storedRole   auth.Role
		claimedRole  auth.Role
		badgeCeiling auth.Role
		wantRole     auth.Role
		wantEvery    bool
	}{
		{"operator from user row", auth.RoleOwner, auth.RoleReader, "", auth.RoleOwner, true},
		{"reader overrides stale owner claim", auth.RoleReader, auth.RoleOwner, "", auth.RoleReader, false},
		{"operator constrained by badge", auth.RoleOwner, auth.RoleOwner, auth.RoleReader, auth.RoleReader, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, stream := gateTestSession(t, eng, tc.storedRole)
			// Force the real ensureAccess resolution, not the helper's cached actor.
			s.access, s.accessLoaded = nil, false
			const userID = "v1:identity:user:my-access-session"
			claims := map[string]any{"sub": userID, "role": string(tc.claimedRole), "sid": "v1:identity:session:session-my-access"}
			if tc.badgeCeiling != "" {
				claims["class"] = "badge"
				claims["role_ceiling"] = string(tc.badgeCeiling)
			}
			stream.ctx = auth.ContextWithClaims(context.Background(), claims)
			if ac, ok := auth.AccessFromContext(stream.Context()); ok || ac != nil {
				t.Fatal("fixture must start without an actor in the stream context")
			}
			lookups := 0
			s.service.identityResolver = auth.NewIdentityResolver(myAccessUserLookup(func(ctx context.Context, query string) (any, error) {
				lookups++
				if query != `query userByIdSystem(userId: "v1:identity:user:my-access-session")` {
					t.Fatalf("unexpected identity lookup: %s", query)
				}
				if auth.OriginFromContext(ctx) != auth.OriginInternal {
					t.Fatal("identity resolver did not stamp its server-only read")
				}
				return []map[string]any{{"role": string(tc.storedRole), "displayName": "Resolved caller"}}, nil
			}), s.logger)

			// The initial request resolves the actor; the next uses the cached
			// session actor. Both must pass it to the engine's context-based API.
			for _, requestID := range []string{"first-access", "cached-access"} {
				msg := &memqlv1.MyAccessMsg{RequestId: requestID}
				envelope := &memqlv1.MemqlClientMessage{MessageId: requestID, Payload: &memqlv1.MemqlClientMessage_MyAccess{MyAccess: msg}}
				if err := s.handleMyAccess(envelope, msg); err != nil {
					t.Fatalf("handleMyAccess: %v", err)
				}
				result := stream.lastSent().GetMyAccessResult()
				if result == nil {
					t.Fatalf("expected MyAccessResult, got %v", stream.lastSent())
				}
				if result.GetRequestId() != requestID || result.GetUserId() != "my-access-session" || result.GetSessionId() != "session-my-access" {
					t.Fatalf("caller or request identity lost: %+v", result)
				}
				if result.GetRole() != string(tc.wantRole) || result.GetDisplayName() != "Resolved caller" {
					t.Fatalf("reply did not use the resolved user row and badge ceiling: %+v", result)
				}
				if result.GetEveryAccount() != tc.wantEvery {
					t.Fatalf("everyAccount = %v, want %v for resolved role %q", result.GetEveryAccount(), tc.wantEvery, tc.wantRole)
				}
				if len(result.GetAccountIds()) != 0 || len(result.GetGroups()) != 0 {
					t.Fatalf("DB-less fixture invented memberships: %+v", result)
				}
			}
			if lookups != 1 {
				t.Fatalf("identity lookups = %d, want one per stream session", lookups)
			}
		})
	}
}

// The MyAccess grant mapping (epic memql#5165, section H).
//
// The interesting case is STAFF, where account_ids is EMPTY and empty means
// "all" rather than "none" -- a mapping that dropped the flag would be
// indistinguishable on the wire from a caller who belongs to nothing.

func TestApplyAccessGrantCarriesTheStaffFlag(t *testing.T) {
	result := &memqlv1.MyAccessResult{}
	applyAccessGrant(result, memqlengine.AccessGrant{EveryAccount: true})
	if !result.GetEveryAccount() {
		t.Fatal("every_account was dropped -- on the wire a staff caller would be " +
			"indistinguishable from somebody who belongs to nothing")
	}
	if len(result.GetAccountIds()) != 0 {
		t.Fatalf("account_ids = %v, want empty beside every_account", result.GetAccountIds())
	}
}

func TestApplyAccessGrantCarriesGroupsAndScope(t *testing.T) {
	result := &memqlv1.MyAccessResult{}
	applyAccessGrant(result, memqlengine.AccessGrant{
		Groups: []memqlengine.AccessGrantGroup{
			{ID: "v1:identity:group:acct-acme", Name: "Acme", Kind: "account", AccountID: "v1:accounts:account:acme", AccountName: "Acme Ltd"},
			{ID: "g-1", Name: "Reviewers", Kind: "custom"},
		},
		AccountIDs: []string{"v1:accounts:account:acme"},
	})
	if got := len(result.GetGroups()); got != 2 {
		t.Fatalf("groups = %d, want 2", got)
	}
	first := result.GetGroups()[0]
	if first.GetId() != "acct-acme" || first.GetAccountId() != "acme" || first.GetKind() != "account" || first.GetAccountName() != "Acme Ltd" {
		t.Fatalf("the first group lost fields: %+v", first)
	}
	// A custom group tied to no account carries empty strings rather than
	// being dropped: it is a real membership, and a client that renders
	// groups would otherwise show a person in fewer groups than they are in.
	second := result.GetGroups()[1]
	if second.GetId() != "g-1" || second.GetAccountId() != "" {
		t.Fatalf("the untied group was mangled: %+v", second)
	}
	if len(result.GetAccountIds()) != 1 || result.GetAccountIds()[0] != "acme" {
		t.Fatalf("account_ids = %v", result.GetAccountIds())
	}
	if result.GetEveryAccount() {
		t.Fatal("every_account is set for a non-staff caller")
	}
}

func TestApplyAccessGrantOnAnEmptyGrant(t *testing.T) {
	// The negative control: a caller in no groups must produce an EMPTY
	// list rather than a nil the mapping never touched -- otherwise the
	// tests above could pass against a function that only ever appends.
	result := &memqlv1.MyAccessResult{}
	applyAccessGrant(result, memqlengine.AccessGrant{})
	if result.GetGroups() == nil {
		t.Fatal("groups is nil rather than an empty list")
	}
	if len(result.GetGroups()) != 0 || len(result.GetAccountIds()) != 0 || result.GetEveryAccount() {
		t.Fatalf("an empty grant produced %+v", result)
	}
	// And a nil result is not a panic.
	applyAccessGrant(nil, memqlengine.AccessGrant{EveryAccount: true})
}
