package memql

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// GuestAuthClaimKey is the claims-map key the guest-auth path injects
// to mark a stream as guest-authenticated. Handlers + middleware read
// this to branch guest behavior. The key lives under "identity.*"
// because the guest-auth primitive is owned by the identity layer;
// the specific claim payload (partitionId, etc.) is product-flavored and
// gets populated by whoever created the v1:identity:invitation record.
const GuestAuthClaimKey = "identity.guest"

// GuestSubjectPrefix is the shape a guest stream's subject takes. Named
// because the mesh forwarded-auth contract classifies a principal by it: the
// guest interceptor sets no `class` claim, so without recognising this prefix a
// guest would be asserted over the mesh as an ordinary user (memql#3205).
const GuestSubjectPrefix = "guest:"

// NewGuestAwareStreamInterceptor wraps `base` and adds support for
// `Authorization: Guest <token>`. Guest streams are validated at
// connect time against the invitation registry (via engine); valid
// guests get a claims map with subject "guest:<invitationId>", the
// resolved space ID, and the token's SHA-256 hash (for handlers
// that need to re-lookup the invitation row, e.g. mutationJoinSpaceAsGuest).
// The plain token is intentionally NOT stowed in claims -- claims maps
// are surfaced in audit logs and debug dumps, and a plain bearer in
// that path is a log-leak surface.
//
// Bearer / non-Guest headers fall through to `base` unchanged.
//
// Missing-header / empty-header streams also fall through -- the
// downstream interceptor decides whether to reject (verifier) or
// inject a dev identity (NoAuth).
func NewGuestAwareStreamInterceptor(base grpc.StreamServerInterceptor, engine *memqlengine.MemQLEngine, logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		scheme, token := schemeAndTokenFromMetadata(ss.Context())
		if !strings.EqualFold(scheme, "Guest") {
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}
		return validateGuestAndDispatch(srv, ss, info, handler, token, engine, logger)
	}
}

func schemeAndTokenFromMetadata(ctx context.Context) (scheme, token string) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ""
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		values = md.Get("Authorization")
	}
	if len(values) == 0 {
		return "", ""
	}
	parts := strings.Fields(strings.TrimSpace(values[0]))
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func validateGuestAndDispatch(
	srv interface{},
	ss grpc.ServerStream,
	_ *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
	plainToken string,
	engine *memqlengine.MemQLEngine,
	logger *slog.Logger,
) error {
	if engine == nil {
		return status.Error(codes.Unavailable, "guest auth: engine not configured")
	}
	if strings.TrimSpace(plainToken) == "" {
		return status.Error(codes.Unauthenticated, "guest auth: token required")
	}
	summary, err := lookupInvitationByTokenHash(ss.Context(), engine, hashGuestToken(plainToken))
	if err != nil {
		if logger != nil {
			logger.Warn("guest auth: lookup failed", "error", err)
		}
		return status.Error(codes.Unauthenticated, "guest auth: invalid token")
	}
	if summary == nil {
		return status.Error(codes.Unauthenticated, "guest auth: no invitation for token")
	}
	switch strings.ToLower(summary.Status) {
	case "pending", "accepted":
		// Allowed.
	default:
		return status.Error(codes.Unauthenticated, "guest invite is no longer active: "+summary.Status)
	}
	if !summary.ExpiresAt.IsZero() && time.Now().UTC().After(summary.ExpiresAt) {
		return status.Error(codes.Unauthenticated, "guest invite expired")
	}

	// Build claims. The guest's subject is derived from the invitation
	// (not the participantId, which doesn't exist pre-join). Space-
	// scoped partition ACL is computed by downstream middleware using
	// the partitionId we stuff into claims.
	guestClaims := map[string]any{
		"sub":   GuestSubjectPrefix + summary.ID,
		"email": summary.InviteeEmail,
		// The cluster-wide role was chosen as a placeholder ("meaningless;
		// partition grant drives access"). Since memql#3179 it is NOT
		// meaningless: the coarse data-plane gate in
		// data_capability_gate.go refuses `reader` any mutation sent over
		// ExecuteQueryMsg, so a guest stream is read-only on that surface.
		// The dedicated guest handlers (JoinSpaceAsGuestMsg,
		// ResolveGuestInviteMsg, ...) are unaffected -- they are their own
		// payload types and do not go through ExecuteQuery. If guests must
		// write through the generic DSL surface, this claim is what has to
		// change, deliberately.
		"role": "reader",
		"name": summary.InviteeName,
		GuestAuthClaimKey: map[string]any{
			"invitationId": summary.ID,
			"partitionId":      summary.PartitionId,
			"tokenHash":    hashGuestToken(plainToken), // hash only -- plain token must not enter the claims map (log-leak surface)
			"status":       strings.ToLower(summary.Status),
		},
	}
	tokenInfo := auth.BuildTokenInfo(guestClaims)
	ctx := ss.Context()
	ctx = auth.ContextWithClaims(ctx, guestClaims)
	ctx = auth.ContextWithToken(ctx, tokenInfo)

	if logger != nil {
		logger.Debug("guest auth success",
			"invitationId", summary.ID,
			"partitionId", summary.PartitionId,
			"status", summary.Status,
		)
	}

	wrapped := &guestAuthenticatedStream{ServerStream: ss, ctx: ctx}
	return handler(srv, wrapped)
}

type guestAuthenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *guestAuthenticatedStream) Context() context.Context {
	if s == nil || s.ctx == nil {
		if s != nil && s.ServerStream != nil {
			return s.ServerStream.Context()
		}
		return context.Background()
	}
	return s.ctx
}
