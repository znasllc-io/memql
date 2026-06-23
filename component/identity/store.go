package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
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
	ID                  string
	CodeHash            string
	ClientId            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	UserId              string
	IdentityId          string
	MagicLinkRequestId  string
	ExpiresAt           time.Time
	ConsumedAt          time.Time // zero = not consumed
	CreatedAt           time.Time
}

// OAuthClientRow projects a v1:identity:oauthClient row -- a public
// client minted at dynamic client registration (RFC 7591). No secret
// is issued for public clients, so there is no secret field.
type OAuthClientRow struct {
	ClientId                string
	ClientName              string
	RedirectURIs            []string
	GrantTypes              string
	ResponseTypes           string
	TokenEndpointAuthMethod string
	CreatedAt               time.Time
}

// UserRow projects a v1:identity:user row.
type UserRow struct {
	ID              string
	DisplayName     string
	FirstName       string
	LastName        string
	PrimaryEmail    string
	Phone           string
	PrimaryRole     string
	Gender          string
	Birthdate       string
	Role            string
	Active          bool
	Internal        bool
	RevocationEpoch int64
	CreatedAt       time.Time
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
	b.WriteString(`createMagicLinkRequest({`)
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
	b.WriteString(`consumeMagicLinkRequest({`)
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
	query := fmt.Sprintf(`magicLinkRequestByTokenHash({tokenHash: "%s"})`, escapeMemQLString(tokenHash))
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
	b.WriteString(`createAccessRequest({`)
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
	codeChallenge, codeChallengeMethod,
	userId, identityId, magicLinkRequestId, expiresAt string,
) error {
	var b strings.Builder
	b.WriteString(`createAuthCode({`)
	writeKVString(&b, "codeId", codeId, true)
	writeKVString(&b, "code", code, false)
	writeKVString(&b, "codeHash", codeHash, false)
	writeKVString(&b, "clientId", clientId, false)
	writeKVString(&b, "redirectURI", redirectURI, false)
	writeKVString(&b, "state", state, false)
	writeKVString(&b, "codeChallenge", codeChallenge, false)
	writeKVString(&b, "codeChallengeMethod", codeChallengeMethod, false)
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
	b.WriteString(`consumeAuthCode({`)
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
	query := fmt.Sprintf(`authCodeByCodeHash({codeHash: "%s"})`, escapeMemQLString(codeHash))
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
		ID:                  firstNonEmpty(g.str("id"), node.GetId()),
		CodeHash:            g.str("codeHash"),
		ClientId:            g.str("clientId"),
		RedirectURI:         g.str("redirectURI"),
		State:               g.str("state"),
		CodeChallenge:       g.str("codeChallenge"),
		CodeChallengeMethod: g.str("codeChallengeMethod"),
		UserId:              g.str("userId"),
		IdentityId:          g.str("identityId"),
		MagicLinkRequestId:  g.str("magicLinkRequestId"),
		ExpiresAt:           g.time("expiresAt"),
		ConsumedAt:          g.time("consumedAt"),
		CreatedAt:           g.time("createdAt"),
	}, nil
}

// ---------------------------------------------------------------------------
// OAuth clients (dynamic client registration, RFC 7591)
// ---------------------------------------------------------------------------

// CreateOAuthClient persists a dynamically-registered public OAuth
// client. redirectURIs is marshaled to a JSON array string (the
// concept stores it as a string column, mirroring
// clusterSettings.registeredClientsJSON). createdAt is an RFC3339
// timestamp the caller stamps.
func (s *Store) CreateOAuthClient(
	ctx context.Context,
	clientId, clientName string,
	redirectURIs []string,
	grantTypes, responseTypes, tokenEndpointAuthMethod, registeredAt string,
) error {
	if redirectURIs == nil {
		redirectURIs = []string{}
	}
	uriJSON, err := json.Marshal(redirectURIs)
	if err != nil {
		return fmt.Errorf("identity.store: marshal redirect_uris: %w", err)
	}
	var b strings.Builder
	b.WriteString(`createOAuthClient({`)
	writeKVString(&b, "clientId", clientId, true)
	writeKVString(&b, "clientName", clientName, false)
	writeKVString(&b, "redirectURIsJSON", string(uriJSON), false)
	writeKVString(&b, "grantTypes", grantTypes, false)
	writeKVString(&b, "responseTypes", responseTypes, false)
	writeKVString(&b, "tokenEndpointAuthMethod", tokenEndpointAuthMethod, false)
	writeKVString(&b, "registeredAt", registeredAt, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create oauth client: %w", err)
	}
	return nil
}

