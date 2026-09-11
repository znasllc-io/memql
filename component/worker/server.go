package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// CapabilityHeadless and CapabilityComputerUse name the host and desktop
// capabilities. HEADLESS is mandatory; COMPUTERUSE is optional, as is the
// inference capability ModelCapability defined in modelcall.go.
const (
	CapabilityHeadless    = "HEADLESS"
	CapabilityComputerUse = "COMPUTERUSE"
)

// server implements memqlv1.WorkerServiceServer. It owns the
// per-stream lifecycle: validate the Register payload, persist the
// registration, install a dispatch hook on the registry, and pump
// inbound ToolResult / Heartbeat / RotationRequest messages until
// the stream closes.
type server struct {
	memqlv1.UnimplementedWorkerServiceServer

	logger   *slog.Logger
	store    Store
	registry *Registry
	auditor  Auditor
	clock    func() time.Time
	// nodeId is this replica's MEMQL_NODE_ID, resolved once at
	// construction and stamped onto every registration whose stream this
	// node holds. Threaded rather than read per call: a process that
	// answered "which node am I" differently at register and at heartbeat
	// would leave a row pointing at a replica that never held the stream.
	nodeId string
	// pingFirst / pingEvery are FirstPingDelay / PingInterval (epic
	// memql#5218, D11), held on the server so a test can run the pinger in
	// milliseconds rather than waiting the real three seconds. Nothing in
	// production sets them to anything else; TestNewServer_PingsOnTheConstants
	// holds the defaults to the constants.
	pingFirst time.Duration
	pingEvery time.Duration
}

func newServer(logger *slog.Logger, store Store, registry *Registry, auditor Auditor, clock func() time.Time, nodeId string) *server {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &server{
		logger:    logger,
		store:     store,
		registry:  registry,
		auditor:   auditor,
		clock:     clock,
		nodeId:    nodeId,
		pingFirst: FirstPingDelay,
		pingEvery: PingInterval,
	}
}

// Stream is the single bidi RPC. The first inbound message must be
// a Register; everything else after that is heartbeat / tool result
// / audit / rotation request traffic.
func (s *server) Stream(stream memqlv1.WorkerService_StreamServer) error {
	ctx := stream.Context()
	identity, err := identityFromContext(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	sourceIP := peerAddrFromContext(ctx)

	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	register := first.GetRegister()
	if register == nil {
		_ = stream.Send(&memqlv1.WorkerServerMessage{
			Payload: &memqlv1.WorkerServerMessage_RegisterError{
				RegisterError: &memqlv1.RegisterError{
					Code:    "register_required",
					Message: "first message must be Register",
				},
			},
		})
		return status.Error(codes.InvalidArgument, "first worker message must be Register")
	}

	session, err := s.admitRegistration(ctx, stream, identity, register, sourceIP)
	if err != nil {
		_ = stream.Send(&memqlv1.WorkerServerMessage{
			Payload: &memqlv1.WorkerServerMessage_RegisterError{
				RegisterError: &memqlv1.RegisterError{
					Code:    "register_failed",
					Message: err.Error(),
				},
			},
		})
		return status.Error(codes.PermissionDenied, err.Error())
	}
	var streamErr error
	defer func() { session.close(streamErr) }()
	// The RegisterAck is on the wire; from here the cluster pings the
	// machine for the life of the stream (epic memql#5218, D11).
	session.startPinger()

	for {
		msg, err := stream.Recv()
		if err != nil {
			streamErr = err
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := session.handle(ctx, msg, sourceIP); err != nil {
			s.logger.Warn("worker stream message error",
				"registration_id", session.worker.RegistrationId,
				"error", err,
			)
		}
	}
}

func (s *server) admitRegistration(
	ctx context.Context,
	stream memqlv1.WorkerService_StreamServer,
	identity *WorkerIdentity,
	register *memqlv1.Register,
	sourceIP string,
) (*streamSession, error) {
	if !identity.Active {
		return nil, fmt.Errorf("worker token is inactive")
	}
	now := s.clock()
	if !identity.ExpiresAt.IsZero() && now.After(identity.ExpiresAt) {
		return nil, fmt.Errorf("worker token expired")
	}
	descriptor, err := validateRegister(register)
	if err != nil {
		return nil, err
	}

	registration, err := s.upsertRegistration(ctx, identity, register, descriptor, now, sourceIP)
	if err != nil {
		return nil, err
	}

	w := &Worker{
		RegistrationId:       registration.ID,
		OwnerUserId:          registration.OwnerUserId,
		IdentityId:           registration.IdentityId,
		Name:                 registration.Name,
		Capabilities:         registration.Capabilities,
		CapabilityDescriptor: registration.CapabilityDescriptor,
		Labels:               registration.Labels,
		Concurrency:          registration.Concurrency,
		Platform:             registration.Platform,
		Permissions:          registration.Permissions,
		Version:              registration.Version,
		BuildTag:             registration.BuildTag,
		ConnectedAt:          now,
		LastSeenAt:           now,
		SourceIP:             sourceIP,
	}
	w.SetApps(registration.Apps)
	// From the ROW rather than from the Register message, so the registry entry
	// and the row are the same object by construction. Idempotent against the
	// label merge upsertRegistration already did.
	w.SetHardware(InventoryFromRow(registration.Hardware))

	streamCtx, cancel := context.WithCancel(stream.Context())
	session := newStreamSession(s, stream, w, streamCtx, cancel)
	w.SetDispatchFunc(session.dispatch, cancel)
	w.SetDrainFunc(session.requestDrain)
	w.SetAppSessionFunc(session.openAppSession)
	w.SetModelCallFunc(session.openModelCall)
	w.SetModelPullFunc(session.openModelPull)
	w.SetModelProbeFunc(session.openModelProbe)
	s.registry.Add(w)

	if err := stream.Send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_RegisterAck{
			RegisterAck: &memqlv1.RegisterAck{
				RegistrationId: registration.ID,
				RegisteredAt:   timestamppb.New(registration.RegisteredAt),
				OwnerUserId:    registration.OwnerUserId,
			},
		},
	}); err != nil {
		s.registry.Remove(registration.ID)
		cancel()
		return nil, fmt.Errorf("send register ack: %w", err)
	}
	if s.auditor != nil {
		s.auditor.Emit(ctx, AuditEvent{
			Action:      "worker_registered",
			Actor:       "worker:" + registration.ID,
			Target:      registration.ID,
			TargetType:  "worker",
			OwnerUserId: registration.OwnerUserId,
			Detail: map[string]any{
				"name":         registration.Name,
				"capabilities": registration.Capabilities,
				"buildTag":     registration.BuildTag,
				"version":      registration.Version,
				"sourceIP":     sourceIP,
				"connectedAt":  now.Format(time.RFC3339),
				"apps":         auditAppDetail(registration.Apps),
			},
			Timestamp: now,
		})
	}
	s.logger.Info("worker registered",
		"registration_id", registration.ID,
		"owner_user_id", registration.OwnerUserId,
		"name", registration.Name,
		"capabilities", registration.Capabilities,
	)
	return session, nil
}

