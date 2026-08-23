package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"

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

	// DirectDB resolves the DIRECT (non-pooled) database handle used by
	// the magic-link consume gate (memql#4301). Optional: with it nil the
	// gate degrades to the pre-gate read-then-write, which is what a unit
	// test with a fake engine gets. app/integrations_identity.go wires it
	// from the same directDBGetter the cron leader and the recovery-key
	// mint use -- a transaction-mode PgBouncer would silently drop the
	// session-scoped lock, so the pooled handle is not interchangeable.
	// See magic_link_gate.go.
	DirectDB func() *sql.DB
}

// warn logs at WARN when a logger is wired, and does nothing otherwise.
func (s *Store) warn(msg string, args ...any) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn(msg, args...)
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

	// BindingHash is the SHA-256 hex digest of the memql_ml nonce handed
	// to the browser that REQUESTED the link. A caller presenting a cookie
	// that hashes to this may finish the sign-in; anybody else may only
	// approve it (memql#4301, design D2/D4). Empty on a row issued before
	// the device-bound flow, and on the API issue path when no browser is
	// on the other end -- such a row can only ever be approved.
	BindingHash string
	// ApprovedAt is set by a cross-device click on the landing page. It
	// authorizes the REQUESTING browser to finish; it is not itself a
	// sign-in and confers nothing on the device that set it.
	ApprovedAt time.Time
	// ApprovedFromIP / ApprovedUserAgent name the device that approved.
	// This is the pair that names B when B clicked A's link.
	ApprovedFromIP    string
	ApprovedUserAgent string
}

// HasBinding reports whether the row was issued with a device binding.
// A row without one cannot be finished by anybody -- only approved.
func (r *MagicLinkRow) HasBinding() bool {
	return r != nil && strings.TrimSpace(r.BindingHash) != ""
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
	// CORSOrigins is the credentialed-CORS allowance an owner/admin granted
	// this client (memql#3716). Empty for every client as registered -- the
	// allowance is a separate admin act, because /register is unauthenticated.
	// Never derived from RedirectURIs; see the concept's field description.
	CORSOrigins []string
	CreatedAt   time.Time
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

	// SharedMailbox is the "several people read this address" hint
	// (memql#4304). Drives copy, gates nothing.
	SharedMailbox bool
	// SignInPolicy is "any" (default) or "passkey_only". An empty string
	// reads as "any": rows written before the field existed carry no value,
	// and the safe reading of a missing policy is the permissive one --
	// treating absence as passkey_only would lock out every account that
	// predates the field.
	SignInPolicy string
}

// PasskeyOnly reports whether sign-in links are disabled for this account.
func (u *UserRow) PasskeyOnly() bool {
	return u != nil && strings.TrimSpace(u.SignInPolicy) == "passkey_only"
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
	sourceIP, userAgent, oauthCtxJSON, invitationId, bindingHash string,
) error {
	var b strings.Builder
	b.WriteString(`mutation createMagicLinkRequest(`)
	writeKVString(&b, "requestId", requestId, true)
	writeKVString(&b, "email", email, false)
	writeKVString(&b, "tokenHash", tokenHash, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "sourceIP", sourceIP, false)
	writeKVString(&b, "userAgent", userAgent, false)
	writeKVString(&b, "oauthCtxJSON", oauthCtxJSON, false)
	writeKVString(&b, "invitationId", invitationId, false)
	writeKVString(&b, "bindingHash", bindingHash, false)
	b.WriteString(`)`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create magic link: %w", err)
	}
	return nil
}

// Sentinel outcomes of the two guarded magic-link writes. Callers branch
// on these rather than on an error string; both are ordinary, expected
// results of a race, not failures.
var (
	// ErrMagicLinkAlreadyConsumed means another caller won the consume.
	// Exactly one caller of ConsumeMagicLinkRequest for a given row gets
	// nil; every other one gets this.
	ErrMagicLinkAlreadyConsumed = errors.New("identity.store: magic link already consumed")
	// ErrMagicLinkNotFound means the row named by requestId does not exist.
	ErrMagicLinkNotFound = errors.New("identity.store: magic link request not found")
)

// ConsumeMagicLinkRequest stamps consumedAt on a magic-link row, EXACTLY
// ONCE (memql#4301).
//
// The compare and the swap both happen inside the advisory-lock critical
// section (magic_link_gate.go): re-read the row, refuse if consumedAt is
// already set, otherwise write. The winner's write is committed before the
// lock is released, so the next holder sees it. N concurrent callers
// therefore produce exactly one nil and N-1 ErrMagicLinkAlreadyConsumed.
//
// This matters more than it used to. Before the device-bound flow the only
// finisher was one human clicking once, and the read-then-write this
// replaces documented its own race as acceptable. There are now two
// LEGITIMATE finishers of a single request -- the /check-email poller and a
// same-device click on the landing page -- so the property is load-bearing
// rather than defensive.
func (s *Store) ConsumeMagicLinkRequest(ctx context.Context, requestId, consumedFromIP string) error {
	return s.withMagicLinkGate(ctx, requestId, func(ctx context.Context) error {
		row, err := s.LookupMagicLinkById(ctx, requestId)
		if err != nil {
			return fmt.Errorf("identity.store: consume magic link: re-read: %w", err)
		}
		if row == nil {
			return ErrMagicLinkNotFound
		}
		if !row.ConsumedAt.IsZero() {
			return ErrMagicLinkAlreadyConsumed
		}
		var b strings.Builder
		b.WriteString(`mutation consumeMagicLinkRequest(`)
		writeKVString(&b, "requestId", requestId, true)
		writeKVString(&b, "consumedFromIP", consumedFromIP, false)
		b.WriteString(`)`)
		if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
			return fmt.Errorf("identity.store: consume magic link: %w", err)
		}
		return nil
	})
}

