package memql

// Actor propagation across a mesh forward -- the memql#3205 contract.
//
// Two properties are pinned here, and they are different properties:
//
//  1. A forwarded request that carries no provable authority binds NO actor,
//     and the engine's actor surfaces see nothing. This is the DEFECT the
//     contract replaces, kept as a test so a future "just attach the claims"
//     regression is loud.
//  2. The authority a session forwards is sourced from the POST-ROTATE
//     SESSION, not the stream context -- so a badge grant that arrived
//     mid-stream forwards its CLAMPED role. This is the one assertion the
//     reverted attempt did not make, and could not have made: its test called
//     the packing helper with a hand-built claims map and never touched a
//     streamSession, so it was green against the very defect it was named for.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// storedRoleRunner is an auth.QueryRunner that answers userByIdSystem with a
// fixed stored role. It stands in for the user row the identity resolver reads
// -- the row whose role a badge ceiling must clamp.
type storedRoleRunner struct{ role string }

func (r storedRoleRunner) ExecuteShaped(_ context.Context, _ string) (any, error) {
	return []any{map[string]any{
		"id":           "v1:identity:user:operator-9",
		"primaryEmail": "operator@example.com",
		"role":         r.role,
	}}, nil
}

// claimsStream is a captureStream whose Context carries claims, so a test can
// tell apart "read from the session" and "read from the stream context".
type claimsStream struct {
	captureStream
}

func newClaimsStream(claims map[string]any) *claimsStream {
	cs := &claimsStream{}
	cs.ctx = auth.ContextWithClaims(context.Background(), claims)
	return cs
}

func (c *claimsStream) Context() context.Context { return c.ctx }

var _ memqlv1.MemqlService_StreamServer = (*claimsStream)(nil)

func (c *claimsStream) SetHeader(metadata.MD) error  { return nil }
func (c *claimsStream) SendHeader(metadata.MD) error { return nil }

// TestForwardedAuthorityIsSourcedFromThePostRotateSession is THE test for the
// producer side of the contract.
//
// Scenario, exactly the one memql#2876 measured: a shared terminal holds a
// stream opened under a bearer whose claims say the operator is `owner`. The
// operator taps in, and a class="badge" grant with role_ceiling="reader"
// arrives MID-STREAM via RotateAuth. handleRotateAuth swaps s.access /
// s.identity / s.badgeExpiresAt -- and cannot touch the stream context, which
// gRPC fixed at stream-open.
//
// So the two candidate sources now disagree, and the disagreement is an
// escalation:
//
//	stream context (stale) -> no `class`, role="owner" -> isClusterOwner TRUE
//	session (post-rotate)  -> clamped role="reader"    -> isClusterOwner FALSE
//
// The test drives a REAL handleRotateAuth through the REAL verifier and then
// calls the REAL forwardedAuthority(), so it fails against a context-sourced
// implementation instead of passing against it.
func TestForwardedAuthorityIsSourcedFromThePostRotateSession(t *testing.T) {
	_, issueBadge, v := newRotateAuthFixtureWithBadge(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The stream context carries the PRE-rotation claims: an ordinary
	// user-class bearer for the operator, whose role is owner and which
	// carries no `class`, so nothing would clamp it.
	preRotateClaims := map[string]any{
		"sub":   "v1:identity:user:operator-9",
		"email": "operator@example.com",
		"role":  "owner",
	}

	svc := &service{
		verifier:         v,
		logger:           logger,
		identityResolver: auth.NewIdentityResolver(storedRoleRunner{role: "owner"}, logger),
	}
	cs := newClaimsStream(preRotateClaims)
	sess := &streamSession{
		service: svc,
		stream:  cs,
		logger:  logger,
		identity: auth.UserIdentity{
			Subject: "v1:identity:user:operator-9",
			Email:   "operator@example.com",
			Role:    "owner",
		},
		closeChan: make(chan struct{}),
	}

	// Sanity: before the rotation, the stream context really does resolve to
	// an unclamped owner. Without this the test could pass for the boring
	// reason that the context was empty all along.
	if ctxAccess := auth.FallbackFromClaims(preRotateClaims); ctxAccess.Role != auth.RoleOwner || !ctxAccess.IsClusterOwner() {
		t.Fatalf("fixture is not exercising the escalation: stream-context claims resolve to role=%q isClusterOwner=%v, want owner/true",
			ctxAccess.Role, ctxAccess.IsClusterOwner())
	}

	grant := issueBadge("v1:identity:user:operator-9", "reader", time.Minute)
	if err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "rot-1"},
		&memqlv1.RotateAuthMsg{AccessToken: grant},
	); err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	if res := cs.lastSent().GetRotateAuthResult(); res == nil || !res.GetOk() {
		t.Fatalf("badge rotate should succeed; got %+v", res)
	}

	authority, err := sess.forwardedAuthority()
	if err != nil {
		t.Fatalf("forwardedAuthority: %v", err)
	}

	if authority.Kind != auth.AuthorityKindBadge {
		t.Errorf("kind = %q, want %q -- a rotated-in badge grant must forward as a badge so the worker can gate on its expiry",
			authority.Kind, auth.AuthorityKindBadge)
	}
	if authority.Role != auth.RoleReader {
		t.Errorf("forwarded role = %q, want %q. The stream context still says owner; reading it instead of the session is the memql#2876 escalation",
			authority.Role, auth.RoleReader)
	}
	if authority.BadgeExpires.IsZero() {
		t.Error("badge authority carries no expiry; the worker cannot gate an expired grant without it")
	}

	// The receiving side must land on the clamped role, not merely carry it.
	ac := authority.AccessContext()
	if ac == nil {
		t.Fatal("badge authority resolved no AccessContext on the receiving side")
	}
	if ac.Role != auth.RoleReader {
		t.Errorf("receiver-side role = %q, want reader", ac.Role)
	}
	if ac.IsClusterOwner() {
		t.Error("receiver resolved isClusterOwner=true for a reader-ceilinged badge grant -- this is the reachable escalation, not a lossy forward")
	}
}

