// Package memql guest-invite handlers.
//
// Two gRPC handlers live here plus a small invitation-lookup helper:
//
//   - handleSendGuestInvite: authenticated space owner flow. Creates a
//     guest invitation record and triggers the invitation email. See
//     SendGuestInviteMsg in memql.proto.
//   - handleResolveGuestInvite: public token resolution. Pre-populates
//     the /join/<token> page with space + inviter + guest metadata.
//
// The full design + threat model is in
// docs/planning/GUEST_INVITE_SPECS.md.

package memql

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// Default lifetime of a fresh guest-invite token when the caller
// didn't specify one. Short by design -- guests are expected to
// click through the email within a few minutes; expired invitations
// get swept by the `expireGuestInvitations` automation
// (automations/v1/copresent/) and removed from the pending list.
const guestInviteDefaultTTL = 5 * time.Minute

// Hard upper bound on any caller-specified TTL. Scheduled / far-
// future invitations ("we'll meet next Tuesday") are a separate
// feature that will carry a different scope and permission set.
const guestInviteMaxTTL = 24 * time.Hour

// invitationSummary is the minimal projection the guest handlers (and
// later the guest auth path) read back from an invitation record.
type invitationSummary struct {
	ID           string
	Kind         string
	Status       string
	SpaceId      string
	SpaceName    string
	InviterId    string
	InviterName  string
	InviteeEmail string
	InviteeName  string
	Token        string // plain token -- read server-side to re-send the same email on resend
	TokenHash    string
	ExpiresAt    time.Time
	RespondedAt  time.Time
	CreatedAt    time.Time
}

// mintGuestToken returns a fresh 32-byte base64url token and its hex
// SHA-256 hash. The plain token is emitted exactly once (in the email
// link); only the hash is persisted.
func mintGuestToken() (plain, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("guest token rng: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plain))
	hash = hex.EncodeToString(sum[:])
	return plain, hash, nil
}

// hashGuestToken computes the hash used for lookup on an inbound plain
// token. Always use this helper so the algorithm stays consistent.
func hashGuestToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// resolveEmailSender returns the sender from the registered email
// integration plug-in, or a LogSender fallback in dev. Accessing the
// plug-in via the engine's registry means app/transport doesn't have
// to wire the sender explicitly -- email is a self-registering plug-in
// in integrations/email/plugin.go.
func (s *streamSession) resolveEmailSender() email.Sender {
	if s == nil || s.service == nil || s.service.engine == nil {
		return email.NewLogSender(s.logger)
	}
	prov := s.service.engine.Integrations().Provider("email")
	emailInt, _ := prov.(*email.Integration)
	if emailInt == nil {
		return email.NewLogSender(s.logger)
	}
	if sender := emailInt.SenderAccess(); sender != nil {
		return sender
	}
	return email.NewLogSender(s.logger)
}

// lookupInvitationByTokenHash runs queryInvitationByTokenHash and
// returns the first matching invitation (or nil when no match). Used
// by handleResolveGuestInvite + the guest auth middleware.
func lookupInvitationByTokenHash(ctx context.Context, engine *memqlengine.MemQLEngine, tokenHash string) (*invitationSummary, error) {
	if engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(tokenHash) == "" {
		return nil, fmt.Errorf("tokenHash required")
	}
	query := fmt.Sprintf(`queryInvitationByTokenHash({tokenHash: "%s"})`, tokenHash)
	result, err := engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query guest invitation: %w", err)
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, nil
	}
	node := result.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return nil, nil
	}
	fields := node.Payload.GetFields()
	if fields == nil {
		return nil, nil
	}

	getStr := func(key string) string {
		v, ok := fields[key]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(v.GetStringValue())
	}
	parseTime := func(key string) time.Time {
		s := getStr(key)
		if s == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}

	summary := &invitationSummary{
		ID:           getStr("id"),
		Kind:         getStr("kind"),
		Status:       getStr("status"),
		SpaceId:      getStr("spaceId"),
		SpaceName:    getStr("spaceName"),
		InviterId:    getStr("inviterId"),
		InviterName:  getStr("inviterName"),
		InviteeEmail: getStr("inviteeEmail"),
		InviteeName:  getStr("inviteeName"),
		Token:        getStr("token"),
		TokenHash:    getStr("tokenHash"),
		ExpiresAt:    parseTime("expiresAt"),
		RespondedAt:  parseTime("respondedAt"),
		CreatedAt:    parseTime("createdAt"),
	}
	if summary.ID == "" {
		// Shape sometimes puts id at the top level; fall back to node.id.
		summary.ID = node.Id
	}
	return summary, nil
}

