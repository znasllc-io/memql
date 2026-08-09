package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

func nowNano() int64 { return time.Now().UnixNano() }

// handleRoot redirects bare / to /login (or /setup if pre-bootstrap).
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if s.preBootstrap(r) {
		http.Redirect(w, r, withQuery("/setup", r.URL.RawQuery), http.StatusFound)
		return
	}
	http.Redirect(w, r, withQuery("/login", r.URL.RawQuery), http.StatusFound)
}

// withQuery appends a non-empty raw query string to a base path. Used
// when one handler redirects to another and we want to preserve the
// caller's OAuth context (return_to / client_id / redirect_uri /
// state) through the hop — e.g. /login → /setup on a pre-bootstrap
// cluster, so the wizard can pass it on to the magic-link issuer.
func withQuery(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	return path + "?" + rawQuery
}

// handleLoginGet renders the email-first login form. Pre-bootstrap
// (no users exist), redirects to /setup so the operator runs the
// wizard before strangers can sign in.
//
// The login UX is intentionally stage-based instead of mode-aware:
// the page always opens with an email field. The submit handler
// looks up whether the email belongs to an existing user and
// branches per registration-mode from there. This way an existing
// user signing in to a waitlist-mode cluster doesn't get the
// "you need an invite" form — they just get their magic link.
func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if s.preBootstrap(r) {
		http.Redirect(w, r, withQuery("/setup", r.URL.RawQuery), http.StatusFound)
		return
	}
	settings := s.snapshotSettings(r)
	data := webtempl.LoginData{
		// passkey-login.js reveals the "Sign in with a passkey" control
		// the login template renders hidden (memql#3407). Shipped on
		// every /login render rather than conditionally: the control is
		// what the script looks for, so a page without one costs a
		// cached 3 KB and does nothing.
		Layout:             s.LayoutData(r, "Sign in", false, nil, []string{s.assetURL("/static/passkey-login.js")}),
		Mode:               string(settings.RegistrationMode),
		Stage:              "email",
		AllowedDomainsHint: settings.RegistrationDomains,
		PrefillEmail:       strings.TrimSpace(r.URL.Query().Get("email")),
		ReturnTo:           strings.TrimSpace(r.URL.Query().Get("return_to")),
		// OAuth-canonical query params from the relying-party SPA.
		// These ride through the form as hidden fields and resurface
		// on POST so handleLoginPost can match the (client_id,
		// redirect_uri) pair against registered clients.
		ClientID:    strings.TrimSpace(r.URL.Query().Get("client_id")),
		RedirectURI: strings.TrimSpace(r.URL.Query().Get("redirect_uri")),
		OAuthState:  strings.TrimSpace(r.URL.Query().Get("state")),
	}
	if msg := strings.TrimSpace(r.URL.Query().Get("flash")); msg != "" {
		kind := strings.TrimSpace(r.URL.Query().Get("flash_kind"))
		if kind == "" {
			kind = "info"
		}
		data.Flash = &webtempl.Flash{Kind: kind, Message: msg}
	}
	s.render(w, r, "login", webtempl.Login(data))
}

// preBootstrap returns true when the cluster has not been bootstrapped
// AND has never been claimed. Used to gate /login (UX redirect to
// /setup) and /login POST (friendly error). The auth-API gate lives
// inside the magic-link Issuer so both code paths share the same check.
//
// Two independent signals, because one is not enough (memql#3415): the
// CountUsers callback is wired to clusterSettings.bootstrappedAt, and a
// stray write that blanked that one field made every / and /login hit
// redirect to /setup on a cluster with hundreds of users -- nobody could
// sign in. ClusterClaimed ("an owner user exists") is not something a
// stray row produces, so it holds the redirect back.
//
// On error from either callback, returns false: "cannot determine" must
// not bounce real users into the first-run wizard.
func (s *Server) preBootstrap(r *http.Request) bool {
	if s == nil || s.CountUsers == nil {
		return false
	}
	n, err := s.CountUsers(r.Context())
	if err != nil || n != 0 {
		return false
	}
	if s.ClusterClaimed != nil {
		claimed, err := s.ClusterClaimed(r.Context())
		if err != nil || claimed {
			return false
		}
	}
	return true
}

