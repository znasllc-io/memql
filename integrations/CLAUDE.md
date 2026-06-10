# Integrations Directory

**Purpose:** External service integrations (Go code)
**Language:** Go
**Type:** Protocol adapters that bridge external services into the memQL ecosystem

---

## Integration Contract

Integrations are **protocol adapters**. They bridge external services (OpenAI voice, Deepgram voice, etc.) into the memQL ecosystem using Go code for protocol-level concerns that cannot be expressed in the MemQL DSL.

### What Integrations DO (Go code)
- Handle external protocols (WebSocket, gRPC, HTTP webhooks, binary audio)
- Emit events into the system when external things happen
- Expose **capabilities** -- typed functions callable from the MemQL DSL
- Manage real-time state (streaming, presence, caching)

### What Integrations DO NOT DO (belongs in MemQL DSL)
- Query orchestration (fetching data, assembling context)
- SI invocation (calling prompts, tool-calling loops)
- Business logic (deciding who responds, building conversation history)
- Data mutations (inserting records, updating nodes)

### IntegrationProvider Interface

All integrations that embed the base `Integration` struct should implement `IntegrationProvider`:

```go
type IntegrationProvider interface {
    IntegrationName() string
    Capabilities() []IntegrationCapability
}
```

Capabilities are registered with the engine -- either through the plug-in path (`memql.RegisterPlugin` at `init()`; preferred and used by core integrations like database, auth, identity, email, embedding, files, gcs) or through explicit `app/` wiring for complex node-type-scoped ones (cognition, agent, stt). Either way they become callable as builtin functions from the DSL. Return `nil` from `Capabilities()` for integrations with no DSL-callable functions.

### Channel-Based Dispatch

When the engine's bus wiring is configured, integration capability calls are dispatched
via the `IntegrationRequests` channel using `IntegrationDispatchRequest` protobuf messages.
The engine's integration dispatcher goroutine reads from the channel, looks up the handler
by FQN, executes it, and sends the response via `ReplyTo`. The `IntegrationProvider`
interface itself does not change -- only the dispatch mechanism.

### IntegrationEngineAccess

Integrations should receive `IntegrationEngineAccess` (narrow interface) instead of the full `MemQLEngine`. This interface provides:
- `RegisterIntegration()` -- capability registration
- Streaming provider access (for protocol-level streaming)
- Tool definitions and execution (for tool-calling streams)

It explicitly does NOT provide `Execute()`, `InvokeSI()`, or `RenderPrompt()` -- those belong in MemQL automations and functions.

### Pattern: Events Inward, Capabilities Outward

```
External Service --> Integration (Go) --> [emits event] --> Automation (MemQL)
                                                                |
                                                    [calls integration capability]
                                                                |
                     Integration (Go) <-- [capability handler] <--
                         |
External Service <-- [protocol response]
```

---

## Directory Structure

```
integrations/
├── CLAUDE.md          # This file
├── arch.md           # Architecture documentation
├── agent/            # SI tool-loop + chat replier + AiSuggest dispatcher (agent build only)
├── audio/            # PCM16 resampling for the Polyphon pipeline
├── avatarvendor/     # CGO-free Anam/Simli avatar-vendor REST + dispatch core (shared by voice-agent + direct/Guide avatar)
├── auth/             # resolveUser, checkPermission
├── cognition/        # Routing+conductor, Polyphon scoring, client-tool relay
├── copresent/        # CoPresent product event-routing rules
├── database/         # healthCheck, stats
├── email/            # Microsoft Graph / SMTP / Log senders -- sendEmail
├── embedding/        # Vendor-neutral text-embedding capability
├── fileprocessor/    # extractText (PDF / DOCX / images via VisionSIProvider)
├── gcs/              # storage.upload
├── identity/         # Identity-side helpers
├── knowledge/        # Corpus seed + lookup helpers (concept:* surfaces for agents)
├── avatardirect/     # Direct/Guide avatar: mint LiveKit room + bring Anam up (avatarDirectStartSession)
├── deepgram/         # Polyphon ASR/TTS via Deepgram (Nova-3 WS + Aura-2 REST)
├── openai/           # Polyphon ASR/TTS via OpenAI (Realtime transcription + /v1/audio/speech)
├── router/           # SI router ledger
├── similarity/       # pgvector similarTo() builtin
├── stt/              # Speech-to-text (batch transcribe capability + streaming session)
└── training/         # Per-agent train pipeline (identity embedding + distilled system prompt + just-in-time knowledge seeding)

# NemoClaw is invoked via webhook tools (claw coding-agent tool surface,
# defined alongside the agent tool definitions), not a Go integration here.
```

