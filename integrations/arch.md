# Integrations Package Architecture

> **Last Updated:** 2026-04-29

External-service adapters and DSL-callable capabilities. The
`integrations/` package bridges protocols memQL doesn't speak natively
(LiveKit, OpenAI WebSocket, Deepgram WebSocket, Microsoft Graph, GCS)
into the MemQL DSL via typed capability functions.

For the broader pattern (when to write Go vs MemQL DSL, plug-in
registration), see [CLAUDE.md](CLAUDE.md).

## Package layout

```
integrations/
├── integration.go            # Base Integration struct (embeddable lifecycle)
├── env.go, errors.go         # Shared env reader + sentinel errors
├── embed.go, integrations.json
├── auth/        capabilities.go        # resolveUser, checkPermission
├── audio/       resample.go, wav.go    # PCM16 sample-rate conversion
├── cognition/                          # routing + conductor + client-tool relay
├── agent/                              # SI tool-loop + replier + suggest
├── copresent/   routing.go             # CoPresent-specific event routing rules
├── database/    capabilities.go        # healthCheck, stats
├── email/                              # Microsoft Graph / SMTP / Log senders
├── embedding/                          # vector embedding capabilities
├── fileprocessor/                      # extractText (PDF / DOCX / image)
├── gcs/                                # storage.upload
├── identity/                           # identity-side helpers
├── knowledge/                          # corpus seed + lookup helpers
├── avatarvendor/                       # CGO-free Anam/Simli vendor REST + dispatch core
├── avatardirect/                       # Direct/Guide avatar: mint LiveKit room + bring Anam up
├── deepgram/    asr.go, tts.go         # Polyphon ASR/TTS via Deepgram (Nova-3 WS + Aura-2 REST)
├── openai/      asr.go, tts.go         # Polyphon ASR/TTS via OpenAI Realtime + /v1/audio/speech
├── router/                             # SI router ledger + integration
├── similarity/                         # pgvector similarTo() builtin
└── stt/                                # Speech-to-text (transcribe + streaming)
```

## Plug-in vs explicit wiring

memQL has two registration paths for integration providers:

1. **Self-registering plug-in** (`memql.RegisterPlugin(name, factory)`
   from `init()`). Factory receives a narrow `PluginContext`
   (Logger, Engine, BunDB getter, VisionProvider,
   EmbeddingProviderByName, partition / variable resolvers). This is
   the preferred path. Anchor the plug-in by adding a blank import to
   `app/plugins_core.go` (core integrations) or
   `app/plugins_copresent.go` (CoPresent-specific integrations).
   Build-tag-gate the anchor file if the integration only runs on
   certain node types.

2. **Explicit `app/` wiring** -- reserved for first-party integrations
   whose dependencies don't fit `PluginContext` (cognition, agent,
   stt). Lives in `app/integrations_<nodeType>.go` with build tags;
   `engine.RegisterIntegration(provider)` is called directly.

Routing + concept-ownership rules are also plug-in-registerable:
`node.RegisterRoutingRule(...)` and
`node.RegisterConceptOwnership(prefix, nodeType)` from `init()`. See
`integrations/copresent/routing.go` for an example.

## IntegrationProvider contract

```go
type IntegrationProvider interface {
    IntegrationName() string                // e.g. "cognition", "openaiVoice"
    Capabilities() []IntegrationCapability   // becomes builtin functions in the DSL
}
```

Integrations receive `IntegrationEngineAccess` (a narrow interface)
rather than the full `MemQLEngine` -- it deliberately excludes
`InvokeSI` / `InvokeSIChatWithTools` so SI orchestration stays in MemQL
DSL functions or routed via the `si()` builtin. The `RegisterPlugin`
surface enforces the same separation by the shape of `PluginContext`.

