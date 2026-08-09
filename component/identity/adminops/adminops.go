// Package adminops is the owner/admin gate for the identity writes the
// server-rendered /admin/* console used to own (memql#3324).
//
// ===========================================================================
// WHY THIS PACKAGE EXISTS AT ALL
// ===========================================================================
// Every write below already had a memQL mutation -- updateUser,
// revokePATIdentity, revokeNodeTokenIdentity, updateClusterSettings -- and the
// portal could have called each one directly over the ordinary query surface.
// That would have deleted the gate rather than moved it.
//
// A memQL mutation cannot carry a role predicate. `filter` is a read
// construct; there is no mutation-side spec, and row-authz enforcement is
// inert by construction (TestRowAuthzIsInert, memql#2920). So the owner/admin
// rule these writes ran under was the templ console's HTTP ROUTE and nothing
// else:
//
//   - updateUser is @serverOnly (memql#2991) -- it names a target id AND takes
//     a payload SPLAT that can carry `role`, so validateMutationCallerArgs is
//     structurally blind to it. There is no client-reachable seam at all.
//   - revokePATIdentity and updateClusterSettings take an arbitrary target and
//     apply no predicate, and the coarse write check admits every role from
//     `writer` up. A writer calling either over the stream would succeed where
//     the console demanded admin.
//
// The gate therefore stays in Go, where it already lived, and moves from an
// HTTP middleware to this package: ONE implementation, one audit trail, one
// place to read the rule. component/grpc/identity_admin_handlers.go is a thin
// envelope translator over it and holds no policy of its own.
//
// ===========================================================================
// WHAT "AUDITED" MEANS HERE
// ===========================================================================
// Every call emits exactly one v1:identity:auditEvent, INCLUDING a refusal --
// the refusal writes the same `admin_auth_forbidden` event with the same
// `role_not_admin` failure reason that component/identity/admin/auth.go wrote,
// so a trail an operator greps is unbroken across the move. Each success emits
// the same action name its templ handler emitted (user_profile_edited,
// user_role_changed, user_suspended, user_unsuspended, pat_revoked_admin,
// node_token_revoked_admin, cluster_settings_updated) for the same reason.
//
// The event's correlation id comes back on every Result, refusals included: an
// operator arguing about a denial needs its id as much as one reconciling a
// change.
package adminops

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/pat"
	"github.com/znasllc-io/memql/core/id"
)

// Canonical gRPC status codes, by number. Named here rather than imported so
// this package -- which is pure policy plus engine calls -- carries no
// transport dependency. component/grpc maps them straight onto
// IdentityAdminResult.error_code, which is where the numbers are observed.
const (
	CodeOK               = 0
	CodeInvalidArgument  = 3
	CodeNotFound         = 5
	CodePermissionDenied = 7
	CodeInternal         = 13
	CodeUnauthenticated  = 16
)

// Result is the uniform outcome of every operation.
//
// It is a VALUE, not an error, because a refusal is a normal outcome on this
// surface that must still carry an audit id -- and because the caller is a
// multiplexed stream handler, where returning a Go error tears the connection
// down and takes every other in-flight request with it.
type Result struct {
	OK bool
	// Canonical gRPC code. 0 on success.
	Code int32
	// Failure detail. Safe to show an operator: it never quotes a credential
	// or a row payload.
	ErrorMessage string
	// Correlation id of the audit event this call wrote. Present on refusals.
	AuditEventId string
	// One line describing what changed, for the console's status line.
	Message string
}

func ok(auditID, message string) Result {
	return Result{OK: true, Code: CodeOK, AuditEventId: auditID, Message: message}
}

func fail(code int32, auditID, message string) Result {
	return Result{Code: code, AuditEventId: auditID, ErrorMessage: message}
}

// Service performs the gated writes. Construct one per node; it holds no
// per-call state.
type Service struct {
	// Engine executes the DSL. Required.
	Engine identity.EngineExecutor
	// Audit receives one event per call, refusals included. Required in
	// production; a nil logger degrades to "no trail", which is why the
	// constructor refuses it.
	Audit identity.AuditLogger
	// Logger receives a warn line per failed write. Optional.
	Logger *slog.Logger
	// Now is a test-friendly clock. nil = time.Now().UTC.
	Now func() time.Time
}