---

## Key Integrations

### cognition/ -- routing + conductor + relay

Owns the cognition pipeline on the cognition node. Capabilities
registered: `cognitionScore`, `cognitionTrackPresence`,
`cognitionForwardToBridgeAgent`.

**Single LLM brain on the text path** (shipped 2026-04-26 in
4249c0b). The conductor (`conductor_consult.go` +
`dsl/cognition/prompts/conductorTurn.tmpl`) emits both the routing
decision (`fitScore`, `turnMode`, `handoff`, `severity`) and the
per-agent plan (primary, sequence, chime-ins, instructions) in one
structured-output call. The standalone router LLM call in
`si_router.go` only fires for voice utterances now (latency-
sensitive). Fast-path mention dispatch
(`tryFastPathDispatch`) bypasses both. Many older docs describing a
two-brain (router + conductor) architecture for the text path are
wrong about the current code.

**Capability-aware routing.** Both the conductor and the voice-path
router render each agent's tool list in their candidate block;
tool-fit mismatch drops fitScore by 0.4+ and full tool gap routes to
the general assistant with `turnMode=escalation_notice`. Prevents
specialists like Vera (HR) from grabbing UI-driving asks ("guide me
around the app") that need Sofia's `uiDescribe` / `uiClick` tools.

**Affirmation guard exempts corrections.** The
"affirmation/follow-up/farewell -- skip dispatch" guard exempts
correction-shaped utterances ("sorry I meant X", "actually no",
"wait that's wrong", etc.) so a user correcting the agent's previous
action lands as actionable instead of conversational.

**Conversational continuity.** The conductor receives an explicit
`lastResponder` input (most-recent SI to speak before this human
utterance), surfaced as its own field at the top of
`conductorTurn.tmpl`. The "Conversational continuity"
meta-principle requires the primary to stay with that agent when
the user's turn is a follow-up shape ("ok cool", "btw", "what
about", "tell me more") and there's no @-mention or domain pivot.
Plugs the "GA jumps in to defer to the specialist" pattern --
implicit follow-ups now route to the agent the user is mid-thread
with instead of falling through to the general assistant.

**Greet-on-join pacing.** `greet_on_join.go` serializes per-space
greetings: 3s initial delay before the FIRST greeting fires (so
the SPA finishes its modal-dismiss + route transition + first
paint before the utterance lands), 4s minimum gap between
consecutive greetings (so multi-`greetOnJoin` rooms don't fire a
chorus). The greeting directive is "familiar" for ALL agents --
every agent in CoPresent is one the user created and named, so
"Hi, I'm X" openers are forbidden across the board (no per-agent
flag; the rule lives in the directive instruction text).

Key files:
- `cognition_handler.go` -- event handler + dispatch flow
- `conductor_consult.go` -- conductor LLM call (+ lastResponder
  computation) + plan -> outcome adapter
- `greet_on_join.go` -- greetOnJoin handler with per-space
  serialization + initial / inter-greeting delays
- `si_router.go` -- voice-path router + fast-path dispatch + tool list
- `client_tool_relay.go` -- cross-node browser tool round-trip
- `agent_forward.go` -- BFF/cognition -> agent gRPC forwarding
- `space_context_engine.go`, `prompt_context_cache.go`,
  `participant_presence.go`

### audio/ - Audio Processing
**Purpose:** Audio streaming format conversion and resampling

**What It Does:**
- PCM16 sample rate resampling (16kHz <-> 24kHz for Polyphon <-> OpenAI)
- Streaming resampler with persistent filter state
- WAV header generation and PCM chunking for TTS delivery
- MP3 helpers for file-based audio

**Key Files:**
- `resample.go` - PCM16Resampler for Polyphon (16kHz) <-> OpenAI (24kHz)
- `wav.go` - WAV header generation and PCM chunking
- `mp3.go` - MP3 helpers

### openai/ - OpenAI Voice Providers (Polyphon)
**Purpose:** OpenAI ASR and TTS for the Polyphon multi-agent voice pipeline

**What It Does:**
- Streaming ASR via OpenAI Realtime API in transcription-only mode (WebSocket)
- TTS via OpenAI /v1/audio/speech HTTP API with PCM16 output
- Automatic 16kHz <-> 24kHz resampling for Polyphon pipeline compatibility
- Implements `polyphon.ASRProvider` and `polyphon.TTSProvider` interfaces

**Key Files:**
- `openai.go` - Package config (APIKey, ASRModel, TTSModel, TTSVoice)
- `asr.go` - ASRClient using Realtime API transcription-only mode
- `tts.go` - TTSClient using /v1/audio/speech HTTP API

**Environment Variables:**
- `POLYPHON_VOICE_PROVIDER=openai` (default) to select this provider
- `POLYPHON_OPENAI_ASR_MODEL` (default: gpt-4o-transcribe)
- `POLYPHON_OPENAI_TTS_MODEL` (default: gpt-4o-mini-tts)
- `POLYPHON_OPENAI_TTS_VOICE` (default: alloy)

### deepgram/ - Deepgram Voice Providers (Polyphon)
**Purpose:** Deepgram Nova-3 ASR + Aura-2 TTS for the Polyphon multi-agent voice pipeline. Auto-selected default when `MEMQL_DEEPGRAM_API_KEY` is set.

**What It Does:**
- Streaming ASR via Deepgram `/v1/listen` WebSocket (Nova-3)
- TTS via Deepgram `/v1/speak` REST (Aura-2, OGG/Opus output)
- Implements `polyphon.ASRProvider` and `polyphon.TTSProvider` interfaces

**Key Files:**
- `deepgram.go` - Package config (APIKey, ASRModel, TTSModel, Language, BaseURL)
- `asr.go` - ASRClient using Nova-3 streaming WebSocket
- `tts.go` - TTSClient using Aura-2 REST (OGG/Opus + raw PCM16 paths)

**Environment Variables:**
- `MEMQL_DEEPGRAM_API_KEY` - required
- `POLYPHON_VOICE_PROVIDER=deepgram` - explicit selection (auto when key set)
- `POLYPHON_DEEPGRAM_ASR_MODEL` (default `nova-3`)
- `POLYPHON_DEEPGRAM_TTS_MODEL` (default `aura-2`)
- `POLYPHON_DEEPGRAM_LANGUAGE` (default `en-US`)

### stt/ - Speech-to-Text
**Purpose:** Convert audio to text transcriptions

**DSL Capabilities:** `integration.stt.transcribe` -- Batch transcription from audio data

**What It Does:**
- Real-time streaming transcription over `MemqlService.Stream` via
  `AiTranscribeStreamStart` / `Chunk` / `End` (client -> server) and
  `AiTranscribeStreamDelta` / `Complete` (server -> client). Voice
  node owns the provider session; BFF proxies through `AiForwardRouter.ForwardContinuation`.
- Single-shot batch transcription via `AiTranscribeMsg` (still supported; used by callers that buffer the whole recording client-side).
- Batch transcription capability callable from DSL.
- Providers:
  - **Deepgram Nova-3** (auto-selected default when `MEMQL_DEEPGRAM_API_KEY`
    is set). True streaming WebSocket via Deepgram's `/v1/listen`; sub-300 ms
    first interim partials. Select explicitly via
    `MEMQL_STT_PROVIDER=deepgram`.
  - **OpenAI Realtime** (startup-time fallback). True streaming WebSocket
    via the Realtime API in transcription-only mode. Select via
    `MEMQL_STT_PROVIDER=openai-realtime`.
  - **OpenAI Whisper**. Batch-only via the transcriptions API.
    `whisper-1` transcribes verbatim; override via `MEMQL_WHISPER_MODEL`
    to use `gpt-4o-transcribe` or another model. Note: `whisper-1` must
    be enabled for your OpenAI project.

**Environment Variables:**
- `MEMQL_STT_PROVIDER` -- `deepgram` (default when key present), `openai-realtime`, or `openai-whisper`
- `MEMQL_STT_LANGUAGE` -- hard-pinned streaming transcription language (default `en`). One knob drives both providers (expanded to `en-US` for Deepgram, `en` for OpenAI Realtime); overrides the client `language_hint`. Prevents wrong/mixed-language drift + short-word hallucination on noisy/short audio.
- `MEMQL_STT_MIN_CONFIDENCE` -- low-confidence FINAL cutoff (default `0.6`). Drops noise/silence hallucination finals on Deepgram (real confidence) and gates a no-speech silence-hallucination denylist; `0` disables both gates. Filtering runs at the provider-agnostic `pumpDeltas` chokepoint in `component/grpc/si_transcribe_stream.go`.
- `MEMQL_DEEPGRAM_API_KEY` -- required for the Deepgram path
- `MEMQL_WHISPER_MODEL` -- OpenAI model name; defaults to `whisper-1`

### auth/ - Authentication Capabilities
**Purpose:** User identity operations callable from the MemQL DSL

**DSL Capabilities:**
- `integration.auth.resolveUser` -- Resolve current authenticated user from context
- `integration.auth.checkPermission` -- Check if user has a specific role

Auth middleware (identity-issued JWT verification) stays in component/identity/verifier/ and component/auth/.

### database/ - Database Management
**Purpose:** Database management operations callable from the MemQL DSL

**DSL Capabilities:**
- `integration.database.healthCheck` -- Check database connectivity and response time
- `integration.database.stats` -- Return connection pool statistics

The database connection itself remains a core component. This integration exposes management operations.

### fileprocessor/ - File Processing
**Purpose:** Extract text from uploaded files

**DSL Capabilities:**
- `integration.files.extractText` -- Extract text from PDF, DOCX, images, text files

Uses VisionSIProvider for image descriptions.

### gcs/ - Google Cloud Storage
**Purpose:** Cloud storage file operations

**DSL Capabilities:**
- `integration.storage.upload` -- Upload file data to GCS bucket

---

---

## Agent integration (`agent/`)

Runs on the agent build only. Holds the streaming SI tool-loop
(`streaming.go`), the chat replier (`replier.go`), the
respondToUser-envelope schema + parser (`envelope.go`), the AiSuggest
dispatcher (`suggest.go`) handling the full domain set
(spaces / spaceTitle / agents / groups / groupDescription /
agentCardSummary / spaceCardSummary / groupCardSummary / knowledge),
and the prompt context builder (`prompt_data.go`).

The unified `MEMQL_TOOL_LOOP_MAX_ITERATIONS` cap (200) gates both
streaming and non-streaming tool loops.

**Reply envelope.** The chat path delivers every user-facing reply
via a sentinel `respondToUser` tool call whose args are
`{response, citations[]}`. The streaming loop intercepts the call by
name (no engine executor exists), parses the args as
`agent.Envelope`, and uses that as the turn's final text + citations
list. See `envelope.go` -- the schema, tool definition, and parser
all live there.

The `spaceTitle` and `groupDescription` domains are lightweight
single-field generations (purpose -> title; name -> one-line
description) used by the create-modal blur handlers on the frontend.
Schema + prompt + post-process for each lives in
`component/server/sihttp/space_title_suggest.go` and
`group_description_suggest.go`. The richer `spaces` / `agents` /
`groups` domains return full payloads (description + suggested member
ids + roles). The three `*CardSummary` domains generate the LLM body
that lands on the agent / space / group canvas-creation cards;
`knowledge` powers the CoPresent KnowledgeModal's domain picker.

---

## START Adding New Integrations

memQL's integration system has two registration paths:

1. **Plug-in** (preferred) -- self-registers at `init()` with a narrow
   `PluginContext`. Good for integrations whose dependencies fit
   the common surface (Logger, Engine, BunDB, VisionProvider,
   EmbeddingProviderByName, partition/variable resolvers). Use this
   for any product-specific integration (e.g. integrations/copresent/)
   that doesn't need deps outside `PluginContext`.
2. **Explicit `app/` wiring** -- reserved for first-party integrations
   that need deps outside `PluginContext` (cognition, agent, stt).
   Stays in `app/integrations_*.go` with build-tag gating.

### Plug-in path (most cases)

**1. Create the package**
```bash
mkdir -p integrations/<name>
```

**2. Capability / provider** -- implement `memql.IntegrationProvider`
```go
// integrations/<name>/capabilities.go
package <name>

import (
    "context"
    "encoding/json"
    memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
    "github.com/znasllc-io/memql/component/memql"
)

type Integration struct{ /* ...deps... */ }

func New(/* deps */) *Integration { return &Integration{} }

func (i *Integration) IntegrationName() string { return "<name>" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
    return []memql.IntegrationCapability{
        {
            Name:        "doThing",
            Description: "...",
            Handler:     i.handleDoThing,
        },
    }
}

func (i *Integration) handleDoThing(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
    // ...
}
```

**3. Plug-in registration** -- self-register at `init()`
```go
// integrations/<name>/plugin.go
package <name>

import "github.com/znasllc-io/memql/component/memql"

func init() {
    memql.RegisterPlugin("<name>", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
        return New(/* pluck deps from pctx */), nil
    })
}
```

Return `(nil, nil)` from the factory to opt out at runtime (e.g., a
feature that needs configuration the environment didn't supply).

**4. Anchor the package** so its `init()` runs. Add a blank import
to `app/plugins_core.go` for core integrations, or
`app/plugins_copresent.go` for CoPresent-specific integrations.
Build-tag-gate the anchor file if the plug-in should only run on
certain node types.

**5. Expose it from the DSL via a builtin**
```memql
// in dsl/common/builtins.memql
@executor("integration.<name>.doThing")
func (Builtin) <name>DoThing { ... }
```

### Explicit `app/` path (complex first-party only)

When a PluginContext isn't enough, add wiring under
`app/integrations_<nodeType>.go` with the matching build tag and
call `a.engine.RegisterIntegration(yourInstance)` directly.

### Test It
```bash
make dev-cluster-up
make dev-cluster-logs | grep -iE "plug-in|<name>"
```

---

## Debugging Integrations

### Check Integration Startup
```bash
docker-compose logs memql | grep "integration.*started"
```

### Watch Integration Activity
```bash
# Cognition integration
docker-compose logs -f memql | grep "cognition"

# Turn state
docker-compose logs -f memql | grep "turn state"

# SI responses
docker-compose logs -f memql | grep "ai.*response"
```

### Common Issues

1. **Not Starting** - Check dependencies (engine, event bus)
2. **Events Not Firing** - Verify event subscription
3. **API Errors** - Check API keys in .env
4. **Performance** - Check cache hit rates in logs

---

## Environment Variables

### SI Providers (Cognition Integration)
```bash
MEMQL_SI_OPENAI_API_KEY=sk-...         # OpenAI provider
MEMQL_SI_ANTHROPIC_API_KEY=sk-ant-...  # Anthropic provider
MEMQL_SI_CACHE_DEFAULT_ENABLED=true
MEMQL_SI_CACHE_MAX_SECONDS=120
```

### Feature Flags
```bash
COGNITION_INTEGRATION_CAPABILITIES_LOGGING=true
COGNITION_INTEGRATION_CAPABILITIES_LOGGING_LOG_LEVEL=debug
```

---

## See Also

- [Architecture](arch.md) - integration system architecture
- [Audio Streaming](../docs/public/build/audio-streaming.md) - audio WebSocket + streaming-transcription gRPC path
- [Polyphon Architecture](../docs/public/operate/voice-bringup-verification.md) - voice pipeline architecture
- [Authoring rules](../docs/public/language/authoring-rules.md) - MemQL gotchas (read before extending the DSL surface)
