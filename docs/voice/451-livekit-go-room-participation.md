# LiveKit room participation + media tracks in Go -- feasibility spike (#451)

Spike deliverable for issue #451, part of the Go-rewrite epic #449
(Phase 0, "Long pole #1"). It answers the load-bearing question that
gates the whole epic:

> Can a Go process join a LiveKit room as a **media participant** --
> subscribe to participants' audio tracks (read PCM), publish an audio
> track back, and observe the active-speaker signal -- so the Go agent
> can replace the Python LiveKit Agents worker?

Status: design + feasibility verdict, grounded in the real
`github.com/livekit/server-sdk-go/v2` API surface. **No runtime code
lands with this doc** and no new dependency is added to `go.mod` (see
[Why this PR is docs-only](#9-why-this-pr-is-docs-only)). Live-room
validation (an actual LiveKit server + browser participant + STT/TTS
credentials) is a flagged follow-up -- see
[Open questions](#7-open-questions-that-need-live-validation).

> **Historical note (epic #449 complete).** The feasibility question this
> spike posed has been answered in the affirmative and shipped: the Go
> voice-agent now joins rooms as a media participant from
> `integrations/voice/agent/` (room files), and the Python LiveKit Agents
> worker it replaced has been deleted. References below to the Python
> implementation describe the spike's starting point, not the current tree.

Docs location note: the voice epic's docs already live under
`docs/voice/` (`eou-tuning.md`, `bringup-verification.md`,
`432-conductor-response-gate.md`, `433-multiparty-audio-routing.md`),
so this spike is placed alongside them.

---

## VERDICT: GO-WITH-CAVEATS

A Go process **can** join a LiveKit room as a full media participant.
The `server-sdk-go/v2` SDK exposes everything #451 requires with
first-class, named APIs:

- Room join: `lksdk.ConnectToRoom` / `ConnectToRoomWithToken`.
- Audio subscribe (Opus -> PCM16): `RoomCallback.OnTrackSubscribed`
  feeding `media.NewPCMRemoteTrack` with a `PCMRemoteTrackWriter`.
- Audio publish (PCM16 -> Opus): `media.NewPCMLocalTrack` +
  `LocalParticipant.PublishTrack`.
- Active speaker: `RoomCallback.OnSpeakersChanged`,
  `Room.ActiveSpeakers()`, plus per-participant
  `OnIsSpeakingChanged` / `OnAudioLevelChanged`.

The path is real and the symbols exist today. It is **GO-WITH-CAVEATS,
not unqualified GO**, because of three concrete, non-blocking caveats
the implementation phase (#449 core) must plan around:

1. **CGO/libopus build dependency.** The PCM decode/encode helpers
   live in `github.com/livekit/media-sdk`, which transitively requires
   the CGO Opus binding `gopkg.in/hraban/opus.v2` (and
   `livekit/amrwb-cgo`). That makes `CGO_ENABLED=1` + system `libopus`
   headers a hard build requirement for the agent binary and the CI
   image that builds it. memQL builds CGO-free today; this is a build-
   system change, not just a code change. See
   [Caveat 1](#caveat-1-cgo--libopus-build-dependency).

2. **Sample-rate conversion is mandatory.** LiveKit Opus is 48 kHz;
   OpenAI Realtime wants exactly 24 kHz PCM16 mono; Deepgram
   `linear16` is flexible but we standardize. Resampling is required
   in both directions. The SDK provides it (`WithTargetSampleRate` on
   the remote track, `media.Resample` for the general case), so this
   is plumbing, not a blocker. See
   [Section 4](#4-codecformat-reality-livekit-opus-vs-sttts).

3. **The Go SDK is a transport, not an agent framework.** Python's
   `livekit.agents.AgentSession` bundled VAD, the STT/LLM/TTS plugin
   chain, turn detection, and the RoomIO audio binding. The Go server
   SDK gives us **only the room + raw media frames**. Everything the
   `AgentSession` did for free -- VAD, endpointing, the STT/TTS wiring,
   the speak() pipeline -- becomes our code in Go. That is the actual
   scope of the epic and is expected; it is called out here so #449's
   estimate reflects it. See [Section 6](#6-what-the-go-sdk-gives-us-vs-the-python-agentsession).

None of these is a NO-GO. They are scoping facts. The capability
itself -- Go-as-media-participant -- is proven by the SDK surface
below.

---

## 1. Where Go is today

memQL currently touches LiveKit in exactly one place and only for JWT
minting. `component/polyphon/localroom.go` builds a room token:

```go
import "github.com/livekit/protocol/auth"

at := auth.NewAccessToken(p.cfg.LiveKitAPIKey, p.cfg.LiveKitAPISecret)
grant := &auth.VideoGrant{RoomJoin: true, Room: roomName}
at.SetVideoGrant(grant).SetIdentity(participantId)...
token, _ := at.ToJWT()
```

`go.mod` imports only `github.com/livekit/protocol v1.46.4` (JWT +
generated protos). It does **not** import the Go server SDK and never
joins a room -- a browser uses the minted token; the Python
voice-agent (its `main.py`) was the thing that
actually joined as a media participant at spike time, via
`livekit.agents.AgentSession`.

This spike is the bridge: replace that Python media participant with a
Go one built on `server-sdk-go/v2`.

---

## 2. The concrete Go SDK API surface

All symbols below are from `github.com/livekit/server-sdk-go/v2`
(canonical import alias `lksdk`) and its
`github.com/livekit/server-sdk-go/v2/pkg/media` subpackage (alias
`lkmedia`), verified against the published package docs. PCM samples
are typed in `github.com/livekit/media-sdk` (alias `media`).

### 2a. Joining a room

```go
import lksdk "github.com/livekit/server-sdk-go/v2"

func ConnectToRoom(url string, info ConnectInfo, callback *RoomCallback,
                   opts ...ConnectOption) (*Room, error)
func ConnectToRoomWithToken(url, token string, callback *RoomCallback,
                            opts ...ConnectOption) (*Room, error)

type ConnectInfo struct {
    APIKey              string
    APISecret           string
    RoomName            string
    ParticipantIdentity string
    // ...
}

func WithAutoSubscribe(val bool) ConnectOption   // default true
```

`ConnectToRoomWithToken` is the natural fit for memQL: we already mint
the JWT in `localroom.go`, so the Go agent reuses that exact token-
minting path (the agent's own identity, e.g. `<space>-ga`) and calls
`ConnectToRoomWithToken(livekitURL, token, cb)`. With auto-subscribe
on (the default), the SDK subscribes to remote tracks automatically
and fires `OnTrackSubscribed`.

`*Room` methods of interest:
`Room.LocalParticipant() *LocalParticipant`,
`Room.GetRemoteParticipants() []*RemoteParticipant`,
`Room.ActiveSpeakers() []*RemoteParticipant`,
`Room.Disconnect()`, `Room.ConnectionState()`.

### 2b. Subscribing to audio + reading PCM frames

The callback struct carries the track events:

```go
type RoomCallback struct {
    OnRoomJoined            func(*livekit.Room, *LocalParticipant, []*RemoteParticipant, *livekit.ServerInfo, []byte)
    OnParticipantDisconnect func(*RemoteParticipant, livekit.DisconnectReason)
    OnTrackSubscribed       func(*webrtc.TrackRemote, *RemoteTrackPublication, *RemoteParticipant)
    OnTrackUnsubscribed     func(*webrtc.TrackRemote, *RemoteTrackPublication, *RemoteParticipant)
    OnSpeakersChanged       func([]*RemoteParticipant)
    ParticipantCallback     // embedded
}
```

`OnTrackSubscribed` is the Go analogue of the Python
`ctx.room.on("track_subscribed", ...)` listener that the Python
voice-agent's `main.py` wired for diagnostics. The
raw track is a `*webrtc.TrackRemote` carrying Opus RTP. To get PCM16
out of it, wrap it in a remote PCM track from the media package:

```go
import lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
import "github.com/livekit/media-sdk" // PCM16Sample lives here

// A writer the SDK calls with decoded, resampled PCM16 frames.
type sttSink struct{ identity string /* + Deepgram stream handle */ }
func (s *sttSink) WriteSample(sample media.PCM16Sample) error { /* -> STT */ return nil }
func (s *sttSink) Close() error { return nil }

cb.OnTrackSubscribed = func(t *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
    if pub.Kind() != lksdk.TrackKindAudio { return }
    _, err := lkmedia.NewPCMRemoteTrack(t, &sttSink{identity: rp.Identity()},
        lkmedia.WithTargetSampleRate(24000),  // OpenAI Realtime / Deepgram
        lkmedia.WithTargetChannels(1),
        lkmedia.WithHandleJitter(true),
    )
    _ = err
}
```

Key types and functions:

- `lkmedia.NewPCMRemoteTrack(track *webrtc.TrackRemote, writer PCMRemoteTrackWriter, opts ...PCMRemoteTrackOption) (*PCMRemoteTrack, error)`
  -- reads the remote Opus track, decodes to PCM16, resamples to the
  target, and calls `writer.WriteSample` per frame.
- `type PCMRemoteTrackWriter interface { WriteSample(media.PCM16Sample) error; Close() error }`
- Options: `WithTargetSampleRate(int)`, `WithTargetChannels(int)`,
  `WithHandleJitter(bool)`, `WithDecryptor(Decryptor)`,
  `WithLogger(...)`.
- `type media.PCM16Sample []int16` -- the frame is a slice of signed
  16-bit little-endian samples. This is exactly the wire shape
  Deepgram `linear16` and OpenAI Realtime `pcm16` consume (after a
  trivial `int16 -> bytes` little-endian pack).

`rp.Identity()` on the `*RemoteParticipant` gives the per-track
speaker identity for attribution -- the same identity memQL stamps as
the participant id, and the same value
`_default_speaker_provider`/`TranscriptForwarder` use today. This is
what makes per-track (not best-effort) attribution possible for the
multi-party router in
[`433-multiparty-audio-routing.md`](./433-multiparty-audio-routing.md).

### 2c. Publishing audio back into the room

```go
import lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"

// 24 kHz mono PCM16 in, Opus out, published to the room.
local, err := lkmedia.NewPCMLocalTrack(24000 /*sourceSampleRate*/, 1 /*channels*/, logger)
pub, err := room.LocalParticipant.PublishTrack(local, &lksdk.TrackPublicationOptions{
    Name:   "agent-voice",
    Source: livekit.TrackSource_MICROPHONE,
})

// Feed it TTS / realtime model audio, frame by frame:
_ = local.WriteSample(media.PCM16Sample(int16Frame))
```

Key types and functions:

- `lkmedia.NewPCMLocalTrack(sourceSampleRate, sourceChannels int, logger, opts ...PCMLocalTrackOption) (*PCMLocalTrack, error)`
  -- accepts PCM16 at the source rate, **encodes to Opus internally**,
  and exposes a `webrtc.TrackLocal` to publish.
- `(*PCMLocalTrack).WriteSample(media.PCM16Sample) error` -- the push
  side. The TTS/realtime audio output gets chunked into PCM16 frames
  and written here.
- `(*LocalParticipant).PublishTrack(track webrtc.TrackLocal, opts *TrackPublicationOptions, pubOpts ...LocalTrackPublishOption) (*LocalTrackPublication, error)`.

(For the lower-level path, `lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability, ...)`
+ `track.WriteSample(media.Sample{...})` lets you publish pre-encoded
Opus directly, bypassing the media-sdk encoder -- relevant only if we
ever feed Opus straight through without a PCM hop. The PCM path above
is the right default for STT/TTS integration.)

### 2d. Active-speaker signal

There is **no** `OnActiveSpeakersChanged` field (that name is the
client/Python SDKs). In `server-sdk-go/v2` the equivalents are:

- `RoomCallback.OnSpeakersChanged func([]*RemoteParticipant)` -- fires
  with the ordered list of currently-speaking participants when the
  set changes. This is the direct analogue of the Python
  `active_speakers_changed` event used in
  [`433-multiparty-audio-routing.md`](./433-multiparty-audio-routing.md)
  Section 2d.
- `Room.ActiveSpeakers() []*RemoteParticipant` -- pull the current
  speaker list on demand.
- Per-participant push: `ParticipantCallback.OnIsSpeakingChanged func(bool)`
  and `ParticipantCallback.OnAudioLevelChanged func(float32)`; plus
  `RemoteParticipant.IsSpeaking() bool` and
  `RemoteParticipant.AudioLevel() float32` for pull.

The multi-party router (#457 / `433`) filters this list to
STANDARD-kind humans (excluding the agent's own published track and
any avatar/ingress participant) and picks index 0 as the floor-holder,
exactly as the Python design specified -- the Go symbols are a 1:1
swap for the Python event names.

---

## 3. Integration plan (file-by-file)

This is the path the implementation phase (#449 core) follows. It does
**not** land here.

### 3a. `go.mod`

Add two direct dependencies:

```
require (
    github.com/livekit/server-sdk-go/v2 v2.16.x  // room join + media
    github.com/livekit/media-sdk        v0.0.x   // PCM16Sample, Resample
)
```

`server-sdk-go/v2` transitively brings in `github.com/pion/webrtc/v4`
and (via `media-sdk`) the CGO Opus binding `gopkg.in/hraban/opus.v2`.
This forces `CGO_ENABLED=1` and a `libopus`/`libopusfile` toolchain in
the build image -- see [Caveat 1](#caveat-1-cgo--libopus-build-dependency).
`github.com/livekit/protocol` is already present and is shared.

### 3b. New package: `component/polyphon/roomagent` (proposed)

A `RoomAgent` that owns the media-participant lifecycle, mirroring what
the Python voice-agent's `main.py::entrypoint` did, but in Go:

- `Join(ctx, spaceId)` -- mints a token via the existing
  `LocalRoomProvider.GenerateToken` path (reuse `localroom.go`; the
  agent identity is `<spaceId>-ga`), then
  `lksdk.ConnectToRoomWithToken`.
- Installs a `RoomCallback`:
  - `OnTrackSubscribed` -> for each audio track from a STANDARD-kind
    human, create a `NewPCMRemoteTrack(track, sink, WithTargetSampleRate(24000), WithTargetChannels(1))`
    whose `sink.WriteSample` forwards PCM16 to that participant's STT
    stream (one per track, tagged with `rp.Identity()`).
  - `OnSpeakersChanged` -> update the active-speaker selection that the
    realtime/router path consumes.
  - `OnParticipantDisconnect` -> tear down that participant's STT
    stream.
- `PublishVoice()` -- `NewPCMLocalTrack(24000, 1, logger)` +
  `LocalParticipant.PublishTrack(...)`; expose `WriteSample` so the
  TTS / realtime output writes agent audio.

### 3c. Where the agent process joins the room

In the **cluster** topology this is the **Voice node** (per the
CoPresent `CLAUDE.md` cluster map: "BFF forwards to Voice node, which
runs the streaming STT provider"). The Voice node hosts the
`RoomAgent`; it joins the LiveKit room whose name memQL already
computes as `polyphon-<spaceId>` (`localroom.go`). The trigger is the
existing `VoiceAgentSessionStart` path -- today it acks the Python
worker; in the Go world it spins up a `RoomAgent.Join`.

### 3d. How frames flow to/from STT/TTS

```
Browser mic ──Opus 48k──▶ LiveKit room
                              │  OnTrackSubscribed
                              ▼
                  NewPCMRemoteTrack(WithTargetSampleRate(24000))
                              │  PCM16 @24k mono  (per participant, tagged identity)
                              ▼
                  Deepgram linear16 STT  /  OpenAI Realtime input_audio_buffer.append
                              │
                       (cognition / conductor: unchanged -- same VoiceAgent* surface)
                              │  reply text / realtime audio
                              ▼
                  TTS (Aura-2) or realtime model audio  ──PCM16 @24k──▶
                  PCMLocalTrack.WriteSample ──Opus 48k──▶ LiveKit room ──▶ Browser plays
```

memQL's existing `voice_agent_handlers.go` surface
(`handleVoiceAgentFinalTranscript`, `handleVoiceAgentTurnRequest`,
`VoiceAgentSpeak`) stays identical -- the room/media layer is what
changes language, not the cognition contract. This matches the
"memql server -- no change" / "conductor -- no change" rows in
`433-multiparty-audio-routing.md` Section 5.

---

## 4. Codec/format reality (LiveKit Opus vs STT/TTS)

This is the conversion matrix the integration must implement. It is
the most error-prone part and the reason the verdict is
GO-**WITH-CAVEATS**.

| Hop | Format | Rate | Channels | Conversion |
|---|---|---|---|---|
| Browser -> LiveKit room | Opus (RTP) | 48 kHz | mono | none (WebRTC) |
| LiveKit -> Go (remote track) | Opus | 48 kHz | -- | SDK decodes |
| `NewPCMRemoteTrack` output | PCM16 (`[]int16`) | **24 kHz** (target) | 1 | `WithTargetSampleRate(24000)` resamples 48k->24k |
| Go -> Deepgram STT | `linear16` PCM16 | 24 kHz | 1 | pack `[]int16` little-endian |
| Go -> OpenAI Realtime | `pcm16` | **24 kHz exactly** | 1 | same pack; rate must be 24k |
| TTS / realtime out -> Go | PCM16 | 24 kHz | 1 | provider-native |
| `PCMLocalTrack.WriteSample` in | PCM16 | 24 kHz (source) | 1 | SDK encodes to Opus 48k |
| Go -> LiveKit -> browser | Opus | 48 kHz | mono | SDK upsamples + encodes |

Concrete facts that pin the table:

- **LiveKit/WebRTC Opus is 48 kHz.** `media-sdk` exposes
  `DefaultOpusSampleRate = 48000` and `DefaultOpusSampleDuration = 20ms`.
- **OpenAI Realtime `pcm16` requires exactly 24 kHz, mono,
  16-bit little-endian.** Not a range -- 24 kHz is mandatory.
- **Deepgram `linear16`** accepts 16/24/44.1/48 kHz; we standardize on
  **24 kHz mono** so the same PCM frame feeds both STT and the realtime
  model with no second resample.
- **`media.PCM16Sample` is `[]int16`.** The pack to the byte buffer
  both providers expect is a little-endian `int16 -> 2 bytes` write;
  no float conversion, no companding.

The resampling is handled **inside** `NewPCMRemoteTrack`
(`WithTargetSampleRate`/`WithTargetChannels`) on the receive side, and
inside `NewPCMLocalTrack` (it upsamples the 24 kHz source to 48 kHz
Opus) on the send side. For any off-track resample we own,
`media.Resample(dst, dstRate, src, srcRate, ...)` from `media-sdk`
covers it. **Net: no custom DSP, no third-party resampler -- the SDK
already carries the conversion primitives.** The only "conversion" we
write by hand is the `[]int16 <-> []byte` little-endian packing at the
STT/TTS provider boundary.

---

## 5. Acceptance mapping (issue #451)

| Acceptance criterion | Where addressed / verdict |
|---|---|
| Go process joins a room, receives a participant's audio frames | **Feasible (API proven).** `ConnectToRoomWithToken` + `OnTrackSubscribed` + `NewPCMRemoteTrack` (Sections 2a, 2b). Frame-level live confirmation is the flagged follow-up (Section 7). |
| ...and publishes audio the browser plays | **Feasible (API proven).** `NewPCMLocalTrack` + `LocalParticipant.PublishTrack` (Section 2c). Browser-audible confirmation needs a live room (Section 7). |
| Active-speaker events available and documented | **Done (documented).** `OnSpeakersChanged`, `Room.ActiveSpeakers()`, `OnIsSpeakingChanged`, `OnAudioLevelChanged` (Section 2d) -- a 1:1 swap for the Python `active_speakers_changed` used in `433`. |
| Findings doc under `docs/voice/` covering Go SDK surface vs Python AgentSession | **This document** (Section 6 is the explicit comparison). |

The two "join + frames + publish + browser plays" criteria are proven
at the **API-surface level** here; the issue's own framing ("connect a
Go client to the local LiveKit room ... confirm the browser hears it")
is a live-infra exercise that, per the issue header, has **no infra
available for this spike**. The verdict and integration plan are the
deliverable; the live round-trip is the first task of the
implementation phase.

---

## 6. What the Go SDK gives us vs the Python `AgentSession`

This is the heart of the "Go SDK surface vs what the Python
AgentSession gave us" ask. The gap is real and is the bulk of the
epic's work.

| Capability | Python `livekit.agents.AgentSession` | Go `server-sdk-go/v2` |
|---|---|---|
| Room join | `ctx.connect()` (worker/job framework) | `ConnectToRoom*` (direct, we own the loop) |
| Track subscribe event | `room.on("track_subscribed", ...)` | `RoomCallback.OnTrackSubscribed` |
| Decoded audio to STT | **Automatic** via RoomIO -> `session.input.audio` | **Manual**: `NewPCMRemoteTrack` -> our `WriteSample` -> our STT client |
| VAD / endpointing | **Built in** (Silero plugin, EOU model) | **Not provided** -- we add it (Silero-Go, WebRTC VAD, or server-VAD on the realtime model) |
| STT/LLM/TTS plugin chain | **Built in** (Deepgram/memQL-LLM/Aura plugins) | **Not provided** -- we call Deepgram/OpenAI/Aura directly |
| `session.say(text)` TTS pipeline | **Built in** | **Manual**: TTS -> PCM16 -> `PCMLocalTrack.WriteSample` |
| Publish agent audio | **Automatic** via `session.output.audio` | **Manual**: `NewPCMLocalTrack` + `PublishTrack` |
| Active speaker | `room.on("active_speakers_changed", ...)` | `OnSpeakersChanged` / `ActiveSpeakers()` |
| Avatar lip-sync seam | `avatar.start(session, room, ...)` reassigns `output.audio` | We own the audio sink; avatar plugin would need a Go equivalent or stays Python |
| Subprocess-per-job worker | `cli.run_app(WorkerOptions(...))` | We own process lifecycle (fits the Voice node) |

**Takeaway.** The Go SDK is a clean, complete **media transport** --
join, subscribe, decode-to-PCM, encode-from-PCM, publish, active-
speaker. What it is **not** is the agent framework. The Python
`AgentSession` was a batteries-included orchestrator (VAD + plugin
graph + RoomIO binding + `say()` + avatar seam). Porting to Go means
**we re-own that orchestration** -- which is exactly the scope of epic
#449, and is why this is GO-WITH-CAVEATS: the capability is proven, but
"join a room" in Go is one bullet of a larger rebuild, not a drop-in.
The avatar lip-sync seam in particular (Anam/Simli, today driven via
the Python `avatar.start` reassigning `session.output.audio`) has no
Go SDK equivalent and is the single biggest open design item the epic
inherits.

---

## 7. Open questions that need live validation

These cannot be resolved without a running LiveKit server + browser
participant + STT/TTS credentials, and are the first tasks of the
implementation phase:

1. **End-to-end audible round-trip.** Confirm a real browser
   participant's mic frames reach `WriteSample` as intelligible 24 kHz
   PCM, and that audio written to `PCMLocalTrack` is actually heard in
   the browser. (The two unproven acceptance bullets.)
2. **CGO build in the real CI/release image.** Verify
   `CGO_ENABLED=1` + `libopus`/`libopusfile` build cleanly in the
   memQL build image and that the produced binary runs in the Voice
   node container. This is the gating caveat -- prove it early.
3. **Latency of the Go decode/resample path** vs the Python
   `AgentSession` baseline (per `433`/`432` latency plans). The CGO
   Opus decode + 48k->24k resample per frame must not add audible lag.
4. **Active-speaker fidelity.** Confirm `OnSpeakersChanged` ordering
   and update cadence match what the `433` dwell-timer router assumes,
   and that the agent's own published track never appears as an active
   human (the feedback-loop guard from `433` Section 4).
5. **Jitter handling.** Whether `WithHandleJitter(true)` on the remote
   track is sufficient, or we need our own jitter buffer for clean STT
   input under packet loss.
6. **Avatar lip-sync in Go.** No Go SDK seam exists; decide whether the
   avatar stays a separate (possibly Python) participant publishing on
   behalf of the agent, or a Go re-implementation. Design item, not a
   #451 blocker.

---

## 8. Verdict restatement (for the epic gate)

**GO-WITH-CAVEATS.** A Go process can be a LiveKit media participant
using `server-sdk-go/v2`; every #451 capability maps to a real,
named API. Proceed with the implementation phase, but budget for:
(1) the CGO/libopus build change, (2) re-owning the VAD + STT/TTS +
publish orchestration the Python `AgentSession` did for free, and
(3) a Go story for avatar lip-sync. No hard blocker was found. The
single thing most likely to surprise the schedule is the CGO build
image change -- validate it first (Open question 2).

---

## Caveat 1: CGO + libopus build dependency

`media-sdk` depends on `gopkg.in/hraban/opus.v2` (CGO bindings to
`libopus`/`libopusfile`) and `github.com/livekit/amrwb-cgo`. Implications:

- The agent build requires `CGO_ENABLED=1` and the Opus dev headers in
  the toolchain image. memQL builds CGO-free today, so the Voice-node
  build target (and its CI lane) needs the C toolchain + `libopus-dev`
  / `libopusfile-dev`.
- Cross-compilation and fully-static binaries get harder (CGO).
- This is why **this PR does not add the dependency** -- doing so
  would put a CGO-Opus build requirement on the repo-wide
  `go build ./...` / `go vet ./...` CI lanes (see `.github/workflows/ci.yml`),
  which run CGO-free and lack `libopus`, turning CI red. The dependency
  belongs in the implementation PR alongside the build-image change,
  ideally scoping the CGO surface to the Voice-node build only.

---

## 9. Why this PR is docs-only

The issue suggests optionally adding `server-sdk-go` to `go.mod` with a
compiling proof-of-concept. We deliberately did **not**, because:

- The only meaningful PoC (one that reads/writes PCM) must import
  `server-sdk-go/v2/pkg/media`, which transitively requires the CGO
  Opus binding. The repo's CI (`go build ./...`, `go vet ./...`,
  `go test ./...`) runs **CGO-free with no `libopus` installed**, so
  such a PoC would not compile in CI -> red checks -> no auto-merge.
- A PoC that imports only the non-media `lksdk` symbols (connect +
  callbacks) would compile, but would not exercise the load-bearing
  PCM read/write that #451 actually asks about, so it would prove less
  than this document already establishes from the verified API surface.

Per the spike instruction "if unsure, keep the PR docs-only," and given
the CGO/CI-redness risk is concrete (not hypothetical), the dependency
and any PoC are deferred to the implementation phase, where the build-
image change lands with them. The API surface quoted above is verified
against the published `server-sdk-go/v2` and `media-sdk` package docs.