// setupSealed reports whether the first-run ownership wizard must be
// refused. It is the gate on BOTH /setup handlers, and it is deliberately
// the most conservative check in the web package: /setup mints the
// cluster owner, so anything short of positive proof that this cluster is
// unclaimed is a refusal (memql#3415).
//
// Sealed when ANY of the following holds:
//
//   - the bootstrap signal (CountUsers, wired to
//     clusterSettings.bootstrappedAt) reports a bootstrapped cluster;
//   - the claim signal (ClusterClaimed, wired to "an owner user exists")
//     reports a claimed cluster;
//   - EITHER signal returns an error -- the previous code swallowed those
//     (`if n, err := CountUsers(); err == nil && n > 0`) and served the
//     wizard, so a transient DB failure opened the ownership surface just
//     as effectively as the blanked field did;
//   - the claim signal is not wired at all. A binary that forgets it must
//     not silently degrade to the single-signal behaviour this issue is
//     about. There is exactly one wiring site (app/integrations_identity.go),
//     and a bricked-but-loud /setup is recoverable in a way an exposed one
//     is not.
//
// A genuinely fresh cluster -- both signals readable, both negative --
// is the only state that serves the wizard.
func (s *Server) setupSealed(r *http.Request) bool {
	if s == nil {
		return true
	}
	if s.CountUsers != nil {
		n, err := s.CountUsers(r.Context())
		if err != nil {
			s.logSetupSeal(r, "bootstrap signal unreadable", err)
			return true
		}
		if n > 0 {
			return true
		}
	}
	if s.ClusterClaimed == nil {
		s.logSetupSeal(r, "cluster-claim signal not wired", nil)
		return true
	}
	claimed, err := s.ClusterClaimed(r.Context())
	if err != nil {
		s.logSetupSeal(r, "cluster-claim signal unreadable", err)
		return true
	}
	return claimed
}

// logSetupSeal records a refusal that was NOT a plain "already
// bootstrapped". Those two cases (unreadable / unwired signal) seal a
// wizard that might legitimately be needed, so they must not be silent.
func (s *Server) logSetupSeal(r *http.Request, reason string, err error) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn("identity/web: refusing /setup -- cannot prove this cluster is unclaimed",
		"reason", reason,
		"error", err,
		"path", r.URL.Path,
		"component", "identity")
}

// handleLoginPost is the unified email-first dispatcher. The form
// variants (`form=email|waitlist|invite`) drive the state machine:
//
//	form=email     initial email submission. The handler looks up
//	               whether the email belongs to an existing user and
//	               picks one of: send magic link / re-render in
//	               waitlist-signup stage / re-render in needs-invite
//	               stage. Existing users always get a magic link
//	               regardless of registration mode.
//	form=waitlist  step-2 waitlist signup. Email + name + context.
//	               Posted as IsAccessRequest, queues a waitlist row.
//	form=invite    invitation-token submission. Skips the
//	               registration-mode gate via invitationId.
func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if s.preBootstrap(r) {
		// Belt-and-suspenders: the Issuer also enforces this. The web
		// pre-check just gives a nicer UX (redirect-to-setup) instead of
		// rendering a generic error page.
		http.Redirect(w, r, withQuery("/setup", r.URL.RawQuery), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "We couldn't read your form submission. Please try again.")
		return
	}
	formKind := strings.TrimSpace(r.PostForm.Get("form"))
	email := strings.TrimSpace(r.PostForm.Get("email"))
	returnTo := strings.TrimSpace(r.PostForm.Get("return_to"))
	formClientId := strings.TrimSpace(r.PostForm.Get("client_id"))
	formRedirectURI := strings.TrimSpace(r.PostForm.Get("redirect_uri"))
	formOAuthState := strings.TrimSpace(r.PostForm.Get("state"))
	// PKCE challenge carried through the /authorize browser flow's hidden
	// fields. Threaded onto the issuer so /auth/complete mints a
	// PKCE-bound auth code. Only meaningful when an OAuth client matches.
	formCodeChallenge := strings.TrimSpace(r.PostForm.Get("code_challenge"))
	formCodeChallengeMethod := strings.TrimSpace(r.PostForm.Get("code_challenge_method"))
	if s.IssueMagicLink == nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable. Please try again in a moment.")
		return
	}

	clientId, redirectURI, state, clientMatched := s.pickOAuthCtx(r.Context(), formClientId, formRedirectURI, returnTo, formOAuthState)
	in := IssueMagicLinkInput{
		Email:        email,
		ClientId:     clientId,
		RedirectURI:  redirectURI,
		State:        state,
		SourceIP:     clientIP(r),
		UserAgent:    r.Header.Get("User-Agent"),
		AdminSession: !clientMatched,
	}
	// Only carry the PKCE challenge when the relying-party client matched;
	// the admin-session path never mints an OAuth code.
	if clientMatched {
		in.CodeChallenge = formCodeChallenge
		in.CodeChallengeMethod = formCodeChallengeMethod
	}

	switch formKind {
	case "invite":
		in.InvitationId = strings.TrimSpace(r.PostForm.Get("invitation"))
		if in.InvitationId == "" {
			s.renderError(w, r, http.StatusBadRequest, "Please paste your invitation token.")
			return
		}
	case "waitlist":
		in.IsAccessRequest = true
		in.WaitlistName = strings.TrimSpace(r.PostForm.Get("name"))
		in.WaitlistContext = strings.TrimSpace(r.PostForm.Get("additional_context"))
		if email == "" {
			s.renderError(w, r, http.StatusBadRequest, "Please give us your email so we can let you know.")
			return
		}
	default:
		// form=email (or empty for back-compat). Branch on existing-user
		// vs registration-mode before issuing.
		if email == "" {
			s.renderError(w, r, http.StatusBadRequest, "Please enter your email address.")
			return
		}
		next, existingUser := s.routeLoginEmail(r, email, returnTo)
		if next != nil {
			s.render(w, r, "login", webtempl.Login(*next))
			return
		}
		// routeLoginEmail returned nil → fall through to IssueMagicLink.
		// existingUser=true means the user-exists check matched; we tell
		// the issuer to skip the registration-mode gate (a returning user
		// always gets a magic link, even on a waitlist-mode cluster).
		in.ExistingUser = existingUser
	}

	if err := s.IssueMagicLink(r.Context(), in); err != nil {
		s.Logger.Warn("identity-web: issue magic link failed",
			"error", err, "form", formKind, "remote", clientIP(r))
		// Generic message — don't leak per-mode rejection reason.
		s.renderError(w, r, http.StatusBadRequest, "We couldn't process that request. Please double-check your email and try again.")
		return
	}

	http.Redirect(w, r, "/check-email?email="+strings.ReplaceAll(email, "+", "%2B"), http.StatusSeeOther)
}

