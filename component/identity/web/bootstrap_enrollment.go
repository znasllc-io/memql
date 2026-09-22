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
	if s.Store == nil || s.Cfg.LocalPasskeyOnly() || s.IssueMagicLink == nil {
		s.renderError(w, r, http.StatusForbidden, "Resume this installation with your passkey.")
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
	pending, err := s.Store.ReservedBootstrap(r.Context())
	if err != nil || pending == nil || pending.Complete || !strings.EqualFold(strings.TrimSpace(r.Form.Get("email")), pending.Settings.BootstrapEmail) {
		s.renderError(w, r, http.StatusBadRequest, "Use the email address originally verified for this setup.")
		return
	}
	res, err := s.IssueMagicLink(r.Context(), IssueMagicLinkInput{Email: pending.Settings.BootstrapEmail, Bootstrap: true, AdminSession: true, SourceIP: clientIP(r), UserAgent: r.UserAgent()})
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