When the engine's bus wiring is configured, capability calls flow over
the `IntegrationRequests` channel via `IntegrationDispatchRequest`
protobuf messages; the engine's dispatcher goroutine looks up the
handler by FQN, executes it, and returns via `ReplyTo`.

## Cognition (text + voice routing)

`integrations/cognition/` owns:

- **Single-LLM-brain routing** (text path, shipped 2026-04-26 in
  4249c0b). The conductor LLM call returns both routing-side fields
  (fitScore / turnMode / handoff / severity) and the per-agent plan
  (primary / sequence / chime-ins / instructions). The standalone
  router LLM call only fires for voice utterances (latency-sensitive).
  Fast-path mention dispatch bypasses both. Files:
  `cognition_handler.go`, `conductor_consult.go`, `si_router.go`.
- **Capability-aware routing** -- the rendered agent block in both
  the conductor and the voice-path router includes a `Tools:` line
  with the agent's declared capability tools. Tool-fit mismatch drops
  fitScore by 0.4+; no agent has the right tool routes to GA with
  `turnMode=escalation_notice`.
- **Polyphon scoring** -- 5-factor turn-taking algorithm for voice
  rooms (live in `component/polyphon/`).
- **Client-tool relay** -- in cluster mode the agent and browser live
  on different nodes; `ClientToolCall` envelopes need a cross-node
  round trip via `v1:cognition:client:tool:request` /
  `:response` events. See `client_tool_relay.go` and the architecture
  block in [CLAUDE.md](../CLAUDE.md#client-tool-relay-agent--browser-across-nodes).
- **Correction-shaped follow-ups** (shipped 2026-04-26 in 916a73d) --
  the affirmation-silencer guard now exempts "sorry I meant X",
  "actually no", "wait that's wrong", etc. so corrections land as
  actionable instead of conversational acks.

Capabilities registered: `cognitionScore`, `cognitionTrackPresence`,
`cognitionForwardToBridgeAgent`.

## Agent (SI tool-loop + replier + suggest)

`integrations/agent/` runs on the `agent` build, holds the streaming
SI tool-loop (`streaming.go`), the chat replier
(`replier.go`), the suggest dispatcher (`suggest.go`), and the prompt
context builder (`prompt_data.go`). The tool loop hits a unified
`MEMQL_TOOL_LOOP_MAX_ITERATIONS` cap (200 today) shared with the
component/memql config.

The suggest path handles all `AiSuggestMsg` domains:
spaces / spaceTitle / agents / groups / groupDescription. The two
"lightweight" domains (`spaceTitle`, `groupDescription`) live in
`component/server/sihttp/space_title_suggest.go` /
`group_description_suggest.go` -- each has a strict single-field
output schema so the modal doesn't have to filter unwanted fields.

## Voice providers (Polyphon)

`integrations/deepgram/` and `integrations/openai/` both implement the
`polyphon.ASRProvider` and `polyphon.TTSProvider` interfaces from
`component/polyphon/`. The active provider is auto-selected at startup
(Deepgram when `MEMQL_DEEPGRAM_API_KEY` is set, OpenAI otherwise) and
can be forced via `POLYPHON_VOICE_PROVIDER`:

| Flavor | ASR | TTS | Notes |
|--------|-----|-----|-------|
| `deepgram` (auto-default when key set) | Deepgram Nova-3 streaming WebSocket (`/v1/listen`) | Deepgram Aura-2 REST (`/v1/speak`, OGG/Opus output) | Cloud-hosted; no GPU required. |
| `openai` (fallback) | OpenAI Realtime API transcription-only WebSocket | OpenAI `/v1/audio/speech` (HTTP, PCM16) | Same Cloud Run / dev target. |

The Polyphon pipeline standardizes on 16kHz PCM16 internally;
`integrations/audio/resample.go` does transparent 16<->24kHz conversion
at the OpenAI boundary. Deepgram's Nova-3 ingests 16kHz PCM16 directly
and Aura-2 returns OGG/Opus at the codec's native rate, so no resampling
runs on the Deepgram path.

Conversation-mode Realtime (the bundled STT+LLM+TTS) was retired with
the Polyphon migration. The provider type `OpenAIRealtime` is gone
from the registry; the only Realtime use today is
**transcription-only** ASR via the openai voice provider.

## Speech-to-text (`integrations/stt/`)

DSL capability: `integration.stt.transcribe` (batch).

Streaming transcription rides `MemqlService.Stream` via the
`AiTranscribeStreamStart` / `Chunk` / `End` ->
`AiTranscribeStreamDelta` / `Complete` flow. Voice node owns the
provider session; BFF proxies through `AiForwardRouter.ForwardContinuation`.
Single-shot batch transcription via `AiTranscribeMsg` is still
supported.

Providers (selected by `MEMQL_STT_PROVIDER`; auto-default is
`deepgram` when `MEMQL_DEEPGRAM_API_KEY` is set, else `openai-realtime`):

- `deepgram` -- streaming WebSocket via Deepgram Nova-3 (`/v1/listen`).
- `openai-realtime` -- streaming WebSocket via the OpenAI Realtime API
  in transcription-only mode.
- `openai-whisper` -- batch via `/v1/audio/transcriptions`.

## Email (`integrations/email/`)

Self-registering plug-in. Capability:
`integration.email.sendEmail`. Three sender flavors selected by env:

- **GraphSender** (preferred) -- Microsoft Graph `sendMail` via OAuth
  client-credentials. `EMAIL_AZURE_TENANT_ID` /
  `EMAIL_AZURE_CLIENT_ID` / `EMAIL_AZURE_CLIENT_SECRET` /
  `EMAIL_SENDER` / `EMAIL_FROM_NAME`. Legacy `AZURE_*` / `MAIL_*`
  names accepted as fallback during the rename window.
- **SMTPSender** -- standard SMTP. `SMTP_HOST` / `SMTP_PORT` /
  `SMTP_USERNAME` / `SMTP_PASSWORD` / `SMTP_FROM_ADDR` /
  `SMTP_FROM_NAME`.
- **LogSender** -- dev fallback when neither is configured.

## Other capabilities

| Integration | DSL capabilities |
|-------------|------------------|
| `auth` | `resolveUser`, `checkPermission` |
| `database` | `healthCheck`, `stats` |
| `embedding` | text embedding (vendor-agnostic over `EmbeddingProviderByName`) |
| `fileprocessor` | `extractText` (PDF, DOCX, images via VisionSIProvider, plain text) |
| `gcs` | `storage.upload` |
| `identity` | identity-side helpers (resolve etc.) |
| `knowledge` | corpus seed + lookup helpers |
| `avatardirect` | `startSession` / `stopSession` (direct/Guide Anam avatar) |
| `router` | SI router ledger writes |
| `similarity` | pgvector `similarTo()` builtin |

## Adding a new integration

See [CLAUDE.md > START Adding New Integrations](CLAUDE.md#start-adding-new-integrations).
The plug-in path is the right answer in almost every case; touch
`app/integrations_*.go` only when the dependencies don't fit
`PluginContext`.

## Configuration

All non-bootstrap configuration lives in concept storage, not env
vars -- `v1:platform:globalSecret` (encrypted) and
`v1:platform:globalVariable` (plain), with per-tenant overrides on
the `partition*` siblings. The full layout, naming convention, and
operator workflow lives in
[docs/guides/env-vars.md](../docs/guides/env-vars.md).

## Related

- [CLAUDE.md](CLAUDE.md) -- integration contract, capability pattern,
  preference order for extension points
- [docs/api/audio-streaming.md](../docs/api/audio-streaming.md) --
  audio WebSocket + streaming-transcription gRPC path
- [docs/polyphon-architecture.md](../docs/polyphon-architecture.md) --
  Polyphon multi-agent voice architecture
