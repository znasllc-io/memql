package memql

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// NewVoiceAgentStreamInterceptor wraps `base` and admits the Python
// voice-agent process via a class="voice_agent" identity-issued JWT
// (#109). The JWT is verified through the per-node verifier (the
// same one wired for every other authenticated surface); on
// successful verification the call is admitted only when the
// dispatched payload is one of the VoiceAgent* message types --
// every other surface is rejected with PermissionDenied so a
// leaked voice-agent credential can't drive other RPCs.
//
// Auth shape: `Authorization: Bearer <eyJ...>` where the JWT
// carries `class="voice_agent"` and an `instance_id` (stamped via
// the verifier into VerifiedClaims.NodeId for audit attribution).
//
// Non-voice-agent bearers fall through to `base` (the rest of the
// auth chain). A nil `v` parameter disables the voice-agent admit
// path entirely; intended for tests and binaries that don't run
// the voice-agent surface.
func NewVoiceAgentStreamInterceptor(
	base grpc.StreamServerInterceptor,
	v *verifier.Verifier,
	logger *slog.Logger,
) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		scheme, token := schemeAndTokenFromMetadata(ctx)

		// Only JWT-shaped bearers are candidates for the voice-agent
		// path. PAT (mql_pat_) and worker-token (mql_wkr_) bearers
		// are reserved for other surfaces and pre-screened here so
		// we don't waste a verifier round-trip on them.
		if v == nil || !strings.EqualFold(scheme, "Bearer") || token == "" || hasReservedTokenPrefix(token) {
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}

		vc, err := v.VerifyBearer(ctx, token)
		if err != nil || vc == nil || vc.Source != verifier.SourceJWT || vc.Class != verifier.ClassVoiceAgent {
			// Not a voice-agent JWT (could be a regular user JWT
			// for a different surface). Hand off to the base chain
			// which re-runs VerifyBearer (cheap; JWKS hit was
			// cached on the first call) and decides.
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}

		if logger != nil {
			logger.Debug("voice-agent admitted",
				"instance_id", vc.NodeId,
				"method", info.FullMethod)
		}
		ctx = withVoiceAgentClaims(ctx, vc)
		return handler(srv, &voiceAgentStream{ServerStream: ss, ctx: ctx, logger: logger})
	}
}

// hasReservedTokenPrefix reports whether `token` starts with one of
// the bearer-prefix schemes that aren't JWTs (PAT, worker token).
// Used to short-circuit the voice-agent verifier path on bearers
// that can't possibly be voice-agent JWTs.
func hasReservedTokenPrefix(token string) bool {
	for _, p := range []string{"mql_pat_", "mql_wkr_"} {
		if strings.HasPrefix(token, p) {
			return true
		}
	}
	return false
}

// voiceAgentStream wraps the gRPC ServerStream so we can:
//
//  1. Inject the voice-agent system actor onto every read context.
//  2. Reject payloads that are NOT VoiceAgent* message types BEFORE
//     they reach handleMessage. This is the surface-pinning gate.
//
// The wrapped RecvMsg unmarshals the inbound envelope, inspects its
// payload oneof, and aborts the stream with PermissionDenied on any
// non-VoiceAgent message. Once the payload check passes we forward
// to the underlying stream unchanged.
type voiceAgentStream struct {
	grpc.ServerStream
	ctx    context.Context
	logger *slog.Logger
}

func (v *voiceAgentStream) Context() context.Context {
	if v == nil || v.ctx == nil {
		return v.ServerStream.Context()
	}
	return v.ctx
}

func (v *voiceAgentStream) RecvMsg(m any) error {
	if err := v.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	envelope, ok := m.(*memqlv1.MemqlClientMessage)
	if !ok {
		// Other RPCs aren't on this surface; pass through unchanged.
		return nil
	}
	if !isVoiceAgentPayload(envelope.GetPayload()) {
		if v.logger != nil {
			v.logger.Warn("voice-agent stream received non-voice payload",
				"payload_type", payloadTypeName(envelope.GetPayload()))
		}
		return status.Error(codes.PermissionDenied,
			"voice-agent tokens may only call VoiceAgent* message types")
	}
	return nil
}

// isVoiceAgentPayload returns true for the five client-to-server
// payload types the voice-agent is allowed to send. ClientHello and
// Heartbeat are passed through too -- those are stream-level control
// frames that every caller needs.
func isVoiceAgentPayload(payload any) bool {
	switch payload.(type) {
	case *memqlv1.MemqlClientMessage_ClientHello,
		*memqlv1.MemqlClientMessage_Ack,
		*memqlv1.MemqlClientMessage_Unsubscribe,
		*memqlv1.MemqlClientMessage_CancelRequest:
		return true
	case *memqlv1.MemqlClientMessage_VoiceAgentSessionStart,
		*memqlv1.MemqlClientMessage_VoiceAgentSessionEnd,
		*memqlv1.MemqlClientMessage_VoiceAgentPartialTranscript,
		*memqlv1.MemqlClientMessage_VoiceAgentFinalTranscript,
		*memqlv1.MemqlClientMessage_VoiceAgentTurnRequest,
		*memqlv1.MemqlClientMessage_VoiceAgentRealtimeOutput:
		return true
	case *memqlv1.MemqlClientMessage_ListTools,
		*memqlv1.MemqlClientMessage_CallTool:
		// The realtime executor's MCP tool bridge (mcp_tool_bridge.go) lists the
		// tool registry (ListToolsMsg) at session bring-up and runs model-driven
		// tool calls (CallToolMsg) through memQL's MCP surface so the result is
		// mirrored + attributed to the space/agent. gpt-realtime function-calling
		// is part of the voice-agent surface, so these belong on the allowlist --
		// without them the read loop is torn down with PermissionDenied the moment
		// the realtime executor initializes, before any audio can flow.
		return true
	}
	return false
}

// payloadTypeName returns a short log-friendly name for the oneof
// payload type. Used only on rejection paths so the fmt overhead is
// fine; we don't pre-pay for the happy path.
func payloadTypeName(payload any) string {
	if payload == nil {
		return "<nil>"
	}
	full := fmt.Sprintf("%T", payload)
	return strings.TrimPrefix(full, "*memqlv1.MemqlClientMessage_")
}

// withVoiceAgentClaims stamps a system-actor identity onto the
// context, with the verified token's instance_id pulled across for
// audit attribution. Downstream handlers see a system-actor on
// every graph write so audit + telemetry can attribute voice-agent
// traffic to the specific process that produced it.
func withVoiceAgentClaims(ctx context.Context, vc *verifier.VerifiedClaims) context.Context {
	instanceId := ""
	if vc != nil {
		instanceId = vc.NodeId
	}
	claims := map[string]any{
		"sub":         "voice-agent",
		"email":       "voice-agent@memql.internal",
		"role":        "system",
		"class":       verifier.ClassVoiceAgent,
		"instance_id": instanceId,
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
