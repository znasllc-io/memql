package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// Store is a narrow database adapter that exposes the identity
// service's persistence operations as plain Go methods. Wraps
// engine.Execute() with the right MemQL mutation / query strings and
// parses the returned graph bundles into typed row structs the
// magic-link / refresh / registration packages can consume directly.
type Store struct {
	Engine EngineExecutor
	Logger *slog.Logger
}

// MagicLinkRow is the minimal projection the magic-link verifier
// needs from a v1:identity:magicLinkRequest row.
type MagicLinkRow struct {
	ID           string
	Email        string
	TokenHash    string
	ExpiresAt    time.Time
	ConsumedAt   time.Time // zero = not consumed
	OAuthCtxJSON string
	InvitationId string
	SourceIP     string
	UserAgent    string
	CreatedAt    time.Time
}

// AuthCodeRow projects a v1:identity:authCode row.
type AuthCodeRow struct {
	ID                 string
	CodeHash           string
	ClientId           string
	RedirectURI        string
	State              string
	UserId             string
	IdentityId         string
	MagicLinkRequestId string
	ExpiresAt          time.Time
	ConsumedAt         time.Time // zero = not consumed
	CreatedAt          time.Time
}

// UserRow projects a v1:identity:user row.
type UserRow struct {
	ID           string
	DisplayName  string
	FirstName    string
	LastName     string
	PrimaryEmail string
	Phone        string
	PrimaryRole  string
	Gender       string
	Birthdate    string
	Role         string
	Active       bool
	Internal     bool
	CreatedAt    time.Time
}

// AuthSessionRow projects a v1:identity:authSession row.
type AuthSessionRow struct {
	ID                       string
	UserId                   string
	IdentityId               string
	Subject                  string
	TokenHash                string
	Source                   string
	ClientLabel              string
	ExpiresAt                time.Time
	LastActivityAt           time.Time
	LastRefreshedAt          time.Time
	FirstAuthAt              time.Time
	RefreshTokenHash         string
	PreviousRefreshTokenHash string
	PreviousRotatedAt        time.Time
	RevokedAt                time.Time
	RevokedReason            string
	CreatedAt                time.Time
}

// ---------------------------------------------------------------------------
// Magic-link rows
// ---------------------------------------------------------------------------

// CreateMagicLinkRequest persists a freshly issued magic-link.
//
// Optional string args are always passed (with empty defaults) rather
// than omitted: the underlying concept schema requires the columns
// even though the mutation's args block flags them optional, and a
// missing arg surfaces as `null` to the JSON-schema validator at
// insert time.
func (s *Store) CreateMagicLinkRequest(
	ctx context.Context,
	requestId, email, tokenHash, expiresAt string,
	sourceIP, userAgent, oauthCtxJSON, invitationId string,
) error {
	var b strings.Builder
	b.WriteString(`mutationCreateMagicLinkRequest({`)
	writeKVString(&b, "requestId", requestId, true)
	writeKVString(&b, "email", email, false)
	writeKVString(&b, "tokenHash", tokenHash, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "sourceIP", sourceIP, false)
	writeKVString(&b, "userAgent", userAgent, false)
	writeKVString(&b, "oauthCtxJSON", oauthCtxJSON, false)
	writeKVString(&b, "invitationId", invitationId, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create magic link: %w", err)
	}
	return nil
}

// ConsumeMagicLinkRequest stamps consumedAt on a magic-link row.
func (s *Store) ConsumeMagicLinkRequest(ctx context.Context, requestId, consumedFromIP string) error {
	var b strings.Builder
	b.WriteString(`mutationConsumeMagicLinkRequest({`)
	writeKVString(&b, "requestId", requestId, true)
	writeKVString(&b, "consumedFromIP", consumedFromIP, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: consume magic link: %w", err)
	}
	return nil
}

// LookupMagicLinkByTokenHash returns the row matching the given hash,
// or nil if none exists.
func (s *Store) LookupMagicLinkByTokenHash(ctx context.Context, tokenHash string) (*MagicLinkRow, error) {
	query := fmt.Sprintf(`queryMagicLinkRequestByTokenHash({tokenHash: "%s"})`, escapeMemQLString(tokenHash))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup magic link: %w", err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	node := nodes[0]
	if node == nil {
		return nil, nil
	}
	g := newFieldGetter(node)
	return &MagicLinkRow{
		ID:           firstNonEmpty(g.str("id"), node.GetId()),
		Email:        g.str("email"),
		TokenHash:    g.str("tokenHash"),
		ExpiresAt:    g.time("expiresAt"),
		ConsumedAt:   g.time("consumedAt"),
		OAuthCtxJSON: g.str("oauthCtxJSON"),
		InvitationId: g.str("invitationId"),
		SourceIP:     g.str("sourceIP"),
		UserAgent:    g.str("userAgent"),
		CreatedAt:    g.time("createdAt"),
	}, nil
}

