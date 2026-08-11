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
	"github.com/znasllc-io/memql/component/secret"
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

// minOperatorKeyLen is the shortest value this interceptor will accept as a
// configured operator credential. It is a floor on operator error, not a
// cryptographic parameter: `MEMQL_OPERATOR_KEY=test` in a hurry would
// otherwise be a cluster-owner credential on a production ingress. 32 chars is
// half of the 64-hex shape the docs prescribe, so it admits every honest key
// and refuses the ones nobody meant as a secret. A short value is treated
// exactly like an unset one -- refused, and logged with the reason.
const minOperatorKeyLen = 32

// NewOperatorAwareStreamInterceptor wraps `base` and admits streams that
// present `Authorization: Operator <key>` matching secret.EnvOperatorKey
// (`MEMQL_OPERATOR_KEY`) exactly. On match the stream is admitted as a
// synthetic cluster-owner identity so out-of-band tooling (`make
// secrets-seed`, `scripts/secrets health`, `scripts/cluster/rolling-drain`)
// can talk to the cluster before any user has been provisioned.
//
// THIS IS A SEPARATE SECRET FROM THE MASTER KEY (memql#3519). It used to read
// MEMQL_MASTER_KEY, on the argument that the master key was already the
// operator credential because anyone with host filesystem access could
// produce it. Two things made that premise stop describing reality: the
// installer wrote the master key into ~/.bashrc at the file's existing
// (typically world-readable) mode, and ESO delivers it to staging and
// production pods -- so the value in a dotfile was a cluster-owner bearer
// token against production over the network. The split costs one more secret
// to rotate and buys back the property the old comment assumed it had: the
// thing that DECRYPTS is not the thing that AUTHENTICATES.
//
// There is deliberately NO fallback to MEMQL_MASTER_KEY. A fallback would
// keep the master key working as an auth credential, which is the entire
// defect. Deployments must seed MEMQL_OPERATOR_KEY before operator tooling
// works again; until they do, the interceptor refuses -- see the rotation
// runbook in docs/public/operate/auth/operator-credential.md.
//
// Bearer / non-Operator traffic falls through to `base`
// unchanged. If MEMQL_OPERATOR_KEY is not set on the node, the
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

		expected := strings.TrimSpace(os.Getenv(secret.EnvOperatorKey))
		if expected == "" {
			if logger != nil {
				logger.Warn("operator auth: rejected -- " + secret.EnvOperatorKey + " not configured on this node")
			}
			return status.Error(codes.Unauthenticated, "operator auth: not configured")
		}
		if len(expected) < minOperatorKeyLen {
			// Refused rather than honoured: a value this short is a
			// placeholder somebody meant to replace, and admitting it would
			// make it a cluster-owner credential.
			if logger != nil {
				logger.Warn("operator auth: rejected -- configured "+secret.EnvOperatorKey+" is too short to be a credential",
					"configuredLen", len(expected),
					"minimum", minOperatorKeyLen,
				)
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

		// Owner role + recognizable subject so auth.FallbackFromClaims
		// stamps an AccessContext with Role=Owner; per-row authz checks
		// then hit the IsClusterOwner() bypass. The OperatorAuthClaimKey
		// marker lets audit / handler code branch when needed (right
		// now nothing branches on it; the tag is there for log clarity
		// + future use).
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