// ApproveMagicLinkRequest records a cross-device approval and reports
// whether THIS caller was the one that recorded it.
//
// The write happens only when approvedAt and consumedAt are both empty, and
// the check runs under the same lock the consume takes, so a second click on
// the same link is idempotent rather than a second approval with a different
// device's IP on it. won=false with a nil error is the ordinary "somebody
// already approved (or finished) this" outcome; the handler renders the
// already-approved message and audits the denial.
func (s *Store) ApproveMagicLinkRequest(ctx context.Context, requestId, approvedFromIP, approvedUserAgent string) (bool, error) {
	won := false
	err := s.withMagicLinkGate(ctx, requestId, func(ctx context.Context) error {
		row, err := s.LookupMagicLinkById(ctx, requestId)
		if err != nil {
			return fmt.Errorf("identity.store: approve magic link: re-read: %w", err)
		}
		if row == nil {
			return ErrMagicLinkNotFound
		}
		if !row.ConsumedAt.IsZero() || !row.ApprovedAt.IsZero() {
			return nil
		}
		var b strings.Builder
		b.WriteString(`mutation approveMagicLinkRequest(`)
		writeKVString(&b, "requestId", requestId, true)
		writeKVString(&b, "approvedFromIP", approvedFromIP, false)
		writeKVString(&b, "approvedUserAgent", approvedUserAgent, false)
		b.WriteString(`)`)
		// INTERNAL ORIGIN: approveMagicLinkRequest is @serverOnly. The human
		// confirmation it records was checked by the handler above this, and
		// the write is the identity service acting on that check.
		if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), b.String()); err != nil {
			return fmt.Errorf("identity.store: approve magic link: %w", err)
		}
		won = true
		return nil
	})
	return won, err
}

// LookupMagicLinkByTokenHash returns the row matching the given hash,
// or nil if none exists.
func (s *Store) LookupMagicLinkByTokenHash(ctx context.Context, tokenHash string) (*MagicLinkRow, error) {
	query := fmt.Sprintf(`query magicLinkRequestByTokenHash(tokenHash: "%s")`, escapeMemQLString(tokenHash))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup magic link: %w", err)
	}
	return firstMagicLinkRow(nodes), nil
}

// LookupMagicLinkById returns the row with the given node id, or nil.
//
// The by-id twin of LookupMagicLinkByTokenHash. Two callers need it and
// neither holds the token: the /auth/magic-link/status poller (which is
// handed the id by the page it sits on and proves itself with the binding
// cookie), and the guarded consume / approve writes, whose re-read inside
// the advisory lock is the compare half of the compare-and-swap.
func (s *Store) LookupMagicLinkById(ctx context.Context, requestId string) (*MagicLinkRow, error) {
	// INTERNAL ORIGIN: magicLinkRequestById is @serverOnly. The caller here
	// is the identity service acting on its own behalf -- checking the
	// presented binding cookie against the row before anything happens --
	// which is exactly the server-initiated work the annotation admits.
	query := fmt.Sprintf(`query magicLinkRequestById(requestId: "%s")`, escapeMemQLString(requestId))
	nodes, err := s.executeAndExtract(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup magic link by id: %w", err)
	}
	return firstMagicLinkRow(nodes), nil
}

