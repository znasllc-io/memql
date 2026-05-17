package memql

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
)

// OperatorAuthClaimKey is the claims-map key the operator-aware
// interceptor stows the operator marker under. Handlers / audit
// logging branch on it when present.
const OperatorAuthClaimKey = "identity.operator"

// OperatorSubject is the synthetic identity attached to streams
// admitted via the operator credential. Distinct from any user /
// guest / pair / worker subject so audit logs disambiguate
// cleanly.
const OperatorSubject = "cluster:operator"

// envMasterKey names the env var that carries the cluster master
// key. The operator-aware interceptor admits streams that present
// the same value as `Authorization: Operator <master_key>`. We
// reuse the secrets-encryption master key intentionally: it is
// already the "operator credential" of the cluster (anyone who
// can read /var/lib/memql secrets from the host can produce it),
// so layering authentication on top of it adds no new secret to
// rotate. Production-style deployments that want the operator
// path disabled simply leave MEMQL_MASTER_KEY unset on the
// node -- the interceptor short-circuits to the next layer.
const envMasterKey = "MEMQL_MASTER_KEY"

// NewOperatorAwareStreamInterceptor wraps `base` and admits
// streams that present `Authorization: Operator <master_key>`
// when the supplied key matches `MEMQL_MASTER_KEY` exactly. On
// match the stream is admitted as a synthetic cluster-owner
// identity so out-of-band tooling (`make secrets-seed`,
// `scripts/secrets health`, future operator CLIs) can talk to
// the cluster before any user has been provisioned.
//
// Bearer / non-Operator traffic falls through to `base`
// unchanged. If MEMQL_MASTER_KEY is not set on the node, the
// interceptor still parses the header but always rejects
// Operator-scheme streams with Unauthenticated -- failing closed
// is the right move for a cluster where no operator credential
// is configured.
func NewOperatorAwareStreamInterceptor(
	base grpc.StreamServerInterceptor,
	logger *slog.Logger,
) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		scheme, token := schemeAndTokenFromMetadata(ss.Context())
		if !strings.EqualFold(scheme, "Operator") {
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}

		expected := strings.TrimSpace(os.Getenv(envMasterKey))
		if expected == "" {
			if logger != nil {
				logger.Warn("operator auth: rejected -- MEMQL_MASTER_KEY not configured on this node")
			}
			return status.Error(codes.Unauthenticated, "operator auth: not configured")
		}
		got := strings.TrimSpace(token)
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			if logger != nil {
				logger.Warn("operator auth: rejected -- master-key mismatch")
			}
			return status.Error(codes.Unauthenticated, "operator auth: invalid credential")
		}

		// Owner role + recognizable subject so access.FallbackFromClaims
		// stamps an AccessContext with Role=Owner; that hits the
		// IsClusterOwner() bypass in CheckPartition. The
		// OperatorAuthClaimKey marker lets audit / handler code
		// branch when needed (right now nothing branches on it; the
		// tag is there for log clarity + future use).
		claims := map[string]any{
			"sub":   OperatorSubject,
			"role":  "owner",
			"roles": []string{"owner"},
			"name":  "Cluster Operator",
			OperatorAuthClaimKey: map[string]any{
				"source": "master_key",
			},
		}
		tokenInfo := auth.BuildTokenInfo(claims)
		ctx := ss.Context()
		ctx = auth.ContextWithClaims(ctx, claims)
		ctx = auth.ContextWithToken(ctx, tokenInfo)

		if logger != nil {
			logger.Warn("operator auth admitted",
				"subject", OperatorSubject,
				"method", info.FullMethod,
			)
		}

		return handler(srv, &operatorAuthenticatedStream{ServerStream: ss, ctx: ctx})
	}
}

// NewRejectAllStreamInterceptor is a terminal interceptor that
// rejects every stream with codes.Unauthenticated. Used as the
// inner layer on binaries where no upstream credential surface
// applies (currently: the identity binary, where the only admit
// path is the operator credential one layer up).
func NewRejectAllStreamInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(_ interface{}, _ grpc.ServerStream, info *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
		if logger != nil {
			logger.Warn("grpc auth: rejected -- no admit path on this binary", "method", info.FullMethod)
		}
		return status.Error(codes.Unauthenticated, "this gRPC surface requires the operator credential")
	}
}

// IsOperatorContext reports whether the stream context was admitted
// via the operator credential.
func IsOperatorContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return false
	}
	_, has := claims[OperatorAuthClaimKey]
	return has
}

type operatorAuthenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *operatorAuthenticatedStream) Context() context.Context {
	if s == nil || s.ctx == nil {
		if s != nil && s.ServerStream != nil {
			return s.ServerStream.Context()
		}
		return context.Background()
	}
	return s.ctx
}