// routeLoginEmail decides what to do with a step-1 email submission.
// Returns (nil, existing) to fall through to IssueMagicLink:
//
//	existing=true  -- caller MUST set IssueInput.ExistingUser=true so
//	                  the issuer skips the registration-mode gate.
//	existing=false -- new user that the registration mode permits
//	                  on the spot (open mode, or domain_restricted
//	                  with an allowed domain).
//
// Returns a non-nil LoginData when the page should be re-rendered
// in a different stage:
//
//	stage=waitlist_signup  new user, mode=waitlist (collect waitlist
//	                       fields) OR mode=domain_restricted with a
//	                       denied domain (offer the waitlist as a
//	                       fallback).
//	stage=needs_invite     new user, mode=invite_only.
//
// Anti-enumeration: the page never tells the operator "this email
// is registered" vs "this email is new". Existing users are routed
// through the same magic-link path regardless of mode, which from
// outside the server is indistinguishable from a successful
// registration in open mode.
func (s *Server) routeLoginEmail(r *http.Request, email, returnTo string) (*webtempl.LoginData, bool) {
	settings := s.snapshotSettings(r)
	mode := string(settings.RegistrationMode)

	// Existing-user lookup is best-effort. On error, fall through as
	// "not existing" — the issuer's mode gate is the authoritative
	// check anyway.
	existing := false
	if s.UserExistsByEmail != nil {
		if ok, err := s.UserExistsByEmail(r.Context(), email); err == nil {
			existing = ok
		} else if s.Logger != nil {
			s.Logger.Warn("identity-web: user-exists lookup failed",
				"error", err, "remote", clientIP(r))
		}
	}

	if existing {
		// Always send a magic link to a known user, regardless of mode.
		return nil, true
	}

	switch mode {
	case "open":
		return nil, false
	case "domain_restricted":
		if emailMatchesAllowedDomain(email, s.Cfg.RegistrationDomains) {
			return nil, false
		}
		return s.renderLoginStage(r, "waitlist_signup", email, returnTo,
			"Your email isn't in this cluster's allowed-domain list. You can join the waitlist instead — the operator will follow up."), false
	case "invite_only":
		return s.renderLoginStage(r, "needs_invite", email, returnTo,
			"This cluster is invite-only. If you have an invitation token, paste it below — otherwise, ask the operator to send you one."), false
	case "waitlist":
		return s.renderLoginStage(r, "waitlist_signup", email, returnTo, ""), false
	default:
		// Unknown mode → fall through; the issuer's gate is the
		// authoritative check.
		return nil, false
	}
}

