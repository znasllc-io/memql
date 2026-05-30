package agent

// avatar.go is the audio-source-agnostic avatar core for the Go voice-agent,
// the port of the Python voice-agent's avatar_drive.py plus the shared HTTP
// plumbing the vendor clients (avatar_anam.go / avatar_simli.go) ride on.
//
// The whole vendor REST/dispatch layer is PURE Go (CGO-free) so it builds and
// unit-tests in the default CI lane; only the LiveKit room/participant glue
// (avatar_room_voice.go) is `//go:build voice`. The split mirrors the rest of
// the package: the wire-protocol logic is testable without standing up a
// libopus media stack, the media-plane binding is not.
//
// The source-agnostic invariant (avatar_drive.py): both executors -- the
// cascade TTS loop (#455) today and the realtime model (#457) later -- write
// the assistant's PCM into the SAME audioSink the cascade publishes from. The
// avatar lip-syncs by INTERCEPTING that sink: instead of (or in addition to)
// the local Opus track, the assistant's PCM frames are forwarded over a
// LiveKit byte data-stream to the avatar participant, which runs lip-sync and
// republishes audio + video. Because the interception is at the sink the
// executors share, the avatar never branches on which executor produced the
// frames -- exactly the Python `output.audio` reassignment, expressed in Go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// avatarRequestTimeout bounds each vendor REST call (session-token mint,
// persona lookup, engine-session start). Generous for a cold cloud call but
// bounded so a wedged vendor can't hang session bring-up. Mirrors the Python
// APIConnectOptions(timeout=10s) the LiveKit plugins ship.
const avatarRequestTimeout = 15 * time.Second

// httpDoer is the minimal HTTP surface the vendor clients depend on. The real
// clients use *http.Client; tests inject a stub so the request shape (URL,
// headers, JSON body) is asserted without a network round-trip. Mirrors the
// doer seam the deepgram clients use.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// AvatarVendor is the resolved vendor name an AvatarPlan drives.
type AvatarVendor string

const (
	avatarVendorAnam  AvatarVendor = "anam"
	avatarVendorSimli AvatarVendor = "simli"

	// anamClientAudioLLM is the Anam llmId that disables Anam's own LLM/TTS so
	// the avatar lip-syncs OUR audio (the ephemeral client-audio path). See
	// anam_persona_session.py: with this id Anam runs lip-sync on the PCM we
	// publish rather than speaking with its own bundled voice.
	anamClientAudioLLM = "CUSTOMER_CLIENT_V1"

	// anamAPIBase / simliAPIBase are the vendor REST roots. Both are
	// overridable per-plan (avatar_url) so a test or a self-hosted deployment
	// can point elsewhere; these are the production defaults.
	anamAPIBase  = "https://api.anam.ai"
	simliAPIBase = "https://api.simli.ai"

	// audioStreamTopic is the LiveKit byte-stream topic the avatar participant
	// subscribes to for the assistant's PCM (LiveKit Agents' DataStreamAudioOutput
	// AUDIO_STREAM_TOPIC). The forwarding sink (avatar_room_voice.go) opens a
	// byte stream on this topic addressed to the avatar identity.
	audioStreamTopic = "lk.audio_stream"

	// rpcClearBuffer is the LiveKit RPC method the avatar exposes to drop any
	// queued-but-unplayed audio on a barge-in (DataStreamAudioOutput.clear_buffer
	// -> "lk.clear_buffer"). The forwarding sink calls it on Flush so an
	// interruption cuts the avatar's speech immediately, not after the buffered
	// tail drains.
	rpcClearBuffer = "lk.clear_buffer"

	// avatarPCMSampleRate is the sample rate stamped on the audio byte-stream
	// header so the avatar engine resamples our PCM correctly. It matches the
	// rate the executor publishes -- the cascade TTS emits linear16 at
	// ttsPCMSampleRate today, and the realtime executor (#457) writes into the
	// same sink, so a single rate covers both. Kept equal to ttsPCMSampleRate
	// (tts_pipeline.go) so the source-agnostic invariant holds without the
	// avatar layer knowing which executor is active.
	avatarPCMSampleRate = ttsPCMSampleRate

	// avatarParticipantIdentity / avatarParticipantName are the room identity
	// the avatar joins under. Fixed (one avatar per session) and excluded from
	// STT so the agent never transcribes the avatar's republished audio.
	avatarParticipantIdentity = "avatar-agent"
	avatarParticipantName     = "Assistant"
)