// TestForwardedAuthorityRefusesAnUnrotatedEmptySession guards the other
// direction of the producer gate: a session with no resolvable principal must
// fail to build an authority rather than emit an empty one. An empty principal
// on the wire is the silent-zero-rows failure the contract removes.
func TestForwardedAuthorityRefusesAnUnrotatedEmptySession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess := &streamSession{
		service:   &service{logger: logger},
		stream:    newClaimsStream(map[string]any{}),
		logger:    logger,
		closeChan: make(chan struct{}),
	}
	if _, err := sess.forwardedAuthority(); err == nil {
		t.Fatal("expected an error building an authority with no resolved principal; emitting an empty one is the defect")
	}
}

// TestForwardedRequestWithoutAuthorityIsRefused pins the receiver's central
// rule against the REAL HandleForwardedRequest.
//
// Before the contract this exact request was accepted: the receiver attached
// whatever claims came with it, never built an AccessContext, and every
// actor-gated construct it then executed returned zero rows or wrote
// createdBy:"" -- silently.
func TestForwardedRequestWithoutAuthorityIsRefused(t *testing.T) {
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	envelope := &memqlv1.MemqlClientMessage{
		MessageId: "req-noauth",
		Payload: &memqlv1.MemqlClientMessage_AiChat{
			AiChat: &memqlv1.AiChatMsg{RequestId: "req-noauth"},
		},
	}
	envBytes, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var sent []*nodev1.NodeServerMessage
	send := func(m *nodev1.NodeServerMessage) error {
		sent = append(sent, m)
		return nil
	}

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId:     "req-noauth",
		MemqlEnvelope: envBytes,
		// Authority deliberately omitted -- the whole point.
	}, send)

	if len(sent) != 1 {
		t.Fatalf("expected exactly one refusal response, got %d", len(sent))
	}
	resp := sent[0].GetAiForwardResponse()
	if resp == nil {
		t.Fatal("refusal was not an AiForwardResponse")
	}
	if !resp.GetDone() {
		t.Error("a refusal on a stream-INITIATING envelope should be terminal so the caller unblocks")
	}
	var serverMsg memqlv1.MemqlServerMessage
	if err := proto.Unmarshal(resp.GetMemqlServerMsg(), &serverMsg); err != nil {
		t.Fatalf("unmarshal refusal: %v", err)
	}
	qe := serverMsg.GetQueryError()
	if qe == nil || qe.GetError() == nil {
		t.Fatalf("refusal did not carry a QueryError: %+v", &serverMsg)
	}
	if got := qe.GetError().GetCode(); got != "PermissionDenied" {
		t.Errorf("refusal code = %q, want PermissionDenied", got)
	}
}

