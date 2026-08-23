package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// CapabilityHeadless and CapabilityComputerUse are the two predefined
// capability names. HEADLESS is mandatory; COMPUTERUSE is optional.
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
}

func newServer(logger *slog.Logger, store Store, registry *Registry, auditor Auditor, clock func() time.Time, nodeId string) *server {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &server{
		logger:   logger,
		store:    store,
		registry: registry,
		auditor:  auditor,
		clock:    clock,
		nodeId:   nodeId,
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
	defer session.close()

	for {
		msg, err := stream.Recv()
		if err != nil {
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

	streamCtx, cancel := context.WithCancel(stream.Context())
	session := newStreamSession(s, stream, w, streamCtx, cancel)
	w.SetDispatchFunc(session.dispatch, cancel)
	w.SetAppSessionFunc(session.openAppSession)
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

	apps := AppsFromProto(register.GetApps())
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
		Labels:              mergeAppLabels(copyStringMap(register.GetLabels()), apps),
		Apps:                apps,
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
	sessions  map[string]*AppSessionHandle
	sendMu    sync.Mutex
	sendErr   error
	closeOnce sync.Once

	// lastPersistedAt is the heartbeat timestamp of the most recent
	// successful lastSeenAt DB flush (memql#1340). Zero until the
	// first heartbeat of the stream persists. Only touched from
	// handleHeartbeat, which runs on the single stream-recv
	// goroutine, so it needs no lock.
	lastPersistedAt time.Time
}

func newStreamSession(
	srv *server,
	stream memqlv1.WorkerService_StreamServer,
	w *Worker,
	ctx context.Context,
	cancel context.CancelFunc,
) *streamSession {
	return &streamSession{
		server:     srv,
		stream:     stream,
		worker:     w,
		ctx:        ctx,
		cancel:     cancel,
		pending:    make(map[string]chan *memqlv1.ToolResult),
		chunkSinks: make(map[string]func(*memqlv1.ToolStream)),
		sessions:   make(map[string]*AppSessionHandle),
	}
}

func (s *streamSession) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.server != nil && s.server.registry != nil {
			s.server.registry.Remove(s.worker.RegistrationId)
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
		s.mu.Unlock()
		for _, h := range liveSessions {
			h.finish(AppSessionOutcome{Error: "worker_disconnected"}, ErrWorkerDisconnected)
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
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.server.store.UpdateLastSeen(ctx, s.worker.RegistrationId, s.worker.OwnerUserId, at, sourceIP, s.server.nodeId, active); err != nil {
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
// helpers
// -----------------------------------------------------------------------------

// validateRegister checks the Register payload and decodes the
// optional structured capability descriptor. The HEADLESS/COMPUTERUSE
// capability-string contract is unchanged: HEADLESS is mandatory,
// COMPUTERUSE is the only other admitted string, and scope checks keep
// keying off those two names. The descriptor is additive metadata.
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
		if c != CapabilityHeadless && c != CapabilityComputerUse {
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
		PlanId:        req.PlanId,
		TaskId:        req.TaskId,
		AppSessionRef: req.AppSessionRef,
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
	var err error
	if outcome.Error != "" {
		err = fmt.Errorf("worker: app session failed: %s", outcome.Error)
	}
	handle.finish(outcome, err)
}
