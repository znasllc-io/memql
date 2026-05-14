package verifier

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/visionarys-io/memql/component/auth"
)

// StreamInterceptor returns a gRPC stream interceptor that verifies
// the bearer token on the incoming Authorization metadata, builds an
// auth.AccessContext-compatible claims map, and stamps it onto the
// stream's context.
//
// Rejects with codes.Unauthenticated on every failure path; on
// success, downstream handlers can reach for auth.ClaimsFromContext
// or auth.UserIdentityFromContext.
//
// An optional auth.DelegationResolver may be provided. When a
// resolved delegation exists for the verified subject, it's attached
// to the context via auth.ContextWithDelegation.
func StreamInterceptor(v *Verifier, logger *slog.Logger, resolver ...auth.DelegationResolver) grpc.StreamServerInterceptor {
	if v == nil {
		return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return status.Error(codes.Internal, "verifier not configured")
		}
	}
	var dr auth.DelegationResolver
	if len(resolver) > 0 {
		dr = resolver[0]
	}
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		token, err := tokenFromGRPCMetadata(ctx)
		if err != nil {
			if logger != nil {
				logger.Warn("grpc auth: token extraction failed", "error", err, "method", info.FullMethod)
			}
			return status.Error(codes.Unauthenticated, err.Error())
		}
		vc, err := v.VerifyBearer(ctx, token)
		if err != nil {
			if logger != nil {
				logger.Warn("grpc auth: token verification failed", "error", err, "method", info.FullMethod)
			}
			return status.Error(codes.Unauthenticated, "invalid or expired token")
		}
		if logger != nil {
			logger.Debug("grpc auth: success",
				"subject", vc.UserId,
				"role", vc.Role,
				"source", string(vc.Source),
				"method", info.FullMethod)
		}
		ctx = AttachToContext(ctx, vc)
		if dr != nil {
			if dc, derr := dr.ResolveActiveDelegation(ctx, vc.UserId); derr == nil && dc != nil {
				ctx = auth.ContextWithDelegation(ctx, dc)
				if logger != nil {
					logger.Debug("grpc auth: delegation attached",
						"subject", vc.UserId,
						"agent_id", dc.AgentId,
						"role_ceiling", dc.RoleCeiling,
						"method", info.FullMethod)
				}
			}
		}
		return handler(srv, &authenticatedStream{ServerStream: ss, ctx: ctx})
	}
}

// authenticatedStream wraps a gRPC stream so handler() sees the
// claim-enriched context.
type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context {
	if s == nil || s.ctx == nil {
		if s != nil && s.ServerStream != nil {
			return s.ServerStream.Context()
		}
		return context.Background()
	}
	return s.ctx
}

// tokenFromGRPCMetadata extracts the bearer token from the incoming
// gRPC metadata.
func tokenFromGRPCMetadata(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", errors.New("metadata missing")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		values = md.Get("Authorization")
	}
	if len(values) == 0 {
		return "", errors.New("authorization header missing")
	}
	header := strings.TrimSpace(values[0])
	if header == "" {
		return "", errors.New("authorization header empty")
	}
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("invalid authorization header format")
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return "", errors.New("bearer token empty")
	}
	return tok, nil
}

