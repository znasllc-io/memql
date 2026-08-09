package web

// GET / POST /device -- the human half of the RFC 8628 device
// authorization grant (memql#3410).
//
// The device shows a short code; the person carries it here on a
// second device, signs in the normal way, and answers one question.
// That question is the whole security value of the flow, so the page
// puts the evidence needed to answer it -- which application, from
// which IP, calling itself what -- in front of the approver rather
// than asking them to confirm a bare code. A device flow that shows
// only the code is a phishing primitive: an attacker starts an
// authorization and reads the code to a victim.
//
// Auth: the page requires a signed-in user before it will resolve a
// code at all. A signed-out visitor is bounced through /login and
// returned HERE, carrying the code, via the post-login cookie in
// component/identity/postlogin.go -- return_to could not do it,
// because return_to names a relying party and /device is first-party.

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/devicecode"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// envDeviceVerifyPerHour tunes the per-IP limit on the verification
// page. The page is a code ORACLE: each submission tells the caller
// whether a given user_code exists and what state it is in, so the
// 40-bit code space is only as strong as the number of guesses allowed
// against it. A human types one code, maybe twice.
const envDeviceVerifyPerHour = "MEMQL_IDENTITY_DEVICE_VERIFY_PER_HOUR"

const defaultDeviceVerifyPerHour = 120

// verifyLimiter returns this Server's per-IP limiter for /device,
// built once on first use. Server-scoped rather than a package global,
// matching the badge-grant limiter in the sibling http package: each
// Server (and each test) gets its own bucket map and captures its own
// logger. Same in-memory caveat as every other limiter in the identity
// tree -- a multi-replica deployment gets per-replica budgets.
func (s *Server) verifyLimiter() *abuse.IPRateLimiter {
	s.deviceVerifyLimiterOnce.Do(func() {
		perHour := defaultDeviceVerifyPerHour
		if v := strings.TrimSpace(os.Getenv(envDeviceVerifyPerHour)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perHour = n
			}
		}
		var logger *slog.Logger
		if s != nil {
			logger = s.Logger
		}
		s.deviceVerifyLimiterVal = abuse.NewIPRateLimiter(perHour, logger)
	})
	return s.deviceVerifyLimiterVal
}

// DeviceCodeAdapter is the narrow port the web package uses to drive
// the verification page, mirroring PATAdapter: the wiring layer
// satisfies it with the live *devicecode.Store so this package stays
// free of the engine.
type DeviceCodeAdapter interface {
	LookupByUserCodeHash(ctx context.Context, hash string) (*devicecode.Row, error)
	Approve(ctx context.Context, id string) error
	Deny(ctx context.Context, id string) error
}

// DeviceFlow wires the verification-page dependencies onto the Server.
// Set from app/integrations_identity.go once the engine is up; nil
// leaves /device unmounted rather than half-served.
type DeviceFlow struct {
	Adapter DeviceCodeAdapter
	Issuer  *identity.JWTIssuer
	Audit   identity.AuditLogger
	// ClientName resolves a client_id to its registered human name.
	// Optional -- a nil hook (or an unknown id) falls back to showing
	// the raw client_id, which is the value the session is actually
	// bound to and so is never the wrong thing to show.
	ClientName func(ctx context.Context, clientId string) string
}

// SetDeviceFlow stashes the device-verification dependencies. Called
// once at bootstrap, before Mount.
func (s *Server) SetDeviceFlow(d *DeviceFlow) {
	if s == nil {
		return
	}
	s.deviceFlow = d
}

// handleDeviceGet renders the code-entry form, or -- when the URL
// carries a user_code (verification_uri_complete) -- resolves it and
// renders the approval panel directly.
func (s *Server) handleDeviceGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUserForDevice(w, r); !ok {
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("user_code"))
	if raw == "" {
		s.renderDevice(w, r, webtempl.DeviceData{
			Layout: s.LayoutData(r, "Connect a device", false, nil, nil),
		})
		return
	}
	row, flash := s.resolveDeviceCode(r, raw)
	if row == nil {
		s.renderDevice(w, r, webtempl.DeviceData{
			Layout:      s.LayoutData(r, "Connect a device", false, nil, nil),
			Flash:       flash,
			PrefillCode: raw,
		})
		return
	}
	s.renderDevice(w, r, webtempl.DeviceData{
		Layout:      s.LayoutData(r, "Approve this sign-in?", false, nil, nil),
		PrefillCode: raw,
		Pending:     s.projectPending(r, row, raw),
	})
}