// renderLoginStage builds a LoginData for a non-default stage,
// preserving the email + return-to context the operator already
// typed in step 1.
func (s *Server) renderLoginStage(r *http.Request, stage, email, returnTo, info string) *webtempl.LoginData {
	settings := s.snapshotSettings(r)
	d := webtempl.LoginData{
		Layout:             s.LayoutData(r, "Sign in", false, nil, nil),
		Mode:               string(settings.RegistrationMode),
		Stage:              stage,
		AllowedDomainsHint: settings.RegistrationDomains,
		PrefillEmail:       email,
		ReturnTo:           returnTo,
	}
	if info != "" {
		d.Flash = &webtempl.Flash{Kind: "info", Message: info}
	}
	return &d
}

// emailMatchesAllowedDomain reports whether the supplied email's
// domain is in the cluster's domain_restricted allowlist. Empty
// allowlist returns false (treat as denied so the operator gets
// the waitlist path instead of a silent IssueMagicLink success).
func emailMatchesAllowedDomain(email string, allowed []string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return false
	}
	got := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if got == "" {
		return false
	}
	for _, d := range allowed {
		want := strings.ToLower(strings.TrimSpace(d))
		if want == got {
			return true
		}
	}
	return false
}

func (s *Server) handleCheckEmail(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	expiresIn := "10 minutes"
	if s.Cfg.MagicLinkTTL > 0 {
		expiresIn = humanDuration(s.Cfg.MagicLinkTTL)
	}
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	if action == "" {
		action = "magic_link_sent"
	}
	data := webtempl.CheckEmailData{
		Layout:    s.LayoutData(r, "Check your email", false, nil, nil),
		Email:     email,
		ExpiresIn: expiresIn,
		Action:    action,
	}
	s.render(w, r, "check_email", webtempl.CheckEmail(data))
}

func (s *Server) handleLogoutComplete(w http.ResponseWriter, r *http.Request) {
	layout := s.LayoutData(r, "Signed out", false, nil, nil)
	returnTo := strings.TrimSpace(r.URL.Query().Get("return_to"))
	label := ""
	if returnTo != "" {
		label = layout.BrandName
	}
	data := webtempl.LogoutCompleteData{
		Layout:        layout,
		ReturnTo:      returnTo,
		ReturnToLabel: label,
	}
	s.render(w, r, "logout_complete", webtempl.LogoutComplete(data))
}