// listPendingGuestInvitationsForEmail returns every pending guest
// invitation in `spaceId` whose inviteeEmail matches `email`
// (case-insensitive). Used by handleSendGuestInvite to clean up stale
// rows before creating a fresh invitation for the same recipient.
func listPendingGuestInvitationsForEmail(ctx context.Context, engine *memqlengine.MemQLEngine, spaceId, email string) ([]*invitationSummary, error) {
	if engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(spaceId) == "" {
		return nil, fmt.Errorf("spaceId required")
	}
	needle := strings.ToLower(strings.TrimSpace(email))
	if needle == "" {
		return nil, nil
	}
	// spaceInvitations is shape-projected via invitationFull, which
	// exposes the fields we need to re-kick the record preserving its
	// guest context.
	query := fmt.Sprintf(`querySpaceInvitations({spaceId: %q})`, spaceId)
	result, err := engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query space invitations: %w", err)
	}
	if result == nil || result.Bundle == nil {
		return nil, nil
	}
	out := make([]*invitationSummary, 0)
	for _, node := range result.Bundle.Nodes {
		if node == nil || node.Payload == nil {
			continue
		}
		fields := node.Payload.GetFields()
		if fields == nil {
			continue
		}
		getStr := func(key string) string {
			v, ok := fields[key]
			if !ok || v == nil {
				return ""
			}
			return strings.TrimSpace(v.GetStringValue())
		}
		if strings.ToLower(getStr("kind")) != "guest" {
			continue
		}
		if strings.ToLower(getStr("status")) != "pending" {
			continue
		}
		if strings.ToLower(getStr("inviteeEmail")) != needle {
			continue
		}
		id := getStr("id")
		if id == "" {
			id = node.Id
		}
		// Parse expiresAt in-place (listPendingGuestInvitationsForEmail
		// callers care about whether the record is still in-window).
		parseTime := func(key string) time.Time {
			s := getStr(key)
			if s == "" {
				return time.Time{}
			}
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return time.Time{}
			}
			return t
		}
		out = append(out, &invitationSummary{
			ID:           id,
			Kind:         "guest",
			Status:       "pending",
			SpaceId:      getStr("spaceId"),
			SpaceName:    getStr("spaceName"),
			InviterId:    getStr("inviterId"),
			InviterName:  getStr("inviterName"),
			InviteeEmail: getStr("inviteeEmail"),
			InviteeName:  getStr("inviteeName"),
			Token:        getStr("token"),
			TokenHash:    getStr("tokenHash"),
			ExpiresAt:    parseTime("expiresAt"),
		})
	}
	return out, nil
}

// kickGuestInvitation marks a single guest invitation as kicked by
// running mutationMarkGuestInvitationKicked. Preserves all guest
// fields from `inv` so the latest-wins projection keeps the context.
func kickGuestInvitation(ctx context.Context, engine *memqlengine.MemQLEngine, inv *invitationSummary) error {
	if inv == nil || strings.TrimSpace(inv.ID) == "" {
		return fmt.Errorf("invitation summary required")
	}
	args := map[string]any{
		"invitationId": inv.ID,
		"spaceId":      inv.SpaceId,
		"spaceName":    inv.SpaceName,
		"inviterId":    inv.InviterId,
		"inviterName":  inv.InviterName,
		"inviteeEmail": inv.InviteeEmail,
		"inviteeName":  inv.InviteeName,
		"tokenHash":    inv.TokenHash,
	}
	argsJSON, _ := json.Marshal(args)
	query := fmt.Sprintf("mutationMarkGuestInvitationKicked(%s)", renderQueryArgs(argsJSON))
	if _, err := engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("kick guest invitation: %w", err)
	}
	return nil
}