// firstMagicLinkRow projects the first node of a magicLinkRequestFull
// result, or nil. One projection shared by both lookups so a field added to
// the shape cannot reach one caller and not the other.
func firstMagicLinkRow(nodes []*memqlv1.MemoryNode) *MagicLinkRow {
	if len(nodes) == 0 || nodes[0] == nil {
		return nil
	}
	node := nodes[0]
	g := newFieldGetter(node)
	return &MagicLinkRow{
		ID:                firstNonEmpty(g.str("id"), node.GetId()),
		Email:             g.str("email"),
		TokenHash:         g.str("tokenHash"),
		ExpiresAt:         g.time("expiresAt"),
		ConsumedAt:        g.time("consumedAt"),
		OAuthCtxJSON:      g.str("oauthCtxJSON"),
		InvitationId:      g.str("invitationId"),
		SourceIP:          g.str("sourceIP"),
		UserAgent:         g.str("userAgent"),
		BindingHash:       g.str("bindingHash"),
		ApprovedAt:        g.time("approvedAt"),
		ApprovedFromIP:    g.str("approvedFromIP"),
		ApprovedUserAgent: g.str("approvedUserAgent"),
		CreatedAt:         g.time("createdAt"),
	}
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
	b.WriteString(`mutation createAccessRequest(`)
	writeKVString(&b, "requestId", requestId, true)
	writeKVString(&b, "email", email, false)
	writeKVString(&b, "name", name, false)
	writeKVString(&b, "additionalContext", additionalContext, false)
	fmt.Fprintf(&b, `,riskScore: %d`, riskScore)
	writeKVString(&b, "riskSignals", riskSignals, false)
	writeKVString(&b, "sourceIP", sourceIP, false)
	writeKVString(&b, "userAgent", userAgent, false)
	b.WriteString(`)`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create access request: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Auth codes (OAuth code flow)
// ---------------------------------------------------------------------------

// CreateAuthCode mints a one-time OAuth auth code.
//
// The PLAINTEXT code is deliberately not a parameter (issue #3187). Only its
// SHA-256 digest is persisted: /oauth/token hashes the presented code and
// looks the row up by codeHash, so finding a row already proves the presenter
// holds the preimage. Storing the cleartext bought no property the digest did
// not already give, and it was the only stored-plaintext credential in the
// tree. The caller still holds the plaintext in memory to hand to the OAuth
// client on the redirect; it just never reaches the database.
func (s *Store) CreateAuthCode(
	ctx context.Context,
	codeId, codeHash, clientId, redirectURI, state,
	codeChallenge, codeChallengeMethod,
	userId, identityId, magicLinkRequestId, expiresAt string,
) error {
	var b strings.Builder
	b.WriteString(`mutation createAuthCode(`)
	writeKVString(&b, "codeId", codeId, true)
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
	b.WriteString(`)`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create auth code: %w", err)
	}
	return nil
}

// ConsumeAuthCode stamps consumedAt on an auth-code row.
func (s *Store) ConsumeAuthCode(ctx context.Context, codeId, consumedFromIP string) error {
	var b strings.Builder
	b.WriteString(`mutation consumeAuthCode(`)
	writeKVString(&b, "codeId", codeId, true)
	writeKVString(&b, "consumedFromIP", consumedFromIP, false)
	b.WriteString(`)`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: consume auth code: %w", err)
	}
	return nil
}

// LookupAuthCodeByCodeHash returns the row matching the given code
// hash, or nil if none exists.
func (s *Store) LookupAuthCodeByCodeHash(ctx context.Context, codeHash string) (*AuthCodeRow, error) {
	query := fmt.Sprintf(`query authCodeByCodeHash(codeHash: "%s")`, escapeMemQLString(codeHash))
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
	b.WriteString(`mutation createOAuthClient(`)
	writeKVString(&b, "clientId", clientId, true)
	writeKVString(&b, "clientName", clientName, false)
	writeKVString(&b, "redirectURIsJSON", string(uriJSON), false)
	writeKVString(&b, "grantTypes", grantTypes, false)
	writeKVString(&b, "responseTypes", responseTypes, false)
	writeKVString(&b, "tokenEndpointAuthMethod", tokenEndpointAuthMethod, false)
	writeKVString(&b, "registeredAt", registeredAt, false)
	b.WriteString(`)`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create oauth client: %w", err)
	}
	return nil
}

// LookupOAuthClientByClientId returns the dynamically-registered OAuth
// client row matching clientId, or nil if none exists. The stored
// redirectURIsJSON array is unmarshaled back into RedirectURIs.
func (s *Store) LookupOAuthClientByClientId(ctx context.Context, clientId string) (*OAuthClientRow, error) {
	query := fmt.Sprintf(`query oAuthClientByClientId(clientId: "%s")`, escapeMemQLString(clientId))
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
	// The dropped entries are deliberately discarded here. This row projection
	// answers "what allowance is IN EFFECT", which is what the admin surface
	// reports as the previous value on a revoke -- an entry the middleware would
	// refuse was never in effect, so naming it would overstate what was removed.
	// The read that the middleware itself drives (GrantedCORSOrigins) logs them.
	corsOrigins, _ := ParseCORSOriginsJSON(g.str("corsOriginsJSON"))
	return &OAuthClientRow{
		ClientId:                firstNonEmpty(g.str("clientId"), node.GetId()),
		ClientName:              g.str("clientName"),
		RedirectURIs:            uris,
		GrantTypes:              g.str("grantTypes"),
		ResponseTypes:           g.str("responseTypes"),
		TokenEndpointAuthMethod: g.str("tokenEndpointAuthMethod"),
		CORSOrigins:             corsOrigins,
		CreatedAt:               g.time("registeredAt"),
	}, nil
}

// SetOAuthClientCORSOrigins writes the credentialed-CORS allowance an
// owner/admin granted a registered OAuth client (memql#3716). origins is the
// COMPLETE allowance; nil or empty revokes.
//
// It writes what the caller hands it and validates nothing. That is not an
// oversight: the only caller is component/identity/adminops, which validates
// against identity.ValidateCORSOrigins so the operator gets a message naming
// the bad entry, and duplicating the check here would put the authoritative
// grammar in two places. The write itself is refused from the wire --
// setOAuthClientCORSOrigins is @serverOnly -- so this method's exposure is
// exactly its Go callers.
func (s *Store) SetOAuthClientCORSOrigins(ctx context.Context, clientId string, origins []string) error {
	encoded, err := MarshalCORSOrigins(origins)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(`mutation setOAuthClientCORSOrigins(`)
	writeKVString(&b, "clientId", clientId, true)
	writeKVString(&b, "corsOriginsJSON", encoded, false)
	b.WriteString(`)`)
	// The internal-origin stamp is load-bearing rather than decorative: the
	// mutation is @serverOnly, so without it this call is refused at runtime by
	// the function validator. Its safety argument is that every caller is
	// downstream of the owner/admin gate in adminops, in the same function.
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), b.String()); err != nil {
		return fmt.Errorf("identity.store: set oauth client cors origins: %w", err)
	}
	return nil
}

