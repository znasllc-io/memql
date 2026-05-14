package memql

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/visionarys-io/memql/component/auth"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

// voiceAgentTokenPrefix is the conventional prefix for shared-secret
// tokens issued to the voice-agent process. The interceptor strips it
// and constant-time-compares the remainder against the configured
// MEMQL_VOICE_AGENT_SHARED_TOKEN.
const voiceAgentTokenPrefix = "mql_va_"

// NewVoiceAgentStreamInterceptor wraps `base` and recognizes the
// shared-secret used by the Python voice-agent (LiveKit Agents 1.5).
//
// Auth shape: Authorization: Bearer mql_va_<shared-secret>
//
// When the supplied bearer matches the expected token, the call is
// admitted ONLY if the dispatched payload is one of the VoiceAgent*
// message types. Every other surface is rejected with PermissionDenied
// regardless of secret validity. This mirrors the worker-token /
// guest-token interceptor approach: per-identity-type surface pinning.
//
// Phase 2 ships this with a shared-secret. A follow-up will swap to a
// proper service-account identity row + JWT, but the surface-pinning
// stays identical -- callers won't notice the auth-source change.
//
// If `expectedToken` is empty the interceptor is a no-op (the voice
// path isn't configured for this build), and any incoming Bearer with
// the mql_va_ prefix is rejected.
func NewVoiceAgentStreamInterceptor(
	base grpc.StreamServerInterceptor,
	expectedToken string,
	logger *slog.Logger,
) grpc.StreamServerInterceptor {
	expectedToken = strings.TrimSpace(expectedToken)
	expectedBytes := []byte(expectedToken)

	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		scheme, token := schemeAndTokenFromMetadata(ss.Context())
		isVoiceAgent := strings.EqualFold(scheme, "Bearer") && strings.HasPrefix(token, voiceAgentTokenPrefix)

		if !isVoiceAgent {
			if base == nil {
				return status.Error(codes.Internal, "auth not configured")
			}
			return base(srv, ss, info, handler)
		}

		// We have what looks like a voice-agent token. If the cluster
		// has no expected token configured, reject (defense in depth --
		// don't accidentally accept an attacker-supplied prefix on a
		// build that isn't supposed to be running the voice path).
		if expectedToken == "" {
			return status.Error(codes.Unauthenticated, "voice-agent path not configured on this node")
		}

		// Constant-time compare the post-prefix portion of the token.
		// Trim the prefix; what remains is the shared secret.
		supplied := strings.TrimPrefix(token, voiceAgentTokenPrefix)
		if subtle.ConstantTimeCompare([]byte(supplied), expectedBytes) != 1 {
			if logger != nil {
				logger.Warn("voice-agent token rejected", "method", info.FullMethod)
			}
			return status.Error(codes.Unauthenticated, "invalid voice-agent token")
		}

		// Admit. Stamp a synthetic actor identity onto the context so
		// downstream handlers see a system-actor on every graph write.
		// Surface-pinning happens per-message inside handleMessage in
		// server.go; the in-stream check below short-circuits any
		// non-VoiceAgent payload that slips through.
		//
		// IMPORTANT: call `handler` directly, NOT `base`. base is
		// the rest of the auth chain (verifier / session-revocation
		// / guest / operator) and the verifier rejects the
		// shared-secret token as not being a valid JWT. The
		// worker-token + guest-token interceptors do the same: when
		// the alt-auth scheme is admitted, we skip the rest of the
		// chain and go straight to the gRPC handler.
		ctx := withVoiceAgentClaims(ss.Context())
		return handler(srv, &voiceAgentStream{ServerStream: ss, ctx: ctx, logger: logger})
	}
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
		*memqlv1.MemqlClientMessage_VoiceAgentTurnRequest:
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

// withVoiceAgentClaims stamps a system-actor identity for the
// voice-agent service. Distinct from the polyphon system actor so
// audit + telemetry can tell voice-agent traffic apart from legacy
// bridge-agent traffic during the Phase 10 cutover window.
func withVoiceAgentClaims(ctx context.Context) context.Context {
	claims := map[string]any{
		"sub":   "voice-agent",
		"email": "voice-agent@memql.internal",
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
