package memql

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// NewServiceAccountStreamInterceptor wraps `base` and admits a machine
// principal via a class="service_account" identity-issued JWT (#691,
// deployment-v2 Phase 3). The JWT is verified through the SAME per-node JWKS
// verifier every authenticated surface uses -- crucially WITHOUT a DB lookup,
// which is why this works on the BFF/mesh where a PAT does not (the per-node
// verifier is constructed with a nil PATVerifier; PATs only verify on the
// identity node). On success the call is pinned to a read/query surface
// (ExecuteQuery / AgentGenerateTurn / Subscribe + control frames); every
// credential/identity/delegation/worker-token/invite mutation is rejected with
// PermissionDenied, so a leaked synthetic credential can't escalate.
//
// Auth shape: `Authorization: Bearer <eyJ...>` with `class="service_account"`
// and a `label` (stamped into VerifiedClaims.NodeId for audit attribution).
//
// Non-service-account bearers fall through to `base`. A nil `v` disables the
// admit path entirely (tests / binaries that don't run this surface).
func NewServiceAccountStreamInterceptor(
	base grpc.StreamServerInterceptor,
	v *verifier.Verifier,
	logger *slog.Logger,
) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		scheme, token := schemeAndTokenFromMetadata(ctx)

		// Only JWT-shaped bearers are candidates. PAT (mql_pat_) / worker-token
		// (mql_wkr_) bearers are reserved for other surfaces; short-circuit so
		// we don't waste a verifier round-trip on them.
		if v == nil || !strings.EqualFold(scheme, "Bearer") || token == "" || hasReservedTokenPrefix(token) {
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}

		vc, err := v.VerifyBearer(ctx, token)
		if err != nil || vc == nil || vc.Source != verifier.SourceJWT || vc.Class != verifier.ClassServiceAccount {
			// Not a service-account JWT (could be a user/voice-agent JWT for a
			// different surface). Hand off to the base chain, which re-runs
			// VerifyBearer (cheap; JWKS was cached on the first call).
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}

		if logger != nil {
			logger.Debug("service-account admitted",
				"label", vc.NodeId, "sub", vc.UserId, "method", info.FullMethod)
		}
		ctx = withServiceAccountClaims(ctx, vc)
		return handler(srv, &serviceAccountStream{ServerStream: ss, ctx: ctx, logger: logger})
	}
}

// serviceAccountStream pins the surface: it injects the service-account system
// actor on every read context and rejects any non-allowlisted payload BEFORE
// it reaches handleMessage.
type serviceAccountStream struct {
	grpc.ServerStream
	ctx    context.Context
	logger *slog.Logger
}

func (s *serviceAccountStream) Context() context.Context {
	if s == nil || s.ctx == nil {
		return s.ServerStream.Context()
	}
	return s.ctx
}

func (s *serviceAccountStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	envelope, ok := m.(*memqlv1.MemqlClientMessage)
	if !ok {
		// Other RPCs aren't on this surface; pass through unchanged.
		return nil
	}
	if !isServiceAccountPayload(envelope.GetPayload()) {
		if s.logger != nil {
			s.logger.Warn("service-account stream received off-surface payload",
				"payload_type", payloadTypeName(envelope.GetPayload()))
		}
		return status.Error(codes.PermissionDenied,
			"service-account tokens may only run read/query message types")
	}
	return nil
}

// isServiceAccountPayload returns true for the read/query + control payloads a
// machine principal (deploy gate / automation) is allowed to send. This is the
// surface-pinning allowlist: it deliberately EXCLUDES every credential and
// admin mutation (IdentityCreate/Update, CreateWorkerToken, DelegationCreate,
// SendGuestInvite, RotateAuth, Revoke*, etc.) so a leaked synthetic credential
// cannot escalate.
func isServiceAccountPayload(payload any) bool {
	switch payload.(type) {
	// Stream-level control frames every caller needs.
	case *memqlv1.MemqlClientMessage_ClientHello,
		*memqlv1.MemqlClientMessage_Ack,
		*memqlv1.MemqlClientMessage_Unsubscribe,
		*memqlv1.MemqlClientMessage_CancelRequest:
		return true
	// Read / query surface -- the synthetic health check's actual work.
	case *memqlv1.MemqlClientMessage_ExecuteQuery,
		*memqlv1.MemqlClientMessage_Subscribe,
		*memqlv1.MemqlClientMessage_ConceptsList,
		*memqlv1.MemqlClientMessage_ConceptsSubscribe,
		*memqlv1.MemqlClientMessage_MyAccess,
		*memqlv1.MemqlClientMessage_EvaluatePolicy:
		return true
	// The deep gate fans BFF -> cognition/agent: a single synthetic agent turn
	// proves the authenticated app path end to end.
	case *memqlv1.MemqlClientMessage_AgentGenerateTurn:
		return true
	}
	return false
}

// withServiceAccountClaims stamps a system-actor identity onto the context,
// carrying the verified token's subject + label for audit attribution. The
// surface allowlist (not the role) is what contains the credential.
func withServiceAccountClaims(ctx context.Context, vc *verifier.VerifiedClaims) context.Context {
	label, sub := "", ""
	if vc != nil {
		label = vc.NodeId
		sub = vc.UserId // JWT `sub` claim
	}
	claims := map[string]any{
		"sub":   sub,
		"email": "service-account@memql.internal",
		"role":  "system",
		"class": verifier.ClassServiceAccount,
		"label": label,
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
