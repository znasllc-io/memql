// Package worker is the SDK's worker subpackage. It wraps the
// transport layer for dialing a memql cluster's WorkerService:
// gRPC connection setup, TLS configuration, auth-token plumbing,
// and the bidi stream opener.
//
// Scope. The SDK owns transport; the protocol (Register /
// RegisterAck / Heartbeat / ToolResult / ToolCall / ...) stays
// exposed via the underlying *memqlv1.WorkerService_StreamClient
// returned by Connection.Stream. Wrapping the full envelope set
// in SDK-owned types is a separate effort -- the goal of this
// package is to stop the cockpit (and any future worker host) from
// re-implementing the dial code per consumer.
//
// Typical use:
//
//	conn, err := worker.Dial(ctx, worker.DialConfig{
//	    Endpoint: "host:443",
//	    UseTLS:   true,
//	    Token:    cfg.Token, // mql_wkr_...
//	    Logger:   logger,
//	})
//	if err != nil { ... }
//	defer conn.Close()
//
//	stream := conn.Stream()
//	// ...regular Send / Recv against the worker protocol...
//
// Surfaced by memql#117 (the issue requesting this SDK module) and
// the SDK-only rule in memql/sdk/go/CLAUDE.md.
package worker

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// DefaultMaxMessageSize is the gRPC inbound/outbound cap the worker
// dial uses when DialConfig.MaxMessageSize is zero. 32 MiB covers
// a 6K-display screenshot tool result with headroom; the default
// gRPC 4 MiB cap caused mid-stream RST_STREAM on every screenshot
// (cockpit side surfaced "worker stream ended" with no actionable
// detail). Bumping at the SDK keeps every consumer aligned without
// each one having to re-discover the failure mode.
const DefaultMaxMessageSize = 32 * 1024 * 1024

// DialConfig describes how to dial a memql cluster's WorkerService.
// Only Endpoint + Token are required; the rest fall back to safe
// defaults.
type DialConfig struct {
	// Endpoint is the gRPC dial address in `host:port` form. Use
	// ParseClusterURL to derive both Endpoint and UseTLS from an
	// operator-facing cluster URL (http:// / https:// / grpc:// /
	// grpcs:// / bare host:port).
	Endpoint string

	// UseTLS enables TLS on the gRPC dial. The TLS configuration is
	// built by BuildTLSConfig (server-name pinned to the endpoint
	// host, MinVersion TLS 1.2, optional env-driven CA / mTLS
	// material). Disable only for local-loopback dev or test
	// clusters.
	UseTLS bool

	// Token is the worker authorization token (mql_wkr_<...>). Sent
	// as `Authorization: Worker <token>` on the outgoing stream
	// metadata. Required.
	Token string

	// Logger receives transport-level INFO ("worker connecting to
	// cluster") and WARN ("InsecureSkipVerify enabled") events. Nil
	// is allowed; the SDK silently skips logging when nil.
	Logger *slog.Logger

	// MaxMessageSize overrides DefaultMaxMessageSize. Zero or
	// negative values fall back to the default.
	MaxMessageSize int
}

// Connection wraps a gRPC client conn + an open WorkerService bidi
// stream. Caller drives the worker protocol via Stream (or the
// Send / Recv pass-throughs); Close shuts both down.
type Connection struct {
	conn   *grpc.ClientConn
	client memqlv1.WorkerServiceClient
	stream memqlv1.WorkerService_StreamClient
}

// Dial opens the gRPC connection, attaches the auth token to the
// stream metadata, and starts the WorkerService bidi stream. The
// caller is responsible for sending the initial Register message
// against the returned stream -- the SDK does not assume the worker
// protocol's lifecycle. See cmd/memql-cockpit/internal/worker/
// connect.go for the reference register / heartbeat / tool-result
// loop.
//
// On any error the partially-opened resources are cleaned up before
// the error is returned.
func Dial(ctx context.Context, cfg DialConfig) (*Connection, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("sdk/worker: DialConfig.Endpoint is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("sdk/worker: DialConfig.Token is required")
	}
	maxMsg := cfg.MaxMessageSize
	if maxMsg <= 0 {
		maxMsg = DefaultMaxMessageSize
	}

	if cfg.Logger != nil {
		cfg.Logger.Info("worker connecting to cluster",
			"endpoint", cfg.Endpoint,
			"tls", cfg.UseTLS,
		)
	}

	dialOpts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsg),
			grpc.MaxCallSendMsgSize(maxMsg),
		),
	}
	if cfg.UseTLS {
		tlsCfg, err := BuildTLSConfig(cfg.Endpoint, cfg.Logger)
		if err != nil {
			return nil, fmt.Errorf("sdk/worker: tls config: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(cfg.Endpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("sdk/worker: dial %s: %w", cfg.Endpoint, err)
	}

	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Worker "+cfg.Token)

	client := memqlv1.NewWorkerServiceClient(conn)
	stream, err := client.Stream(streamCtx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sdk/worker: open stream: %w", err)
	}

	return &Connection{
		conn:   conn,
		client: client,
		stream: stream,
	}, nil
}

// Stream returns the underlying bidi stream. Caller drives the
// worker protocol against it (Register, Heartbeat, ToolResult,
// etc.).
func (c *Connection) Stream() memqlv1.WorkerService_StreamClient {
	if c == nil {
		return nil
	}
	return c.stream
}

// Send is a pass-through to Stream().Send(msg). Convenience method
// so the consumer doesn't have to grab the stream explicitly for
// each call.
func (c *Connection) Send(msg *memqlv1.WorkerClientMessage) error {
	if c == nil || c.stream == nil {
		return fmt.Errorf("sdk/worker: connection is closed")
	}
	return c.stream.Send(msg)
}

// Recv is a pass-through to Stream().Recv().
func (c *Connection) Recv() (*memqlv1.WorkerServerMessage, error) {
	if c == nil || c.stream == nil {
		return nil, fmt.Errorf("sdk/worker: connection is closed")
	}
	return c.stream.Recv()
}

// Close terminates the stream (CloseSend) and the underlying gRPC
// connection. Safe to call on a nil receiver and idempotent.
func (c *Connection) Close() {
	if c == nil {
		return
	}
	if c.stream != nil {
		_ = c.stream.CloseSend()
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
