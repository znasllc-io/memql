// Package avatardirect exposes the direct/Guide avatar session to the MemQL
// DSL for the Anam vendor path (copresent#237 step 2 / #291).
//
// Background. The legacy direct avatar (integration.liveavatar) used
// liveavatar.com, which OWNS the LiveKit room and does its own TTS + lip-sync:
// memQL just minted a liveavatar session and handed back room creds. Anam works
// the opposite way -- it DIALS INTO an existing LiveKit room as the avatar
// participant and lip-syncs to audio published INTO that room. There is no
// voice-agent on the direct path, so memQL itself must, for an `anam`-vendor
// agent:
//
//  1. Resolve the agent's avatarVendor + avatarPersonaId.
//  2. Mint a fresh LiveKit room + a BROWSER join token (returned to copresent)
//     + an AVATAR-participant join token (handed to Anam).
//  3. Bring Anam up via the shared integrations/avatarvendor client
//     (CUSTOMER_CLIENT_V1 audio-driven, so it lip-syncs the audio copresent
//     publishes in step 3 / #292).
//  4. Return the same `{ livekit_url, livekit_client_token }` shape
//     useLiveAvatar already consumes, plus session_id + vendor + room_name.
//
// Vendor gate: this capability is the `anam` direct path. Non-anam agents keep
// the liveavatar path (unchanged) -- the liveavatar integration is retired
// separately in #237 step 5 (#294). Simli joins as the second vendor in #293.
//
// Capabilities (callable as builtins from .memql files):
//
//	avatardirect.startSession({ agentId: string, spaceId?: string })
//	    -> { livekit_url, livekit_client_token, session_id, vendor, room_name }
//	avatardirect.stopSession({ session_id?, vendor?, room_name? })
//	    -> { ok }
package avatardirect

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	coreauth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/avatarvendor"
)

const (
	// secretAnamAPIKey is the global secret the Anam REST calls authenticate
	// with. Resolved lazily at request time (with an env fallback) so the
	// factory succeeds on a fresh install -- the "key not configured" surface
	// error fires only when a caller actually invokes startSession for an
	// anam-vendor agent.
	secretAnamAPIKey = "ANAM_API_KEY"

	// tokenTTL bounds both the browser and avatar LiveKit join tokens.
	tokenTTL = 24 * time.Hour

	// systemActorId is the synthetic actor the internal agent lookup runs as
	// (read-only; the agent rows are owner-scoped but this is a server-side
	// resolution, mirroring the voice-agent's contextWithVoiceAgentActor).
	systemActorId = "system:avatardirect"
)

// Integration implements memql.IntegrationProvider for the direct Anam avatar.
// Registered as a core plug-in (init() in plugin.go) so the node copresent
// calls (the bff) carries it.
type Integration struct {
	engine        memql.IntegrationEngineAccess
	resolveSecret func(ctx context.Context, name string) (string, error)
	logger        *slog.Logger
	lk            polyphon.Config

	// newClient builds the vendor REST client from a resolved plan. Defaults to
	// avatarvendor.NewAvatarVendorClient; overridable in tests so the Anam REST
	// calls are asserted against a stub without a network.
	newClient func(plan avatarvendor.AvatarPlan) (avatarvendor.AvatarVendorClient, error)
}

// New constructs the integration. resolveSecret is usually
// engine.ResolveSystemSecret; lk carries the LiveKit transport creds
// (POLYPHON_LIVEKIT_*). The Anam key is resolved lazily at request time.
func New(
	engine memql.IntegrationEngineAccess,
	resolveSecret func(ctx context.Context, name string) (string, error),
	lk polyphon.Config,
	logger *slog.Logger,
) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		engine:        engine,
		resolveSecret: resolveSecret,
		logger:        logger,
		lk:            lk,
		newClient: func(plan avatarvendor.AvatarPlan) (avatarvendor.AvatarVendorClient, error) {
			return avatarvendor.NewAvatarVendorClient(plan, nil)
		},
	}
}

func (i *Integration) IntegrationName() string { return "avatardirect" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "startSession",
			Description: "Start a direct/Guide avatar session for an anam-vendor agent: mint a LiveKit room + browser join token, and bring Anam up (audio-driven) to dial in. Returns { livekit_url, livekit_client_token, session_id, vendor, room_name }.",
			Handler:     i.handleStartSession,
			ArgsSchema: map[string]string{
				"agentId": "string (required) - the agent whose avatarVendor + avatarPersonaId drive the session",
				"spaceId": "string (optional) - originating space, for logging/correlation",
			},
		},
		{
			Name:        "stopSession",
			Description: "Stop a direct avatar session. Best-effort: Anam sessions self-expire and the LiveKit room is ephemeral, so this always returns ok.",
			Handler:     i.handleStopSession,
			ArgsSchema: map[string]string{
				"session_id": "string (optional) - vendor session id from startSession",
				"vendor":     "string (optional) - vendor name from startSession",
				"room_name":  "string (optional) - LiveKit room name from startSession",
			},
		},
	}
}