// AvatarPlan is the resolved, vendor-agnostic decision of HOW (or whether) to
// drive an avatar for one session. It is produced by ResolveAvatarPlan from the
// Config + resolved Persona, entirely without network or media dependencies,
// so the dispatch rules (persona-stamped vendor wins, runtime default
// fallback, video-gating -> no avatar) are unit-tested in the default lane.
//
// A nil *AvatarPlan means "no avatar -- ride audio-only", the explicit signal
// the Python build_avatar(...) returns None for.
type AvatarPlan struct {
	// Vendor is the resolved vendor that will render this session.
	Vendor AvatarVendor

	// PersonaID is the vendor persona/face id. For Anam this is the personaId
	// whose avatarId is fetched and rendered with our audio; for Simli it is
	// the faceId. Empty only for the Anam bare-avatarId default path.
	PersonaID string

	// AvatarID is the Anam bare avatar (face-model) id for the default
	// ephemeral path (ANAM_DEFAULT_AVATAR_ID) when no personaId is available.
	// Unused by Simli.
	AvatarID string

	// DisplayName is the ephemeral persona name shown for the avatar
	// (ANAM_DEFAULT_PERSONA_NAME). Vendor-cosmetic.
	DisplayName string

	// APIKey is the vendor API key the REST calls authenticate with.
	APIKey string

	// APIBase is the vendor REST root. Defaulted by ResolveAvatarPlan to the
	// vendor production base; overridable for tests / self-hosting.
	APIBase string
}

// AvatarConfig is the slice of the agent Config the avatar layer reads. It is a
// narrow view (not the whole Config) so ResolveAvatarPlan stays trivially
// testable and the dispatch rules read cleanly. Populated from Config by
// avatarConfigFromConfig.
type AvatarConfig struct {
	// Vendor is the runtime default vendor (MEMQL_AVATAR_VENDOR): "anam" |
	// "simli" | "none". Used only when the persona carries no stamped vendor.
	Vendor string

	AnamAPIKey  string
	SimliAPIKey string

	// AnamDefaultPersonaID / AnamDefaultAvatarID / AnamDefaultPersonaName are
	// the platform-wide Anam fallbacks used when the agent record carries no
	// stamped avatar persona id.
	AnamDefaultPersonaID  string
	AnamDefaultAvatarID   string
	AnamDefaultPersonaNam string
}

// avatarConfigFromConfig projects the process Config onto the narrow
// AvatarConfig the dispatch layer consumes.
func avatarConfigFromConfig(cfg Config) AvatarConfig {
	return AvatarConfig{
		Vendor:                cfg.AvatarVendor,
		AnamAPIKey:            cfg.AnamAPIKey,
		SimliAPIKey:           cfg.SimliAPIKey,
		AnamDefaultPersonaID:  cfg.AnamDefaultPersonaID,
		AnamDefaultAvatarID:   cfg.AnamDefaultAvatarID,
		AnamDefaultPersonaNam: cfg.AnamDefaultPersonaNm,
	}
}

// avatarStartResult is what a vendor client returns after minting + starting
// its session: the vendor session id (for logging / teardown) and the LiveKit
// participant identity the avatar joined under (the byte-stream destination
// the forwarding sink targets). Vendor-neutral so the room glue stays
// dispatch-free.
type avatarStartResult struct {
	SessionID         string
	AvatarIdentity    string
	LiveKitSampleRate int
}

// avatarVendorClient is the vendor-specific REST integration: given a minted
// LiveKit join token for the avatar participant, mint the vendor session and
// instruct the vendor's cloud engine to join the room. The CGO-free vendor
// clients (anamClient / simliClient) satisfy it; the voice-tagged room glue
// constructs the right one from the AvatarPlan and supplies the LiveKit token.
//
// Start does NOT touch the media plane -- it is pure REST -- so the whole
// vendor surface is unit-tested against a stub httpDoer in the default lane.
type avatarVendorClient interface {
	// Start mints the vendor session and tells the vendor engine to join the
	// LiveKit room named roomName using livekitURL + livekitToken (the token
	// minted for the avatar participant identity). It returns the started
	// session details. Errors are returned for the caller to treat as
	// non-fatal (fall back to audio-only).
	Start(ctx context.Context, roomName, livekitURL, livekitToken string) (avatarStartResult, error)
}