// GrantedCORSOrigins returns every origin an owner/admin has granted
// credentialed CORS access, unioned across every registered OAuth client.
//
// This is the graph half of identity's CORS allowlist, and it is read LIVE on
// the middleware's miss path rather than snapshotted at boot -- which is the
// entire point of memql#3716: a grant has to take effect without an identity
// restart, and the grant may have been executed on a different node.
//
// NOT stamped with an internal origin, unlike its neighbour
// SetOAuthClientCORSOrigins above. oAuthClientCORSGrants is an ordinary read and
// the request context already carries the synthetic actor that
// identity.SystemActorHandlerFunc stamps -- it wraps every route cors() sits
// inside, outermost -- which is the same footing LookupOAuthClientByClientId
// reads on from the same middleware chain. Stamping internal origin here would
// widen what that stamp means: component/auth's call-origin allowlist admits
// component/identity as "server-initiated", and a browser preflight is not that.
//
// Unparseable entries are dropped with a warning rather than failing the read;
// see ParseCORSOriginsJSON for why the read path is lenient where the write path
// is strict.
func (s *Store) GrantedCORSOrigins(ctx context.Context) ([]string, error) {
	nodes, err := s.executeAndExtract(ctx, `query oAuthClientCORSGrants()`)
	if err != nil {
		return nil, fmt.Errorf("identity.store: granted cors origins: %w", err)
	}
	var out []string
	seen := map[string]bool{}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		g := newFieldGetter(node)
		origins, dropped := ParseCORSOriginsJSON(g.str("corsOriginsJSON"))
		if len(dropped) > 0 && s.Logger != nil {
			s.Logger.Warn("identity.store: dropped unusable granted CORS origin(s)",
				slog.String("clientId", firstNonEmpty(g.str("clientId"), node.GetId())),
				slog.Int("dropped", len(dropped)))
		}
		for _, origin := range origins {
			if seen[origin] {
				continue
			}
			seen[origin] = true
			out = append(out, origin)
		}
	}
	return out, nil
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
	// #2800: identity-store lookup by id, server-side only.
	query := fmt.Sprintf(`query userByIdSystem(userId: %s)`, dslJSONString(userId))
	nodes, err := s.executeAndExtractInternal(ctx, query)
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

// dslJSONString returns a JSON-encoded string literal (with surrounding
// quotes) for safe interpolation into a MemQL DSL call. encoding/json's
// quoting keeps a value containing a double quote from breaking out of its
// enclosing literal -- and is the form CodeQL go/unsafe-quoting recognizes
// as safe.
func dslJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (s *Store) LookupUserByEmail(ctx context.Context, email string) (*UserRow, error) {
	// #2881: userByEmail is @serverOnly -- it projects userFull (every @pii
	// field plus the cluster-wide auth role) keyed on an email address. This is
	// the trusted server-side caller, so it stamps internal origin, exactly as
	// the by-id lookup above does for userByIdSystem.
	query := fmt.Sprintf(`query userByEmail(primaryEmail: %s)`, dslJSONString(email))
	nodes, err := s.executeAndExtractInternal(ctx, query)
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
	b.WriteString(`mutation bumpUserRevocationEpoch(`)
	writeKVString(&b, "userId", userId, true)
	writeKVInt(&b, "newEpoch", int(next), false)
	b.WriteString(`)`)
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

// userRowFromNode projects a MemQL node onto UserRow. Shared by every
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
		SharedMailbox:   g.boolField("sharedMailbox"),
		SignInPolicy:    g.str("signInPolicy"),
		CreatedAt:       g.time("createdAt"),
	}
}

// SelfSessionRow is one of the caller's own sessions, as a person sees it.
//
// NO TOKEN HASHES, matching the authSessionSelf shape. The digests are the
// auth hot path's lookup key and there is no reason for a device list to
// carry them, so they are absent from the shape AND from this struct -- two
// places rather than one, so adding a field to either does not quietly widen
// what a browser receives.
type SelfSessionRow struct {
	ID          string
	Source      string
	ClientLabel string
	FirstAuthAt time.Time
	LastSeenAt  time.Time
	LastRotated time.Time
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// SessionsForSelf returns the caller's own live sessions.
//
// THE CONTEXT'S ACTOR IS THE ARGUMENT. authSessionsForSelf takes none: it
// filters userId==actor.userId, so a caller cannot point it at another
// person, and a context with no actor yields nothing rather than everything.
// Pass a context carrying the caller (auth.ContextWithUserActor) -- passing a
// system actor here would return an empty list, not a full one, which is the
// right direction for a mistake to fail in.
func (s *Store) SessionsForSelf(ctx context.Context) ([]SelfSessionRow, error) {
	nodes, err := s.executeAndExtract(ctx, `query authSessionsForSelf()`)
	if err != nil {
		return nil, fmt.Errorf("identity.store: sessions for self: %w", err)
	}
	out := make([]SelfSessionRow, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		g := newFieldGetter(node)
		out = append(out, SelfSessionRow{
			ID:          firstNonEmpty(g.str("id"), node.GetId()),
			Source:      g.str("source"),
			ClientLabel: g.str("clientLabel"),
			FirstAuthAt: g.time("firstAuthenticatedAt"),
			LastSeenAt:  g.time("lastActivityAt"),
			LastRotated: g.time("lastRefreshedAt"),
			ExpiresAt:   g.time("expiresAt"),
			CreatedAt:   g.time("createdAt"),
		})
	}
	return out, nil
}

// SetUserSignInPolicy sets which factors may enter one account.
//
// THE PRECONDITION IS THE CALLER'S, NOT THIS FUNCTION'S. Enabling
// passkey_only requires the user to hold at least one active passkey, and
// that check lives where the caller can also explain the refusal to a
// person. What is enforced here is only that the value is one of the two the
// enum admits -- a typo would otherwise refuse the row at insert with a
// schema error nobody can act on.
func (s *Store) SetUserSignInPolicy(ctx context.Context, userId, policy string) error {
	policy = strings.TrimSpace(policy)
	if policy != "any" && policy != "passkey_only" {
		return fmt.Errorf("identity.store: sign-in policy %q is not one of any|passkey_only", policy)
	}
	var b strings.Builder
	b.WriteString(`mutation setUserSignInPolicy(`)
	writeKVString(&b, "userId", userId, true)
	writeKVString(&b, "policy", policy, false)
	b.WriteString(`)`)
	// INTERNAL ORIGIN: setUserSignInPolicy is @serverOnly -- the admin reset
	// is a legitimate caller acting on somebody else's row, which no
	// actor-scoped filter can express.
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), b.String()); err != nil {
		return fmt.Errorf("identity.store: set sign-in policy: %w", err)
	}
	return nil
}