// handleSendGuestInvite creates a guest invitation + triggers email
// delivery. Caller must be an authenticated user (the stream
// interceptor has run); space-membership enforcement is left to the
// partition check on envelope.partition, which the caller populates
// with space_id.
func (s *streamSession) handleSendGuestInvite(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SendGuestInviteMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	sendResult := func(result *memqlv1.SendGuestInviteResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SendGuestInviteResult{
				SendGuestInviteResult: result,
			},
		})
	}

	spaceId := strings.TrimSpace(msg.GetSpaceId())
	emailAddr := strings.ToLower(strings.TrimSpace(msg.GetEmail()))
	joinURLBase := strings.TrimRight(strings.TrimSpace(msg.GetJoinUrlBase()), "/")

	if spaceId == "" {
		return sendResult(&memqlv1.SendGuestInviteResult{ErrorCode: "invalid_space", ErrorMessage: "space_id is required"})
	}
	if _, err := mail.ParseAddress(emailAddr); err != nil {
		return sendResult(&memqlv1.SendGuestInviteResult{ErrorCode: "invalid_email", ErrorMessage: fmt.Sprintf("invalid email: %v", err)})
	}
	if joinURLBase == "" {
		return sendResult(&memqlv1.SendGuestInviteResult{ErrorCode: "invalid_join_url", ErrorMessage: "join_url_base is required"})
	}

	if s.service.engine == nil {
		return sendResult(&memqlv1.SendGuestInviteResult{ErrorCode: "unavailable", ErrorMessage: "engine not configured"})
	}

	// Resolve the inviter's user ID from the auth claims.
	inviterId := ""
	if ac := s.ensureAccess(s.stream.Context()); ac != nil {
		inviterId = ac.UserId
	}
	if inviterId == "" {
		return sendResult(&memqlv1.SendGuestInviteResult{ErrorCode: "forbidden", ErrorMessage: "caller has no resolved identity"})
	}

	ctx := contextWithSystemActor(s.stream.Context())

	// Dedupe: if there's already a pending (non-expired) guest
	// invitation for this email in this space, don't create a new
	// one. The UI should surface the existing row (with resend +
	// revoke buttons) instead of piling up duplicates. Expired
	// pendings are ignored -- they'll be reaped by the expiry sweep
	// and the caller can create a fresh one.
	existing, err := listPendingGuestInvitationsForEmail(ctx, s.service.engine, spaceId, emailAddr)
	if err != nil && s.logger != nil {
		s.logger.Warn("guest_invite: failed to list existing pendings", "error", err, "email", emailAddr)
	}
	now := time.Now().UTC()
	for _, old := range existing {
		if !old.ExpiresAt.IsZero() && now.After(old.ExpiresAt) {
			continue
		}
		return sendResult(&memqlv1.SendGuestInviteResult{
			InvitationId: old.ID,
			ErrorCode:    "already_pending",
			ErrorMessage: "An invitation for this email is already pending. Revoke or resend it instead.",
		})
	}

	// Mint a fresh opaque token and its hash. Hash gates the guest-
	// auth middleware; the plain token gets stored alongside so the
	// "resend email" button can re-send the same /join/<token> URL
	// without creating a new invitation.
	plainToken, tokenHash, err := mintGuestToken()
	if err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "guest_invite: token mint", err)
	}

	invitationId := "v1:identity:invitation:" + uuid.NewString()
	// Caller-selectable TTL in whole minutes; fall back to default
	// when unset / non-positive. Clamp to a sane upper bound so a
	// malformed client can't persist a year-long guest invite.
	ttl := guestInviteDefaultTTL
	if reqMinutes := msg.GetExpiresInMinutes(); reqMinutes > 0 {
		ttl = time.Duration(reqMinutes) * time.Minute
		if ttl > guestInviteMaxTTL {
			ttl = guestInviteMaxTTL
		}
	}
	expiresAt := now.Add(ttl)

	// Persist the invitation.
	args := map[string]any{
		"invitationId": invitationId,
		"spaceId":      spaceId,
		"spaceName":    strings.TrimSpace(msg.GetSpaceName()),
		"inviterId":    inviterId,
		"inviterName":  strings.TrimSpace(msg.GetInviterName()),
		"inviteeEmail": emailAddr,
		"inviteeName":  strings.TrimSpace(msg.GetGuestName()),
		"token":        plainToken,
		"tokenHash":    tokenHash,
		"expiresAt":    expiresAt.Format(time.RFC3339),
	}
	argsJSON, _ := json.Marshal(args)
	query := fmt.Sprintf("mutationCreateGuestInvitation(%s)", renderQueryArgs(argsJSON))

	if _, err := s.service.engine.Execute(ctx, query); err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "guest_invite: persist invitation", err)
	}

	// Fire the email. Fall back to a LogSender so dev doesn't crash.
	sender := s.resolveEmailSender()
	joinURL := joinURLBase + "/join/" + plainToken
	if err := email.SendGuestInvite(ctx, sender, email.GuestInviteParams{
		To:          emailAddr,
		GuestName:   strings.TrimSpace(msg.GetGuestName()),
		InviterName: strings.TrimSpace(msg.GetInviterName()),
		SpaceName:   strings.TrimSpace(msg.GetSpaceName()),
		JoinURL:     joinURL,
		ExpiresAt:   expiresAt,
	}); err != nil {
		// The invitation exists; surface a delivery-specific error so
		// the caller can retry or show a degraded state.
		if s.logger != nil {
			s.logger.Error("guest invite email delivery failed", "error", err, "invitation_id", invitationId)
		}
		return sendResult(&memqlv1.SendGuestInviteResult{
			Success:      false,
			InvitationId: invitationId,
			ErrorCode:    "email_failed",
			ErrorMessage: err.Error(),
		})
	}

	return sendResult(&memqlv1.SendGuestInviteResult{
		Success:      true,
		InvitationId: invitationId,
	})
}

