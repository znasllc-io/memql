// Package client is the canonical Go SDK for talking to a MemQL
// cluster. It manages the gRPC connection -- one persistent bidirectional
// stream per cluster -- and provides typed wrappers for queries,
// mutations, subscriptions, and Sense calls, with automatic correlation
// of request/response messages.
//
// Every MemQL Go client (the cockpit TUI, the worker daemon, future
// integrations) should consume this package rather than reimplementing
// the wire dance. Higher-level helpers (voice, computer-use,
// chat-state-machine) live in sibling sub-packages under
// memql/sdk/go/.
package client

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ReconnectConfig turns on the SDK's auto-reconnect (memql#4537).
//
// OPT-IN: a caller that manages its own lifecycle off Dispatcher.Unexpected()
// keeps exactly the behaviour it had. Enabling this moves that job into the
// SDK, once, for every Go consumer.
type ReconnectConfig struct {
	// InitialDelay is the first backoff delay (default 1s).
	InitialDelay time.Duration
	// MaxDelay is the backoff ceiling (default 30s).
	MaxDelay time.Duration
	// MaxAttempts gives up after this many consecutive failures. 0 = never.
	MaxAttempts int
	// StableAfter is how long a stream must SURVIVE before the backoff
	// resets (default 10s).
	//
	// Resetting on "a dial succeeded" would be wrong in the case that
	// matters: a server accepting streams and dropping them immediately looks
	// like success every time, so the client would hammer it at the initial
	// delay forever. Survival means a flapping server converges to the
	// ceiling and a healthy one that blips comes back at full speed.
	StableAfter time.Duration
	// TokenSource re-resolves the bearer per redial. A stream that has been
	// down for a while may be holding a token that expired while it was down,
	// and redialing with it just fails again. nil re-presents ConnectConfig's
	// static Token.
	TokenSource func(context.Context) (string, error)
}

// ConnectionStatus is what a caller renders (memql#4537).
type ConnectionStatus string

const (
	StatusConnected    ConnectionStatus = "connected"
	StatusReconnecting ConnectionStatus = "reconnecting"
	// StatusDisconnected is FINAL: Close() was called, or the attempt budget
	// is spent.
	StatusDisconnected ConnectionStatus = "disconnected"
)

// Connection holds an active gRPC stream to a MemQL cluster.
type Connection struct {
	conn       *grpc.ClientConn
	client     memqlv1.MemqlServiceClient
	stream     memqlv1.MemqlService_StreamClient
	dispatcher *Dispatcher
	logger     *slog.Logger

	// ---- auto-reconnect (memql#4537), nil/zero when not configured -------
	reconnect     *ReconnectConfig
	streamCtx     context.Context
	subscriptions *SubscriptionManager
	closeOnce     sync.Once
	closed        chan struct{}

	stateMu   sync.Mutex
	status    ConnectionStatus
	attempt   int
	lastErr   error
	cycle     uint64
	onCycle   []func(uint64)
	finalOnce sync.Once
	finalCh   chan struct{}

	// Server info from handshake.
	NodeId string
	// Version is the WIRE PROTOCOL version the node speaks ("v1"), not its
	// release. Read EngineVersion for that.
	Version string
	// EngineVersion is the release the node's binary was cut from --
	// e.g. "v0.18.1" -- or "dev" when it was not cut from a release
	// (memql#3998). Empty when the node predates the field, which is a
	// meaningful third answer: it says the cluster is older than this
	// contract, not that it has no version.
	EngineVersion string
}

// ConnectConfig configures a new gRPC connection.
type ConnectConfig struct {
	Endpoint string
	Token    string // JWT bearer token (empty for no-auth mode)
	Logger   *slog.Logger
	// Reconnect turns on SDK-owned auto-reconnect with resubscribe
	// (memql#4537). nil keeps the historic one-shot behaviour exactly.
	Reconnect *ReconnectConfig
}