// New validates dependencies and returns a ready Service.
func New(s *Service) (*Service, error) {
	if s == nil {
		return nil, fmt.Errorf("adminops: nil Service")
	}
	if s.Engine == nil {
		return nil, fmt.Errorf("adminops: Engine is required")
	}
	if s.Audit == nil {
		// Refused rather than defaulted to a no-op. An unaudited admin write
		// surface is worse than an absent one: it looks like the gate is
		// working and leaves nothing behind to check it by.
		return nil, fmt.Errorf("adminops: Audit is required -- an unaudited admin write surface is not permitted")
	}
	if s.Now == nil {
		s.Now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

type actor struct {
	userID     string
	email      string
	role       auth.Role
	identityID string
}

// resolveActor reads the caller off the context, preferring the resolved
// AccessContext (what the stream interceptor stamps) and falling back to the
// user context. Mirrors component/deploycontrol.resolveActor -- the two gates
// must agree about who is calling, and the way to guarantee that is to read
// the same two surfaces in the same order.
func resolveActor(ctx context.Context) (actor, bool) {
	if ac, okAccess := auth.AccessFromContext(ctx); okAccess {
		return actor{
			userID:     ac.UserId,
			email:      ac.PrimaryEmail,
			role:       ac.Role,
			identityID: ac.IdentityId,
		}, true
	}
	if uc, okUser := auth.UserFromContext(ctx); okUser {
		return actor{userID: uc.ID, email: uc.Email, role: uc.Role}, true
	}
	return actor{}, false
}

// authorize enforces owner-or-admin and returns the caller.
//
// On refusal it emits `admin_auth_forbidden` -- byte-identical in category,
// action, outcome and failure reason to what component/identity/admin/auth.go
// wrote at the route -- and returns a Result the caller returns verbatim. The
// boolean, not the zero Result, is what says "proceed": a Result is a value
// and an empty one is indistinguishable from a successful no-op.
func (s *Service) authorize(ctx context.Context, verb string, detail map[string]any) (actor, Result, bool) {
	act, resolved := resolveActor(ctx)
	if !resolved {
		eventID := s.emit(ctx, identity.AuditCategoryAdmin, "admin_auth_forbidden", actor{}, "", "", detail,
			identity.AuditOutcomeBlocked, "no_authenticated_actor")
		return actor{}, fail(CodeUnauthenticated, eventID,
			"identity admin: no authenticated caller on this connection"), false
	}
	if !auth.AtLeastAdmin(act.userContext()) {
		eventID := s.emit(ctx, identity.AuditCategoryAdmin, "admin_auth_forbidden", act, "", "", detail,
			identity.AuditOutcomeBlocked, "role_not_admin")
		return actor{}, fail(CodePermissionDenied, eventID, fmt.Sprintf(
			"identity admin: %s requires the owner or admin role (you hold %q)", verb, act.role)), false
	}
	return act, Result{}, true
}

func (a actor) userContext() auth.UserContext {
	return auth.UserContext{ID: a.userID, Email: a.email, Role: a.role}
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// emit writes exactly one audit event and returns its correlation id.
func (s *Service) emit(
	ctx context.Context,
	category identity.AuditCategory,
	action string,
	act actor,
	targetID, targetEmail string,
	detail map[string]any,
	outcome identity.AuditOutcome,
	failureReason string,
) string {
	eventID := id.NewShortId()
	targetType := ""
	if targetID != "" {
		targetType = "user"
	}
	s.Audit.Log(ctx, identity.AuditEvent{
		OccurredAt:    s.Now(),
		Category:      category,
		Action:        action,
		ActorUserId:   act.userID,
		ActorEmail:    act.email,
		ActorRole:     string(act.role),
		ActorIdentity: act.identityID,
		TargetType:    targetType,
		TargetId:      targetID,
		TargetEmail:   targetEmail,
		Detail:        detail,
		Outcome:       outcome,
		FailureReason: failureReason,
		CorrelationId: eventID,
	})
	return eventID
}

// finish emits the success-or-failure event for a write that has already run
// and packages the Result. One place, so an operation cannot forget the trail.
func (s *Service) finish(
	ctx context.Context,
	category identity.AuditCategory,
	action string,
	act actor,
	targetID, targetEmail string,
	detail map[string]any,
	message string,
	runErr error,
) Result {
	if runErr != nil {
		if s.Logger != nil {
			s.Logger.Warn("identity admin write failed",
				slog.String("action", action), slog.String("target", targetID),
				slog.String("error", runErr.Error()))
		}
		eventID := s.emit(ctx, category, action, act, targetID, targetEmail, detail,
			identity.AuditOutcomeFailure, runErr.Error())
		return fail(CodeInternal, eventID, runErr.Error())
	}
	eventID := s.emit(ctx, category, action, act, targetID, targetEmail, detail,
		identity.AuditOutcomeSuccess, "")
	return ok(eventID, message)
}

// ---------------------------------------------------------------------------
// People
// ---------------------------------------------------------------------------

// UserProfile is the directory-field edit. Every field is applied, empty ones
// included: the underlying mutation is a shallow merge, so treating "" as
// "leave alone" would make it impossible to clear a phone number -- and the
// form this replaces could.
type UserProfile struct {
	UserId      string
	DisplayName string
	FirstName   string
	LastName    string
	Phone       string
	PrimaryRole string
	Gender      string
	Birthdate   string
}

// UpdateUserProfile replaces POST /admin/users/profile.
func (s *Service) UpdateUserProfile(ctx context.Context, in UserProfile) Result {
	userID := strings.TrimSpace(in.UserId)
	detail := map[string]any{"userId": userID}
	act, refusal, allowed := s.authorize(ctx, "editing a profile", detail)
	if !allowed {
		return refusal
	}
	if userID == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_profile_edited",
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_user_id"),
			"identity admin: userId is required")
	}

	user, err := s.userById(ctx, userID)
	if err != nil || user == nil {
		return s.notFound(ctx, "user_profile_edited", act, userID, detail, err)
	}

	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		// Derived exactly as the templ handler derived it, so a console that
		// leaves the field blank after setting first + last does not end up
		// with a stale "first.last+from-email" string on the row.
		displayName = strings.TrimSpace(strings.TrimSpace(in.FirstName) + " " + strings.TrimSpace(in.LastName))
	}
	if displayName == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_profile_edited",
			act, userID, user.PrimaryEmail, detail, identity.AuditOutcomeFailure, "missing_display_name"),
			"identity admin: a display name is required (or a first / last name to derive one from)")
	}

	user.DisplayName = displayName
	user.FirstName = strings.TrimSpace(in.FirstName)
	user.LastName = strings.TrimSpace(in.LastName)
	user.Phone = strings.TrimSpace(in.Phone)
	user.PrimaryRole = strings.TrimSpace(in.PrimaryRole)
	user.Gender = strings.TrimSpace(in.Gender)
	user.Birthdate = strings.TrimSpace(in.Birthdate)

	detail["firstName"] = user.FirstName
	detail["lastName"] = user.LastName
	detail["phone"] = user.Phone
	detail["primaryRole"] = user.PrimaryRole
	detail["gender"] = user.Gender
	detail["birthdate"] = user.Birthdate

	return s.finish(ctx, identity.AuditCategoryAdmin, "user_profile_edited", act, userID, user.PrimaryEmail,
		detail, "Profile saved.", s.writeUser(ctx, user))
}

