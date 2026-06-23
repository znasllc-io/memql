// Package magiclink owns the issuance and verification of magic-link
// tokens.
//
// Issuance flow (POST /auth/magic-link):
//
//  1. Issuer.Issue validates the email under the current
//     registration mode + invitation context (via the registration
//     package). On reject, returns ErrEmailNotAllowed and the HTTP
//     handler responds 403.
//  2. On accept, mints a 32-byte random token, persists its SHA-256
//     hash on a v1:identity:magicLinkRequest row, and hands the
//     plaintext link to the Sender.
//  3. The Sender delivers via email (Microsoft Graph / SMTP) or logs
//     to slog in dev. Issuer.Issue returns nil and the HTTP handler
//     responds 200/204 — the user is told to "check your email."
//
// Verification flow (GET /auth/complete) lives in verifier.go.
package magiclink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/registration"
)

// Sentinel errors callers can branch on. The HTTP layer translates
// these into stable, non-leaky responses (403 / 400 / 500).
var (
	ErrEmailNotAllowed   = errors.New("magiclink: email not allowed by registration policy")
	ErrUnknownClient     = errors.New("magiclink: unknown clientId")
	ErrRedirectMismatch  = errors.New("magiclink: redirectURI not registered for clientId")
	ErrAccessRequestPath = errors.New("magiclink: routed to access-request path (waitlist mode)")

	// ErrClusterNotBootstrapped fires when the cluster hasn't
	// completed first-run setup (clusterSettings.bootstrappedAt is
	// empty). The HTTP layer responds 423 (Locked) and the web layer
	// redirects to /setup. Prevents the race where a random visitor
	// lands on /login before the operator runs the wizard, submits
	// an email, and accidentally claims the cluster-owner role via
	// the magic-link verifier's first-user-is-owner path.
	ErrClusterNotBootstrapped = errors.New("magiclink: cluster not yet bootstrapped (run /setup first)")
)

// Sender abstracts the outbound email channel. component/identity
// supplies an EngineEmailSender that resolves the email integration
// plug-in at call time; tests pass an in-memory recorder.
type Sender interface {
	SendMagicLink(ctx context.Context, in SendInput) error
}

// SendInput is the rendered email request the Sender consumes.
type SendInput struct {
	Email             string
	LinkURL           string
	BrandName         string
	BrandPrimaryColor string
	BrandLogoDataURI  string
	ExpiresIn         time.Duration

	// Bootstrap=true tells the email integration to render the
	// claim-ownership variant of the email (subject + body) instead
	// of the regular sign-in copy. Set by the issuer when the
	// IssueInput was a /setup-wizard owner-mint request; the
	// flag is server-stamped, not request-derived.
	Bootstrap bool
}

// Issuer mints magic-link tokens. Constructed once at app boot and
// shared across requests; safe for concurrent use.
type Issuer struct {
	Cfg    identity.Config
	Store  *identity.Store
	Audit  identity.AuditLogger
	Sender Sender
	Logger *slog.Logger

	// IsBootstrapped, when set, gates the entire pipeline behind a
	// "cluster has at least one user" check. The wiring layer hooks
	// this to Store.CountActiveUsers. When unset (e.g. tests), the
	// gate is open. Returning false short-circuits Issue with
	// ErrClusterNotBootstrapped — no email is sent, no row is
	// written. The first user MUST come through the /setup wizard's
	// trusted entry point, not through random /login traffic.
	IsBootstrapped func(ctx context.Context) bool
}

// IssueInput is the per-request payload the HTTP handler passes in.
type IssueInput struct {
	Email               string
	ClientId            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	SourceIP            string
	UserAgent           string
	InvitationId        string
	CorrelationId       string

	// Bootstrap=true marks the call as the /setup wizard's owner-mint
	// path. Skips the bootstrap-state gate (chicken-and-egg: the
	// wizard IS the bootstrap event) AND the registration-mode gate
	// (waitlist / invite_only / domain_restricted shouldn't apply to
	// the operator who's literally running setup). The wizard is the
	// only trusted caller; HTTP handlers MUST NOT propagate this
	// flag from a request body.
	Bootstrap bool

	// ExistingUser=true means the caller has already verified that
	// the email belongs to an enrolled v1:identity:user row, so the
	// registration-mode gate (waitlist, invite_only, domain_restricted)
	// MUST NOT apply -- those modes are for *new* sign-ups; an
	// existing user signing in always gets a magic link regardless
	// of the cluster's current registration policy. Set by the
	// web /login handler after routeLoginEmail's user-exists check;
	// not propagated from request bodies.
	ExistingUser bool

	// AdminSession=true marks the request as an identity-admin
	// sign-in (no relying-party OAuth callback; the click should
	// land in /admin/, not bounce out to a client app). The web
	// handler sets this when /login has no return_to / matched
	// client. Skips the FindClient + AllowsRedirectURI gates --
	// there is no client to validate -- and stamps `adminSession`
	// on the magic-link row's oauthCtx so the verifier short-
	// circuits to startAdminSession at consume time.
	AdminSession bool
}

