---
title: Audio Streaming Architecture
audience: public
status: stable
area: build
sinceVersion: 0.9.0
owner: znas
---

# Audio Streaming Architecture

> **Last Updated:** 2026-04-29

This document describes the audio paths in memQL: the **audio
WebSocket** (browser-based STT/TTS for spaces), the **gRPC streaming
transcription** flow on `MemqlService.Stream`, and the **Polyphon
pipeline** (multi-agent real-time voice conversations).

## Overview

memQL provides three audio paths, each for a different use case:

1. **Audio WebSocket** (`/memql/audio`) -- legacy browser path for
   in-space STT and the "Read Aloud" TTS feature. Users speak into
   their mic; audio is transcribed and committed as a
   `v1:cognition:utterance`. Still in production use.

2. **gRPC streaming transcription** -- canonical path for new clients.
   `AiTranscribeStreamStart` / `Chunk` / `End` (client -> server) plus
   `AiTranscribeStreamDelta` / `Complete` (server -> client) on
   `MemqlService.Stream`. The voice node owns the provider session;
   the BFF proxies via `AiForwardRouter.ForwardContinuation`. See
   `component/grpc/ai_transcribe_stream.go`.

3. **Polyphon Pipeline** -- multi-agent, multi-human real-time voice
   conversations. LiveKit for audio transport, a Bridge Agent for
   ASR/TTS, and the cognition pipeline for turn-taking decisions.

```
┌─────────────────────────────────────────────────────────────────────────┐
│                              MEMQL SERVER                                │
│                                                                          │
│  /memql/ws  (gRPC tunneled over WS)         /memql/audio (legacy WS)     │
│   - All gRPC messages incl. AiTranscribe*    - In-space STT + TTS chunks │
│   - Queries / mutations / subscriptions      - Single-user per stream    │
│   - Streaming transcription                                              │
│                                                                          │
│  HTTP (browser-required exceptions only):                                │
│    /auth/*        OAuth callbacks (HTTP-required)                        │
│    /healthz       Health probe                                           │
│    /spaces/{id}/attachments  multipart upload                            │
│    /polyphon/room-token, /polyphon/status  Polyphon multi-agent voice    │
│                                                                          │
│  All paths share the same identity-service-validated context.            │
└─────────────────────────────────────────────────────────────────────────┘
```

## When to use each path

| Path | Purpose | Transport | Multi-party |
|------|---------|-----------|-------------|
| `AiTranscribeStream*` (gRPC) | Transcription for any client | gRPC stream | No |
| `AiTranscribe` (gRPC, batch) | One-shot upload-and-transcribe | gRPC stream | No |
| `/memql/audio` (WebSocket) | Legacy browser STT + Read Aloud TTS | WebSocket | No |
| Polyphon pipeline | Real-time voice conversations | LiveKit (WebRTC SFU) | Yes (up to 3 agents + 5 humans) |

New clients should use the gRPC streaming path
(`AiTranscribeStreamStart`/`Chunk`/`End`). The `/memql/audio` WebSocket
exists for the older browser flow and the Read-Aloud TTS feature.

---

## Audio WebSocket Endpoint

The audio WebSocket (`/memql/audio`) provides browser-based STT transcription and TTS synthesis for spaces. When users speak, their audio is streamed to the server, transcribed using a speech-to-text provider, and converted into utterances that appear in the chat.

### Connection

- **Endpoint**: `/memql/audio`
- **Protocol**: WebSocket
- **Auth**: Same JWT/cookie as `/memql/ws`

### Why a Separate WebSocket?

Audio, video, and query traffic have fundamentally different characteristics:

| Traffic Type | Frequency | Message Size | Latency Sensitivity |
|-------------|-----------|--------------|---------------------|
| Queries | 1-10/minute | 100B - 10KB | Low |
| Audio | 10-20/second | 2-4KB | High |
| Video | 30-60/second | 10-100KB | Very High |