// ---------------------------------------------------------------------------
// Access requests (waitlist mode)
// ---------------------------------------------------------------------------

// CreateAccessRequest enqueues a v1:identity:accessRequest row.
func (s *Store) CreateAccessRequest(
	ctx context.Context,
	requestId, email, name, additionalContext string,
	riskScore int, riskSignals, sourceIP, userAgent string,
) error {
	var b strings.Builder
	b.WriteString(`mutationCreateAccessRequest({`)
	writeKVString(&b, "requestId", requestId, true)
	writeKVString(&b, "email", email, false)
	writeKVString(&b, "name", name, false)
	writeKVString(&b, "additionalContext", additionalContext, false)
	fmt.Fprintf(&b, `,"riskScore":%d`, riskScore)
	writeKVString(&b, "riskSignals", riskSignals, false)
	writeKVString(&b, "sourceIP", sourceIP, false)
	writeKVString(&b, "userAgent", userAgent, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create access request: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Auth codes (OAuth code flow)
// ---------------------------------------------------------------------------

// CreateAuthCode mints a one-time OAuth auth code.
func (s *Store) CreateAuthCode(
	ctx context.Context,
	codeId, code, codeHash, clientId, redirectURI, state,
	userId, identityId, magicLinkRequestId, expiresAt string,
) error {
	var b strings.Builder
	b.WriteString(`mutationCreateAuthCode({`)
	writeKVString(&b, "codeId", codeId, true)
	writeKVString(&b, "code", code, false)
	writeKVString(&b, "codeHash", codeHash, false)
	writeKVString(&b, "clientId", clientId, false)
	writeKVString(&b, "redirectURI", redirectURI, false)
	writeKVString(&b, "state", state, false)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "identityId", identityId, false)
	writeKVString(&b, "magicLinkRequestId", magicLinkRequestId, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create auth code: %w", err)
	}
	return nil
}

// ConsumeAuthCode stamps consumedAt on an auth-code row.
func (s *Store) ConsumeAuthCode(ctx context.Context, codeId, consumedFromIP string) error {
	var b strings.Builder
	b.WriteString(`mutationConsumeAuthCode({`)
	writeKVString(&b, "codeId", codeId, true)
	writeKVString(&b, "consumedFromIP", consumedFromIP, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: consume auth code: %w", err)
	}
	return nil
}

// LookupAuthCodeByCodeHash returns the row matching the given code
// hash, or nil if none exists.
func (s *Store) LookupAuthCodeByCodeHash(ctx context.Context, codeHash string) (*AuthCodeRow, error) {
	query := fmt.Sprintf(`queryAuthCodeByCodeHash({codeHash: "%s"})`, escapeMemQLString(codeHash))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup auth code: %w", err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	node := nodes[0]
	if node == nil {
		return nil, nil
	}
	g := newFieldGetter(node)
	return &AuthCodeRow{
		ID:                 firstNonEmpty(g.str("id"), node.GetId()),
		CodeHash:           g.str("codeHash"),
		ClientId:           g.str("clientId"),
		RedirectURI:        g.str("redirectURI"),
		State:              g.str("state"),
		UserId:             g.str("userId"),
		IdentityId:         g.str("identityId"),
		MagicLinkRequestId: g.str("magicLinkRequestId"),
		ExpiresAt:          g.time("expiresAt"),
		ConsumedAt:         g.time("consumedAt"),
		CreatedAt:          g.time("createdAt"),
	}, nil
}

// ---------------------------------------------------------------------------
// Users + identities
// ---------------------------------------------------------------------------

// LookupUserById returns the v1:identity:user row matching the given
// node id (the token's `sub` claim), or nil when none exists. Used by
// the /oauth/token + /auth/refresh handlers to resolve directory
// fields onto the freshly minted access token.
func (s *Store) LookupUserById(ctx context.Context, userId string) (*UserRow, error) {
	if strings.TrimSpace(userId) == "" {
		return nil, nil
	}
	query := fmt.Sprintf(`queryUserById({userId: "%s"})`, escapeMemQLString(userId))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup user by id: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil {
		return nil, nil
	}
	return userRowFromNode(nodes[0]), nil
}

// LookupUserByEmail returns the user with the given primary email,
// or nil if no match exists.
func (s *Store) LookupUserByEmail(ctx context.Context, email string) (*UserRow, error) {
	query := fmt.Sprintf(`queryUserByEmail({primaryEmail: "%s"})`, escapeMemQLString(email))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup user by email: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil {
		return nil, nil
	}
	return userRowFromNode(nodes[0]), nil
}

// userRowFromNode projects a memQL node onto UserRow. Shared by every
// LookupUserBy* path so they all surface the same fields.
func userRowFromNode(node *memqlv1.MemoryNode) *UserRow {
	if node == nil {
		return nil
	}
	g := newFieldGetter(node)
	return &UserRow{
		ID:           firstNonEmpty(g.str("id"), node.GetId()),
		DisplayName:  g.str("displayName"),
		FirstName:    g.str("firstName"),
		LastName:     g.str("lastName"),
		PrimaryEmail: g.str("primaryEmail"),
		Phone:        g.str("phone"),
		PrimaryRole:  g.str("primaryRole"),
		Gender:       g.str("gender"),
		Birthdate:    g.str("birthdate"),
		Role:         g.str("role"),
		Active:       g.boolField("active"),
		Internal:     g.boolField("internal"),
		CreatedAt:    g.time("createdAt"),
	}
}

// UserProfileSeed is the optional bundle of directory-style fields the
// caller can stamp at user creation time. The bootstrap path (the
// /setup wizard's owner-mint) reads these from clusterSettings; the
// non-bootstrap path leaves every field empty so the admin or the
// user fills them in later from /admin/users/detail or /profile.
type UserProfileSeed struct {
	FirstName   string
	LastName    string
	Phone       string
	PrimaryRole string
	Gender      string
	Birthdate   string
}

// CreateUserOnFirstLogin creates a v1:identity:user row at first
// magic-link consumption. `seed` lets the caller stamp directory
// fields the wizard captured (firstName, lastName, phone, etc.); pass
// the zero value when none are known.
func (s *Store) CreateUserOnFirstLogin(
	ctx context.Context,
	userId, displayName, email, role string,
	internal bool,
	seed UserProfileSeed,
) error {
	if role == "" {
		role = "reader"
	}
	var b strings.Builder
	b.WriteString(`mutationCreateUserOnFirstLogin({`)
	writeKVString(&b, "userId", userId, true)
	writeKVString(&b, "displayName", displayName, false)
	writeKVString(&b, "firstName", seed.FirstName, false)
	writeKVString(&b, "lastName", seed.LastName, false)
	writeKVString(&b, "primaryEmail", email, false)
	writeKVString(&b, "phone", seed.Phone, false)
	writeKVString(&b, "primaryRole", seed.PrimaryRole, false)
	writeKVString(&b, "gender", seed.Gender, false)
	writeKVString(&b, "birthdate", seed.Birthdate, false)
	writeKVString(&b, "role", role, false)
	if internal {
		b.WriteString(`,"internal":true`)
	} else {
		b.WriteString(`,"internal":false`)
	}
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create user on first login: %w", err)
	}
	return nil
}

