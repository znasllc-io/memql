package automations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

func TestRoleTestCancellationDB_OwnRunAndRevocation(t *testing.T) {
	engine := sharedJournalEngine(t)
	persona := "v1:identity:user:role-test-cancel"
	revoked := false
	auth.SetRoleTestValidator(journalRoleValidator(func(_ context.Context, session, user string, role auth.Role) error {
		if revoked || session != "cancel-session" || role != auth.RoleReader || !auth.IsRoleTestUser(user) {
			return errors.New("test ended or invalid")
		}
		return nil
	}))
	t.Cleanup(func() { auth.SetRoleTestValidator(nil) })
	actor := func(user string) context.Context {
		a := auth.ForwardedAuthority{CredentialClass: auth.RoleTestClass, RoleTestSessionID: "cancel-session", Subject: user, Role: auth.RoleReader, ExpiresAt: time.Now().Add(time.Minute)}
		return auth.BindForwardedContext(context.Background(), a.Principal().Claims, &auth.AccessContext{UserId: user, Role: auth.RoleReader}, a)
	}
	ctx := actor(persona)
	runID := fmt.Sprintf("v1:work:run:cancel-test-%d", time.Now().UnixNano())
	write := func(name string, args map[string]any) {
		t.Helper()
		q, err := journalArgs(name, args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+q); err != nil {
			t.Fatal(err)
		}
	}
	write("createWorkRun", map[string]any{"runId": runID, "automationName": "cancelProbe", "templateFingerprint": "cancelProbe", "status": "running", "startedAt": time.Now().Format(time.RFC3339Nano)})
	calls, revokeDuringRead := 0, false
	exec := roleJournalExecutor(func(read context.Context, q string) (*memql.ExecuteResult, error) {
		calls++
		ac, _ := auth.AccessFromContext(read)
		if !strings.HasPrefix(q, "query workRunForOwner(") || ac.Synthetic || ac.Role != auth.RoleReader || !memql.FreshReadFromContext(read) {
			t.Fatalf("cancel poll changed the persona or read scope: %s %+v", q, ac)
		}
		res, err := engine.Execute(read, q)
		if revokeDuringRead {
			revoked = true
		}
		return res, err
	})
	j := newWorkJournal(exec, nil)
	if asked, _ := j.cancelRequested(ctx, runID); asked {
		t.Fatal("uncancelled run was stopped")
	}
	write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true, "cancelledBy": persona})
	if asked, by := j.cancelRequested(ctx, runID); !asked || by != persona {
		t.Fatalf("Viewer's stop was ignored: %v %q", asked, by)
	}
	if asked, by := j.cancelRequested(actor("v1:identity:user:role-test-other"), runID); asked || by != "" {
		t.Fatal("another persona read this run's cancellation")
	}
	revokeDuringRead = true
	if asked, by := j.cancelRequested(ctx, runID); asked || by != "" {
		t.Fatal("in-flight revocation exposed a cancellation")
	}
	before := calls
	if asked, _ := j.cancelRequested(ctx, runID); asked || calls != before {
		t.Fatal("ended test reached the cancellation read")
	}
}
