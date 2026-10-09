package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

func (s *Server) setBootstrapCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: identity.BootstrapCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode, MaxAge: 24 * 60 * 60})
}

func (s *Server) bootstrapPage(w http.ResponseWriter, r *http.Request) bool {
	cookie, err := r.Cookie(identity.BootstrapCookie)
	if err != nil || s.Store == nil {
		return false
	}
	row, err := s.Store.BootstrapEnrollment(r.Context(), cookie.Value, s.Cfg)
	if err != nil || row == nil || row.Complete {
		return false
	}
	writeNative(w, map[string]any{"page": "setup_passkey", "csrf": CSRFTokenFromRequest(r), "data": map[string]any{
		"Local": row.Local, "Email": row.Settings.BootstrapEmail, "EnrollmentToken": cookie.Value, "HasProof": len(row.Proof) > 0,
	}})
	return true
}

func (s *Server) handleBootstrapPasskey(w http.ResponseWriter, r *http.Request) {
	if !nativeRequest(r) {
		http.Redirect(w, r, s.nativeOrigin(r)+"/identity/auth/setup/passkey", http.StatusSeeOther)
		return
	}
	if s.bootstrapPage(w, r) {
		return
	}
	s.renderError(w, r, http.StatusUnauthorized, "Your setup enrollment has expired. Return to setup to resume with your passkey or verify your owner email again.")
}

func (s *Server) handleBootstrapResume(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		s.renderError(w, r, http.StatusForbidden, "Resume this installation with your passkey.")
		return
	}
	if allowed, _ := s.enrolLimiter().Allow(clientIP(r)); !allowed {
		s.renderError(w, r, http.StatusTooManyRequests, "Too many setup attempts. Please try again later.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form.")
		return
	}
	release, err := s.Store.AcquireBootstrapGate(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Setup is temporarily unavailable.")
		return
	}
	defer release()
	pending, err := s.Store.PendingOwnerSetup(r.Context(), s.Cfg)
	if err != nil || pending == nil || pending.Complete || !strings.EqualFold(strings.TrimSpace(r.Form.Get("email")), pending.Settings.BootstrapEmail) {
		s.renderError(w, r, http.StatusBadRequest, "Enter the owner email used during installation. If you already registered a passkey, use it to sign in.")
		return
	}
	if s.Cfg.LocalPasskeyOnly() {
		token, err := s.Store.ResumeOwnerSetupLocked(r.Context(), s.Cfg, r.Form.Get("email"), s.bootstrapOAuth(r), false)
		if err != nil {
			s.renderError(w, r, http.StatusConflict, "Setup cannot be resumed by email. Use your existing passkey.")
			return
		}
		s.setBootstrapCookie(w, token)
		http.Redirect(w, r, "/auth/setup/passkey", http.StatusSeeOther)
		return
	}
	if s.IssueMagicLink == nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Email verification is unavailable. Please retry.")
		return
	}
	// Persist configured details before issuing a hosted verification link.
	if err := s.Store.PersistClusterSettings(r.Context(), pending.Settings); err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Setup is temporarily unavailable.")
		return
	}
	oauth := s.bootstrapOAuth(r)
	res, err := s.IssueMagicLink(r.Context(), IssueMagicLinkInput{Email: pending.Settings.BootstrapEmail, Bootstrap: true, AdminSession: oauth == nil, ClientId: oauth["client_id"], RedirectURI: oauth["redirect_uri"], State: oauth["state"], CodeChallenge: oauth["code_challenge"], CodeChallengeMethod: oauth["code_challenge_method"], SourceIP: clientIP(r), UserAgent: r.UserAgent()})
	if err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "The verification link could not be sent. Please retry.")
		return
	}
	s.setMagicLinkCookie(w, res.BindingNonce, time.Until(res.ExpiresAt))
	http.Redirect(w, r, checkEmailURL(pending.Settings.BootstrapEmail, res.RequestId), http.StatusSeeOther)
}

func bootstrapSettings(in ClusterSettingsInput) identity.ClusterSettingsRow {
	return identity.ClusterSettingsRow{ClusterDomain: in.Domain, BrandName: in.BrandName, RegistrationMode: in.RegistrationMode,
		RegistrationDomains: in.RegistrationDomains, InternalDomains: in.InternalDomains, InternalDefaultRole: in.InternalDefaultRole,
		AccessRequestNotifyEmails: in.AccessRequestNotifyEmails, BootstrapEmail: in.OwnerEmail, BootstrapFirstName: in.OwnerFirstName,
		BootstrapLastName: in.OwnerLastName, BootstrapPhone: in.OwnerPhone, BootstrapPrimaryRole: in.OwnerPrimaryRole,
		BootstrapGender: in.OwnerGender, BootstrapBirthdate: in.OwnerBirthdate}
}

func (s *Server) bootstrapOAuth(r *http.Request) map[string]string {
	cid, uri, state, matched := s.pickOAuthCtx(r.Context(), r.Form.Get("client_id"), r.Form.Get("redirect_uri"), r.Form.Get("return_to"), r.Form.Get("state"))
	if !matched || r.Form.Get("code_challenge") == "" {
		return nil
	}
	return map[string]string{"client_id": cid, "redirect_uri": uri, "state": state, "code_challenge": r.Form.Get("code_challenge"), "code_challenge_method": r.Form.Get("code_challenge_method")}
}