// upsertRegistration writes either a new row or refreshes an
// existing registration belonging to the identityId.
//
// Two fields are NOT written here and must stay that way: operatorLabels and
// displayName. Both are the OWNER's, set from the Fleet page, and both would
// otherwise be erased by the machine that carries them on the next reconnect
// -- `labels` and `name` beside them ARE overwritten from the Register
// message, which is exactly why the operator's versions are separate fields
// (design D3, memql#4350). The registration row this builds simply leaves them
// zero, and EngineStore.RefreshRegistration does not name them either, so the
// update's read-merge preserves whatever the owner set.
func (s *server) upsertRegistration(
	ctx context.Context,
	identity *WorkerIdentity,
	register *memqlv1.Register,
	descriptor *CapabilityDescriptor,
	now time.Time,
	sourceIP string,
) (RegistrationRow, error) {
	// The owner comes off the resolved WorkerIdentity, not off the Register
	// message: the registration concept is owner-tiered, so this read
	// returns nothing without an actor and the handshake would then insert a
	// duplicate row on every reconnect. See the note at the top of store.go.
	existing, err := s.store.WorkerByIdentityId(ctx, identity.IdentityId, identity.OwnerUserId)
	if err != nil {
		return RegistrationRow{}, fmt.Errorf("worker lookup: %w", err)
	}
	// Reclaim by stable machine key when the token is new (re-pair / rotate)
	// but the physical install already has an unrevoked registration. Lookup
	// by identityId alone is what minted the e18ce→6ec52e→0ba471→6aa926
	// duplicate lineage for one Mac.
	if existing == nil {
		existing, err = s.findRegistrationByMachineKey(ctx, identity.OwnerUserId, register)
		if err != nil {
			return RegistrationRow{}, err
		}
	}

	apps := AppsFromProto(register.GetApps())
	descriptors := AppDescriptorsFromProto(register.GetAppDescriptors())
	// The inventory was validated in validateRegister, which refuses the
	// registration on a malformed one; by here it can only be well-formed or
	// absent, and absent is the ordinary case for a cockpit that predates it.
	hardware, _ := InventoryFromProto(register.GetHardware())
	registration := RegistrationRow{
		IdentityId:           identity.IdentityId,
		OwnerUserId:          identity.OwnerUserId,
		Name:                 stringFallback(register.GetName(), platformHostname(register.GetPlatform())),
		Capabilities:         normalizeCapabilities(register.GetCapabilities()),
		CapabilityDescriptor: descriptor,
		// The persisted labels carry the derived `app:` labels alongside
		// whatever the cockpit reported (memql#4359), so a routing
		// decision made against the ROW agrees with one made against the
		// live registry entry -- which is what lets a planner node, with
		// no registry at all, answer the same question.
		// The `runtime:` labels ride the same reasoning one layer down: they are
		// derived from the inventory rather than reported, they live on the ROW
		// as well as in the registry, and a machine that reported no inventory
		// keeps whatever the cockpit sent under that prefix untouched.
		Labels:              mergeRuntimeLabels(mergeAppLabels(copyStringMap(register.GetLabels()), apps), hardware),
		Apps:                apps,
		AppDescriptors:      descriptors,
		Hardware:            hardware.Row(),
		Concurrency:         register.GetConcurrency(),
		Platform:            platformInfoToMap(register.GetPlatform()),
		Permissions:         permissionStatusToMap(register.GetPermissions()),
		Version:             register.GetVersion(),
		BuildTag:            register.GetBuildTag(),
		LastSeenAt:          now,
		LastConnectedFromIP: sourceIP,
		// This replica now holds the stream, so it is where a dispatch for
		// this machine has to be forwarded. Stamped on register and
		// re-asserted on every heartbeat flush; cleared on disconnect.
		ConnectedNodeId: s.nodeId,
	}

	if existing == nil {
		registration.ID = newRegistrationId()
		registration.RegisteredAt = now
		if err := s.store.CreateRegistration(ctx, registration); err != nil {
			return RegistrationRow{}, fmt.Errorf("worker create: %w", err)
		}
		return registration, nil
	}
	if !existing.RevokedAt.IsZero() {
		return RegistrationRow{}, fmt.Errorf("worker is revoked")
	}
	registration.ID = existing.ID
	registration.RegisteredAt = existing.RegisteredAt
	// Refresh the registration-authoritative fields, not just
	// lastSeenAt (memql#1332): the persisted capabilities /
	// capabilityDescriptor / platform / version must track the
	// LATEST Register message, or the row goes stale across cockpit
	// upgrades while the in-memory registry stays fresh. A
	// registration that omits the descriptor clears the persisted
	// one -- the worker no longer advertises it.
	if err := s.store.RefreshRegistration(ctx, registration); err != nil {
		return RegistrationRow{}, fmt.Errorf("worker refresh registration: %w", err)
	}
	return registration, nil
}


// findRegistrationByMachineKey returns the owner's unrevoked registration that
// matches the Register message's stable machine key, or nil when none match.
// Newest lastSeenAt wins when more than one row shares a key (should not
// happen once reclaim is live; still deterministic).
func (s *server) findRegistrationByMachineKey(ctx context.Context, ownerUserId string, register *memqlv1.Register) (*RegistrationRow, error) {
	want := MachineKeyFromRegister(register)
	if want == "" {
		return nil, nil
	}
	rows, err := s.store.WorkersForUser(ctx, ownerUserId)
	if err != nil {
		return nil, fmt.Errorf("worker machine-key lookup: %w", err)
	}
	var best *RegistrationRow
	for i := range rows {
		row := rows[i]
		if !row.RevokedAt.IsZero() {
			continue
		}
		if MachineKeyFromRow(row) != want {
			continue
		}
		if best == nil || row.LastSeenAt.After(best.LastSeenAt) {
			cp := row
			best = &cp
		}
	}
	return best, nil
}

// streamSession is the server-side state for one connected worker.
// Owns the inbound -> outbound bridge and the in-flight call map.
type streamSession struct {
	server *server
	stream memqlv1.WorkerService_StreamServer
	worker *Worker

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	pending map[string]chan *memqlv1.ToolResult
	// chunkSinks holds the per-call ToolStream callback, keyed by call id,
	// under the SAME lock as pending because the two have the same
	// lifetime: registered when a dispatch goes out, dropped when its
	// result lands or the call is abandoned. Only calls that asked for
	// chunks get an entry, so a missing key is the ordinary case rather
	// than an error.
	chunkSinks map[string]func(*memqlv1.ToolStream)
	// sessions holds the live app-session handles (memql#4359), under
	// the same lock and for the same reason chunkSinks is: a session's
	// lifetime is the stream's, and a disconnect must end every one.
	sessions map[string]*AppSessionHandle
	// modelCalls holds the live model-call handles (memql#4677), under
	// the same lock and for the same reason: a call's lifetime is the
	// stream's, and a disconnect must end every one.
	modelCalls map[string]*ModelCallHandle
	// modelPulls is the pull table. Separate from modelCalls because the
	// id spaces are separate and a pull is not a call: conflating them
	// would let a cancel for one reach the other.
	modelPulls map[string]*ModelPullHandle
	// modelProbes is the probe table, separate from modelPulls for the reason
	// modelPulls is separate from modelCalls: the id spaces are separate, and a
	// cancel for one must not reach the other.
	modelProbes map[string]*ModelProbeHandle
	sendMu      sync.Mutex
	sendErr     error
	closeOnce   sync.Once

	// lastPersistedAt is the heartbeat timestamp of the most recent
	// successful lastSeenAt DB flush (memql#1340). Zero until the
	// first heartbeat of the stream persists. Only touched from
	// handleHeartbeat, which runs on the single stream-recv
	// goroutine, so it needs no lock.
	lastPersistedAt time.Time
	// hardwarePending is a non-material inventory refresh that has reached the
	// registry and not yet the row (epic memql#5146). It is a FLAG ON THE
	// SESSION rather than a local in the beat handler, and that is the whole
	// point of it: the cockpit reports hardware on every tenth beat, so a
	// change arriving inside the throttle window would otherwise be dropped by
	// the nine beats that carry no inventory and only reach the row at the
	// tenth. The flag makes the very next flush carry it.
	hardwarePending bool

	// pingMu guards the ONE outstanding Ping (epic memql#5218, D11). Two
	// goroutines meet here -- runPinger writes a fresh id on its own timer,
	// the stream-recv goroutine reads and clears it on the Pong -- so unlike
	// lastPersistedAt this cannot ride the recv goroutine's serialisation.
	// One outstanding rather than a table: a Ping that goes unanswered for a
	// whole PingInterval is superseded, not remembered, and a Pong for it is
	// then stale and dropped.
	pingMu           sync.Mutex
	outstandingPing  string
	outstandingPingS time.Time
	// rttMs / rttAt are the latest measured round trip, the value the next
	// heartbeat flush persists. Written by handlePong and read by
	// handleHeartbeat, both on the recv goroutine, so like lastPersistedAt
	// they need no lock. A zero rttAt is NOT MEASURED and the flush leaves
	// both out of the write.
	// both out of the write.
	rttMs int
	rttAt time.Time
	// drainReason, when non-empty, was set by requestDrain before the stream
	// ended. disconnectCodeReason prefers it so roll logs say server_drain
	// rather than a bare Canceled from context cancel.
	drainReason string
}

func newStreamSession(
	srv *server,
	stream memqlv1.WorkerService_StreamServer,
	w *Worker,
	ctx context.Context,
	cancel context.CancelFunc,
) *streamSession {
	return &streamSession{
		server:      srv,
		stream:      stream,
		worker:      w,
		ctx:         ctx,
		cancel:      cancel,
		pending:     make(map[string]chan *memqlv1.ToolResult),
		chunkSinks:  make(map[string]func(*memqlv1.ToolStream)),
		sessions:    make(map[string]*AppSessionHandle),
		modelCalls:  make(map[string]*ModelCallHandle),
		modelPulls:  make(map[string]*ModelPullHandle),
		modelProbes: make(map[string]*ModelProbeHandle),
	}
}