// humanDuration formats a Duration as a short human string suitable
// for "the link expires in X" copy.
func humanDuration(d time.Duration) string {
	if d >= time.Hour {
		hours := int(d / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	if d >= time.Minute {
		mins := int(d / time.Minute)
		if mins == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", mins)
	}
	secs := int(d / time.Second)
	if secs == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", secs)
}

func (s *Server) handleError(w http.ResponseWriter, r *http.Request) {
	msg := strings.TrimSpace(r.URL.Query().Get("message"))
	if msg == "" {
		msg = "Something went wrong. Please try again."
	}
	data := webtempl.ErrorData{
		Layout:  s.LayoutData(r, "Error", false, nil, nil),
		Heading: "Something went wrong",
		Message: msg,
		ErrorID: strings.TrimSpace(r.URL.Query().Get("errorId")),
	}
	s.render(w, r, "error", webtempl.Error(data))
}

// renderError writes a 400/500-class response with the error template.
// Single-purpose helper used inside other handlers.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.WriteHeader(status)
	data := webtempl.ErrorData{
		Layout:  s.LayoutData(r, "Error", false, nil, nil),
		Heading: "Something went wrong",
		Message: msg,
	}
	s.render(w, r, "error", webtempl.Error(data))
}

// handleSetupGet renders the first-run wizard. 404s unless the cluster
// is provably unclaimed -- see setupSealed (memql#3415).
func (s *Server) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	if s.setupSealed(r) {
		http.NotFound(w, r)
		return
	}
	// Wizard interactions (disabled-button + show/hide of conditional
	// rows) are plain vanilla JS in setup-wizard.js. The page does
	// NOT depend on Stimulus — the previous Stimulus iteration kept
	// failing to bootstrap in some browsers and the cost of getting
	// it wrong here (perma-disabled submit button) is high. Stimulus
	// stays loaded globally by the layout for future pages where the
	// controller-style organization actually pays off.
	extra := []string{s.assetURL("/static/setup-wizard.js")}
	// Wizard prefill source: every IDENTITY_BOOTSTRAP_* env var
	// the operator set at deploy time. When all required fields
	// are set, the auto-bootstrap path (in app/integrations_identity)
	// finishes setup before the wizard ever renders -- so by the
	// time someone visits /setup interactively, they're filling in
	// what env didn't cover.
	bs := s.Cfg.Bootstrap
	mode := strings.TrimSpace(bs.RegistrationMode)
	if mode == "" {
		mode = string(s.Cfg.RegistrationMode)
	}
	if mode == "" {
		// Waitlist is the conservative default — strangers can't grab
		// accounts on a freshly bootstrapped cluster, the operator gets
		// to review every request from the admin UI. Operator can
		// switch to open / domain_restricted / invite_only any time
		// from /admin/settings.
		mode = "waitlist"
	}
	internalDomains := strings.Join(bs.InternalDomains, ", ")
	if internalDomains == "" {
		internalDomains = strings.Join(s.Cfg.InternalDomains, ", ")
	}
	registrationDomains := strings.Join(bs.RegistrationDomains, ", ")
	if registrationDomains == "" {
		registrationDomains = strings.Join(s.Cfg.RegistrationDomains, ", ")
	}
	data := webtempl.SetupWizardData{
		Layout:                     s.LayoutData(r, "Set up your cluster", false, nil, extra),
		PrefillDomain:              strings.TrimSpace(bs.Domain),
		PrefillOwnerEmail:          strings.TrimSpace(bs.OwnerEmail),
		PrefillOwnerFirstName:      strings.TrimSpace(bs.OwnerFirstName),
		PrefillOwnerLastName:       strings.TrimSpace(bs.OwnerLastName),
		PrefillOwnerPhone:          strings.TrimSpace(bs.OwnerPhone),
		PrefillOwnerPrimaryRole:    strings.TrimSpace(bs.OwnerPrimaryRole),
		PrefillOwnerGender:         strings.TrimSpace(bs.OwnerGender),
		PrefillOwnerBirthdate:      strings.TrimSpace(bs.OwnerBirthdate),
		PrefillOrgName:             strings.TrimSpace(bs.OrgName),
		PrefillInternalDomains:     internalDomains,
		PrefillRegistrationDomains: registrationDomains,
		PrefillNotifyEmails:        strings.Join(bs.NotifyEmails, ", "),
		PrefillMode:                mode,
		// OAuth context threaded from /login (or directly from a
		// relying-party-driven /setup hit). The form re-emits these as
		// hidden fields so /setup POST can call pickOAuthCtx and pass
		// the resolved client into IssueMagicLink — making the magic-
		// link click land at the cockpit's loopback callback instead
		// of /admin/.
		ReturnTo:    strings.TrimSpace(r.URL.Query().Get("return_to")),
		ClientID:    strings.TrimSpace(r.URL.Query().Get("client_id")),
		RedirectURI: strings.TrimSpace(r.URL.Query().Get("redirect_uri")),
		OAuthState:  strings.TrimSpace(r.URL.Query().Get("state")),
	}
	s.render(w, r, "setup_wizard", webtempl.SetupWizard(data))
}

