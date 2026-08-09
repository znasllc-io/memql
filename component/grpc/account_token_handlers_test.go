package memql

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/accounttoken"
	"github.com/znasllc-io/memql/component/identity/workertoken"
)

// The runnable half of the account-token handler's behaviour: the
// branches that resolve before any engine call, plus the two structural
// properties (badge restriction, audit shape) that need no database.
//
// The authorization claim itself -- a non-owner can neither read an
// account nor mint against it -- is in account_token_authz_db_test.go,
// against a real engine, because it is a claim about the engine.

func accountTokenSession(t *testing.T, userId string) (*streamSession, *captureStream) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cs := newCaptureStream(t)
	cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: userId})
	return &streamSession{
		// engine deliberately nil: every case below must resolve BEFORE
		// the handler would touch it, and a nil engine turns "resolved
		// too late" into a visible unavailable rather than a panic.
		service:      &service{logger: logger},
		stream:       cs,
		logger:       logger,
		access:       &auth.AccessContext{UserId: userId, Role: auth.RoleWriter},
		accessLoaded: true,
	}, cs
}

func TestCreateAccountTokenRefusesIncompleteRequests(t *testing.T) {
	for _, tc := range []struct {
		name      string
		msg       *memqlv1.CreateAccountTokenMsg
		wantCode  string
		engineNil bool
	}{
		{
			name:      "no engine",
			msg:       &memqlv1.CreateAccountTokenMsg{RequestId: "r", AccountId: "a", Label: "l"},
			wantCode:  "unavailable",
			engineNil: true,
		},
		{
			name:      "no account",
			msg:       &memqlv1.CreateAccountTokenMsg{RequestId: "r", Label: "l"},
			wantCode:  "bad_request",
			engineNil: true,
		},
		{
			name:      "no label",
			msg:       &memqlv1.CreateAccountTokenMsg{RequestId: "r", AccountId: "a"},
			wantCode:  "bad_request",
			engineNil: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cs := accountTokenSession(t, "v1:identity:user:ada")
			env := &memqlv1.MemqlClientMessage{
				MessageId: "m",
				Payload:   &memqlv1.MemqlClientMessage_CreateAccountToken{CreateAccountToken: tc.msg},
			}
			require.NoError(t, s.handleCreateAccountToken(env, tc.msg))

			res := cs.lastSent().GetCreateAccountTokenResult()
			require.NotNil(t, res, "the handler must always reply")
			assert.False(t, res.GetSuccess())
			assert.Equal(t, tc.wantCode, res.GetErrorCode())
			assert.Empty(t, res.GetPlainToken(),
				"a refused mint must not return a credential")
		})
	}
}

// An unlabelled credential is refused rather than defaulted. The
// revoke surface lists credentials by label, so a blank one is a row an
// operator cannot confidently pick out of a list of blanks -- and the
// consequence of picking the wrong one is revoking a live integration.
func TestCreateAccountTokenSaysWhyALabelIsRequired(t *testing.T) {
	s, cs := accountTokenSession(t, "v1:identity:user:ada")
	msg := &memqlv1.CreateAccountTokenMsg{RequestId: "r", AccountId: "a", Label: "   "}
	require.NoError(t, s.handleCreateAccountToken(
		&memqlv1.MemqlClientMessage{MessageId: "m"}, msg))
	res := cs.lastSent().GetCreateAccountTokenResult()
	require.NotNil(t, res)
	assert.Equal(t, "bad_request", res.GetErrorCode())
	assert.Contains(t, res.GetErrorMessage(), "revoke",
		"the message should say what a blank label costs, not just that it is missing")
}

func TestRevokeAccountTokenRefusesIncompleteRequests(t *testing.T) {
	s, cs := accountTokenSession(t, "v1:identity:user:ada")
	msg := &memqlv1.RevokeAccountTokenMsg{RequestId: "r"}
	require.NoError(t, s.handleRevokeAccountToken(
		&memqlv1.MemqlClientMessage{MessageId: "m"}, msg))
	res := cs.lastSent().GetRevokeAccountTokenResult()
	require.NotNil(t, res)
	assert.False(t, res.GetSuccess())
	assert.Equal(t, "bad_request", res.GetErrorCode(),
		"a revoke naming no credential is malformed regardless of whether this node "+
			"has an engine wired")
}

