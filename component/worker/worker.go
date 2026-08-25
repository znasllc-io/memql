// Package worker hosts the WorkerService gRPC surface and the
// in-memory registry of connected Cockpit worker machines. Lives on
// the agent node binary; per-user routing means agents in the same
// cluster only dispatch to workers owned by their session-owner.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/common"
)

const (
	// ComponentName identifies the worker service in the dependency graph.
	ComponentName = common.ComponentName("workerService")

	// TokenPrefix is the canonical scheme for worker authentication tokens.
	TokenPrefix = "mql_wkr_"

	// HeartbeatBatchInterval throttles the per-worker lastSeenAt DB
	// flush: the stream handler persists a heartbeat at most once
	// per interval (the first heartbeat of a stream always
	// persists). The in-memory registry is updated on every
	// heartbeat regardless. See streamSession.handleHeartbeat.
	//
	// It was 60s, and the reason given was that a per-beat write bought
	// no freshness anyone read. That reasoning was circular: nothing read
	// lastSeenAt BECAUSE a minute-stale timestamp answers no question
	// worth asking, and staleness was therefore the cause of the disuse
	// rather than a consequence of it. The Fleet (epic memql#4349) asks
	// the question -- `online` is DERIVED from lastSeenAt against
	// OnlineWindow, which is two of these intervals -- so this cadence is
	// now the freshness budget of that flag. At 60s a closed laptop would
	// have read as online for two more minutes. At 15s, the cockpit's own
	// beat, the flush is one write per worker per beat and `online`
	// decays within 30s. See IsOnline in online.go.
	HeartbeatBatchInterval = 15 * time.Second

	// DispatchTimeoutDefault is the default ToolDispatch timeout when
	// the calling tool doesn't supply one.
	DispatchTimeoutDefault = 5 * time.Minute
)

// HashToken returns the SHA-256 hex digest of a worker token.
// Storage and lookup paths use the hash; plaintext is only ever
// shown once at issuance.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// HasTokenPrefix reports whether s carries the worker token prefix.
// Used by the auth interceptor to short-circuit token classification
// before the SHA-256 lookup.
func HasTokenPrefix(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), TokenPrefix)
}

// Service is the top-level worker subsystem. Wraps the gRPC server
// implementation, the registry, and the dispatch dispatcher behind a
// single Dependency surface so app/ can wire it without touching the
// guts.
type Service struct {
	logger    *slog.Logger
	store     Store
	registry  *Registry
	server    *server
	auditor   Auditor
	clock     func() time.Time
	mu        sync.Mutex
	running   bool
	startedAt time.Time
}

// Store is the engine-backed persistence interface for worker rows.
// Implementations live in component/worker/store.go (the production
// adapter) and the integration test layer.
type Store interface {
	CreateRegistration(ctx context.Context, row RegistrationRow) error
	// RefreshRegistration re-stamps the registration-authoritative
	// fields (name, capabilities, capabilityDescriptor, labels,
	// concurrency, platform, permissions, version, buildTag,
	// lastSeenAt, lastConnectedFromIP) on an existing row when its
	// worker reconnects. A nil row.CapabilityDescriptor CLEARS the
	// persisted descriptor -- the worker no longer advertises one.
	RefreshRegistration(ctx context.Context, row RegistrationRow) error
	// UpdateLastSeen flushes the batched heartbeat: lastSeenAt plus the
	// two fields that only mean anything while a stream is live --
	// connectedNodeId (which replica holds it) and activeCount (how many
	// calls are in flight on it).
	UpdateLastSeen(ctx context.Context, registrationId, ownerUserId string, lastSeenAt time.Time, sourceIP, connectedNodeId string, activeCount int) error
	// ClearConnectedNode is the disconnect half of connectedNodeId.
	// Without it a machine whose stream dropped keeps naming the replica
	// that used to hold it, and a router forwards a dispatch to a node
	// that will refuse it -- which presents as a mesh fault rather than
	// as an offline laptop. lastSeenAt is deliberately NOT touched: it
	// records when the machine was last heard from, and moving it on the
	// way out would make a disconnected worker look fresh for one whole
	// online window.
	ClearConnectedNode(ctx context.Context, registrationId, ownerUserId string) error
	RevokeRegistration(ctx context.Context, registrationId, ownerUserId, revokedBy, reason string, at time.Time) error
	// WorkerByIdentityId takes the OWNER as well as the identity, and the
	// owner is not redundant: v1:worker:registration declares an owned
	// tier, so the read returns zero rows unless the context carries an
	// actor (see the borrowed-authority note at the top of store.go). The
	// register handshake has already resolved the WorkerIdentity, which
	// names the owner, before it asks this question.
	WorkerByIdentityId(ctx context.Context, identityId, ownerUserId string) (*RegistrationRow, error)
	// UpdateApps re-stamps the reported app inventory and the labels
	// derived from it (memql#4359). Called out of band from the
	// throttled lastSeenAt flush because an inventory change alters
	// ROUTING, and a row whose `app:` labels disagree with the live
	// registry entry is a split no reader can detect.
	//
	// Owner-scoped like the reads above, and for the same reason:
	// v1:worker:registration declares an owned tier, so the write needs
	// a context carrying an actor.
	UpdateApps(ctx context.Context, registrationId, ownerUserId string, apps []AppInfo, labels map[string]string, at time.Time, sourceIP string) error
	WorkersForUser(ctx context.Context, ownerUserId string) ([]RegistrationRow, error)
	CreateInvocation(ctx context.Context, row InvocationRow) error
	IdentityByTokenHash(ctx context.Context, tokenHash string) (*WorkerIdentity, error)
}

