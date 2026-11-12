package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

const sessionPerson = "v1:identity:user:6b0f4c3e-2a51-4a8e-9b0c-8f2d0e7a1c55"

// session is a person's own signed-in browser session as the stream resolves it:
// the verified claims and the access context, naming the same person.
func session(class string, extra map[string]any) context.Context {
	claims := map[string]any{"sub": sessionPerson, "role": "owner"}
	if class != "" {
		claims["class"] = class
	}
	for k, v := range extra {
		claims[k] = v
	}
	ctx := ContextWithClaims(context.Background(), claims)
	return ContextWithAccess(ctx, &AccessContext{UserId: sessionPerson, Role: RoleOwner})
}

// A person's own session passes, and the gate answers who they are. A user token
// the identity service issues carries no class claim at all, which the verifier
// reads as a user's, so both spellings pass.
func TestRequirePersonSessionAdmitsAPersonsOwnSession(t *testing.T) {
	for _, class := range []string{"", "user"} {
		id, err := RequirePersonSession(session(class, nil))
		if err != nil {
			t.Fatalf("class %q: a person's own session was refused: %v", class, err)
		}
		if id != sessionPerson {
			t.Fatalf("class %q: answered %q, want %q", class, id, sessionPerson)
		}
	}
}

// Everything that is not a person's own session is refused with the one code,
// before any handler can read or write a row for it.
func TestRequirePersonSessionRefusesEverythingElse(t *testing.T) {
	withAccess := func(ac *AccessContext, claims map[string]any) context.Context {
		ctx := context.Background()
		if claims != nil {
			ctx = ContextWithClaims(ctx, claims)
		}
		return ContextWithAccess(ctx, ac)
	}
	cases := map[string]context.Context{
		"no access context":    context.Background(),
		"nil context":          nil,
		"claims but no access": ContextWithClaims(context.Background(), map[string]any{"sub": sessionPerson}),
		"access but no claims": withAccess(&AccessContext{UserId: sessionPerson, Role: RoleOwner}, nil),
		"claims for somebody else": withAccess(&AccessContext{UserId: sessionPerson, Role: RoleOwner},
			map[string]any{"sub": "v1:identity:user:someone-else"}),
		"anonymous":        ContextWithAnonymousActor(context.Background()),
		"system actor":     ContextWithSystemActor(context.Background(), "edge"),
		"maintenance":      withAccess(MaintenanceActor("auditEventRetentionSweep"), map[string]any{"sub": MaintenanceActor("auditEventRetentionSweep").UserId}),
		"borrowed":         ContextWithUserActor(context.Background(), sessionPerson),
		"connector":        ContextWithConnectorActor(context.Background(), "shopify"),
		"synthetic person": withAccess(&AccessContext{UserId: sessionPerson, Role: RoleOwner, Synthetic: true}, map[string]any{"sub": sessionPerson}),
		"unranked person":  withAccess(&AccessContext{UserId: sessionPerson, Role: RoleOwner, Unranked: true}, map[string]any{"sub": sessionPerson}),
		// The operator credential resolves to an owner with no Synthetic flag, so
		// only its subject says it is nobody.
		"operator credential": withAccess(&AccessContext{UserId: "cluster:operator", Role: RoleOwner},
			map[string]any{"sub": "cluster:operator", "role": "owner"}),
		// The no-auth troubleshooting shim admits every stream as this owner.
		"local-dev owner": withAccess(&AccessContext{UserId: LocalDevSubject, Role: RoleOwner},
			map[string]any{"sub": LocalDevSubject, "role": "owner"}),
		"forwarded local-dev":   session("", map[string]any{forwardedClaimLocalDev: "1"}),
		"service account":       session(ForwardedClassServiceAccount, nil),
		"app session":           session(ForwardedClassAppSession, nil),
		"worker token":          session(ForwardedClassWorkerToken, nil),
		"node":                  session(ForwardedClassNode, nil),
		"role test":             session(RoleTestClass, nil),
		"malformed class":       session("", map[string]any{"class": 7}),
		"unknown class":         session("tomorrow", nil),
		"personal access token": session("", map[string]any{"pat_id": "v1:identity:identity:pat"}),
		"delegated":             ContextWithDelegation(session("", nil), &DelegationContext{DelegationId: "v1:identity:delegation:d"}),
		"inside a run":          common.ContextWithRun(session("", nil), common.RunContext{RunId: "v1:work:run:r"}),
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			id, err := RequirePersonSession(ctx)
			if err == nil {
				t.Fatalf("admitted as %q", id)
			}
			if id != "" {
				t.Fatalf("a refusal answered an id: %q", id)
			}
			if !strings.HasPrefix(err.Error(), PersonSessionRequired+":") {
				t.Fatalf("refusal does not begin with the code: %v", err)
			}
			if !IsPersonSessionRequired(err) {
				t.Fatalf("IsPersonSessionRequired did not recognise its own refusal: %v", err)
			}
		})
	}
}