func (s *streamSession) requestDrain(reason string) {
	if s == nil {
		return
	}
	if reason == "" {
		reason = DisconnectReasonServerDrain
	}
	s.mu.Lock()
	if s.drainReason == "" {
		s.drainReason = reason
	}
	s.mu.Unlock()
	_ = s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_Drain{
			Drain: &memqlv1.Drain{},
		},
	})
	// Grace then cancel: cockpit finishes in-flight work on Drain, then the
	// stream must not block SIGTERM / GracefulStop forever.
	go func() {
		t := time.NewTimer(3 * time.Second)
		defer t.Stop()
		select {
		case <-t.C:
			s.cancel()
		case <-s.ctx.Done():
		}
	}()
}

func (s *streamSession) close(cause error) {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.server != nil && s.server.registry != nil {
			// Session-scoped: only drop THIS worker pointer. Unconditional
			// Remove(registrationId) deletes a successor already Add'ed on
			// reclaim/reconnect while the DB connectedNodeId stamp remains.
			s.server.registry.RemoveSession(s.worker)
		}
		s.mu.Lock()
		pendingCount := len(s.pending)
		for _, ch := range s.pending {
			close(ch)
		}
		s.pending = nil
		s.chunkSinks = nil
		// A disconnect ends every live app session with a NAMED error.
		// Without this a caller parked in Wait would sit there until its
		// own context expired, with nothing in the log saying the machine
		// had gone away.
		liveSessions := make([]*AppSessionHandle, 0, len(s.sessions))
		for _, h := range s.sessions {
			liveSessions = append(liveSessions, h)
		}
		s.sessions = nil
		// The same argument for model calls: a caller parked in Wait
		// would otherwise sit until its own ceiling expired, with
		// nothing saying the machine had gone.
		liveCalls := make([]*ModelCallHandle, 0, len(s.modelCalls))
		for _, h := range s.modelCalls {
			liveCalls = append(liveCalls, h)
		}
		s.modelCalls = nil
		// And the same for pulls, where the stake is higher: a pull that
		// loses its stream has left bytes on somebody's disk, so a caller
		// parked in Wait must be told the machine went away rather than
		// being left to conclude the download is still running.
		livePulls := make([]*ModelPullHandle, 0, len(s.modelPulls))
		for _, h := range s.modelPulls {
			livePulls = append(livePulls, h)
		}
		s.modelPulls = nil
		// And probes, where the caller is a goroutine ranging over the progress
		// channel: a handle left unfinished on a disconnect leaves it ranging
		// forever, and the runner that owns it never releases the probe id.
		liveProbes := make([]*ModelProbeHandle, 0, len(s.modelProbes))
		for _, h := range s.modelProbes {
			liveProbes = append(liveProbes, h)
		}
		s.modelProbes = nil
		s.mu.Unlock()
		for _, h := range liveProbes {
			h.Finish(ModelProbeOutcome{Error: "worker_disconnected"}, ErrWorkerDisconnected)
		}
		for _, h := range liveSessions {
			h.finish(AppSessionOutcome{Error: "worker_disconnected"}, ErrWorkerDisconnected)
		}
		for _, h := range liveCalls {
			h.finish(ModelCallOutcome{
				FinishReason: ModelFinishError,
				Error:        "worker_disconnected",
				ErrorCode:    "worker_disconnected",
			}, ErrWorkerDisconnected)
		}
		for _, h := range livePulls {
			h.finish(ModelPullOutcome{Error: "worker_disconnected"}, ErrWorkerDisconnected)
		}
		s.clearConnectedNode()
		// Log disconnect symmetrically to "worker registered" on the
		// connect path. Without this the agent log was silent on
		// disconnect -- the only signal was the absence of further
		// "worker registered" lines on reconnect, which made it hard to
		// answer "is the worker actually connected right now?" from the
		// log alone.
		if s.server != nil && s.server.logger != nil {
			fields := []any{
				"registration_id", s.worker.RegistrationId,
				"owner_user_id", s.worker.OwnerUserId,
				"name", s.worker.Name,
			}
			if pendingCount > 0 {
				fields = append(fields, "pending_calls_aborted", pendingCount)
			}
			if len(liveSessions) > 0 {
				fields = append(fields, "app_sessions_aborted", len(liveSessions))
			}
			if len(liveCalls) > 0 {
				fields = append(fields, "model_calls_aborted", len(liveCalls))
			}
			s.mu.Lock()
			drainReason := s.drainReason
			s.mu.Unlock()
			code, reason := disconnectCodeReason(cause, drainReason)
			fields = append(fields, "code", code, "reason", reason)
			s.server.logger.Info("worker disconnected", fields...)
		}
		// Emit an audit event mirroring "worker_registered" so downstream
		// security telemetry has a paired connect/disconnect record.
		if s.server != nil && s.server.auditor != nil {
			s.server.auditor.Emit(s.ctx, AuditEvent{
				Action:      "worker_disconnected",
				Actor:       "worker:" + s.worker.RegistrationId,
				Target:      s.worker.RegistrationId,
				TargetType:  "worker",
				OwnerUserId: s.worker.OwnerUserId,
				Detail: map[string]any{
					"name":                s.worker.Name,
					"connectedAt":         s.worker.ConnectedAt.Format(time.RFC3339),
					"pendingCallsAborted": pendingCount,
					"appSessionsAborted":  len(liveSessions),
				},
				Timestamp: time.Now().UTC(),
			})
		}
	})
}

// clearConnectedNode blanks the registration's connectedNodeId now that this
// replica no longer holds the stream. Until it runs, the row still names this
// node and a router forwards a dispatch to a replica that will refuse it --
// which reads as a mesh fault rather than as an offline laptop.
//
// SUCCESSOR GUARD: if another replica already re-stamped connectedNodeId
// during our teardown (same machine re-attach after a roll / sticky rebind),
// do NOT blank their hold. Prod tip windows showed clear→new-holder races that
// left Ask forwarding to a dead replica while StreamHeld still read true, or
// wiped a live hold and flapped readiness.
//
// THE CONTEXT COMES FROM Background(), NOT from the session. By the time close
// runs, s.cancel has already fired and s.ctx is done, so a write on it would
// be cancelled before it left the process -- and the failure would be silent,
// because the flush is best-effort. The audit Emit just below still passes
// s.ctx; that is a separate question about a separate sink and is not the
// pattern to copy here.
func (s *streamSession) clearConnectedNode() {
	if s == nil || s.server == nil || s.server.store == nil || s.worker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// SAME-POD SUCCESSOR (prod dump: Ask on holder pszjr). Reclaim/reconnect
	// Add's a new Worker under the same registration id before this close runs.
	// RemoveSession keeps that pointer; blanking connectedNodeId here would
	// leave StreamHeld false OR — if a later flush re-stamps — Connected with
	// no registry entry if Remove raced. Never clear while a different Worker
	// pointer still owns this registration in the local registry.
	if s.server.registry != nil && s.worker != nil {
		if live := s.server.registry.WorkerById(s.worker.RegistrationId); live != nil && live != s.worker {
			if s.server.logger != nil {
				s.server.logger.Info("worker: skip clear connectedNodeId; local successor holds stream",
					"registration_id", s.worker.RegistrationId,
					"self_node_id", strings.TrimSpace(s.server.nodeId),
				)
			}
			return
		}
	}
	self := strings.TrimSpace(s.server.nodeId)
	if self != "" {
		if rows, err := s.server.store.WorkersForUser(ctx, s.worker.OwnerUserId); err == nil {
			for _, row := range rows {
				if row.ID != s.worker.RegistrationId {
					continue
				}
				holder := strings.TrimSpace(row.ConnectedNodeId)
				if holder != "" && holder != self {
					if s.server.logger != nil {
						s.server.logger.Info("worker: skip clear connectedNodeId; successor holds stream",
							"registration_id", s.worker.RegistrationId,
							"self_node_id", self,
							"holder_node_id", holder,
						)
					}
					return
				}
				break
			}
		}
	}
	if err := s.server.store.ClearConnectedNode(ctx, s.worker.RegistrationId, s.worker.OwnerUserId); err != nil {
		if s.server.logger != nil {
			s.server.logger.Warn("worker: clear connectedNodeId failed",
				"registration_id", s.worker.RegistrationId,
				"error", err,
			)
		}
	}
}

