package roletest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// Role testing is a person's own act. The part it shares with the other acts
// that need a person's own session (auth.RequirePersonSession) is decided there;
// this test holds that every such refusal still reaches the caller as ErrDenied,
// the one answer role testing has always given. The reason each case is refused
// is held by component/auth's TestRequirePersonSessionRefusesEverythingElse.
func TestRoleTestingRefusesWhatIsNotAPersonsOwnSession(t *testing.T) {
	const person = "v1:identity:user:0d6b3c1e-9f2a-4c57-8e1d-3b4a5c6d7e8f"
	session := func(claims map[string]any) context.Context {
		base := map[string]any{"sub": person, "role": "owner"}
		for k, v := range claims {
			base[k] = v
		}
		ctx := auth.ContextWithClaims(context.Background(), base)
		return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: person, Role: auth.RoleOwner})
	}
	persona := "v1:identity:user:" + auth.RoleTestUserPrefix + "x"
	cases := map[string]context.Context{
		"no access context":     context.Background(),
		"personal access token": session(map[string]any{"pat_id": "v1:identity:identity:pat"}),
		"delegated":             auth.ContextWithDelegation(session(nil), &auth.DelegationContext{DelegationId: "d"}),
		"claims for another":    session(map[string]any{"sub": "v1:identity:user:other"}),
		"synthetic":             auth.ContextWithAccess(session(nil), &auth.AccessContext{UserId: person, Role: auth.RoleOwner, Synthetic: true}),
		"unranked":              auth.ContextWithAccess(session(nil), &auth.AccessContext{UserId: person, Role: auth.RoleOwner, Unranked: true}),
		"borrowed":              auth.ContextWithUserActor(context.Background(), person),
		"service account":       session(map[string]any{"class": "service_account"}),
		"app session":           session(map[string]any{"class": "app_session"}),
		"node":                  session(map[string]any{"class": "node"}),
		"role-test token":       session(map[string]any{"class": auth.RoleTestClass}),
		"connector":             auth.ContextWithConnectorActor(context.Background(), "shopify"),
		"anonymous":             auth.ContextWithAnonymousActor(context.Background()),
		"system actor":          auth.ContextWithSystemActor(context.Background(), "edge"),
		"inside a run":          common.ContextWithRun(session(nil), common.RunContext{RunId: "v1:work:run:r"}),
		// A persona is refused by role testing itself, after the shared gate:
		// a test session never starts another.
		"a test persona": auth.ContextWithAccess(
			auth.ContextWithClaims(context.Background(), map[string]any{"sub": persona}),
			&auth.AccessContext{UserId: persona, Role: auth.RoleDeveloper}),
	}
	s := &Service{}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			u, err := s.actor(ctx)
			require.ErrorIs(t, err, ErrDenied)
			require.Nil(t, u)
		})
	}
}