// handleSetupPost validates the wizard submission, persists the
// settings row, and triggers a magic link to the captured owner email.
func (s *Server) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	if s.setupSealed(r) {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "We couldn't read your form submission.")
		return
	}
	in := ClusterSettingsInput{
		Domain:                    strings.TrimSpace(r.PostForm.Get("domain")),
		OwnerEmail:                strings.TrimSpace(r.PostForm.Get("owner_email")),
		OwnerFirstName:            strings.TrimSpace(r.PostForm.Get("owner_first_name")),
		OwnerLastName:             strings.TrimSpace(r.PostForm.Get("owner_last_name")),
		OwnerPhone:                strings.TrimSpace(r.PostForm.Get("owner_phone")),
		OwnerPrimaryRole:          strings.TrimSpace(r.PostForm.Get("owner_primary_role")),
		OwnerGender:               strings.TrimSpace(r.PostForm.Get("owner_gender")),
		OwnerBirthdate:            strings.TrimSpace(r.PostForm.Get("owner_birthdate")),
		BrandName:                 strings.TrimSpace(r.PostForm.Get("brand_name")),
		RegistrationMode:          strings.TrimSpace(r.PostForm.Get("registration_mode")),
		RegistrationDomains:       strings.TrimSpace(r.PostForm.Get("registration_domains")),
		InternalDomains:           strings.TrimSpace(r.PostForm.Get("internal_domains")),
		InternalDefaultRole:       strings.TrimSpace(r.PostForm.Get("internal_default_role")),
		AccessRequestNotifyEmails: strings.TrimSpace(r.PostForm.Get("access_request_notify_emails")),
	}
	if in.Domain == "" {
		s.renderError(w, r, http.StatusBadRequest, "Cluster domain is required.")
		return
	}
	if in.OwnerEmail == "" {
		s.renderError(w, r, http.StatusBadRequest, "Owner email is required.")
		return
	}
	if in.OwnerFirstName == "" || in.OwnerLastName == "" {
		s.renderError(w, r, http.StatusBadRequest, "Owner first name and last name are required.")
		return
	}
	// The system-actor middleware stamped "system:identity-svc"
	// on r.Context() before this handler ran (see web.Mount /
	// identity.SystemActorMiddleware). Downstream mutations
	// (PersistClusterSettings, IssueMagicLink) inherit that actor
	// and the engine's actor-required check admits them.
	if s.PersistClusterSettings != nil {
		if err := s.PersistClusterSettings(r.Context(), in); err != nil {
			s.Logger.Warn("identity-web: persist cluster settings failed", "error", err)
			s.renderError(w, r, http.StatusInternalServerError, "We couldn't save those settings. Please try again.")
			return
		}
	}
	// Trigger a magic link to the owner email so they verify ownership
	// before being granted the cluster-owner role. The actual user
	// row is created by the magic-link verifier's first-user-is-owner
	// path when the operator clicks the emailed link.
	//
	// Bootstrap=true bypasses the registration-mode gate (the operator
	// running setup hasn't been able to add themselves to any allow-list
	// yet) and the bootstrap-state gate (clusterSettings was just written
	// above; the issuer would race its own stamp otherwise).
	if s.IssueMagicLink != nil {
		// Wizard owner-mint. Two callers reach here:
		//   1. Admin web /setup with no relying-party in scope --
		//      AdminSession=true, the click lands in /admin/.
		//   2. A relying-party-initiated /setup (e.g. memql-cockpit
		//      hit /login on a pre-bootstrap cluster, which redirected
		//      to /setup with return_to preserved). The wizard re-
		//      emits the OAuth context as hidden fields; pickOAuthCtx
		//      matches return_to against registered clients. When a
		//      client matches we issue the magic link with the OAuth
		//      callback in scope (AdminSession=false), so /auth/complete
		//      bounces the user back to the cockpit's loopback callback
		//      with an auth code -- letting the cockpit complete its
		//      login automatically, no second "press L" round.
		// Bootstrap=true in BOTH cases -- it's the trust marker that
		// bypasses the registration-mode gate and the "cluster not
		// bootstrapped" gate.
		formReturnTo := strings.TrimSpace(r.PostForm.Get("return_to"))
		formClientId := strings.TrimSpace(r.PostForm.Get("client_id"))
		formRedirectURI := strings.TrimSpace(r.PostForm.Get("redirect_uri"))
		formOAuthState := strings.TrimSpace(r.PostForm.Get("state"))
		clientId, redirectURI, oauthState, clientMatched := s.pickOAuthCtx(r.Context(), formClientId, formRedirectURI, formReturnTo, formOAuthState)
		issue := IssueMagicLinkInput{
			Email:        in.OwnerEmail,
			ClientId:     clientId,
			RedirectURI:  redirectURI,
			State:        oauthState,
			SourceIP:     clientIP(r),
			UserAgent:    r.Header.Get("User-Agent"),
			Bootstrap:    true,
			AdminSession: !clientMatched,
		}
		if !clientMatched {
			// Preserve the pre-existing "setup" state marker on the
			// admin-only path so audit logs / verifier traces still
			// distinguish wizard-issued links from regular admin
			// sign-ins.
			issue.State = "setup"
		}
		if err := s.IssueMagicLink(r.Context(), issue); err != nil && s.Logger != nil {
			s.Logger.Warn("identity-web: wizard issue magic link failed",
				"error", err, "email", in.OwnerEmail)
		}
	}
	http.Redirect(w, r, "/check-email?email="+strings.ReplaceAll(in.OwnerEmail, "+", "%2B"), http.StatusSeeOther)
}

