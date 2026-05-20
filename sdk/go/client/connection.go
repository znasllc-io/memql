// Package client is the canonical Go SDK for talking to a memQL
// cluster. It manages the gRPC connection -- one persistent bidirectional
// stream per cluster -- and provides typed wrappers for queries,
// mutations, subscriptions, and Sense calls, with automatic correlation
// of request/response messages.
//
// Every memQL Go client (the cockpit TUI, the worker daemon, future
// integrations) should consume this package rather than reimplementing
// the wire dance. Higher-level helpers (voice, computer-use,
// chat-state-machine) live in sibling sub-packages under
// memql/sdk/go/.
package client

import (
	"context"
	"fmt"
	"log/slog"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Connection holds an active gRPC stream to a memQL cluster.
type Connection struct {
	conn       *grpc.ClientConn
	client     memqlv1.MemqlServiceClient
	stream     memqlv1.MemqlService_StreamClient
	dispatcher *Dispatcher
	logger     *slog.Logger

	// Server info from handshake.
	NodeId  string
	Version string
}

// ConnectConfig configures a new gRPC connection.
type ConnectConfig struct {
	Endpoint string
	Token    string // JWT bearer token (empty for no-auth mode)
	Logger   *slog.Logger
}

// Connect dials the gRPC endpoint, opens a bidirectional stream,
// and performs the ClientHello/ServerHello handshake.
//
// The endpoint may be a bare host:port (plaintext gRPC), or carry
// an explicit scheme: http://, grpc:// (plaintext), https://,
// grpcs:// (TLS). When TLS is selected the client uses the system
// trust store -- mkcert-issued dev certs are trusted automatically
// once `make setup-tls` has installed the local root CA, so
// https://bff.local.znas.io Just Works against the canonical local
// stack. See ParseClusterEndpoint for the full grammar.
func Connect(ctx context.Context, cfg ConnectConfig) (*Connection, error) {
	dial, useTLS, err := ParseClusterEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint %s: %w", cfg.Endpoint, err)
	}
	var transport grpc.DialOption
	if useTLS {
		// nil tls.Config -> use system trust store + ServerName from
		// the dial target. The cockpit talks to a public-looking name
		// (bff.${DOMAIN}) so SNI / verify-hostname work out of the box.
		transport = grpc.WithTransportCredentials(credentials.NewTLS(nil))
	} else {
		transport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	if cfg.Logger != nil {
		cfg.Logger.Info("cockpit dialing cluster",
			"endpoint", dial,
			"tls", useTLS,
		)
	}
	conn, err := grpc.NewClient(dial, transport)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", dial, err)
	}

	// The stream context must outlive the connect timeout.
	// Use a background context for the stream's lifetime, with metadata if needed.
	streamCtx := context.Background()
	if cfg.Token != "" {
		md := metadata.Pairs("authorization", "Bearer "+cfg.Token)
		streamCtx = metadata.NewOutgoingContext(streamCtx, md)
	}

	client := memqlv1.NewMemqlServiceClient(conn)
	stream, err := client.Stream(streamCtx)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open stream: %w", err)
	}

	c := &Connection{
		conn:   conn,
		client: client,
		stream: stream,
		logger: cfg.Logger,
	}

	// Start the response dispatcher.
	c.dispatcher = NewDispatcher(stream, cfg.Logger)
	go c.dispatcher.Run()

	// Perform handshake.
	if err := c.handshake(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}

	return c, nil
}

// handshake sends ClientHello and waits for ServerHello.
func (c *Connection) handshake(ctx context.Context) error {
	helloMsg := &memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{
				ClientId:   "memql-cockpit",
				SdkName:    "memql-cockpit",
				SdkVersion: "0.1.0",
			},
		},
	}

	resp, err := c.dispatcher.SendAndWait(ctx, helloMsg)
	if err != nil {
		return fmt.Errorf("send ClientHello: %w", err)
	}

	if hello := resp.GetServerHello(); hello != nil {
		c.NodeId = hello.GetNodeId()
		c.Version = hello.GetVersion()
		if c.logger != nil {
			c.logger.Info("connected to memQL node",
				"nodeId", c.NodeId,
				"version", c.Version,
			)
		}
	}

	return nil
}

// Dispatcher returns the message dispatcher for sending/receiving messages.
func (c *Connection) Dispatcher() *Dispatcher {
	return c.dispatcher
}

// Close shuts down the stream and connection.
func (c *Connection) Close() {
	if c.dispatcher != nil {
		c.dispatcher.Stop()
	}
	if c.stream != nil {
		c.stream.CloseSend()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}