// dispatch is the hook the registry's dispatcher invokes when an
// agent-side request has been admission-checked.
//
// onChunk, when non-nil, receives every ToolStream the worker emits for this
// call, in arrival order and BEFORE this function returns its result. It runs
// on the stream-recv goroutine, so a slow callback stalls every other message
// on the connection -- hand work off rather than doing it there.
func (s *streamSession) dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch, onChunk func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
	if dispatch == nil {
		return nil, fmt.Errorf("worker: nil dispatch")
	}
	if dispatch.GetCallId() == "" {
		dispatch.CallId = newCallId()
	}
	resCh := make(chan *memqlv1.ToolResult, 1)
	s.mu.Lock()
	if s.pending == nil {
		s.mu.Unlock()
		return nil, ErrWorkerDisconnected
	}
	s.pending[dispatch.GetCallId()] = resCh
	if onChunk != nil && s.chunkSinks != nil {
		s.chunkSinks[dispatch.GetCallId()] = onChunk
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, dispatch.GetCallId())
		delete(s.chunkSinks, dispatch.GetCallId())
		s.mu.Unlock()
	}()

	if err := s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ToolDispatch{ToolDispatch: dispatch},
	}); err != nil {
		return nil, fmt.Errorf("worker dispatch send: %w", err)
	}

	select {
	case res, ok := <-resCh:
		if !ok || res == nil {
			return nil, ErrWorkerDisconnected
		}
		return res, nil
	case <-ctx.Done():
		_ = s.send(&memqlv1.WorkerServerMessage{
			Payload: &memqlv1.WorkerServerMessage_ToolCancel{
				ToolCancel: &memqlv1.ToolCancel{
					CallId: dispatch.GetCallId(),
					Reason: "context_cancelled",
				},
			},
		})
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrWorkerDisconnected
	}
}

// handle processes a single inbound WorkerClientMessage.
func (s *streamSession) handle(ctx context.Context, msg *memqlv1.WorkerClientMessage, sourceIP string) error {
	if msg == nil {
		return nil
	}
	switch payload := msg.GetPayload().(type) {
	case *memqlv1.WorkerClientMessage_Heartbeat:
		s.handleHeartbeat(payload.Heartbeat, sourceIP)
	case *memqlv1.WorkerClientMessage_ToolResult:
		s.handleToolResult(payload.ToolResult)
	case *memqlv1.WorkerClientMessage_ToolStream:
		// Relayed to the per-call sink the dispatch registered, in arrival
		// order and before the call's ToolResult returns. That is the local
		// half of memql#4352: chunks cannot cross a node hop they do not
		// reach locally first, and the cross-node forward reads them from
		// this same callback.
		s.handleToolStream(payload.ToolStream)
	case *memqlv1.WorkerClientMessage_AppSessionChunk:
		s.handleAppSessionChunk(payload.AppSessionChunk)
	case *memqlv1.WorkerClientMessage_AppSessionEnd:
		s.handleAppSessionEnd(payload.AppSessionEnd)
	case *memqlv1.WorkerClientMessage_ModelCallDelta:
		s.handleModelCallDelta(payload.ModelCallDelta)
	case *memqlv1.WorkerClientMessage_ModelCallEnd:
		s.handleModelCallEnd(payload.ModelCallEnd)
	case *memqlv1.WorkerClientMessage_ModelPullProgress:
		s.handleModelPullProgress(payload.ModelPullProgress)
	case *memqlv1.WorkerClientMessage_ModelPullEnd:
		s.handleModelPullEnd(payload.ModelPullEnd)
	case *memqlv1.WorkerClientMessage_ModelProbeProgress:
		s.handleModelProbeProgress(payload.ModelProbeProgress)
	case *memqlv1.WorkerClientMessage_ModelProbeEnd:
		s.handleModelProbeEnd(payload.ModelProbeEnd)
	case *memqlv1.WorkerClientMessage_Pong:
		s.handlePong(payload.Pong)
	case *memqlv1.WorkerClientMessage_RotationRequest:
		s.handleRotationRequest(ctx, payload.RotationRequest)
	case *memqlv1.WorkerClientMessage_AuditEvent:
		s.handleAuditEvent(ctx, payload.AuditEvent)
	default:
		return fmt.Errorf("unknown worker message payload")
	}
	return nil
}