// SetUserSharedMailbox sets or clears the shared-mailbox hint.
func (s *Store) SetUserSharedMailbox(ctx context.Context, userId string, shared bool) error {
	var b strings.Builder
	b.WriteString(`mutation setUserSharedMailbox(`)
	writeKVString(&b, "userId", userId, true)
	if shared {
		b.WriteString(`,shared: true`)
	} else {
		b.WriteString(`,shared: false`)
	}
	b.WriteString(`)`)
	// INTERNAL ORIGIN: setUserSharedMailbox is @serverOnly, same reasoning.
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), b.String()); err != nil {
		return fmt.Errorf("identity.store: set shared mailbox: %w", err)
	}
	return nil
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

	// SharedMailbox is the registration-time verdict of the local-part
	// heuristic (memql#4304). Stamped at creation so the flag is right from
	// the first sign-in rather than appearing later; the user or an admin
	// can change it afterwards, and the heuristic never runs again.
	SharedMailbox bool
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
	b.WriteString(`mutation createUserOnFirstLogin(`)
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
		b.WriteString(`,internal: true`)
	} else {
		b.WriteString(`,internal: false`)
	}
	if seed.SharedMailbox {
		b.WriteString(`,sharedMailbox: true`)
	} else {
		b.WriteString(`,sharedMailbox: false`)
	}
	b.WriteString(`)`)
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
	b.WriteString(`mutation createIdentity(`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "identityType", "magic_link", false)
	// credentials is an object literal, not a string. Inline JSON.
	fmt.Fprintf(&b, `,credentials: {"verifiedAt":"%s","lastLinkSentAt":"%s"}`,
		escapeMemQLString(now), escapeMemQLString(now))
	writeKVString(&b, "label", label, false)
	b.WriteString(`)`)
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
	b.WriteString(`mutation createVoiceAgentTokenIdentity(`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "instanceId", instanceId, false)
	writeKVString(&b, "keyHash", keyHash, false)
	writeKVString(&b, "mintedBy", mintedBy, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "label", label, false)
	b.WriteString(`)`)
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
	sb.WriteString(`query nodeTokenIdentityByBinding(`)
	writeKVString(&sb, "nodeType", b.NodeType, true)
	writeKVString(&sb, "nodeId", b.NodeId, false)
	sb.WriteString(`)`)
	// Internal origin: nodeTokenIdentityByBinding is @serverOnly as of
	// memql#2987 (identityFull projects `credentials`, and the binding is
	// guessable -- node types are published and node ids are pod names).
	// This is the bootstrap-minted identity persistence path: server-
	// initiated, no caller in scope, which is exactly why this package is
	// allowlisted in call_origin_conformance_test.go.
	result, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), sb.String())
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

// VoiceAgentTokenLookup is what LookupVoiceAgentTokenIdentityById
// returns when a row exists for the given identity id. Active is the
// whole answer the verify path needs (memql#4111).
type VoiceAgentTokenLookup struct {
	IdentityId string
	Active     bool
}

