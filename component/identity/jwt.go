package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenClaims is the JWT payload identity issues for every
// successful login. Keep this stable — every node in the cluster
// validates against this exact shape.
//
// Conventions:
//
//	sub          v1:identity:user.id (canonical user identifier)
//	email        the user's primary email at time of issue
//	name         display name (best-effort)
//	given_name   first name (OIDC-style claim) — the CoPresent profile
//	             shows this as "First name". Empty when the user row
//	             doesn't carry it yet.
//	family_name  last name (OIDC-style claim) — same lifecycle as
//	             given_name.
//	role         cluster-wide role: owner / admin / writer / reader.
//	             external users have empty role here; their scoping
//	             lives in `partitions`.
//	internal     true if the user's email matched an internal-domain
//	             entry at registration. UI surfaces use this for the
//	             "employee" / "external" distinction.
//	partitions   per-partition role grants. Map of partition name to
//	             role. Cluster owners have empty map (they bypass
//	             the ACL check entirely).
//	jti          unique token id, useful for the future "blacklist
//	             this specific token" capability if we ever need it.
//	sid          v1:identity:authSession.id — every refresh keeps the
//	             same sid so per-device revoke can target it.
//	revocation_epoch  monotonically-increasing per-user counter snapshot
//	             at issue time. The verifier rejects any JWT whose claim
//	             is below the user's current revocationEpoch row value;
//	             admin "revoke all tokens for this user NOW" tooling
//	             bumps the row, which invalidates every prior token at
//	             the next stream-open or periodic in-stream re-check.
//	             See memql#106.
type AccessTokenClaims struct {
	Email           string            `json:"email,omitempty"`
	Name            string            `json:"name,omitempty"`
	GivenName       string            `json:"given_name,omitempty"`
	FamilyName      string            `json:"family_name,omitempty"`
	Role            string            `json:"role,omitempty"`
	Internal        bool              `json:"internal,omitempty"`
	Partitions      map[string]string `json:"partitions,omitempty"`
	SessionId       string            `json:"sid,omitempty"`
	RevocationEpoch int64             `json:"revocation_epoch,omitempty"`
	// Class identifies the credential family the JWT was minted for:
	// "user" (magic-link / OAuth interactive sessions; default when
	// the claim is omitted, for backwards compatibility) or "node"
	// (cluster-internal binary; minted via IssueNodeAccessToken).
	// Surface-pinned filters (NodeService.Stream requires
	// class="node"; the cockpit's user-facing surfaces reject
	// class="node") read this to keep cross-class tokens off the
	// wrong wire. See #105.
	Class string `json:"class,omitempty"`
	// NodeId binds a class="node" token to a specific v1:cluster:node.id.
	// The NodeService interceptor cross-checks NodeHello.NodeId
	// against this value -- a token minted for nodeId=A cannot drive
	// a stream that announces nodeId=B. Empty for any other class.
	NodeId string `json:"node_id,omitempty"`
	// NodeType is the build-tag-derived role the node binary serves
	// ("bff", "voice", "cognition", "agent", "planner", "workbench").
	// Cross-checked against NodeHello.NodeType the same way as
	// NodeId. Empty for any other class.
	NodeType string `json:"node_type,omitempty"`

	jwt.RegisteredClaims
}

// Class string constants for the `class` claim. Centralized so
// callers don't sprinkle bare string literals through the codebase.
const (
	// ClassUser is the default class for human-driven sessions
	// (magic-link, OAuth refresh). Empty class claim is treated as
	// ClassUser for backward compatibility.
	ClassUser = "user"
	// ClassNode marks a cluster-internal node service-account token
	// (#105). NodeService.Stream rejects every other class.
	ClassNode = "node"
	// ClassVoiceAgent marks the Python voice-agent process's
	// service-account token (#109). MemqlService.Stream's
	// voice-agent interceptor admits this class and pins the call
	// to VoiceAgent* payload types -- a leaked voice-agent token
	// can't drive other RPCs.
	ClassVoiceAgent = "voice_agent"
)

// JWTIssuer mints access tokens. Wraps a KeyManager for signing-key
// material plus a Config for issuer / audience / TTL.
type JWTIssuer struct {
	keys     *KeyManager
	issuer   string
	audience string
	ttl      time.Duration
}