func (s *streamSession) handleHeartbeat(hb *memqlv1.Heartbeat, sourceIP string) {
	if hb == nil {
		return
	}
	at := s.server.clock()
	if hb.GetTs() != nil {
		at = hb.GetTs().AsTime()
	}
	s.worker.TouchLastSeen(at, sourceIP)

	// An app inventory on the beat is applied IMMEDIATELY to the
	// live registry entry (memql#4359): signing into Claude Code
	// makes the machine selectable on the next beat rather than on
	// the next reconnect, and signing out removes it just as fast.
	// The DB flush below is throttled; selection is not.
	//
	// apps_present distinguishes "reporting an empty inventory" from
	// "not reporting apps", which a proto3 repeated field cannot.
	// A beat that says nothing leaves the inventory alone.
	appsChanged := false
	if hb.GetAppsPresent() {
		reported := AppsFromProto(hb.GetApps())
		if !appsEqual(s.worker.Apps(), reported) {
			s.worker.SetApps(reported)
			appsChanged = true
		}
	}

	// The hardware inventory, on the same terms and with one extra distinction
	// (epic memql#5146, D1). hardware_present is apps_present's twin: a beat
	// that says nothing leaves the stored inventory alone rather than clearing
	// it.
	//
	// A MALFORMED inventory on a beat is DROPPED, where the same thing on
	// Register refuses the registration. The asymmetry is deliberate: refusing
	// here would drop a live stream and take the machine offline over a field
	// that decides a recommendation, and the previous good inventory is still
	// on the row. It is logged so the cockpit bug is findable.
	//
	// The change is split in two because the two halves have different costs.
	// A MATERIAL change -- the chip, the memory, the accelerator, the runtime
	// set -- moves the `runtime:` labels and must land on the row now, even
	// inside the throttle window, or the row disagrees with the registry and a
	// planner node reads the stale one. Anything else is free disk moving,
	// which happens on every report and decides nothing, so it rides the
	// throttled write below.
	hardwareMaterial := false
	if hb.GetHardwarePresent() {
		reported, err := InventoryFromProto(hb.GetHardware())
		switch {
		case err != nil:
			if s.server != nil && s.server.logger != nil {
				s.server.logger.Warn("worker: heartbeat carried a malformed hardware inventory; keeping the stored one",
					"registration_id", s.worker.RegistrationId,
					"error", err,
				)
			}
		default:
			current := s.worker.Hardware()
			if !InventoriesEqual(current, reported) {
				hardwareMaterial = MaterialChange(current, reported)
				s.worker.SetHardware(reported)
				s.hardwarePending = true
			}
		}
	}

	// Persist lastSeenAt at most once per HeartbeatBatchInterval
	// (memql#1340). The FIRST heartbeat of a stream always persists
	// (lastPersistedAt zero value), so a (re)connected worker's row is
	// fresh within one beat; a failed flush does NOT advance
	// lastPersistedAt, so the next beat retries. The in-memory registry
	// (touched above) is updated on every beat regardless -- only the DB
	// flush is throttled.
	//
	// WHAT THE THROTTLE BUYS CHANGED IN memql#4350, and the old reasoning
	// here would now mislead. It said a per-beat write bought no freshness
	// anyone read, and set the interval to 60s. Nothing read lastSeenAt
	// because a minute-stale timestamp answers no question worth asking;
	// the Fleet page asks one, deriving `online` from this value against
	// OnlineWindow. So the interval is now the cockpit's own 15s beat and
	// this is, in practice, one write per worker per beat. The throttle
	// still does the job it was built for -- a worker that beats faster
	// than the interval, or a reconnect storm, cannot turn into a write
	// storm -- but it is no longer suppressing the ordinary case.
	if s.server == nil || s.server.store == nil {
		return
	}
	// An inventory CHANGE always persists, throttle or not: the
	// derived app: labels live on the registration row as well as in
	// the registry, and a row that disagrees with the live entry is
	// exactly the split a reader cannot detect.
	// A material inventory change lands NOW, for the reason the app inventory
	// does one branch below: both move routing labels that live on the row as
	// well as in the registry, and a planner node holds no registry at all.
	// This runs before the apps branch so that a beat carrying both writes the
	// inventory rather than returning early on the apps write; the two write
	// different fields through different mutations and neither subsumes the
	// other.
	if hardwareMaterial {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		if err := s.server.store.UpdateHardware(ctx, s.worker.RegistrationId, s.worker.OwnerUserId, s.worker.Hardware().Row(), s.worker.LabelsSnapshot(), at, sourceIP); err != nil {
			if s.server.logger != nil {
				s.server.logger.Warn("worker: persist hardware inventory failed",
					"registration_id", s.worker.RegistrationId,
					"error", err,
				)
			}
			return
		}
		s.lastPersistedAt = at
		s.hardwarePending = false
	}
	if appsChanged {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		if err := s.server.store.UpdateApps(ctx, s.worker.RegistrationId, s.worker.OwnerUserId, s.worker.Apps(), s.worker.LabelsSnapshot(), at, sourceIP); err != nil {
			if s.server.logger != nil {
				s.server.logger.Warn("worker: persist app inventory failed",
					"registration_id", s.worker.RegistrationId,
					"error", err,
				)
			}
			return
		}
		s.lastPersistedAt = at
		return
	}
	if !s.lastPersistedAt.IsZero() && at.Sub(s.lastPersistedAt) < HeartbeatBatchInterval {
		return
	}
	// activeCount is the WORKER's own report (Heartbeat.active_calls_total),
	// which is what v1:worker:registration.activeCount documents it to be. A
	// cockpit build predating that field sends 0, so fall back to this
	// replica's registry sum -- the dispatches it has admitted and not yet
	// released. Both are best-effort and up to one interval stale: the field
	// is a routing input for leastLoaded, never a correctness one, and
	// Worker.Acquire remains the real valve.
	active := int(hb.GetActiveCallsTotal())
	if active == 0 {
		active = s.worker.ActiveCount()
	}
	// A non-material inventory refresh rides this write rather than buying one
	// of its own. It is exactly as fresh as the beat that carried it, and free
	// disk moving on every report is not worth a second write to the same row.
	// NIL when nothing changed, which the store reads as "leave it alone".
	var hardware map[string]any
	if s.hardwarePending {
		hardware = s.worker.Hardware().Row()
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	// The latest Ping round trip rides this write too (epic memql#5218, D11).
	// Re-asserted on every flush rather than sent once: it is one field on a
	// write that already happens, and the row then always carries the latest
	// figure this replica holds. A zero rttAt is "not measured" and the store
	// leaves both fields out.
	if err := s.server.store.UpdateLastSeen(ctx, s.worker.RegistrationId, s.worker.OwnerUserId, at, sourceIP, s.server.nodeId, active, hardware, s.rttMs, s.rttAt); err != nil {
		if s.server.logger != nil {
			s.server.logger.Warn("worker: persist heartbeat failed",
				"registration_id", s.worker.RegistrationId,
				"error", err,
			)
		}
		return
	}
	s.lastPersistedAt = at
}

func (s *streamSession) handleToolResult(res *memqlv1.ToolResult) {
	if res == nil || res.GetCallId() == "" {
		return
	}
	s.mu.Lock()
	ch, ok := s.pending[res.GetCallId()]
	if ok {
		delete(s.pending, res.GetCallId())
		// The result is the end of the call's output. Retiring the sink here
		// rather than waiting for dispatch's defer means a chunk the worker
		// sends after its own result cannot reach a caller that has already
		// been handed one.
		delete(s.chunkSinks, res.GetCallId())
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- res:
	default:
		s.server.logger.Warn("dropping tool result -- caller already returned",
			"call_id", res.GetCallId(),
		)
	}
}

// handleToolStream relays one output chunk to the sink its call registered.
//
// A chunk with no sink is DROPPED, not an error, and there are two ordinary
// ways to get one: the caller asked for no chunks (most calls), or the chunk
// arrived after its ToolResult, which the worker is free to do. Both are debug
// lines, the same treatment handleToolResult gives a result whose caller has
// already returned.
func (s *streamSession) handleToolStream(chunk *memqlv1.ToolStream) {
	if chunk == nil || chunk.GetCallId() == "" {
		return
	}
	s.mu.Lock()
	sink := s.chunkSinks[chunk.GetCallId()]
	s.mu.Unlock()
	if sink == nil {
		if s.server != nil && s.server.logger != nil {
			s.server.logger.Debug("dropping tool stream chunk -- no sink for call",
				"call_id", chunk.GetCallId(),
			)
		}
		return
	}
	sink(chunk)
}

func (s *streamSession) handleRotationRequest(ctx context.Context, req *memqlv1.RotationRequest) {
	// MVP responds with an empty rotation -- token rotation lands in
	// Phase 7 alongside the rest of the hardening track. Until then
	// the call is acknowledged so workers don't loop on it.
	_ = req
	_ = s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_RotationResponse{
			RotationResponse: &memqlv1.RotationResponse{},
		},
	})
}

func (s *streamSession) handleAuditEvent(ctx context.Context, evt *memqlv1.AuditEvent) {
	if evt == nil || s.server.auditor == nil {
		return
	}
	s.server.auditor.Emit(ctx, AuditEvent{
		Action:      evt.GetAction(),
		Actor:       "worker:" + s.worker.RegistrationId,
		Target:      s.worker.RegistrationId,
		TargetType:  "worker",
		OwnerUserId: s.worker.OwnerUserId,
		Detail:      map[string]any{"raw": string(evt.GetDetailJson())},
		Timestamp:   s.server.clock(),
	})
}

func (s *streamSession) send(msg *memqlv1.WorkerServerMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	if err := s.stream.Send(msg); err != nil {
		s.sendErr = err
		return err
	}
	return nil
}

// -----------------------------------------------------------------------------
// Ping / Pong (epic memql#5218, D11)
//
// A heartbeat is the MACHINE's word that it is there. The Ping is the
// cluster's own evidence that the return path -- this replica to the cockpit
// -- works, and how fast. The agent sends one FirstPingDelay after RegisterAck
// and then every PingInterval; the cockpit echoes it as a Pong; the round trip
// lands on the Worker and, on the next heartbeat flush, on the row as
// rttMs / rttAt. A cockpit that predates the message never answers, and the
// row then carries no figure, which every reader takes as "not measured".
// -----------------------------------------------------------------------------

// startPinger runs the pinger for the life of the session. Bound to s.ctx, so
// close() ends it with everything else on the stream.
func (s *streamSession) startPinger() {
	go s.runPinger()
}

// runPinger sends the first Ping after FirstPingDelay and then one every
// PingInterval until the session context ends. It is its own goroutine
// because the recv loop is the only other one on the stream and it blocks in
// Recv: a cockpit that goes quiet would otherwise never be pinged, which is
// the exact case the Ping exists to notice.
func (s *streamSession) runPinger() {
	if s == nil || s.server == nil {
		return
	}
	timer := time.NewTimer(s.server.pingFirst)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}
		s.sendPing()
		timer.Reset(s.server.pingEvery)
	}
}

// sendPing records a fresh outstanding Ping and puts it on the stream.
//
// The id and the send time are recorded BEFORE the send, under pingMu, so a
// Pong that comes back faster than this goroutine can return finds the id it
// is answering. The send time is the agent's own clock and is what the round
// trip is measured against: the cockpit echoes it, but the echo is never read
// for the figure, so no clock on the machine can shape the number.
//
// A nil stream is tolerated rather than refused: the heartbeat tests build a
// session with none, because the flush path never touches it, and the pinger
// must not be the reason that harness cannot start a session.
func (s *streamSession) sendPing() {
	if s == nil || s.server == nil {
		return
	}
	now := s.server.clock()
	id := randomHex(8)
	s.pingMu.Lock()
	s.outstandingPing = id
	s.outstandingPingS = now
	s.pingMu.Unlock()
	if s.stream == nil {
		return
	}
	if err := s.send(&memqlv1.WorkerServerMessage{
		MessageId: id,
		Payload: &memqlv1.WorkerServerMessage_Ping{
			Ping: &memqlv1.Ping{
				RequestId: id,
				SentAt:    timestamppb.New(now),
			},
		},
	}); err != nil && s.server.logger != nil {
		// Debug rather than Warn: a stream whose send fails is a stream about
		// to close, and Stream's recv loop reports that once, by name.
		s.server.logger.Debug("worker: ping send failed",
			"registration_id", s.worker.RegistrationId,
			"error", err,
		)
	}
}