// handleDevicePost handles all three form submissions: the lookup that
// turns a typed code into the approval panel, and the Approve / Deny
// answers themselves. CSRF is enforced by the middleware Mount wraps
// every non-exempt POST in.
func (s *Server) handleDevicePost(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.requireUserForDevice(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "We couldn't read your form submission.")
		return
	}
	raw := strings.TrimSpace(r.PostForm.Get("user_code"))
	action := strings.TrimSpace(r.PostForm.Get("action"))

	row, flash := s.resolveDeviceCode(r, raw)
	if row == nil {
		s.renderDevice(w, r, webtempl.DeviceData{
			Layout:      s.LayoutData(r, "Connect a device", false, nil, nil),
			Flash:       flash,
			PrefillCode: raw,
		})
		return
	}

	switch action {
	case "approve", "deny":
		// fall through to the write below
	default:
		// "lookup", or a form posted without an action: show the panel.
		s.renderDevice(w, r, webtempl.DeviceData{
			Layout:      s.LayoutData(r, "Approve this sign-in?", false, nil, nil),
			PrefillCode: raw,
			Pending:     s.projectPending(r, row, raw),
		})
		return
	}

	// The approver is stamped from the actor by the mutation, so the
	// context has to carry the SIGNED-IN user rather than the identity
	// service's system actor. Same requirement, and the same reason, as
	// callerActorCtx in me_tokens.go: SystemActorMiddleware attaches
	// claims but not an auth.AccessContext, and `actor.userId` resolves
	// only from the latter.
	ctx := auth.ContextWithUserActor(r.Context(), claims.Subject)

	approved := action == "approve"
	var err error
	if approved {
		err = s.deviceFlow.Adapter.Approve(ctx, row.ID)
	} else {
		err = s.deviceFlow.Adapter.Deny(ctx, row.ID)
	}
	if err != nil {
		s.Logger.Warn("device: transition failed", "action", action, "id", row.ID, "error", err)
		s.deviceAudit(r, identity.AuditEvent{
			Action:        "device_authorization_answer_failed",
			ActorUserId:   claims.Subject,
			TargetType:    "deviceCode",
			TargetId:      row.ID,
			Outcome:       identity.AuditOutcomeFailure,
			FailureReason: "persist_failed",
		})
		s.renderError(w, r, http.StatusInternalServerError, "We couldn't record your answer. Please try again.")
		return
	}

	auditAction := "device_authorization_denied"
	if approved {
		auditAction = "device_authorization_approved"
	}
	s.deviceAudit(r, identity.AuditEvent{
		Action:      auditAction,
		ActorUserId: claims.Subject,
		ActorEmail:  claims.Email,
		ActorRole:   claims.Role,
		TargetType:  "deviceCode",
		TargetId:    row.ID,
		Outcome:     identity.AuditOutcomeSuccess,
		Detail:      map[string]any{"clientId": row.ClientId},
	})

	s.renderDevice(w, r, webtempl.DeviceData{
		Layout:       s.LayoutData(r, "Connect a device", false, nil, nil),
		Done:         true,
		DoneApproved: approved,
	})
}

// resolveDeviceCode turns a typed code into an APPROVABLE row, or into
// the flash explaining why there isn't one.
//
// The four refusals are kept distinct on purpose. "No such code",
// "already answered", "already used" and "expired" send the human to
// four different next steps, and collapsing them into one message
// would leave someone retyping a correct code that simply timed out.
// The information disclosed is bounded by the fact that reaching this
// function at all requires a valid session plus a 40-bit code.
func (s *Server) resolveDeviceCode(r *http.Request, raw string) (*devicecode.Row, *webtempl.Flash) {
	hash := devicecode.HashUserCode(raw)
	if hash == "" {
		return nil, &webtempl.Flash{Kind: "error", Message: "That doesn't look like a valid code. Check it against the device and try again."}
	}
	row, err := s.deviceFlow.Adapter.LookupByUserCodeHash(r.Context(), hash)
	if err != nil {
		s.Logger.Warn("device: lookup failed", "error", err)
		return nil, &webtempl.Flash{Kind: "error", Message: "We couldn't look that code up. Please try again."}
	}
	if row == nil {
		return nil, &webtempl.Flash{Kind: "error", Message: "We don't recognize that code. It may have already expired -- start again on the device."}
	}
	if row.IsExpired(time.Now().UTC()) {
		return nil, &webtempl.Flash{Kind: "error", Message: "That code has expired. Start the sign-in again on the device to get a new one."}
	}
	switch row.Status {
	case devicecode.StatusPending:
		return row, nil
	case devicecode.StatusApproved:
		return nil, &webtempl.Flash{Kind: "success", Message: "That code was already approved. The device should be signed in."}
	case devicecode.StatusDenied:
		return nil, &webtempl.Flash{Kind: "error", Message: "That code was already denied. Start again on the device if you meant to allow it."}
	case devicecode.StatusRedeemed:
		return nil, &webtempl.Flash{Kind: "success", Message: "That code has already been used to sign a device in."}
	default:
		return nil, &webtempl.Flash{Kind: "error", Message: "That code is in an unexpected state. Start the sign-in again on the device."}
	}
}

