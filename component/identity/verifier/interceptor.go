package verifier

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
)

// EpochResolver looks up the v1:identity:user.revocationEpoch for the
// given user. Implementations should be fast + cache-friendly --
// CurrentEpoch is called on every authenticated stream open AND on
// every periodic re-check tick for long-lived streams (see memql#106).
//
// Returning an error short-circuits the epoch check; the stream is
// rejected with codes.Unauthenticated. Returning (0, nil) is the
// expected steady state for a user whose row has never been bumped --
// stamped tokens carry 0 too, so they match.
type EpochResolver interface {
	CurrentEpoch(ctx context.Context, userId string) (int64, error)
}

// EpochCheck wires the periodic-recheck loop. When non-nil and the
// verified token's Source == SourceJWT, the interceptor:
//   - rejects at stream-open if the token's revocation_epoch claim is
//     below Resolver.CurrentEpoch(userId);
//   - starts a per-stream ticker (Interval, default 5m) that re-checks
//     the epoch and cancels the stream's derived context when the
//     epoch has advanced past the token claim. The cancellation
//     propagates to the handler via ctx.Done(); long-lived streaming
//     RPCs observe and shut down.
//
// PAT tokens (SourcePAT) bypass the check entirely -- PAT revocation
// is row-state on v1:identity:identity, not claim-state.
//
// Resolver == nil disables the whole feature.
type EpochCheck struct {
	Resolver EpochResolver
	// Interval between epoch re-checks for a single open stream.
	// Defaults to 5 minutes. Tests may pass a shorter value.
	Interval time.Duration
}

// DefaultEpochCheckInterval is the period the per-stream re-check
// loop runs at when EpochCheck.Interval is zero. Matches the value in
// memql#106's acceptance criteria.
const DefaultEpochCheckInterval = 5 * time.Minute

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
	return StreamInterceptorWithEpoch(v, logger, nil, resolver...)
}

// StreamInterceptorWithEpoch is StreamInterceptor plus an opt-in
// revocation-epoch check (memql#106). When epoch == nil it behaves
// identically to StreamInterceptor. When epoch != nil and the
// verified token is a JWT, the interceptor (a) rejects if the
// token's revocation_epoch claim is below the user's current row
// value and (b) starts a per-stream goroutine that re-checks every
// epoch.Interval (default 5m) and cancels the stream's derived
// context when the epoch advances. PAT tokens skip both checks --
// PAT revocation is row-state on v1:identity:identity, not claim-
// state.
func StreamInterceptorWithEpoch(v *Verifier, logger *slog.Logger, epoch *EpochCheck, resolver ...auth.DelegationResolver) grpc.StreamServerInterceptor {
	if v == nil {
		return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return status.Error(codes.Internal, "verifier not configured")
		}
	}
	var dr auth.DelegationResolver
	if len(resolver) > 0 {
		dr = resolver[0]
	}
	interval := DefaultEpochCheckInterval
	if epoch != nil && epoch.Interval > 0 {
		interval = epoch.Interval
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
		if epoch != nil && epoch.Resolver != nil && vc.Source == SourceJWT {
			cur, eerr := epoch.Resolver.CurrentEpoch(ctx, vc.UserId)
			if eerr != nil {
				if logger != nil {
					logger.Warn("grpc auth: epoch lookup failed (rejecting open)", "subject", vc.UserId, "method", info.FullMethod, "error", eerr)
				}
				return status.Error(codes.Unauthenticated, "revocation check failed")
			}
			if vc.RevocationEpoch < cur {
				if logger != nil {
					logger.Info("grpc auth: token rejected by revocation epoch",
						"subject", vc.UserId,
						"token_epoch", vc.RevocationEpoch,
						"current_epoch", cur,
						"method", info.FullMethod)
				}
				return status.Error(codes.Unauthenticated, "token revoked")
			}
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
		if epoch != nil && epoch.Resolver != nil && vc.Source == SourceJWT {
			derived, cancel := context.WithCancel(ctx)
			defer cancel()
			go runEpochRecheck(derived, logger, info.FullMethod, vc, epoch.Resolver, interval, cancel)
			ctx = derived
		}
		return handler(srv, &authenticatedStream{ServerStream: ss, ctx: ctx})
	}
}

// runEpochRecheck polls the EpochResolver every `interval` and
// cancels the parent context once the resolver reports an epoch
// greater than the token's. Exits when the parent context fires
// (i.e. the stream ends normally) -- the deferred cancel() in the
// caller cleans up either way.
func runEpochRecheck(ctx context.Context, logger *slog.Logger, method string, vc *VerifiedClaims, resolver EpochResolver, interval time.Duration, cancel context.CancelFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur, err := resolver.CurrentEpoch(ctx, vc.UserId)
			if err != nil {
				if logger != nil {
					logger.Warn("grpc auth: periodic epoch lookup failed (closing stream)",
						"subject", vc.UserId,
						"method", method,
						"error", err)
				}
				cancel()
				return
			}
			if vc.RevocationEpoch < cur {
				if logger != nil {
					logger.Info("grpc auth: epoch advanced -- closing long-lived stream",
						"subject", vc.UserId,
						"token_epoch", vc.RevocationEpoch,
						"current_epoch", cur,
						"method", method)
				}
				cancel()
				return
			}
		}
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