// handleResolveGuestInvite validates the plain token presented by the
// /join/<token> page and returns guest-visible metadata so the page
// can render before asking for a display name.
func (s *streamSession) handleResolveGuestInvite(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ResolveGuestInviteMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.ResolveGuestInviteResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ResolveGuestInviteResult{
				ResolveGuestInviteResult: result,
			},
		})
	}

	plain := strings.TrimSpace(msg.GetToken())
	if plain == "" {
		return send(&memqlv1.ResolveGuestInviteResult{Status: "invalid", ErrorMessage: "token required"})
	}
	if s.service.engine == nil {
		return send(&memqlv1.ResolveGuestInviteResult{Status: "invalid", ErrorMessage: "engine not configured"})
	}

	ctx := contextWithSystemActor(s.stream.Context())
	summary, err := lookupInvitationByTokenHash(ctx, s.service.engine, hashGuestToken(plain))
	if err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "guest_invite: lookup", err)
	}
	if summary == nil {
		return send(&memqlv1.ResolveGuestInviteResult{Status: "invalid", ErrorMessage: "no invitation matches this token"})
	}

	// Partition is going away on the wire in #56 phase 8; the
	// /join/<token> page no longer needs to adopt it. The proto field
	// stays populated with "" for compat until the field itself is
	// removed.
	partition := ""

	// Derive status from the record.
	result := &memqlv1.ResolveGuestInviteResult{
		InvitationId: summary.ID,
		SpaceId:      summary.SpaceId,
		SpaceName:    summary.SpaceName,
		InviterName:  summary.InviterName,
		InviteeEmail: summary.InviteeEmail,
		InviteeName:  summary.InviteeName,
		Partition:    partition,
	}
	if !summary.ExpiresAt.IsZero() {
		result.ExpiresAt = timestamppb.New(summary.ExpiresAt)
	}

	switch strings.ToLower(summary.Status) {
	case "pending":
		if !summary.ExpiresAt.IsZero() && time.Now().After(summary.ExpiresAt) {
			result.Status = "expired"
		} else {
			result.Status = "ok"
		}
	case "accepted":
		result.Status = "already_accepted"
	case "kicked", "dismissed", "ignored", "left":
		result.Status = "cancelled"
	default:
		result.Status = "invalid"
	}

	return send(result)
}

