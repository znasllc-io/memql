package http

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// appSessionUserEngine answers the identity store's user-by-id
// lookup with exactly the users it was seeded with, and nothing for
// anyone else. That single behaviour is what the mint path gates on.
type appSessionUserEngine struct {
	users map[string]bool
}

func (e *appSessionUserEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if !strings.HasPrefix(q, "query userByIdSystem(") {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	for id := range e.users {
		if strings.Contains(q, id) {
			payload, err := structpb.NewStruct(map[string]any{
				"id": id, "email": "u@example.com", "role": "writer", "active": true,
			})
			if err != nil {
				return nil, err
			}
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
				Nodes: []*memqlv1.MemoryNode{{Id: id, Payload: payload}},
			}}, nil
		}
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func newAppSessionCredentialServer(t *testing.T, secret string, knownUsers ...string) *Server {
	t.Helper()
	s := newBootstrapTestServer(t, secret)
	users := map[string]bool{}
	for _, u := range knownUsers {
		users[u] = true
	}
	s.Store = &identity.Store{Engine: &appSessionUserEngine{users: users}, Logger: slog.Default()}
	s.Logger = slog.Default()
	return s
}

func appSessionBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	fields["tokenClass"] = "app_session"
	body, err := json.Marshal(fields)
	require.NoError(t, err)
	return string(body)
}

// TestAppSessionCredential_SubjectIsTheUser is the security core of
// memql#4360's back-channel: the minted bearer's `sub` is the OWNING
// USER's id, which is what makes row authz apply to the delegated app
// exactly as it does to that user's browser. A machine-principal
// subject here would silently detach the app from the user's own
// access, and nothing downstream would notice.
func TestAppSessionCredential_SubjectIsTheUser(t *testing.T) {
	const secret = "bootstrap-secret"
	const userId = "v1:identity:user:alice"
	s := newAppSessionCredentialServer(t, secret, userId)

	rec := driveBootstrapRequest(t, s, secret, appSessionBody(t, map[string]any{
		"ownerUserId": userId,
		"sessionId":   "v1:worker:appSession:s1",
	}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	resp := decodeBootstrapResponse(t, rec)
	require.True(t, resp.Success)
	require.NotEmpty(t, resp.PlainToken)

	claims, err := s.Issuer.VerifyAccessToken(resp.PlainToken, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, identity.ClassAppSession, claims.Class,
		"the credential must verify on every node's DB-free JWKS path, AS ITSELF")
	assert.NotEqual(t, identity.ClassServiceAccount, claims.Class,
		"it was minted as service_account until memql#4857, and the collapse is what "+
			"stopped the Library's byte routes admitting it: one class name for a "+
			"machine subject and a user subject cannot express a rule that admits "+
			"only the second")
	assert.Equal(t, userId, claims.Subject,
		"sub must be the owning user so row authz applies to the delegated app")
	assert.Equal(t, "app-session:v1:worker:appSession:s1", claims.NodeId,
		"the label must name the session so a leaked bearer traces back to the run")
}

// TestAppSessionCredential_RequiresAKnownUser: the bootstrap secret
// previously bought only MACHINE identities. This path mints a
// user-scoped one, so the named user must exist -- otherwise a forged
// subject would verify (the verify path is JWKS-only and DB-free) and
// then act as a user nobody can point at in an audit.
func TestAppSessionCredential_RequiresAKnownUser(t *testing.T) {
	const secret = "bootstrap-secret"
	s := newAppSessionCredentialServer(t, secret, "v1:identity:user:alice")

	rec := driveBootstrapRequest(t, s, secret, appSessionBody(t, map[string]any{
		"ownerUserId": "v1:identity:user:nobody",
		"sessionId":   "v1:worker:appSession:s1",
	}))
	assert.Equal(t, 400, rec.Code)
	resp := decodeBootstrapResponse(t, rec)
	assert.False(t, resp.Success)
	assert.Empty(t, resp.PlainToken, "a refused mint must not leak a token")
	assert.Equal(t, "bad_request", resp.ErrorCode,
		"the same shape as any other bad request -- this endpoint is not a user-existence oracle")
}

func TestAppSessionCredential_RequiresOwnerAndSession(t *testing.T) {
	const secret = "bootstrap-secret"
	s := newAppSessionCredentialServer(t, secret, "v1:identity:user:alice")

	for _, body := range []map[string]any{
		{"sessionId": "s1"},
		{"ownerUserId": "v1:identity:user:alice"},
		{},
	} {
		rec := driveBootstrapRequest(t, s, secret, appSessionBody(t, body))
		assert.Equal(t, 400, rec.Code, "body %v", body)
		assert.Empty(t, decodeBootstrapResponse(t, rec).PlainToken)
	}
}

// TestAppSessionCredential_TTLIsCapped: the bearer ends up in a file
// on somebody's laptop and there is no per-token revoke (the verify
// path is DB-free by design). Capping the lifetime is the mitigation
// that does not depend on that file being deleted, so the cap must
// hold whatever the caller asks for.
func TestAppSessionCredential_TTLIsCapped(t *testing.T) {
	const secret = "bootstrap-secret"
	const userId = "v1:identity:user:alice"
	s := newAppSessionCredentialServer(t, secret, userId)

	rec := driveBootstrapRequest(t, s, secret, appSessionBody(t, map[string]any{
		"ownerUserId": userId,
		"sessionId":   "s1",
		"ttlSeconds":  int64((30 * 24 * time.Hour) / time.Second),
	}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	resp := decodeBootstrapResponse(t, rec)

	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	require.NoError(t, err)
	ttl := time.Until(expiresAt)
	assert.LessOrEqual(t, ttl, appSessionCredentialMaxTTL+time.Minute,
		"a 30-day request must be clamped to the hard ceiling")
	assert.Greater(t, ttl, appSessionCredentialMaxTTL-time.Minute,
		"clamping must land ON the ceiling, not below it")
}

// TestAppSessionCredential_RefusedWithoutAStore: a binary with no
// engine cannot check that the user exists, so it refuses rather than
// minting an unverifiable user-scoped credential.
func TestAppSessionCredential_RefusedWithoutAStore(t *testing.T) {
	const secret = "bootstrap-secret"
	s := newBootstrapTestServer(t, secret)
	s.Logger = slog.Default()

	rec := driveBootstrapRequest(t, s, secret, appSessionBody(t, map[string]any{
		"ownerUserId": "v1:identity:user:alice",
		"sessionId":   "s1",
	}))
	assert.Equal(t, 503, rec.Code)
	assert.Equal(t, "store_unavailable", decodeBootstrapResponse(t, rec).ErrorCode)
}
