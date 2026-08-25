// Package adminops is the owner/admin gate for the identity writes the
// server-rendered /admin/* console used to own (memql#3324).
//
// ===========================================================================
// WHY THIS PACKAGE EXISTS AT ALL
// ===========================================================================
// Every write below already had a MemQL mutation -- updateUser,
// revokePATIdentity, revokeNodeTokenIdentity, updateClusterSettings -- and the
// portal could have called each one directly over the ordinary query surface.
// That would have deleted the gate rather than moved it.
//
// A MemQL mutation cannot carry a role predicate. `filter` is a read
// construct and there is no mutation-side spec. Row-authz does not fill the
// gap either, for two reasons that survive it having become real: it enforces
// OWNERSHIP (memql#3174 refuses a write whose target row's declared owner is
// not the actor), which is the wrong axis for writes whose entire purpose is
// an admin editing SOMEBODY ELSE'S row; and `v1:identity:user` deliberately
// declares no tier at all -- `owned` would narrow an admin's user list to the
// admin's own row and break the pre-actor lookups that build the actor in the
// first place (memql#3349 / #3350, argued at the concept in
// dsl/identity/concepts.memql). This comment used to say instead that
// enforcement did not exist, which had been false since memql#3172 (swept in
// memql#3987) and made the gate below look like a stopgap for something about
// to arrive. It is not. So the owner/admin rule these writes ran under was the
// templ console's HTTP ROUTE and nothing else:
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
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// Canonical gRPC status codes, by number. Named here rather than imported so
// this package -- which is pure policy plus engine calls -- carries no
// transport dependency. component/grpc maps them straight onto
// IdentityAdminResult.error_code, which is where the numbers are observed.
const (
	CodeOK              = 0
	CodeInvalidArgument = 3
	CodeNotFound        = 5
	// CodeFailedPrecondition is the cluster-state refusal: the request is
	// well-formed and the caller is entitled, but the cluster is not in a
	// state where the operation means anything -- rotating a recovery key on a
	// cluster with no owner (memql#3970). Distinct from InvalidArgument, which
	// says the CALLER got something wrong, and from NotFound, which says a
	// named thing is absent.
	CodeFailedPrecondition = 9
	CodePermissionDenied   = 7
	CodeInternal           = 13
	CodeUnauthenticated    = 16
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

	// RecoveryKey is the SECOND field on this Result that carries a credential,
	// set by exactly one operation (RotateRecoveryKey, memql#3970).
	//
	// It earns the exception the same way EnrolmentURL does and for the same
	// reasons: the key IS the product of that call, the plaintext exists
	// nowhere else (only its SHA-256 hash is persisted), and no later request
	// can fetch it. Empty on every other operation and on every refusal.
	//
	// One case where it is set on a NON-ok Result: the replacement was minted
	// and shown but retiring a predecessor failed. Reporting the error while
	// withholding the key would tell the caller nothing happened when
	// something did, and they would rotate again.
	RecoveryKey string

	// InvitationURL is the THIRD credential-bearing field on this Result, set
	// by exactly one operation (IssueUserInvitation, memql#4270).
	//
	// It earns the exception the same way the two above do: the link IS the
	// product of that call, only its SHA-256 hash is persisted, and no later
	// request can fetch it. Empty on every other operation and on every
	// refusal, and a surface showing it must show it ONCE.
	InvitationURL string

	// InvitationEmailSent reports whether the invitation email actually left
	// the process, set by IssueUserInvitation (memql#4584).
	//
	// NOT a credential, and not redundant with OK. OK says the invitation was
	// ISSUED -- the row is written and the link in InvitationURL admits
	// somebody. This says whether the recipient was told. The two are
	// deliberately separable: a send failure must not fail the issue, because
	// the link is the thing that actually admits and discarding it over a
	// transient Graph outage would be the worse loss.
	//
	// False with an empty InvitationEmailError means no send was attempted --
	// the node has no mail seam wired. False WITH an error means one was tried
	// and failed. A surface must be able to tell those apart: the first is a
	// configuration statement, the second is an incident.
	InvitationEmailSent bool

	// InvitationEmailError is why delivery failed, empty when it did not fail.
	//
	// Present so the failure is RETRIEVABLE rather than buried in a log line
	// on a node the operator is not tailing. The whole defect this fixes
	// (memql#4584) was an invitation that looked sent and never was; replacing
	// it with an invitation that looks sent, fails to send, and says so only
	// to slog would be the same defect wearing a different hat.
	//
	// Safe to show an operator -- it is the sender's own error text, which
	// names transport and configuration faults and never the link.
	InvitationEmailError string

	// RegistrationMode is what the cluster's policy MEANT for the call that
	// just ran, set by IssueUserInvitation. Not a credential -- it is here so a
	// console can say something true about what the link is for without
	// re-reading cluster settings and racing them. Under `open` an invitation
	// is a courtesy rather than a gate, and telling an operator that is the
	// difference between a link they understand and one they misread.
	RegistrationMode string

	// EnrolmentURL is the ONE field on this Result that carries a credential,
	// and it is set by exactly one operation (IssueEnrolmentLink, memql#3408).
	//
	// It breaks the "never quotes a credential" rule ErrorMessage states, and
	// it has to: an enrolment link IS the product of that call, the plaintext
	// exists nowhere else (only its SHA-256 hash was persisted), and there is
	// no second request that could fetch it later. The alternative -- a
	// separate message type for one operation -- would put the same value on
	// the same stream while making the exception harder to find.
	//
	// Empty on every other operation and on every refusal. A surface showing
	// it must show it ONCE and hold it nowhere else: not in storage, not in a
	// URL of its own, not in a row.
	EnrolmentURL string
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

	// IdentityBaseURL resolves the PUBLIC base URL of the identity service --
	// the origin an enrolment link has to point at (memql#3408).
	//
	// A seam rather than a string because this Service is constructed on every
	// node with an engine, and the answer is not the same on all of them. The
	// identity node knows it from MEMQL_IDENTITY_BASE_URL; the bff (which
	// serves the portal, and so is where an admin actually clicks) has only
	// MEMQL_IDENTITY_VERIFIER_BASE_URL, which is the IN-CLUSTER address
	// (https://identity:8085) and would produce a link nobody outside the
	// cluster can open. The wiring layer resolves it properly and this package
	// stays out of the environment.
	//
	// Optional: nil, empty, or non-https refuses IssueEnrolmentLink with an
	// actionable message and leaves every other operation untouched.
	IdentityBaseURL func(ctx context.Context) string

	// RegistrationPolicy resolves the cluster's registration mode and its
	// domain allowlist (memql#4270).
	//
	// A seam for the reason IdentityBaseURL is one: this Service is built on
	// every node with an engine and the config is not the same on all of them,
	// so the package stays out of the environment and the wiring layer answers.
	//
	// Optional. Unset degrades to "open" -- the mode that adds no restriction.
	// A node that cannot read the policy must not invent one, and inventing
	// invite_only here would refuse invitations on a cluster that never asked
	// for it.
	RegistrationPolicy func(ctx context.Context) (mode string, domains []string)

	// SendInvitationEmail delivers the invitation email an issued invitation is
	// useless without (memql#4584).
	//
	// A seam for the reason the two above are seams, plus one specific to mail:
	// this package cannot reach a Sender. Engine here is
	// identity.EngineExecutor -- a narrow interface over Execute -- and the
	// email integration hangs off *memqlengine.MemQLEngine's integration
	// registry. Widening Engine to the concrete type would hand the entire admin
	// surface every integration on the node in order to send one message. The
	// wiring layer points this at
	// emailsender.EngineEmailSender.SendUserInvitation, which is the SAME
	// plug-in SendMagicLink resolves, so the cluster keeps exactly one mail path.
	//
	// Optional. Unset means no email is attempted and the caller is told so on
	// the Result -- the state every install was in before memql#4584, and still
	// the right one for a node with no mail wired. It is NOT reported as a
	// delivery failure, because nothing failed: nobody asked for a send.
	SendInvitationEmail func(ctx context.Context, in InvitationEmail) error
}

