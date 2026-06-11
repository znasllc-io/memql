// Package avatardirect exposes the direct/Guide avatar session to the MemQL
// DSL for the cloud-engine avatar vendors -- Anam (copresent#237 step 2 / #291)
// and Simli (memql#782).
//
// Background. The legacy direct avatar (integration.liveavatar) used
// liveavatar.com, which OWNS the LiveKit room and does its own TTS + lip-sync:
// memQL just minted a liveavatar session and handed back room creds. The
// cloud-engine vendors work the opposite way -- the vendor DIALS INTO an
// existing LiveKit room as the avatar participant and lip-syncs to audio
// published INTO that room. There is no voice-agent on the direct path, so
// memQL drives a TWO-PHASE handshake for an `anam`- or `simli`-vendor agent.
// The phases exist because Simli's cloud engine joins the room, waits for an
// audio-providing AGENT-kind participant to ALREADY be present, and reads that
// participant's lk.audio_stream byte stream for lip-sync; if memQL starts the
// engine before the browser has joined and begun forwarding audio (the old
// single-phase flow), the engine waits on an absent participant and never
// publishes video (memql#782).
//
//  1. startSession: resolve the agent's avatarVendor + avatarPersonaId, mint a
//     fresh LiveKit room + an AGENT-kind BROWSER join token, validate the
//     persona, and return { livekit_url, livekit_client_token, browser_identity,
//     vendor, room_name }. The vendor engine is NOT started here.
//  2. The browser joins the room with those creds and starts forwarding the
//     assistant audio over the lk.audio_stream byte stream.
//  3. engageVendor: mint the AVATAR-participant join token (kind=agent,
//     lk.publish_on_behalf=browser_identity) and bring the vendor engine up via
//     the shared integrations/avatarvendor client so it dials in and lip-syncs
//     the forwarded audio. Returns { session_id, vendor, avatar_identity, ok }.
//
// Vendor gate: this capability serves the cloud-engine vendors `anam` and
// `simli`; the agent's stamped avatarVendor (or the MEMQL_AVATAR_VENDOR runtime
// default) selects which. The per-vendor API key (ANAM_API_KEY / SIMLI_API_KEY)
// is resolved lazily from the globalSecret store (env fallback).
//
// Capabilities (callable as builtins from .memql files):
//
//	avatardirect.startSession({ agentId: string, spaceId?: string })
//	    -> { livekit_url, livekit_client_token, browser_identity, vendor, room_name }
//	avatardirect.engageVendor({ agentId, room_name, browser_identity, spaceId? })
//	    -> { session_id, vendor, avatar_identity, ok }
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
	// secretAnamAPIKey / secretSimliAPIKey are the global secrets the vendor
	// REST calls authenticate with. Resolved lazily at request time (with an
	// env fallback) so the factory succeeds on a fresh install -- the "key not
	// configured" surface error fires only when a caller actually invokes
	// startSession for an agent of that vendor.
	secretAnamAPIKey  = "ANAM_API_KEY"
	secretSimliAPIKey = "SIMLI_API_KEY"

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
			Description: "Phase 1 of the direct/Guide avatar handshake for an anam- or simli-vendor agent: mint a dedicated LiveKit room + an AGENT-kind browser join token, validate the agent has a usable avatar persona, and return the creds the browser joins with. Does NOT bring the vendor cloud engine up -- the browser must join the room and start forwarding the assistant audio first, then call engageVendor (Simli's engine waits for an audio-providing agent participant to already be present, memql#782). Returns { livekit_url, livekit_client_token, browser_identity, vendor, room_name }.",
			Handler:     i.handleStartSession,
			ArgsSchema: map[string]string{
				"agentId": "string (required) - the agent whose avatarVendor + avatarPersonaId drive the session",
				"spaceId": "string (optional) - originating space, for logging/correlation",
			},
		},
		{
			Name:        "engageVendor",
			Description: "Phase 2 of the direct/Guide avatar handshake: bring the vendor cloud engine (Anam / Simli) up so it dials into the room minted by startSession. Call this only AFTER the browser has joined the room (browser_identity) and started forwarding the assistant audio over the lk.audio_stream byte stream, so the vendor engine finds the audio-providing participant already present. Mints the avatar join token (kind=agent, lk.publish_on_behalf=browser_identity) and starts the vendor session. Returns { session_id, vendor, avatar_identity, ok }.",
			Handler:     i.handleEngageVendor,
			ArgsSchema: map[string]string{
				"agentId":          "string (required) - the same agent passed to startSession",
				"room_name":        "string (required) - the room_name returned by startSession",
				"browser_identity": "string (required) - the browser_identity returned by startSession; the avatar publishes its video on behalf of it",
				"spaceId":          "string (optional) - originating space, for logging/correlation",
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

// handleStartSession is phase 1 of the avatar handshake. It mints the room +
// the AGENT-kind browser join token and validates the agent has a usable avatar
// persona, but does NOT bring the vendor engine up. The browser joins the room
// with these creds and starts forwarding the assistant audio (over the
// lk.audio_stream byte stream) BEFORE calling engageVendor -- because Simli's
// cloud receiver waits for an audio-providing agent participant to already be
// present and reads its byte stream; joining the engine first (the old
// single-phase flow) left the engine waiting on an absent participant and it
// never published video (memql#782).
func (i *Integration) handleStartSession(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	agentId := strings.TrimSpace(asString(args["agentId"]))
	if agentId == "" {
		return nil, fmt.Errorf("avatardirect.startSession: agentId is required")
	}
	spaceId := strings.TrimSpace(asString(args["spaceId"]))

	if !i.lk.LiveKitConfigured() {
		return nil, fmt.Errorf("avatardirect.startSession: LiveKit not configured (set POLYPHON_LIVEKIT_URL/API_KEY/API_SECRET)")
	}

	vendor, personaId, ac, err := i.resolveVendorConfig(ctx, agentId)
	if err != nil {
		return nil, err
	}

	// Validate the agent has a usable avatar persona up front (no network: this
	// is the pure plan resolution), so phase 1 fails fast instead of handing
	// back a room the avatar will never render in. A nil plan means audio-only.
	plan, perr := avatarvendor.ResolveAvatarPlan(ac, avatarvendor.PersonaInput{
		AvatarPersonaID: personaId,
		AvatarVendor:    vendor,
		VideoEnabled:    true,
	})
	if perr != nil {
		return nil, fmt.Errorf("avatardirect.startSession: resolve %s avatar: %w", vendor, perr)
	}
	if plan == nil {
		return nil, fmt.Errorf("avatardirect.startSession: agent %s has no avatar persona for vendor %q (audio-only)", agentId, vendor)
	}

	// Mint a fresh, dedicated room for this avatar session (the direct/Guide
	// avatar is its own room the browser joins, like the liveavatar.com room).
	roomName := "avatar-" + id.NewShortId()
	browserIdentity := "viewer-" + id.NewShortId()

	browserToken, err := i.mintBrowserToken(roomName, browserIdentity)
	if err != nil {
		return nil, fmt.Errorf("avatardirect.startSession: mint browser token: %w", err)
	}

	// browserURL is what copresent connects with: POLYPHON_LIVEKIT_PUBLIC_URL
	// (e.g. wss://livekit.local.znas.io), or the internal URL as a fallback.
	browserURL := i.lk.LiveKitPublicURL
	if browserURL == "" {
		browserURL = i.lk.LiveKitURL
	}

	if i.logger != nil {
		i.logger.Info("avatardirect startSession (phase 1: room minted, engine deferred)",
			"agent_id", agentId,
			"space_id", spaceId,
			"vendor", vendor,
			"room_name", roomName,
			"browser_identity", browserIdentity)
	}

	out := map[string]any{
		"livekit_url":          browserURL,
		"livekit_client_token": browserToken,
		"browser_identity":     browserIdentity,
		"vendor":               vendor,
		"room_name":            roomName,
	}
	return wrapResult("avatardirect:session-start", "integration:avatardirect:sessionStart", out)
}

// handleEngageVendor is phase 2 of the avatar handshake. With the browser
// already in the room and forwarding audio, it mints the avatar join token
// (kind=agent, lk.publish_on_behalf=browser_identity so the avatar's video is
// attributed to the participant Simli reads audio from) and brings the vendor
// cloud engine up to dial in.
func (i *Integration) handleEngageVendor(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	agentId := strings.TrimSpace(asString(args["agentId"]))
	if agentId == "" {
		return nil, fmt.Errorf("avatardirect.engageVendor: agentId is required")
	}
	roomName := strings.TrimSpace(asString(args["room_name"]))
	if roomName == "" {
		return nil, fmt.Errorf("avatardirect.engageVendor: room_name is required (from startSession)")
	}
	browserIdentity := strings.TrimSpace(asString(args["browser_identity"]))
	if browserIdentity == "" {
		return nil, fmt.Errorf("avatardirect.engageVendor: browser_identity is required (from startSession)")
	}
	spaceId := strings.TrimSpace(asString(args["spaceId"]))

	if !i.lk.LiveKitConfigured() {
		return nil, fmt.Errorf("avatardirect.engageVendor: LiveKit not configured (set POLYPHON_LIVEKIT_URL/API_KEY/API_SECRET)")
	}

	vendor, personaId, ac, err := i.resolveVendorConfig(ctx, agentId)
	if err != nil {
		return nil, err
	}

	// The avatar token carries lk.publish_on_behalf=browser_identity. The
	// browser is already in the room (phase 1 + the browser's join), so the
	// on-behalf target exists -- setting it before the participant was present
	// is what made Simli refuse to join (memql#782).
	avatarToken, err := i.mintAvatarToken(roomName, browserIdentity)
	if err != nil {
		return nil, fmt.Errorf("avatardirect.engageVendor: mint avatar token: %w", err)
	}

	// engineURL is where the vendor's CLOUD engine dials in from the public
	// internet. A browser-local URL (livekit.local / 192.168.x /
	// ws://livekit:7880) is unreachable from the cloud, so this MUST be an
	// externally-reachable tunnel -- LIVEKIT_PUBLIC_URL (the same env the
	// voice-agent avatar uses; set by `make dev-refresh`'s ngrok step). Without
	// it the engine can't join the room and never publishes video, so fall back
	// to the browser URL but log a warning.
	browserURL := i.lk.LiveKitPublicURL
	if browserURL == "" {
		browserURL = i.lk.LiveKitURL
	}
	engineURL := strings.TrimSpace(os.Getenv("LIVEKIT_PUBLIC_URL"))
	if engineURL == "" {
		engineURL = browserURL
		if i.logger != nil {
			i.logger.Warn("avatardirect: LIVEKIT_PUBLIC_URL unset; the vendor cloud engine will get the browser-local URL and likely cannot dial in (avatar won't render). Set LIVEKIT_PUBLIC_URL to a publicly-reachable LiveKit tunnel.",
				"engineURL", engineURL, "vendor", vendor)
		}
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
		LiveKitURL:   engineURL,
		LiveKitToken: avatarToken,
	}, i.newClient)
	if err != nil {
		// On the local dev path the engine URL is an ngrok tunnel and the only
		// way bring-up fails after signaling works is the WebRTC MEDIA leg never
		// connecting: the cloud engine joins LiveKit signaling but the dockerized
		// LiveKit advertises only its docker host ICE candidate and the cloud
		// engine forms no relay candidate, so ICE has no reachable pair and the
		// vendor's join-and-publish POST never returns before our deadline
		// (memql#1277). That is an intrinsic dev-tunnel limitation, NOT a bug to
		// retry -- surface it as a definitive, documented error instead of the
		// vendor's cryptic "context deadline exceeded".
		if isDevTunnelURL(engineURL) && isMediaTimeoutErr(err) {
			return nil, fmt.Errorf("avatardirect.engageVendor: %s cloud avatar joined LiveKit signaling but could not establish WebRTC media to the local dev LiveKit over the ngrok TURN relay -- local cloud-avatar VIDEO is not supported on the dev cluster (the cloud engine and the dockerized LiveKit only exchange unreachable host ICE candidates; the cloud engine forms no relay candidate from the advertised TURN server). Run the direct avatar audio-only locally; cloud-avatar video is validated on staging (memql#784), where LiveKit is directly reachable. See docs/internal/design/voice-turn-relay.md. (underlying: %w)", vendor, err)
		}
		// A local-only engine URL (the *.local.znas.io front door, localhost,
		// or an in-cluster service name) is unreachable from the vendor's
		// cloud BY CONSTRUCTION -- the engine never even joins signaling. Name
		// the misconfiguration instead of surfacing the vendor's dial timeout
		// (memql#1336): on the dev cluster this is the documented no-tunnel
		// state; anywhere else it means LIVEKIT_PUBLIC_URL is missing.
		if isLocalOnlyURL(engineURL) {
			return nil, fmt.Errorf("avatardirect.engageVendor: the %s cloud engine was given the locally-scoped LiveKit URL %q, which is unreachable from the vendor's cloud -- set LIVEKIT_PUBLIC_URL to a publicly-reachable LiveKit endpoint (staging serves one; locally use the ngrok overlay via scripts/dev/ngrok-up.sh, noting local cloud-avatar VIDEO stays unsupported per memql#1277). (underlying: %w)", vendor, engineURL, err)
		}
		return nil, fmt.Errorf("avatardirect.engageVendor: bring up %s avatar: %w", vendor, err)
	}
	if !started {
		return nil, fmt.Errorf("avatardirect.engageVendor: agent %s has no avatar persona for vendor %q (audio-only)", agentId, vendor)
	}

	if i.logger != nil {
		i.logger.Info("avatardirect engageVendor (phase 2: vendor engine started)",
			"agent_id", agentId,
			"space_id", spaceId,
			"vendor", vendor,
			"room_name", roomName,
			"browser_identity", browserIdentity,
			"session_id", res.SessionID,
			"avatar_identity", res.AvatarIdentity)
	}

	out := map[string]any{
		"session_id":      res.SessionID,
		"vendor":          vendor,
		"avatar_identity": res.AvatarIdentity,
		"ok":              true,
	}
	return wrapResult("avatardirect:engage-vendor", "integration:avatardirect:engageVendor", out)
}

// resolveVendorConfig resolves the agent's avatar vendor + persona id and builds
// the AvatarConfig with the matching vendor API key. Shared by both phases of
// the handshake (startSession validates the plan; engageVendor starts it). The
// vendor gate accepts only the cloud-engine vendors anam and simli.
func (i *Integration) resolveVendorConfig(ctx context.Context, agentId string) (vendor, personaId string, ac avatarvendor.AvatarConfig, err error) {
	vendor, personaId, err = i.resolveAgentAvatar(ctx, agentId)
	if err != nil {
		return "", "", avatarvendor.AvatarConfig{}, err
	}
	ac = avatarvendor.AvatarConfig{Vendor: vendor}
	switch vendor {
	case string(avatarvendor.AvatarVendor("anam")):
		key, kerr := i.vendorAPIKey(ctx, secretAnamAPIKey)
		if kerr != nil {
			return "", "", avatarvendor.AvatarConfig{}, kerr
		}
		ac.AnamAPIKey = key
	case string(avatarvendor.AvatarVendor("simli")):
		key, kerr := i.vendorAPIKey(ctx, secretSimliAPIKey)
		if kerr != nil {
			return "", "", avatarvendor.AvatarConfig{}, kerr
		}
		ac.SimliAPIKey = key
	default:
		return "", "", avatarvendor.AvatarConfig{}, fmt.Errorf("avatardirect: agent %s avatarVendor=%q is not supported on the direct avatar path (expected anam or simli)", agentId, vendor)
	}
	return vendor, personaId, ac, nil
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

// avatarPersonaCatalogPrefix is the canonical id prefix of the operator
// persona catalog rows (v1:agents:avatarPersona). The CoPresent PersonaPicker
// stamps the agent's avatarPersonaId with the CATALOG ROW ID it picked, not
// the vendor-issued face/persona id, so a stamped value carrying this prefix
// must be hydrated through the catalog before it reaches the vendor client
// (resolveSimliPlan treats AvatarPersonaID as the raw Simli faceId, memql#1336).
const avatarPersonaCatalogPrefix = "v1:agents:avatarPersona:"

// resolveAgentAvatar resolves the {vendor, vendor-issued persona id} pair the
// vendor client needs for the given agent (memql#1336):
//
//   - Agent stamped with a CATALOG row id -> hydrate via queryAvatarPersonaById
//     to the entry's vendor + vendor-issued personaId. A dangling reference is
//     a hard error naming the missing row.
//   - Agent stamped with a raw vendor id (legacy direct stamping) -> verbatim.
//   - Agent unstamped (every auto-provisioned assistant -- the PersonaPicker
//     only mounts in the create/edit assistant modal, which a user with an
//     auto-provisioned GA never visits) -> fall back to the operator catalog
//     default: the active persona matching the agent's gender, else the first
//     active entry. Only an EMPTY catalog is an error.
//
// The agent lookup uses the named queryAgentById (the agentFull shape projects
// avatarVendor + avatarPersonaId + gender, memql #692/#721). Named queries
// honor the no-raw-DSL contract and avoid the parser choking on a
// `?.`-prefixed colon-bearing id literal.
func (i *Integration) resolveAgentAvatar(ctx context.Context, agentId string) (vendor, personaId string, err error) {
	if i.engine == nil {
		return "", "", fmt.Errorf("avatardirect: no engine configured")
	}
	agentIdJSON, _ := json.Marshal(agentId)
	q := fmt.Sprintf(`queryAgentById({agentId: %s})`, string(agentIdJSON))
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

	if strings.HasPrefix(personaId, avatarPersonaCatalogPrefix) {
		return i.hydrateCatalogPersona(ctx, agentId, personaId)
	}
	if vendor != "" && personaId != "" {
		return vendor, personaId, nil
	}
	return i.defaultCatalogPersona(ctx, agentId, strings.TrimSpace(rowString(row, "gender")))
}

// hydrateCatalogPersona resolves a stamped catalog row id to the entry's
// {vendor, vendor-issued personaId}.
func (i *Integration) hydrateCatalogPersona(ctx context.Context, agentId, catalogId string) (string, string, error) {
	catalogIdJSON, _ := json.Marshal(catalogId)
	raw, err := i.engine.Execute(systemActorContext(ctx), fmt.Sprintf(`queryAvatarPersonaById({avatarPersonaId: %s})`, string(catalogIdJSON)))
	if err != nil {
		return "", "", fmt.Errorf("avatardirect: hydrate persona %s for agent %s: %w", catalogId, agentId, err)
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return "", "", fmt.Errorf("avatardirect: agent %s references avatar persona %s which does not exist in the catalog", agentId, catalogId)
	}
	vendor := strings.TrimSpace(rowString(rows[0], "vendor"))
	personaId := strings.TrimSpace(rowString(rows[0], "personaId"))
	if vendor == "" || personaId == "" {
		return "", "", fmt.Errorf("avatardirect: catalog persona %s carries an incomplete vendor/personaId pair", catalogId)
	}
	i.logInfo("avatardirect: hydrated catalog persona", "agent_id", agentId, "catalog_id", catalogId, "vendor", vendor)
	return vendor, personaId, nil
}

// defaultCatalogPersona picks the catalog default for an unstamped agent:
// the active entry matching the agent's gender, else the first active entry.
func (i *Integration) defaultCatalogPersona(ctx context.Context, agentId, gender string) (string, string, error) {
	raw, err := i.engine.Execute(systemActorContext(ctx), `queryAvatarPersonas({})`)
	if err != nil {
		return "", "", fmt.Errorf("avatardirect: agent %s has no stamped avatar persona and the catalog lookup failed: %w", agentId, err)
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return "", "", fmt.Errorf("avatardirect: agent %s has no stamped avatar persona and the avatar persona catalog is empty", agentId)
	}
	pick := rows[0]
	if gender != "" {
		for _, row := range rows {
			if strings.EqualFold(strings.TrimSpace(rowString(row, "gender")), gender) {
				pick = row
				break
			}
		}
	}
	vendor := strings.TrimSpace(rowString(pick, "vendor"))
	personaId := strings.TrimSpace(rowString(pick, "personaId"))
	if vendor == "" || personaId == "" {
		return "", "", fmt.Errorf("avatardirect: catalog default persona %s carries an incomplete vendor/personaId pair", rowString(pick, "id"))
	}
	i.logInfo("avatardirect: agent has no stamped persona; using catalog default",
		"agent_id", agentId, "catalog_id", rowString(pick, "id"), "vendor", vendor, "gender", gender)
	return vendor, personaId, nil
}

func (i *Integration) logInfo(msg string, args ...any) {
	if i.logger != nil {
		i.logger.Info(msg, args...)
	}
}

// -----------------------------------------------------------------
// LiveKit token minting
// -----------------------------------------------------------------

// mintBrowserToken builds the LiveKit join token copresent connects with. It is
// kind=AGENT: Simli's cloud receiver lip-syncs the audio of whichever AGENT-kind
// participant it finds in the room (DataStreamAudioReceiver waits for an
// agent-kind participant and reads its lk.audio_stream byte stream), so the
// browser -- which forwards the assistant audio over that byte stream -- must
// present as an agent participant for the engine to pick it up (memql#782). It
// can publish (the audio byte stream) and subscribe (the avatar's video).
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
		SetKind(livekit.ParticipantInfo_AGENT).
		SetIdentity(identity).
		SetName("You").
		SetValidFor(tokenTTL)
	return at.ToJWT()
}

// mintAvatarToken builds the LiveKit join token the vendor engine dials in with.
// kind=agent so it is recognized as an agent participant; it publishes the
// lip-synced video the browser subscribes to. onBehalfIdentity, when non-empty,
// is stamped as lk.publish_on_behalf so the avatar's video track is attributed
// to the audio-providing participant -- mirroring the voice-agent path's
// ATTRIBUTE_PUBLISH_ON_BEHALF. It must reference a participant already in the
// room (the browser, joined in phase 1); setting it for an absent participant
// makes the engine refuse to join (memql#782).
func (i *Integration) mintAvatarToken(roomName, onBehalfIdentity string) (string, error) {
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
	if strings.TrimSpace(onBehalfIdentity) != "" {
		at.SetAttributes(map[string]string{"lk.publish_on_behalf": onBehalfIdentity})
	}
	return at.ToJWT()
}

// -----------------------------------------------------------------
// Config resolution
// -----------------------------------------------------------------

// vendorAPIKey resolves a vendor REST key (secretName, e.g. ANAM_API_KEY /
// SIMLI_API_KEY) from the globalSecret store, falling back to the process env
// so a single-node dev box that exports the key works without a globalSecret
// row.
func (i *Integration) vendorAPIKey(ctx context.Context, secretName string) (string, error) {
	if i.resolveSecret != nil {
		if v, err := i.resolveSecret(ctx, secretName); err == nil {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed, nil
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv(secretName)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("avatardirect: %s not found in v1:platform:globalSecret or env -- run `make secret-set NAME=%s VALUE=... SCOPE=global KIND=integration`", secretName, secretName)
}

// -----------------------------------------------------------------
// helpers
// -----------------------------------------------------------------

// isDevTunnelURL reports whether engineURL points at a dev ngrok tunnel (the
// local cloud-avatar relay path), as opposed to a directly-reachable
// staging/prod LiveKit. Cloud-avatar VIDEO media cannot complete over the dev
// tunnel (memql#1277), so a media-establishment timeout there is an expected,
// documented limitation -- "ngrok" in the host cleanly separates dev from a
// real staging domain with no false positives.
func isDevTunnelURL(engineURL string) bool {
	return strings.Contains(engineURL, "ngrok")
}

// isLocalOnlyURL reports whether engineURL can only resolve/route on the
// developer's machine or LAN -- localhost, the docker service name, or the
// *.local.znas.io dev front door -- i.e. a URL a vendor's CLOUD engine can
// never dial. Used to name the misconfiguration instead of surfacing the
// vendor's opaque dial timeout (memql#1336).
func isLocalOnlyURL(engineURL string) bool {
	s := strings.ToLower(engineURL)
	return strings.Contains(s, "localhost") ||
		strings.Contains(s, "127.0.0.1") ||
		strings.Contains(s, ".local.znas.io") ||
		strings.Contains(s, "//livekit:")
}

// isMediaTimeoutErr reports whether err is the vendor-join timeout shape a
// failed local WebRTC media leg produces (the cloud engine joins signaling,
// retries ICE, and the vendor REST POST never returns before our deadline). It
// deliberately does NOT match a fast 4xx/5xx (bad key, bad face id) so those
// still surface as themselves rather than the dev-tunnel media hint.
func isMediaTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadline") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "awaiting headers")
}

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