// handleLegalTOS renders the TOS markdown wrapped in the legal_view
// template. Markdown rendering is intentionally minimal — we wrap the
// body in <pre> for now, which preserves headings and paragraphs in a
// readable monospace block. A proper markdown renderer can land later
// without changing the storage format or override path.
func (s *Server) handleLegalTOS(w http.ResponseWriter, r *http.Request) {
	s.renderLegal(w, r, "Terms of Service", s.tosBytes, s.tosVersion)
}

func (s *Server) handleLegalPrivacy(w http.ResponseWriter, r *http.Request) {
	s.renderLegal(w, r, "Privacy Notice", s.privacyBytes, s.privacyVersion)
}

func (s *Server) renderLegal(w http.ResponseWriter, r *http.Request, title string, body []byte, version string) {
	data := webtempl.LegalViewData{
		Layout:   s.LayoutData(r, title, false, nil, nil),
		DocTitle: title,
		Heading:  title,
		Body:     stripFrontMatter(string(body)),
		Version:  version,
	}
	s.render(w, r, "legal_view", webtempl.LegalView(data))
}

// stripFrontMatter removes the leading `---\n...\n---\n` block so the
// rendered page only shows the document body.
func stripFrontMatter(src string) string {
	if !strings.HasPrefix(src, "---") {
		return src
	}
	end := strings.Index(src[3:], "---")
	if end < 0 {
		return src
	}
	rest := src[3+end+3:]
	return strings.TrimLeft(rest, "\r\n")
}

// meNavLinks is the standard /me/* nav rendered at the top of every
// /me/* shell page. Mirrors the sidebar in the templ components.
func meNavLinks() []webtempl.NavLink {
	return []webtempl.NavLink{
		{Href: "/me/", Label: "Dashboard"},
		{Href: "/me/settings", Label: "Settings"},
		{Href: "/me/devices", Label: "Devices"},
		{Href: "/me/export", Label: "Export"},
	}
}

// handleMeDashboard renders /me/. The page is a thin shell — app.js
// hydrates the overview panel by fetching account data through the
// SPA refresh flow.
func (s *Server) handleMeDashboard(w http.ResponseWriter, r *http.Request) {
	extra := []string{s.assetURL("/static/me-dashboard.js")}
	data := webtempl.MeDashboardData{
		Layout: s.LayoutData(r, "Dashboard", true, meNavLinks(), extra),
	}
	s.render(w, r, "me/dashboard", webtempl.MeDashboard(data))
}

func (s *Server) handleMeSettings(w http.ResponseWriter, r *http.Request) {
	extra := []string{s.assetURL("/static/me-settings.js")}
	data := webtempl.MeSettingsData{
		Layout:               s.LayoutData(r, "Settings", true, meNavLinks(), extra),
		DeletionCooldownDays: int(s.Cfg.DeletionCooldown / (24 * time.Hour)),
	}
	s.render(w, r, "me/settings", webtempl.MeSettings(data))
}

// handleMeDevices renders /me/devices.
//
// Two shapes, decided by whether the passkey adapter is wired
// (memql#3409). With it, the page carries per-user credential rows and is
// auth-gated server-side like /me/tokens. Without it -- a binary with no
// engine -- it stays the plain client-hydrated sessions shell, because
// gating a page that has nothing per-user on it would only cost a
// redirect.
func (s *Server) handleMeDevices(w http.ResponseWriter, r *http.Request) {
	if s.passkeysWired() {
		s.handleMeDevicesPasskeys(w, r)
		return
	}
	data := webtempl.MeDevicesData{
		Layout: s.LayoutData(r, "Devices", true, meNavLinks(), nil),
	}
	s.render(w, r, "me/devices", webtempl.MeDevices(data))
}

func (s *Server) handleMeExport(w http.ResponseWriter, r *http.Request) {
	data := webtempl.MeExportData{
		Layout:         s.LayoutData(r, "Export your data", true, meNavLinks(), nil),
		RateLimitHours: int(s.Cfg.DataExportRateLimit / time.Hour),
	}
	s.render(w, r, "me/export", webtempl.MeExport(data))
}

func (s *Server) handleMeDeletionPending(w http.ResponseWriter, r *http.Request) {
	data := webtempl.MeDeletionPendingData{
		Layout:               s.LayoutData(r, "Deletion scheduled", true, meNavLinks(), nil),
		DeletionCooldownDays: int(s.Cfg.DeletionCooldown / (24 * time.Hour)),
		// ScheduledDeletionDate is populated by the SPA shell in v1
		// — the page renders with an empty date because the actual
		// scheduled-deletion record is fetched client-side via the
		// existing /me/* hydration flow. Future work can render this
		// server-side once the handler grows access to the user row.
	}
	s.render(w, r, "me/deletion_pending", webtempl.MeDeletionPending(data))
}

