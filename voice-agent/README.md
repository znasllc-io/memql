# memql voice-agent

Python process running [LiveKit Agents 1.5](https://docs.livekit.io/agents/)
to drive the realtime voice + video channel for the General Assistant.

This process is the **only** voice participant for the GA in a memql space.
Specialists are text-only. The Bridge Agent (`cmd/bridge-agent/`) is retired
once cutover lands.

## Architecture

```
LiveKit room
   |
   |  (voice-agent process -- LiveKit Agents 1.5)
   |
   +-- Deepgram Nova-3 STT plugin     (user audio in)
   |                  |
   |                  v
   |             memql LLM custom plugin
   |             speaks VoiceAgent* gRPC contract
   |                  |
   |                  v
   |             memql cognition (BYO LLM)
   |             returns GA's response text
   |                  |
   |                  v
   +-- Deepgram Aura-2 TTS plugin     (token-by-token input)
   |                  |
   |                  v
   +-- Anam (CARA-3) avatar plugin    (lip-synced video)
```

## Setup

```bash
cd voice-agent
make voice-agent          # install deps via uv (or pip)
make voice-agent-run      # join LiveKit rooms, drive GA voice
make voice-agent-test     # run unit tests
```

Manual setup:

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[anam,dev]"
python -m voice_agent.main start
```

## Environment

Required:

| Var | What |
|---|---|
| `MEMQL_DEEPGRAM_API_KEY` | Deepgram Nova-3 + Aura-2 |
| `LIVEKIT_URL` | LiveKit signaling URL (ws://livekit:7880 in dev) |
| `LIVEKIT_API_KEY` | LiveKit API key |
| `LIVEKIT_API_SECRET` | LiveKit API secret |
| `MEMQL_GRPC_ADDR` | memql cluster gRPC address (e.g. bff:50051) |
| `VOICE_AGENT_TOKEN` | identity-issued `class="voice_agent"` JWT bearer. Minted via `JWTIssuer.IssueVoiceAgentAccessToken` on the cluster side -- see `docs/auth/voice-agent-jwt.md`. |

Optional:

| Var | Default | What |
|---|---|---|
| `MEMQL_AVATAR_VENDOR` | `anam` | `anam` or `simli` |
| `ANAM_API_KEY` | -- | required when vendor=anam |
| `SIMLI_API_KEY` | -- | required when vendor=simli |
| `VOICE_AGENT_LOG_LEVEL` | `INFO` | DEBUG / INFO / WARNING / ERROR |

## Protocol

The voice-agent speaks `MemqlService.Stream` over gRPC with these message
types:

Client to server:
- `VoiceAgentSessionStart` / `End` -- bind a LiveKit room to a space
- `VoiceAgentPartialTranscript` -- streaming ASR partials (event-only on memql)
- `VoiceAgentFinalTranscript` -- final user utterance, becomes a chat row
- `VoiceAgentTurnRequest` -- ask the GA to respond

Server to client:
- `VoiceAgentSessionAck` -- carries canonical voice + persona + initial gate
- `VoiceAgentPartialAck` / `FinalAck`
- `VoiceAgentTurnDelta` -- streaming prose (INCREMENTAL, not accumulated)
- `VoiceAgentTurnComplete` -- terminal

All graph writes happen server-side inside memql. The voice-agent has zero
direct graph-write surface; the shared-secret token pins it to this set of
message types only.

## Project layout

```
voice-agent/
  pyproject.toml
  README.md
  Dockerfile
  voice_agent/
    __init__.py
    main.py                  entry point; LiveKit Agents Worker
    config.py                env var loading
    grpc_client.py           memql gRPC stream wrapper
    memql_llm_plugin.py      custom LLM plugin (Phase 4)
    persona_resolver.py      canonical voice + persona lookup
    thread_context.py        Team vs Group thread tracking
    transcript_forwarder.py  partial + final forwarding
    proto/                   generated python proto stubs (gitignored)
  tests/
    test_config.py
```

Generated proto stubs land in `voice_agent/proto/` after running
`make voice-agent` -- they are git-ignored because they are reproducible
from `component/grpc/memql.proto`.
