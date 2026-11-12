package auth

import (
	"context"
	"strings"
	"testing"
	"time"

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

// forwardedSession is a person's request as a mesh receiver binds it: the
// claims rebuilt from the assertion carry no class, no pat_id and no
// delegation, so only the assertion itself says what credential it came from.
func forwardedSession(t *testing.T, class string, subject string, ceiling Role, expires time.Time) context.Context {
	t.Helper()
	ac := &AccessContext{UserId: subject, Role: RoleDeveloper}
	if class == RoleTestClass {
		ac.RoleTestSessionID = "session"
	}
	if ceiling != "" {
		ac.Role = ceiling
	}
	a, err := ForwardedAuthorityForUser(ac, class, ceiling, expires, time.Now())
	if err != nil {
		t.Fatalf("class %q: %v", class, err)
	}
	verified, err := VerifyForwardedAuthority(a, time.Now())
	if err != nil {
		t.Fatalf("class %q: %v", class, err)
	}
	return BindForwardedContext(context.Background(), a.Principal().Claims, verified, a)
}

// A forwarded hop is admitted only when the assertion says a user's session:
// the credential class travels on the assertion and nowhere else.
func TestRequirePersonSessionReadsTheForwardedAssertion(t *testing.T) {
	soon := time.Now().Add(time.Minute)
	id, err := RequirePersonSession(forwardedSession(t, ForwardedClassUser, sessionPerson, "", time.Time{}))
	if err != nil || id != sessionPerson {
		t.Fatalf("a person's forwarded session: id=%q err=%v", id, err)
	}
	persona := "v1:identity:user:" + RoleTestUserPrefix + "p"
	refused := map[string]context.Context{
		"pat":         forwardedSession(t, ForwardedClassPat, sessionPerson, "", time.Time{}),
		"app session": forwardedSession(t, ForwardedClassAppSession, sessionPerson, "", soon),
		"badge":       forwardedSession(t, ForwardedClassBadge, sessionPerson, RoleReader, soon),
		"role test":   forwardedSession(t, RoleTestClass, persona, "", soon),
		// What a refused binding leaves on the context (component/grpc
		// work_authority.go): an assertion present but zero.
		"zero assertion": ContextWithForwardedAuthority(session("", nil), ForwardedAuthority{}),
	}
	resolver := NewIdentityResolver(QueryRunnerFunc(func(context.Context, string) (any, error) {
		return map[string]any{"role": string(RoleOwner)}, nil
	}), nil)
	persisted, err := ContextWithPersistedOwner(context.Background(), sessionPerson,
		map[string]any{"roleCeiling": string(RoleOwner), "credentialClass": ForwardedClassPat}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	refused["persisted pat"] = persisted
	ungranted, err := ContextWithPersistedOwner(context.Background(), sessionPerson, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	refused["persisted with no grant"] = ungranted
	for name, ctx := range refused {
		t.Run(name, func(t *testing.T) {
			id, err := RequirePersonSession(ctx)
			if err == nil || id != "" || !IsPersonSessionRequired(err) {
				t.Fatalf("id=%q err=%v", id, err)
			}
		})
	}
}

// A badge is a person signed in under a ceiling, not a machine, and the
// refusal says so.
func TestRequirePersonSessionNamesABadgeAsABadge(t *testing.T) {
	_, err := RequirePersonSession(session(ForwardedClassBadge, nil))
	if err == nil || strings.Contains(err.Error(), "machine") || !strings.Contains(err.Error(), "badge") {
		t.Fatalf("badge refusal = %v", err)
	}
	_, err = RequirePersonSession(forwardedSession(t, ForwardedClassBadge, sessionPerson, RoleReader, time.Now().Add(time.Minute)))
	if err == nil || !strings.Contains(err.Error(), "badge") {
		t.Fatalf("forwarded badge refusal = %v", err)
	}
}

// The ids are compared as they are: a subject or a user id with stray
// whitespace is not a person's own session trimmed into one.
func TestRequirePersonSessionComparesIdsExactly(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"padded subject": ContextWithAccess(ContextWithClaims(context.Background(), map[string]any{"sub": " " + sessionPerson}),
			&AccessContext{UserId: sessionPerson, Role: RoleOwner}),
		"padded user id": ContextWithAccess(ContextWithClaims(context.Background(), map[string]any{"sub": sessionPerson + " "}),
			&AccessContext{UserId: sessionPerson + " ", Role: RoleOwner}),
	} {
		if id, err := RequirePersonSession(ctx); err == nil {
			t.Errorf("%s: admitted as %q", name, id)
		}
	}
}
