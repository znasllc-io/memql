package reviewstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type reviewEngine struct {
	memql.IntegrationEngineAccess
	execute func(context.Context, string) (*memql.ExecuteResult, error)
}

func (e reviewEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	return e.execute(ctx, query)
}

type reviewValidator func(context.Context, string, string, auth.Role) error

func (v reviewValidator) ValidateRoleTest(ctx context.Context, session, subject string, role auth.Role) error {
	return v(ctx, session, subject, role)
}

func TestApprovedRevisionsConfinesServiceReadAndRechecksSession(t *testing.T) {
	revoked := false
	auth.SetRoleTestValidator(reviewValidator(func(context.Context, string, string, auth.Role) error {
		if revoked {
			return errors.New("test ended")
		}
		return nil
	}))
	t.Cleanup(func() { auth.SetRoleTestValidator(nil) })
	persona := "v1:identity:user:role-test-review"
	a := auth.ForwardedAuthority{CredentialClass: auth.RoleTestClass, RoleTestSessionID: "review-session", Subject: persona, Role: auth.RoleReader}
	ctx := auth.BindForwardedContext(context.Background(), a.Principal().Claims, &auth.AccessContext{UserId: persona, Role: auth.RoleReader}, a)
	calls := 0
	revokeDuringRead := false
	engine := reviewEngine{execute: func(read context.Context, query string) (*memql.ExecuteResult, error) {
		calls++
		if !strings.HasPrefix(query, "query workDocumentApprovedRevisions(") || !strings.Contains(query, `artifactId: "artifact"`) {
			t.Fatalf("unconfined service query: %s", query)
		}
		ac, _ := auth.AccessFromContext(read)
		if !auth.IsSystemActorId(ac.UserId) || auth.OriginFromContext(read) != auth.OriginInternal {
			t.Fatal("missing fixed service identity")
		}
		if err := auth.ValidateRoleTestContext(read); err != nil {
			t.Fatalf("service read impersonated persona: %v", err)
		}
		if revokeDuringRead {
			revoked = true
		}
		return &memql.ExecuteResult{}, nil
	}}
	if _, err := ApprovedRevisions(ctx, engine, "artifact", "", []int{1}); err != nil {
		t.Fatal(err)
	}
	revokeDuringRead = true
	if result, err := ApprovedRevisions(ctx, engine, "artifact", "", []int{1}); err == nil || result != nil {
		t.Fatal("in-flight revocation exposed receipts")
	}
	if _, err := ApprovedRevisions(ctx, engine, "artifact", "", []int{1}); err == nil {
		t.Fatal("ended test reached service read")
	}
	if calls != 2 {
		t.Fatalf("ended test performed %d service reads", calls)
	}
}