// SetUserRole replaces POST /admin/users/role.
func (s *Service) SetUserRole(ctx context.Context, userId, role string) Result {
	userID := strings.TrimSpace(userId)
	newRole := strings.ToLower(strings.TrimSpace(role))
	detail := map[string]any{"userId": userID, "newRole": newRole}
	act, refusal, allowed := s.authorize(ctx, "changing a role", detail)
	if !allowed {
		return refusal
	}
	if userID == "" || !auth.IsValidRole(auth.Role(newRole)) {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "user_role_changed",
			act, userID, "", detail, identity.AuditOutcomeFailure, "invalid_role_or_user"),
			"identity admin: a user id and a valid role are required")
	}

	user, err := s.userById(ctx, userID)
	if err != nil || user == nil {
		return s.notFound(ctx, "user_role_changed", act, userID, detail, err)
	}
	detail["oldRole"] = user.Role
	user.Role = newRole

	return s.finish(ctx, identity.AuditCategoryAdmin, "user_role_changed", act, userID, user.PrimaryEmail,
		detail, "Role updated to "+newRole+".", s.writeUser(ctx, user))
}

// SetUserSuspended replaces POST /admin/users/suspend and .../unsuspend.
//
// One operation for both, because they are one decision with two values. The
// templ app needed two routes only because an HTML form cannot carry a boolean
// without a hidden input.
func (s *Service) SetUserSuspended(ctx context.Context, userId string, suspended bool, reason string) Result {
	userID := strings.TrimSpace(userId)
	action := "user_unsuspended"
	if suspended {
		action = "user_suspended"
	}
	detail := map[string]any{"userId": userID}
	if suspended {
		detail["reason"] = strings.TrimSpace(reason)
	}
	act, refusal, allowed := s.authorize(ctx, "suspending an account", detail)
	if !allowed {
		return refusal
	}
	if userID == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, action,
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_user_id"),
			"identity admin: userId is required")
	}

	user, err := s.userById(ctx, userID)
	if err != nil || user == nil {
		return s.notFound(ctx, action, act, userID, detail, err)
	}

	message := "Suspension lifted."
	if suspended {
		user.SuspendedAt = s.Now().Format(time.RFC3339Nano)
		user.SuspendedReason = strings.TrimSpace(reason)
		user.Active = false
		message = "Account suspended."
	} else {
		user.SuspendedAt = ""
		user.SuspendedReason = ""
		user.Active = true
	}

	return s.finish(ctx, identity.AuditCategoryAdmin, action, act, userID, user.PrimaryEmail,
		detail, message, s.writeUser(ctx, user))
}

