package automations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type roleJournalExecutor func(context.Context, string) (*memql.ExecuteResult, error)

func (f roleJournalExecutor) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	return f(ctx, q)
}

type journalRoleValidator func(context.Context, string, string, auth.Role) error

func (f journalRoleValidator) ValidateRoleTest(ctx context.Context, s, u string, r auth.Role) error {
	return f(ctx, s, u, r)
}

func TestRoleTestJournalReadChecksOwnershipAndRevocation(t *testing.T) {
	revoked := false
	auth.SetRoleTestValidator(journalRoleValidator(func(context.Context, string, string, auth.Role) error {
		if revoked {
			return errors.New("test ended")
		}
		return nil
	}))
	t.Cleanup(func() { auth.SetRoleTestValidator(nil) })
	persona := "v1:identity:user:role-test-journal"
	a := auth.ForwardedAuthority{CredentialClass: auth.RoleTestClass, RoleTestSessionID: "journal-session", Subject: persona, Role: auth.RoleReader}
	ctx := auth.BindForwardedContext(context.Background(), a.Principal().Claims, &auth.AccessContext{UserId: persona, Role: auth.RoleReader}, a)
	owner, calls, revokeDuringRead := persona, 0, false
	exec := roleJournalExecutor(func(read context.Context, q string) (*memql.ExecuteResult, error) {
		calls++
		if err := auth.ValidateRoleTestContext(read); err != nil {
			t.Fatal(err)
		}
		ac, _ := auth.AccessFromContext(read)
		if ac.UserId != workJournalActor || !ac.Synthetic || auth.OriginFromContext(read) != auth.OriginInternal {
			t.Fatal("journal read lost fixed service identity")
		}
		if strings.HasPrefix(q, "query workRunById(") {
			return memql.NewResultWithOutput([]map[string]any{{"id": "run", "ownerUserId": owner, "automationName": "review"}}), nil
		}
		if !strings.HasPrefix(q, "query workStepsForRun(") {
			t.Fatalf("unexpected read %s", q)
		}
		if revokeDuringRead {
			revoked = true
		}
		return memql.NewResultWithOutput([]map[string]any{}), nil
	})
	if _, err := LoadRunJournal(ctx, exec, "run"); err != nil {
		t.Fatal(err)
	}
	owner = "another-user"
	if _, err := LoadRunJournal(ctx, exec, "run"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("read another owner's journal: %v", err)
	}
	if calls != 3 {
		t.Fatal("unauthorized journal exposed its steps")
	}
	owner = persona
	revokeDuringRead = true
	if result, err := LoadRunJournal(ctx, exec, "run"); err == nil || result != nil {
		t.Fatal("in-flight revocation exposed journal")
	}
	if _, err := LoadRunJournal(ctx, exec, "run"); err == nil {
		t.Fatal("ended test read journal")
	}
	if calls != 5 {
		t.Fatal("ended test reached service read")
	}
}
