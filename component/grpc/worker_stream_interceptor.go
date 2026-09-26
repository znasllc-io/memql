package memql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity/workertoken"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/worker"
)

// workerServicePathPrefix is the gRPC method-path prefix exposed by
// WorkerService. The interceptor uses it to gate worker-token usage
// to that single service.
const workerServicePathPrefix = "/znasllc.memql.worker.v1.WorkerService/"

// NewWorkerAwareStreamInterceptor wraps `base` and recognizes worker
// tokens (Authorization: Worker mql_wkr_<token> OR a Bearer header
// whose value starts with mql_wkr_). Worker tokens are admitted ONLY
// on WorkerService paths; presenting one on any other RPC fails with
// PermissionDenied. Tokens are looked up by SHA-256 hash via the
// supplied resolver -- typically a thin shim around the identity
// store's lookup-by-keyHash. Returns the resolved WorkerIdentity to
// the downstream handler via worker.ContextWithWorkerIdentity.
//
// Bearer / non-Worker traffic falls through to `base` unchanged.
func NewWorkerAwareStreamInterceptor(
	base grpc.StreamServerInterceptor,
	resolver WorkerTokenResolver,
	logger *slog.Logger,
) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		scheme, token := schemeAndTokenFromMetadata(ss.Context())
		isWorkerScheme := strings.EqualFold(scheme, "Worker")
		isBearerWorker := strings.EqualFold(scheme, "Bearer") && worker.HasTokenPrefix(token)

		if !isWorkerScheme && !isBearerWorker {
			// Not a worker token. If the path nonetheless targets
			// WorkerService, reject -- only worker tokens may speak
			// that surface. Otherwise fall through.
			if isWorkerServicePath(info) {
				return status.Error(codes.Unauthenticated, "WorkerService requires a worker token")
			}
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}

		// Worker tokens are only allowed on WorkerService paths.
		if !isWorkerServicePath(info) {
			return status.Error(codes.PermissionDenied, "worker tokens may only call WorkerService")
		}

		ident, err := resolveWorkerToken(ss.Context(), resolver, token)
		if err != nil {
			if logger != nil {
				logger.Warn("worker authentication failed",
					"method", info.FullMethod,
					"error", err,
				)
			}
			if errors.Is(err, ErrWorkerTokenNotFound) {
				return status.Error(codes.Unauthenticated, "invalid worker token")
			}
			// A failed database lookup says nothing about the credential. Keep
			// transient infrastructure failures retryable and do not expose DB
			// details (or ask the user to rotate a perfectly valid token).
			return status.Error(codes.Unavailable, "worker authentication temporarily unavailable")
		}

		ctx := worker.ContextWithWorkerIdentity(ss.Context(), ident)
		wrapped := &workerAuthenticatedStream{ServerStream: ss, ctx: ctx}
		return handler(srv, wrapped)
	}
}

// WorkerTokenResolver looks up a worker identity from a presented
// plain token. Hash + lookup are implementation details (see
// WorkerTokenResolverFunc and EngineWorkerTokenResolver below).
type WorkerTokenResolver interface {
	ResolveWorkerToken(ctx context.Context, plainToken string) (*worker.WorkerIdentity, error)
}

// WorkerTokenResolverFunc adapts an ordinary function into the
// resolver interface.
type WorkerTokenResolverFunc func(ctx context.Context, plainToken string) (*worker.WorkerIdentity, error)

// ResolveWorkerToken implements WorkerTokenResolver.
func (f WorkerTokenResolverFunc) ResolveWorkerToken(ctx context.Context, plainToken string) (*worker.WorkerIdentity, error) {
	return f(ctx, plainToken)
}

// EngineWorkerTokenResolver looks up worker tokens by hashing the
// presented plain token and consulting workerTokenByKeyHash
// via the workertoken.Store.
type EngineWorkerTokenResolver struct {
	Engine *memqlengine.MemQLEngine
	Logger *slog.Logger
}

// ErrWorkerTokenNotFound is returned when the inbound token does
// not match any persisted worker_token identity row.
var ErrWorkerTokenNotFound = errors.New("worker token not found")