// LookupOAuthClientByClientId returns the dynamically-registered OAuth
// client row matching clientId, or nil if none exists. The stored
// redirectURIsJSON array is unmarshaled back into RedirectURIs.
func (s *Store) LookupOAuthClientByClientId(ctx context.Context, clientId string) (*OAuthClientRow, error) {
	query := fmt.Sprintf(`oAuthClientByClientId({clientId: "%s"})`, escapeMemQLString(clientId))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup oauth client: %w", err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	node := nodes[0]
	if node == nil {
		return nil, nil
	}
	g := newFieldGetter(node)
	var uris []string
	if raw := g.str("redirectURIsJSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &uris); err != nil {
			return nil, fmt.Errorf("identity.store: unmarshal redirect_uris for client %q: %w", clientId, err)
		}
	}
	return &OAuthClientRow{
		ClientId:                firstNonEmpty(g.str("clientId"), node.GetId()),
		ClientName:              g.str("clientName"),
		RedirectURIs:            uris,
		GrantTypes:              g.str("grantTypes"),
		ResponseTypes:           g.str("responseTypes"),
		TokenEndpointAuthMethod: g.str("tokenEndpointAuthMethod"),
		CreatedAt:               g.time("registeredAt"),
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
	query := fmt.Sprintf(`userById({userId: "%s"})`, escapeMemQLString(userId))
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
	query := fmt.Sprintf(`userByEmail({primaryEmail: "%s"})`, escapeMemQLString(email))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup user by email: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil {
		return nil, nil
	}
	return userRowFromNode(nodes[0]), nil
}

// BumpUserRevocationEpoch reads the user's current revocationEpoch,
// computes current+1, and persists it via
// bumpUserRevocationEpoch. Returns the new value. Used by
// the bulk-revoke admin path (memql#106).
//
// Note: this is a non-transactional read-then-write. Concurrent
// bumps can collide on the same target value (both observe N, both
// write N+1, one no-op for revocation purposes); the user-visible
// outcome is still "tokens minted before the first bump are
// invalidated," which is the property the feature guarantees. If
// strict monotonicity ever matters, swap the mutation for a CAS
// shape that takes (expectedCurrent, newEpoch).
func (s *Store) BumpUserRevocationEpoch(ctx context.Context, userId string) (int64, error) {
	if strings.TrimSpace(userId) == "" {
		return 0, errors.New("identity.store: BumpUserRevocationEpoch: userId required")
	}
	user, err := s.LookupUserById(ctx, userId)
	if err != nil {
		return 0, fmt.Errorf("identity.store: lookup before epoch bump: %w", err)
	}
	if user == nil {
		return 0, fmt.Errorf("identity.store: BumpUserRevocationEpoch: user %q not found", userId)
	}
	next := user.RevocationEpoch + 1
	var b strings.Builder
	b.WriteString(`bumpUserRevocationEpoch({`)
	writeKVString(&b, "userId", userId, true)
	writeKVInt(&b, "newEpoch", int(next), false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return 0, fmt.Errorf("identity.store: bump revocation epoch: %w", err)
	}
	return next, nil
}

// CurrentUserRevocationEpoch implements verifier.EpochResolver
// against the identity Store. Falls back to 0 (the steady-state
// value for never-bumped users) when the user row can't be found --
// matches the issued-token value of 0 so the comparison passes.
func (s *Store) CurrentUserRevocationEpoch(ctx context.Context, userId string) (int64, error) {
	user, err := s.LookupUserById(ctx, userId)
	if err != nil {
		return 0, err
	}
	if user == nil {
		return 0, nil
	}
	return user.RevocationEpoch, nil
}

