package adminops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type displayNameEngine struct {
	readErr, writeErr error
	missing           bool
	queries           []string
	freshRead         bool
	fencedWrite       bool
	internalCalls     bool
}

func (e *displayNameEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, query)
	e.internalCalls = e.internalCalls && auth.OriginFromContext(ctx).IsInternal()
	if strings.HasPrefix(query, "query userByIdSystem") {
		e.freshRead = memqlengine.FreshReadFromContext(ctx)
		if e.readErr != nil || e.missing {
			return &memqlengine.ExecuteResult{}, e.readErr
		}
		payload, _ := structpb.NewStruct(map[string]any{
			"displayName": "Old name", "primaryEmail": "person@example.test", "firstName": "Keep",
			"lastName": "Family", "phone": "555", "role": "developer", "signInPolicy": "passkey_only",
			"sharedMailbox": true, "preferences": map[string]any{"theme": "dark"},
		})
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:identity:user:target", Payload: payload,
			CreatedAt: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		}}}}, nil
	}
	e.fencedWrite = memqlengine.HasRowVersionFence(ctx, "v1:identity:user", "target")
	return &memqlengine.ExecuteResult{}, e.writeErr
}

func displayNameService(t *testing.T, engine *displayNameEngine) (*Service, *capturingAudit) {
	t.Helper()
	engine.internalCalls = true
	audit := &capturingAudit{}
	service, err := New(&Service{Engine: engine, Audit: audit})
	require.NoError(t, err)
	return service, audit
}

func TestSetUserDisplayNameUsesOnlyTheNameDeltaAndFreshVersion(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			engine := &displayNameEngine{}
			service, audit := displayNameService(t, engine)
			name := "Jésus \"Mendivil\"\nFamily"
			result := service.SetUserDisplayName(ctxAs(role), " target ", " "+name+" ")
			require.True(t, result.OK, result.ErrorMessage)
			require.Len(t, engine.queries, 2)
			require.True(t, engine.internalCalls)
			require.True(t, engine.freshRead)
			require.True(t, engine.fencedWrite)
			require.Equal(t, fmt.Sprintf(`mutation updateUser(userId: "v1:identity:user:target", payload: {displayName: %s})`, quote(name)), engine.queries[1])
			require.Len(t, audit.events, 1)
			require.Equal(t, "user_display_name_changed", audit.events[0].Action)
			require.Equal(t, identity.AuditOutcomeSuccess, audit.events[0].Outcome)
			require.Equal(t, name, audit.events[0].Detail["displayName"])
			require.NotEmpty(t, result.AuditEventId)
		})
	}
}

func TestSetUserDisplayNameInvalidOrMissingTargetCannotWrite(t *testing.T) {
	for _, tc := range []struct {
		name, userID, displayName string
		missing                   bool
		readErr                   error
		wantCode                  int32
		wantReads                 int
	}{
		{name: "missing id", displayName: "Name", wantCode: CodeInvalidArgument},
		{name: "blank name", userID: "target", displayName: " \n ", wantCode: CodeInvalidArgument},
		{name: "missing user", userID: "target", displayName: "Name", missing: true, wantCode: CodeNotFound, wantReads: 1},
		{name: "read failure", userID: "target", displayName: "Name", readErr: errTestEngine, wantCode: CodeInternal, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &displayNameEngine{missing: tc.missing, readErr: tc.readErr}
			service, audit := displayNameService(t, engine)
			result := service.SetUserDisplayName(ctxAs(auth.RoleOwner), tc.userID, tc.displayName)
			require.False(t, result.OK)
			require.Equal(t, tc.wantCode, result.Code)
			require.Len(t, engine.queries, tc.wantReads)
			require.Len(t, audit.events, 1)
		})
	}
}

func TestSetUserDisplayNameConcurrentChangeIsAuditedAndNotRetried(t *testing.T) {
	engine := &displayNameEngine{writeErr: fmt.Errorf("commit: %w", memqlengine.ErrRowVersionChanged)}
	service, audit := displayNameService(t, engine)
	result := service.SetUserDisplayName(ctxAs(auth.RoleOwner), "target", "New name")
	require.False(t, result.OK)
	require.EqualValues(t, CodeFailedPrecondition, result.Code)
	require.Len(t, engine.queries, 2, "a stale read must not trigger a blind retry")
	require.Len(t, audit.events, 1)
	require.Equal(t, "user_version_changed", audit.events[0].FailureReason)
}