// handlePong records the round trip for the ONE outstanding Ping. A Pong for
// any other id -- late for a superseded Ping, duplicated, or invented -- is
// dropped at debug, because a figure the agent did not ask for is not a
// measurement. The round trip is measured from the recorded send time on this
// replica's clock, never from anything the machine sent, and clamped at zero:
// a clock that steps backwards mid-flight is a fact about the clock, not a
// negative latency.
func (s *streamSession) handlePong(pong *memqlv1.Pong) {
	if pong == nil || pong.GetRequestId() == "" {
		return
	}
	s.pingMu.Lock()
	matched := s.outstandingPing != "" && pong.GetRequestId() == s.outstandingPing
	sentAt := s.outstandingPingS
	if matched {
		s.outstandingPing = ""
		s.outstandingPingS = time.Time{}
	}
	s.pingMu.Unlock()
	if !matched {
		if s.server != nil && s.server.logger != nil {
			s.server.logger.Debug("worker: dropping pong -- not the outstanding ping",
				"registration_id", s.worker.RegistrationId,
				"request_id", pong.GetRequestId(),
			)
		}
		return
	}
	now := s.server.clock()
	rtt := now.Sub(sentAt)
	if rtt < 0 {
		rtt = 0
	}
	rttMs := int(rtt / time.Millisecond)
	s.worker.RecordRoundTrip(rttMs, now)
	s.rttMs = rttMs
	s.rttAt = now
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// validateRegister checks the Register payload and decodes the
// optional structured capability descriptor. HEADLESS is mandatory;
// COMPUTERUSE and MODEL are optional capabilities. Inference-enabled Cockpit
// workers advertise MODEL, so refusing it rejects the entire registration,
// including that machine's other capabilities. The descriptor is metadata.
func validateRegister(r *memqlv1.Register) (*CapabilityDescriptor, error) {
	caps := r.GetCapabilities()
	if len(caps) == 0 {
		return nil, fmt.Errorf("register: at least one capability required")
	}
	hasHeadless := false
	for _, c := range caps {
		if c == CapabilityHeadless {
			hasHeadless = true
		}
		if c != CapabilityHeadless && c != CapabilityComputerUse && c != ModelCapability {
			return nil, fmt.Errorf("register: unknown capability %q", c)
		}
	}
	if !hasHeadless {
		return nil, fmt.Errorf("register: HEADLESS capability is mandatory")
	}
	descriptor, err := ParseCapabilityDescriptor(r.GetCapabilityDescriptorJson())
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	// A MALFORMED INVENTORY REFUSES THE REGISTRATION rather than being dropped
	// (epic memql#5146, D1). The inventory decides the machine's class, which
	// decides what the machine is told to pull, so one silently discarded
	// leaves a machine unclassifiable forever with nothing anywhere to read --
	// and an unclassifiable machine looks exactly like one whose cockpit
	// predates the field, which is a state nobody investigates. An ABSENT
	// inventory is not malformed and registers exactly as before.
	if _, err := InventoryFromProto(r.GetHardware()); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	return descriptor, nil
}

func normalizeCapabilities(caps []string) []string {
	out := make([]string, 0, len(caps))
	seen := map[string]struct{}{}
	for _, c := range caps {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func platformInfoToMap(p *memqlv1.PlatformInfo) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"os":       p.GetOs(),
		"arch":     p.GetArch(),
		"hostname": p.GetHostname(),
	}
}

func permissionStatusToMap(p *memqlv1.PermissionStatus) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"accessibility":    p.GetAccessibility(),
		"screen_recording": p.GetScreenRecording(),
		"x11_display":      p.GetX11Display(),
		"detail":           p.GetDetail(),
	}
}

func platformHostname(p *memqlv1.PlatformInfo) string {
	if p == nil {
		return ""
	}
	return p.GetHostname()
}

func stringFallback(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func newRegistrationId() string {
	return randomHex(12)
}

func newCallId() string {
	return randomHex(12)
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func peerAddrFromContext(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.Addr == nil {
		return ""
	}
	return p.Addr.String()
}

// -----------------------------------------------------------------------------
// App sessions (memql#4359)
// -----------------------------------------------------------------------------

// openAppSession is the per-stream hook behind Worker.StartAppSession.
// It registers the session BEFORE sending Start, so a worker that
// answers instantly cannot deliver a chunk for a session this side has
// not yet recorded.
func (s *streamSession) openAppSession(ctx context.Context, req AppSessionRequest) (*AppSessionHandle, error) {
	if req.SessionId == "" {
		return nil, fmt.Errorf("worker: app session requires a session id")
	}
	handle := &AppSessionHandle{
		sessionId: req.SessionId,
		app:       req.App,
		worker:    s.worker,
		chunks:    make(chan AppSessionChunk, appSessionChunkBuffer),
		done:      make(chan struct{}),
		control:   s.sendAppSessionControl,
	}
	handle.detach = func() {
		s.mu.Lock()
		if s.sessions != nil {
			delete(s.sessions, req.SessionId)
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	if s.sessions == nil {
		s.mu.Unlock()
		return nil, ErrWorkerDisconnected
	}
	if _, exists := s.sessions[req.SessionId]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("worker: app session %s already open", req.SessionId)
	}
	s.sessions[req.SessionId] = handle
	s.mu.Unlock()

	start := &memqlv1.AppSessionStart{
		SessionId:     req.SessionId,
		App:           req.App,
		Kind:          req.Kind,
		Prompt:        req.Prompt,
		Inputs:        req.Inputs,
		Workspace:     req.Workspace,
		Credential:    req.Credential,
		McpEndpoint:   req.MCPEndpoint,
		Limits:        req.Limits.toProto(),
		RunId:         req.RunId,
		StepId:        req.StepId,
		AppSessionRef: req.AppSessionRef,
		// Empty when nothing structured was asked for, which the far side
		// reads as "free text" rather than as an empty schema.
		ResponseSchemaJson: req.ResponseSchema,
	}
	if err := s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_AppSessionStart{AppSessionStart: start},
	}); err != nil {
		handle.finish(AppSessionOutcome{Error: "start_send_failed"}, err)
		return nil, fmt.Errorf("worker: send app session start: %w", err)
	}

	// A caller context that dies before the session ends cancels the
	// run on the machine. Without this a headless agent keeps working
	// on somebody's laptop after the plan that asked for it is gone.
	go func() {
		select {
		case <-ctx.Done():
			_ = handle.Cancel("caller_context_done")
		case <-handle.done:
		case <-s.ctx.Done():
		}
	}()

	return handle, nil
}

// appSessionChunkBuffer is how many chunks the handle buffers before
// a slow consumer backpressures the stream-recv goroutine.
const appSessionChunkBuffer = 64

func (s *streamSession) sendAppSessionControl(control *memqlv1.AppSessionControl) error {
	if control == nil {
		return nil
	}
	return s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_AppSessionControl{AppSessionControl: control},
	})
}

func (s *streamSession) lookupAppSession(sessionId string) *AppSessionHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		return nil
	}
	return s.sessions[sessionId]
}

func (s *streamSession) handleAppSessionChunk(chunk *memqlv1.AppSessionChunk) {
	if chunk == nil || chunk.GetSessionId() == "" {
		return
	}
	handle := s.lookupAppSession(chunk.GetSessionId())
	if handle == nil {
		// A chunk for a session this node is not hosting. Logged, not
		// fatal: a worker that reconnected to a different replica
		// mid-run will do exactly this, and dropping it is correct.
		if s.server != nil && s.server.logger != nil {
			s.server.logger.Debug("worker: app session chunk for unknown session",
				"session_id", chunk.GetSessionId(),
				"registration_id", s.worker.RegistrationId,
			)
		}
		return
	}
	handle.deliverChunk(AppSessionChunk{
		Stream: chunk.GetStream(),
		Data:   chunk.GetData(),
		Seq:    chunk.GetSeq(),
	})
}

