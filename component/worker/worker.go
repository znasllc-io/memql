// Package worker hosts the WorkerService gRPC surface and the
// in-memory registry of connected memql-cockpit workers. Lives on
// the agent node binary; per-user routing means agents in the same
// cluster only dispatch to workers owned by their session-owner.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
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

	// HeartbeatBatchInterval is how long the registry waits before
	// flushing accumulated heartbeats to the database.
	HeartbeatBatchInterval = 60 * time.Second

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
	UpdateLastSeen(ctx context.Context, registrationId string, lastSeenAt time.Time, sourceIP string) error
	RevokeRegistration(ctx context.Context, registrationId, revokedBy, reason string, at time.Time) error
	WorkerByIdentityId(ctx context.Context, identityId string) (*RegistrationRow, error)
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
	Labels               map[string]string
	Concurrency          map[string]uint32
	Platform             map[string]any
	Permissions          map[string]any
	Version              string
	BuildTag             string
	RegisteredAt         time.Time
	LastSeenAt           time.Time
	LastConnectedFromIP  string
	RevokedAt            time.Time
	RevokedBy            string
	RevokeReason         string
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
	svc.server = newServer(opts.Logger, opts.Store, registry, opts.Auditor, clock)
	return svc, nil
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