func (s *Service) notFound(ctx context.Context, action string, act actor, userID string,
	detail map[string]any, readErr error) Result {
	reason := "user_not_found"
	if readErr != nil {
		reason = readErr.Error()
	}
	eventID := s.emit(ctx, identity.AuditCategoryAdmin, action, act, userID, "", detail,
		identity.AuditOutcomeFailure, reason)
	return fail(CodeNotFound, eventID, "identity admin: no such user: "+userID)
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// RevokePersonalAccessToken replaces POST /admin/tokens/revoke.
func (s *Service) RevokePersonalAccessToken(ctx context.Context, identityId string) Result {
	target := strings.TrimSpace(identityId)
	detail := map[string]any{"identityId": target}
	act, refusal, allowed := s.authorize(ctx, "revoking a personal access token", detail)
	if !allowed {
		return refusal
	}
	if target == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "pat_revoked_admin",
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_identity_id"),
			"identity admin: identityId is required")
	}

	store := &pat.Store{Engine: s.Engine, Logger: s.Logger}
	// Internal origin: the PAT queries behind LookupById / Revoke read and
	// write another person's credential row by id, which is precisely what
	// @serverOnly exists to keep a client from doing. The authorization for
	// this read happened above, in Go, against a role the interceptor
	// verified -- the stamp says "this call has already been gated", and the
	// gate is three lines up rather than in another package.
	internal := auth.ContextWithInternalOrigin(ctx)

	row, err := store.LookupById(internal, target)
	if err != nil || row == nil {
		reason := "token_not_found"
		if err != nil {
			reason = err.Error()
		}
		return fail(CodeNotFound, s.emit(ctx, identity.AuditCategoryAdmin, "pat_revoked_admin",
			act, "", "", detail, identity.AuditOutcomeFailure, reason),
			"identity admin: no such personal access token: "+target)
	}
	detail["ownerUserId"] = row.UserId
	detail["label"] = row.Label

	return s.finish(ctx, identity.AuditCategoryAdmin, "pat_revoked_admin", act, row.UserId, "",
		detail, "Token revoked.", store.Revoke(internal, target))
}

// RevokeNodeToken replaces POST /admin/tokens/node/revoke.
//
// Separate from the PAT revoke despite the identical argument, because the
// write is not the same write: a node_token row is a MACHINE credential and
// the memql#2513 credential-actor guard admits only a system actor to it. The
// operator's authorization happened at the gate above; the engine write runs
// under the system credential actor and the operator's identity is what the
// audit event records. Same split component/identity/admin/tokens.go made.
func (s *Service) RevokeNodeToken(ctx context.Context, identityId string) Result {
	target := strings.TrimSpace(identityId)
	detail := map[string]any{"identityId": target}
	act, refusal, allowed := s.authorize(ctx, "revoking a node token", detail)
	if !allowed {
		return refusal
	}
	if target == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "node_token_revoked_admin",
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_identity_id"),
			"identity admin: identityId is required")
	}

	store := &identity.Store{Engine: s.Engine, Logger: s.Logger}
	internal := auth.ContextWithInternalOrigin(ctx)

	row, err := store.LookupNodeTokenIdentityById(internal, target)
	if err != nil || row == nil {
		reason := "node_token_not_found"
		if err != nil {
			reason = err.Error()
		}
		return fail(CodeNotFound, s.emit(ctx, identity.AuditCategoryAdmin, "node_token_revoked_admin",
			act, "", "", detail, identity.AuditOutcomeFailure, reason),
			"identity admin: no such node token: "+target)
	}
	detail["nodeId"] = row.NodeId
	detail["nodeType"] = row.NodeType
	detail["mintedBy"] = row.MintedBy

	writeCtx := identity.ContextWithSystemCredentialActor(internal)
	return s.finish(ctx, identity.AuditCategoryAdmin, "node_token_revoked_admin", act, "", "",
		detail, "Node token revoked.", store.RevokeNodeTokenIdentity(writeCtx, target))
}