// newAvatarVendorClient constructs the vendor REST client for a resolved plan.
// doer may be nil, in which case a default *http.Client with avatarRequestTimeout
// is used. The room glue passes nil; tests pass a stub.
func newAvatarVendorClient(plan AvatarPlan, doer httpDoer) (avatarVendorClient, error) {
	if doer == nil {
		doer = &http.Client{Timeout: avatarRequestTimeout}
	}
	switch plan.Vendor {
	case avatarVendorAnam:
		return &anamClient{plan: plan, doer: doer}, nil
	case avatarVendorSimli:
		return &simliClient{plan: plan, doer: doer}, nil
	default:
		return nil, fmt.Errorf("avatar: unknown vendor %q", plan.Vendor)
	}
}

// avatarStartParams carries everything startAvatarSession needs to bring an
// avatar up, decoupled from the LiveKit media types so the start/fallback
// decision is unit-testable in the default lane.
type avatarStartParams struct {
	RoomName     string
	LiveKitURL   string
	LiveKitToken string
}

// startAvatarSession is the CGO-free core of the avatar-start seam (the testable
// half of avatar_drive.py::start_avatar). It resolves the plan, constructs the
// vendor client (via newClient, overridable in tests), and starts the vendor
// session, returning the started-session result and whether an avatar is now
// active.
//
// The audio-source-agnostic / non-fatal contract lives here so it is provable
// without a live room:
//   - plan nil (avatar disabled / video gated / no usable persona) -> (zero,
//     false, nil): the caller rides audio-only, no error.
//   - ResolveAvatarPlan config mismatch -> (zero, false, err): surfaced for the
//     caller to log and treat as audio-only (non-fatal at the call site).
//   - vendor Start failure -> (zero, false, err): same non-fatal treatment --
//     the assistant keeps talking on its own audio track.
//
// On success the caller wraps the executor's audio sink so the assistant's PCM
// is forwarded to res.AvatarIdentity; because that sink is the one BOTH the
// cascade (#455) and the realtime executor (#457) write into, the avatar
// lip-syncs whichever produced the frames without this code ever branching on
// the executor.
func startAvatarSession(
	ctx context.Context,
	ac AvatarConfig,
	persona Persona,
	params avatarStartParams,
	newClient func(plan AvatarPlan) (avatarVendorClient, error),
) (avatarStartResult, bool, error) {
	plan, err := ResolveAvatarPlan(ac, persona)
	if err != nil {
		return avatarStartResult{}, false, err
	}
	if plan == nil {
		return avatarStartResult{}, false, nil
	}
	if newClient == nil {
		newClient = func(p AvatarPlan) (avatarVendorClient, error) {
			return newAvatarVendorClient(p, nil)
		}
	}
	client, err := newClient(*plan)
	if err != nil {
		return avatarStartResult{}, false, err
	}
	res, err := client.Start(ctx, params.RoomName, params.LiveKitURL, params.LiveKitToken)
	if err != nil {
		return avatarStartResult{}, false, err
	}
	if res.LiveKitSampleRate <= 0 {
		res.LiveKitSampleRate = avatarPCMSampleRate
	}
	if res.AvatarIdentity == "" {
		res.AvatarIdentity = avatarParticipantIdentity
	}
	return res, true, nil
}

// doJSON performs one JSON HTTP request against the vendor API and decodes the
// response into out (when non-nil). It centralizes the marshal / header /
// status-check / decode dance the vendor clients share, matching the
// liveavatar integration's callAPI helper. applyAuth stamps the vendor's auth
// header (Bearer / x-simli-api-key) so the caller stays declarative.
func doJSON(ctx context.Context, doer httpDoer, method, url string, body any, applyAuth func(*http.Request), out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("avatar: marshal body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if applyAuth != nil {
		applyAuth(req)
	}

	resp, err := doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s -> %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 {
		return fmt.Errorf("%s %s returned empty body", method, url)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: parse json: %w", method, url, err)
	}
	return nil
}
