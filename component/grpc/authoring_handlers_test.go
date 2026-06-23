package memql

// Tests for the cockpit-facing authoring gRPC handlers (issue memql#2128 / C1):
// AuthoringValidateBundle (read-only Gate-1 sandbox) and
// AuthoringSessionDefineBundle (validate -> inject into the stream-scoped,
// owner-scoped authored registry). They prove the gate (owner/developer only),
// the validate-fail path (diagnostics, no mutation), and the inject-success path
// (defined constructs become callable by name in the session registry).

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

const authConceptSrc = `@version("1.0.0")
@namespace("c1grpc")
@description("C1 grpc test widget")
concept c1Widget {
  ownerUserId  string  @required
  label        string
}`

const authMutationSrc = `use c1grpc.concepts.{ c1Widget }

@description("Create a C1 grpc widget")
mutate c1Widget mutationCreateC1Widget {
  args {
    widgetId  string  @required
  }
  insert {
    id:    canonicalId(args.widgetId, c1Widget)
    label: "x"
  }
}`

// newAuthoringSession builds a streamSession with a pre-seeded AccessContext so
// the authoring gate resolves without a DB.
func newAuthoringSession(t *testing.T, role auth.Role, userId string) (*streamSession, *captureStream) {
	t.Helper()
	cs := newCaptureStream(t)
	svc := &service{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	s := &streamSession{
		service: svc,
		stream:  cs,
		logger:  svc.logger,
		access:  &auth.AccessContext{UserId: userId, Role: role},
	}
	s.accessLoaded = true // short-circuit ensureAccess to the seeded access
	return s, cs
}

func dispatchValidate(s *streamSession, sources string) error {
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_AuthoringValidateBundle{
			AuthoringValidateBundle: &memqlv1.AuthoringValidateBundleMsg{RequestId: "r1", Sources: sources},
		},
	}
	return s.handleAuthoringValidateBundle(env, env.GetAuthoringValidateBundle())
}

func dispatchSessionDefine(s *streamSession, sources string) error {
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m2",
		Payload: &memqlv1.MemqlClientMessage_AuthoringSessionDefineBundle{
			AuthoringSessionDefineBundle: &memqlv1.AuthoringSessionDefineBundleMsg{RequestId: "r2", Sources: sources},
		},
	}
	return s.handleAuthoringSessionDefineBundle(env, env.GetAuthoringSessionDefineBundle())
}

// A well-formed bundle validates OK with a clean per-construct diagnostic, and
// nothing is registered (validate is pure / read-only).
func TestAuthoringValidate_OK(t *testing.T) {
	s, cs := newAuthoringSession(t, auth.RoleDeveloper, "u1")
	require.NoError(t, dispatchValidate(s, authConceptSrc+"\n\n"+authMutationSrc))

	res := cs.lastSent().GetAuthoringValidateBundleResult()
	require.NotNil(t, res, "reply must be an AuthoringValidateBundleResult")
	assert.True(t, res.GetOk(), "valid bundle should validate OK")
	assert.NotEmpty(t, res.GetDiagnostics())
	// Validate never mutates: no session registry was even created.
	assert.Nil(t, s.authoredSession, "validate must not create or touch a session registry")
}

// The validate-fail path: a broken bundle comes back OK=false with a populated
// hard-fail diagnostic.
func TestAuthoringValidate_FailCarriesDiagnostics(t *testing.T) {
	s, cs := newAuthoringSession(t, auth.RoleOwner, "u1")
	require.NoError(t, dispatchValidate(s, `spec broken { actor.role == }`))

	res := cs.lastSent().GetAuthoringValidateBundleResult()
	require.NotNil(t, res)
	assert.False(t, res.GetOk())
	var hardFail bool
	for _, d := range res.GetDiagnostics() {
		if !d.GetOk() && !d.GetSkipped() {
			hardFail = true
			assert.NotEmpty(t, d.GetError(), "hard-fail diagnostic must carry an error message")
		}
	}
	assert.True(t, hardFail, "expected a non-skipped hard failure diagnostic")
}

// The authoring gate rejects a non-author role (writer/reader/admin) with
// permission_denied, and never validates.
func TestAuthoring_RoleGateRejects(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader, auth.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			s, cs := newAuthoringSession(t, role, "u1")
			require.NoError(t, dispatchValidate(s, authConceptSrc))

			msg := cs.lastSent()
			require.NotNil(t, msg)
			qe := msg.GetQueryError()
			require.NotNil(t, qe, "a non-author caller must get a query error, got %T", msg.GetPayload())
			assert.Equal(t, "PermissionDenied", qe.GetError().GetCode())
		})
	}
}

// The inject-success path: a valid bundle session-defines into the stream's
// owner-scoped registry; the construct becomes callable by name (it is present
// in the registry under the caller's owner id).
func TestAuthoringSessionDefine_InjectsCallable(t *testing.T) {
	s, cs := newAuthoringSession(t, auth.RoleDeveloper, "owner-1")
	require.NoError(t, dispatchSessionDefine(s, authConceptSrc+"\n\n"+authMutationSrc))

	res := cs.lastSent().GetAuthoringSessionDefineBundleResult()
	require.NotNil(t, res, "reply must be an AuthoringSessionDefineBundleResult")
	assert.True(t, res.GetOk(), "valid bundle should session-define; error=%q", res.GetError())
	assert.Empty(t, res.GetError())

	var definedNames []string
	for _, c := range res.GetDefined() {
		definedNames = append(definedNames, c.GetName())
	}
	assert.Contains(t, definedNames, "mutationCreateC1Widget", "the function-family construct must be reported as defined")

	// It is callable by name: present in the stream's owner-scoped registry.
	require.NotNil(t, s.authoredSession, "session-define must create the stream registry")
	c, ok := s.authoredSession.Lookup("owner-1", "mutation", "mutationCreateC1Widget")
	require.True(t, ok, "the session-defined construct must be registered under the caller's owner id")
	assert.Equal(t, "mutationCreateC1Widget", c.Name)
}

// A failing session-define registers nothing and surfaces the error + the
// per-construct diagnostics.
func TestAuthoringSessionDefine_FailRegistersNothing(t *testing.T) {
	s, cs := newAuthoringSession(t, auth.RoleOwner, "owner-1")
	require.NoError(t, dispatchSessionDefine(s, `spec broken { actor.role == }`))

	res := cs.lastSent().GetAuthoringSessionDefineBundleResult()
	require.NotNil(t, res)
	assert.False(t, res.GetOk())
	assert.NotEmpty(t, res.GetError(), "a rejected bundle must carry an error")
	assert.NotEmpty(t, res.GetDiagnostics())
	if s.authoredSession != nil {
		assert.Equal(t, 0, s.authoredSession.Count(), "nothing should be registered on failure")
	}
}
