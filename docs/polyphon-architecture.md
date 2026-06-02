# Voice + Video Architecture

The realtime voice + video channel is owned by the **Go voice-agent**
in [`integrations/voice/agent/`](../integrations/voice/agent/), shipped
as the `voice-agent` subcommand of the `memql-voice` binary
(`memql-voice voice-agent`; build with `make voice`). It joins LiveKit
rooms as the General Assistant's sole voice + video participant.
Specialists are text-only by design (Initiative C): they never publish
into the LiveKit room, but their replies still flow to chat via memql's
normal agent dispatch path.

## Pipeline

```
LiveKit room
   |
   |  (voice-agent subcommand -- Go, integrations/voice/agent)
   |
   +-- Deepgram Nova-3 STT (user audio in)
   |              |
   |              v
   |        memql gRPC client
   |        speaks the VoiceAgent* gRPC contract on
   |        MemqlService.Stream
   |              |
   |              v
   |        memql cognition (BYO conductor + agent tool loop)
   |              |
   |              v
   +-- Deepgram Aura-2 TTS (token-by-token input streaming)
   |              |
   |              v
   +-- Anam or Simli avatar (lip-synced video, gated by Phase 7's
                              videoControl + videoOverride)
```

The agent supports two executors selected by `MEMQL_VOICE_EXECUTOR`:
`realtime` (default since #483 -- OpenAI gpt-realtime speech-to-speech)
and `cascade` (the Deepgram cascade above). Realtime degrades cleanly
to the cascade when its preconditions fail (no `OPENAI_API_KEY` /
persona build) and the live executor is logged loudly at session
start. The realtime voice resolves through the catalog's GA voice set
(`integrations/voice/voices.go`), defaulting to `marin`.

## Files

All under [`integrations/voice/agent/`](../integrations/voice/agent/):

- `agent.go` / `config.go` / `bootstrap.go` -- run entry, env loading,
  class="voice_agent" token resolution.
- `grpc_client.go` -- speaks the `VoiceAgent*` gRPC contract on
  `MemqlService.Stream`.
- `cascade.go` / `stt_pipeline.go` / `tts_pipeline.go` /
  `turntaking.go` -- Deepgram cascade + turn-taking / barge-in.
- `realtime_executor.go` / `realtime_lifecycle.go` /
  `realtime_budget.go` -- the gpt-realtime executor + guardrails.
- `persona.go` / `grounding.go` / `instructions.go` /
  `voice_resolve.go` -- persona + grounding parity, canonical voice +
  persona id lookup at session start.
- `avatar_room_voice.go` (`//go:build voice`) -- the LiveKit
  room/media glue for the Anam / Simli avatar participant. The
  CGO-free vendor REST + dispatch core it drives lives in the shared
  `integrations/avatarvendor` package (so the direct/Guide avatar
  capability reuses it).

## memql side

- `component/grpc/memql.proto` -- the `VoiceAgent*` message family.
- `component/grpc/voice_agent_handlers.go` -- handlers for the five
  client-to-server message types (SessionStart / End, PartialTranscript,
  FinalTranscript, TurnRequest).
- `component/grpc/voice_agent_stream_interceptor.go` -- class="voice_agent"
  JWT bearer pinned to the `VoiceAgent*` message surface. The
  voice-agent has zero direct graph-write surface.
- `app/transport_voice.go` -- voice transport wiring.

## Concepts

- `v1:agents:agent.audioControl` / `videoControl` -- per-agent
  defaults, `always_on` / `always_off` / `mirror_user`. Default
  `mirror_user` for every new agent.
- `v1:cognition:audioOverride` / `videoOverride` -- per-(space, agent)
  session overrides, beat the agent default.
- `v1:agents:agent.avatarPersonaId` / `avatarVendor` -- vendor-
  issued persona id minted from a still image. Empty disables the
  avatar (voice-agent falls back to audio-only).

## Env

| Var | What |
|---|---|
| `MEMQL_GRPC_ADDR` | BFF gRPC address the agent dials (e.g. `bff:50051`) |
| `MEMQL_DEEPGRAM_API_KEY` | Deepgram (STT + TTS) |
| `MEMQL_VOICE_EXECUTOR` | `realtime` (default) or `cascade` |
| `MEMQL_VOICE_ROOM_NAME` | room to join (fallback when no `--room` flag) |
| `OPENAI_API_KEY` / `MEMQL_REALTIME_*` | realtime executor path only |
| `VOICE_AGENT_TOKEN` | identity-issued class="voice_agent" JWT |
| `MEMQL_AVATAR_VENDOR` | `anam` (default), `simli`, `none` |
| `ANAM_API_KEY` / `SIMLI_API_KEY` | vendor keys |
| `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | LiveKit |

## Make targets

| Target | Purpose |
|---|---|
| `make voice` | Build the `memql-voice` binary (carries the `voice-agent` subcommand) |
| `make voice-agent-token` | Mint a class="voice_agent" JWT for the local cluster |
| `make voice-trace` | Tail voice-path log lines across nodes |

Docker: the `voice-agent` service in
`docker/docker-compose.polyphon.yml` runs the `memql-voice` image (the
`voice-runtime` CGO stage) with `command: voice-agent`.

## What stayed from the legacy Go path

The Go Bridge Agent (`cmd/bridge-agent/` + `component/polyphon/bridge/`)
was retired in Initiative C Phase 11, and the Python voice-agent
(LiveKit Agents 1.5) was retired in epic #449's cutover. What remains
on the Go side:

- `integrations/deepgram/` + `integrations/openai/` -- still consumed
  by the `/memql/audio` WebSocket path for voice-first creation modals
  (CreateSpaceModal, KnowledgeModal, etc.). The Go voice-agent talks to
  Deepgram directly from `integrations/voice/agent/`.
- `integrations/voice/voices.go` -- canonical voice catalog.
- `integrations/stt/` -- streaming + batch transcription provider.
- `component/polyphon/` (minus `bridge/`) -- score engine, room
  provider, mention / intent helpers. Still consumed by the cognition
  path for turn-taking decisions.