// ---------------------------------------------------------------------------
// Cluster settings
// ---------------------------------------------------------------------------

// ClusterSettings is the editable slice of v1:identity:clusterSettings.
//
// TTLs are the concept's own units (seconds, days for the invitation). 0 means
// "fall back to the boot-time default" -- the concept's sentinel, not a
// missing value.
type ClusterSettings struct {
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
	AccessTokenTTLSeconds     int
	RefreshTokenTTLSeconds    int
	MagicLinkTTLSeconds       int
	InvitationTTLDays         int
	RefreshCookieSameSite     string
}

// UpdateClusterSettings replaces POST /admin/settings.
//
// Only the fields this console edits are passed to the mutation. That is a
// correctness property, not a shortcut: updateClusterSettings read-merges, so
// an omitted arg inherits from the persisted row while a passed empty string
// clears. The templ handler passed `bootstrapEmail: ""` unconditionally and
// therefore wiped it on every save; omitting it preserves it.
func (s *Service) UpdateClusterSettings(ctx context.Context, in ClusterSettings) Result {
	mode := strings.TrimSpace(in.RegistrationMode)
	role := strings.ToLower(strings.TrimSpace(in.InternalDefaultRole))
	detail := map[string]any{"registrationMode": mode, "internalDefaultRole": role}

	act, refusal, allowed := s.authorize(ctx, "editing cluster settings", detail)
	if !allowed {
		return refusal
	}

	if reason, message := validateSettings(in, mode, role); reason != "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryConfiguration,
			"cluster_settings_updated", act, "", "", detail, identity.AuditOutcomeFailure, reason), message)
	}

	q := fmt.Sprintf(`mutation updateClusterSettings(`+
		`id: "cluster",`+
		`brandName: %s,`+
		`brandPrimaryColor: %s,`+
		`brandLogoDataURI: %s,`+
		`brandIconDataURI: %s,`+
		`registrationMode: %s,`+
		`registrationDomains: %s,`+
		`internalDomains: %s,`+
		`internalDefaultRole: %s,`+
		`registeredClientsJSON: %s,`+
		`accessRequestNotifyEmails: %s,`+
		`accessTokenTTLSeconds: %d,`+
		`refreshTokenTTLSeconds: %d,`+
		`magicLinkTTLSeconds: %d,`+
		`invitationTTLDays: %d,`+
		`refreshCookieSameSite: %s`+
		`)`,
		quote(in.BrandName),
		quote(in.BrandPrimaryColor),
		quote(in.BrandLogoDataURI),
		quote(in.BrandIconDataURI),
		quote(mode),
		quote(in.RegistrationDomains),
		quote(in.InternalDomains),
		quote(role),
		quote(in.RegisteredClientsJSON),
		quote(in.AccessRequestNotifyEmails),
		in.AccessTokenTTLSeconds,
		in.RefreshTokenTTLSeconds,
		in.MagicLinkTTLSeconds,
		in.InvitationTTLDays,
		quote(in.RefreshCookieSameSite),
	)
	_, err := s.Engine.Execute(ctx, q)

	return s.finish(ctx, identity.AuditCategoryConfiguration, "cluster_settings_updated", act, "", "",
		detail, "Settings saved.", err)
}