// userRowFromNode projects a memQL node onto UserRow. Shared by every
// LookupUserBy* path so they all surface the same fields.
func userRowFromNode(node *memqlv1.MemoryNode) *UserRow {
	if node == nil {
		return nil
	}
	g := newFieldGetter(node)
	return &UserRow{
		ID:              firstNonEmpty(g.str("id"), node.GetId()),
		DisplayName:     g.str("displayName"),
		FirstName:       g.str("firstName"),
		LastName:        g.str("lastName"),
		PrimaryEmail:    g.str("primaryEmail"),
		Phone:           g.str("phone"),
		PrimaryRole:     g.str("primaryRole"),
		Gender:          g.str("gender"),
		Birthdate:       g.str("birthdate"),
		Role:            g.str("role"),
		Active:          g.boolField("active"),
		Internal:        g.boolField("internal"),
		RevocationEpoch: g.int64Field("revocationEpoch"),
		CreatedAt:       g.time("createdAt"),
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
	b.WriteString(`createUserOnFirstLogin({`)
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
	b.WriteString(`createIdentity({`)
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

// CreateIdentityVoiceAgentToken creates a v1:identity:identity row with
// the voice_agent_token variant for the Go voice-agent process.
// keyHash is the SHA-256 hex digest of an auxiliary random bearer; the
// actual auth credential handed to the voice-agent is a
// class="voice_agent" JWT signed via
// JWTIssuer.IssueVoiceAgentAccessToken (the caller signs after this
// row is persisted, since the JWT's `sub` claim is the identityId
// stamped here). expiresAt is RFC3339Nano; empty string is allowed
// for "no expiry stamped yet."
func (s *Store) CreateIdentityVoiceAgentToken(
	ctx context.Context,
	identityId, userId, instanceId, keyHash, mintedBy, expiresAt, label string,
) error {
	var b strings.Builder
	b.WriteString(`createVoiceAgentTokenIdentity({`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "instanceId", instanceId, false)
	writeKVString(&b, "keyHash", keyHash, false)
	writeKVString(&b, "mintedBy", mintedBy, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "label", label, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create voice_agent_token identity: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Node-token identity rows (memql#338 / #343)
// ---------------------------------------------------------------------------

// NodeTokenBinding is the (nodeType, nodeId) pair the bootstrap
// handler keys node_token identity rows on. Equality on this pair
// is the row's logical identity -- the actual identityId is a
// synthetic v1:identity:identity id minted at first-bootstrap and
// reused across subsequent restarts of the same (nodeType, nodeId).
type NodeTokenBinding struct {
	NodeType string
	NodeId   string
}

// NodeTokenLookup is what LookupNodeTokenIdentityByBinding returns
// when a row exists for the given (nodeType, nodeId). The bootstrap
// handler uses Active to refuse to re-mint a revoked credential
// silently; IdentityId is reused on the JWT subject so subsequent
// audit lookups correlate across restarts.
type NodeTokenLookup struct {
	IdentityId string
	Active     bool
}

// LookupNodeTokenIdentityByBinding returns the v1:identity:identity
// row for the given (nodeType, nodeId), or (nil, nil) when no row
// exists yet (a first-time bootstrap for that binding). Returns
// rows even when active=false so the bootstrap handler can detect
// "the operator revoked this row" and refuse to re-mint silently.
func (s *Store) LookupNodeTokenIdentityByBinding(
	ctx context.Context,
	b NodeTokenBinding,
) (*NodeTokenLookup, error) {
	var sb strings.Builder
	sb.WriteString(`nodeTokenIdentityByBinding({`)
	writeKVString(&sb, "nodeType", b.NodeType, true)
	writeKVString(&sb, "nodeId", b.NodeId, false)
	sb.WriteString(`})`)
	result, err := s.Engine.Execute(ctx, sb.String())
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup node_token identity: %w", err)
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, nil
	}
	node := result.Bundle.Nodes[0]
	if node == nil {
		return nil, nil
	}
	out := &NodeTokenLookup{IdentityId: strings.TrimSpace(node.GetId())}
	if node.Payload != nil {
		// `active` is a bool; the proto-struct accessor returns false
		// when the field is missing, which is the desired
		// "treat-as-revoked" fallback.
		out.Active = node.Payload.GetFields()["active"].GetBoolValue()
	}
	if out.IdentityId == "" {
		return nil, nil
	}
	return out, nil
}

// CreateNodeTokenIdentity inserts a fresh v1:identity:identity row
// for the node_token credential type. Used by both the operator
// CLI (`memql node-token mint`) and the /node/bootstrap handler's
// first-time path. keyHash is the SHA-256 hex digest of an
// auxiliary random bearer kept only for fingerprint audit; the
// actual auth credential handed to the node is a class="node" JWT
// signed via JWTIssuer.IssueNodeAccessToken.
//
// expiresAt / bootstrappedAt / bootstrappedFrom are optional; pass
// the empty string for "not set." memql#343.
func (s *Store) CreateNodeTokenIdentity(
	ctx context.Context,
	identityId, userId, nodeId, nodeType, keyHash, mintedBy,
	expiresAt, bootstrappedAt, bootstrappedFrom string,
) error {
	var b strings.Builder
	b.WriteString(`createNodeTokenIdentity({`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "nodeId", nodeId, false)
	writeKVString(&b, "nodeType", nodeType, false)
	writeKVString(&b, "keyHash", keyHash, false)
	writeKVString(&b, "mintedBy", mintedBy, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "bootstrappedAt", bootstrappedAt, false)
	writeKVString(&b, "bootstrappedFrom", bootstrappedFrom, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create node_token identity: %w", err)
	}
	return nil
}

// StampNodeTokenBootstrap updates the audit fields on an existing
// node_token identity row when the /node/bootstrap handler issues a
// fresh JWT for it. Pass through the full credentials payload --
// MutationStmt's update() semantics replace the credentials object
// whole (not deep-merge), so the caller must restate every field it
// wants preserved. memql#343.
func (s *Store) StampNodeTokenBootstrap(
	ctx context.Context,
	identityId, userId, nodeId, nodeType, keyHash, mintedBy,
	expiresAt, bootstrappedAt, bootstrappedFrom string,
) error {
	var b strings.Builder
	b.WriteString(`stampNodeTokenBootstrap({`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "nodeId", nodeId, false)
	writeKVString(&b, "nodeType", nodeType, false)
	writeKVString(&b, "keyHash", keyHash, false)
	writeKVString(&b, "mintedBy", mintedBy, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "bootstrappedAt", bootstrappedAt, false)
	writeKVString(&b, "bootstrappedFrom", bootstrappedFrom, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: stamp node_token bootstrap audit: %w", err)
	}
	return nil
}

// NodeTokenRow is the projection the admin UI consumes via the
// /admin/tokens page (memql#350). Mirrors the credentials variant
// schema -- nodeId / nodeType / mintedBy / expiresAt /
// bootstrappedAt / bootstrappedFrom / lastConnectAt -- plus the row-
// level Active flag the revocation surface flips and the createdAt
// stamp for chronological sorting.
//
// MintedBy carries the human user-id when the row was minted out-of-
// band via the operator CLI; bootstrap-path rows carry the synthetic
// `system:node-bootstrap` token (see component/identity/http/
// node_bootstrap.go::mintNodeBootstrapToken). The admin UI renders
// "(bootstrapped)" in the Minted-by column when MintedBy starts with
// "system:".
type NodeTokenRow struct {
	ID               string
	UserId           string
	NodeId           string
	NodeType         string
	KeyHash          string
	MintedBy         string
	ExpiresAt        string
	LastConnectAt    string
	BootstrappedAt   string
	BootstrappedFrom string
	Active           bool
	CreatedAt        time.Time
}

// ListNodeTokenIdentities returns every node_token identity row in
// the cluster (active + revoked) so the admin UI can show revoked
// rows explicitly rather than ghosting them. Backs the
// `/admin/tokens` Node-tokens section per memql#350.
func (s *Store) ListNodeTokenIdentities(ctx context.Context) ([]NodeTokenRow, error) {
	nodes, err := s.executeAndExtract(ctx, `nodeTokenIdentities({})`)
	if err != nil {
		return nil, fmt.Errorf("identity.store: list node_token identities: %w", err)
	}
	out := make([]NodeTokenRow, 0, len(nodes))
	for _, n := range nodes {
		row := nodeTokenRowFromNode(n)
		if row == nil {
			continue
		}
		out = append(out, *row)
	}
	return out, nil
}

// LookupNodeTokenIdentityById returns the node_token identity row by
// its canonical id, or nil when no row matches. Used by the admin
// revoke handler to load the current credentials before issuing the
// whole-replace update. Filtered to node_token rows on the DSL
// side (nodeTokenIdentityById carries the
// identityIsNodeToken predicate) -- a caller that passes a PAT
// id by mistake gets nil rather than a spurious revoke.
func (s *Store) LookupNodeTokenIdentityById(ctx context.Context, identityId string) (*NodeTokenRow, error) {
	if strings.TrimSpace(identityId) == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`nodeTokenIdentityById({identityId: "%s"})`, escapeMemQLString(identityId))
	nodes, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup node_token identity %q: %w", identityId, err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	return nodeTokenRowFromNode(nodes[0]), nil
}

// RevokeNodeTokenIdentity flips a node_token row's active flag to
// false (memql#350). update() has whole-replace semantics on nested
// objects, so we look up the live credentials first and restate
// every field on the mutation. The /node/bootstrap pre-mint gate
// (memql#343) consults active and refuses to re-mint a revoked row,
// so flipping the flag is the load-bearing revocation surface.
func (s *Store) RevokeNodeTokenIdentity(ctx context.Context, identityId string) error {
	row, err := s.LookupNodeTokenIdentityById(ctx, identityId)
	if err != nil {
		return fmt.Errorf("identity.store: revoke node_token: lookup %q: %w", identityId, err)
	}
	if row == nil {
		return fmt.Errorf("identity.store: revoke node_token: row %q not found", identityId)
	}
	var b strings.Builder
	b.WriteString(`revokeNodeTokenIdentity({`)
	writeKVString(&b, "identityId", row.ID, true)
	writeKVString(&b, "userId", row.UserId, false)
	writeKVString(&b, "nodeId", row.NodeId, false)
	writeKVString(&b, "nodeType", row.NodeType, false)
	writeKVString(&b, "keyHash", row.KeyHash, false)
	writeKVString(&b, "mintedBy", row.MintedBy, false)
	writeKVString(&b, "expiresAt", row.ExpiresAt, false)
	writeKVString(&b, "lastConnectAt", row.LastConnectAt, false)
	writeKVString(&b, "bootstrappedAt", row.BootstrappedAt, false)
	writeKVString(&b, "bootstrappedFrom", row.BootstrappedFrom, false)
	b.WriteString(`})`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: revoke node_token %q: %w", identityId, err)
	}
	return nil
}

// nodeTokenRowFromNode lifts a single MemoryNode (from
// nodeTokenIdentities / queryIdentityById's identityFull shape)
// into the admin-facing NodeTokenRow projection. The credentials
// sub-object lives under payload.credentials per the
// `@variant(discriminator="identityType")` schema; we navigate the
// structpb directly because there's no generic typed-cred-lookup
// helper today.
func nodeTokenRowFromNode(node *memqlv1.MemoryNode) *NodeTokenRow {
	if node == nil {
		return nil
	}
	g := newFieldGetter(node)
	out := &NodeTokenRow{
		ID:       firstNonEmpty(g.str("id"), node.GetId()),
		UserId:   g.str("userId"),
		Active:   g.boolField("active"),
		MintedBy: g.str("mintedBy"),
	}
	if out.ID == "" {
		return nil
	}
	if node.Payload != nil {
		if v, ok := node.Payload.GetFields()["credentials"]; ok && v != nil {
			if sv := v.GetStructValue(); sv != nil {
				creds := sv.GetFields()
				if s, ok := creds["nodeId"]; ok {
					out.NodeId = strings.TrimSpace(s.GetStringValue())
				}
				if s, ok := creds["nodeType"]; ok {
					out.NodeType = strings.TrimSpace(s.GetStringValue())
				}
				if s, ok := creds["keyHash"]; ok {
					out.KeyHash = strings.TrimSpace(s.GetStringValue())
				}
				if s, ok := creds["mintedBy"]; ok && out.MintedBy == "" {
					// Some shapes carry mintedBy on the credentials
					// sub-object rather than the row level. Prefer
					// the row-level value when set; fall through to
					// the credentials value when not.
					out.MintedBy = strings.TrimSpace(s.GetStringValue())
				}
				if s, ok := creds["expiresAt"]; ok {
					out.ExpiresAt = strings.TrimSpace(s.GetStringValue())
				}
				if s, ok := creds["lastConnectAt"]; ok {
					out.LastConnectAt = strings.TrimSpace(s.GetStringValue())
				}
				if s, ok := creds["bootstrappedAt"]; ok {
					out.BootstrappedAt = strings.TrimSpace(s.GetStringValue())
				}
				if s, ok := creds["bootstrappedFrom"]; ok {
					out.BootstrappedFrom = strings.TrimSpace(s.GetStringValue())
				}
			}
		}
	}
	out.CreatedAt = g.time("createdAt")
	return out
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
	b.WriteString(`createAuthSession({`)
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
	query := fmt.Sprintf(`authSessionByTokenHash({tokenHash: "%s"})`, escapeMemQLString(tokenHash))
	return s.lookupAuthSession(ctx, query)
}

// LookupAuthSessionByRefreshTokenHash returns the session row whose
// refreshTokenHash matches, or nil. Used by the /auth/refresh handler.
func (s *Store) LookupAuthSessionByRefreshTokenHash(ctx context.Context, refreshTokenHash string) (*AuthSessionRow, error) {
	query := fmt.Sprintf(`authSessionByRefreshTokenHash({refreshTokenHash: "%s"})`, escapeMemQLString(refreshTokenHash))
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
	query := fmt.Sprintf(`authSessionByPreviousRefreshTokenHash({previousRefreshTokenHash: "%s"})`, escapeMemQLString(previousRefreshTokenHash))
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
	b.WriteString(`rotateAuthSession({`)
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
	b.WriteString(`revokeAuthSession({`)
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
	nodes, err := s.executeAndExtract(ctx, `activeUsers({})`)
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
//
// CAUTION: this collapses "definitely not bootstrapped" and "couldn't
// determine (DB error)" into the same false. That is correct for the
// /login gate (fail-closed) but DANGEROUS for the claim-email guard,
// which must NOT treat an error as "unclaimed" or it re-spams the
// owner on every transient DB hiccup (memql#1864). Callers that need
// to distinguish the two must use IsClusterBootstrappedE.
func (s *Store) IsClusterBootstrapped(ctx context.Context) bool {
	ok, err := s.IsClusterBootstrappedE(ctx)
	if err != nil {
		return false
	}
	return ok
}

// IsClusterBootstrappedE is the error-returning form of
// IsClusterBootstrapped. It returns (true, nil) when bootstrappedAt is
// set, (false, nil) when the row is absent or the field is empty, and
// (false, err) when the underlying query fails. The claim-email guard
// (memql#1864) uses this so a transient DB error at boot is treated as
// "unknown -> do not send" rather than "unclaimed -> send".
func (s *Store) IsClusterBootstrappedE(ctx context.Context) (bool, error) {
	nodes, err := s.executeAndExtract(ctx, `clusterSettingsCurrent({})`)
	if err != nil {
		return false, fmt.Errorf("identity.store: is cluster bootstrapped: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil || nodes[0].Payload == nil {
		return false, nil
	}
	fields := nodes[0].Payload.GetFields()
	if fields == nil {
		return false, nil
	}
	v, ok := fields["bootstrappedAt"]
	if !ok || v == nil {
		return false, nil
	}
	return strings.TrimSpace(v.GetStringValue()) != "", nil
}

// HasOwnerUser reports whether at least one active user with the
// cluster-owner role exists. An existing owner is definitional proof
// the cluster was claimed (the only sanctioned path that mints an
// owner is the wizard / auto-bootstrap magic-link consume), so the
// claim email must NEVER auto-send once this is true — regardless of
// whether the bootstrappedAt stamp landed (memql#1864).
//
// Error-returning by design: a DB failure must surface as "unknown",
// not be silently swallowed into "no owner", so the caller can
// fail-safe (do not send) rather than re-spam the owner.
func (s *Store) HasOwnerUser(ctx context.Context) (bool, error) {
	nodes, err := s.executeAndExtract(ctx, `activeUsers({role: "owner"})`)
	if err != nil {
		return false, fmt.Errorf("identity.store: has owner user: %w", err)
	}
	return len(nodes) > 0, nil
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
// Without (2), every shape-wrapped identity query (userById,
// userByEmail, activeUsers, clusterSettingsCurrent...)
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

func (g *fieldGetter) int64Field(key string) int64 {
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
	return int64(v.GetNumberValue())
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

// NewRandomId returns a fresh opaque instance-row shortId with the
// given prefix. Delegates to core/id.NewShortId so every identity row
// shares the same wire format as the rest of memql (currently a
// 36-char UUIDv4). Callers in magiclink / refresh / pat /
// workerpairing / workertoken / http all pass an empty prefix; the
// parameter is preserved because the engine's id-validation path is
// strict about leading characters and a non-empty prefix is the
// documented way to namespace a class of ids without altering the
// shortId core. The error return is preserved for source
// compatibility but is always nil now -- id.NewShortId does not fail.
func NewRandomId(prefix string) (string, error) {
	return prefix + id.NewShortId(), nil
}

// NewRequestId is the convenience id minter used by the web/wizard
// path. The underlying NewRandomId never errors now (id.NewShortId
// cannot fail), so the historical fallback branch is unreachable;
// the function still returns the bare string for caller ergonomics.
func NewRequestId() string {
	idStr, _ := NewRandomId("")
	if idStr == "" {
		return fmt.Sprintf("fallback-%d", len([]byte("fallback")))
	}
	return idStr
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
	b.WriteString(`createClusterSettings({`)
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
	nodes, err := s.executeAndExtract(ctx, `clusterSettingsCurrent({})`)
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
	b.WriteString(`updateClusterSettings({`)
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