// NewJWTIssuer builds a JWTIssuer rooted at the given key manager
// and config. Returns an error if config doesn't have a usable issuer
// or audience.
func NewJWTIssuer(km *KeyManager, cfg Config) (*JWTIssuer, error) {
	if km == nil {
		return nil, errors.New("identity: nil KeyManager")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("identity: cfg.BaseURL required for JWT issuer (sets the iss claim)")
	}
	if cfg.JWTAudience == "" {
		return nil, errors.New("identity: cfg.JWTAudience required for JWT issuer")
	}
	ttl := cfg.AccessTokenTTL
	if ttl <= 0 {
		ttl = time.Duration(DefaultAccessTokenTTLSeconds) * time.Second
	}
	return &JWTIssuer{
		keys:     km,
		issuer:   cfg.BaseURL,
		audience: cfg.JWTAudience,
		ttl:      ttl,
	}, nil
}

// IssueInput is the per-call payload for IssueAccessToken. Caller
// supplies the user identity; the issuer fills in iat/exp/iss/aud/jti.
type IssueInput struct {
	UserId          string
	Email           string
	Name            string
	GivenName       string
	FamilyName      string
	Role            string
	Internal        bool
	Partitions      map[string]string
	SessionId       string
	RevocationEpoch int64
	// Class identifies the credential family this token is being
	// minted for. Defaults to ClassUser when empty -- existing
	// magic-link / OAuth-refresh mint sites don't need to set this
	// explicitly; node-token mints (IssueNodeAccessToken) stamp
	// ClassNode + the node id / type fields below.
	Class    string
	NodeId   string
	NodeType string
	// TTLOverride is the per-call access-token lifetime. When > 0,
	// it replaces the issuer's boot-time default for THIS issuance
	// only. The HTTP handlers read the runtime-tunable value from
	// ClusterSettings (LiveTokenSettings.AccessTokenTTL) and pass
	// it through here so admin-edited TTLs apply on the next mint
	// without an identity restart. 0 = use the issuer default.
	TTLOverride time.Duration
}

// IssueAccessToken signs a fresh JWT for the given input. Returns the
// signed compact-form string plus the absolute expiry time so the
// caller can stamp the right value into v1:identity:authSession.
func (j *JWTIssuer) IssueAccessToken(in IssueInput, now time.Time) (string, time.Time, error) {
	if in.UserId == "" {
		return "", time.Time{}, errors.New("identity: IssueInput.UserId required")
	}
	mat := j.keys.Current()
	if mat == nil {
		return "", time.Time{}, errors.New("identity: no current signing key (was KeyManager.Load() called?)")
	}
	ttl := j.ttl
	if in.TTLOverride > 0 {
		ttl = in.TTLOverride
	}
	expiresAt := now.Add(ttl)

	jti, err := newJTI()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: generate jti: %w", err)
	}

	// Normalize the class. Empty / "user" both serialize to the
	// omit-empty case, which keeps existing tokens unchanged. Node
	// tokens carry "node" explicitly + the bound nodeId / nodeType.
	class := strings.TrimSpace(in.Class)
	if class == ClassUser {
		class = ""
	}
	claims := AccessTokenClaims{
		Email:           in.Email,
		Name:            in.Name,
		GivenName:       in.GivenName,
		FamilyName:      in.FamilyName,
		Role:            in.Role,
		Internal:        in.Internal,
		Partitions:      in.Partitions,
		SessionId:       in.SessionId,
		RevocationEpoch: in.RevocationEpoch,
		Class:           class,
		NodeId:          in.NodeId,
		NodeType:        in.NodeType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    j.issuer,
			Subject:   in.UserId,
			Audience:  jwt.ClaimStrings{j.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        jti,
		},
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = mat.KID

	signed, err := tok.SignedString(mat.Private)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// VerifyAccessToken parses and validates a signed access token against
// the issuer's KeyManager. Returns the claims on success. Used both
// by the identity service itself (for refresh and admin requests) and
// — once Phase 2/5 lands — by every other node's auth interceptor
// against a JWKS-fetched public-key set.
//
// Strict checks: signature with the kid'd key, iss match, aud match,
// exp/nbf bounds, EdDSA algorithm only (downgrade-attack-proof).
func (j *JWTIssuer) VerifyAccessToken(raw string, now time.Time) (*AccessTokenClaims, error) {
	parsed, err := jwt.ParseWithClaims(raw, &AccessTokenClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("unexpected signing method %v (expected EdDSA)", t.Method.Alg())
		}
		kidIface, ok := t.Header["kid"]
		if !ok {
			return nil, errors.New("missing kid header")
		}
		kid, ok := kidIface.(string)
		if !ok {
			return nil, errors.New("kid header is not a string")
		}
		// Try current then previous (overlap window).
		if cur := j.keys.Current(); cur != nil && cur.KID == kid {
			return ed25519.PublicKey(cur.Public), nil
		}
		if prev := j.keys.Previous(); prev != nil && prev.KID == kid {
			return ed25519.PublicKey(prev.Public), nil
		}
		return nil, fmt.Errorf("unknown kid %q", kid)
	},
		jwt.WithIssuer(j.issuer),
		jwt.WithAudience(j.audience),
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, errors.New("token marked invalid by parser")
	}
	claims, ok := parsed.Claims.(*AccessTokenClaims)
	if !ok {
		return nil, errors.New("unexpected claims type")
	}
	return claims, nil
}

