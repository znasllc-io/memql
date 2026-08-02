package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// ---------------------------------------------------------------------------
// Auth-establishment handlers (no requireAdmin gate)
// ---------------------------------------------------------------------------

// handleLoginGet renders the paste-token form. The form is the only
// way to seed an admin session in a vanilla browser today; a future
// commit can wire a "Open admin" button on the SPA that POSTs the
// live access token to /admin/establish.
func (s *AdminServer) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	data := webtempl.AdminLoginData{
		Layout: s.layoutData(r, "Admin sign-in", true),
		Flash:  readFlash(r),
	}
	s.render(w, r, "admin/login", webtempl.AdminLogin(data))
}

// handleEstablish accepts a JWT (form field `token` or Authorization
// header), validates it, requires role=owner|admin, and sets the
// admin cookie. On success: redirects to /admin/. On failure: drops
// the operator back to /admin/login with an error flash.
func (s *AdminServer) handleEstablish(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/login?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	tok := strings.TrimSpace(r.PostForm.Get("token"))
	if tok == "" {
		// Try the Authorization header for API-style consumers.
		tok = extractAdminToken(r)
	}
	if tok == "" {
		http.Redirect(w, r, "/admin/login?flash=Paste+a+JWT+to+continue&flash_kind=error", http.StatusSeeOther)
		return
	}

	claims, err := s.Issuer.VerifyAccessToken(tok, time.Now().UTC())
	if err != nil {
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAdmin,
			Action:        "admin_establish_failed",
			Outcome:       identity.AuditOutcomeFailure,
			FailureReason: err.Error(),
		})
		http.Redirect(w, r, "/admin/login?flash=Token+is+invalid+or+expired&flash_kind=error", http.StatusSeeOther)
		return
	}
	role := strings.ToLower(strings.TrimSpace(claims.Role))
	if role != "owner" && role != "admin" {
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAdmin,
			Action:        "admin_establish_forbidden",
			ActorUserId:   claims.Subject,
			ActorEmail:    claims.Email,
			ActorRole:     claims.Role,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "role_not_admin",
		})
		http.Redirect(w, r, "/admin/login?flash=Token+is+valid+but+lacks+admin+role&flash_kind=error", http.StatusSeeOther)
		return
	}

	setAdminCookie(w, tok, s.Cfg.BaseURL)
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAdmin,
		Action:      "admin_established",
		ActorUserId: claims.Subject,
		ActorEmail:  claims.Email,
		ActorRole:   claims.Role,
		Outcome:     identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

// handleLogout clears the admin cookie and bounces to the login page.
func (s *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if claims := claimsFromRequestToken(r, s.Issuer); claims != nil {
		s.audit(r, identity.AuditEvent{
			Category:    identity.AuditCategoryAdmin,
			Action:      "admin_logout",
			ActorUserId: claims.Subject,
			ActorEmail:  claims.Email,
			ActorRole:   claims.Role,
			Outcome:     identity.AuditOutcomeSuccess,
		})
	}
	clearAdminCookie(w)
	http.Redirect(w, r, "/admin/login?flash=Signed+out&flash_kind=info", http.StatusSeeOther)
}

