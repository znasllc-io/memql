package http

// POST /auth/badge/grant handler tests (memql#2513). Drives the
// handler directly with httptest against a real JWT issuer (throwaway
// key) and a stub engine that serves badge rows, asserting the full
// exchange contract: golden path claims, unknown/revoked badge,
// class-gated terminal bearer, missing bearer.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/badge"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// badgeStubEngine serves badgeByKeyHash lookups from a fixed row set,
// answers the authSessionByTokenHash revocation probe, and swallows
// every mutation.
type badgeStubEngine struct {
	rows                    map[string]map[string]any // keyHash -> payload fields
	revokedSessionForBearer string                    // raw bearer whose session is revoked
	queries                 []string
}

func (e *badgeStubEngine) node(fields map[string]any) (*memqlengine.ExecuteResult, error) {
	st, err := structpb.NewStruct(fields)
	if err != nil {
		return nil, err
	}
	id, _ := fields["id"].(string)
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: id, Payload: st}}},
	}, nil
}

func (e *badgeStubEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, query)
	if strings.Contains(query, "authSessionByTokenHash") {
		if e.revokedSessionForBearer != "" {
			sum := sha256.Sum256([]byte(strings.TrimSpace(e.revokedSessionForBearer)))
			if strings.Contains(query, hex.EncodeToString(sum[:])) {
				return e.node(map[string]any{"id": "v1:identity:authSession:s1", "revokedAt": "2026-07-14T00:00:00Z"})
			}
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	if !strings.Contains(query, "badgeByKeyHash") {
		return nil, nil
	}
	for hash, fields := range e.rows {
		if strings.Contains(query, hash) {
			return e.node(fields)
		}
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func badgeRow(id, userId string, active bool, keyHash string) map[string]any {
	return map[string]any{
		"id":     id,
		"userId": userId,
		"label":  "Test badge",
		"active": active,
		"credentials": map[string]any{
			"keyHash":      keyHash,
			"registeredBy": "v1:identity:user:admin-1",
			"lastUsedAt":   "",
		},
	}
}

func newBadgeGrantTestServer(t *testing.T, engine *badgeStubEngine) *Server {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	require.NoError(t, err)
	return &Server{
		Cfg:    cfg,
		Issuer: iss,
		Store:  &identity.Store{Engine: engine},
	}
}

func mintTerminalBearer(t *testing.T, s *Server, role string) string {
	t.Helper()
	token, _, err := s.Issuer.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:kiosk-7",
		Role:   role,
	}, time.Now().UTC())
	require.NoError(t, err)
	return token
}

func driveBadgeGrant(t *testing.T, s *Server, bearer string, body BadgeGrantRequest) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv(envAllowInsecurePair, "1")
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://identity.test/auth/badge/grant", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.handleBadgeGrant(rec, req)
	return rec
}

func decodeBadgeGrant(t *testing.T, rec *httptest.ResponseRecorder) BadgeGrantResponse {
	t.Helper()
	var resp BadgeGrantResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

func TestHandleBadgeGrant_GoldenPath(t *testing.T) {
	const badgeId = "04:A3:1B:9C"
	hash := badge.HashBadgeId(badgeId)
	engine := &badgeStubEngine{rows: map[string]map[string]any{
		hash: badgeRow("v1:identity:identity:bdg-1", "v1:identity:user:op-1", true, hash),
	}}
	s := newBadgeGrantTestServer(t, engine)
	bearer := mintTerminalBearer(t, s, "writer")

	rec := driveBadgeGrant(t, s, bearer, BadgeGrantRequest{BadgeId: badgeId})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeBadgeGrant(t, rec)
	require.True(t, resp.Success)
	require.Equal(t, "v1:identity:user:op-1", resp.OperatorUserId)
	require.NotEmpty(t, resp.AccessToken)
	require.NotContains(t, resp.AccessToken, badgeId, "plaintext badge id must never echo")

	claims, err := s.Issuer.VerifyAccessToken(resp.AccessToken, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, "v1:identity:user:op-1", claims.Subject, "sub = the OPERATOR")
	require.Equal(t, identity.ClassBadge, claims.Class)
	require.Equal(t, "writer", claims.RoleCeiling, "ceiling = the TERMINAL's role")
	require.Equal(t, "v1:identity:identity:bdg-1", claims.NodeId)

	exp, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	require.NoError(t, err)
	require.WithinDuration(t,
		time.Now().Add(time.Duration(identity.DefaultBadgeGrantTTLSeconds)*time.Second), exp, 5*time.Second)
}

func TestHandleBadgeGrant_UnknownBadge(t *testing.T) {
	engine := &badgeStubEngine{rows: map[string]map[string]any{}}
	s := newBadgeGrantTestServer(t, engine)
	bearer := mintTerminalBearer(t, s, "writer")

	rec := driveBadgeGrant(t, s, bearer, BadgeGrantRequest{BadgeId: "not-registered"})
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "unknown_badge", decodeBadgeGrant(t, rec).ErrorCode)
}