// Connect dials the gRPC endpoint, opens a bidirectional stream,
// and performs the ClientHello/ServerHello handshake.
//
// The endpoint may be a bare host:port (plaintext gRPC), or carry
// an explicit scheme: http://, grpc:// (plaintext), https://,
// grpcs:// (TLS). When TLS is selected the client uses the system
// trust store, so a publicly-trusted https://bff.<domain> endpoint
// Just Works. The local k3d cluster is reached over plaintext gRPC
// via the bff port-forward (localhost:50051). See
// ParseClusterEndpoint for the full grammar.
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
		conn:      conn,
		client:    client,
		stream:    stream,
		logger:    cfg.Logger,
		reconnect: normalizeReconnect(cfg.Reconnect),
		streamCtx: streamCtx,
		closed:    make(chan struct{}),
		status:    StatusConnected,
		finalCh:   make(chan struct{}),
	}

	// Start the response dispatcher. Supervision is what keeps a node roll
	// from looking like the end of the connection: with reconnect on, only
	// this Connection decides that.
	if c.reconnect != nil {
		c.dispatcher = NewSupervisedDispatcher(stream, cfg.Logger)
	} else {
		c.dispatcher = NewDispatcher(stream, cfg.Logger)
	}
	go c.dispatcher.Run()

	// Perform handshake.
	if err := c.handshake(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}

	if c.reconnect != nil {
		c.subscriptions = NewSubscriptionManager(c.dispatcher)
		go c.supervise()
	}

	return c, nil
}