// ResolveWorkerToken is the production lookup path. Hashes the
// inbound plain token, looks up the matching v1:identity:identity
// row of identityType="worker_token", and returns the
// auth-relevant subset.
func (r *EngineWorkerTokenResolver) ResolveWorkerToken(ctx context.Context, plainToken string) (*worker.WorkerIdentity, error) {
	if r == nil || r.Engine == nil {
		return nil, fmt.Errorf("worker: resolver engine not configured")
	}
	if strings.TrimSpace(plainToken) == "" {
		return nil, fmt.Errorf("%w: empty token", ErrWorkerTokenNotFound)
	}
	hash := workertoken.Hash(plainToken)
	if hash == "" {
		return nil, fmt.Errorf("%w: hash failed", ErrWorkerTokenNotFound)
	}
	store := &workertoken.Store{Engine: r.Engine, Logger: r.Logger}
	row, err := store.LookupByKeyHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrWorkerTokenNotFound
	}
	// THE ROTATION GRACE IS DECIDED HERE, not in the query (epic memql#5327,
	// design D5). workerTokenByKeyHash matches the current hash OR the
	// displaced one, because a window is a comparison against now and a
	// filter that made one would be comparing against a moment nobody chose.
	// So a hit on the previous hash is admitted only while its own window is
	// still open.
	//
	// Branch order matters: a row whose two hashes are equal -- which
	// re-rotating to the same material would produce -- takes the current arm
	// and needs no window at all.
	if row.KeyHash != hash {
		switch {
		case row.PreviousKeyHash != hash:
			// The lookup matched on something this resolver does not accept.
			// Nothing should reach here; reading it as not-found rather than
			// as an admission is the only safe treatment of a query that
			// answered a question it was not asked.
			return nil, ErrWorkerTokenNotFound
		case row.PreviousKeyExpiresAt.IsZero(), !time.Now().Before(row.PreviousKeyExpiresAt):
			return nil, fmt.Errorf("%w: the rotated token's grace window has closed", ErrWorkerTokenNotFound)
		default:
			if r.Logger != nil {
				r.Logger.Info("worker token admitted on the rotation grace hash",
					"identity_id", row.ID,
					"grace_until", row.PreviousKeyExpiresAt.UTC().Format(time.RFC3339),
				)
			}
		}
	}
	return &worker.WorkerIdentity{
		IdentityId:  row.ID,
		OwnerUserId: row.UserId,
		Active:      row.Active,
		ExpiresAt:   row.ExpiresAt,
	}, nil
}

func resolveWorkerToken(ctx context.Context, resolver WorkerTokenResolver, plainToken string) (*worker.WorkerIdentity, error) {
	if resolver == nil {
		return nil, errors.New("worker: resolver not configured")
	}
	if strings.TrimSpace(plainToken) == "" {
		return nil, fmt.Errorf("%w: empty token", ErrWorkerTokenNotFound)
	}
	if !worker.HasTokenPrefix(plainToken) {
		return nil, fmt.Errorf("%w: token does not carry mql_wkr_ prefix", ErrWorkerTokenNotFound)
	}
	ident, err := resolver.ResolveWorkerToken(ctx, plainToken)
	if err != nil {
		return nil, err
	}
	if ident == nil || ident.IdentityId == "" || ident.OwnerUserId == "" {
		return nil, fmt.Errorf("%w: identity not found for token", ErrWorkerTokenNotFound)
	}
	// Active==false is how worker tokens are revoked (see
	// workertoken.Store.Revoke). Reject revoked tokens.
	if !ident.Active {
		return nil, fmt.Errorf("%w: identity inactive", ErrWorkerTokenNotFound)
	}
	// Expiry is optional (ExpiresAt zero = non-expiring). When set,
	// the bound is inclusive of `now` for backwards-compatibility
	// with downstream callers that look at expiresAt for display.
	if !ident.ExpiresAt.IsZero() && !time.Now().Before(ident.ExpiresAt) {
		return nil, fmt.Errorf("%w: identity expired", ErrWorkerTokenNotFound)
	}
	return ident, nil
}

func isWorkerServicePath(info *grpc.StreamServerInfo) bool {
	if info == nil {
		return false
	}
	return strings.HasPrefix(info.FullMethod, workerServicePathPrefix)
}

// workerAuthenticatedStream is the ServerStream wrapper that swaps
// in the worker-authenticated context.
type workerAuthenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *workerAuthenticatedStream) Context() context.Context {
	if w == nil || w.ctx == nil {
		return w.ServerStream.Context()
	}
	return w.ctx
}
