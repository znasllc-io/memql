package memql

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"

	"github.com/znasllc-io/memql/component/auth"
)

// LocalDevSubject is the synthetic subject stamped on every stream when
// authentication is DISABLED for troubleshooting
// (MEMQL_IDENTITY_ENABLED=false). ai_forward.go keys the worker
// "local-dev" propagation marker off this exact value, so keep them in
// sync.
const LocalDevSubject = "local-dev"

// LocalDevEmail is the synthetic principal email in no-auth dev mode.
const LocalDevEmail = "local-dev@memql.local"

// NewLocalDevStreamInterceptor is the terminal admit path installed ONLY
// when authentication is disabled on a verifier-consuming node
// (MEMQL_IDENTITY_ENABLED=false -- troubleshooting only, NEVER
// staging/production). It stamps a synthetic cluster-owner identity
// (subject "local-dev", role "owner") onto every stream so downstream
// per-row authorization resolves via auth.FallbackFromClaims to an owner
// AccessContext -- i.e. the whole gRPC surface behaves as one local owner
// with no credential presented.
//
// It NEVER rejects a stream; the loud, one-time SECURITY warning is
// emitted at boot (see app/configAndAuth) and the memql_auth_enabled
// gauge is pinned to 0. The operator-credential path is layered on top of
// this in transportBase, so operator tooling still works in no-auth mode.
//
// Mirrors NewOperatorAwareStreamInterceptor's synthetic-owner stamping
// (operator_stream_interceptor.go) -- the only difference is that this
// admits UNCONDITIONALLY rather than on a presented credential.
func NewLocalDevStreamInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		claims := map[string]any{
			"sub":   LocalDevSubject,
			"email": LocalDevEmail,
			"role":  "owner",
			"roles": []string{"owner"},
			"name":  "Local Dev (no-auth)",
		}
		ctx := ss.Context()
		ctx = auth.ContextWithClaims(ctx, claims)
		ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))

		if logger != nil {
			logger.Debug("local-dev no-auth stream admitted (auth disabled)",
				"subject", LocalDevSubject,
				"method", info.FullMethod,
			)
		}

		return handler(srv, &localDevAuthenticatedStream{ServerStream: ss, ctx: ctx})
	}
}

// localDevAuthenticatedStream overrides the stream context so the
// synthetic local-dev identity flows to every downstream handler.
type localDevAuthenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *localDevAuthenticatedStream) Context() context.Context {
	if s == nil || s.ctx == nil {
		if s != nil && s.ServerStream != nil {
			return s.ServerStream.Context()
		}
		return context.Background()
	}
	return s.ctx
}