func normalizeReconnect(in *ReconnectConfig) *ReconnectConfig {
	if in == nil {
		return nil
	}
	out := *in
	if out.InitialDelay <= 0 {
		out.InitialDelay = time.Second
	}
	if out.MaxDelay < out.InitialDelay {
		// A ceiling under the floor would make the backoff run BACKWARDS,
		// retrying faster the longer an outage lasts.
		out.MaxDelay = maxDuration(30*time.Second, out.InitialDelay)
	}
	if out.StableAfter <= 0 {
		out.StableAfter = 10 * time.Second
	}
	return &out
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// Subscriptions returns the connection-owned subscription manager, which is
// what a reconnect replays (memql#4537). Non-nil only when ReconnectConfig
// was supplied: without it the SDK has no reconnect to replay for, and a
// caller building its own manager over Dispatcher() is the historic shape.
func (c *Connection) Subscriptions() *SubscriptionManager {
	return c.subscriptions
}

// Status reports the connection state. Without ReconnectConfig it is
// StatusConnected until Close().
func (c *Connection) Status() ConnectionStatus {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.status
}

// Attempt is the number of consecutive failed redials since the last live
// stream. 0 while connected.
func (c *Connection) Attempt() int {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.attempt
}

// OnReconnect registers a hook fired once per successful RECONNECT, AFTER
// every subscription has been replayed on the new stream.
//
// This is the seam a caller re-reads on. It fires after the replay, never
// before, because a re-read racing its own subscription is exactly the
// read-then-subscribe hole the ordering contract closes (memql#4536). It is
// NOT fired for the first connection: a caller that just connected is about
// to read anyway.
func (c *Connection) OnReconnect(fn func(cycle uint64)) {
	if fn == nil {
		return
	}
	c.stateMu.Lock()
	c.onCycle = append(c.onCycle, fn)
	c.stateMu.Unlock()
}

// Final returns a channel closed when the connection is FINISHED: Close() was
// called, or the redial budget is spent. With reconnect on, a transport drop
// the SDK recovers from deliberately does not close it -- telling callers the
// connection ended each time a node rolled is the behaviour this replaces.
func (c *Connection) Final() <-chan struct{} {
	return c.finalCh
}

// supervise runs the reconnect loop for a reconnect-enabled connection.
func (c *Connection) supervise() {
	for {
		select {
		case <-c.closed:
			return
		case <-c.dispatcher.TransportDown():
		}
		select {
		case <-c.closed:
			return
		default:
		}
		if !c.reconnectLoop() {
			return
		}
	}
}

// reconnectLoop redials until it succeeds, the caller closes, or the budget
// runs out. Returns false when the connection is finished.
func (c *Connection) reconnectLoop() bool {
	cfg := c.reconnect
	c.setStatus(StatusReconnecting)
	for {
		c.stateMu.Lock()
		c.attempt++
		attempt := c.attempt
		c.stateMu.Unlock()

		if cfg.MaxAttempts > 0 && attempt > cfg.MaxAttempts {
			c.finish(StatusDisconnected)
			return false
		}

		delay := backoffDelay(attempt-1, cfg.InitialDelay, cfg.MaxDelay)
		select {
		case <-c.closed:
			return false
		case <-time.After(delay):
		}

		if err := c.redial(); err != nil {
			c.stateMu.Lock()
			c.lastErr = err
			c.stateMu.Unlock()
			if c.logger != nil {
				c.logger.Warn("memql sdk: redial failed", "attempt", attempt, "error", err)
			}
			continue
		}

		// Live again. REPLAY FIRST, then notify -- a caller that re-reads on
		// the hook must already be subscribed when its read goes out.
		if c.subscriptions != nil {
			c.subscriptions.Replay()
		}
		c.armStable()
		c.setStatus(StatusConnected)
		c.notifyCycle()
		return true
	}
}

func (c *Connection) redial() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	streamCtx := c.streamCtx
	if c.reconnect.TokenSource != nil {
		token, err := c.reconnect.TokenSource(ctx)
		if err != nil {
			return fmt.Errorf("resolve token: %w", err)
		}
		streamCtx = context.Background()
		if token != "" {
			streamCtx = metadata.NewOutgoingContext(streamCtx,
				metadata.Pairs("authorization", "Bearer "+token))
		}
	}

	stream, err := c.client.Stream(streamCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	if err := c.dispatcher.Rebind(stream); err != nil {
		return err
	}
	c.stream = stream
	c.streamCtx = streamCtx
	go c.dispatcher.Run()
	if err := c.handshake(ctx); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}

// armStable resets the backoff once a stream has SURVIVED long enough to
// count as healthy. See ReconnectConfig.StableAfter for why survival rather
// than dial success is the trigger.
func (c *Connection) armStable() {
	stable := c.reconnect.StableAfter
	go func() {
		select {
		case <-c.closed:
		case <-c.dispatcher.TransportDown():
		case <-time.After(stable):
			c.stateMu.Lock()
			c.attempt = 0
			c.lastErr = nil
			c.stateMu.Unlock()
		}
	}()
}

func (c *Connection) setStatus(s ConnectionStatus) {
	c.stateMu.Lock()
	if c.status != StatusDisconnected {
		c.status = s
	}
	c.stateMu.Unlock()
}

func (c *Connection) notifyCycle() {
	c.stateMu.Lock()
	c.cycle++
	cycle := c.cycle
	hooks := make([]func(uint64), len(c.onCycle))
	copy(hooks, c.onCycle)
	c.stateMu.Unlock()
	for _, fn := range hooks {
		fn(cycle)
	}
}

func (c *Connection) finish(s ConnectionStatus) {
	c.stateMu.Lock()
	c.status = s
	c.stateMu.Unlock()
	c.finalOnce.Do(func() { close(c.finalCh) })
	// An exhausted budget is as terminal as a Close(): tear the transport
	// down so a caller ranging Events() or waiting on a reply is told, rather
	// than parked on a stream nothing is going to revive.
	if c.subscriptions != nil {
		c.subscriptions.Stop()
	}
	c.dispatcher.Stop()
}

// backoffDelay is exponential with FULL JITTER: a uniform draw from
// [0, capped), not a tight band around it.
//
// Full jitter is the shape that actually decorrelates a fleet. A node
// restarting drops every client at once, and a tight band leaves them moving
// as a herd -- the thundering retry lands on a node that has just come up
// with nothing warm.
func backoffDelay(attempt int, initial, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	capped := max
	if attempt < 32 {
		if scaled := initial << uint(attempt); scaled > 0 && scaled < max {
			capped = scaled
		}
	}
	return time.Duration(rand.Int63n(int64(capped) + 1))
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
		c.EngineVersion = hello.GetEngineVersion()
		if c.logger != nil {
			c.logger.Info("connected to MemQL node",
				"nodeId", c.NodeId,
				"protocolVersion", c.Version,
				"engineVersion", c.EngineVersion,
			)
		}
	}

	return nil
}

// Dispatcher returns the message dispatcher for sending/receiving messages.
func (c *Connection) Dispatcher() *Dispatcher {
	return c.dispatcher
}

// ClientConn returns the underlying gRPC client connection. Used by
// sibling typed clients (e.g. the DeployControl client) that speak a
// secondary unary service mounted on the same listener as
// MemqlService rather than riding the multiplexed Stream.
func (c *Connection) ClientConn() *grpc.ClientConn {
	return c.conn
}

// Close shuts down the stream and connection.
//
// A DELIBERATE close never reconnects, whatever the reconnect config says.
// That distinction is why the SDK can own reconnect at all: without it, "the
// stream died" and "the caller is finished" arrive at the same signal.
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		if c.closed != nil {
			close(c.closed)
		}
	})
	c.stateMu.Lock()
	c.status = StatusDisconnected
	c.stateMu.Unlock()
	c.finalOnce.Do(func() {
		if c.finalCh != nil {
			close(c.finalCh)
		}
	})
	if c.subscriptions != nil {
		c.subscriptions.Stop()
	}
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