// InvitationEmail is what SendInvitationEmail needs to compose the message.
//
// LinkURL is a credential, and this struct is the only place it travels
// outside IssueUserInvitation's own stack frame. It is not logged here, not
// audited here, and not stored -- see the delivery block in
// IssueUserInvitation.
type InvitationEmail struct {
	// To is the invitee address, already lowercased and validated.
	To string
	// InviterName is the display name or address of whoever issued it, taken
	// off the resolved actor rather than from caller input.
	InviterName string
	// Role is the cluster role the invitation grants. Empty means the cluster's
	// default.
	Role string
	// LinkURL is the redemption link. Shown once, stored nowhere.
	LinkURL string
	// ExpiresAt is when the link stops working.
	ExpiresAt time.Time
	// RegistrationMode is the policy in force, so a sender can word the message
	// honestly under `open` if it ever wants to.
	RegistrationMode string
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

	// SharedMailbox / SignInPolicy are the two magic-link hardening fields
	// (memql#4304). Read here so writeUser's read-merge-write does not blank
	// them: updateUser takes a payload splat, and a field this row does not
	// carry is a field the next admin edit silently resets.
	SharedMailbox bool
	SignInPolicy  string
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
		SharedMailbox:   boolean("sharedMailbox", false),
		// signInPolicy defaults to "any" on the concept; a row written before
		// the field existed carries nothing, and the safe reading of a missing
		// policy is the permissive one -- treating absence as passkey_only
		// would lock out every account that predates the field.
		SignInPolicy: str("signInPolicy"),
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
		// Written back unconditionally so an ordinary profile edit cannot
		// silently clear them. updateUser is a payload splat: a field absent
		// from the map is a field the write resets.
		"sharedMailbox": u.SharedMailbox,
		"signInPolicy":  signInPolicyOrDefault(u.SignInPolicy),
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

// quote renders a Go string as a MemQL string literal.
//
// It was `fmt.Sprintf("%q", s)` until memql#3611, on the stated belief that %q
// "is the same escape grammar" the rest of the identity path used. It is not
// the lexer's grammar, and the rest of the identity path was wrong in the same
// way. Go's %q emits `\x00`, `\a`, `\v` and `\xNN`; readString implements the
// JSON escape set and rejects every one of them, so a single control byte or
// one invalid UTF-8 byte anywhere in a value made the WHOLE statement
// unparseable and the write never happened. Every value below is
// operator-supplied over the wire -- a brand name, a pasted data-URI, a
// registered-clients JSON blob -- so none of them is a value this package gets
// to assume is clean.
//
// One helper, so one edit fixed all eleven call sites; that is the only good
// thing about having had it.
func quote(s string) string { return langparser.QuoteString(s) }

// ResetSignInPolicy puts one user's sign-in policy back to "any"
// (memql#4304).
//
// # The rescue path, and only that
//
// passkey_only disables sign-in LINKS for an account. Its whole value is
// that a shared mailbox stops being a way in -- and its whole risk is that
// somebody turns it on, loses their passkey, and has nothing left. The
// enrolment token and the owner recovery key are the designed answers to
// that, but this is the cheap one: turn links back on and let the person
// sign in the ordinary way.
//
// # One direction, deliberately
//
// There is no admin path to turn passkey_only ON for somebody else. That
// call would let an admin lock a colleague out of their own account, and
// nothing operational needs it: the control belongs to the person whose
// passkey it is. Expressing only the reset in the message shape means the
// wrong direction is not a rule that can be got wrong -- it is not
// representable.
//
// Trust level: the same as issuing an enrolment token for the user, which
// owners and admins already can.
func (s *Service) ResetSignInPolicy(ctx context.Context, userId string) Result {
	userID := strings.TrimSpace(userId)
	const action = "sign_in_policy_reset_by_admin"
	detail := map[string]any{"userId": userID, "to": "any"}

	act, refusal, allowed := s.authorize(ctx, "resetting a sign-in policy", detail)
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
	detail["from"] = user.SignInPolicy
	if user.SignInPolicy != "passkey_only" {
		// Already permissive. Reported as success rather than refused: the
		// caller asked for a state, the state holds, and an operator running
		// this against the wrong account should not be told "failed" when
		// nothing was wrong.
		return ok(s.emit(ctx, identity.AuditCategoryAdmin, action, act, userID, user.PrimaryEmail,
			detail, identity.AuditOutcomeSuccess, ""),
			"Sign-in links were already on for this account.")
	}

	user.SignInPolicy = "any"
	return s.finish(ctx, identity.AuditCategoryAdmin, action, act, userID, user.PrimaryEmail,
		detail, "Sign-in links turned back on.", s.writeUser(ctx, user))
}

// SetUserSharedMailbox sets or clears the shared-mailbox hint (memql#4304).
//
// The flag gates nothing -- it drives copy -- so both directions are
// available, unlike the policy above. The heuristic that seeds it at
// registration is a guess (`info@` belongs to plenty of solo operators), and
// an admin has to be able to correct it either way.
func (s *Service) SetUserSharedMailbox(ctx context.Context, userId string, shared bool) Result {
	userID := strings.TrimSpace(userId)
	const action = "shared_mailbox_changed"
	detail := map[string]any{"userId": userID, "by": "admin", "to": shared}

	act, refusal, allowed := s.authorize(ctx, "changing a shared-mailbox flag", detail)
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
	detail["from"] = user.SharedMailbox

	user.SharedMailbox = shared
	message := "Marked as a shared mailbox."
	if !shared {
		message = "No longer marked as a shared mailbox."
	}
	return s.finish(ctx, identity.AuditCategoryAdmin, action, act, userID, user.PrimaryEmail,
		detail, message, s.writeUser(ctx, user))
}

// signInPolicyOrDefault normalizes a missing policy to the permissive one.
//
// The concept declares @default("any"), but a default is not applied on
// insert or on a payload-splat update -- so writing an empty string back
// would store an empty string, and every later reader would have to know
// that empty means "any". Normalizing here keeps that knowledge in one
// place.
func signInPolicyOrDefault(policy string) string {
	policy = strings.TrimSpace(policy)
	if policy != "passkey_only" {
		return "any"
	}
	return policy
}
