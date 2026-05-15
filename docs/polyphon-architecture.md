# Voice + Video Architecture

The realtime voice + video channel is owned by the Python `voice-agent`
process under [`voice-agent/`](../voice-agent/) (LiveKit Agents 1.5).
It joins LiveKit rooms as the General Assistant's sole voice +
video participant. Specialists are text-only by design (Initiative C):
they never publish into the LiveKit room, but their replies still
flow to chat via memql's normal agent dispatch path.

## Pipeline

```
LiveKit room
   |
   |  (voice-agent process -- Python, LiveKit Agents 1.5)
   |
   +-- Deepgram Nova-3 STT (user audio in)
   |              |
   |              v
   |        memql LLM custom plugin
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

## Files

- [`voice-agent/`](../voice-agent/) -- Python project. See its
  [README](../voice-agent/README.md) for setup + per-plugin details.
- `voice-agent/voice_agent/stt_plugin.py` -- Deepgram Nova-3.
- `voice-agent/voice_agent/memql_llm_plugin.py` -- memql custom LLM.
- `voice-agent/voice_agent/tts_plugin.py` -- Deepgram Aura-2.
- `voice-agent/voice_agent/avatar_plugin.py` -- Anam / Simli.
- `voice-agent/voice_agent/persona_resolver.py` -- canonical voice
  + persona id lookup at session start.
- `voice-agent/voice_agent/transcript_forwarder.py` -- streaming
  partials + finals over `VoiceAgentPartialTranscript /
  FinalTranscript`.

## memql side

- `component/grpc/memql.proto` -- the `VoiceAgent*` message family.
- `component/grpc/voice_agent_handlers.go` -- handlers for the five
  client-to-server message types (SessionStart / End, PartialTranscript,
  FinalTranscript, TurnRequest).
- `component/grpc/voice_agent_stream_interceptor.go` -- shared-secret
  bearer (`Authorization: Bearer mql_va_<...>`) pinned to the
  `VoiceAgent*` message surface. Voice-agent has zero direct
  graph-write surface.
- `app/voice_agent.go` -- env var loading.

## Concepts

- `v1:agents:agent.audioControl` / `videoControl` -- per-agent
  defaults, `always_on` / `always_off` / `mirror_user`. Default
  `mirror_user` for every new agent.
- `v1:cognition:audioOverride` / `videoOverride` -- per-(space, agent)
  session overrides, beat the agent default.
- `v1:agents:agent.avatarPersonaId` / `avatarVendor` -- vendor-
  issued persona id minted from a still image. Empty disables the
  avatar plugin (voice-agent falls back to audio-only).

## Env

| Var | What |
|---|---|
| `MEMQL_DEEPGRAM_API_KEY` | Deepgram (STT + TTS) |
| `MEMQL_VOICE_AGENT_SHARED_TOKEN` | shared secret on the memql side |
| `VOICE_AGENT_SHARED_TOKEN` | same secret on the voice-agent side |
| `MEMQL_AVATAR_VENDOR` | `anam` (default), `simli`, `none` |
| `ANAM_API_KEY` / `SIMLI_API_KEY` | vendor keys |
| `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | LiveKit |

## Make targets

| Target | Purpose |
|---|---|
| `make voice-agent` | Install Python deps + regenerate proto stubs |
| `make voice-agent-run` | Run worker locally |
| `make voice-agent-test` | pytest suite |
| `make voice-agent-docker` | Build the voice-agent container |
| `make voice-loop-test-livekit` | Smoke + operator runbook |
| `make voice-trace` | Tail voice-path log lines across nodes |

## What stayed from the legacy Go path

The Go Bridge Agent (`cmd/bridge-agent/` + `component/polyphon/bridge/`)
was retired in Initiative C Phase 11. What remains on the Go side:

- `integrations/deepgram/` + `integrations/openai/` -- still consumed
  by the `/memql/audio` WebSocket path for voice-first creation modals
  (CreateSpaceModal, KnowledgeModal, etc.). NOT used by the
  voice-agent path -- voice-agent talks to Deepgram directly via the
  `livekit-plugins-deepgram` Python package.
- `integrations/voice/voices.go` -- canonical voice catalog.
- `integrations/stt/` -- streaming + batch transcription provider.
- `component/polyphon/` (minus `bridge/`) -- score engine, room
  provider, mention / intent helpers. Still consumed by the cognition
  path for turn-taking decisions.
