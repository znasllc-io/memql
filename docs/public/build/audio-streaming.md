---
title: Audio Streaming Architecture
audience: public
status: stable
area: build
sinceVersion: 0.9.0
owner: znas
---

# Audio Streaming Architecture

> **Last Updated:** 2026-09-06

MemQL has one audio path: **streaming transcription over gRPC**, carried
by the `AiTranscribeStream*` message family on `MemqlService.Stream`. A
client opens a session, pumps audio bytes, reads partial transcripts as
they arrive, and receives a final transcript when the session closes.
That is the whole surface. There is no audio WebSocket, no separate
media transport, and no text-to-speech path in the engine.

## Overview

The engine turns audio into text and hands the text back. What the
caller does with the text -- open a goal, write a note, run a command --
is the caller's business, which is why the SDK's transcription primitive
deliberately writes no rows of its own.

One message family, two provider backends. The client sends
`AiTranscribeStreamStart` / `Chunk` / `End`; the server answers with zero
or more `AiTranscribeStreamDelta` frames and one
`AiTranscribeStreamComplete`. Partial transcripts arrive while the
speaker is still talking, which is what a hold-to-talk button renders.

There is no one-request-one-response transcribe message. A caller that
already has the whole recording still opens a session, sends it as
chunks, and reads the final transcript -- one wire shape, so there is
only one thing to implement and only one thing to authorize.

The agent node owns the provider session. A client that reaches a bff is
proxied there by `AiForwardRouter.ForwardContinuation`, so every chunk of
a session lands on the same agent instance that opened it -- the session
is in-memory state on exactly one replica, and a chunk routed anywhere
else has nothing to attach to.

```
client                            bff                        agent
──────────────────────────        ────────────────────       ─────────────────
AiTranscribeStreamStart  ───────> ForwardContinuation ──────> open ASR session
AiTranscribeStreamChunk  ───────> ForwardContinuation ──────> send audio
   ... more chunks ...
                         <─────── AiTranscribeStreamDelta <── interim text
AiTranscribeStreamEnd    ───────> ForwardContinuation ──────> finalize
                         <─────── AiTranscribeStreamComplete <── transcript
```

The flow is keyed by `request_id`. A browser reaches the same stream
through the WebSocket bridge at `/memql/ws`, which tunnels
`MemqlService.Stream` -- it is still gRPC underneath, and carries the
transcription messages like any other.

## gRPC Streaming Transcription

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

A `Delta` carries the FULL accumulated transcript so far, not an
incremental token, so a caller can render it directly rather than
maintaining its own buffer.

## Using it from an SDK

Neither SDK asks the caller to speak the wire protocol. `pushToTalk` is
the canonical entry point in both:

- `sdk/go/voice` -- `PushToTalk` takes an `io.Reader` of audio bytes,
  fires a callback per partial transcript, and returns the final one.
- `sdk/ts/src/voice` -- the same shape over a `ReadableStream`.

**The caller owns the audio source; the SDK owns the protocol.** A
terminal app wires a microphone library, a browser wires a
`MediaStream`. MemQL OS's Ask surface is the reference consumer: hold the
button, `clients/os/src/ask/micCapture.ts` produces 16 kHz mono PCM16,
and `clients/os/src/ask/sdkVoice.ts` hands it to the SDK's `pushToTalk`.

The SDK deliberately does NOT write a row when the transcript finalizes.
The caller receives the text and decides what it means, which keeps
transcription reusable for callers that are not conversational at all --
dictation, note capture, a command line.

## Audio format

### Recommended settings

- **Sample Rate**: 16000 Hz (optimal for speech recognition)
- **Channels**: 1 (mono)
- **Format**: PCM16 (16-bit signed integer)
- **Chunk Size**: ~100-200ms of audio per chunk

### PCM16 conversion

Browser audio (a `Float32Array` with values -1.0 to 1.0) must be
converted to PCM16 before it goes on the wire:

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

`opus`, `webm` and `wav` are accepted as well; PCM16 is the default and
the one every provider takes without a transcode.

## Providers

| Feature | OpenAI Realtime | OpenAI Whisper |
|---------|-----------------|----------------|
| Transcribes as audio arrives | Yes | No (buffers, then transcribes) |
| Interim results | Yes | No |
| Word timestamps | Yes | Yes |
| Deploy | Cloud API | Cloud API |
| Best for | Streaming default | Accuracy |

**OpenAI Realtime** (default): streaming transcription via the Realtime
API in transcription-only mode.

**OpenAI Whisper**: transcription via the transcriptions API, which takes
a whole recording rather than a stream. The session buffers the audio and
transcribes it when the speaker stops, so the wire shape is unchanged and
only the timing differs. Best for accuracy, but there are no interim
results -- a hold-to-talk button backed by Whisper shows nothing until
the user lets go.

### Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `MEMQL_STT_PROVIDER` | `openai-realtime` or `openai-whisper` | `openai-realtime` |
| `MEMQL_STT_LANGUAGE` | Server-side language pin (ISO-639-1) | `en` |
| `MEMQL_AI_OPENAI_API_KEY` | OpenAI key (Realtime / Whisper) | required for OpenAI |

`MEMQL_STT_LANGUAGE` **overrides any client-supplied language hint by
design**, and pinning it is the fix for the classic multi-language
hallucination: an unpinned model auto-detects and drifts to another
language on short or noisy audio, whatever the browser asked for.

## Provider interface

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

## Component structure

```
component/grpc/
├── ai_transcribe_stream.go  # handler + per-stream state machine
└── ai_forward.go            # bff -> agent forwarding

integrations/stt/
├── stt.go                # provider interface, common types
├── asr_session.go        # session lifecycle shared by both providers
├── filter.go             # transcript filtering
├── openai_whisper.go     # OpenAI Whisper (buffer, then transcribe)
└── openai_realtime.go    # OpenAI Realtime (streaming)

sdk/go/voice/pushtotalk.go       # Go SDK entry point
sdk/ts/src/voice/pushToTalk.ts   # TypeScript SDK entry point
```

## Limitations

- Maximum audio duration is bounded by the provider (typically 5+
  minutes).
- One concurrent stream per `request_id`; start a new session after the
  previous one completes.
- A session is tied to the agent replica that opened it. If that replica
  goes away mid-session the session is lost -- there is no resume, and
  the caller starts a new one.

---

*For the overall MemQL architecture, see [docs/public/concepts/architecture.md](../concepts/architecture.md)*
*For integration patterns, see [integrations/CLAUDE.md](../../../integrations/CLAUDE.md)*
