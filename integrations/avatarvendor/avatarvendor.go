package avatarvendor

// avatarvendor.go is the audio-source-agnostic, CGO-free avatar-vendor core,
// extracted from the voice-tagged `integrations/voice/agent` package so BOTH
// the voice-agent (via the voice-tagged LiveKit room glue) AND the direct/Guide
// avatar capability (#237 step 2, a core plugin) can mint Anam / Simli sessions
// from the same code.
//
// The whole vendor REST/dispatch layer is PURE Go (CGO-free, build-tag-free) so
// it builds and unit-tests in the default CI lane. Only the LiveKit
// room/participant glue stays `//go:build voice` in the voice package; this
// package never touches the media plane -- it deals in plans, REST calls, and a
// vendor-neutral start result.
//
// The source-agnostic invariant (avatar_drive.py) lives where the audio sink
// is wrapped (the voice room glue / the direct-path capability): the assistant
// PCM is forwarded to the avatar participant over a LiveKit byte data-stream so
// the avatar lip-syncs whichever executor produced the frames. This package
// only resolves the vendor session that the avatar joins under.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// AvatarRequestTimeout bounds each vendor REST call (session-token mint,
// persona lookup, engine-session start). Generous for a cold cloud call but
// bounded so a wedged vendor can't hang session bring-up. This is the DEFAULT;
// override with MEMQL_AVATAR_REQUEST_TIMEOUT_SECONDS via avatarRequestTimeout().
//
// 30s (was 15s): Simli's POST /integrations/livekit/agents -- which spins up an
// avatar agent and joins the LiveKit room before responding -- routinely exceeds
// 15s when the room is reached over a dev ngrok tunnel (memql#1274). On a real
// public LiveKit (staging/prod) it returns fast, so the higher cap only matters
// on the slow local path; a wedged vendor is still bounded.
const AvatarRequestTimeout = 30 * time.Second

// avatarRequestTimeout returns the per-call REST timeout: the env override
// MEMQL_AVATAR_REQUEST_TIMEOUT_SECONDS (1..300s) when set + valid, else
// AvatarRequestTimeout. Lets a slow local tunnel be tuned without a rebuild.
func avatarRequestTimeout() time.Duration {
	if s := strings.TrimSpace(os.Getenv("MEMQL_AVATAR_REQUEST_TIMEOUT_SECONDS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 300 {
			return time.Duration(n) * time.Second
		}
	}
	return AvatarRequestTimeout
}

// HTTPDoer is the minimal HTTP surface the vendor clients depend on. The real
// clients use *http.Client; tests inject a stub so the request shape (URL,
// headers, JSON body) is asserted without a network round-trip. Mirrors the
// doer seam the voice provider clients use.
type HTTPDoer interface {
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

	// AudioStreamTopic is the LiveKit byte-stream topic the avatar participant
	// subscribes to for the assistant's PCM (LiveKit Agents' DataStreamAudioOutput
	// AUDIO_STREAM_TOPIC). The forwarding sink (voice room glue / direct-path)
	// opens a byte stream on this topic addressed to the avatar identity.
	AudioStreamTopic = "lk.audio_stream"

	// RPCClearBuffer is the LiveKit RPC method the avatar exposes to drop any
	// queued-but-unplayed audio on a barge-in (DataStreamAudioOutput.clear_buffer
	// -> "lk.clear_buffer"). The forwarding sink calls it on Flush so an
	// interruption cuts the avatar's speech immediately, not after the buffered
	// tail drains.
	RPCClearBuffer = "lk.clear_buffer"

	// DefaultPCMSampleRate is the sample rate stamped on the audio byte-stream
	// header so the avatar engine resamples our PCM correctly. It matches the
	// rate the producers publish -- the voice cascade TTS emits linear16 at this
	// rate (ttsPCMSampleRate), and the realtime executor + the direct-path
	// browser audio republish into the same sink, so a single rate covers them
	// all. Previously coupled to the voice package's ttsPCMSampleRate; this
	// package owns its own const so it stays voice-dependency-free.
	DefaultPCMSampleRate = 16000

	// AvatarParticipantIdentity / AvatarParticipantName are the room identity
	// the avatar joins under. Fixed (one avatar per session) and excluded from
	// STT so the agent never transcribes the avatar's republished audio.
	AvatarParticipantIdentity = "avatar-agent"
	AvatarParticipantName     = "Assistant"
)

// AvatarPlan is the resolved, vendor-agnostic decision of HOW (or whether) to
// drive an avatar for one session. It is produced by ResolveAvatarPlan from the
// AvatarConfig + a PersonaInput, entirely without network or media
// dependencies, so the dispatch rules (persona-stamped vendor wins, runtime
// default fallback, video-gating -> no avatar) are unit-tested in the default
// lane.
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