// NodeIssueInput is the per-call payload for IssueNodeAccessToken.
// Node tokens authenticate cluster-internal binaries on
// NodeService.Stream and carry no human-directory fields.
//
// Subject (the JWT `sub` claim) is the v1:identity:identity row id
// of the node_token credential -- NOT a v1:identity:user.id. The
// NodeService interceptor reads the bound NodeId / NodeType from
// the class-specific claims (NOT from sub).
//
// TTL: defaults to identity.DefaultNodeTokenTTLSeconds when zero.
// There's no refresh-token path for node tokens; ops rotates them
// out-of-band by minting a fresh one and restarting the target
// binary with the new MEMQL_NODE_TOKEN.
type NodeIssueInput struct {
	// IdentityId is the v1:identity:identity row carrying the
	// node_token credential. Used as the JWT subject so admin
	// tooling can correlate the token back to its credential row
	// for revoke / rotate.
	IdentityId string
	// NodeId is the v1:cluster:node row id the token binds to.
	// Stamped onto the `node_id` claim; the NodeService interceptor
	// cross-checks NodeHello.NodeId against it.
	NodeId string
	// NodeType is the build-tag-derived role ("bff", "voice",
	// "cognition", "agent", "planner", "workbench"). Stamped onto
	// the `node_type` claim; cross-checked against NodeHello.NodeType.
	NodeType string
	// TTLOverride is the per-call lifetime. Zero falls back to
	// DefaultNodeTokenTTLSeconds.
	TTLOverride time.Duration
}

