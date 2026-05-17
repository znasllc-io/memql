package memql

import (
	"context"
	"net"
	"strings"

	grpcMetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	nodeMetadata "github.com/znasllc-io/memql/component/metadata"
)

// extractRequestMeta extracts transport-level metadata from a gRPC stream context.
func extractRequestMeta(ctx context.Context) *nodeMetadata.RequestMeta {
	rm := &nodeMetadata.RequestMeta{
		Protocol: "grpc",
	}

	// Extract client IP from gRPC peer.
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr := p.Addr.String()
		if host, _, err := net.SplitHostPort(addr); err == nil {
			rm.ClientIP = host
		} else {
			rm.ClientIP = addr
		}
	}

	// Extract metadata headers from gRPC context.
	if md, ok := grpcMetadata.FromIncomingContext(ctx); ok {
		// User-Agent
		if vals := md.Get("user-agent"); len(vals) > 0 {
			rm.UserAgent = vals[0]
		}

		// X-Forwarded-For (if behind a load balancer, use the first IP)
		if vals := md.Get("x-forwarded-for"); len(vals) > 0 {
			forwarded := vals[0]
			if idx := strings.Index(forwarded, ","); idx > 0 {
				forwarded = forwarded[:idx]
			}
			rm.ClientIP = strings.TrimSpace(forwarded)
		}

		// Custom platform header
		if vals := md.Get("x-memql-platform"); len(vals) > 0 {
			rm.Platform = vals[0]
		}

		// Custom app version header
		if vals := md.Get("x-memql-app-version"); len(vals) > 0 {
			rm.AppVersion = vals[0]
		}
	}

	return rm
}

// enrichContextWithMetadata adds the session's request metadata to a context
// so that downstream handlers and the metadata collector can read it.
func (s *streamSession) enrichContextWithMetadata(ctx context.Context, envelope interface{ GetMessageId() string }) context.Context {
	if s.requestMeta != nil {
		rm := *s.requestMeta // copy to avoid mutation
		if envelope != nil {
			rm.RequestId = envelope.GetMessageId()
		}
		ctx = nodeMetadata.ContextWithRequestMeta(ctx, &rm)
	}

	// Inject default source metadata for API-originated requests.
	ctx = nodeMetadata.ContextWithSourceMeta(ctx, &nodeMetadata.SourceMeta{
		System:    "api",
		Component: "grpc",
	})

	return ctx
}
