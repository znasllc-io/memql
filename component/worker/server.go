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

// CapabilityHeadless and CapabilityGUI are the two predefined
// capability names. HEADLESS is mandatory; GUI is optional.
const (
	CapabilityHeadless = "HEADLESS"
	CapabilityGUI      = "GUI"
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
}

func newServer(logger *slog.Logger, store Store, registry *Registry, auditor Auditor, clock func() time.Time) *server {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &server{
		logger:   logger,
		store:    store,
		registry: registry,
		auditor:  auditor,
		clock:    clock,
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

	streamCtx, cancel := context.WithCancel(stream.Context())
	session := newStreamSession(s, stream, w, streamCtx, cancel)
	w.SetDispatchFunc(session.dispatch, cancel)
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
func (s *server) upsertRegistration(
	ctx context.Context,
	identity *WorkerIdentity,
	register *memqlv1.Register,
	descriptor *CapabilityDescriptor,
	now time.Time,
	sourceIP string,
) (RegistrationRow, error) {
	existing, err := s.store.WorkerByIdentityId(ctx, identity.IdentityId)
	if err != nil {
		return RegistrationRow{}, fmt.Errorf("worker lookup: %w", err)
	}

	registration := RegistrationRow{
		IdentityId:           identity.IdentityId,
		OwnerUserId:          identity.OwnerUserId,
		Name:                 stringFallback(register.GetName(), platformHostname(register.GetPlatform())),
		Capabilities:         normalizeCapabilities(register.GetCapabilities()),
		CapabilityDescriptor: descriptor,
		Labels:               copyStringMap(register.GetLabels()),
		Concurrency:          register.GetConcurrency(),
		Platform:             platformInfoToMap(register.GetPlatform()),
		Permissions:          permissionStatusToMap(register.GetPermissions()),
		Version:              register.GetVersion(),
		BuildTag:             register.GetBuildTag(),
		LastSeenAt:           now,
		LastConnectedFromIP:  sourceIP,
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
	if err := s.store.UpdateLastSeen(ctx, existing.ID, now, sourceIP); err != nil {
		return RegistrationRow{}, fmt.Errorf("worker update lastSeen: %w", err)
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

	mu        sync.Mutex
	pending   map[string]chan *memqlv1.ToolResult
	sendMu    sync.Mutex
	sendErr   error
	closeOnce sync.Once
}

func newStreamSession(
	srv *server,
	stream memqlv1.WorkerService_StreamServer,
	w *Worker,
	ctx context.Context,
	cancel context.CancelFunc,
) *streamSession {
	return &streamSession{
		server:  srv,
		stream:  stream,
		worker:  w,
		ctx:     ctx,
		cancel:  cancel,
		pending: make(map[string]chan *memqlv1.ToolResult),
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
		s.mu.Unlock()
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
				},
				Timestamp: time.Now().UTC(),
			})
		}
	})
}

// dispatch is the hook the registry's dispatcher invokes when an
// agent-side request has been admission-checked.
func (s *streamSession) dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.ToolResult, error) {
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
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, dispatch.GetCallId())
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
		// Streaming output is currently bridged at the agent dispatch
		// boundary; we accept the message but do not relay it further
		// in MVP. Phase 7 wires a streaming sink for stdout/stderr
		// pass-through into the agent's tool result.
		_ = payload
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

	// Persist the heartbeat immediately. The cockpit sends a
	// heartbeat every 15s; the frontend treats a worker as offline
	// when lastSeenAt drifts more than 60s past now. Without this
	// write the row's lastSeenAt only ever reflects register-time,
	// so workers go offline 60s after pairing even when the
	// cockpit is happily heart-beating.
	if s.server == nil || s.server.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.server.store.UpdateLastSeen(ctx, s.worker.RegistrationId, at, sourceIP); err != nil {
		if s.server.logger != nil {
			s.server.logger.Warn("worker: persist heartbeat failed",
				"registration_id", s.worker.RegistrationId,
				"error", err,
			)
		}
	}
}

func (s *streamSession) handleToolResult(res *memqlv1.ToolResult) {
	if res == nil || res.GetCallId() == "" {
		return
	}
	s.mu.Lock()
	ch, ok := s.pending[res.GetCallId()]
	if ok {
		delete(s.pending, res.GetCallId())
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
// optional structured capability descriptor. The HEADLESS/GUI
// capability-string contract is unchanged: HEADLESS is mandatory,
// GUI is the only other admitted string, and scope checks keep
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
		if c != CapabilityHeadless && c != CapabilityGUI {
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