// LookupVoiceAgentTokenIdentityById returns the
// v1:identity:identity[voice_agent_token] row whose id is the
// class="voice_agent" JWT's subject, or (nil, nil) when no such row
// exists.
//
// A nil result means "unknown", NOT "revoked" -- the caller
// (component/grpc.VoiceAgentRevocationCheck) treats it as
// not-revoked, matching the node-class convention in
// component/node/node_token_revocation.go. Rows are returned even
// when active=false; that is the state the caller is asking about.
func (s *Store) LookupVoiceAgentTokenIdentityById(
	ctx context.Context,
	identityId string,
) (*VoiceAgentTokenLookup, error) {
	identityId = strings.TrimSpace(identityId)
	if identityId == "" {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString(`query voiceAgentTokenIdentityById(`)
	writeKVString(&sb, "identityId", identityId, true)
	sb.WriteString(`)`)
	// Internal origin: the query is @serverOnly, and this runs inside an
	// auth interceptor -- BEFORE any actor exists to scope to. Same
	// reasoning as LookupNodeTokenIdentityByBinding above.
	result, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), sb.String())
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup voice_agent_token identity: %w", err)
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, nil
	}
	node := result.Bundle.Nodes[0]
	if node == nil {
		return nil, nil
	}
	out := &VoiceAgentTokenLookup{IdentityId: strings.TrimSpace(node.GetId())}
	if node.Payload != nil {
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
	b.WriteString(`mutation createNodeTokenIdentity(`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "nodeId", nodeId, false)
	writeKVString(&b, "nodeType", nodeType, false)
	writeKVString(&b, "keyHash", keyHash, false)
	writeKVString(&b, "mintedBy", mintedBy, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "bootstrappedAt", bootstrappedAt, false)
	writeKVString(&b, "bootstrappedFrom", bootstrappedFrom, false)
	b.WriteString(`)`)
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
	b.WriteString(`mutation stampNodeTokenBootstrap(`)
	writeKVString(&b, "identityId", identityId, true)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "nodeId", nodeId, false)
	writeKVString(&b, "nodeType", nodeType, false)
	writeKVString(&b, "keyHash", keyHash, false)
	writeKVString(&b, "mintedBy", mintedBy, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	writeKVString(&b, "bootstrappedAt", bootstrappedAt, false)
	writeKVString(&b, "bootstrappedFrom", bootstrappedFrom, false)
	b.WriteString(`)`)
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
	// Internal: @serverOnly as of memql#2987. This query has NO filter and
	// projects `credentials`, so as @public it exposed every node credential
	// in the cluster to any authenticated caller in one unparameterised call.
	// Reached from the /admin/tokens section, whose routes are all behind
	// requireAdmin (asserted by component/identity/admin/route_gate_test.go).
	nodes, err := s.executeAndExtractInternal(ctx, `query nodeTokenIdentities()`)
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
	q := fmt.Sprintf(`query nodeTokenIdentityById(identityId: "%s")`, escapeMemQLString(identityId))
	// Internal: @serverOnly as of memql#2987 -- identityFull projects
	// `credentials`. Same admin revoke path as ListNodeTokenIdentities above.
	nodes, err := s.executeAndExtractInternal(ctx, q)
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
	b.WriteString(`mutation revokeNodeTokenIdentity(`)
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
	b.WriteString(`)`)
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
	b.WriteString(`mutation createAuthSession(`)
	writeKVString(&b, "sessionId", sessionId, true)
	writeKVString(&b, "subject", subject, false)
	writeKVString(&b, "tokenHash", tokenHash, false)
	writeKVString(&b, "source", source, false)
	writeKVString(&b, "userId", userId, false)
	writeKVString(&b, "identityId", identityId, false)
	writeKVString(&b, "clientLabel", clientLabel, false)
	writeKVString(&b, "expiresAt", expiresAt, false)
	b.WriteString(`)`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: create auth session: %w", err)
	}
	return nil
}

// LookupAuthSessionByTokenHash returns the session row matching the
// given access-token hash, or nil if none exists. Used by the auth
// middleware on every authenticated request.
func (s *Store) LookupAuthSessionByTokenHash(ctx context.Context, tokenHash string) (*AuthSessionRow, error) {
	query := fmt.Sprintf(`query authSessionByTokenHash(tokenHash: "%s")`, escapeMemQLString(tokenHash))
	return s.lookupAuthSession(ctx, query)
}

// LookupAuthSessionByRefreshTokenHash returns the session row whose
// refreshTokenHash matches, or nil. Used by the /auth/refresh handler.
func (s *Store) LookupAuthSessionByRefreshTokenHash(ctx context.Context, refreshTokenHash string) (*AuthSessionRow, error) {
	query := fmt.Sprintf(`query authSessionByRefreshTokenHash(refreshTokenHash: "%s")`, escapeMemQLString(refreshTokenHash))
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
	query := fmt.Sprintf(`query authSessionByPreviousRefreshTokenHash(previousRefreshTokenHash: "%s")`, escapeMemQLString(previousRefreshTokenHash))
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
	b.WriteString(`mutation rotateAuthSession(`)
	writeKVString(&b, "sessionId", sessionId, true)
	writeKVString(&b, "newRefreshTokenHash", newRefreshTokenHash, false)
	writeKVString(&b, "previousRefreshTokenHash", previousRefreshTokenHash, false)
	writeKVString(&b, "newExpiresAt", newExpiresAt, false)
	b.WriteString(`)`)
	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.store: rotate auth session: %w", err)
	}
	return nil
}

