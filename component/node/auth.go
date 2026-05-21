package node

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity/verifier"
)

// nodeBindingKey is the context key carrying the verified
// (nodeId, nodeType) binding extracted from the class="node" JWT.
// The Stream() handler reads this and cross-checks NodeHello.
type nodeBindingKey struct{}

type nodeBinding struct {
	NodeId   string
	NodeType string
}

func withNodeBinding(ctx context.Context, nodeId, nodeType string) context.Context {
	return context.WithValue(ctx, nodeBindingKey{}, nodeBinding{NodeId: nodeId, NodeType: nodeType})
}

// NodeBindingFromContext returns the (nodeId, nodeType) the
// verifier extracted from the class="node" JWT for this stream. Used
// by the Stream() handler to cross-check NodeHello.NodeId /
// NodeType against the token's claims so a node-class token minted
// for nodeId=A cannot drive a stream that announces nodeId=B.
//
// Returns ("", "", false) when the binding isn't on the context
// (single-node dev / no auth interceptor wired). Callers should
// skip the cross-check in that case.
func NodeBindingFromContext(ctx context.Context) (nodeId, nodeType string, ok bool) {
	if ctx == nil {
		return "", "", false
	}
	b, ok := ctx.Value(nodeBindingKey{}).(nodeBinding)
	if !ok {
		return "", "", false
	}
	return b.NodeId, b.NodeType, true
}

// NodeClassStreamInterceptor returns a gRPC stream interceptor that
// pins NodeService.Stream to class="node" JWTs minted by the identity
// service (#105). Every other JWT class (user / pat / worker via the
// PAT bearer prefix) is rejected with codes.Unauthenticated.
//
// `v` is the per-node verifier (the same one bff/voice/cognition/etc.
// use for the user-facing gRPC surface). When nil the returned
// interceptor is a no-op -- callers wire it only when
// MEMQL_NODE_REQUIRE_AUTH=1 (or equivalent operator config) so
// single-node dev runs unaffected.
//
// The interceptor stamps the (nodeId, nodeType) binding onto the
// stream's context via withNodeBinding so the Stream() handler can
// cross-check NodeHello.NodeId / NodeType against them. A token
// minted for nodeId=A cannot drive a stream that announces nodeId=B.
//
// PATs (verifier.SourcePAT) are rejected -- PATs are issued to
// human users for CLI use, not to cluster nodes.
func NodeClassStreamInterceptor(v *verifier.Verifier, logger *slog.Logger) grpc.StreamServerInterceptor {
	if v == nil {
		// No-op pass-through. Callers haven't wired the verifier
		// (e.g. single-node dev) -- preserve the legacy "any peer
		// can NodeHello" behavior.
		return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, ss)
		}
	}
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		tok, err := tokenFromIncomingNodeContext(ctx)
		if err != nil {
			if logger != nil {
				logger.Warn("node auth: token extraction failed", "error", err, "method", info.FullMethod)
			}
			return status.Error(codes.Unauthenticated, err.Error())
		}
		vc, err := v.VerifyBearer(ctx, tok)
		if err != nil {
			if logger != nil {
				logger.Warn("node auth: token verification failed", "error", err, "method", info.FullMethod)
			}
			return status.Error(codes.Unauthenticated, "invalid or expired token")
		}
		// Surface pin: only class="node" JWTs may speak this wire.
		// PATs (SourcePAT) and user-class JWTs (SourceJWT with Class
		// != ClassNode) are both rejected here.
		if vc.Source != verifier.SourceJWT || vc.Class != verifier.ClassNode {
			if logger != nil {
				logger.Warn("node auth: non-node-class token rejected",
					"source", string(vc.Source),
					"class", vc.Class,
					"subject", vc.UserId,
					"method", info.FullMethod)
			}
			return status.Error(codes.PermissionDenied, "node service requires a node-class token")
		}
		if vc.NodeId == "" || vc.NodeType == "" {
			return status.Error(codes.PermissionDenied, "node-class token missing node_id / node_type binding")
		}
		if logger != nil {
			logger.Debug("node auth: success",
				"node_id", vc.NodeId,
				"node_type", vc.NodeType,
				"method", info.FullMethod)
		}
		ctx = withNodeBinding(ctx, vc.NodeId, vc.NodeType)
		return handler(srv, &nodeAuthStream{ServerStream: ss, ctx: ctx})
	}
}

// nodeAuthStream wraps a ServerStream so the handler sees the
// binding-enriched context.
type nodeAuthStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *nodeAuthStream) Context() context.Context {
	if s == nil || s.ctx == nil {
		if s != nil && s.ServerStream != nil {
			return s.ServerStream.Context()
		}
		return context.Background()
	}
	return s.ctx
}

// tokenFromIncomingNodeContext extracts the Bearer token from the
// gRPC metadata on the inbound stream. Duplicates the equivalent
// helper in the verifier package -- promoting that one to public
// would expose an HTTP-shaped helper from the verifier surface
// without a need.
func tokenFromIncomingNodeContext(ctx context.Context) (string, error) {
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