// Issue runs the full issuance pipeline. Returns nil on success; the
// HTTP handler responds 200 regardless of whether a magic link was
// actually sent (the access-request path also returns nil) so it
// doesn't leak whether a given email is registered.
//
// On policy reject, returns ErrEmailNotAllowed; the handler still
// responds 200 to the caller (don't leak), but emits an audit row
// and a slog warning.
func (i *Issuer) Issue(ctx context.Context, in IssueInput) error {
	if i == nil {
		return errors.New("magiclink: nil issuer")
	}
	if i.Store == nil {
		return errors.New("magiclink: nil store")
	}

	email := strings.TrimSpace(in.Email)
	if email == "" {
		return errors.New("magiclink: email required")
	}
	if !strings.Contains(email, "@") {
		return errors.New("magiclink: malformed email")
	}

	// Bootstrap gate. Pre-bootstrap (no users), only the /setup wizard
	// can mint the first user. Random /login + /auth/magic-link traffic
	// is rejected so a stranger can't claim cluster ownership before the
	// operator runs setup.
	//
	// in.Bootstrap=true bypasses this — that's the wizard's own
	// owner-mint call. The wizard already wrote clusterSettings
	// before invoking us; we'd be racing our own bootstrap stamp
	// otherwise.
	if !in.Bootstrap && i.IsBootstrapped != nil && !i.IsBootstrapped(ctx) {
		i.audit(ctx, identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "magic_link_blocked",
			TargetEmail:   email,
			SourceIP:      in.SourceIP,
			UserAgent:     in.UserAgent,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "cluster_not_bootstrapped",
		})
		return ErrClusterNotBootstrapped
	}

	// Admin-session sign-ins bypass the client-redirect gate: there
	// is no relying party, the click will land in /admin/ via
	// startAdminSession. Every other path must name a registered
	// client + a registered redirect URI for that client (so callers
	// can't smuggle in attacker-supplied targets via the magic link).
	if !in.AdminSession {
		if in.ClientId == "" {
			return ErrUnknownClient
		}
		// Resolve through the unified resolver so dynamically-registered
		// (RFC 7591) clients in the store work exactly like static
		// MEMQL_IDENTITY_REGISTERED_CLIENTS.
		if identity.ResolveClient(ctx, i.Cfg, i.Store, in.ClientId) == nil {
			return ErrUnknownClient
		}
		if in.RedirectURI == "" || !identity.ClientAllowsRedirectURI(ctx, i.Cfg, i.Store, in.ClientId, in.RedirectURI) {
			return ErrRedirectMismatch
		}
	}

	// Registration-mode dispatch.
	//
	// Two flags bypass the mode gate:
	//   - Bootstrap (the wizard's owner-mint; the operator running
	//     /setup is the trusted entry point and hasn't been able to
	//     add themselves to any allow-list yet)
	//   - ExistingUser (the email already belongs to an enrolled
	//     user; registration policy doesn't apply to returning
	//     sign-ins)
	// Both are server-stamped by trusted callers, never propagated
	// from a request body.
	var decision registration.Decision
	if in.Bootstrap || in.ExistingUser {
		decision = registration.Decision{Action: registration.ActionIssueMagicLink}
	} else {
		hasInvitation := strings.TrimSpace(in.InvitationId) != ""
		var err error
		decision, err = registration.Decide(i.Cfg, email, hasInvitation)
		if err != nil {
			i.audit(ctx, identity.AuditEvent{
				Category:      identity.AuditCategoryAuth,
				Action:        "magic_link_issue_blocked",
				TargetEmail:   email,
				SourceIP:      in.SourceIP,
				UserAgent:     in.UserAgent,
				CorrelationId: in.CorrelationId,
				Outcome:       identity.AuditOutcomeBlocked,
				FailureReason: decision.Reason,
			})
			return ErrEmailNotAllowed
		}
	}

	switch decision.Action {
	case registration.ActionCreateAccessRequest:
		return i.handleAccessRequest(ctx, email, in)

	case registration.ActionIssueMagicLink:
		// fall through

	default:
		// Reject (handled above) or unknown action.
		return ErrEmailNotAllowed
	}

	// Mint token + request id.
	plainToken, tokenHash, err := newMagicLinkToken()
	if err != nil {
		return fmt.Errorf("magiclink: generate token: %w", err)
	}
	requestId, err := identity.NewRandomId("")
	if err != nil {
		return fmt.Errorf("magiclink: generate request id: %w", err)
	}

	ttl := i.Cfg.MagicLinkTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	expiresAt := time.Now().UTC().Add(ttl)

	// Encode the OAuth context the verifier will use to construct the
	// final redirect target. `bootstrap` is server-stamped at issue
	// time (never accepted from a request body); the verifier reads it
	// back to decide whether the click should land in identity's own
	// /admin (bootstrap path) or bounce through the OAuth callback to
	// the relying-party SPA.
	oauthCtx := map[string]any{
		"clientId":    in.ClientId,
		"redirectURI": in.RedirectURI,
	}
	if in.State != "" {
		oauthCtx["state"] = in.State
	}
	if in.CodeChallenge != "" {
		oauthCtx["codeChallenge"] = in.CodeChallenge
		oauthCtx["codeChallengeMethod"] = in.CodeChallengeMethod
	}
	if in.Bootstrap {
		oauthCtx["bootstrap"] = true
	}
	if in.AdminSession {
		oauthCtx["adminSession"] = true
	}
	oauthCtxJSON, _ := json.Marshal(oauthCtx)

	if err := i.Store.CreateMagicLinkRequest(
		ctx,
		requestId,
		email,
		tokenHash,
		expiresAt.Format(time.RFC3339Nano),
		in.SourceIP,
		in.UserAgent,
		string(oauthCtxJSON),
		in.InvitationId,
	); err != nil {
		return fmt.Errorf("magiclink: persist request: %w", err)
	}

	linkURL := buildMagicLinkURL(i.Cfg.BaseURL, plainToken, in.State)

	if i.Sender != nil {
		brandName := i.Cfg.BrandName
		if brandName == "" {
			brandName = "memQL"
		}
		if err := i.Sender.SendMagicLink(ctx, SendInput{
			Email:             email,
			LinkURL:           linkURL,
			BrandName:         brandName,
			BrandPrimaryColor: i.Cfg.BrandPrimaryColor,
			BrandLogoDataURI:  i.Cfg.BrandLogoDataURI,
			ExpiresIn:         ttl,
			Bootstrap:         in.Bootstrap,
		}); err != nil {
			// Log but don't fail the request — the row is already
			// persisted; the user can request another link if the
			// email never arrives.
			if i.Logger != nil {
				i.Logger.Warn("magiclink: sender failed",
					slog.String("email", email),
					slog.String("error", err.Error()),
				)
			}
		}
	}

	i.audit(ctx, identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        "magic_link_issued",
		TargetType:    "magicLinkRequest",
		TargetId:      requestId,
		TargetEmail:   email,
		SourceIP:      in.SourceIP,
		UserAgent:     in.UserAgent,
		CorrelationId: in.CorrelationId,
		Outcome:       identity.AuditOutcomeSuccess,
		Detail: map[string]any{
			"clientId":    in.ClientId,
			"redirectURI": in.RedirectURI,
		},
	})

	return nil
}