// AvatarConfig is the slice of config the avatar layer reads. It is a narrow
// view (not a whole process Config) so ResolveAvatarPlan stays trivially
// testable and the dispatch rules read cleanly. The voice package and the
// direct-path capability each project their own config onto it.
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

// PersonaInput is the narrow, voice-type-free view of the resolved persona the
// dispatch layer reads. It decouples this package from the voice agent's rich
// Persona type: callers project their own persona representation onto these
// three fields. VideoEnabled is the already-resolved video gate (the voice
// persona resolver / the direct-path video-control lookup computes it).
type PersonaInput struct {
	// AvatarPersonaID is the per-agent stamped Anam personaId / Simli faceId.
	AvatarPersonaID string

	// AvatarVendor is the per-agent stamped vendor ("anam" / "simli" / ""); it
	// wins over the runtime default when set.
	AvatarVendor string

	// VideoEnabled is the resolved video gate. False -> no avatar (audio-only).
	VideoEnabled bool
}

// AvatarStartResult is what a vendor client returns after minting + starting
// its session: the vendor session id (for logging / teardown) and the LiveKit
// participant identity the avatar joined under (the byte-stream destination the
// forwarding sink targets). Vendor-neutral so the room glue stays
// dispatch-free.
type AvatarStartResult struct {
	SessionID         string
	AvatarIdentity    string
	LiveKitSampleRate int
}

// AvatarVendorClient is the vendor-specific REST integration: given a minted
// LiveKit join token for the avatar participant, mint the vendor session and
// instruct the vendor's cloud engine to join the room. The CGO-free vendor
// clients (anamClient / simliClient) satisfy it; the caller (voice room glue /
// direct-path capability) constructs the right one from the AvatarPlan and
// supplies the LiveKit token.
//
// Start does NOT touch the media plane -- it is pure REST -- so the whole
// vendor surface is unit-tested against a stub HTTPDoer in the default lane.
type AvatarVendorClient interface {
	// Start mints the vendor session and tells the vendor engine to join the
	// LiveKit room named roomName using livekitURL + livekitToken (the token
	// minted for the avatar participant identity). It returns the started
	// session details. Errors are returned for the caller to treat as
	// non-fatal (fall back to audio-only).
	Start(ctx context.Context, roomName, livekitURL, livekitToken string) (AvatarStartResult, error)
}

// NewAvatarVendorClient constructs the vendor REST client for a resolved plan.
// doer may be nil, in which case a default *http.Client with AvatarRequestTimeout
// is used. The room glue passes nil; tests pass a stub.
func NewAvatarVendorClient(plan AvatarPlan, doer HTTPDoer) (AvatarVendorClient, error) {
	if doer == nil {
		doer = &http.Client{Timeout: avatarRequestTimeout()}
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

// AvatarStartParams carries everything StartAvatarSession needs to bring an
// avatar up, decoupled from the LiveKit media types so the start/fallback
// decision is unit-testable in the default lane.
type AvatarStartParams struct {
	RoomName     string
	LiveKitURL   string
	LiveKitToken string
}

// StartAvatarSession is the CGO-free core of the avatar-start seam (the testable
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
// cascade and the realtime executor (and the direct-path browser republish)
// write into, the avatar lip-syncs whichever produced the frames without this
// code ever branching on the source.
func StartAvatarSession(
	ctx context.Context,
	ac AvatarConfig,
	persona PersonaInput,
	params AvatarStartParams,
	newClient func(plan AvatarPlan) (AvatarVendorClient, error),
) (AvatarStartResult, bool, error) {
	plan, err := ResolveAvatarPlan(ac, persona)
	if err != nil {
		return AvatarStartResult{}, false, err
	}
	if plan == nil {
		return AvatarStartResult{}, false, nil
	}
	if newClient == nil {
		newClient = func(p AvatarPlan) (AvatarVendorClient, error) {
			return NewAvatarVendorClient(p, nil)
		}
	}
	client, err := newClient(*plan)
	if err != nil {
		return AvatarStartResult{}, false, err
	}
	res, err := client.Start(ctx, params.RoomName, params.LiveKitURL, params.LiveKitToken)
	if err != nil {
		return AvatarStartResult{}, false, err
	}
	if res.LiveKitSampleRate <= 0 {
		res.LiveKitSampleRate = DefaultPCMSampleRate
	}
	if res.AvatarIdentity == "" {
		res.AvatarIdentity = AvatarParticipantIdentity
	}
	return res, true, nil
}

// doJSON performs one JSON HTTP request against the vendor API and decodes the
// response into out (when non-nil). It centralizes the marshal / header /
// status-check / decode dance the vendor clients share, matching the
// liveavatar integration's callAPI helper. applyAuth stamps the vendor's auth
// header (Bearer / x-simli-api-key) so the caller stays declarative.
func doJSON(ctx context.Context, doer HTTPDoer, method, url string, body any, applyAuth func(*http.Request), out any) error {
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