func TestHandleBadgeGrant_RevokedBadge(t *testing.T) {
	const badgeId = "04:A3:1B:9C"
	hash := badge.HashBadgeId(badgeId)
	engine := &badgeStubEngine{rows: map[string]map[string]any{
		hash: badgeRow("v1:identity:identity:bdg-1", "v1:identity:user:op-1", false, hash),
	}}
	s := newBadgeGrantTestServer(t, engine)
	bearer := mintTerminalBearer(t, s, "writer")

	rec := driveBadgeGrant(t, s, bearer, BadgeGrantRequest{BadgeId: badgeId})
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "badge_revoked", decodeBadgeGrant(t, rec).ErrorCode)
}

func TestHandleBadgeGrant_NoChaining(t *testing.T) {
	// A badge grant cannot mint from another badge grant: the terminal
	// bearer must be user-class.
	const badgeId = "04:A3:1B:9C"
	hash := badge.HashBadgeId(badgeId)
	engine := &badgeStubEngine{rows: map[string]map[string]any{
		hash: badgeRow("v1:identity:identity:bdg-1", "v1:identity:user:op-1", true, hash),
	}}
	s := newBadgeGrantTestServer(t, engine)

	grantBearer, _, err := s.Issuer.IssueBadgeGrantToken(identity.BadgeGrantIssueInput{
		UserId:      "v1:identity:user:op-1",
		RoleCeiling: "writer",
	}, time.Now().UTC())
	require.NoError(t, err)

	rec := driveBadgeGrant(t, s, grantBearer, BadgeGrantRequest{BadgeId: badgeId})
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "terminal_class_rejected", decodeBadgeGrant(t, rec).ErrorCode)
}

func TestHandleBadgeGrant_MissingBearer(t *testing.T) {
	s := newBadgeGrantTestServer(t, &badgeStubEngine{})
	rec := driveBadgeGrant(t, s, "", BadgeGrantRequest{BadgeId: "x"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleBadgeGrant_TTLOverrideCapped(t *testing.T) {
	const badgeId = "04:A3:1B:9C"
	hash := badge.HashBadgeId(badgeId)
	engine := &badgeStubEngine{rows: map[string]map[string]any{
		hash: badgeRow("v1:identity:identity:bdg-1", "v1:identity:user:op-1", true, hash),
	}}
	s := newBadgeGrantTestServer(t, engine)
	bearer := mintTerminalBearer(t, s, "writer")

	rec := driveBadgeGrant(t, s, bearer, BadgeGrantRequest{BadgeId: badgeId, TTLSeconds: 86400})
	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeBadgeGrant(t, rec)
	exp, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(time.Hour), exp, 5*time.Second,
		"per-request TTL must cap at one hour")
}

func TestHandleBadgeGrant_RevokedTerminalSession(t *testing.T) {
	// The identity node runs no session-revocation interceptor, so the
	// grant endpoint must consult the authSession row itself: a station
	// whose session an admin revoked cannot keep minting operator grants.
	const badgeId = "04:A3:1B:9C"
	hash := badge.HashBadgeId(badgeId)
	engine := &badgeStubEngine{rows: map[string]map[string]any{
		hash: badgeRow("v1:identity:identity:bdg-1", "v1:identity:user:op-1", true, hash),
	}}
	s := newBadgeGrantTestServer(t, engine)
	bearer := mintTerminalBearer(t, s, "writer")

	// Mark the terminal bearer's session revoked.
	engine.revokedSessionForBearer = bearer

	rec := driveBadgeGrant(t, s, bearer, BadgeGrantRequest{BadgeId: badgeId})
	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "terminal_session_revoked", decodeBadgeGrant(t, rec).ErrorCode)
}

func TestHandleBadgeGrant_EmptyTerminalRole(t *testing.T) {
	// An external user carries an empty role claim; a grant needs a role
	// for its ceiling, so refuse with a typed error, not a 500.
	const badgeId = "04:A3:1B:9C"
	hash := badge.HashBadgeId(badgeId)
	engine := &badgeStubEngine{rows: map[string]map[string]any{
		hash: badgeRow("v1:identity:identity:bdg-1", "v1:identity:user:op-1", true, hash),
	}}
	s := newBadgeGrantTestServer(t, engine)
	bearer := mintTerminalBearer(t, s, "") // empty role

	rec := driveBadgeGrant(t, s, bearer, BadgeGrantRequest{BadgeId: badgeId})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "terminal_role_missing", decodeBadgeGrant(t, rec).ErrorCode)
}