Using separate connections provides:

1. **No interference**: Audio flows independently from queries
2. **Optimized for purpose**: Each connection is tuned for its traffic type
3. **Independent scaling**: Audio processing can scale separately
4. **Failure isolation**: STT provider issues don't affect chat
5. **Future-proof**: Same pattern extends to video

### Message Protocol

All messages are JSON-encoded.

#### Start Stream (Client to Server)

Sent when the user begins recording:

```json
{
  "type": "start",
  "streamId": "550e8400-e29b-41d4-a716-446655440000",
  "spaceId": "space-123",
  "participantId": "participant-456",
  "format": "pcm16",
  "sampleRate": 16000,
  "channels": 1,
  "languageHint": "en"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | Yes | Must be `"start"` |
| `streamId` | string | Yes | Client-generated UUID for this audio stream |
| `spaceId` | string | Yes | ID of the space |
| `participantId` | string | Yes | ID of the participant speaking |
| `format` | string | No | Audio format: `"pcm16"` (default), `"opus"`, `"webm"` |
| `sampleRate` | number | No | Sample rate in Hz (default: 16000) |
| `channels` | number | No | Number of channels (default: 1) |
| `languageHint` | string | No | Language code hint (e.g., `"en"`, `"es"`) |

#### Audio Chunk (Client to Server)

Sent continuously while recording:

```json
{
  "type": "chunk",
  "streamId": "550e8400-e29b-41d4-a716-446655440000",
  "audio": "SGVsbG8gV29ybGQ=",
  "sequence": 1
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | Yes | Must be `"chunk"` |
| `streamId` | string | Yes | Same UUID from start message |
| `audio` | string | Yes | Base64-encoded audio data |
| `sequence` | number | No | Sequence number for ordering |

#### End Stream (Client to Server)

Sent when the user stops recording:

```json
{
  "type": "end",
  "streamId": "550e8400-e29b-41d4-a716-446655440000",
  "cancelled": false
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | Yes | Must be `"end"` |
| `streamId` | string | Yes | Same UUID from start message |
| `cancelled` | boolean | No | `true` to discard without creating utterance |

#### Started Response (Server to Client)

Sent after successful stream initialization:

```json
{
  "type": "started",
  "streamId": "550e8400-e29b-41d4-a716-446655440000"
}
```

#### Transcription Event (Server to Client)

Sent as transcription results arrive:

```json
{
  "type": "transcription",
  "streamId": "550e8400-e29b-41d4-a716-446655440000",
  "text": "Hello, how are you?",
  "isFinal": true,
  "confidence": 0.95,
  "words": [
    { "word": "Hello", "start": 0, "end": 320, "confidence": 0.98 },
    { "word": "how", "start": 350, "end": 480, "confidence": 0.94 },
    { "word": "are", "start": 500, "end": 580, "confidence": 0.96 },
    { "word": "you", "start": 600, "end": 750, "confidence": 0.93 }
  ],
  "utteranceId": "utt-voice-1702156800000000000",
  "durationMs": 1500
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Always `"transcription"` |
| `streamId` | string | Stream this result belongs to |
| `text` | string | Transcribed text |
| `isFinal` | boolean | `false` for interim, `true` for final |
| `confidence` | number | Confidence score (0.0 - 1.0) |
| `words` | array | Word-level timestamps (final only) |
| `utteranceId` | string | ID of created utterance (final only) |
| `durationMs` | number | Audio duration in ms (final only) |

#### Error Response (Server to Client)

```json
{
  "type": "error",
  "streamId": "550e8400-e29b-41d4-a716-446655440000",
  "error": {
    "code": "STREAM_NOT_FOUND",
    "message": "No active stream with this ID"
  }
}
```

### Audio Format

#### Recommended Settings

- **Sample Rate**: 16000 Hz (optimal for speech recognition)
- **Channels**: 1 (mono)
- **Format**: PCM16 (16-bit signed integer)
- **Chunk Size**: ~100-200ms of audio per chunk

#### PCM16 Format

Browser audio (Float32Array with values -1.0 to 1.0) must be converted to PCM16:

```javascript
function float32ToPcm16(float32Array) {
  const pcm16 = new Int16Array(float32Array.length);
  for (let i = 0; i < float32Array.length; i++) {
    const s = Math.max(-1, Math.min(1, float32Array[i]));
    pcm16[i] = s < 0 ? s * 0x8000 : s * 0x7FFF;
  }
  return pcm16;
}
```

### STT Data Flow

```
1. User presses mic button
2. Client opens /memql/audio WebSocket (if not already open)
3. Client sends "start" message with spaceId, participantId
4. Client captures audio via getUserMedia + AudioWorklet
5. Client converts Float32 to PCM16, base64 encodes, sends "chunk" messages
6. Server forwards chunks to STT provider (Deepgram Nova-3 when configured; OpenAI Realtime / OpenAI Whisper otherwise)
7. Server receives interim transcriptions, sends to client (isFinal: false)
8. User releases mic button
9. Client sends "end" message
10. Server finalizes STT stream, gets complete transcription
11. Server inserts v1:cognition:utterance with:
    - utteranceType: "speech"
    - source.inputMethod: "stt"
    - source.sttProvider: configured provider name
    - timestamps.words: word-level timing
12. Server sends final transcription event (isFinal: true) with utteranceId
13. Event bus emits graph.node.created.v1:cognition:utterance
14. All participants receive utterance via /memql/ws subscription
15. Chat UI displays the voice message
```

### Utterance Structure

Voice messages create `v1:cognition:utterance` records with this structure:

```json
{
  "concept": "v1:cognition:utterance",
  "id": "utt-voice-1702156800000000000",
  "payload": {
    "spaceId": "space-123",
    "participantId": "participant-456",
    "utteranceType": "speech",
    "text": "Hello, how are you?",
    "duration": 1500,
    "timestamps": {
      "words": [
        { "word": "Hello", "start": 0, "end": 320 },
        { "word": "how", "start": 350, "end": 480 },
        { "word": "are", "start": 500, "end": 580 },
        { "word": "you", "start": 600, "end": 750 }
      ]
    },
    "source": {
      "inputMethod": "stt",
      "sttProvider": "openai-whisper"
    }
  }
}
```

### STT Configuration

#### Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `MEMQL_STT_PROVIDER` | STT provider: `deepgram` (auto-default when key set) / `openai-realtime` / `openai-whisper` | No |
| `MEMQL_DEEPGRAM_API_KEY` | Deepgram API key (selects `deepgram` automatically when set) | Yes (for Deepgram) |
| `MEMQL_SI_OPENAI_API_KEY` | OpenAI API key (for Whisper / Realtime) | Yes (if using OpenAI providers) |

#### MemQL Variables (v1:platform:partitionVariable)

| Variable | Description | Default |
|----------|-------------|---------|
| `MEMQL_STT_PROVIDER` | STT provider name | `openai-realtime` |
| `MEMQL_STT_DEFAULT_LANGUAGE` | Default language hint | `en` |

#### Provider Comparison

| Feature | Deepgram Nova-3 | OpenAI Realtime | OpenAI Whisper |
|---------|-----------------|-----------------|----------------|
| Real-time streaming | Yes | Yes | No (batch) |
| Interim results | Yes | Yes | No |
| Word timestamps | Yes | Yes | Yes |
| Deploy | Cloud API | Cloud API | Cloud API |
| Best for | Lowest TTFB, default | OpenAI-only stacks | Accuracy, offline |

**Deepgram Nova-3** (default when `MEMQL_DEEPGRAM_API_KEY` is set):
Streaming WebSocket via Deepgram's `/v1/listen`; sub-300 ms first
interim partials.

**OpenAI Realtime** (fallback): Streaming transcription via the
Realtime API in transcription-only mode.

**OpenAI Whisper**: Batch transcription via the transcriptions API.
Audio is buffered during the session and transcribed when the user
stops speaking. Best for accuracy but no interim results.

### STT Component Structure

```
server/audiows/
├── handler.go      # WebSocket handler, session management
└── messages.go     # Message type definitions

integrations/stt/
├── stt.go              # Provider interface, common types
├── openai_whisper.go   # OpenAI Whisper (batch)
├── openai_realtime.go  # OpenAI Realtime (streaming)
└── deepgram.go         # Deepgram Nova-3 (streaming)
```

### STT Provider Interface

```go
// StreamingProvider provides real-time streaming transcription
type StreamingProvider interface {
    // StartStream begins a new streaming session
    StartStream(ctx context.Context, config StreamConfig) (StreamingSession, error)

    // Name returns the provider name
    Name() string
}

// StreamingSession represents an active transcription session
type StreamingSession interface {
    // SendAudio sends audio data to the STT service
    SendAudio(audio []byte) error

    // Receive returns a channel for transcription events
    Receive() <-chan TranscriptionResult

    // Finalize closes the stream and returns final transcription
    Finalize(ctx context.Context) (*FinalTranscription, error)

    // Close terminates without waiting for final result
    Close() error
}
```

### Error Handling

| Error Code | Description | Recovery |
|------------|-------------|----------|
| `STREAM_NOT_FOUND` | streamId doesn't exist | Start a new stream |
| `STREAM_START_FAILED` | Failed to connect to STT | Retry or check config |
| `INVALID_FORMAT` | Bad audio format | Check format settings |
| `STT_ERROR` | STT provider error | Retry |

### Limitations

- Maximum audio duration: Limited by STT provider (typically 5+ minutes)
- Chunk size: ~200ms recommended for balance of latency/overhead
- Concurrent streams per connection: 1 (start new after previous ends)

---

## Text-to-Speech (TTS) via Audio WebSocket

The audio WebSocket also supports TTS synthesis for the "Read Aloud" feature in spaces. All TTS requests through this endpoint use the OpenAI TTS API provider configured in the engine's provider registry.

### Read Aloud Feature

The "Read Aloud" feature allows any chat message to be spoken by the SI agent.

#### Synthesize Request (Client to Server)

Sent when the user clicks "Read Aloud" on a message:

```json
{
  "type": "synthesize",
  "requestId": "req-550e8400-e29b-41d4-a716-446655440000",
  "text": "Hello, how are you today?",
  "voice": "nova",
  "format": "wav",
  "sampleRate": 24000
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | Yes | Either `"synthesize"` or `"tts_synthesize"` (both accepted) |
| `requestId` | string | Yes | Client-generated UUID for this request |
| `text` | string | Yes | Text to synthesize |
| `voice` | string | No | Voice ID (defaults to agent's configured voice) |
| `format` | string | No | Audio format: `"wav"` (default) - each chunk is complete WAV file |
| `sampleRate` | number | No | Sample rate in Hz (default: 24000) |

#### TTS Started Response (Server to Client)

Sent immediately when TTS synthesis begins:

```json
{
  "type": "tts_started",
  "requestId": "req-550e8400-e29b-41d4-a716-446655440000",
  "format": "wav",
  "sampleRate": 24000,
  "spaceId": "space-123",
  "participantId": "ai-participant-456",
  "text": "Hello, how can I help you?"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Always `"tts_started"` |
| `requestId` | string | Matches the synthesize request |
| `format` | string | Audio format: `"wav"` (each chunk is complete WAV file) |
| `sampleRate` | number | Sample rate in Hz |
| `spaceId` | string | Space ID for context |
| `participantId` | string | SI participant ID generating the audio |
| `text` | string | The text being synthesized |

#### TTS Chunk Response (Server to Client)

Streamed back as TTS generates audio. **Each chunk is a complete WAV file** that browsers can decode independently:

```json
{
  "type": "tts_chunk",
  "requestId": "req-550e8400-e29b-41d4-a716-446655440000",
  "audio": "UklGRiQAAABXQVZFZm10IBAAAA...",
  "format": "wav",
  "sampleRate": 24000,
  "sequence": 0,
  "done": false,
  "spaceId": "space-123",
  "participantId": "ai-participant-456",
  "text": "Hello, how can I help you?"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Always `"tts_chunk"` |
| `requestId` | string | Matches the synthesize request |
| `audio` | string | Base64-encoded WAV file (complete file with header, ~10KB per 200ms) |
| `format` | string | Audio format: `"wav"` |
| `sampleRate` | number | Sample rate in Hz (24000) |
| `sequence` | number | Chunk sequence number (starts at 0) |
| `done` | boolean | `true` for last chunk |
| `spaceId` | string | Space ID for context |
| `participantId` | string | SI participant ID generating the audio |
| `text` | string | The text being synthesized |

#### TTS Ended Response (Server to Client)

Sent when TTS synthesis completes or fails:

```json
{
  "type": "tts_ended",
  "requestId": "req-550e8400-e29b-41d4-a716-446655440000",
  "spaceId": "space-123",
  "participantId": "ai-participant-456"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Always `"tts_ended"` |
| `requestId` | string | Matches the synthesize request |
| `spaceId` | string | Space ID for context |
| `participantId` | string | SI participant ID |
| `cancelled` | boolean | `true` if TTS was cancelled (optional) |
| `error` | string | Error message if TTS failed (optional) |

### Audio Format Recommendation

**WAV is the default format for reliable progressive playback:**

| Format | Size per 200ms | Browser Decode | Recommendation |
|--------|---------------|----------------|----------------|
| `wav` | ~10KB | **Perfect - native support** | **Default - Most reliable** |
| `mp3` | ~800 bytes | Requires frame parsing | Complex, error-prone |
| `opus` | ~400 bytes | Needs Ogg container | Not supported raw |

**Why WAV:**

- **100% reliable**: Simple 44-byte header + raw PCM data
- **Zero decoding issues**: Browser's `decodeAudioData()` handles WAV perfectly
- **No frame boundaries**: Unlike MP3/Opus, no complex parsing required
- **Immediate playback**: Each chunk plays immediately with no initialization

**Frontend playback with WAV (progressive):**
```javascript
// Each chunk is a complete WAV file - decode and play immediately!
for await (const chunk of ttsStream) {
  const wavBuffer = base64ToArrayBuffer(chunk.audio);
  const audioBuffer = await audioContext.decodeAudioData(wavBuffer);
  // Queue immediately for playback - starts playing within ~200ms
  queueAudioForPlayback(audioBuffer);
}
```

**Chunk characteristics:**
- Each chunk is ~200ms of audio
- Each chunk is a complete WAV file (44-byte header + PCM data)
- Each chunk is ~10KB (24kHz mono 16-bit)
- Browser decodes each chunk instantly and perfectly

### TTS Data Flow (Read Aloud)

```
1. User clicks "Read Aloud" on a message
2. Client sends "synthesize" message with text and requestId
3. Server sends "tts_started" message with format info
4. Server calls OpenAI TTS API (from engine provider registry)
5. Server streams "tts_chunk" messages (WAV audio)
6. Client uses native decodeAudioData() for playback
7. Server sends "tts_ended" on completion
```

**Voice consistency:** The agent's `providerConfig.voice.voiceId` is used, ensuring the SI agent has a consistent voice identity.

### TTS Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `MEMQL_DEFAULT_TTS_PROVIDER` | TTS provider name from registry | `tts1` |

TTS providers are configured in `providers/v1/openai/` as `.memql` files with `@type("OpenAITTS")`. The default voice, format, and speed are set per-provider in the MemQL configuration.

### Chunk Sizing

Chunk sizes are optimized per format for ~200-300ms of audio:

| Format | Chunk Size | Duration |
|--------|------------|----------|
| `wav` | ~10 KB | ~200-300ms |
| `opus` | 8 KB | ~200-300ms |
| `mp3` | 8 KB | ~200-300ms |
| `pcm` | 12 KB | ~250ms |

---

## gRPC Streaming Transcription

The canonical streaming-transcription path for new clients lives on
`MemqlService.Stream` -- the same bidirectional gRPC stream that
carries chat, suggest, and graph traffic.

### Message flow

```
client -> server                        server -> client
─────────────────────────────────       ─────────────────────────────────
AiTranscribeStreamStart {               AiTranscribeStreamDelta {
  request_id, sample_rate, ...           request_id, text, is_final
}                                       }     (zero or more interim deltas)
AiTranscribeStreamChunk {              AiTranscribeStreamComplete {
  request_id, audio  (PCM16 bytes)       request_id, transcript, words
}                                       }
... more chunks ...
AiTranscribeStreamEnd { request_id }
```

The flow is keyed by `request_id`. The voice node owns the provider
session; the BFF proxies via `AiForwardRouter.ForwardContinuation`
so chunks land on the same voice instance that owns the session.

### Files

- `component/grpc/ai_transcribe_stream.go` -- handler + per-stream
  state machine
- `component/grpc/ai_forward.go` -- BFF -> voice forwarding
- `integrations/stt/` -- provider implementations (Deepgram Nova-3, OpenAI Realtime, OpenAI Whisper)

### Provider selection

Same env vars as the legacy `/memql/audio` path:

| Variable | Values | Default |
|----------|--------|---------|
| `MEMQL_STT_PROVIDER` | `deepgram`, `openai-realtime`, `openai-whisper` | auto (`deepgram` when `MEMQL_DEEPGRAM_API_KEY` is set, else `openai-realtime`) |
| `MEMQL_DEEPGRAM_API_KEY` | Deepgram key | required for `deepgram` |
| `MEMQL_SI_OPENAI_API_KEY` | OpenAI key (Realtime / Whisper) | required for OpenAI |

`docker-compose.full.yml` brings up a voice node alongside the BFF so
streaming transcription works on the basic dev path without needing
the cluster overlay (this was the change in 545537d).

### Single-shot batch path

`AiTranscribeMsg` (one request, one response) is still supported for
clients that buffer the whole recording client-side. Same provider
backends.

---

## Polyphon Voice Pipeline

Multi-agent real-time voice conversations route through the Polyphon
pipeline -- LiveKit for audio transport, a Bridge Agent for ASR/TTS,
and the cognition node for turn-taking.

The full architecture (audio flow, provider flavors, configuration,
component structure, costs) lives in
[/docs/polyphon-architecture.md](/docs/polyphon-architecture.md). Don't
duplicate it here.

### Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/polyphon/room-token` | POST | Generate a LiveKit room token for a participant |
| `/polyphon/status` | GET | Session count and health status |

These are HTTP endpoints (not gRPC) because the LiveKit JavaScript
SDK expects an HTTP token endpoint. Available only when the LiveKit
env vars are configured.

### Provider selection

`POLYPHON_VOICE_PROVIDER`:

- `deepgram` (auto-default when `MEMQL_DEEPGRAM_API_KEY` is set) -- Nova-3 ASR + Aura-2 TTS.
- `openai` (fallback) -- OpenAI Realtime transcription + `/v1/audio/speech` TTS.

---

*For the Polyphon architecture and deployment details, see [/docs/polyphon-architecture.md](/docs/polyphon-architecture.md)*
*For the overall memQL architecture, see [/docs/public/concepts/architecture.md](/docs/public/concepts/architecture.md)*
*For integration patterns, see [/integrations/CLAUDE.md](/integrations/CLAUDE.md)*