// IssueNodeAccessToken mints a class="node" JWT bound to the given
// v1:identity:identity row + v1:cluster:node id + node type. Same
// signing-key material as the user-token path -- the verifier
// validates both with the same JWKS-published EdDSA key.
//
// Returns (signedJWT, expiresAt, jti, err). The jti is the unique
// per-mint identifier stamped into the JWT's `jti` claim; memql#343
// surfaces it back to /node/bootstrap so the bootstrap path can
// record "which JTI is the current row state" on the persisted
// v1:identity:identity row without re-parsing the signed token.
func (j *JWTIssuer) IssueNodeAccessToken(in NodeIssueInput, now time.Time) (string, time.Time, string, error) {
	if strings.TrimSpace(in.IdentityId) == "" {
		return "", time.Time{}, "", errors.New("identity: NodeIssueInput.IdentityId required")
	}
	if strings.TrimSpace(in.NodeId) == "" {
		return "", time.Time{}, "", errors.New("identity: NodeIssueInput.NodeId required")
	}
	if strings.TrimSpace(in.NodeType) == "" {
		return "", time.Time{}, "", errors.New("identity: NodeIssueInput.NodeType required")
	}
	mat := j.keys.Current()
	if mat == nil {
		return "", time.Time{}, "", errors.New("identity: no current signing key (was KeyManager.Load() called?)")
	}
	ttl := in.TTLOverride
	if ttl <= 0 {
		ttl = time.Duration(DefaultNodeTokenTTLSeconds) * time.Second
	}
	expiresAt := now.Add(ttl)
	jti, err := newJTI()
	if err != nil {
		return "", time.Time{}, "", fmt.Errorf("identity: generate jti: %w", err)
	}
	claims := AccessTokenClaims{
		Class:    ClassNode,
		NodeId:   in.NodeId,
		NodeType: in.NodeType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    j.issuer,
			Subject:   in.IdentityId,
			Audience:  jwt.ClaimStrings{j.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        jti,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = mat.KID
	signed, err := tok.SignedString(mat.Private)
	if err != nil {
		return "", time.Time{}, "", fmt.Errorf("identity: sign node access token: %w", err)
	}
	return signed, expiresAt, jti, nil
}

// VoiceAgentIssueInput is the per-call payload for
// IssueVoiceAgentAccessToken. Voice-agent tokens authenticate the
// Python voice-agent process on MemqlService.Stream; the
// interceptor pins the surface to VoiceAgent* payload types so a
// leaked credential can't drive other RPCs.
//
// Subject is the v1:identity:identity row id of the
// voice_agent_token credential -- NOT a v1:identity:user.id.
type VoiceAgentIssueInput struct {
	// IdentityId is the v1:identity:identity row carrying the
	// voice_agent_token credential. Used as the JWT subject so
	// admin tooling can correlate the token back to its credential
	// row for revoke / rotate.
	IdentityId string
	// InstanceId is the operator-chosen voice-agent instance label
	// (e.g. "voice-agent-prod-us-east-1"). Stamped onto a
	// surface-specific claim so audit can attribute traffic to a
	// specific process.
	InstanceId string
	// TTLOverride is the per-call lifetime. Zero falls back to
	// DefaultVoiceAgentTokenTTLSeconds.
	TTLOverride time.Duration
}

// IssueVoiceAgentAccessToken mints a class="voice_agent" JWT bound
// to the given v1:identity:identity row + instance id. The
// interceptor admits these tokens on MemqlService.Stream and pins
// the call to VoiceAgent* payload types; a leaked credential can't
// drive other RPCs.
func (j *JWTIssuer) IssueVoiceAgentAccessToken(in VoiceAgentIssueInput, now time.Time) (string, time.Time, error) {
	if strings.TrimSpace(in.IdentityId) == "" {
		return "", time.Time{}, errors.New("identity: VoiceAgentIssueInput.IdentityId required")
	}
	if strings.TrimSpace(in.InstanceId) == "" {
		return "", time.Time{}, errors.New("identity: VoiceAgentIssueInput.InstanceId required")
	}
	mat := j.keys.Current()
	if mat == nil {
		return "", time.Time{}, errors.New("identity: no current signing key (was KeyManager.Load() called?)")
	}
	ttl := in.TTLOverride
	if ttl <= 0 {
		ttl = time.Duration(DefaultVoiceAgentTokenTTLSeconds) * time.Second
	}
	expiresAt := now.Add(ttl)
	jti, err := newJTI()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: generate jti: %w", err)
	}
	// Reuse the NodeId claim slot for the instance id -- the claims
	// struct is shared across class types, and adding a third
	// per-class binding field would clutter every other JWT's
	// payload. The verifier reads NodeId without interpretation
	// (no class-specific cross-check on the voice-agent path); the
	// interceptor uses it purely for audit logging.
	claims := AccessTokenClaims{
		Class:  ClassVoiceAgent,
		NodeId: in.InstanceId,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    j.issuer,
			Subject:   in.IdentityId,
			Audience:  jwt.ClaimStrings{j.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        jti,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = mat.KID
	signed, err := tok.SignedString(mat.Private)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: sign voice-agent access token: %w", err)
	}
	return signed, expiresAt, nil
}

// newJTI returns a 128-bit random hex id suitable for the JWT `jti`
// claim.
func newJTI() (string, error) {
	const jtiBytes = 16
	buf := make([]byte, jtiBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
}