// projectPending builds the approval panel's evidence block.
func (s *Server) projectPending(r *http.Request, row *devicecode.Row, typed string) *webtempl.DevicePending {
	name := row.ClientId
	if s.deviceFlow != nil && s.deviceFlow.ClientName != nil {
		if resolved := strings.TrimSpace(s.deviceFlow.ClientName(r.Context(), row.ClientId)); resolved != "" {
			name = resolved
		}
	}
	// Echo the CANONICAL display form rather than what the user typed:
	// the panel is asking them to compare this against the device's
	// screen, so it has to look like the device's screen.
	display := devicecode.FormatUserCode(devicecode.CanonicalizeUserCode(typed))
	ua := strings.TrimSpace(row.UserAgent)
	if ua == "" {
		ua = "(not reported)"
	}
	ip := strings.TrimSpace(row.SourceIP)
	if ip == "" {
		ip = "(unknown)"
	}
	return &webtempl.DevicePending{
		UserCode:    display,
		ClientName:  name,
		ClientId:    row.ClientId,
		SourceIP:    ip,
		UserAgent:   ua,
		RequestedAt: formatStamp(row.CreatedAt),
		ExpiresAt:   formatStamp(row.ExpiresAt),
	}
}

func formatStamp(t time.Time) string {
	if t.IsZero() {
		return "(unknown)"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

func (s *Server) renderDevice(w http.ResponseWriter, r *http.Request, data webtempl.DeviceData) {
	s.render(w, r, "device", webtempl.Device(data))
}

// requireUserForDevice is /device's auth gate. It mirrors requireUser
// (me_tokens.go) with one deliberate difference: the bounce stashes the
// FULL request URI, query string included, so a visitor who arrived on
// verification_uri_complete comes back with their code still attached
// instead of landing on an empty form.
func (s *Server) requireUserForDevice(w http.ResponseWriter, r *http.Request) (*identity.AccessTokenClaims, bool) {
	if s.deviceFlow == nil || s.deviceFlow.Issuer == nil || s.deviceFlow.Adapter == nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Device sign-in is temporarily unavailable.")
		return nil, false
	}
	// The per-IP limit is checked BEFORE the session check, and on both
	// verbs, because both are reachable without one: an unauthenticated
	// caller can still burn budget probing this endpoint, and the
	// page's whole job is to answer questions about a 40-bit code.
	if allowed, retryAfter := s.verifyLimiter().Allow(clientIP(r)); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.renderError(w, r, http.StatusTooManyRequests,
			"Too many attempts from this address. Wait a little and try again.")
		return nil, false
	}
	raw := extractUserToken(r)
	if raw == "" {
		s.bounceToLoginForDevice(w, r)
		return nil, false
	}
	claims, err := s.deviceFlow.Issuer.VerifyAccessToken(raw, time.Now().UTC())
	if err != nil || claims == nil {
		s.bounceToLoginForDevice(w, r)
		return nil, false
	}
	return claims, true
}

// bounceToLoginForDevice sends a signed-out visitor through the normal
// magic-link login and arranges for them to come back here.
//
// A GET can be resumed exactly; a POST cannot (the browser will issue a
// GET on the way back), so the destination is always rebuilt as a GET
// on /device carrying whichever code we know about.
func (s *Server) bounceToLoginForDevice(w http.ResponseWriter, r *http.Request) {
	dest := "/device"
	if code := deviceCodeFromRequest(r); code != "" {
		dest += "?user_code=" + url.QueryEscape(code)
	}
	identity.SetPostLoginRedirect(w, dest, s.cookieSecure())
	http.Redirect(w, r,
		"/login?flash=Sign+in+to+approve+the+device&flash_kind=info&return_to="+url.QueryEscape(dest),
		http.StatusSeeOther)
}

// deviceCodeFromRequest pulls the user_code out of either carrier
// (query on the GET, form field on the POST) and normalizes it. An
// unparseable value yields "" so the bounce target stays clean.
func deviceCodeFromRequest(r *http.Request) string {
	raw := strings.TrimSpace(r.URL.Query().Get("user_code"))
	if raw == "" && r.Method == http.MethodPost {
		// ParseForm may not have run yet on the auth-gate path.
		if err := r.ParseForm(); err == nil {
			raw = strings.TrimSpace(r.PostForm.Get("user_code"))
		}
	}
	canonical := devicecode.CanonicalizeUserCode(raw)
	if canonical == "" {
		return ""
	}
	return devicecode.FormatUserCode(canonical)
}

// deviceAudit forwards a verification-page event, attaching request
// metadata. Safe when the flow or its logger is unwired.
func (s *Server) deviceAudit(r *http.Request, ev identity.AuditEvent) {
	if s == nil || s.deviceFlow == nil || s.deviceFlow.Audit == nil {
		return
	}
	ev.Category = identity.AuditCategoryAuth
	if r != nil {
		if ev.SourceIP == "" {
			ev.SourceIP = clientIP(r)
		}
		if ev.UserAgent == "" {
			ev.UserAgent = r.Header.Get("User-Agent")
		}
	}
	s.deviceFlow.Audit.Log(r.Context(), ev)
}