// handleJoinSpaceAsGuest atomically marks a guest invitation accepted
// and creates the Participant record. The stream interceptor already
// validated the guest token and stashed the invitation + spaceId in
// the claims map (see guest_stream_interceptor.go); this handler
// reads them back out, runs the two DSL mutations via engine.Execute,
// and replies.
//
// Splits into two single-insert mutations because the memQL DSL
// function body takes a single expression -- two inserts in one
// function don't parse. Running them back-to-back on the server is
// the next-best thing to atomic: both mutations are idempotent under
// append-only memQL semantics, so a partial failure just means the
// guest can retry.
func (s *streamSession) handleJoinSpaceAsGuest(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.JoinSpaceAsGuestMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.JoinSpaceAsGuestResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_JoinSpaceAsGuestResult{
				JoinSpaceAsGuestResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.JoinSpaceAsGuestResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	participantId := strings.TrimSpace(msg.GetParticipantId())
	displayName := strings.TrimSpace(msg.GetDisplayName())
	if participantId == "" || displayName == "" {
		return send(&memqlv1.JoinSpaceAsGuestResult{
			ErrorCode:    "invalid_arguments",
			ErrorMessage: "participantId and displayName are required",
		})
	}

	// Pull the invitation + space info the interceptor resolved.
	claims, _ := auth.ClaimsFromContext(s.stream.Context())
	guest, ok := claims[GuestAuthClaimKey].(map[string]any)
	if !ok || guest == nil {
		return send(&memqlv1.JoinSpaceAsGuestResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "no guest claims on stream",
		})
	}
	invitationId, _ := guest["invitationId"].(string)
	spaceId, _ := guest["spaceId"].(string)
	plainToken, _ := guest["token"].(string)
	if invitationId == "" || spaceId == "" {
		return send(&memqlv1.JoinSpaceAsGuestResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "guest claims missing invitation/space",
		})
	}

	// Re-fetch the invitation so we have the full payload (for the
	// denormalized fields on the accept row) and re-check expiry --
	// the interceptor already looked it up once, but the invitation
	// may have been revoked between dispatch and handler execution.
	invite, err := lookupInvitationByTokenHash(s.stream.Context(), s.service.engine, hashGuestToken(plainToken))
	if err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "join: lookup", err)
	}
	if invite == nil {
		return send(&memqlv1.JoinSpaceAsGuestResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "invitation no longer resolves",
		})
	}
	if !invite.ExpiresAt.IsZero() && time.Now().UTC().After(invite.ExpiresAt) {
		return send(&memqlv1.JoinSpaceAsGuestResult{
			ErrorCode:    "invitation_expired",
			ErrorMessage: "guest invitation has expired",
		})
	}

	ctx := contextWithSystemActor(s.stream.Context())

	// Mutation 1: stamp the invitation as accepted.
	markArgs := map[string]any{
		"invitationId": invitationId,
		"spaceId":      spaceId,
		"spaceName":    invite.SpaceName,
		"inviterId":    invite.InviterId,
		"inviterName":  invite.InviterName,
		"inviteeEmail": invite.InviteeEmail,
		"inviteeName":  displayName,
		"tokenHash":    invite.TokenHash,
	}
	markJSON, _ := json.Marshal(markArgs)
	markQuery := fmt.Sprintf("mutationMarkGuestInvitationAccepted(%s)", renderQueryArgs(markJSON))
	if _, err := s.service.engine.Execute(ctx, markQuery); err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "join: mark invitation accepted", err)
	}

	// Mutation 2: create the guest's Participant record.
	pArgs := map[string]any{
		"participantId": participantId,
		"spaceId":       spaceId,
		"displayName":   displayName,
	}
	pJSON, _ := json.Marshal(pArgs)
	pQuery := fmt.Sprintf("mutationCreateGuestParticipant(%s)", renderQueryArgs(pJSON))
	if _, err := s.service.engine.Execute(ctx, pQuery); err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "join: create participant", err)
	}

	return send(&memqlv1.JoinSpaceAsGuestResult{
		Success:       true,
		ParticipantId: participantId,
		SpaceId:       spaceId,
	})
}

