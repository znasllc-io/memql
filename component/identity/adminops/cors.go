package adminops

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
)

// cors.go -- the owner/admin grant of credentialed cross-origin access
// (memql#3716).
//
// # WHY THIS IS AN OPERATION AND NOT A DERIVATION
//
// The allowlist it writes gates CREDENTIALED cross-origin requests: an allowed
// origin can make cookie-bearing calls to identity's auth endpoints and READ
// THE RESPONSES. The cheap way to make that allowlist graph-backed would be to
// derive it from the registered OAuth clients' redirect URIs -- and `POST
// /register` (RFC 7591 dynamic client registration) is deliberately
// UNAUTHENTICATED and on by default. Chain the two and one anonymous POST
// carrying `redirect_uris: ["https://evil.example/cb"]` buys credentialed read
// access to the auth surface. That is not a refactor, it is a privilege
// escalation, so registration stays open and the ALLOWANCE lands here: on the
// same row, behind the same owner/admin gate and the same audit trail as every
// other operation in this package.
//
// # WHY EXPLICIT ORIGINS AND NOT A BOOLEAN
//
// A `corsAllowed` flag deriving the origin from the redirect URI would, on any
// client with a loopback redirect, admit `http://127.0.0.1` -- and loopback
// redirects are NORMAL on this concept, because component/identity/config.go
// implements the RFC 8252 loopback-any-port exception for exactly them. An
// admin flipping that switch would be granting every local process on somebody's
// machine cookie-bearing read access to identity with no way to see that they
// had. Naming the origins also lets an allowance differ from the redirect's
// origin, which it legitimately does when a SPA redirects to
// https://app.example.com/cb while also running at https://www.example.com.

// SetOAuthClientCORSOrigins grants or revokes credentialed cross-origin access
// for one registered OAuth client.
//
// origins is the COMPLETE allowance rather than an addition to it, so an empty
// list is the revoke. One operation for both directions, for the reason
// SetUserSuspended is one operation: they are one decision with two values, and
// a separate revoke verb is a second code path that can fall out of step with
// this one.
//
// Every entry is validated here, on the way in, where the caller is a person who
// can be told which one is wrong -- rather than only on the way out, where the
// read runs on an anonymous preflight with nobody to tell. The whole write is
// refused on the first bad entry, naming it: a partially-applied allowance is
// the outcome nobody can reason about afterwards.
func (s *Service) SetOAuthClientCORSOrigins(ctx context.Context, clientId string, origins []string) Result {
	target := strings.TrimSpace(clientId)
	// The action name distinguishes the two directions, so an operator greps a
	// trust decision rather than a field edit. Same split SetUserSuspended
	// makes, and for the same reason: "who granted this" and "who took it away"
	// are different questions.
	action := "oauth_client_cors_granted"
	verb := "granting cross-origin access"
	if len(origins) == 0 {
		action = "oauth_client_cors_revoked"
		verb = "revoking cross-origin access"
	}
	// Recorded BEFORE the gate so a refusal audits what was attempted. The
	// clientId is carried in the detail rather than as the event's targetId
	// because emit() types a non-empty target as a "user", and an OAuth client
	// is not one -- same reason RevokeNodeToken keeps the node id in detail.
	detail := map[string]any{"clientId": target, "origins": origins}

	act, refusal, allowed := s.authorize(ctx, verb, detail)
	if !allowed {
		return refusal
	}
	if target == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryConfiguration, action,
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_client_id"),
			"identity admin: clientId is required")
	}

	canonical, err := identity.ValidateCORSOrigins(origins)
	if err != nil {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryConfiguration, action,
			act, "", "", detail, identity.AuditOutcomeFailure, "invalid_cors_origin"),
			"identity admin: "+err.Error())
	}
	// The canonical forms are what gets stored, so they are what the trail must
	// record -- an audit event quoting the operator's input while the row holds
	// something else is an audit event that answers the wrong question.
	detail["origins"] = canonical

	store := &identity.Store{Engine: s.Engine, Logger: s.Logger}
	// Internal origin: oAuthClientByClientId is an ordinary read, but this call
	// is the existence check for a write, and stamping the pair identically
	// keeps the read from being reachable on a looser footing than the write it
	// guards. The write itself REQUIRES the stamp -- setOAuthClientCORSOrigins
	// is @serverOnly -- and its safety argument is that authorize() above has
	// already refused anyone below owner/admin.
	internal := auth.ContextWithInternalOrigin(ctx)

	row, err := store.LookupOAuthClientByClientId(internal, target)
	if err != nil || row == nil {
		// Refused rather than written blind. setOAuthClientCORSOrigins is a
		// partial update keyed on the row id, so a mistyped clientId would
		// otherwise leave an allowance attached to nothing -- an origin an
		// operator believes is granted, sitting on no client, invisible on the
		// list they would check.
		reason := "oauth_client_not_found"
		if err != nil {
			reason = err.Error()
		}
		return fail(CodeNotFound, s.emit(ctx, identity.AuditCategoryConfiguration, action,
			act, "", "", detail, identity.AuditOutcomeFailure, reason),
			"identity admin: no such registered OAuth client: "+target+
				" (statically configured clients carry no row -- their origins belong in "+
				"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS)")
	}
	detail["clientName"] = row.ClientName
	// What the allowance WAS. A revoke whose trail does not say what it removed
	// records that something changed without recording what.
	detail["previousOrigins"] = row.CORSOrigins

	message := "Cross-origin access revoked."
	if len(canonical) > 0 {
		message = "Cross-origin access granted for " + strings.Join(canonical, ", ") + "."
	}
	return s.finish(ctx, identity.AuditCategoryConfiguration, action, act, "", "",
		detail, message, store.SetOAuthClientCORSOrigins(internal, target, canonical))
}