func (s *streamSession) handleAppSessionEnd(end *memqlv1.AppSessionEnd) {
	if end == nil || end.GetSessionId() == "" {
		return
	}
	handle := s.lookupAppSession(end.GetSessionId())
	if handle == nil {
		return
	}
	usage := AppSessionUsage{}
	if u := end.GetUsage(); u != nil {
		usage = AppSessionUsage{
			InputTokens:  u.GetInputTokens(),
			OutputTokens: u.GetOutputTokens(),
			CostUSD:      u.GetCostUsd(),
			Known:        u.GetKnown(),
		}
	}
	outcome := AppSessionOutcome{
		ExitCode:            end.GetExitCode(),
		Usage:               usage,
		AppSessionRef:       end.GetAppSessionRef(),
		ProducedArtifactIds: end.GetProducedArtifactIds(),
		Error:               end.GetError(),
	}
	// Carried whatever the exit code says. A harness can answer the schema
	// and still exit non-zero, and dropping the answer because the run
	// failed would lose the only part of it we can read.
	if raw := end.GetResultJson(); raw != "" {
		outcome.Result = []byte(raw)
	}
	var err error
	if outcome.Error != "" {
		err = fmt.Errorf("worker: app session failed: %s", outcome.Error)
	}
	handle.finish(outcome, err)
}

// -----------------------------------------------------------------------------
// Model calls (epic memql#4676, task memql#4677)
// -----------------------------------------------------------------------------

// openModelCall is the per-stream hook behind Worker.StartModelCall.
// It registers the call BEFORE sending Start, so a worker that answers
// instantly cannot deliver a delta for a call this side has not yet
// recorded -- the same ordering openAppSession keeps and for the same
// reason.
func (s *streamSession) openModelCall(ctx context.Context, req ModelCallRequest) (*ModelCallHandle, error) {
	if req.RequestId == "" {
		return nil, fmt.Errorf("worker: model call requires a request id")
	}
	limits := req.Limits.withDefaults()
	clock := time.Now
	if s.server != nil && s.server.clock != nil {
		clock = s.server.clock
	}
	handle := &ModelCallHandle{
		requestId:    req.RequestId,
		worker:       s.worker,
		limits:       limits,
		deltas:       make(chan ModelCallDelta, modelDeltaBuffer),
		done:         make(chan struct{}),
		cancelFn:     s.sendModelCallCancel,
		clock:        clock,
		lastActivity: clock(),
	}
	handle.detach = func() {
		s.mu.Lock()
		if s.modelCalls != nil {
			delete(s.modelCalls, req.RequestId)
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	if s.modelCalls == nil {
		s.mu.Unlock()
		return nil, ErrWorkerDisconnected
	}
	if _, exists := s.modelCalls[req.RequestId]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("worker: model call %s already open", req.RequestId)
	}
	s.modelCalls[req.RequestId] = handle
	s.mu.Unlock()

	msgs := make([]*memqlv1.ModelCallMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, &memqlv1.ModelCallMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallId: m.ToolCallId,
			Name:       m.Name,
			ToolCalls:  toolCallsToProto(m.ToolCalls),
		})
	}
	tools := make([]*memqlv1.ModelCallTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, &memqlv1.ModelCallTool{
			Name:           t.Name,
			Description:    t.Description,
			ParametersJson: t.ParametersJSON,
		})
	}
	start := &memqlv1.ModelCallStart{
		RequestId:            req.RequestId,
		Model:                req.Model,
		Kind:                 req.Kind,
		Messages:             msgs,
		ResponseFormatSchema: req.ResponseFormatSchema,
		EmbeddingInput:       req.EmbeddingInput,
		Limits:               limits.toProto(),
		RunId:                req.RunId,
		StepId:               req.StepId,
		Purpose:              req.Purpose,
		Tools:                tools,
		Params: &memqlv1.ModelCallParams{
			Temperature:     req.Params.Temperature,
			TemperatureSet:  req.Params.TemperatureSet,
			TopP:            req.Params.TopP,
			TopPSet:         req.Params.TopPSet,
			MaxOutputTokens: req.Params.MaxOutputTokens,
			ContextTokens:   req.Params.ContextTokens,
			Stop:            req.Params.Stop,
			Seed:            req.Params.Seed,
			SeedSet:         req.Params.SeedSet,
		},
	}
	if err := s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelCallStart{ModelCallStart: start},
	}); err != nil {
		handle.finish(ModelCallOutcome{
			FinishReason: ModelFinishError,
			Error:        "start_send_failed",
			ErrorCode:    "start_send_failed",
		}, err)
		return nil, fmt.Errorf("worker: send model call start: %w", err)
	}

	// A caller context that dies before the call ends cancels the
	// generation on the machine.
	go func() {
		select {
		case <-ctx.Done():
			_ = handle.Cancel("caller_context_done")
		case <-handle.done:
		case <-s.ctx.Done():
		}
	}()

	return handle, nil
}

func (s *streamSession) sendModelCallCancel(cancel *memqlv1.ModelCallCancel) error {
	if cancel == nil {
		return nil
	}
	return s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelCallCancel{ModelCallCancel: cancel},
	})
}

func (s *streamSession) lookupModelCall(requestId string) *ModelCallHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelCalls == nil {
		return nil
	}
	return s.modelCalls[requestId]
}

func (s *streamSession) handleModelCallDelta(delta *memqlv1.ModelCallDelta) {
	if delta == nil || delta.GetRequestId() == "" {
		return
	}
	handle := s.lookupModelCall(delta.GetRequestId())
	if handle == nil {
		// A delta for a call this node is not hosting. Logged, not
		// fatal: a worker that reconnected to a different replica
		// mid-generation will do exactly this, and dropping it is right.
		if s.server != nil && s.server.logger != nil {
			s.server.logger.Debug("worker: model call delta for unknown request",
				"request_id", delta.GetRequestId(),
				"registration_id", s.worker.RegistrationId,
			)
		}
		return
	}
	handle.deliverDelta(ModelCallDelta{
		Seq:       delta.GetSeq(),
		Content:   delta.GetContent(),
		Keepalive: delta.GetKeepalive(),
		ToolCalls: toolCallsFromProto(delta.GetToolCalls()),
	})
}

func (s *streamSession) handleModelCallEnd(end *memqlv1.ModelCallEnd) {
	if end == nil || end.GetRequestId() == "" {
		return
	}
	handle := s.lookupModelCall(end.GetRequestId())
	if handle == nil {
		return
	}
	usage := ModelCallUsage{}
	if u := end.GetUsage(); u != nil {
		usage = ModelCallUsage{
			InputTokens:  u.GetInputTokens(),
			OutputTokens: u.GetOutputTokens(),
			Known:        u.GetKnown(),
			Model:        u.GetModel(),
		}
	}
	embeddings := make([][]float32, 0, len(end.GetEmbeddings()))
	for _, e := range end.GetEmbeddings() {
		embeddings = append(embeddings, e.GetValues())
	}
	outcome := ModelCallOutcome{
		FinishReason: end.GetFinishReason(),
		Usage:        usage,
		Content:      end.GetContent(),
		Error:        end.GetError(),
		ErrorCode:    end.GetErrorCode(),
		ToolCalls:    toolCallsFromProto(end.GetToolCalls()),
	}
	if len(embeddings) > 0 {
		outcome.Embeddings = embeddings
	}
	var err error
	if outcome.Error != "" {
		err = fmt.Errorf("worker: model call failed: %s", outcome.Error)
	}
	handle.finish(outcome, err)
}

// toolCallsToProto / toolCallsFromProto are the one mapping between the wire
// tool call and its Go twin. Kept as a pair beside the handlers rather than as
// methods, because the direction matters at each call site and a method named
// on one side reads as if it worked on both.
func toolCallsToProto(in []ModelCallToolCall) []*memqlv1.ModelCallToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]*memqlv1.ModelCallToolCall, 0, len(in))
	for _, c := range in {
		out = append(out, &memqlv1.ModelCallToolCall{
			Id:            c.Id,
			Name:          c.Name,
			ArgumentsJson: c.ArgumentsJSON,
			Index:         c.Index,
		})
	}
	return out
}

func toolCallsFromProto(in []*memqlv1.ModelCallToolCall) []ModelCallToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]ModelCallToolCall, 0, len(in))
	for _, c := range in {
		if c == nil {
			continue
		}
		out = append(out, ModelCallToolCall{
			Id:            c.GetId(),
			Name:          c.GetName(),
			ArgumentsJSON: c.GetArgumentsJson(),
			Index:         c.GetIndex(),
		})
	}
	return out
}