// lookupInvitationById keys on the invitation's record id. Used by
// the resend + cancel handlers which take an invitationId from the
// authenticated inviter side. Runs the `queryInvitationById` DSL so
// the returned fields come through `invitationFull` (matches the
// other lookups in this file).
func lookupInvitationById(ctx context.Context, engine *memqlengine.MemQLEngine, invitationId string) (*invitationSummary, error) {
	if engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(invitationId) == "" {
		return nil, fmt.Errorf("invitationId required")
	}
	q := fmt.Sprintf(`queryInvitationById({invitationId: %q})`, invitationId)
	result, err := engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("lookup by id: %w", err)
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, nil
	}
	node := result.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return nil, nil
	}
	fields := node.Payload.GetFields()
	if fields == nil {
		return nil, nil
	}
	getStr := func(key string) string {
		v, ok := fields[key]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(v.GetStringValue())
	}
	parseTime := func(key string) time.Time {
		s := getStr(key)
		if s == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	id := getStr("id")
	if id == "" {
		id = node.Id
	}
	return &invitationSummary{
		ID:           id,
		Kind:         getStr("kind"),
		Status:       getStr("status"),
		SpaceId:      getStr("spaceId"),
		SpaceName:    getStr("spaceName"),
		InviterId:    getStr("inviterId"),
		InviterName:  getStr("inviterName"),
		InviteeEmail: getStr("inviteeEmail"),
		InviteeName:  getStr("inviteeName"),
		Token:        getStr("token"),
		TokenHash:    getStr("tokenHash"),
		ExpiresAt:    parseTime("expiresAt"),
		RespondedAt:  parseTime("respondedAt"),
		CreatedAt:    parseTime("createdAt"),
	}, nil
}

// handleCancelGuestInvite revokes a pending guest invitation by id.
// Authenticated space-owner op; the handler looks up the invitation,
// verifies it's a guest-kind row, and stamps status=kicked while
// preserving all guest fields.
func (s *streamSession) handleCancelGuestInvite(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.CancelGuestInviteMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(r *memqlv1.CancelGuestInviteResult) error {
		r.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_CancelGuestInviteResult{
				CancelGuestInviteResult: r,
			},
		})
	}

	invitationId := strings.TrimSpace(msg.GetInvitationId())
	if invitationId == "" {
		return send(&memqlv1.CancelGuestInviteResult{
			ErrorCode:    "invalid_arguments",
			ErrorMessage: "invitation_id is required",
		})
	}
	if s.service.engine == nil {
		return send(&memqlv1.CancelGuestInviteResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	ctx := contextWithSystemActor(s.stream.Context())
	inv, err := lookupInvitationById(ctx, s.service.engine, invitationId)
	if err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "cancel: lookup", err)
	}
	if inv == nil {
		return send(&memqlv1.CancelGuestInviteResult{
			InvitationId: invitationId,
			ErrorCode:    "not_found",
			ErrorMessage: "invitation not found",
		})
	}
	if strings.ToLower(inv.Kind) != "guest" {
		return send(&memqlv1.CancelGuestInviteResult{
			InvitationId: invitationId,
			ErrorCode:    "not_guest",
			ErrorMessage: "invitation is not a guest invite",
		})
	}
	if err := kickGuestInvitation(ctx, s.service.engine, inv); err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "cancel: mark kicked", err)
	}
	return send(&memqlv1.CancelGuestInviteResult{
		Success:      true,
		InvitationId: invitationId,
	})
}