// validateSettings mirrors the bounds the templ form enforced. Out-of-bounds
// is an explicit reject rather than a silent clamp: an operator should know
// what they got.
func validateSettings(in ClusterSettings, mode, role string) (reason, message string) {
	switch mode {
	case "open", "domain_restricted", "invite_only", "waitlist":
	default:
		return "invalid_registration_mode",
			`identity admin: registrationMode must be one of open, domain_restricted, invite_only, waitlist`
	}
	if !auth.IsValidRole(auth.Role(role)) {
		return "invalid_internal_default_role",
			"identity admin: internalDefaultRole must be a valid cluster role"
	}
	if in.RegisteredClientsJSON != "" {
		var probe any
		if err := json.Unmarshal([]byte(in.RegisteredClientsJSON), &probe); err != nil {
			return "invalid_registered_clients_json", "identity admin: registeredClientsJSON is not valid JSON"
		}
	}
	if v := in.AccessTokenTTLSeconds; v != 0 &&
		(v < identity.MinAccessTokenTTLSeconds || v > identity.MaxAccessTokenTTLSeconds) {
		return "access_token_ttl_out_of_range",
			"identity admin: the access-token lifetime must be between 1 minute and 24 hours"
	}
	if v := in.RefreshTokenTTLSeconds; v != 0 &&
		(v < identity.MinRefreshTokenTTLSeconds || v > identity.MaxRefreshTokenTTLSeconds) {
		return "refresh_token_ttl_out_of_range",
			"identity admin: the refresh-token lifetime must be between 1 and 365 days"
	}
	if v := in.MagicLinkTTLSeconds; v != 0 &&
		(v < identity.MinMagicLinkTTLSeconds || v > identity.MaxMagicLinkTTLSeconds) {
		return "magic_link_ttl_out_of_range",
			"identity admin: the magic-link lifetime must be between 1 and 60 minutes"
	}
	if v := in.InvitationTTLDays; v != 0 &&
		(v < identity.MinInvitationTTLDays || v > identity.MaxInvitationTTLDays) {
		return "invitation_ttl_out_of_range",
			"identity admin: the invitation lifetime must be between 1 and 90 days"
	}
	switch in.RefreshCookieSameSite {
	case "", "lax", "none":
	default:
		return "invalid_samesite", "identity admin: the refresh cookie SameSite must be lax or none"
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// Engine access
// ---------------------------------------------------------------------------

// userRow is the internal projection of a v1:identity:user row. Only the
// fields updateUser writes back live here -- the console reads everything else
// through the ordinary gated query surface.
type userRow struct {
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
}

// userById reads one person by id.
//
// userByIdSystem is @serverOnly and takes a caller-supplied id, so this is a
// cross-user read by construction and the internal-origin stamp is what makes
// it run. Its whole safety argument is that authorize() has already refused
// anyone below owner/admin -- every caller of this method is downstream of
// that check in the same function. Do not call it from a path that is not.
func (s *Service) userById(ctx context.Context, userID string) (*userRow, error) {
	res, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx),
		fmt.Sprintf(`query userByIdSystem(userId: %s)`, quote(userID)))
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return nil, nil
	}
	n := res.Bundle.Nodes[0]
	if n == nil || n.Payload == nil {
		return nil, nil
	}
	fields := n.Payload.GetFields()
	str := func(k string) string {
		if v, present := fields[k]; present && v != nil {
			return strings.TrimSpace(v.GetStringValue())
		}
		return ""
	}
	boolean := func(k string, missing bool) bool {
		v, present := fields[k]
		if !present || v == nil {
			return missing
		}
		return v.GetBoolValue()
	}
	return &userRow{
		ID:           n.GetId(),
		DisplayName:  str("displayName"),
		FirstName:    str("firstName"),
		LastName:     str("lastName"),
		PrimaryEmail: str("primaryEmail"),
		Phone:        str("phone"),
		PrimaryRole:  str("primaryRole"),
		Gender:       str("gender"),
		Birthdate:    str("birthdate"),
		Role:         str("role"),
		Internal:     boolean("internal", false),
		// active defaults to true on the concept, so a missing field means
		// active. Reading it as false would have the console claim every
		// legacy row is suspended.
		Active:          boolean("active", true),
		SuspendedAt:     str("suspendedAt"),
		SuspendedReason: str("suspendedReason"),
	}, nil
}

// writeUser persists the merged row through updateUser.
//
// The internal-origin stamp is load-bearing: updateUser is @serverOnly
// (memql#2991) precisely because it names a target id and takes a payload
// splat that can carry `role`. Reaching it requires having been gated first,
// which authorize() did.
func (s *Service) writeUser(ctx context.Context, u *userRow) error {
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
	// Written unconditionally, including empty, so lifting a suspension
	// actually clears the stamp rather than leaving a reinstated account
	// carrying the reason it was suspended for.
	payload["suspendedAt"] = u.SuspendedAt
	payload["suspendedReason"] = u.SuspendedReason

	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	q := fmt.Sprintf(`mutation updateUser(userId: %s, payload: %s)`, quote(u.ID), string(encoded))
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return nil
}

// quote renders a Go string as a MemQL string literal. %q is the same escape
// grammar the identity store and the templ admin handlers use for every
// caller-supplied value that reaches a query string.
func quote(s string) string { return fmt.Sprintf("%q", s) }
