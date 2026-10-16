package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type actorTestValidator func(context.Context, string, string, Role) error

func (f actorTestValidator) ValidateRoleTest(ctx context.Context, session, subject string, role Role) error {
	return f(ctx, session, subject, role)
}

func TestRoleTestBorrowingAndDetachedWorkRetainAuthority(t *testing.T) {
	persona := "v1:identity:user:role-test-actor"
	revoked := false
	SetRoleTestValidator(actorTestValidator(func(_ context.Context, session, subject string, role Role) error {
		if revoked || session != "session" || subject != persona {
			return errors.New("test ended")
		}
		return nil
	}))
	t.Cleanup(func() { SetRoleTestValidator(nil) })
	for _, role := range []Role{RoleReader, RoleWriter, RoleDeveloper, RoleAdmin} {
		for _, forwarded := range []bool{false, true} {
			t.Run(string(role)+map[bool]string{false: "/direct", true: "/forwarded"}[forwarded], func(t *testing.T) {
				expires := time.Now().Add(time.Minute).Truncate(time.Second)
				claims := map[string]any{"sub": persona, "role": string(role), "class": RoleTestClass, "role_test_session": "session", "exp": expires.Unix()}
				ac := &AccessContext{UserId: persona, Role: role, RoleTestSessionID: "session"}
				ctx := ContextWithAccess(ContextWithToken(ContextWithClaims(context.Background(), claims), BuildTokenInfo(claims)), ac)
				if forwarded {
					a, _ := ForwardedAuthorityForUser(ac, RoleTestClass, "", expires, time.Now())
					ctx = BindForwardedContext(context.Background(), a.Principal().Claims, ac, a)
				}
				for _, owner := range []string{persona, strings.TrimPrefix(persona, "v1:identity:user:")} {
					borrowed := ContextWithUserActor(ctx, owner)
					if borrowed != ctx {
						t.Fatal("same-person work replaced bounded authority")
					}
					if err := ValidateRoleTestContext(borrowed); err != nil {
						t.Fatal(err)
					}
				}
				wrong := ContextWithUserActor(ctx, "v1:identity:user:operator")
				if ValidateRoleTestContext(wrong) == nil || ValidateRoleTestContext(ContextWithUserActor(wrong, persona)) == nil {
					t.Fatal("different-owner borrow shed its refusal")
				}
				request, cancelRequest := context.WithCancel(ctx)
				detached, cancel, err := DetachUserWork(request, persona)
				if err != nil {
					t.Fatal(err)
				}
				defer cancel()
				cancelRequest()
				if detached.Err() != nil {
					t.Fatal("HTTP cancellation killed accepted analysis")
				}
				if deadline, ok := detached.Deadline(); !ok || !deadline.Equal(expires) {
					t.Fatal("analysis lost session expiry")
				}
				if got, _ := AccessFromContext(detached); got != ac {
					t.Fatal("analysis changed effective role")
				}
				revoked = true
				if ValidateRoleTestContext(detached) == nil {
					t.Fatal("ended analysis retained authority")
				}
				if _, _, err := DetachUserWork(ctx, persona); err == nil {
					t.Fatal("ended test started work")
				}
				revoked = false
			})
		}
	}
	missing := ContextWithAccess(context.Background(), &AccessContext{UserId: persona, Role: RoleReader})
	if _, _, err := DetachUserWork(missing, persona); err == nil {
		t.Fatal("unbounded persona started work")
	}
	if ValidateRoleTestContext(ContextWithUserActor(missing, "ordinary-user")) == nil {
		t.Fatal("unbounded persona escaped reserved identity")
	}
}
