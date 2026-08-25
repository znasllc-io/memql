package memql

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// THE ANONYMOUS STREAM (epic memql#4541, D4).
//
// A hosted site serving visitors who have not signed in reads the graph
// through the edge's same-origin /_memql/* proxy, which lands on the WS
// bridge, which opens this stream. Until now that stream needed an
// identity, so an anonymous visitor could not hydrate a page at all.
//
// # Three properties, in the order they matter
//
//  1. WITH THE FLAG OFF -- the default, and every cluster that exists today
//     -- this interceptor does NOTHING. It passes every stream to `base`
//     untouched, so a credential-less dial is refused by exactly the code
//     that refuses it now, with exactly the message it produces now. Not
//     "equivalent behaviour": the same code path.
//
//  2. A STREAM CARRYING ANY CREDENTIAL is passed through untouched too,
//     flag or no flag. Anonymous is what a caller gets for presenting
//     nothing, never a fallback for presenting something the chain did not
//     like. A bad bearer must fail as a bad bearer -- silently degrading a
//     malformed or EXPIRED credential to anonymous would turn "your session
//     ended" into "you are now a stranger", which is the same page with
//     less on it and no way to tell.
//
//  3. AN ADMITTED ANONYMOUS STREAM IS PINNED. Query execution and graph
//     subscriptions; nothing else.
//
// # Why the decision is here and not in the WS bridge
//
// The bridge is not the only way to reach this stream. The bff's gRPC edge
// is routed by the front door at `/`, so a client can dial it directly, and
// a design where the bridge decided "this session is anonymous" would have
// to tell the stream so -- over metadata, which the direct dialler can
// send. That is an authorization decision keyed on a header the caller
// controls.
//
// Deciding here removes the signal entirely. The rule is "no credential at
// all, on a cluster whose operator opted in", which is the same question
// with the same answer whether the caller is a browser behind the edge
// proxy or curl. The bridge's only remaining job is to stop returning 401
// before the stream is ever opened.
//
// # The pin is the surface, not the data
//
// What an anonymous caller may READ is decided by row admission against
// each concept's declared tier (component/memql/rowauthz_anonymous.go), and
// that gate is where the security property lives -- an anonymous actor
// reaches @rowAuthz(public) concepts and nothing else, undeclared included.
// This pin is the cheaper, coarser half: it stops a stranger from reaching
// the AI surface, the identity-admin surface or the tool loop at all,
// rather than letting each of those refuse in its own way.

// NewAnonymousStreamInterceptor wraps `base` and admits credential-less
// streams as the anonymous actor when the cluster opted in.
//
// enabled is injected rather than read from the environment on every call
// so a test can exercise both postures without mutating process state, and
// so the decision is made ONCE at wiring time -- a flag re-read per stream
// would let a cluster change its authentication posture halfway through its
// life without a restart, which is not a property anyone asked for.
func NewAnonymousStreamInterceptor(base grpc.StreamServerInterceptor, enabled bool, logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if base == nil {
			return status.Error(codes.Internal, "auth not configured")
		}
		if !enabled || streamCarriesCredential(ss.Context()) {
			return base(srv, ss, info, handler)
		}

		if logger != nil {
			logger.Debug("anonymous stream admitted (public reads enabled)",
				"method", info.FullMethod)
		}
		ctx := auth.ContextWithAnonymousActor(ss.Context())
		return handler(srv, &anonymousStream{ServerStream: ss, ctx: ctx, logger: logger})
	}
}

// streamCarriesCredential reports whether the caller presented anything at
// all that the auth chain would try to resolve.
//
// It reads the raw metadata rather than asking the chain, because the
// question here is deliberately NOT "is this credential valid" -- see
// property 2 above. Any non-empty authorization value, of any scheme, means
// this stream is somebody's and belongs to the chain.
//
// The guest query-parameter dial (?guest_token=) arrives as an
// `Authorization: Guest` header by the time it reaches this stream: the WS
// bridge rewrites it in metadataFromRequest. So there is no second shape to
// check here, and adding a speculative one would be a claim about a
// transport this file cannot see.
func streamCarriesCredential(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, key := range []string{"authorization", "Authorization"} {
		for _, v := range md.Get(key) {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	return false
}

// anonymousStream is the surface pin, in the voiceAgentStream mould: it
// stamps the anonymous actor onto every read context and refuses any
// payload outside the read surface BEFORE it reaches handleMessage.
type anonymousStream struct {
	grpc.ServerStream
	ctx    context.Context
	logger *slog.Logger
}

func (a *anonymousStream) Context() context.Context {
	if a == nil || a.ctx == nil {
		return a.ServerStream.Context()
	}
	return a.ctx
}

func (a *anonymousStream) RecvMsg(m any) error {
	if err := a.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	envelope, ok := m.(*memqlv1.MemqlClientMessage)
	if !ok {
		// Another RPC on another service; not this surface.
		return nil
	}
	if !isAnonymousPayload(envelope.GetPayload()) {
		if a.logger != nil {
			a.logger.Warn("anonymous stream received an off-surface payload",
				"payload_type", payloadTypeName(envelope.GetPayload()))
		}
		return status.Error(codes.PermissionDenied,
			"an unauthenticated session may execute queries and subscribe to graph events only -- sign in to reach anything else")
	}
	return nil
}

// isAnonymousPayload returns true for the payload types an unauthenticated
// session may send.
//
// The list is SHORT and it is an allowlist, which is the property that
// matters: a payload type added to the proto next year is refused here
// until somebody decides otherwise, rather than admitted because nobody
// thought about it. That is the opposite posture from a denylist, and it is
// why this cannot be written as "everything except the AI and identity
// messages".
//
// ExecuteQuery is on the list and CAN carry a mutation -- the surface is
// one message for both. That is not a hole left open: executeWrite refuses
// every anonymous write outright, before the concept is even resolved, so
// the read/write split is enforced where writes actually happen rather than
// by trying to parse intent out of a query string here. Two independent
// gates, and this is the coarser one.
//
// Subscribe is on the list, and its KIND is decided downstream: a non-graph
// kind is owner/admin-only (memql#4311), which the anonymous role fails, so
// the anonymous session gets graph subscriptions and no other kind without
// this file naming them. Each delivered graph event is then admitted per
// row by the same function the read path uses (memql#4309) -- which is what
// makes the live path inherit the read path's correctness instead of
// restating it.
func isAnonymousPayload(payload any) bool {
	switch payload.(type) {
	case *memqlv1.MemqlClientMessage_ClientHello,
		*memqlv1.MemqlClientMessage_Ack,
		*memqlv1.MemqlClientMessage_CancelRequest:
		// Stream-level control frames every caller needs.
		return true
	case *memqlv1.MemqlClientMessage_ExecuteQuery,
		*memqlv1.MemqlClientMessage_Subscribe,
		*memqlv1.MemqlClientMessage_Unsubscribe:
		return true
	}
	return false
}