// claimsFromRequestToken decodes (without re-validating beyond the
// signature check) the JWT carried by the request. Used by logout to
// stamp an audit entry without rejecting the call when the token is
// already expired.
func claimsFromRequestToken(r *http.Request, issuer *identity.JWTIssuer) *identity.AccessTokenClaims {
	tok := extractAdminToken(r)
	if tok == "" || issuer == nil {
		return nil
	}
	claims, err := issuer.VerifyAccessToken(tok, time.Now().UTC())
	if err != nil {
		return nil
	}
	return claims
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

// handleDashboard renders the cluster-overview page: user count,
// recent-audit count, current signing key, registration mode.
func (s *AdminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	users, _ := s.queryUsers(r.Context(), "")
	events, _ := s.queryRecentAudit(r.Context(), "", 10)

	currentKID := "(no key loaded)"
	currentAge := "(unknown)"
	if cur := s.Keys.Current(); cur != nil {
		currentKID = cur.KID
		if !cur.CreatedAt.IsZero() {
			currentAge = humanizeDuration(time.Since(cur.CreatedAt))
		}
	}

	data := webtempl.AdminDashboardData{
		Layout:        s.layoutData(r, "Admin overview", false),
		Flash:         readFlash(r),
		Mode:          s.modeSnapshot(r),
		UserCount:     len(users),
		AuditCount:    len(events),
		AuditWindow:   "last 10",
		AuditEvents:   projectAuditViews(events),
		CurrentKID:    currentKID,
		CurrentKeyAge: currentAge,
	}
	s.render(w, r, "admin/dashboard", webtempl.AdminDashboard(data))
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

const usersListLimit = 200

func (s *AdminServer) handleUsersList(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	users, err := s.queryUsers(r.Context(), q)
	if err != nil {
		s.Logger.Warn("admin: users query failed", slog.String("error", err.Error()))
	}
	capped := false
	if len(users) > usersListLimit {
		users = users[:usersListLimit]
		capped = true
	}

	data := webtempl.AdminUsersListData{
		Layout:    s.layoutData(r, "Users", false),
		Flash:     readFlash(r),
		Users:     projectUserViews(users),
		UserCount: len(users),
		Query:     q,
		Capped:    capped,
		Limit:     usersListLimit,
	}
	s.render(w, r, "admin/users_list", webtempl.AdminUsersList(data))
}

func (s *AdminServer) handleUsersDetail(w http.ResponseWriter, r *http.Request) {
	userId := strings.TrimSpace(r.URL.Query().Get("id"))
	if userId == "" {
		http.Redirect(w, r, "/admin/users?flash=Missing+user+id&flash_kind=error", http.StatusSeeOther)
		return
	}
	user, err := s.userById(r.Context(), userId)
	if err != nil || user == nil {
		http.Redirect(w, r, "/admin/users?flash=User+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	data := webtempl.AdminUsersDetailData{
		Layout: s.layoutData(r, "User: "+user.PrimaryEmail, false),
		Flash:  readFlash(r),
		User:   projectUserView(*user),
	}
	s.render(w, r, "admin/users_detail", webtempl.AdminUsersDetail(data))
}

// handleEditUserProfile lets the admin set / update the user's
// directory-style profile fields (display name, phone, primaryRole,
// gender, birthdate). All but display_name are optional, but the
// product's ProfileCompletenessGuard demands all four of phone +
// primaryRole + gender + birthdate before the user can use the
// app -- so leaving any blank means the user stays gated.
func (s *AdminServer) handleEditUserProfile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/users?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	userId := strings.TrimSpace(r.PostForm.Get("user_id"))
	if userId == "" {
		http.Redirect(w, r, "/admin/users?flash=Missing+user+id&flash_kind=error", http.StatusSeeOther)
		return
	}
	user, err := s.userById(r.Context(), userId)
	if err != nil || user == nil {
		http.Redirect(w, r, "/admin/users?flash=User+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	firstName := strings.TrimSpace(r.PostForm.Get("first_name"))
	lastName := strings.TrimSpace(r.PostForm.Get("last_name"))
	displayName := strings.TrimSpace(r.PostForm.Get("display_name"))
	// If the admin set first / last name, derive displayName from
	// them so we don't end up with stale "first.last+from-email"
	// strings on rows where the fields used to be missing. The form
	// also still accepts an explicit display_name (its input is
	// pre-filled but optional); a non-empty value wins so the admin
	// can override the derivation.
	if displayName == "" {
		if firstName != "" || lastName != "" {
			displayName = strings.TrimSpace(firstName + " " + lastName)
		}
	}
	if displayName == "" {
		http.Redirect(w, r,
			"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Display+name+is+required&flash_kind=error",
			http.StatusSeeOther)
		return
	}
	user.DisplayName = displayName
	user.FirstName = firstName
	user.LastName = lastName
	user.Phone = strings.TrimSpace(r.PostForm.Get("phone"))
	user.PrimaryRole = strings.TrimSpace(r.PostForm.Get("primary_role"))
	user.Gender = strings.TrimSpace(r.PostForm.Get("gender"))
	user.Birthdate = strings.TrimSpace(r.PostForm.Get("birthdate"))
	if err := s.updateUser(r.Context(), user); err != nil {
		s.Logger.Warn("admin: profile update failed", slog.String("error", err.Error()))
		http.Redirect(w, r,
			"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Profile+update+failed&flash_kind=error",
			http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAdmin,
		Action:      "user_profile_edited",
		TargetType:  "user",
		TargetId:    userId,
		TargetEmail: user.PrimaryEmail,
		Detail: map[string]any{
			"firstName":   user.FirstName,
			"lastName":    user.LastName,
			"phone":       user.Phone,
			"primaryRole": user.PrimaryRole,
			"gender":      user.Gender,
			"birthdate":   user.Birthdate,
		},
		Outcome: identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r,
		"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Profile+saved&flash_kind=success",
		http.StatusSeeOther)
}

func (s *AdminServer) handleChangeUserRole(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/users?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	userId := strings.TrimSpace(r.PostForm.Get("user_id"))
	newRole := strings.ToLower(strings.TrimSpace(r.PostForm.Get("role")))
	if userId == "" || !isValidRole(newRole) {
		http.Redirect(w, r, "/admin/users?flash=Invalid+role+or+user&flash_kind=error", http.StatusSeeOther)
		return
	}
	user, err := s.userById(r.Context(), userId)
	if err != nil || user == nil {
		http.Redirect(w, r, "/admin/users?flash=User+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	oldRole := user.Role

	user.Role = newRole
	if err := s.updateUser(r.Context(), user); err != nil {
		s.Logger.Warn("admin: change role failed", slog.String("error", err.Error()))
		http.Redirect(w, r,
			"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Role+update+failed&flash_kind=error",
			http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAdmin,
		Action:      "user_role_changed",
		TargetType:  "user",
		TargetId:    userId,
		TargetEmail: user.PrimaryEmail,
		Detail:      map[string]any{"oldRole": oldRole, "newRole": newRole},
		Outcome:     identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r,
		"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Role+updated&flash_kind=success",
		http.StatusSeeOther)
}

func (s *AdminServer) handleSuspendUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/users?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	userId := strings.TrimSpace(r.PostForm.Get("user_id"))
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if userId == "" {
		http.Redirect(w, r, "/admin/users?flash=Missing+user+id&flash_kind=error", http.StatusSeeOther)
		return
	}
	user, err := s.userById(r.Context(), userId)
	if err != nil || user == nil {
		http.Redirect(w, r, "/admin/users?flash=User+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	user.SuspendedAt = time.Now().UTC().Format(time.RFC3339Nano)
	user.SuspendedReason = reason
	user.Active = false
	if err := s.updateUser(r.Context(), user); err != nil {
		s.Logger.Warn("admin: suspend failed", slog.String("error", err.Error()))
		http.Redirect(w, r,
			"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Suspend+failed&flash_kind=error",
			http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAdmin,
		Action:      "user_suspended",
		TargetType:  "user",
		TargetId:    userId,
		TargetEmail: user.PrimaryEmail,
		Detail:      map[string]any{"reason": reason},
		Outcome:     identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r,
		"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=User+suspended&flash_kind=success",
		http.StatusSeeOther)
}

func (s *AdminServer) handleUnsuspendUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/users?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	userId := strings.TrimSpace(r.PostForm.Get("user_id"))
	if userId == "" {
		http.Redirect(w, r, "/admin/users?flash=Missing+user+id&flash_kind=error", http.StatusSeeOther)
		return
	}
	user, err := s.userById(r.Context(), userId)
	if err != nil || user == nil {
		http.Redirect(w, r, "/admin/users?flash=User+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	user.SuspendedAt = ""
	user.SuspendedReason = ""
	user.Active = true
	if err := s.updateUser(r.Context(), user); err != nil {
		s.Logger.Warn("admin: unsuspend failed", slog.String("error", err.Error()))
		http.Redirect(w, r,
			"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Unsuspend+failed&flash_kind=error",
			http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAdmin,
		Action:      "user_unsuspended",
		TargetType:  "user",
		TargetId:    userId,
		TargetEmail: user.PrimaryEmail,
		Outcome:     identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r,
		"/admin/users/detail?id="+url.QueryEscape(userId)+"&flash=Suspension+lifted&flash_kind=success",
		http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

const auditListLimit = 200

func (s *AdminServer) handleAuditList(w http.ResponseWriter, r *http.Request) {
	cat := strings.TrimSpace(r.URL.Query().Get("category"))
	events, err := s.queryRecentAudit(r.Context(), cat, auditListLimit)
	if err != nil {
		s.Logger.Warn("admin: audit query failed", slog.String("error", err.Error()))
	}
	data := webtempl.AdminAuditListData{
		Layout:   s.layoutData(r, "Audit log", false),
		Flash:    readFlash(r),
		Category: cat,
		Limit:    auditListLimit,
		Events:   projectAuditViews(events),
	}
	s.render(w, r, "admin/audit_list", webtempl.AdminAuditList(data))
}

// ---------------------------------------------------------------------------
// JWKS
// ---------------------------------------------------------------------------

func (s *AdminServer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	data := webtempl.AdminJWKSData{
		Layout:       s.layoutData(r, "JWT signing keys", false),
		Flash:        readFlash(r),
		RotationDays: int(s.Cfg.KeyRotationInterval / (24 * time.Hour)),
		OverlapHours: int(s.Cfg.JWKSOverlapWindow / time.Hour),
	}

	cur := s.Keys.Current()
	if cur != nil {
		data.CurrentKID = cur.KID
		data.CurrentCreatedAt = cur.CreatedAt.UTC().Format(time.RFC3339)
		data.CurrentAge = humanizeDuration(time.Since(cur.CreatedAt))
	} else {
		data.CurrentKID = "(no key loaded)"
	}

	prev := s.Keys.Previous()
	if prev != nil {
		data.PreviousKID = prev.KID
		if !prev.RetiresAt.IsZero() {
			data.PreviousRetiresAt = prev.RetiresAt.UTC().Format(time.RFC3339)
		}
	}

	s.render(w, r, "admin/jwks", webtempl.AdminJWKS(data))
}

func (s *AdminServer) handleJWKSRotate(w http.ResponseWriter, r *http.Request) {
	if s.RotateNow == nil {
		s.Logger.Error("admin: RotateNow callback not wired")
		http.Redirect(w, r, "/admin/jwks?flash=Rotation+not+available&flash_kind=error", http.StatusSeeOther)
		return
	}
	if err := s.RotateNow(r); err != nil {
		s.Logger.Warn("admin: rotate now failed", slog.String("error", err.Error()))
		http.Redirect(w, r, "/admin/jwks?flash=Rotation+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	// RotateNow itself emits an audit event via Service.RotateNow, so
	// no duplicate audit here.
	http.Redirect(w, r, "/admin/jwks?flash=Key+rotated&flash_kind=success", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func (s *AdminServer) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	form := s.currentSettingsForm(r.Context())
	data := webtempl.AdminSettingsData{
		Layout: s.layoutData(r, "Cluster settings", false, "/static/admin-settings-branding.js"),
		Flash:  readFlash(r),
		Form:   form,
	}
	s.render(w, r, "admin/settings", webtempl.AdminSettings(data))
}

func (s *AdminServer) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/settings?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}

	// Token TTLs come in as user-friendly units (minutes / days) and
	// get normalised to the on-the-wire seconds. Empty / 0 means "use
	// default" -- stored as 0 on the row, picked up as the boot-time
	// fallback by effectiveTokenSettings.
	accessMinutes, err := parseOptionalInt(r.PostForm.Get("access_token_ttl_minutes"))
	if err != nil {
		http.Redirect(w, r, "/admin/settings?flash=Access+token+lifetime+must+be+a+number&flash_kind=error", http.StatusSeeOther)
		return
	}
	refreshDays, err := parseOptionalInt(r.PostForm.Get("refresh_token_ttl_days"))
	if err != nil {
		http.Redirect(w, r, "/admin/settings?flash=Refresh+token+lifetime+must+be+a+number&flash_kind=error", http.StatusSeeOther)
		return
	}
	magicMinutes, err := parseOptionalInt(r.PostForm.Get("magic_link_ttl_minutes"))
	if err != nil {
		http.Redirect(w, r, "/admin/settings?flash=Magic-link+lifetime+must+be+a+number&flash_kind=error", http.StatusSeeOther)
		return
	}
	invitationDays, err := parseOptionalInt(r.PostForm.Get("invitation_ttl_days"))
	if err != nil {
		http.Redirect(w, r, "/admin/settings?flash=Invitation+lifetime+must+be+a+number&flash_kind=error", http.StatusSeeOther)
		return
	}

	in := identity.ClusterSettingsRow{
		BrandName:                 strings.TrimSpace(r.PostForm.Get("brand_name")),
		BrandPrimaryColor:         strings.TrimSpace(r.PostForm.Get("brand_primary_color")),
		BrandLogoDataURI:          strings.TrimSpace(r.PostForm.Get("brand_logo_data_uri")),
		BrandIconDataURI:          strings.TrimSpace(r.PostForm.Get("brand_icon_data_uri")),
		RegistrationMode:          strings.TrimSpace(r.PostForm.Get("registration_mode")),
		RegistrationDomains:       strings.TrimSpace(r.PostForm.Get("registration_domains")),
		InternalDomains:           strings.TrimSpace(r.PostForm.Get("internal_domains")),
		InternalDefaultRole:       strings.TrimSpace(r.PostForm.Get("internal_default_role")),
		RegisteredClientsJSON:     strings.TrimSpace(r.PostForm.Get("registered_clients_json")),
		AccessRequestNotifyEmails: strings.TrimSpace(r.PostForm.Get("access_request_notify_emails")),
		AccessTokenTTLSeconds:     accessMinutes * 60,
		RefreshTokenTTLSeconds:    refreshDays * 24 * 60 * 60,
		MagicLinkTTLSeconds:       magicMinutes * 60,
		InvitationTTLDays:         invitationDays,
		RefreshCookieSameSite:     strings.TrimSpace(r.PostForm.Get("refresh_cookie_samesite")),
	}
	if !isValidRegistrationMode(in.RegistrationMode) {
		http.Redirect(w, r, "/admin/settings?flash=Pick+a+valid+registration+mode&flash_kind=error", http.StatusSeeOther)
		return
	}
	if !isValidRole(in.InternalDefaultRole) {
		http.Redirect(w, r, "/admin/settings?flash=Pick+a+valid+internal+default+role&flash_kind=error", http.StatusSeeOther)
		return
	}
	if in.RegisteredClientsJSON != "" {
		var probe any
		if err := json.Unmarshal([]byte(in.RegisteredClientsJSON), &probe); err != nil {
			http.Redirect(w, r, "/admin/settings?flash=Registered+clients+JSON+is+invalid&flash_kind=error", http.StatusSeeOther)
			return
		}
	}
	// Bounds check on TTLs. Each is allowed to be 0 (= "use default")
	// but anything non-zero must fall inside the operator-friendly
	// limits in identity/config.go. Out-of-bounds = explicit reject;
	// no silent clamping (operators should know what they got).
	if v := in.AccessTokenTTLSeconds; v != 0 && (v < identity.MinAccessTokenTTLSeconds || v > identity.MaxAccessTokenTTLSeconds) {
		http.Redirect(w, r, "/admin/settings?flash=Access+token+lifetime+must+be+1+minute+to+24+hours&flash_kind=error", http.StatusSeeOther)
		return
	}
	if v := in.RefreshTokenTTLSeconds; v != 0 && (v < identity.MinRefreshTokenTTLSeconds || v > identity.MaxRefreshTokenTTLSeconds) {
		http.Redirect(w, r, "/admin/settings?flash=Refresh+token+lifetime+must+be+1+day+to+365+days&flash_kind=error", http.StatusSeeOther)
		return
	}
	if v := in.MagicLinkTTLSeconds; v != 0 && (v < identity.MinMagicLinkTTLSeconds || v > identity.MaxMagicLinkTTLSeconds) {
		http.Redirect(w, r, "/admin/settings?flash=Magic-link+lifetime+must+be+1+to+60+minutes&flash_kind=error", http.StatusSeeOther)
		return
	}
	if v := in.InvitationTTLDays; v != 0 && (v < identity.MinInvitationTTLDays || v > identity.MaxInvitationTTLDays) {
		http.Redirect(w, r, "/admin/settings?flash=Invitation+lifetime+must+be+1+to+90+days&flash_kind=error", http.StatusSeeOther)
		return
	}
	switch in.RefreshCookieSameSite {
	case "", "lax", "none":
		// ok
	default:
		http.Redirect(w, r, "/admin/settings?flash=Refresh+cookie+SameSite+must+be+lax+or+none&flash_kind=error", http.StatusSeeOther)
		return
	}

	if err := s.persistSettings(r.Context(), in); err != nil {
		s.Logger.Warn("admin: settings save failed", slog.String("error", err.Error()))
		http.Redirect(w, r, "/admin/settings?flash=Save+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category: identity.AuditCategoryConfiguration,
		Action:   "cluster_settings_updated",
		Detail: map[string]any{
			"registrationMode":    in.RegistrationMode,
			"internalDefaultRole": in.InternalDefaultRole,
		},
		Outcome: identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r, "/admin/settings?flash=Settings+saved&flash_kind=success", http.StatusSeeOther)
}

func (s *AdminServer) currentSettingsForm(ctx context.Context) webtempl.AdminSettingsForm {
	// Brand fields (BrandName / BrandPrimaryColor / BrandLogoDataURI)
	// intentionally NOT seeded from s.Cfg. Those env-var defaults exist
	// so the public web pages render with a sensible brand even on a
	// freshly bootstrapped cluster -- they are NOT operator preferences.
	// Showing them in the settings form makes them look like saved
	// values the operator chose, when in fact they came from
	// IDENTITY_BRAND_* env vars. The form should reflect *operator
	// choices*; if nothing's saved, the field renders empty so the
	// operator can pick fresh.
	form := webtempl.AdminSettingsForm{
		RegistrationMode:          string(s.Cfg.RegistrationMode),
		RegistrationDomains:       strings.Join(s.Cfg.RegistrationDomains, ", "),
		InternalDomains:           strings.Join(s.Cfg.InternalDomains, ", "),
		InternalDefaultRole:       s.Cfg.InternalDefaultRole,
		AccessRequestNotifyEmails: strings.Join(s.Cfg.AccessRequestNotifyEmails, ", "),
	}
	if reader, ok := s.Settings.(*LiveSettingsReader); ok && reader != nil {
		row, err := reader.read(ctx)
		if err == nil && row != nil {
			if row.BrandName != "" {
				form.BrandName = row.BrandName
			}
			if row.BrandPrimaryColor != "" {
				form.BrandPrimaryColor = row.BrandPrimaryColor
			}
			if row.BrandLogoDataURI != "" {
				form.BrandLogoDataURI = row.BrandLogoDataURI
			}
			if row.BrandIconDataURI != "" {
				form.BrandIconDataURI = row.BrandIconDataURI
			}
			if row.RegistrationMode != "" {
				form.RegistrationMode = row.RegistrationMode
			}
			if row.RegistrationDomains != "" {
				form.RegistrationDomains = row.RegistrationDomains
			}
			if row.InternalDomains != "" {
				form.InternalDomains = row.InternalDomains
			}
			if row.InternalDefaultRole != "" {
				form.InternalDefaultRole = row.InternalDefaultRole
			}
			if row.RegisteredClientsJSON != "" {
				form.RegisteredClientsJSON = row.RegisteredClientsJSON
			}
			if row.AccessRequestNotifyEmails != "" {
				form.AccessRequestNotifyEmails = row.AccessRequestNotifyEmails
			}
			// Token TTLs. Stored on the row in their on-the-wire
			// units (seconds for access/refresh/magic-link, days
			// for invitation). The form shows minutes / days for
			// readability; the post handler converts back.
			form.AccessTokenTTLSeconds = row.AccessTokenTTLSeconds
			if row.AccessTokenTTLSeconds > 0 {
				form.AccessTokenTTLMinutes = row.AccessTokenTTLSeconds / 60
			}
			form.RefreshTokenTTLSeconds = row.RefreshTokenTTLSeconds
			if row.RefreshTokenTTLSeconds > 0 {
				form.RefreshTokenTTLDays = row.RefreshTokenTTLSeconds / (24 * 60 * 60)
			}
			form.MagicLinkTTLSeconds = row.MagicLinkTTLSeconds
			if row.MagicLinkTTLSeconds > 0 {
				form.MagicLinkTTLMinutes = row.MagicLinkTTLSeconds / 60
			}
			form.InvitationTTLDays = row.InvitationTTLDays
			form.RefreshCookieSameSite = row.RefreshCookieSameSite
		}
	}
	if form.RegisteredClientsJSON == "" && len(s.Cfg.RegisteredClients) > 0 {
		buf, err := json.Marshal(s.Cfg.RegisteredClients)
		if err == nil {
			form.RegisteredClientsJSON = string(buf)
		}
	}
	// Effective-value labels show what's currently in force after
	// the override-vs-default resolution. The operator sees both
	// "you typed X" (the input value) and "system is using Y"
	// (the effective label) -- useful when override is empty and
	// the effective label is the env / built-in default.
	form.EffectiveAccessTokenLabel = describeDuration(effectiveAccessTokenTTL(form, s.Cfg))
	form.EffectiveRefreshTokenLabel = describeDuration(effectiveRefreshTokenTTL(form, s.Cfg))
	form.EffectiveMagicLinkLabel = describeDuration(effectiveMagicLinkTTL(form, s.Cfg))
	form.EffectiveInvitationLabel = describeDuration(effectiveInvitationTTL(form, s.Cfg))
	form.EffectiveSameSiteLabel = effectiveSameSiteLabel(form)
	return form
}

// effectiveAccessTokenTTL / effectiveRefreshTokenTTL / etc. mirror
// the runtime resolution path (override -> env -> default). Used
// to render the "Effective: X" line under each form input.
func effectiveAccessTokenTTL(form webtempl.AdminSettingsForm, cfg identity.Config) time.Duration {
	if form.AccessTokenTTLSeconds > 0 {
		return time.Duration(form.AccessTokenTTLSeconds) * time.Second
	}
	if cfg.AccessTokenTTL > 0 {
		return cfg.AccessTokenTTL
	}
	return time.Duration(identity.DefaultAccessTokenTTLSeconds) * time.Second
}

func effectiveRefreshTokenTTL(form webtempl.AdminSettingsForm, cfg identity.Config) time.Duration {
	if form.RefreshTokenTTLSeconds > 0 {
		return time.Duration(form.RefreshTokenTTLSeconds) * time.Second
	}
	if cfg.RefreshTokenTTL > 0 {
		return cfg.RefreshTokenTTL
	}
	return time.Duration(identity.DefaultRefreshTokenTTLSeconds) * time.Second
}

func effectiveMagicLinkTTL(form webtempl.AdminSettingsForm, cfg identity.Config) time.Duration {
	if form.MagicLinkTTLSeconds > 0 {
		return time.Duration(form.MagicLinkTTLSeconds) * time.Second
	}
	if cfg.MagicLinkTTL > 0 {
		return cfg.MagicLinkTTL
	}
	return time.Duration(identity.DefaultMagicLinkTTLSeconds) * time.Second
}

func effectiveInvitationTTL(form webtempl.AdminSettingsForm, cfg identity.Config) time.Duration {
	if form.InvitationTTLDays > 0 {
		return time.Duration(form.InvitationTTLDays) * 24 * time.Hour
	}
	if cfg.InvitationTTL > 0 {
		return cfg.InvitationTTL
	}
	return time.Duration(identity.DefaultInvitationTTLDays) * 24 * time.Hour
}

func effectiveSameSiteLabel(form webtempl.AdminSettingsForm) string {
	switch strings.ToLower(strings.TrimSpace(form.RefreshCookieSameSite)) {
	case "lax":
		return "lax (override)"
	case "none":
		return "none (override)"
	}
	return "lax (default; env can override)"
}

// describeDuration renders a duration in operator-friendly units.
// 90s -> "1m 30s", 86400s -> "1 day", 2592000s -> "30 days".
func describeDuration(d time.Duration) string {
	if d <= 0 {
		return "(none)"
	}
	days := int(d / (24 * time.Hour))
	if days >= 1 && d%(24*time.Hour) == 0 {
		if days == 1 {
			return "1 day"
		}
		return strconv.Itoa(days) + " days"
	}
	hours := int(d / time.Hour)
	if hours >= 1 && d%time.Hour == 0 {
		if hours == 1 {
			return "1 hour"
		}
		return strconv.Itoa(hours) + " hours"
	}
	minutes := int(d / time.Minute)
	if minutes >= 1 && d%time.Minute == 0 {
		if minutes == 1 {
			return "1 minute"
		}
		return strconv.Itoa(minutes) + " minutes"
	}
	return d.String()
}

// persistSettings writes the new cluster-settings row via
// updateClusterSettings. Distinct from the wizard's
// createClusterSettings call so the audit event stream can
// tell first-run bootstrap writes from runtime admin edits — the
// data layer treats both as append-only inserts (memQL has no
// upsert primitive) but the event-stream distinction is useful.
func (s *AdminServer) persistSettings(ctx context.Context, in identity.ClusterSettingsRow) error {
	mode := in.RegistrationMode
	if mode == "" {
		mode = "open"
	}
	role := in.InternalDefaultRole
	if role == "" {
		role = "writer"
	}
	// Preserve BootstrappedAt + ClusterDomain across admin saves.
	// Neither is exposed by the admin form; both are inherited
	// from the latest existing row so a settings save doesn't
	// reset the bootstrap stamp or wipe the operator-supplied
	// cluster domain.
	bootstrappedAt := ""
	clusterDomain := ""
	if reader, ok := s.Settings.(*LiveSettingsReader); ok && reader != nil {
		if row, err := reader.read(ctx); err == nil && row != nil {
			bootstrappedAt = row.BootstrappedAt
			clusterDomain = row.ClusterDomain
		}
	}
	q := fmt.Sprintf(`mutation updateClusterSettings(`+
		`id: "cluster",`+
		`clusterDomain: %q,`+
		`brandName: %q,`+
		`brandPrimaryColor: %q,`+
		`brandLogoDataURI: %q,`+
		`brandIconDataURI: %q,`+
		`registrationMode: %q,`+
		`registrationDomains: %q,`+
		`internalDomains: %q,`+
		`internalDefaultRole: %q,`+
		`registeredClientsJSON: %q,`+
		`accessRequestNotifyEmails: %q,`+
		`bootstrapEmail: %q,`+
		`bootstrappedAt: %q,`+
		`accessTokenTTLSeconds: %d,`+
		`refreshTokenTTLSeconds: %d,`+
		`magicLinkTTLSeconds: %d,`+
		`invitationTTLDays: %d,`+
		`refreshCookieSameSite: %q`+
		`)`,
		clusterDomain,
		in.BrandName,
		in.BrandPrimaryColor,
		in.BrandLogoDataURI,
		in.BrandIconDataURI,
		mode,
		in.RegistrationDomains,
		in.InternalDomains,
		role,
		in.RegisteredClientsJSON,
		in.AccessRequestNotifyEmails,
		in.BootstrapEmail,
		bootstrappedAt,
		in.AccessTokenTTLSeconds,
		in.RefreshTokenTTLSeconds,
		in.MagicLinkTTLSeconds,
		in.InvitationTTLDays,
		in.RefreshCookieSameSite,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("admin: persist cluster settings: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Placeholder handler
// ---------------------------------------------------------------------------

func (s *AdminServer) handlePlaceholder(heading, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := webtempl.AdminPlaceholderData{
			Layout:  s.layoutData(r, heading, false),
			Heading: heading,
			Body:    body,
		}
		s.render(w, r, "admin/placeholder", webtempl.AdminPlaceholder(data))
	}
}

// ---------------------------------------------------------------------------
// Engine queries / mutations
// ---------------------------------------------------------------------------

// userView is the per-row projection used internally by the admin
// queries before being projected to the templ-facing AdminUserView.
type userView struct {
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
	Internal        bool
	Active          bool
	SuspendedAt     string
	SuspendedReason string
	CreatedAt       string
}

// queryUsers returns active users matching the optional substring
// filter. The MemQL grammar doesn't support LIKE, so the filter is
// applied in Go after the query.
//
// activeUsers is `paginate 50` now (5.3 / epic #1964), so a single
// Execute returns only the first 50 users. This admin list view needs
// the COMPLETE set (the Go-side substring filter further narrows it), so
// walk the keyset cursor across all pages instead of silently rendering
// only the first 50 (5.10b).
func (s *AdminServer) queryUsers(ctx context.Context, q string) ([]userView, error) {
	out := []userView{}
	needle := strings.ToLower(strings.TrimSpace(q))
	cursor := ""
	for i := 0; i < maxUserPageWalk; i++ {
		// #2883: activeUsers is @serverOnly. The admin app is server-side Go
		// gated at its own HTTP layer and reads arbitrary users by design --
		// the same reasoning userById below already records for userByIdSystem.
		// #2934 made that gate enforced rather than assumed; see the note there.
		res, err := s.Engine.Execute(auth.ContextWithInternalOrigin(memqlengine.ContextWithCursor(ctx, cursor)), `query activeUsers()`)
		if err != nil {
			return nil, err
		}
		if res == nil || res.Bundle == nil {
			break
		}
		for _, n := range res.Bundle.Nodes {
			uv := userViewFromNode(n)
			if uv == nil {
				continue
			}
			if needle != "" {
				if !strings.Contains(strings.ToLower(uv.DisplayName), needle) &&
					!strings.Contains(strings.ToLower(uv.PrimaryEmail), needle) {
					continue
				}
			}
			out = append(out, *uv)
		}
		if res.Meta == nil || res.Meta.Cursor == "" {
			break
		}
		cursor = res.Meta.Cursor
	}
	return out, nil
}

// maxUserPageWalk bounds the keyset walk over activeUsers (50/page)
// so a mis-paginated query can never spin forever -- 50k users is far
// beyond any real cluster, but a hard stop nonetheless.
const maxUserPageWalk = 1000

func (s *AdminServer) userById(ctx context.Context, userId string) (*userView, error) {
	// #2800: the admin app is server-side Go and is already gated at its own
	// HTTP layer; it reads arbitrary users by design.
	//
	// That gate is this stamp's whole safety argument. userId is caller-supplied
	// and userByIdSystem is @serverOnly, so a route reaching here without
	// `gated` would turn this into a cross-user read of any user row. #2934
	// made the precondition enforced rather than assumed:
	// route_gate_test.go asserts every mounted route goes through `gated`, and
	// that `gated` turns away a caller who is not owner or admin. Do not call
	// this from a path that is not so gated.
	q := fmt.Sprintf(`query userByIdSystem(userId: %q)`, userId)
	res, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), q)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return nil, nil
	}
	uv := userViewFromNode(res.Bundle.Nodes[0])
	return uv, nil
}

// updateUser writes the user back via updateUser. The mutation
// expects the full payload object; we stitch one together from the
// userView fields.
func (s *AdminServer) updateUser(ctx context.Context, u *userView) error {
	if u == nil || u.ID == "" {
		return fmt.Errorf("admin: updateUser requires id")
	}
	payload := map[string]any{
		"displayName":  u.DisplayName,
		"firstName":    u.FirstName,
		"lastName":     u.LastName,
		"primaryEmail": u.PrimaryEmail,
		"phone":        u.Phone,
		"primaryRole":  u.PrimaryRole,
		"gender":       u.Gender,
		"birthdate":    u.Birthdate,
		"role":         u.Role,
		"internal":     u.Internal,
		"active":       u.Active,
	}
	if u.SuspendedAt != "" {
		payload["suspendedAt"] = u.SuspendedAt
	}
	if u.SuspendedReason != "" {
		payload["suspendedReason"] = u.SuspendedReason
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	q := fmt.Sprintf(`mutation updateUser(userId: %q, payload: %s)`, u.ID, string(payloadJSON))
	// Internal origin, because updateUser is @serverOnly as of memql#2991. This
	// call site was the ONLY one not stamping it -- its sibling at :933, the PAT
	// store and the identity store all did -- so the omission was an
	// inconsistency before it was a blocker.
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
		return fmt.Errorf("admin: update user: %w", err)
	}
	return nil
}

// auditView is the in-package projection used by the queries before
// projection to the templ-facing AdminAuditView.
type auditView struct {
	ID            string
	OccurredAt    string
	Category      string
	Action        string
	ActorEmail    string
	ActorRole     string
	TargetId      string
	TargetEmail   string
	Outcome       string
	FailureReason string
	SourceIP      string
}

func (s *AdminServer) queryRecentAudit(ctx context.Context, category string, limit int) ([]auditView, error) {
	q := `query recentAuditEvents()`
	if category != "" {
		q = fmt.Sprintf(`query recentAuditEvents(category: %q)`, category)
	}
	res, err := s.Engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []auditView{}
	if res == nil || res.Bundle == nil {
		return out, nil
	}
	for _, n := range res.Bundle.Nodes {
		av := auditViewFromNode(n)
		if av == nil {
			continue
		}
		out = append(out, *av)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Field-projection helpers (Go-internal -> templ-facing)
// ---------------------------------------------------------------------------

func projectUserView(u userView) webtempl.AdminUserView {
	return webtempl.AdminUserView{
		ID:              u.ID,
		DisplayName:     u.DisplayName,
		FirstName:       u.FirstName,
		LastName:        u.LastName,
		PrimaryEmail:    u.PrimaryEmail,
		Phone:           u.Phone,
		PrimaryRole:     u.PrimaryRole,
		Gender:          u.Gender,
		Birthdate:       u.Birthdate,
		Role:            u.Role,
		Internal:        u.Internal,
		Active:          u.Active,
		SuspendedAt:     u.SuspendedAt,
		SuspendedReason: u.SuspendedReason,
		CreatedAt:       u.CreatedAt,
	}
}

func projectUserViews(rows []userView) []webtempl.AdminUserView {
	out := make([]webtempl.AdminUserView, 0, len(rows))
	for _, u := range rows {
		out = append(out, projectUserView(u))
	}
	return out
}

func projectAuditViews(rows []auditView) []webtempl.AdminAuditView {
	out := make([]webtempl.AdminAuditView, 0, len(rows))
	for _, a := range rows {
		out = append(out, webtempl.AdminAuditView{
			ID:            a.ID,
			OccurredAt:    a.OccurredAt,
			Category:      a.Category,
			Action:        a.Action,
			ActorEmail:    a.ActorEmail,
			ActorRole:     a.ActorRole,
			TargetID:      a.TargetId,
			TargetEmail:   a.TargetEmail,
			Outcome:       a.Outcome,
			FailureReason: a.FailureReason,
			SourceIP:      a.SourceIP,
		})
	}
	return out
}

func userViewFromNode(n *memqlv1.MemoryNode) *userView {
	if n == nil || n.Payload == nil {
		return nil
	}
	fields := n.Payload.GetFields()
	if fields == nil {
		return nil
	}
	str := func(k string) string {
		if v, ok := fields[k]; ok && v != nil {
			return strings.TrimSpace(v.GetStringValue())
		}
		return ""
	}
	boolean := func(k string) bool {
		if v, ok := fields[k]; ok && v != nil {
			return v.GetBoolValue()
		}
		return false
	}
	uv := &userView{
		ID:              n.GetId(),
		DisplayName:     str("displayName"),
		FirstName:       str("firstName"),
		LastName:        str("lastName"),
		PrimaryEmail:    str("primaryEmail"),
		Phone:           str("phone"),
		PrimaryRole:     str("primaryRole"),
		Gender:          str("gender"),
		Birthdate:       str("birthdate"),
		Role:            str("role"),
		Internal:        boolean("internal"),
		Active:          boolean("active"),
		SuspendedAt:     str("suspendedAt"),
		SuspendedReason: str("suspendedReason"),
		CreatedAt:       str("createdAt"),
	}
	// active defaults to true on the concept; the bool extractor
	// returns false when the field is missing. Treat "missing" as
	// active so the UI doesn't claim suspended-by-default.
	if _, ok := fields["active"]; !ok {
		uv.Active = true
	}
	return uv
}

func auditViewFromNode(n *memqlv1.MemoryNode) *auditView {
	if n == nil || n.Payload == nil {
		return nil
	}
	fields := n.Payload.GetFields()
	if fields == nil {
		return nil
	}
	str := func(k string) string {
		if v, ok := fields[k]; ok && v != nil {
			return strings.TrimSpace(v.GetStringValue())
		}
		return ""
	}
	return &auditView{
		ID:            n.GetId(),
		OccurredAt:    str("occurredAt"),
		Category:      str("category"),
		Action:        str("action"),
		ActorEmail:    str("actorEmail"),
		ActorRole:     str("actorRole"),
		TargetId:      str("targetId"),
		TargetEmail:   str("targetEmail"),
		Outcome:       str("outcome"),
		FailureReason: str("failureReason"),
		SourceIP:      str("sourceIP"),
	}
}

// ---------------------------------------------------------------------------
// Validators
// ---------------------------------------------------------------------------

func isValidRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner", "admin", "developer", "writer", "reader":
		return true
	}
	return false
}

func isValidRegistrationMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "open", "domain_restricted", "invite_only", "waitlist":
		return true
	}
	return false
}

// parseOptionalInt accepts an empty string (returns 0) or a numeric
// string (returns the parsed int). Other inputs return an error so
// the post handler can flash a clear validation message instead of
// silently coercing to 0.
func parseOptionalInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.Atoi(raw)
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

// humanizeDuration produces a coarse human-readable string like
// "5d", "12h", "3m". Nothing fancy; sufficient for the dashboard.
func humanizeDuration(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	day := 24 * time.Hour
	switch {
	case d >= day:
		return fmt.Sprintf("%dd", int(d/day))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}