// TestRefusalOnAContinuationDoesNotCloseTheParentStream is the memql#3205
// acceptance criterion about pausing a Plan in cluster mode.
//
// Continuations reuse the PARENT turn's request_id. sendForwardError used to
// set Done unconditionally, and AiForwardRouter.Dispatch calls cleanupInflight
// on done -- which closes the parent's response channel. So a refused pause
// signal would have killed the in-flight reply the user was watching while the
// agent kept running.
//
// The test refuses an AgentPreemptTurn (by withholding the authority) and
// asserts the response is NOT terminal.
func TestRefusalOnAContinuationDoesNotCloseTheParentStream(t *testing.T) {
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for _, tc := range []struct {
		name    string
		payload *memqlv1.MemqlClientMessage
	}{
		{
			name: "AgentPreemptTurn",
			payload: &memqlv1.MemqlClientMessage{
				MessageId: "parent-1",
				Payload: &memqlv1.MemqlClientMessage_AgentPreemptTurn{
					AgentPreemptTurn: &memqlv1.AgentPreemptTurnMsg{RequestId: "parent-1"},
				},
			},
		},
		{
			name: "ClientToolResult",
			payload: &memqlv1.MemqlClientMessage{
				MessageId: "parent-2",
				Payload: &memqlv1.MemqlClientMessage_ClientToolResult{
					ClientToolResult: &memqlv1.ClientToolResult{CallId: "call-2"},
				},
			},
		},
		{
			name: "AiTranscribeStreamChunk",
			payload: &memqlv1.MemqlClientMessage{
				MessageId: "parent-3",
				Payload: &memqlv1.MemqlClientMessage_AiTranscribeStreamChunk{
					AiTranscribeStreamChunk: &memqlv1.AiTranscribeStreamChunk{RequestId: "parent-3"},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !isContinuationPayload(tc.payload) {
				t.Fatalf("%s must be classified as a continuation; it reuses the parent request_id", tc.name)
			}
			envBytes, err := proto.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var sent []*nodev1.NodeServerMessage
			svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
				RequestId:     tc.payload.GetMessageId(),
				MemqlEnvelope: envBytes,
			}, func(m *nodev1.NodeServerMessage) error {
				sent = append(sent, m)
				return nil
			})
			if len(sent) != 1 {
				t.Fatalf("expected one refusal, got %d", len(sent))
			}
			if sent[0].GetAiForwardResponse().GetDone() {
				t.Error("refusal marked done on a continuation: the BFF will cleanupInflight the PARENT request_id and blank an in-flight turn")
			}
		})
	}
}

// TestForwardedInternalAuthorityIsAcceptedAndBindsNoActor covers the two
// deliberate no-principal producers (the planner's pause signal, the
// client-tool relay). They must pass the gate -- "no principal" is a VALUE --
// while still binding no actor, so anything actor-gated on that path fails
// closed exactly as before.
func TestForwardedInternalAuthorityIsAcceptedAndBindsNoActor(t *testing.T) {
	authority := auth.InternalAuthority()
	if err := authority.Validate(time.Now()); err != nil {
		t.Fatalf("internal authority must be accepted, got %v", err)
	}
	if ac := authority.AccessContext(); ac != nil {
		t.Errorf("internal authority bound an actor (%+v); it must bind none", ac)
	}
	ctx := auth.ContextWithForwardedAuthority(context.Background(), authority)
	if actor := auth.ActorFromContext(ctx); actor != "" {
		t.Errorf("internal authority put actor %q on the ctx; actor-gated work must fail closed here", actor)
	}
	// ...but the assertion itself survives, so a node that forwards onward
	// re-asserts "no principal" rather than upgrading it.
	if got := auth.ForwardedAuthorityFromContext(ctx); got == nil || got.Kind != auth.AuthorityKindInternal {
		t.Errorf("internal authority did not survive on the ctx for the next hop: %+v", got)
	}
}