// -----------------------------------------------------------------
// Capability handlers
// -----------------------------------------------------------------

func (i *Integration) handleStartSession(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	agentId := strings.TrimSpace(asString(args["agentId"]))
	if agentId == "" {
		return nil, fmt.Errorf("avatardirect.startSession: agentId is required")
	}
	spaceId := strings.TrimSpace(asString(args["spaceId"]))

	if !i.lk.LiveKitConfigured() {
		return nil, fmt.Errorf("avatardirect.startSession: LiveKit not configured (set POLYPHON_LIVEKIT_URL/API_KEY/API_SECRET)")
	}

	vendor, personaId, err := i.resolveAgentAvatar(ctx, agentId)
	if err != nil {
		return nil, err
	}

	// Vendor gate: this capability is the anam direct path. Other vendors keep
	// the liveavatar path until #294, and Simli arrives in #293.
	if vendor != string(avatarvendor.AvatarVendor("anam")) {
		return nil, fmt.Errorf("avatardirect.startSession: agent %s avatarVendor=%q is not supported on the direct Anam path (use the liveavatar path)", agentId, vendor)
	}

	anamKey, err := i.anamAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	// Mint a fresh, dedicated room for this avatar session (the direct/Guide
	// avatar is its own room the browser joins, like the liveavatar.com room).
	roomName := "avatar-" + id.NewShortId()
	browserIdentity := "viewer-" + id.NewShortId()

	browserToken, err := i.mintBrowserToken(roomName, browserIdentity)
	if err != nil {
		return nil, fmt.Errorf("avatardirect.startSession: mint browser token: %w", err)
	}
	avatarToken, err := i.mintAvatarToken(roomName)
	if err != nil {
		return nil, fmt.Errorf("avatardirect.startSession: mint avatar token: %w", err)
	}

	// Anam's cloud engine dials INTO LiveKit from outside, so it needs the
	// browser-reachable public URL (the internal ws://livekit:7880 the cluster
	// uses is unreachable from Anam's cloud).
	publicURL := i.lk.LiveKitPublicURL
	if publicURL == "" {
		publicURL = i.lk.LiveKitURL
	}

	ac := avatarvendor.AvatarConfig{
		Vendor:     vendor,
		AnamAPIKey: anamKey,
	}
	persona := avatarvendor.PersonaInput{
		AvatarPersonaID: personaId,
		AvatarVendor:    vendor,
		// The direct/Guide avatar is an explicit video session by definition --
		// the user is opening the avatar -- so video is on (no mirror-user gate
		// here, unlike the voice-agent path).
		VideoEnabled: true,
	}

	res, started, err := avatarvendor.StartAvatarSession(ctx, ac, persona, avatarvendor.AvatarStartParams{
		RoomName:     roomName,
		LiveKitURL:   publicURL,
		LiveKitToken: avatarToken,
	}, i.newClient)
	if err != nil {
		return nil, fmt.Errorf("avatardirect.startSession: bring up %s avatar: %w", vendor, err)
	}
	if !started {
		// nil plan -- the agent has no usable persona id (and no platform
		// default). Surface a specific error instead of silently returning a
		// room the avatar will never join.
		return nil, fmt.Errorf("avatardirect.startSession: agent %s has no avatar persona for vendor %q (audio-only)", agentId, vendor)
	}

	if i.logger != nil {
		i.logger.Info("avatardirect session started",
			"agent_id", agentId,
			"space_id", spaceId,
			"vendor", vendor,
			"room_name", roomName,
			"session_id", res.SessionID,
			"avatar_identity", res.AvatarIdentity)
	}

	out := map[string]any{
		"livekit_url":          publicURL,
		"livekit_client_token": browserToken,
		"session_id":           res.SessionID,
		"vendor":               vendor,
		"room_name":            roomName,
	}
	return wrapResult("avatardirect:session-start", "integration:avatardirect:sessionStart", out)
}

func (i *Integration) handleStopSession(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// Anam engine sessions self-expire and the LiveKit room is ephemeral
	// (it is torn down when the last participant leaves), so stop is a
	// best-effort acknowledgement -- it keeps the frontend clean-up path
	// idempotent without an Anam stop endpoint in the extracted client.
	if i.logger != nil {
		i.logger.Debug("avatardirect session stop (best-effort ok)",
			"session_id", asString(args["session_id"]),
			"vendor", asString(args["vendor"]),
			"room_name", asString(args["room_name"]))
	}
	return wrapResult("avatardirect:session-stop", "integration:avatardirect:sessionStop", map[string]any{"ok": true})
}

// -----------------------------------------------------------------
// Agent resolution
// -----------------------------------------------------------------