// CreateIdentityMagicLink creates a v1:identity:identity row with the
// magic_link variant. Stamps verifiedAt + lastLinkSentAt to now.
func (s *Store) CreateIdentityMagicLink(ctx context.Context, identityId, userId, label string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var b strings.Builder
	b.WriteString(`mutationCreateIdentity({`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "identityType", "magic_link", false)
	// credentials is an object literal, not a string. Inline JSON.
	fmt.Fprintf(&b, `,"credentials":{"verifiedAt":"%s","lastLinkSentAt":"%s"}`,
		escapeMemQLString(now), escapeMemQLString(now))
	writeKVString(&b, "label", label, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create magic_link identity: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Auth sessions
// ---------------------------------------------------------------------------

// CreateAuthSession persists a freshly minted session row at token
// issuance time. Always passes optional fields (empty default) for
// the same JSON-schema-validation reason as CreateMagicLinkRequest.
func (s *Store) CreateAuthSession(
	ctx context.Context,
	sessionId, subject, tokenHash, source, userId, identityId,
	clientLabel, expiresAt string,
) error {
	var b strings.Builder
	b.WriteString(`mutationCreateAuthSession({`)
	writeKVString(&b, "sessionId", sessionId, true)
	writeKVString(&b, "subject", subject, false)
	writeKVString(&b, "tokenHash", tokenHash, false)
	writeKVString(&b, "source", source, false)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "identityId", identityId, false)
	writeKVString(&b, "clientLabel", clientLabel, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create auth session: %w", err)
	}
	return nil
}

// LookupAuthSessionByTokenHash returns the session row matching the
// given access-token hash, or nil if none exists. Used by the auth
// middleware on every authenticated request.
func (s *Store) LookupAuthSessionByTokenHash(ctx context.Context, tokenHash string) (*AuthSessionRow, error) {
	query := fmt.Sprintf(`queryAuthSessionByTokenHash({tokenHash: "%s"})`, escapeMemQLString(tokenHash))
	return s.lookupAuthSession(ctx, query)
}

// LookupAuthSessionByRefreshTokenHash returns the session row whose
// refreshTokenHash matches, or nil. Used by the /auth/refresh handler.
func (s *Store) LookupAuthSessionByRefreshTokenHash(ctx context.Context, refreshTokenHash string) (*AuthSessionRow, error) {
	query := fmt.Sprintf(`queryAuthSessionByRefreshTokenHash({refreshTokenHash: "%s"})`, escapeMemQLString(refreshTokenHash))
	return s.lookupAuthSession(ctx, query)
}

// LookupAuthSessionByPreviousRefreshTokenHash returns the session row
// whose previousRefreshTokenHash matches, or nil. Used by the
// /auth/refresh handler's grace-window fallback when a presented hash
// doesn't match the current refreshTokenHash -- handles the
// "client hard-refreshed mid-rotation" race where the server
// completed the rotation but the browser never received the new
// cookie.
func (s *Store) LookupAuthSessionByPreviousRefreshTokenHash(ctx context.Context, previousRefreshTokenHash string) (*AuthSessionRow, error) {
	query := fmt.Sprintf(`queryAuthSessionByPreviousRefreshTokenHash({previousRefreshTokenHash: "%s"})`, escapeMemQLString(previousRefreshTokenHash))
	return s.lookupAuthSession(ctx, query)
}

func (s *Store) lookupAuthSession(ctx context.Context, query string) (*AuthSessionRow, error) {
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup auth session: %w", err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	node := nodes[0]
	if node == nil {
		return nil, nil
	}
	g := newFieldGetter(node)
	return &AuthSessionRow{
		ID:                       firstNonEmpty(g.str("id"), node.GetId()),
		UserId:                   g.str("userId"),
		IdentityId:               g.str("identityId"),
		Subject:                  g.str("subject"),
		TokenHash:                g.str("tokenHash"),
		Source:                   g.str("source"),
		ClientLabel:              g.str("clientLabel"),
		ExpiresAt:                g.time("expiresAt"),
		LastActivityAt:           g.time("lastActivityAt"),
		LastRefreshedAt:          g.time("lastRefreshedAt"),
		FirstAuthAt:              g.time("firstAuthenticatedAt"),
		RefreshTokenHash:         g.str("refreshTokenHash"),
		PreviousRefreshTokenHash: g.str("previousRefreshTokenHash"),
		PreviousRotatedAt:        g.time("previousRotatedAt"),
		RevokedAt:                g.time("revokedAt"),
		RevokedReason:            g.str("revokedReason"),
		CreatedAt:                g.time("createdAt"),
	}, nil
}

// RotateAuthSession bumps the refresh-token bookkeeping after a
// successful /auth/refresh call. previousRefreshTokenHash is the OLD
// hash being rotated FROM (not the new one) -- it's stored on the
// row so the rotator can accept its presentation again within a short
// grace window, covering the "client aborted mid-response" case.
func (s *Store) RotateAuthSession(ctx context.Context, sessionId, newRefreshTokenHash, previousRefreshTokenHash, newExpiresAt string) error {
	var b strings.Builder
	b.WriteString(`mutationRotateAuthSession({`)
	writeKVString(&b, "sessionId", sessionId, true)
	writeKVString(&b, "newRefreshTokenHash", newRefreshTokenHash, false)
	writeKVString(&b, "previousRefreshTokenHash", previousRefreshTokenHash, false)
	writeKVString(&b, "newExpiresAt", newExpiresAt, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: rotate auth session: %w", err)
	}
	return nil
}

// RevokeAuthSession stamps a session row as revoked. Preserves the
// discriminator fields the projection requires; callers must pass the
// session's existing subject / tokenHash / source / expiresAt.
func (s *Store) RevokeAuthSession(
	ctx context.Context,
	sessionId, subject, tokenHash, source, expiresAt, reason, userId string,
) error {
	var b strings.Builder
	b.WriteString(`mutationRevokeAuthSession({`)
	writeKVString(&b, "sessionId", sessionId, true)
	writeKVString(&b, "subject", subject, false)
	writeKVString(&b, "tokenHash", tokenHash, false)
	writeKVString(&b, "source", source, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "revokedReason", reason, false)
	writeKVString(&b, "userId", userId, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: revoke auth session: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// CountActiveUsers returns the count of active v1:identity:user rows.
// Used by admin counts. The bootstrap gate is IsClusterBootstrapped,
// which keys on clusterSettings.bootstrappedAt rather than user rows
// — those can be created out-of-band (e.g. via direct mutation) and
// shouldn't unilaterally satisfy "the wizard has run."
//
// MemQL doesn't have an aggregate count(); the query returns the
// full row set and the count is a Go-side len. Cheap on a small
// user base.
func (s *Store) CountActiveUsers(ctx context.Context) (int, error) {
	nodes, err := s.executeAndExtract(ctx, `queryActiveUsers({})`)
	if err != nil {
		return 0, fmt.Errorf("identity.store: count active users: %w", err)
	}
	return len(nodes), nil
}

// IsClusterBootstrapped returns true once the first-run wizard has
// completed (the singleton v1:identity:clusterSettings row exists
// AND its bootstrappedAt field is non-empty). The wizard stamps
// bootstrappedAt deliberately on completion; nothing else writes it.
//
// This is the gate for /login + /auth/magic-link. Pre-bootstrap, the
// only way to mint the first real user is the wizard at /setup,
// which sends a magic link to the operator-supplied owner email.
// Random sign-in traffic is rejected until then so a stranger can't
// claim cluster ownership before the operator runs setup.
//
// On error (engine down, query failure), returns false — fail-closed.
// The wizard remains accessible so the operator can recover.
func (s *Store) IsClusterBootstrapped(ctx context.Context) bool {
	nodes, err := s.executeAndExtract(ctx, `queryClusterSettingsCurrent({})`)
	if err != nil || len(nodes) == 0 || nodes[0] == nil || nodes[0].Payload == nil {
		return false
	}
	fields := nodes[0].Payload.GetFields()
	if fields == nil {
		return false
	}
	v, ok := fields["bootstrappedAt"]
	if !ok || v == nil {
		return false
	}
	return strings.TrimSpace(v.GetStringValue()) != ""
}

// Internal helpers
// ---------------------------------------------------------------------------

// executeAndExtract runs the query and pulls out a memory-node-shaped
// result list. Two output paths to handle:
//
//  1. Raw queries (`concept==X; ...` with no shape wrapper) -> Bundle
//     populated with []*memqlv1.MemoryNode. Pass-through.
//  2. Shape-wrapped queries (`shape(...)` -> userFull / agentFull /
//     etc.) -> Data populated with []*structpb.Value, each a flat
//     struct of the projected fields. The store's downstream
//     fieldGetter only knows how to read MemoryNode.Payload.Fields,
//     so synthesize MemoryNodes from each Data entry: copy the flat
//     struct into Payload.Fields verbatim, lift "id" / "createdBy" /
//     "createdAt" out as top-level node fields. The shape templates
//     for identity-side queries (userFull, etc.) project flat fields
//     -- "primaryEmail", "displayName", etc. -- not nested
//     `payload.*`, so a 1:1 copy works without unwrapping.
//
// Without (2), every shape-wrapped identity query (queryUserById,
// queryUserByEmail, queryActiveUsers, queryClusterSettingsCurrent...)
// silently returns empty -- the JWT path's LookupUserById was hitting
// exactly this, leaving Email/Role/Name unset on freshly minted
// access tokens (which then broke the GA-auto-join chain because
// hash(actor) diverged from hash(email)).
func (s *Store) executeAndExtract(ctx context.Context, query string) ([]*memqlv1.MemoryNode, error) {
	res, err := s.Engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		return res.Bundle.Nodes, nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return nil, fmt.Errorf("identity.store: extract shape result: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	out := make([]*memqlv1.MemoryNode, 0, len(data))
	for _, v := range data {
		if v == nil {
			continue
		}
		sv := v.GetStructValue()
		if sv == nil {
			continue
		}
		node := &memqlv1.MemoryNode{
			Payload: &structpb.Struct{Fields: sv.Fields},
		}
		if id, ok := sv.Fields["id"]; ok {
			node.Id = id.GetStringValue()
		}
		if cb, ok := sv.Fields["createdBy"]; ok {
			node.CreatedBy = cb.GetStringValue()
		}
		out = append(out, node)
	}
	return out, nil
}

// fieldGetter wraps a MemoryNode with typed accessors. Mirrors the
// pattern in component/grpc/auth_session_handlers.go to keep the
// shape-projection parsing identical across the codebase.
type fieldGetter struct {
	node *memqlv1.MemoryNode
}

func newFieldGetter(n *memqlv1.MemoryNode) *fieldGetter { return &fieldGetter{node: n} }

func (g *fieldGetter) str(key string) string {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return ""
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return ""
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(v.GetStringValue())
}

// intField extracts a numeric field. structpb's NumberValue is the
// only numeric carrier (memql encodes ints + floats both as
// float64), so we round-trip through float64. Missing / non-numeric
// fields return 0.
func (g *fieldGetter) intField(key string) int {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return 0
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return 0
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return 0
	}
	return int(v.GetNumberValue())
}

func (g *fieldGetter) boolField(key string) bool {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return false
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return false
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return false
	}
	return v.GetBoolValue()
}

func (g *fieldGetter) time(key string) time.Time {
	s := g.str(key)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// NewRandomId returns a 128-bit random hex id with the given prefix.
// Exported so the magiclink and refresh subpackages can mint ids
// without re-implementing the same loop.
func NewRandomId(prefix string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}

// NewRequestId is the convenience id minter used by the web/wizard
// path. Falls back to a deterministic-but-unique string if rand.Read
// somehow fails (which would itself be a fatal-class error in
// production but should not crash the wizard).
func NewRequestId() string {
	id, err := NewRandomId("ar-")
	if err != nil {
		return fmt.Sprintf("ar-fallback-%d", len([]byte("fallback")))
	}
	return id
}

// ---------------------------------------------------------------------------
// Cluster settings (Phase 3 wizard / Phase 6 admin UI)
// ---------------------------------------------------------------------------

// ClusterSettingsRow is the shape persisted to v1:identity:clusterSettings
// (id="cluster"). Empty strings are written through; the consumer side
// is expected to treat empty as "fall back to env-var default".
type ClusterSettingsRow struct {
	// ClusterDomain is the deployment-wide hostname suffix the
	// operator entered in the /setup wizard (or set via
	// IDENTITY_BOOTSTRAP_DOMAIN). Examples: local.znas.io,
	// staging.acme.com, acme.com. Every public service URL the
	// cluster builds derives from it (app.<domain>, identity.
	// <domain>, etc.). Required at bootstrap time.
	ClusterDomain             string
	BrandName                 string
	BrandPrimaryColor         string
	BrandLogoDataURI          string
	BrandIconDataURI          string
	RegistrationMode          string
	RegistrationDomains       string
	InternalDomains           string
	InternalDefaultRole       string
	RegisteredClientsJSON     string
	AccessRequestNotifyEmails string
	BootstrapEmail            string
	// Bootstrap* are the wizard-captured owner profile fields. Read
	// once by the magic-link verifier when the owner's user row is
	// minted (UserProfileSeed) and never used again. Stored on the
	// clusterSettings row rather than a transient bag because the
	// wizard runs before any user row exists -- there's nowhere else
	// for them to live across the magic-link round-trip.
	BootstrapFirstName   string
	BootstrapLastName    string
	BootstrapPhone       string
	BootstrapPrimaryRole string
	BootstrapGender      string
	BootstrapBirthdate   string
	// BootstrappedAt: empty means "wizard ran, magic-link issued, but
	// owner hasn't claimed yet"; non-empty (RFC3339 timestamp) means
	// "claimed". The wizard writes empty; the magic-link verifier
	// stamps it via StampClusterBootstrapped on bootstrap-link
	// consumption. /setup remains accessible until this is non-empty
	// so an unclicked first-run wizard doesn't lock the operator out.
	BootstrappedAt string

	// AccessTokenTTLSeconds is the runtime-tunable lifetime for
	// issued access tokens, in seconds. 0 means "fall back to the
	// IDENTITY_ACCESS_TOKEN_TTL_SECONDS env / built-in default".
	// Read by /oauth/token + /auth/refresh on every issuance via
	// LiveTokenSettings; admin form bounds it to [60, 86400].
	AccessTokenTTLSeconds int
	// RefreshTokenTTLSeconds is the absolute lifetime of refresh
	// tokens, in seconds. 0 = fall through to env / default.
	// Bounds: [86400, 31536000] (1 day -- 1 year).
	RefreshTokenTTLSeconds int
	// MagicLinkTTLSeconds is how long an issued magic link stays
	// valid, in seconds. 0 = fall through. Bounds: [60, 3600].
	MagicLinkTTLSeconds int
	// InvitationTTLDays is how long an admin-issued invitation token
	// is valid, in days. 0 = fall through. Bounds: [1, 90].
	InvitationTTLDays int
	// RefreshCookieSameSite is the SameSite policy for the refresh
	// cookie. Empty = inherit from IDENTITY_REFRESH_COOKIE_SAMESITE
	// env (which itself defaults to "lax"). Valid values: "lax",
	// "none". The cookie is always Secure when BaseURL is HTTPS.
	RefreshCookieSameSite string
}

// PersistClusterSettings writes (or refreshes) the singleton
// v1:identity:clusterSettings row. Because the MemQL parser doesn't
// have an upsert primitive, the wizard handler treats this as
// fire-and-forget — second submission inserts a new row, the engine's
// time-series semantics keep the latest version effective.
func (s *Store) PersistClusterSettings(ctx context.Context, in ClusterSettingsRow) error {
	var b strings.Builder
	b.WriteString(`mutationCreateClusterSettings({`)
	writeKVString(&b, "id", "cluster", true)
	writeKVString(&b, "clusterDomain", in.ClusterDomain, false)
	writeKVString(&b, "brandName", in.BrandName, false)
	writeKVString(&b, "brandPrimaryColor", in.BrandPrimaryColor, false)
	writeKVString(&b, "brandLogoDataURI", in.BrandLogoDataURI, false)
	writeKVString(&b, "brandIconDataURI", in.BrandIconDataURI, false)
	writeKVString(&b, "registrationMode", emptyToOpen(in.RegistrationMode), false)
	writeKVString(&b, "registrationDomains", in.RegistrationDomains, false)
	writeKVString(&b, "internalDomains", in.InternalDomains, false)
	writeKVString(&b, "internalDefaultRole", emptyToWriter(in.InternalDefaultRole), false)
	writeKVString(&b, "registeredClientsJSON", in.RegisteredClientsJSON, false)
	writeKVString(&b, "accessRequestNotifyEmails", in.AccessRequestNotifyEmails, false)
	writeKVString(&b, "bootstrapEmail", in.BootstrapEmail, false)
	writeKVString(&b, "bootstrapFirstName", in.BootstrapFirstName, false)
	writeKVString(&b, "bootstrapLastName", in.BootstrapLastName, false)
	writeKVString(&b, "bootstrapPhone", in.BootstrapPhone, false)
	writeKVString(&b, "bootstrapPrimaryRole", in.BootstrapPrimaryRole, false)
	writeKVString(&b, "bootstrapGender", in.BootstrapGender, false)
	writeKVString(&b, "bootstrapBirthdate", in.BootstrapBirthdate, false)
	writeKVString(&b, "bootstrappedAt", in.BootstrappedAt, false)
	writeKVInt(&b, "accessTokenTTLSeconds", in.AccessTokenTTLSeconds, false)
	writeKVInt(&b, "refreshTokenTTLSeconds", in.RefreshTokenTTLSeconds, false)
	writeKVInt(&b, "magicLinkTTLSeconds", in.MagicLinkTTLSeconds, false)
	writeKVInt(&b, "invitationTTLDays", in.InvitationTTLDays, false)
	writeKVString(&b, "refreshCookieSameSite", in.RefreshCookieSameSite, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: persist cluster settings: %w", err)
	}
	return nil
}

// ReadClusterSettings returns the latest singleton row, or nil when
// the wizard hasn't been run. Used by the admin-settings save path
// to read existing fields it needs to preserve (BootstrappedAt) and
// by the magic-link verifier to copy fields forward when stamping
// bootstrappedAt on bootstrap-link consumption.
func (s *Store) ReadClusterSettings(ctx context.Context) (*ClusterSettingsRow, error) {
	nodes, err := s.executeAndExtract(ctx, `queryClusterSettingsCurrent({})`)
	if err != nil {
		return nil, fmt.Errorf("identity.store: read cluster settings: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil || nodes[0].Payload == nil {
		return nil, nil
	}
	g := newFieldGetter(nodes[0])
	return &ClusterSettingsRow{
		ClusterDomain:             g.str("clusterDomain"),
		BrandName:                 g.str("brandName"),
		BrandPrimaryColor:         g.str("brandPrimaryColor"),
		BrandLogoDataURI:          g.str("brandLogoDataURI"),
		BrandIconDataURI:          g.str("brandIconDataURI"),
		RegistrationMode:          g.str("registrationMode"),
		RegistrationDomains:       g.str("registrationDomains"),
		InternalDomains:           g.str("internalDomains"),
		InternalDefaultRole:       g.str("internalDefaultRole"),
		RegisteredClientsJSON:     g.str("registeredClientsJSON"),
		AccessRequestNotifyEmails: g.str("accessRequestNotifyEmails"),
		BootstrapEmail:            g.str("bootstrapEmail"),
		BootstrapFirstName:        g.str("bootstrapFirstName"),
		BootstrapLastName:         g.str("bootstrapLastName"),
		BootstrapPhone:            g.str("bootstrapPhone"),
		BootstrapPrimaryRole:      g.str("bootstrapPrimaryRole"),
		BootstrapGender:           g.str("bootstrapGender"),
		BootstrapBirthdate:        g.str("bootstrapBirthdate"),
		BootstrappedAt:            g.str("bootstrappedAt"),
		AccessTokenTTLSeconds:     g.intField("accessTokenTTLSeconds"),
		RefreshTokenTTLSeconds:    g.intField("refreshTokenTTLSeconds"),
		MagicLinkTTLSeconds:       g.intField("magicLinkTTLSeconds"),
		InvitationTTLDays:         g.intField("invitationTTLDays"),
		RefreshCookieSameSite:     g.str("refreshCookieSameSite"),
	}, nil
}

// StampClusterBootstrapped marks the cluster as fully bootstrapped
// by stamping `bootstrappedAt` on a fresh row that copies every
// other field from the latest existing row. Called by the magic-link
// verifier when an operator clicks the wizard-issued bootstrap link.
//
// Pre-claim, /setup remains accessible (bootstrappedAt empty); once
// this stamps, /setup 404s and the cluster is considered live.
func (s *Store) StampClusterBootstrapped(ctx context.Context) error {
	row, err := s.ReadClusterSettings(ctx)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("identity.store: cannot stamp bootstrapped — no clusterSettings row exists")
	}
	row.BootstrappedAt = time.Now().UTC().Format(time.RFC3339Nano)
	var b strings.Builder
	b.WriteString(`mutationUpdateClusterSettings({`)
	writeKVString(&b, "id", "cluster", true)
	writeKVString(&b, "clusterDomain", row.ClusterDomain, false)
	writeKVString(&b, "brandName", row.BrandName, false)
	writeKVString(&b, "brandPrimaryColor", row.BrandPrimaryColor, false)
	writeKVString(&b, "brandLogoDataURI", row.BrandLogoDataURI, false)
	writeKVString(&b, "brandIconDataURI", row.BrandIconDataURI, false)
	writeKVString(&b, "registrationMode", emptyToOpen(row.RegistrationMode), false)
	writeKVString(&b, "registrationDomains", row.RegistrationDomains, false)
	writeKVString(&b, "internalDomains", row.InternalDomains, false)
	writeKVString(&b, "internalDefaultRole", emptyToWriter(row.InternalDefaultRole), false)
	writeKVString(&b, "registeredClientsJSON", row.RegisteredClientsJSON, false)
	writeKVString(&b, "accessRequestNotifyEmails", row.AccessRequestNotifyEmails, false)
	writeKVString(&b, "bootstrapEmail", row.BootstrapEmail, false)
	writeKVString(&b, "bootstrapFirstName", row.BootstrapFirstName, false)
	writeKVString(&b, "bootstrapLastName", row.BootstrapLastName, false)
	writeKVString(&b, "bootstrapPhone", row.BootstrapPhone, false)
	writeKVString(&b, "bootstrapPrimaryRole", row.BootstrapPrimaryRole, false)
	writeKVString(&b, "bootstrapGender", row.BootstrapGender, false)
	writeKVString(&b, "bootstrapBirthdate", row.BootstrapBirthdate, false)
	writeKVString(&b, "bootstrappedAt", row.BootstrappedAt, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: stamp bootstrapped: %w", err)
	}
	return nil
}

func emptyToOpen(s string) string {
	if s == "" {
		return "open"
	}
	return s
}
func emptyToWriter(s string) string {
	if s == "" {
		return "writer"
	}
	return s
}