// RevokeAuthSession stamps a session row as revoked.
//
// Two arguments, because that is what the mutation declares. It used to take
// six more -- subject / tokenHash / source / expiresAt / userId -- on the
// theory that the projection needed them re-supplied. memql#1628 replaced that
// with a read-merge: the mutation inherits every field it is not given, and
// its declared args are `sessionId` and `revokedReason` alone, so the extras
// were silently discarded on every call (memql#4258).
func (s *Store) RevokeAuthSession(ctx context.Context, sessionId, reason string) error {
	var b strings.Builder
	b.WriteString(`mutation revokeAuthSession(`)
	writeKVString(&b, "sessionId", sessionId, true)
	writeKVString(&b, "revokedReason", reason, false)
	b.WriteString(`)`)
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
	// #2883: activeUsers is @serverOnly. This call runs during first-run setup,
	// before any actor exists -- which is exactly why the construct is gated by
	// ORIGIN rather than caller-scoped.
	nodes, err := s.executeAndExtractInternal(ctx, `query activeUsers()`)
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
	nodes, err := s.executeAndExtract(ctx, `query clusterSettingsCurrent()`)
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
// cluster-owner role exists.
//
// AN OWNER ROW IS NO LONGER PROOF OF A CLAIM (memql#3591). It was, while the only
// path that minted one was a magic-link consume. The env bootstrap now writes the
// owner row when it stamps clusterSettings -- so the cluster has a named owner a
// passkey-enrolment link can be minted for -- and such a row means nobody has
// authenticated yet. Use HasClaimedOwner for the "was this cluster claimed"
// question; this one answers only "is there an owner named", which is what the
// admin surfaces want.
//
// Error-returning by design: a DB failure must surface as "unknown",
// not be silently swallowed into "no owner", so the caller can
// fail-safe (do not send) rather than re-spam the owner.
func (s *Store) HasOwnerUser(ctx context.Context) (bool, error) {
	// #2883: activeUsers is @serverOnly. Like CountActiveUsers this answers a
	// bootstrap question that precedes any actor.
	nodes, err := s.executeAndExtractInternal(ctx, `query activeUsers(role: "owner")`)
	if err != nil {
		return false, fmt.Errorf("identity.store: has owner user: %w", err)
	}
	return len(nodes) > 0, nil
}

// OwnerUserIds returns the ids of the cluster's owner users.
//
// Backs the recovery-key mint invariant (memql#3965), which needs the ids
// themselves rather than HasOwnerUser's boolean: it binds a key to each owner.
//
// IT ASKS "IS THERE AN OWNER NAMED", NOT "HAS THE CLUSTER BEEN CLAIMED", and
// the distinction is the whole judgment. HasClaimedOwner is the stricter
// question and is the wrong one here: it reports false until somebody has
// authenticated, and an owner who has never authenticated is precisely the
// owner most in need of a break-glass route. Gating the key on a claim would
// withhold it from the one case where a cluster can be locked out from its
// very first minute.
//
// Error-returning by design, like its siblings: a DB failure must surface as
// "unknown" so the caller declines to act, rather than being swallowed into
// "no owner" and skipping the invariant silently.
func (s *Store) OwnerUserIds(ctx context.Context) ([]string, error) {
	nodes, err := s.executeAndExtractInternal(ctx, `query activeUsers(role: "owner")`)
	if err != nil {
		return nil, fmt.Errorf("identity.store: owner user ids: %w", err)
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if id := strings.TrimSpace(n.GetId()); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// HasSignInRoute reports whether a NAMED user still holds an active credential
// they could sign in with -- a magic-link identity or a passkey, and nothing
// else.
//
// THE BREAK-GLASS PREDICATE (memql#3967). Recovery-key redemption is refused
// whenever this returns true: the key exists for an owner who has lost every
// way back in, and a key redeemable while an ordinary sign-in still works is
// not a break-glass credential, it is a second password for the account.
//
// IT MUST BE THE NAMED-USER VARIANT, AND THIS IS THE TRAP. `signInIdentitiesForSelf`
// filters `userId==actor.userId`, and the redeem path runs PRE-ACTOR -- the key
// is the credential, so no AccessContext exists yet. The self variant would
// therefore return ZERO ROWS for every caller, report every account as having
// no sign-in route, and FAIL OPEN: every redemption allowed, on an account
// whose owner can sign in perfectly well. `signInIdentitiesForUser` takes the
// user id as an argument and is the variant Store.HasClaimedOwner already uses
// for exactly this reason.
//
// A RECOVERY KEY IS NOT A SIGN-IN ROUTE and must never be counted as one. The
// query filters `identityIsMagicLink || identityIsPasskey`; adding
// `identityIsRecoveryKey` to it would make an active key satisfy the gate that
// guards its own redemption, so the key would permanently refuse itself. Said
// here, on the trait, and on the concept -- three places, because every
// individual edit that would break it looks reasonable.
//
// Error-returning by design: a DB failure must surface as "unknown" so the
// caller can fail SAFE. For this predicate failing safe means REFUSING the
// redemption, not allowing it -- the opposite direction from HasClaimedOwner's
// caller, and the reason both return an error rather than a bare bool.
func (s *Store) HasSignInRoute(ctx context.Context, userId string) (bool, error) {
	userId = strings.TrimSpace(userId)
	if userId == "" {
		return false, fmt.Errorf("identity.store: has sign-in route: userId required")
	}
	creds, err := s.executeAndExtractInternal(ctx,
		fmt.Sprintf(`query signInIdentitiesForUser(userId: %s)`, dslJSONString(userId)))
	if err != nil {
		return false, fmt.Errorf("identity.store: has sign-in route: %w", err)
	}
	return len(creds) > 0, nil
}

// HasClaimedOwner reports whether the cluster's owner has ever AUTHENTICATED --
// the question memql#1864's self-heal actually wanted, and the one the
// auto-bootstrap guard asks (memql#3591).
//
// WHY IT IS NOT HasOwnerUser. The install writes the owner user row when it
// bootstraps from env, so an owner row can exist before anybody has signed in.
// Reading that as a claim would stamp bootstrappedAt on the next boot: the cluster
// would report itself claimed while no credential existed, /setup would 404 as a
// fallback, and both would happen silently.
//
// CREDENTIALS ARE THE PROOF. An owner holding an active magic-link or passkey
// identity has authenticated by one of the two routes there are; an owner holding
// neither has never signed in. `signInIdentitiesForUser` filters on
// isActiveRecord, so a revoked credential correctly does not count as a way in.
//
// Error-returning by design, like HasOwnerUser: a DB failure must surface as
// "unknown" rather than be swallowed into "not claimed", so the caller can
// fail-safe (do not send the claim email) instead of re-spamming the owner.
func (s *Store) HasClaimedOwner(ctx context.Context) (bool, error) {
	nodes, err := s.executeAndExtractInternal(ctx, `query activeUsers(role: "owner")`)
	if err != nil {
		return false, fmt.Errorf("identity.store: has claimed owner: %w", err)
	}
	for _, n := range nodes {
		userId := strings.TrimSpace(n.GetId())
		if userId == "" {
			continue
		}
		// One query per owner. A cluster has one owner in every case this runs
		// for; the loop is here so a cluster with several is answered correctly
		// rather than by looking at whichever came back first.
		// dslJSONString, NOT `"%s"` around an escaped value. CodeQL calls the
		// latter unsafe quoting and it is right to: the quotes are mine and the
		// escaping is a separate function, so the two can drift, and one
		// unescaped double quote breaks out of the literal. json.Marshal emits
		// the whole quoted literal, which cannot. Same form as userByEmail and
		// userByIdSystem, the two closest analogues.
		creds, err := s.executeAndExtractInternal(ctx,
			fmt.Sprintf(`query signInIdentitiesForUser(userId: %s)`, dslJSONString(userId)))
		if err != nil {
			return false, fmt.Errorf("identity.store: has claimed owner: sign-in identities: %w", err)
		}
		if len(creds) > 0 {
			return true, nil
		}
	}
	return false, nil
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

// executeAndExtractInternal is executeAndExtract for a @serverOnly construct
// (memql#2800). The stamp lives here rather than at each call site so the
// trusted marking is visible in one place and greppable.
//
// Keep the caller list short and deliberate: everything routed through here
// can reach constructs that are barred from the wire.
func (s *Store) executeAndExtractInternal(ctx context.Context, query string) ([]*memqlv1.MemoryNode, error) {
	return s.executeAndExtract(auth.ContextWithInternalOrigin(ctx), query)
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
	// MEMQL_IDENTITY_BOOTSTRAP_DOMAIN). Examples: memql.localhost,
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
	// MEMQL_IDENTITY_ACCESS_TOKEN_TTL_SECONDS env / built-in default".
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
	// cookie. Empty = inherit from MEMQL_IDENTITY_REFRESH_COOKIE_SAMESITE
	// env (which itself defaults to "lax"). Valid values: "lax",
	// "none". The cookie is always Secure when BaseURL is HTTPS.
	RefreshCookieSameSite string
}

// PersistClusterSettings writes (or refreshes) the singleton
// v1:identity:clusterSettings row. Because the MemQL parser doesn't
// have an upsert primitive, the wizard handler treats this as
// fire-and-forget — second submission inserts a new row, the engine's
// time-series semantics keep the latest version effective.
//
// It will not un-bootstrap the cluster (memql#3415). ClusterSettingsRow's
// zero value carries BootstrappedAt == "", and every caller here builds the
// row from scratch, so "forgot to carry the stamp forward" is the natural
// mistake at this seam — and its consequence is that /login stops working for
// everyone and /setup, the wizard that mints the cluster owner, starts
// answering 200. So the stored stamp is read first and carried forward when
// the caller supplied none, and a read failure REFUSES the write rather than
// guessing: an unprovable "this is not a blanking write" is not good enough
// for this row. The engine-side @noUnset("bootstrappedAt") annotation on the
// mutation is the same invariant enforced one layer down, for callers that
// never come through here.
func (s *Store) PersistClusterSettings(ctx context.Context, in ClusterSettingsRow) error {
	if strings.TrimSpace(in.BootstrappedAt) == "" {
		existing, err := s.ReadClusterSettings(ctx)
		if err != nil {
			return fmt.Errorf("identity.store: persist cluster settings: refusing to write without knowing the current bootstrap state: %w", err)
		}
		if existing != nil && strings.TrimSpace(existing.BootstrappedAt) != "" {
			in.BootstrappedAt = existing.BootstrappedAt
		}
	}
	var b strings.Builder
	b.WriteString(`mutation createClusterSettings(`)
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
	b.WriteString(`)`)
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
	nodes, err := s.executeAndExtract(ctx, `query clusterSettingsCurrent()`)
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
	b.WriteString(`mutation updateClusterSettings(`)
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
	b.WriteString(`)`)
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

// ---------------------------------------------------------------------------
// User invitations (memql#4270, memql#4282)
// ---------------------------------------------------------------------------

// InvitationRow projects the fields the redeem path needs from a
// v1:identity:invitation row.
type InvitationRow struct {
	ID     string
	Kind   string
	Status string
	Active bool
	// Email is the address the invitation was issued FOR. The redeem path
	// compares it against the address being registered, which is what makes
	// the invitation a credential for one person rather than a general-purpose
	// bypass (memql#4282).
	Email       string
	Role        string
	InviterId   string
	InviterName string
	ExpiresAt   time.Time
	RespondedAt time.Time
}

// LookupInvitationByTokenHash resolves a presented invitation token to its row.
//
// The lookup is by HASH, so the plaintext never has to be stored and a database
// snapshot cannot be replayed into a registration. It returns the row whatever
// its state -- expired, revoked, already accepted -- because the caller has to
// tell those apart: "you already used this", "somebody cancelled this" and
// "this expired" are three different next steps for the person holding the
// link, and collapsing them into "invalid" is what makes an invitation flow
// feel broken.
func (s *Store) LookupInvitationByTokenHash(ctx context.Context, tokenHash string) (*InvitationRow, error) {
	if strings.TrimSpace(tokenHash) == "" {
		return nil, nil
	}
	query := fmt.Sprintf(`query invitationByTokenHash(tokenHash: "%s")`, escapeMemQLString(tokenHash))
	nodes, err := s.executeAndExtract(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup invitation: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil {
		return nil, nil
	}
	node := nodes[0]
	g := newFieldGetter(node)
	return &InvitationRow{
		ID:          firstNonEmpty(g.str("id"), node.GetId()),
		Kind:        g.str("kind"),
		Status:      g.str("status"),
		Active:      g.boolField("active"),
		Email:       g.str("inviteeEmail"),
		Role:        g.str("inviteeRole"),
		InviterId:   g.str("inviterId"),
		InviterName: g.str("inviterName"),
		ExpiresAt:   g.time("expiresAt"),
		RespondedAt: g.time("respondedAt"),
	}, nil
}