// resolveAgentAvatar reads the agent's avatarVendor + avatarPersonaId from the
// graph. Uses a raw `from(...) select` lookup (the same precedent the
// voice-agent session handler uses for the channel-mode lookup), since the
// shared agentFull shape does not project the avatar fields.
func (i *Integration) resolveAgentAvatar(ctx context.Context, agentId string) (vendor, personaId string, err error) {
	if i.engine == nil {
		return "", "", fmt.Errorf("avatardirect: no engine configured")
	}
	agentIdJSON, _ := json.Marshal(agentId)
	q := fmt.Sprintf(`from(v1:agents:agent) ?.id==%s select id, payload.avatarVendor, payload.avatarPersonaId`, string(agentIdJSON))
	raw, err := i.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return "", "", fmt.Errorf("avatardirect: resolve agent %s: %w", agentId, err)
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return "", "", fmt.Errorf("avatardirect: agent %s not found", agentId)
	}
	row := rows[0]
	vendor = strings.TrimSpace(rowString(row, "avatarVendor"))
	personaId = strings.TrimSpace(rowString(row, "avatarPersonaId"))
	return vendor, personaId, nil
}

// -----------------------------------------------------------------
// LiveKit token minting
// -----------------------------------------------------------------

// mintBrowserToken builds the LiveKit join token copresent connects with: a
// standard participant that can publish (the assistant audio it republishes in
// step 3) and subscribe (the avatar's video).
func (i *Integration) mintBrowserToken(roomName, identity string) (string, error) {
	canPublish := true
	canSubscribe := true
	at := auth.NewAccessToken(i.lk.LiveKitAPIKey, i.lk.LiveKitAPISecret)
	at.SetVideoGrant(&auth.VideoGrant{
		RoomJoin:     true,
		Room:         roomName,
		CanPublish:   &canPublish,
		CanSubscribe: &canSubscribe,
	}).
		SetIdentity(identity).
		SetName("You").
		SetValidFor(tokenTTL)
	return at.ToJWT()
}

// mintAvatarToken builds the LiveKit join token the Anam engine dials in with.
// kind=agent so it is recognized as an agent participant; it publishes the
// lip-synced video the browser subscribes to.
func (i *Integration) mintAvatarToken(roomName string) (string, error) {
	canPublish := true
	canSubscribe := true
	at := auth.NewAccessToken(i.lk.LiveKitAPIKey, i.lk.LiveKitAPISecret)
	at.SetVideoGrant(&auth.VideoGrant{
		RoomJoin:     true,
		Room:         roomName,
		CanPublish:   &canPublish,
		CanSubscribe: &canSubscribe,
	}).
		SetKind(livekit.ParticipantInfo_AGENT).
		SetIdentity(avatarvendor.AvatarParticipantIdentity).
		SetName(avatarvendor.AvatarParticipantName).
		SetValidFor(tokenTTL)
	return at.ToJWT()
}

// -----------------------------------------------------------------
// Config resolution
// -----------------------------------------------------------------

func (i *Integration) anamAPIKey(ctx context.Context) (string, error) {
	if i.resolveSecret != nil {
		if v, err := i.resolveSecret(ctx, secretAnamAPIKey); err == nil {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed, nil
			}
		}
	}
	// Env fallback (the voice-agent reads ANAM_API_KEY from env; mirror it so a
	// single-node dev box that exports the key works without a globalSecret row).
	if v := strings.TrimSpace(os.Getenv(secretAnamAPIKey)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("avatardirect: %s not found in v1:platform:globalSecret or env -- run `make secret-set NAME=%s VALUE=... SCOPE=global KIND=integration`", secretAnamAPIKey, secretAnamAPIKey)
}

// -----------------------------------------------------------------
// helpers
// -----------------------------------------------------------------

func systemActorContext(ctx context.Context) context.Context {
	return coreauth.ContextWithToken(ctx, &coreauth.TokenInfo{
		Subject: systemActorId,
		Claims: map[string]any{
			"sub":  systemActorId,
			"role": "system",
		},
	})
}

// extractRows normalizes the engine's Execute return into row maps. Mirrors the
// dailyspace integration's helper: unwrap *ExecuteResult, then walk the
// GraphBundle (raw concept / `select` queries surface as a bundle).
func extractRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	if res, ok := raw.(*memql.ExecuteResult); ok && res != nil {
		raw = res.OutputPayload()
	}
	if raw == nil {
		return nil
	}
	if bundle, ok := raw.(*memqlv1.GraphBundle); ok && bundle != nil {
		out := make([]map[string]any, 0, len(bundle.GetNodes()))
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{"id": n.GetId(), "concept": n.GetConcept()}
			if payload := n.GetPayload(); payload != nil {
				for k, v := range payload.AsMap() {
					row[k] = v
				}
			}
			out = append(out, row)
		}
		return out
	}
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}

// rowString reads a string field off a row map, probing the bare key and the
// nested payload map (shape() output nests under payload).
func rowString(row map[string]any, field string) string {
	if v, ok := row[field].(string); ok {
		return v
	}
	if payload, ok := row["payload"].(map[string]any); ok {
		if v, ok := payload[field].(string); ok {
			return v
		}
	}
	return ""
}

func wrapResult(idPrefix, concept string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, _ := json.Marshal(payload)
	return []memorynodes.MemoryNode{{
		ID:      fmt.Sprintf("%s:%d", idPrefix, time.Now().UnixNano()),
		Concept: concept,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}, nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