// Auditor emits security events for worker actions. Wraps the
// existing v1:identity:auditEvent infrastructure -- the service
// hands off events to keep this package free of the identity import.
type Auditor interface {
	Emit(ctx context.Context, event AuditEvent)
}

// AuditEvent is a worker-shaped security event. Translated to a
// v1:identity:auditEvent row by the auditor implementation.
type AuditEvent struct {
	Action        string
	Actor         string
	Target        string
	TargetType    string
	CorrelationId string
	OwnerUserId   string
	Detail        map[string]any
	Timestamp     time.Time
}

// RegistrationRow is the persistence projection of v1:worker:registration.
type RegistrationRow struct {
	ID           string
	OwnerUserId  string
	IdentityId   string
	Name         string
	Capabilities []string
	// CapabilityDescriptor is the optional structured capability
	// self-description from Register.capability_descriptor_json.
	// Nil when the worker didn't send one.
	CapabilityDescriptor *CapabilityDescriptor
	// Labels are the cockpit's own tags, overwritten from the Register
	// message on every reconnect. OperatorLabels are the owner's, set
	// from the Fleet page and never written by register or heartbeat --
	// which is the whole reason they are a second field rather than a
	// merge into Labels (design D3, memql#4350).
	Labels         map[string]string
	OperatorLabels map[string]string
	// DisplayName is the name the OWNER gave this machine. Name stays the
	// cockpit's hostname and is re-stamped on every reconnect, so a
	// rename kept there would not survive one.
	DisplayName string
	// ConnectedNodeId is the MEMQL_NODE_ID of the agent replica currently
	// holding this worker's stream, or empty when no replica does. It is
	// what makes a machine reachable from a replica that is NOT holding
	// its stream: the router forwards there instead of finding nothing.
	ConnectedNodeId string
	// LastSelectedAt is stamped by the router (touchWorkerSelected) on
	// every successful pick -- the shared clock roundRobin rotates on.
	// component/worker never writes it.
	LastSelectedAt time.Time
	// ActiveCount is calls in flight as of the most recent heartbeat
	// flush. Best-effort and up to one interval stale: a routing input
	// for leastLoaded, never a correctness one -- Worker.Acquire is the
	// real valve.
	ActiveCount int
	Concurrency map[string]uint32
	Platform    map[string]any
	Permissions map[string]any
	// Apps is the local-app inventory the cockpit reported
	// (memql#4359). Stored verbatim, ids the engine cannot drive
	// included; only a runnable entry becomes an `app:` routing label.
	//
	// Those labels go into Labels -- the COCKPIT's side of the pair --
	// rather than OperatorLabels: the engine derives them from what the
	// machine reported, and the owner does not set them. MergeLabels
	// then lets an operator label win, which is the right precedence.
	Apps                []AppInfo
	Version             string
	BuildTag            string
	RegisteredAt        time.Time
	LastSeenAt          time.Time
	LastConnectedFromIP string
	RevokedAt           time.Time
	RevokedBy           string
	RevokeReason        string
}

// IsActive reports whether the registration is currently usable.
func (r RegistrationRow) IsActive() bool {
	return r.RevokedAt.IsZero()
}

