package adminops

// The admin issuer for enrolment links (memql#3408).
//
// WHY IT LIVES HERE AND NOT ON AN HTTP ENDPOINT. Minting an enrolment link is
// the ability to hand somebody a credential for another person's account. That
// is an owner/admin decision of exactly the kind this package exists to gate:
// the gate is Go, it is one implementation, and it writes one audit event per
// call including the refusals. A new HTTP route would need its own copy of the
// gate and its own audit trail, and would be reachable from the identity node
// only -- while the portal, where an admin actually clicks, is served by the
// bff. So it rides IdentityAdminMsg on MemqlService.Stream like the other six
// operations (memql#3324's pattern).
//
// HTTPS IS REQUIRED ON ISSUE, and here that means something stronger than a
// per-request check. The redeem side has an http.Request to inspect, so it
// runs identity.RequestIsSecure. This side has no request -- it arrives over
// the gRPC stream -- so the check moves to the ARTIFACT: refuse to mint at all
// unless the link this call would produce is an https:// URL. A plaintext
// enrolment URL is never emitted, which is a strictly stronger property than
// refusing to serve one after the fact.

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/enrolment"
)

// EnrolmentLink is the issue request.
type EnrolmentLink struct {
	// UserId is the account the link will enrol a passkey into.
	UserId string
	// TTLSeconds overrides the default lifetime. 0 uses enrolment.DefaultTTL;
	// anything above enrolment.MaxTTL is clamped down to it.
	TTLSeconds int
	// SourceIP is the origin address, for the audit trail and the row.
	SourceIP string
}

// IssueEnrolmentLink mints a single-use enrolment token for another user and
// returns the link that redeems it.
//
// The plaintext is returned ONCE, on this Result, and is never persisted:
// enrolment.Mint hands back the token and its SHA-256 digest, and only the
// digest reaches the row.
func (s *Service) IssueEnrolmentLink(ctx context.Context, in EnrolmentLink) Result {
	userID := strings.TrimSpace(in.UserId)
	detail := map[string]any{"userId": userID}

	act, refusal, allowed := s.authorize(ctx, "issuing an enrolment link", detail)
	if !allowed {
		return refusal
	}
	if userID == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "enrolment_link_issued",
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_user_id"),
			"identity admin: userId is required")
	}

	base, baseErr := s.enrolmentBaseURL(ctx)
	if baseErr != "" {
		// Refused BEFORE the row is written. Minting a token whose link cannot
		// be composed would leave a live credential in the database that
		// nobody can use and nobody knows exists.
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "enrolment_link_issued",
			act, userID, "", detail, identity.AuditOutcomeFailure, "identity_base_url_unusable"), baseErr)
	}

	// The target must exist. Minting against a typo'd id would produce a link
	// that authorizes a passkey onto an account that is not there, and the
	// failure would only surface when the holder had already followed it.
	user, err := s.userById(ctx, userID)
	if err != nil || user == nil {
		return s.notFound(ctx, "enrolment_link_issued", act, userID, detail, err)
	}

	ttl := enrolment.ClampTTL(time.Duration(in.TTLSeconds) * time.Second)
	now := s.Now()
	expiresAt := now.Add(ttl)
	detail["expiresAt"] = expiresAt.Format(time.RFC3339Nano)
	detail["ttlSeconds"] = int(ttl / time.Second)

	plain, hash, err := enrolment.Mint()
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "enrolment_link_issued", act, userID,
			user.PrimaryEmail, detail, "", fmt.Errorf("enrolment token mint: %w", err))
	}
	enrolmentID, err := enrolment.NewId()
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "enrolment_link_issued", act, userID,
			user.PrimaryEmail, detail, "", fmt.Errorf("enrolment id mint: %w", err))
	}

	store := &enrolment.Store{Engine: s.Engine, Logger: s.Logger}
	if err := store.Create(ctx, enrolmentID, userID, hash, act.userID, expiresAt, in.SourceIP); err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "enrolment_link_issued", act, userID,
			user.PrimaryEmail, detail, "", err)
	}
	detail["enrolmentTokenId"] = enrolment.CanonicalId(enrolmentID)

	res := s.finish(ctx, identity.AuditCategoryAdmin, "enrolment_link_issued", act, userID, user.PrimaryEmail,
		detail, "Enrolment link issued. Copy it now -- it is not shown again.", nil)
	// The URL rides the Result and NOT the audit detail. The audit trail
	// records that a link was issued, to whom, by whom and when; recording the
	// link itself would put a live credential into an append-only log that is
	// deliberately hard to redact.
	res.EnrolmentURL = enrolmentURL(base, plain)
	return res
}

// RevokeEnrolmentLink kills an unused enrolment token before its TTL runs out.
//
// The counterpart to issuing: a link sent to the wrong address, or thought
// better of, stops working now instead of in fifteen minutes. Consuming and
// revoking are kept distinct all the way down so the /enroll page can tell a
// holder which of the two happened.
func (s *Service) RevokeEnrolmentLink(ctx context.Context, enrolmentId string) Result {
	target := strings.TrimSpace(enrolmentId)
	detail := map[string]any{"enrolmentTokenId": target}

	act, refusal, allowed := s.authorize(ctx, "revoking an enrolment link", detail)
	if !allowed {
		return refusal
	}
	if target == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "enrolment_link_revoked",
			act, "", "", detail, identity.AuditOutcomeFailure, "missing_enrolment_id"),
			"identity admin: enrolmentTokenId is required")
	}

	store := &enrolment.Store{Engine: s.Engine, Logger: s.Logger}
	return s.finish(ctx, identity.AuditCategoryAdmin, "enrolment_link_revoked", act, "", "",
		detail, "Enrolment link revoked.", store.Revoke(ctx, target, s.Now()))
}

// enrolmentBaseURL resolves and validates the public identity origin. Returns
// ("", reason) when a usable https base URL is not available; the reason is
// written straight back to the operator, so it names the fix.
func (s *Service) enrolmentBaseURL(ctx context.Context) (string, string) {
	if s.IdentityBaseURL == nil {
		return "", "identity admin: this node cannot compose an enrolment link -- no identity base URL is wired"
	}
	base := strings.TrimRight(strings.TrimSpace(s.IdentityBaseURL(ctx)), "/")
	if base == "" {
		return "", "identity admin: the public identity URL is not configured -- set MEMQL_IDENTITY_BASE_URL, " +
			"or set the cluster domain in cluster settings so it can be derived"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return "", "identity admin: the configured identity base URL is not a usable URL: " + base
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		// The link carries a plaintext bearer in a query string. Over http it
		// would be readable by every hop between the holder and the cluster,
		// and would sit in their proxy's access log afterwards.
		return "", "identity admin: enrolment links must be https (the link carries a credential); " +
			"the configured identity base URL is " + base
	}
	return base, ""
}

// enrolmentURL composes the redeem link. The token goes in the `code` query
// parameter -- the same place GET /enroll reads it from.
func enrolmentURL(base, plainToken string) string {
	return base + "/enroll?code=" + url.QueryEscape(plainToken)
}