// -----------------------------------------------------------------------------
// Model pull (epic memql#5103)
// -----------------------------------------------------------------------------

// openModelPull is the per-stream hook behind Worker.StartModelPull. It
// registers the pull BEFORE sending Start, so a machine that answers instantly
// cannot deliver an observation for a pull this side has not yet recorded --
// the ordering openModelCall and openAppSession both keep, for the same
// reason.
func (s *streamSession) openModelPull(ctx context.Context, req ModelPullRequest) (*ModelPullHandle, error) {
	if req.RequestId == "" {
		return nil, fmt.Errorf("worker: model pull requires a request id")
	}
	limits := req.Limits.withDefaults()
	clock := time.Now
	if s.server != nil && s.server.clock != nil {
		clock = s.server.clock
	}
	handle := &ModelPullHandle{
		requestId:    req.RequestId,
		model:        req.Model,
		limits:       limits,
		progress:     make(chan ModelPullProgress, modelPullProgressBuffer),
		done:         make(chan struct{}),
		cancelFn:     s.sendModelPullCancel,
		clock:        clock,
		lastActivity: clock(),
	}
	handle.detach = func() {
		s.mu.Lock()
		if s.modelPulls != nil {
			delete(s.modelPulls, req.RequestId)
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	if s.modelPulls == nil {
		s.mu.Unlock()
		return nil, ErrWorkerDisconnected
	}
	if _, exists := s.modelPulls[req.RequestId]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("worker: model pull %s already open", req.RequestId)
	}
	s.modelPulls[req.RequestId] = handle
	s.mu.Unlock()

	start := &memqlv1.ModelPullStart{
		RequestId:      req.RequestId,
		RegistrationId: s.worker.RegistrationId,
		Model:          req.Model,
	}
	if err := s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelPullStart{ModelPullStart: start},
	}); err != nil {
		handle.finish(ModelPullOutcome{Error: "start_send_failed"}, err)
		return nil, fmt.Errorf("worker: send model pull start: %w", err)
	}

	// A caller context that dies before the pull ends stops the download on
	// the machine.
	go func() {
		select {
		case <-ctx.Done():
			_ = handle.Cancel("caller_context_done")
		case <-handle.done:
		case <-s.ctx.Done():
		}
	}()

	return handle, nil
}

// openModelProbe is the per-stream hook behind Worker.StartModelProbe.
//
// It registers the probe BEFORE sending Start, the ordering openModelPull,
// openModelCall and openAppSession all keep: a machine that answers instantly
// must not be able to deliver an observation for a probe this side has not yet
// recorded.
func (s *streamSession) openModelProbe(ctx context.Context, req ModelProbeRequest) (*ModelProbeHandle, error) {
	if req.RequestId == "" {
		return nil, fmt.Errorf("worker: model probe requires a request id")
	}
	clock := time.Now
	if s.server != nil && s.server.clock != nil {
		clock = s.server.clock
	}
	detach := func() {
		s.mu.Lock()
		if s.modelProbes != nil {
			delete(s.modelProbes, req.RequestId)
		}
		s.mu.Unlock()
	}
	handle := NewModelProbeHandle(req.RequestId, req.Model, req.Limits, s.sendModelProbeCancel, detach, clock)

	s.mu.Lock()
	if s.modelProbes == nil {
		s.mu.Unlock()
		return nil, ErrWorkerDisconnected
	}
	if _, exists := s.modelProbes[req.RequestId]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("worker: model probe %s already open", req.RequestId)
	}
	s.modelProbes[req.RequestId] = handle
	s.mu.Unlock()

	start := &memqlv1.ModelProbeStart{
		RequestId:      req.RequestId,
		RegistrationId: s.worker.RegistrationId,
		Model:          req.Model,
		SuiteVersion:   req.SuiteVersion,
	}
	if err := s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelProbeStart{ModelProbeStart: start},
	}); err != nil {
		handle.Finish(ModelProbeOutcome{Error: "start_send_failed"}, err)
		return nil, fmt.Errorf("worker: send model probe start: %w", err)
	}

	// A caller context that dies before the probe ends stops the suite on the
	// machine. It is somebody's GPU.
	go func() {
		select {
		case <-ctx.Done():
			_ = handle.Cancel("caller_context_done")
		case <-handle.done:
		case <-s.ctx.Done():
		}
	}()

	return handle, nil
}

func (s *streamSession) sendModelProbeCancel(cancel *memqlv1.ModelProbeCancel) error {
	if cancel == nil {
		return nil
	}
	return s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelProbeCancel{ModelProbeCancel: cancel},
	})
}

func (s *streamSession) lookupModelProbe(requestId string) *ModelProbeHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelProbes == nil {
		return nil
	}
	return s.modelProbes[requestId]
}

func (s *streamSession) handleModelProbeProgress(p *memqlv1.ModelProbeProgress) {
	if p == nil || p.GetRequestId() == "" {
		return
	}
	handle := s.lookupModelProbe(p.GetRequestId())
	if handle == nil {
		return
	}
	handle.DeliverProgress(ModelProbeProgress{
		CaseId:    p.GetCaseId(),
		Completed: p.GetCompletedCases(),
		Total:     p.GetTotalCases(),
		Ok:        p.GetCaseOk(),
		Error:     p.GetCaseError(),
	})
}

func (s *streamSession) handleModelProbeEnd(end *memqlv1.ModelProbeEnd) {
	if end == nil || end.GetRequestId() == "" {
		return
	}
	handle := s.lookupModelProbe(end.GetRequestId())
	if handle == nil {
		return
	}
	// A FAILED PROBE IS NOT A TRANSPORT ERROR, the rule handleModelPullEnd
	// states one function along: `ok=false` with text is an ANSWER, and Wait
	// returns it with a nil error so the caller records the machine's own
	// words. Returning an error here would make "the runtime crashed on case
	// three" indistinguishable from the machine falling off the network -- the
	// one thing the two ends of this protocol exist to separate, and here the
	// difference decides whether a figure is `failed` or `unmeasured`.
	handle.Finish(ModelProbeOutcome{
		Model:        end.GetModel(),
		Ok:           end.GetOk(),
		Error:        end.GetError(),
		SuiteVersion: end.GetSuiteVersion(),
		Figures:      FiguresFromProto(end),
	}, nil)
}

func (s *streamSession) sendModelPullCancel(cancel *memqlv1.ModelPullCancel) error {
	if cancel == nil {
		return nil
	}
	return s.send(&memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelPullCancel{ModelPullCancel: cancel},
	})
}

func (s *streamSession) lookupModelPull(requestId string) *ModelPullHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelPulls == nil {
		return nil
	}
	return s.modelPulls[requestId]
}

func (s *streamSession) handleModelPullProgress(p *memqlv1.ModelPullProgress) {
	if p == nil || p.GetRequestId() == "" {
		return
	}
	handle := s.lookupModelPull(p.GetRequestId())
	if handle == nil {
		// An observation for a pull this node is not hosting -- a machine
		// that reconnected to a different replica mid-download does exactly
		// this. Dropping it is right, and it is not an error.
		if s.server != nil && s.server.logger != nil {
			s.server.logger.Debug("worker: model pull progress for unknown request",
				"request_id", p.GetRequestId(),
				"registration_id", s.worker.RegistrationId,
			)
		}
		return
	}
	handle.deliverProgress(ModelPullProgress{
		CompletedBytes: p.GetCompletedBytes(),
		TotalBytes:     p.GetTotalBytes(),
		Status:         p.GetStatus(),
		Layer:          p.GetLayer(),
	})
}

func (s *streamSession) handleModelPullEnd(end *memqlv1.ModelPullEnd) {
	if end == nil || end.GetRequestId() == "" {
		return
	}
	handle := s.lookupModelPull(end.GetRequestId())
	if handle == nil {
		return
	}
	outcome := ModelPullOutcome{
		Model:        end.GetModel(),
		Ok:           end.GetOk(),
		Error:        end.GetError(),
		Readvertised: end.GetReadvertised(),
	}
	// A FAILED PULL IS NOT A TRANSPORT ERROR. The runtime reports its own
	// failures in the body of a successful stream, so `ok=false` with text is
	// an ANSWER: Wait returns it with a nil error and the caller renders the
	// machine's own words. Returning an error here would make an ordinary
	// "no space left on device" indistinguishable from the machine falling
	// off the network, which is the one thing the two ends of this protocol
	// exist to separate.
	handle.finish(outcome, nil)
}