// pickOAuthCtx resolves the relying-party context for a /login
// submission. Returns (clientId, redirectURI, state, matched):
//
//	matched=true   returnTo matched a registered client's redirect URI;
//	               that client + URI are returned. The magic link will
//	               bounce back to the client app on consume.
//	matched=false  returnTo was empty or didn't match any registered
//	               client. clientId + redirectURI are empty; the
//	               caller should mark the request as AdminSession so
//	               the verifier short-circuits to /admin/ instead of
//	               silently picking an arbitrary client.
//
// state is always populated (random if not supplied) so OAuth callers
// who DO match a client get CSRF protection, and the row is keyed
// distinctly across attempts.
// pickOAuthCtx resolves the relying-party OAuth context for a /login
// flow. Inputs:
//
//	clientId + redirectURI: OAuth-canonical query params the SPA
//	    posted as hidden form fields (preferred). When both are
//	    present and the pair is a registered client, that's the
//	    match.
//
//	fallbackReturnTo: legacy callers sometimes pass return_to set
//	    to the OAuth redirect URI. We try each registered client's
//	    redirect URIs against return_to as a fallback.
//
//	spState: OAuth state parameter the SPA generated and stored
//	    locally for CSRF validation. RFC 6749 §10.12 says the
//	    state value MUST be returned to the client unmodified --
//	    so when the SPA supplies one, we thread it through. Only
//	    when absent do we generate our own (covers the legacy
//	    in-product /login revisit case where there's no SPA).
//
// The (clientId, redirectURI, state) returned are what eventually
// land on the magic-link row's oauthCtx; /auth/complete uses them
// to build the bounce URL after the user clicks the link.
//
// matched=false means no relying party in scope; the caller should
// mark the flow as AdminSession so the magic link lands in /admin/.
func (s *Server) pickOAuthCtx(ctx context.Context, clientId, redirectURI, fallbackReturnTo, spState string) (matchedClientId, matchedRedirectURI, state string, matched bool) {
	state = strings.TrimSpace(spState)
	if state == "" {
		state = randomState()
	}
	// No relying parties at all (no static config AND no DB store for
	// dynamically-registered clients) -> nothing to match.
	if len(s.Cfg.RegisteredClients) == 0 && s.Store == nil {
		return "", "", state, false
	}

	// OAuth-canonical path: SPA supplied client_id + redirect_uri. Resolve
	// via the DB-aware helper (static MEMQL_IDENTITY_REGISTERED_CLIENTS + the
	// dynamically-registered v1:identity:oauthClient store, #1573/#1586) so a
	// DCR client (e.g. a claude.ai custom connector) matches here just as it
	// does at /authorize -- otherwise the magic link is issued with no OAuth
	// context and /auth/complete falls back to a plain session login.
	// ClientAllowsRedirectURI handles exact match + the RFC 8252 loopback rule.
	if clientId != "" && redirectURI != "" {
		if identity.ClientAllowsRedirectURI(ctx, s.Cfg, s.Store, clientId, redirectURI) {
			return clientId, redirectURI, state, true
		}
	}

	// Legacy fallback: return_to set to the OAuth redirect URI.
	// Walk every registered client looking for a match.
	if fallbackReturnTo != "" {
		for _, c := range s.Cfg.RegisteredClients {
			if s.Cfg.AllowsRedirectURI(c.ClientId, fallbackReturnTo) {
				return c.ClientId, fallbackReturnTo, state, true
			}
		}
	}

	return "", "", state, false
}

// randomState returns a short random string for the OAuth state param.
// Used only when the form doesn't supply one. Not a security primitive
// — the security-relevant state validation lives on the magic-link
// completion path which checks against the persisted oauthCtx.
func randomState() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	now := nowNano()
	for i := range b {
		now = now*1103515245 + 12345
		b[i] = charset[int(uint64(now)>>16)%len(charset)]
	}
	return string(b)
}

// EncodeJSON helper used by future endpoints (kept here because the
// admin-UI Phase 6 will reuse the helpers). Suppresses HTML escaping.
func EncodeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		_ = err
	}
}

// clientIP pulls the originating IP off the request, honoring common
// proxy headers when set. Best-effort.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if comma := strings.IndexByte(v, ','); comma >= 0 {
			return strings.TrimSpace(v[:comma])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	return r.RemoteAddr
}