// A live badge grant is a shared-terminal credential with a short TTL.
// An account token is DURABLE and is minted under the operator's own
// subject, so a walked-away kiosk must not be able to leave one behind
// -- nor to dismantle the operator's issued set on the way out.
func TestAccountTokenMintAndRevokeAreBadgeRestricted(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  *memqlv1.MemqlClientMessage
	}{
		{"mint", &memqlv1.MemqlClientMessage{
			Payload: &memqlv1.MemqlClientMessage_CreateAccountToken{
				CreateAccountToken: &memqlv1.CreateAccountTokenMsg{RequestId: "r"}}}},
		{"revoke", &memqlv1.MemqlClientMessage{
			Payload: &memqlv1.MemqlClientMessage_RevokeAccountToken{
				RevokeAccountToken: &memqlv1.RevokeAccountTokenMsg{RequestId: "r"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := accountTokenSession(t, "v1:identity:user:ada")
			s.badgeStamped = true
			s.badgeExpiresAt = time.Now().Add(time.Hour)

			verdict := s.badgeGate(tc.env)
			assert.Equal(t, badgeGateRestricted, verdict,
				"a live badge grant must not reach the account-token %s surface", tc.name)
		})
	}
}

// recordingAuditor captures what was logged instead of writing it.
type recordingAuditor struct{ events []identity.AuditEvent }

func (r *recordingAuditor) Log(_ context.Context, ev identity.AuditEvent) {
	r.events = append(r.events, ev)
}

// The audit row's shape is a contract with whoever reads the trail
// later, and two of its properties are the whole point of the feature:
// the account binding is recorded, and the SUBJECT is stated to be a
// user rather than left to be inferred.
func TestAuditAccountTokenRecordsTheBindingAndTheSubjectKind(t *testing.T) {
	rec := &recordingAuditor{}
	caller := auth.UserIdentity{Subject: "v1:identity:user:ada", Email: "ada@example.com", Role: "writer"}

	eventId := auditAccountToken(context.Background(), rec, caller, "account_token_created",
		"v1:identity:account:acme", "v1:identity:identity:tok-1", "Nightly export",
		identity.AuditOutcomeSuccess, "")

	require.Len(t, rec.events, 1)
	ev := rec.events[0]
	assert.Equal(t, identity.AuditCategoryAuth, ev.Category)
	assert.Equal(t, "account_token_created", ev.Action)
	assert.Equal(t, "identity", ev.TargetType,
		"targetType is a CLOSED enum on createAuditEvent; the credential row is the "+
			"target and the account rides in detail")
	assert.Equal(t, "v1:identity:identity:tok-1", ev.TargetId)
	assert.Equal(t, "v1:identity:account:acme", ev.Detail["accountId"])
	assert.Equal(t, "Nightly export", ev.Detail["label"])
	assert.Equal(t, "user", ev.Detail["subjectKind"],
		"the trail must SAY the credential's subject is a user. Leaving a reader to "+
			"infer it from actorUserId is how 'the account authenticated' becomes the "+
			"story a year from now.")
	assert.Equal(t, "account_token", ev.Detail["credentialFamily"])
	assert.Equal(t, eventId, ev.CorrelationId,
		"the reply's audit_event_id is the row's correlationId (deploycontrol's "+
			"convention), so the two must be the same value")
	assert.NotEmpty(t, eventId)
}

func TestAuditAccountTokenRecordsBlockedAttempts(t *testing.T) {
	rec := &recordingAuditor{}
	caller := auth.UserIdentity{Subject: "v1:identity:user:mallory"}
	auditAccountToken(context.Background(), rec, caller, "account_token_create_blocked",
		"v1:identity:account:acme", "", "stolen", identity.AuditOutcomeBlocked, "not_account_owner")

	require.Len(t, rec.events, 1)
	assert.Equal(t, identity.AuditOutcomeBlocked, rec.events[0].Outcome)
	assert.Equal(t, "not_account_owner", rec.events[0].FailureReason)
}

// The audit detail must never carry credential material. There is no
// parameter for it, and this pins that: a future signature that grew
// one would fail here rather than in a log-review six months later.
func TestAuditAccountTokenCarriesNoCredentialMaterial(t *testing.T) {
	plain, hash, err := accounttoken.Mint()
	require.NoError(t, err)

	rec := &recordingAuditor{}
	auditAccountToken(context.Background(), rec, auth.UserIdentity{Subject: "u"},
		"account_token_created", "acct", "tok", "label", identity.AuditOutcomeSuccess, "")

	require.Len(t, rec.events, 1)
	for k, v := range rec.events[0].Detail {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, plain) || strings.Contains(s, hash) {
			t.Errorf("audit detail %q carries credential material", k)
		}
	}
}

// mql_acct_ is RESERVED at the interceptors' prefix check but ADMITTED
// nowhere. Both halves matter, so both are asserted: reserved, so a
// presented account token short-circuits instead of being parsed as a
// JWT; and absent from every admitting path, which is what makes
// "authorizes nothing" true rather than aspirational.
func TestAccountTokenPrefixIsReservedButNotAdmitted(t *testing.T) {
	assert.True(t, hasReservedTokenPrefix(accounttoken.TokenPrefix+"whatever"),
		"an account token presented as a Bearer must be recognised as not-a-JWT")
	assert.False(t, hasReservedTokenPrefix("eyJhbGciOiJSUzI1NiJ9.e30.sig"),
		"the reserved check must not swallow real JWTs")

	// The admission half. component/grpc's only bearer-family admission
	// path is the worker interceptor, which resolves mql_wkr_ and
	// nothing else; an account token offered to it must be treated as
	// ordinary Bearer traffic and fall through to the base verifier
	// (which has no branch for the prefix either), NOT admitted.
	assert.False(t, workertoken.IsWorkerToken(accounttoken.TokenPrefix+"whatever"),
		"an account token must not satisfy the worker-token predicate; if it does, "+
			"the two prefixes have collided and an mql_acct_ bearer would reach "+
			"WorkerService")
}