// TestForwardedSessionBindsTheCarriedDecision covers the ACCEPT-AND-BIND half
// of the contract -- the step that actually fixes memql#3205.
//
// Every other test here exercises a REFUSAL, and a refusal returns before this
// code runs. So without this, deleting either the ctx binding or the access
// seeding in newForwardedSession restores the original silent-zero-rows defect
// with component/grpc and component/auth both fully green.
func TestForwardedSessionBindsTheCarriedDecision(t *testing.T) {
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	send := func(*nodev1.NodeServerMessage) error { return nil }

	t.Run("user authority binds the actor on the ctx handlers read", func(t *testing.T) {
		authority := &auth.ForwardedAuthority{
			Kind:         auth.AuthorityKindUser,
			UserId:       "v1:identity:user:alice",
			PrimaryEmail: "alice@example.com",
			Role:         auth.RoleWriter,
			FirstName:    "Alice",
			LastName:     "Nakamura",
		}
		sess := svc.newForwardedSession(context.Background(), authority, "req-1", send)

		// The ctx handlers actually read is s.stream.Context(). If the binding
		// is dropped, worker-side DSL resolves actor.userId to "" and every
		// owned query silently returns zero rows.
		ctx := sess.stream.Context()
		ac, ok := auth.AccessFromContext(ctx)
		if !ok || ac == nil {
			t.Fatal("forwarded session ctx carries no AccessContext -- this IS the memql#3205 defect")
		}
		if ac.UserId != "v1:identity:user:alice" || ac.Role != auth.RoleWriter {
			t.Errorf("bound actor = %+v, want alice/writer", ac)
		}
		if actor := auth.ActorFromContext(ctx); actor == "" {
			t.Error("no actor on the forwarded ctx; mutations would fail with \"no actor found in context\"")
		}

		// Seeded access is what removes the per-message userByIdSystem
		// round-trip: ensureAccess must be a cache hit, and must NOT be able
		// to fall through to the claims path (which would resurrect
		// FallbackFromClaims on the mesh).
		if !sess.accessLoaded {
			t.Error("accessLoaded is false; ensureAccess would re-resolve per forwarded message")
		}
		if got := sess.ensureAccess(ctx); got == nil || got.UserId != "v1:identity:user:alice" {
			t.Errorf("ensureAccess on the forwarded session = %+v, want the carried decision", got)
		}

		// Provenance parity: identity.displayName is stamped on every row a
		// mutation writes, and it resolves through UserIdentityFromContext.
		id, err := auth.UserIdentityFromContext(ctx)
		if err != nil {
			t.Fatalf("UserIdentityFromContext: %v", err)
		}
		if id.FirstName != "Alice" || id.LastName != "Nakamura" {
			t.Errorf("display name lost across the hop: first=%q last=%q -- rows this worker writes would omit identity.displayName that the same user's direct-path rows carry",
				id.FirstName, id.LastName)
		}
	})

	t.Run("badge authority arms the worker-side expiry gate", func(t *testing.T) {
		exp := time.Now().Add(time.Minute)
		authority := &auth.ForwardedAuthority{
			Kind:         auth.AuthorityKindBadge,
			UserId:       "v1:identity:user:operator-9",
			Role:         auth.RoleReader,
			BadgeExpires: exp,
		}
		sess := svc.newForwardedSession(context.Background(), authority, "req-2", send)

		sess.accessMu.Lock()
		stamped, at := sess.badgeStamped, sess.badgeExpiresAt
		sess.accessMu.Unlock()
		if !stamped {
			t.Error("badge gate not stamped; it would lazily re-stamp from a stream context a forwarded session does not have")
		}
		if !at.Equal(exp) {
			t.Errorf("badge expiry on the session = %v, want %v", at, exp)
		}
	})

	t.Run("internal authority binds no actor", func(t *testing.T) {
		sess := svc.newForwardedSession(context.Background(), auth.InternalAuthority(), "req-3", send)
		if ac, ok := auth.AccessFromContext(sess.stream.Context()); ok && ac != nil {
			t.Errorf("internal authority bound actor %+v; actor-gated work must fail closed here", ac)
		}
		// accessLoaded must still be true, or ensureAccess falls through to
		// the claims path and FallbackFromClaims becomes reachable again.
		if !sess.accessLoaded {
			t.Error("accessLoaded false for internal; ensureAccess could fall through to FallbackFromClaims")
		}
		if got := sess.ensureAccess(sess.stream.Context()); got != nil {
			t.Errorf("ensureAccess resolved %+v for an internal forward; it must stay nil", got)
		}
	})
}

// TestForwardedAuthorityRoundTripsThroughTheWire proves the proto conversion
// preserves every field the receiver's decisions depend on -- in particular
// the badge expiry, whose loss would make the ceiling unenforceable downstream.
func TestForwardedAuthorityRoundTripsThroughTheWire(t *testing.T) {
	exp := time.Now().Add(90 * time.Second).Truncate(time.Second)
	in := &auth.ForwardedAuthority{
		Kind:         auth.AuthorityKindBadge,
		UserId:       "v1:identity:user:operator-9",
		PrimaryEmail: "operator@example.com",
		Role:         auth.RoleReader,
		BadgeExpires: exp,
		LocalDev:     true,
		FirstName:    "Ops",
		LastName:     "Operator",
	}
	out := authorityFromProto(authorityToProto(in))
	if out == nil {
		t.Fatal("round trip produced nil")
	}
	if out.Kind != in.Kind || out.UserId != in.UserId || out.Role != in.Role ||
		out.PrimaryEmail != in.PrimaryEmail || out.LocalDev != in.LocalDev ||
		out.FirstName != in.FirstName || out.LastName != in.LastName {
		t.Errorf("round trip changed the authority:\n got %+v\nwant %+v", out, in)
	}
	if !out.BadgeExpires.Equal(exp) {
		t.Errorf("badge expiry did not survive the wire: got %v want %v", out.BadgeExpires, exp)
	}
	// nil in, nil out: the receiver's refusal depends on nil being
	// representable end to end rather than collapsing to an empty message.
	if authorityToProto(nil) != nil {
		t.Error("nil authority must encode as a nil message, not an empty one -- the receiver refuses on nil")
	}
	if authorityFromProto(nil) != nil {
		t.Error("a nil wire authority must decode to nil so Validate refuses it")
	}
}