// handleResendGuestInviteEmail re-sends the invitation email for an
// existing pending guest invitation. Server-side lookup of the stored
// plain token means the /join/<token> URL stays the same across
// resends -- the email is rebuilt and re-delivered, no new record is
// created.
func (s *streamSession) handleResendGuestInviteEmail(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ResendGuestInviteEmailMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(r *memqlv1.ResendGuestInviteEmailResult) error {
		r.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ResendGuestInviteEmailResult{
				ResendGuestInviteEmailResult: r,
			},
		})
	}

	invitationId := strings.TrimSpace(msg.GetInvitationId())
	joinURLBase := strings.TrimRight(strings.TrimSpace(msg.GetJoinUrlBase()), "/")
	if invitationId == "" || joinURLBase == "" {
		return send(&memqlv1.ResendGuestInviteEmailResult{
			ErrorCode:    "invalid_arguments",
			ErrorMessage: "invitation_id and join_url_base are required",
		})
	}
	if s.service.engine == nil {
		return send(&memqlv1.ResendGuestInviteEmailResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	ctx := contextWithSystemActor(s.stream.Context())
	inv, err := lookupInvitationById(ctx, s.service.engine, invitationId)
	if err != nil {
		return s.sendGuestError(requestId, correlate, codes.Internal, "resend: lookup", err)
	}
	if inv == nil || strings.ToLower(inv.Kind) != "guest" {
		return send(&memqlv1.ResendGuestInviteEmailResult{
			InvitationId: invitationId,
			ErrorCode:    "not_found",
			ErrorMessage: "guest invitation not found",
		})
	}
	if strings.ToLower(inv.Status) != "pending" {
		return send(&memqlv1.ResendGuestInviteEmailResult{
			InvitationId: invitationId,
			ErrorCode:    "not_pending",
			ErrorMessage: "invitation is no longer pending",
		})
	}
	if !inv.ExpiresAt.IsZero() && time.Now().UTC().After(inv.ExpiresAt) {
		return send(&memqlv1.ResendGuestInviteEmailResult{
			InvitationId: invitationId,
			ErrorCode:    "expired",
			ErrorMessage: "invitation has expired; send a new one",
		})
	}
	if strings.TrimSpace(inv.Token) == "" {
		// Pre-token-storage invitations can't be resent; caller
		// should revoke + re-invite instead.
		return send(&memqlv1.ResendGuestInviteEmailResult{
			InvitationId: invitationId,
			ErrorCode:    "not_found",
			ErrorMessage: "invitation has no resendable token; revoke and re-invite",
		})
	}

	sender := s.resolveEmailSender()
	joinURL := joinURLBase + "/join/" + inv.Token
	if err := email.SendGuestInvite(ctx, sender, email.GuestInviteParams{
		To:          inv.InviteeEmail,
		GuestName:   inv.InviteeName,
		InviterName: inv.InviterName,
		SpaceName:   inv.SpaceName,
		JoinURL:     joinURL,
		ExpiresAt:   inv.ExpiresAt,
	}); err != nil {
		if s.logger != nil {
			s.logger.Error("guest resend: email delivery failed", "error", err, "invitation_id", invitationId)
		}
		return send(&memqlv1.ResendGuestInviteEmailResult{
			InvitationId: invitationId,
			ErrorCode:    "email_failed",
			ErrorMessage: err.Error(),
		})
	}
	return send(&memqlv1.ResendGuestInviteEmailResult{
		Success:      true,
		InvitationId: invitationId,
	})
}

// sendGuestError sends a QueryErrorMsg for guest-invite failures with
// an error ID stamped into metadata (matching the AI handler pattern).
func (s *streamSession) sendGuestError(requestId, correlate string, code codes.Code, message string, err error) error {
	eid := generateErrorId()
	if s.logger != nil {
		s.logger.Error(message, "error", err, "errorId", eid, "requestId", requestId)
	}
	return s.sendQueryErrorWithMetadata(requestId, correlate, code, message, map[string]string{"errorId": eid})
}

// renderQueryArgs turns a JSON object into MemQL's dsl-object literal
// ({key: "val", ...}). It unmarshals to a map and re-emits with bare
// identifier keys; values stay JSON-escaped strings, numbers, bools.
// Only supports flat objects (sufficient for the guest-invite args).
func renderQueryArgs(argsJSON []byte) string {
	var m map[string]any
	if err := json.Unmarshal(argsJSON, &m); err != nil {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for k, v := range m {
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(": ")
		vb, _ := json.Marshal(v)
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}