// handleAccessRequest creates a v1:identity:accessRequest row instead
// of issuing a magic link (waitlist mode).
func (i *Issuer) handleAccessRequest(ctx context.Context, email string, in IssueInput) error {
	requestId, err := identity.NewRandomId("")
	if err != nil {
		return fmt.Errorf("magiclink: generate access-request id: %w", err)
	}
	if err := i.Store.CreateAccessRequest(
		ctx,
		requestId,
		email,
		"", // name unknown at this stage
		"", // additionalContext
		0,  // riskScore — Phase 4 plumbs the real value
		"", // riskSignals
		in.SourceIP,
		in.UserAgent,
	); err != nil {
		return fmt.Errorf("magiclink: persist access request: %w", err)
	}
	i.audit(ctx, identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        "access_request_created",
		TargetType:    "accessRequest",
		TargetId:      requestId,
		TargetEmail:   email,
		SourceIP:      in.SourceIP,
		UserAgent:     in.UserAgent,
		CorrelationId: in.CorrelationId,
		Outcome:       identity.AuditOutcomeSuccess,
	})
	// Return ErrAccessRequestPath so the HTTP handler can shape the
	// response body differently (still 200, but a "we received your
	// request" message instead of a "check your email" message).
	return ErrAccessRequestPath
}

// audit is a small wrapper that nil-guards the AuditLogger so callers
// don't have to.
func (i *Issuer) audit(ctx context.Context, ev identity.AuditEvent) {
	if i == nil || i.Audit == nil {
		return
	}
	i.Audit.Log(ctx, ev)
}

// newMagicLinkToken returns the plaintext URL-safe base64 token and
// its lowercase-hex SHA-256 hash.
func newMagicLinkToken() (plain, hash string, err error) {
	const tokenBytes = 32
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plain))
	hash = hex.EncodeToString(sum[:])
	return plain, hash, nil
}

// buildMagicLinkURL constructs the link the user clicks in their
// email. The token rides as a query param so existing email clients
// don't garble it.
func buildMagicLinkURL(baseURL, plainToken, state string) string {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/auth/complete")
	if err != nil {
		// Defensive: build by string concat if baseURL is malformed,
		// validation upstream would already have caught this.
		return baseURL + "/auth/complete?ml=" + plainToken
	}
	q := u.Query()
	q.Set("ml", plainToken)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// HashMagicLinkToken hashes a plaintext magic-link token with the
// same algorithm Issuer used. Exposed so the verifier (and tests)
// share a single implementation.
func HashMagicLinkToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}
