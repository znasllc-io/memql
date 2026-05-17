package pat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// Sentinel errors callers branch on.
var (
	ErrEmptyAuthToken    = errors.New("pat.Verifier: empty token")
	ErrPATNotFound       = errors.New("pat.Verifier: token not recognized")
	ErrPATRevoked        = errors.New("pat.Verifier: token has been revoked")
	ErrPATUserNotFound   = errors.New("pat.Verifier: PAT row found but owning user is missing")
	ErrPATUserSuspended  = errors.New("pat.Verifier: PAT owner is suspended")
	ErrJWTUnverifiable   = errors.New("pat.Verifier: JWT path requires Issuer")
	ErrUserStoreMissing  = errors.New("pat.Verifier: UserLookup not wired")
)

// Source identifies which credential family produced the claims.
// gRPC handlers can use it to scope per-request behavior (e.g. PAT
// callers don't carry an interactive session id, so per-device
// revocation logic is JWT-only).
type Source string

const (
	SourceJWT Source = "jwt"
	SourcePAT Source = "pat"
)

// Claims is the unified shape verifyJWT and verifyPAT both produce.
// Mirrors AccessTokenClaims for the JWT path; for the PAT path,
// SessionId is empty and PATID carries the v1:identity:identity row id
// the request authenticated against.
type Claims struct {
	UserId    string
	Email     string
	Name      string
	Role      string
	Internal  bool
	SessionId string // empty for PATs
	PATID     string // empty for JWTs
	ExpiresAt time.Time
	Source    Source
}

// UserLookup is the narrow interface the PAT path uses to materialize
// claims after the keyHash lookup. The wiring layer satisfies this
// with a closure that runs queryUserById against the engine.
type UserLookup interface {
	UserById(ctx context.Context, userId string) (*UserSummary, error)
}

// UserSummary projects the v1:identity:user fields the verifier needs
// to build PAT claims. Mirrors identity.UserRow but lives here so the
// pat package doesn't drag in the full Store surface.
type UserSummary struct {
	ID           string
	DisplayName  string
	PrimaryEmail string
	Role         string
	Active       bool
	Internal     bool
}

// Verifier accepts either a PAT or a JWT and returns unified Claims.
//
//	Store    -- pat row lookup. Required for the PAT path.
//	Issuer   -- JWT issuer. Required for the JWT path.
//	Users    -- looks up the owning user when a PAT verifies.
//	Audit    -- audit logger; may be nil (no-op).
//	Logger   -- slog logger; may be nil.
//
// The Verifier is safe for concurrent use.
type Verifier struct {
	Store  *Store
	Issuer *identity.JWTIssuer
	Users  UserLookup
	Audit  identity.AuditLogger
	Logger *slog.Logger
}

// VerifyToken returns claims for either a PAT or a JWT. Callers
// (gRPC interceptors, HTTP middleware) don't need to know which kind
// of token came in.
//
// Strict failure modes:
//
//	ErrEmptyAuthToken    -- empty input.
//	ErrPATNotFound       -- mql_pat_<...> didn't match any row.
//	ErrPATRevoked        -- row matched but is inactive.
//	ErrPATUserNotFound   -- row matched but owning user is missing.
//	ErrPATUserSuspended  -- owning user is suspended (active=false).
//	(JWT errors)         -- propagated from JWTIssuer.VerifyAccessToken.
func (v *Verifier) VerifyToken(ctx context.Context, token string) (*Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrEmptyAuthToken
	}
	if IsPATToken(token) {
		return v.verifyPAT(ctx, token)
	}
	return v.verifyJWT(ctx, token)
}

// verifyPAT hashes the token, looks up the matching PAT row, materializes
// the owning user's claims, and triggers a best-effort lastUsedAt bump.
func (v *Verifier) verifyPAT(ctx context.Context, token string) (*Claims, error) {
	if v == nil || v.Store == nil {
		return nil, errors.New("pat.Verifier: Store not wired")
	}
	if v.Users == nil {
		return nil, ErrUserStoreMissing
	}
	hash := Hash(token)
	row, err := v.Store.LookupByKeyHash(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("pat.Verifier: lookup: %w", err)
	}
	if row == nil {
		return nil, ErrPATNotFound
	}
	if !row.Active {
		v.audit(ctx, identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "pat_auth_rejected",
			ActorUserId:   row.UserId,
			TargetType:    "identity",
			TargetId:      row.ID,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "revoked",
		})
		return nil, ErrPATRevoked
	}
	user, err := v.Users.UserById(ctx, row.UserId)
	if err != nil {
		return nil, fmt.Errorf("pat.Verifier: user lookup: %w", err)
	}
	if user == nil {
		return nil, ErrPATUserNotFound
	}
	if !user.Active {
		v.audit(ctx, identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "pat_auth_rejected",
			ActorUserId:   user.ID,
			ActorEmail:    user.PrimaryEmail,
			TargetType:    "identity",
			TargetId:      row.ID,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "user_suspended",
		})
		return nil, ErrPATUserSuspended
	}

	// Best-effort bump. Failure is logged but not surfaced to caller.
	if err := v.Store.BumpLastUsed(ctx, row.ID); err != nil {
		v.warn("pat.Verifier: lastUsedAt bump failed",
			"identityId", row.ID, "userId", user.ID, "error", err)
	}

	v.audit(ctx, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "pat_auth_accepted",
		ActorUserId: user.ID,
		ActorEmail:  user.PrimaryEmail,
		ActorRole:   user.Role,
		TargetType:  "identity",
		TargetId:    row.ID,
		Outcome:     identity.AuditOutcomeSuccess,
	})

	return &Claims{
		UserId:    user.ID,
		Email:     user.PrimaryEmail,
		Name:      user.DisplayName,
		Role:      user.Role,
		Internal:  user.Internal,
		PATID:     row.ID,
		ExpiresAt: time.Time{}, // PATs don't expire; rely on revocation.
		Source:    SourcePAT,
	}, nil
}

// verifyJWT delegates to the JWT issuer and adapts the result to the
// unified Claims shape.
func (v *Verifier) verifyJWT(ctx context.Context, token string) (*Claims, error) {
	if v == nil || v.Issuer == nil {
		return nil, ErrJWTUnverifiable
	}
	c, err := v.Issuer.VerifyAccessToken(token, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	exp := time.Time{}
	if c.RegisteredClaims.ExpiresAt != nil {
		exp = c.RegisteredClaims.ExpiresAt.Time
	}
	return &Claims{
		UserId:    c.RegisteredClaims.Subject,
		Email:     c.Email,
		Name:      c.Name,
		Role:      c.Role,
		Internal:  c.Internal,
		SessionId: c.SessionId,
		ExpiresAt: exp,
		Source:    SourceJWT,
	}, nil
}

func (v *Verifier) audit(ctx context.Context, ev identity.AuditEvent) {
	if v == nil || v.Audit == nil {
		return
	}
	v.Audit.Log(ctx, ev)
}

func (v *Verifier) warn(msg string, args ...any) {
	if v == nil || v.Logger == nil {
		return
	}
	v.Logger.Warn(msg, args...)
}