// InvocationRow is the persistence projection of v1:worker:invocation.
type InvocationRow struct {
	ID            string
	OwnerUserId   string
	WorkerId      string
	AgentId       string
	PlanId        string
	TaskId        string
	CorrelationId string
	Tool          string
	Action        string
	ArgsRedacted  map[string]any
	StartedAt     time.Time
	CompletedAt   time.Time
	DurationMs    int
	Outcome       string
	ExitCode      int
	Signal        string
	ErrorCode     string
	ErrorMessage  string
	BytesIn       int
	BytesOut      int
	OutputPreview string
	// Routing records HOW this machine was chosen -- the policy row, the
	// strategy it named, the candidates considered (memql#4351). Free
	// shape because the router owns it; the mutation coalesces a nil to
	// {}.
	Routing map[string]any
}

// WorkerIdentity is the auth-path projection: the minimum needed to
// resolve a presented token to an active registration.
type WorkerIdentity struct {
	IdentityId  string
	OwnerUserId string
	Active      bool
	ExpiresAt   time.Time
}

// Options configures NewService.
type Options struct {
	Logger  *slog.Logger
	Store   Store
	Auditor Auditor
	Clock   func() time.Time
	// NodeId is this replica's MEMQL_NODE_ID, stamped onto every
	// registration this node holds a stream for. Left empty it is read
	// from the environment once, at construction -- the same shape
	// component/campaigns and component/node use, and read ONCE rather
	// than per call so a single process cannot disagree with itself about
	// which node it is.
	NodeId string
}

// NewService constructs a worker subsystem. The returned service is
// not yet running; mount it on the gRPC server with Register and
// then call Start.
func NewService(opts Options) (*Service, error) {
	if opts.Logger == nil {
		return nil, fmt.Errorf("worker: logger required")
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("worker: store required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	registry := NewRegistry(opts.Logger, clock)
	svc := &Service{
		logger:   opts.Logger,
		store:    opts.Store,
		registry: registry,
		auditor:  opts.Auditor,
		clock:    clock,
	}
	svc.server = newServer(opts.Logger, opts.Store, registry, opts.Auditor, clock, resolveNodeId(opts.NodeId))
	return svc, nil
}

// resolveNodeId returns the explicit node id when one was configured and
// otherwise reads MEMQL_NODE_ID. There is no accessor to call: every other
// component that needs this value reads the variable directly
// (component/node/identity.go, component/campaigns/config.go), and
// component/envregistry publishes the manifest entry rather than a getter.
// An empty result is not an error -- a single-process deployment sets no node
// id, and an empty connectedNodeId there is honest: there is one replica and
// nothing to forward to.
func resolveNodeId(configured string) string {
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("MEMQL_NODE_ID"))
}

// Register attaches the WorkerService implementation to the supplied
// gRPC server. Must be called BEFORE the gRPC server starts serving.
func (s *Service) Register(grpcServer *grpc.Server) {
	if s == nil || s.server == nil || grpcServer == nil {
		return
	}
	memqlv1.RegisterWorkerServiceServer(grpcServer, s.server)
}

// Registry exposes the in-memory worker registry for the dispatcher.
// Caller cannot mutate registrations directly -- only via the
// gRPC stream lifecycle.
func (s *Service) Registry() *Registry {
	if s == nil {
		return nil
	}
	return s.registry
}

// NodeId reports the replica id this service stamps onto connectedNodeId.
// The agent-side dispatcher reads it to decide local dispatch versus a
// cross-node forward (memql#4352), and takes it from HERE rather than reading
// MEMQL_NODE_ID again: two readings of one variable is two chances to
// disagree, and the disagreement would be a dispatcher that forwards every
// call to a peer for machines connected to itself.
func (s *Service) NodeId() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.nodeId
}

// Start begins worker subsystem background work. Currently no-op;
// the registry's heartbeat flusher is wired by the per-stream
// goroutines in server.go.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	s.running = true
	s.startedAt = s.clock()
	s.logger.Info("worker service started",
		"component", string(ComponentName),
	)
}

// Stop gracefully shuts down the registry and ends every active
// stream.
func (s *Service) Stop(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	if s.registry != nil {
		s.registry.Drain()
	}
	s.running = false
	s.logger.Info("worker service stopped",
		"component", string(ComponentName),
		"uptime_seconds", int(s.clock().Sub(s.startedAt).Seconds()),
	)
}

// IsRunning reports whether the service has been started.
func (s *Service) IsRunning() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Order ensures the worker service starts after the gRPC server but
// before integrations that dispatch tools.
func (*Service) Order() int { return 60 }

// ComponentName identifies the worker service in the dependency graph.
func (*Service) ComponentName() common.ComponentName { return ComponentName }

// Ready returns a channel that's closed once the service is running.
// Mirrors the lifecycle hook used by the rest of the cluster.
func (s *Service) Ready() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
