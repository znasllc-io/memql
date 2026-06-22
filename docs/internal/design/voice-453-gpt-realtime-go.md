---
title: gpt-realtime over WebSocket in Go -- feasibility spike
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# gpt-realtime over WebSocket in Go -- feasibility spike

Spike deliverable for issue #453, part of epic #449 ("Replace the Python
voice-agent with a Go voice agent"). Phase 0, long pole #3. No code
dependencies; blocks the Go realtime executor (#457).

Status: design + feasibility verdict, grounded in the repo as it stood at
spike time and the live OpenAI Realtime GA API. This document does not change
runtime behavior -- it is a findings doc. The headline is in section 0.

> **Historical note (epic #449 complete).** The cutover described as future
> work here has shipped: the Go voice-agent now lives in
> `integrations/voice/agent/` (realtime executor in
> `realtime_executor.go`), and the Python voice-agent it replaced has been
> deleted. References below to the Python implementation describe the
> starting point, not the current tree.

---

## 0. VERDICT

**GO.**

Driving OpenAI `gpt-realtime` (speech-to-speech) from Go with the
conductor-gate posture (`turn_detection: null`, explicit
`response.create` / `response.cancel`, streaming audio in/out, async
function calling) is feasible today with **no new third-party dependency**.

Decision on the client: **hand-roll a thin WebSocket client on
`nhooyr.io/websocket` (already a dependency). Do NOT use
`github.com/sashabaranov/go-openai`.**

The deciding facts:

1. `go-openai` v1.41.2 (the pinned version in `go.mod`) has **zero**
   Realtime-API coverage -- no session websocket, no
   `input_audio_buffer.*`, no `response.create` / `response.cancel`, no
   audio-delta decoding, no realtime function-calling. Its README and
   package surface cover chat/completions/images/whisper/TTS/embeddings/
   assistants/batch only. There is no realtime type in the module cache
   (`grep -i realtime` over the vendored package returns nothing).
2. The repo **already hand-rolls an OpenAI Realtime WebSocket client in
   Go** -- `integrations/openai/asr.go` drives
   `wss://api.openai.com/v1/realtime` over `nhooyr.io/websocket` today
   (transcription-only mode). It already does the exact mechanics the
   speech-to-speech path needs: `Authorization: Bearer` + `OpenAI-Beta:
   realtime=v1` headers on the dial, a `session.update`-style config
   message, `input_audio_buffer.append` with base64 PCM16, a
   `receiveLoop` that switch-dispatches on the event `type` string, PCM16
   resampling via `integrations/audio`, and `websocket.StatusNormalClosure`
   teardown. The speech-to-speech executor is the **same client shape**
   with a wider event vocabulary and an audio-out path.

So the work is additive and well-precedented, not greenfield. The risk
is integration/latency (owned by #457 and already enumerated in
`docs/internal/design/voice-432-conductor-response-gate.md` section 6), not "can Go
speak this protocol." It can; we already do half of it in production.

GO-WITH-CAVEATS would only apply if `nhooyr.io/websocket` could not carry
the audio-out throughput or if the GA event schema lacked an explicit
no-VAD posture. Neither is true: the transport already streams realtime
audio in production, and `turn_detection: null` is a first-class GA
session value (section 3.1). NO-GO is not on the table.

Per-issue acceptance mapping is in section 8.

---

## 1. The question

Epic #449 is deleting the Python voice-agent and rebuilding it in Go.
The Python realtime path (epic #440) leaned entirely on the OpenAI LiveKit
plugin's `RealtimeModel`, a Python-only SDK that wraps the OpenAI Realtime
websocket, decodes its events, and bridges audio to the LiveKit
`AgentSession`. None of that exists for Go.

This spike de-risks the replacement: **can a Go process open a
`gpt-realtime` speech-to-speech session and drive it with the exact
posture the Python executor used?** That posture, established by the
merged spike #432 and implemented in the Python realtime executor, was:

- `turn_detection = None` -- the model never runs input VAD, never
  commits the input buffer, never self-triggers a response. memQL's
  conductor is the sole driver (#432 section 2.2, option A).
- Explicit `response.create` per conductor "engage" decision, with a
  per-response `instructions` string carrying the directive.
- Explicit `response.cancel` (+ output-audio clear) for barge-in, gated
  on a conductor floor-change read.
- Streaming audio in (human PCM) and out (model PCM, to be published into
  the LiveKit room per #451).
- Async function calling so a long-running tool does not freeze the
  audio session.

---

## 2. Client decision: hand-rolled `nhooyr.io/websocket`, not go-openai

### 2.1 go-openai does not cover Realtime

`go.mod` pins `github.com/sashabaranov/go-openai v1.41.2`. That library
is a REST client for the OpenAI HTTP surface (chat completions, images,
whisper batch transcription, `/v1/audio/speech` TTS, embeddings,
assistants, files, batch, moderation). It has **no** Realtime API:

- No websocket session type, no `RealtimeClient` / `RealtimeSession`.
- No `input_audio_buffer.append` / `.commit`, no `response.create` /
  `.cancel`, no `response.output_audio.delta` decoding.
- No realtime function-calling events.

Verified against the pinned version (no `realtime`-bearing source file in
the module cache) and the upstream README (no mention of "realtime",
websockets, or session events). Pulling a newer go-openai would not
change this materially -- realtime is not on its roadmap, and adopting a
new major just for an unsupported feature is worse than the alternative
below.

### 2.2 We already speak this protocol in Go

`integrations/openai/asr.go` is a hand-rolled OpenAI Realtime websocket
client. It opens `wss://api.openai.com/v1/realtime?intent=transcription`
over `nhooyr.io/websocket`, sets the `Authorization: Bearer <key>` and
`OpenAI-Beta: realtime=v1` headers, sends a config message, streams
base64 PCM16 audio in via `input_audio_buffer.append`, and runs a
`receiveLoop` that `json.Unmarshal`s each frame and switches on the
`type` field. It even handles the operational footguns the GA protocol
has (URL `intent` vs `model` form, `SetReadLimit`, close-frame error
surfacing).

The speech-to-speech executor is that same client with:

- the **conversation** session shape (`?model=gpt-realtime` instead of
  `?intent=transcription`), and
- a wider switch in `receiveLoop` (audio-out deltas, transcript deltas,
  function-call events, response lifecycle), plus
- an **audio-out** channel feeding #451's LiveKit publisher.

This is incremental work over a proven base, not a new integration.

### 2.3 Why a thin client beats a heavy abstraction

The Realtime protocol is ~20 event types over a single JSON websocket.
A hand-rolled client lets us:

- Map server events **directly** onto the memQL voice gRPC contract
  (`VoiceAgentRealtimeRespond` / `Cancel` / `Output` already exist;
  see #432 section 4 and `component/grpc/voice_agent_handlers.go`).
- Keep `turn_detection: null` and the single-`response.create`-emitter
  invariant explicit in our code, not buried under an SDK's auto-VAD /
  auto-response defaults that we would have to fight.
- Reuse `integrations/audio` resampling and the existing close/teardown
  discipline.

A heavy SDK (even if one existed for Go) would re-introduce exactly the
"the model owns turn-taking" assumption #432 spent a spike removing.

**Recommendation: build `integrations/openai/realtime.go` (speech-to-
speech) next to `asr.go`, sharing the dial/header/teardown helpers,
on `nhooyr.io/websocket`. No new dependency.**

---

## 3. The exact Realtime event sequence

All events are JSON objects on the single bidirectional websocket,
discriminated by a `type` string. Client events are sent with
`conn.Write(ctx, websocket.MessageText, data)`; server events arrive in
the `receiveLoop` and are dispatched on `type` -- identical to the
`asr.go` pattern.

Endpoint (GA conversation mode):

```
wss://api.openai.com/v1/realtime?model=gpt-realtime
Headers:
  Authorization: Bearer <OPENAI_API_KEY>
  OpenAI-Beta: realtime=v1
```

Note the difference from `asr.go`: transcription-only mode uses
`?intent=transcription` and a `transcription_session.update` event; the
speech-to-speech conversation mode uses `?model=<model>` and the
`session.update` event. Mixing the two (the exact footgun documented in
`asr.go`'s `StartStream` comment) makes OpenAI accept the upgrade then
immediately close the socket.

### 3.1 Session config with `turn_detection: null` (client -> server)

Sent once, immediately after dial. This is the conductor-gate posture --
the model never self-triggers.

```jsonc
{
  "type": "session.update",
  "session": {
    "type": "realtime",
    "instructions": "<static persona instructions>",   // realtime_instructions.build_persona_instructions
    "audio": {
      "input":  { "format": { "type": "audio/pcm", "rate": 24000 } },
      "output": { "format": { "type": "audio/pcm", "rate": 24000 },
                  "voice": "marin" }                    // realtime_output.resolve_realtime_voice
    },
    "turn_detection": null,                             // <-- option A: model NEVER self-triggers
    "tools": [ /* low-risk read-tool allowlist, section 5 */ ],
    "tool_choice": "auto"
  }
}
```

`turn_detection: null` is the load-bearing field. With it set, the server
does not run input VAD, does not auto-commit the input buffer, and does
not auto-create a response. Every `response.create` is ours.

(GA note: the GA session nests audio config under `audio.input` /
`audio.output`; the 2024 preview used flat `input_audio_format` /
`output_audio_format` / `voice` keys. The Go client targets the GA shape
under `?model=gpt-realtime`. `voice` and persona `instructions` come from
`realtime_instructions.build_session_persona`, ported to Go.)

### 3.2 Sending audio (client -> server)

Per audio chunk from the LiveKit input track (per #451's media plumbing),
upsampled to 24 kHz PCM16 exactly as `asr.go` does today via
`integrations/audio`:

```jsonc
{ "type": "input_audio_buffer.append", "audio": "<base64 PCM16 24kHz>" }
```

Under `turn_detection: null` the buffer is **never auto-committed**.
memQL commits it explicitly, driven from Deepgram finals (the parallel
STT that the conductor already consumes -- #432 section 2.5):

```jsonc
{ "type": "input_audio_buffer.commit" }
```

The commit appends the buffered audio to the conversation as a user item
but does **not** create a response -- response creation stays separate
and conductor-gated.

### 3.3 Conductor-driven `response.create` (client -> server)

Emitted only when the conductor's `routeOutcome.Respond == true` /
`PrimaryAgentId() != ""` (#432 section 2.3). The per-response
`instructions` carry the rendered directive (mode + brevity + angle) and
override the session default for this one response -- the Realtime analog
of the cascade's per-turn prompt:

```jsonc
{
  "type": "response.create",
  "response": {
    "instructions": "<RealtimeInstructionsForDirective(directive)>",
    "output_modalities": ["audio"]                     // audio + its transcript
  }
}
```

Exactly one `response.create` per "speak" decision; silence/defer emit
nothing. This is the gate.

Grounding (#436) is injected *before* the `response.create` as
conversation items, mirroring
`realtime_instructions.build_grounding_items` (a `system`-role
`conversation.item.create` with `input_text` content), so grounded
answers do not depend solely on a tool round-trip:

```jsonc
{
  "type": "conversation.item.create",
  "item": {
    "type": "message", "role": "system",
    "content": [{ "type": "input_text", "text": "<grounding block>" }]
  }
}
```

### 3.4 Barge-in `response.cancel` (client -> server)

On a conductor-scored floor-change while a response is in flight (#432
section 3). Two messages: cancel the generation, then clear already-
buffered output audio so playback stops *immediately*, not at the next
token boundary:

```jsonc
{ "type": "response.cancel" }
{ "type": "output_audio_buffer.clear" }
```

The `output_audio_buffer.clear` matters because the model may have
streamed several hundred ms of audio ahead of what has been played out
into the room; clearing flushes the unplayed tail.

### 3.5 Receiving audio + transcript + lifecycle (server -> client)

Dispatched in `receiveLoop` on the `type` string (GA event names):

| Server event `type`                          | Meaning / Go handling                                                                 |
|----------------------------------------------|----------------------------------------------------------------------------------------|
| `session.created` / `session.updated`        | Lifecycle ack. Log at INFO (same posture as `asr.go`).                                  |
| `response.created`                            | A response started. Stash `response.id` -> `ConductorState.RealtimeResponseId`; set `RealtimeResponseInFlight=true` (#432 step 3). |
| `response.output_audio.delta`                 | **Streamed model audio.** Base64 PCM16; decode and hand to the LiveKit publisher (#451). This is the first-audio-frame stamp T5 (#432 section 5.1). |
| `response.output_audio.done`                  | Audio for one item finished.                                                           |
| `response.output_audio_transcript.delta`      | Streamed text of what the model is saying. Accumulate (same `interimBuf` pattern as `asr.go`) for the utterance capture (#437). |
| `response.output_audio_transcript.done`       | Final transcript for the item -> forward as an AI utterance via `VoiceAgentRealtimeOutput` (mirrors `realtime_output.RealtimeOutputForwarder`). |
| `response.function_call_arguments.delta`      | Streamed tool-call arguments (one call). Accumulate per `call_id`.                      |
| `response.function_call_arguments.done`       | Tool call complete: `{ call_id, name, arguments }`. Dispatch async (section 5).         |
| `response.done`                               | Response finished. Clear `RealtimeResponseInFlight`. Also carries the consolidated output items (incl. any function calls) as a backstop if the streamed `.done` was missed. |
| `error`                                       | Surface `error.message` at ERROR (same as `asr.go`).                                    |

(GA renamed several events from the 2024 preview: preview
`response.audio.delta` / `response.audio_transcript.delta` became GA
`response.output_audio.delta` / `response.output_audio_transcript.delta`.
The Go client targets the GA names under `?model=gpt-realtime`; a thin
alias map can absorb the preview spellings if we ever pin a preview model
for testing.)

Audio out -> LiveKit room: the `response.output_audio.delta` PCM16 stream
is the input to #451's track-publish path. This spike treats #451 as the
consumer of an audio-frame channel (mirroring how `asr.go` exposes a
`results` channel); the executor owns producing decoded PCM frames + the
"first frame" trace stamp, #451 owns RTP/Opus encode + publish. The seam
is a Go channel of PCM frames, not a cross-process hop.

---

## 4. Concurrency model in Go (why audio never stalls)

The Python path gets async-for-free from `asyncio` + the LiveKit plugin.
In Go the same non-blocking property is structural and, again,
already-precedented in `asr.go`:

- **One reader goroutine** (`receiveLoop`) owns `conn.Read`. It never
  blocks on application work -- it decodes the event and hands off
  (audio frames onto a buffered channel to the publisher; transcript
  deltas onto an accumulator; function calls onto a worker, section 5).
- **Writes are mutex-guarded** (`asr.go` already does `s.mu.Lock()`
  around `conn.Write`). `response.create`, `response.cancel`,
  `input_audio_buffer.append/commit`, and `function_call_output` all go
  through the same guarded writer, so the conductor goroutine and the
  audio-input goroutine can both write safely.
- **Audio-out is a buffered channel.** `response.output_audio.delta`
  frames are pushed onto a buffered channel the publisher drains. A slow
  consumer applies backpressure without blocking the reader (bounded
  buffer + drop-oldest on overflow, same discipline as the cascade).

This is the Go shape of "long tools don't freeze audio": tool execution
never runs on the reader goroutine, so audio deltas keep flowing while a
tool is in flight.

---

## 5. Async function calling (long tools don't block audio)

The GA-model capability epic #440 relied on: the model can call a
function mid-response, the tool runs **asynchronously**, and audio keeps
streaming. In Go:

1. **Tool registration.** Tools are declared in `session.update.tools`
   (section 3.1) -- the same low-risk read-tool allowlist the Python MCP
   bridge exposes (#435: privileged tools are never exposed; default-
   deny). Each is `{ type: "function", name, description, parameters }`.
2. **Call surfaced.** When the model decides to call a tool, the server
   streams `response.function_call_arguments.delta` then emits
   `response.function_call_arguments.done` with `{ call_id, name,
   arguments }` (and the same call is consolidated in `response.done`'s
   output items as a backstop).
3. **Dispatch off the hot path.** The `receiveLoop` hands the
   `{ call_id, name, arguments }` to a **worker goroutine** (e.g.
   `go s.runTool(...)` or a small bounded worker pool) and returns
   immediately to reading. The MCP/cognition round-trip (the same bridge
   #435 wires) runs there. Audio deltas for the in-flight response
   continue to arrive and play out while the tool runs -- the reader is
   never blocked.
4. **Result returned.** When the tool completes, the worker writes (via
   the guarded writer) a `function_call_output` item, then a
   `response.create` so the model continues with the result in context:

   ```jsonc
   { "type": "conversation.item.create",
     "item": { "type": "function_call_output",
               "call_id": "<call_id>",
               "output": "<JSON string result>" } }
   { "type": "response.create" }
   ```

5. **Cancel vs in-flight tool (flagged).** The interaction of a barge-in
   `response.cancel` with a tool still running is the open question #432
   section 6 item 6 and #435 flag. Default: cancel the *response* (stop
   audio) but let the tool finish and drop its output, or tag the result
   stale by `call_id`. This is a #457 integration detail, not a
   feasibility blocker -- the protocol supports both.

**Demonstrability for the acceptance checklist:** a standalone Go harness
(no LiveKit needed) can open a `gpt-realtime` session with one slow stub
tool, send a canned audio clip + `response.create`, and assert that
`response.output_audio.delta` frames keep arriving on the channel during
the stub tool's `time.Sleep`. That isolates "async tool does not block
audio" from the full room wiring. (Requires a live `OPENAI_API_KEY`; it
is the live-credential follow-up, same caveat #432 carries.)

---

## 6. Integration plan (the seam)

This stays behind the **same executor-selection seam** the Python path
established. Realtime is now the default (#483); the cascade stays
available as an explicit opt-out and as the safe fallback, so the voice
path always comes up.

- **Selection flag.** `MEMQL_VOICE_EXECUTOR` (`cascade` | `realtime`),
  read today by `voice_agent/config.py` and consumed by
  `realtime_executor.select_voice_executor`. The Go voice agent keeps the
  identical env var; the default flipped to `realtime` (#483). The Go
  analog of `VoiceExecutorPlan` / `select_voice_executor` chooses between
  the Go cascade (#455) and the Go realtime executor (#457), with the same
  clean fallback-to-cascade-on-any-failure contract
  (`RealtimeExecutorError` -> cascade with a recorded reason).

- **New file.** `integrations/openai/realtime.go` -- the speech-to-speech
  websocket client described here, next to `asr.go`, sharing dial +
  header + teardown helpers. Exposes: an audio-in `SendAudio` /
  `CommitInput`, conductor controls `CreateResponse(instructions)` /
  `CancelResponse()`, an audio-out frame channel for #451, a transcript
  channel for #437, and a tool-call channel for #435.

- **Persona + grounding port.** `realtime_instructions.py`
  (`build_persona_instructions`, `resolve_realtime_voice`,
  `build_grounding_items`) and the voice catalog are pure/deterministic
  and port directly to Go next to the existing canonical-voice catalog
  (`integrations/voice/voices.go`, per epic #449's "what survives").

- **Conductor gate (unchanged).** The gate already lives in Go --
  `integrations/cognition/cognition_handler.go` +
  `conductor.go` (`RealtimeInstructionsForDirective`,
  `ConductorState.RealtimeResponseInFlight` / `RealtimeResponseId`), all
  specified in #432 section 4. The Go realtime executor is simply the
  thing the gate now drives directly, in-process, instead of pushing
  `VoiceAgentRealtimeRespond` / `Cancel` over gRPC to a separate Python
  runtime. (In a single Go binary the gRPC hop collapses to a function
  call; the proto messages remain valid for any out-of-process variant.)

- **Output capture (unchanged contract).**
  `response.output_audio_transcript.done` -> the same
  `VoiceAgentRealtimeOutput` insert path
  (`handleVoiceAgentRealtimeOutput`), so chat/canvas/conductor-history/
  audit render a Go realtime turn byte-identically, exactly as
  `realtime_output.RealtimeOutputForwarder` does today.

- **Audio out -> LiveKit (coordinate with #451).** The decoded PCM frame
  channel is #451's input. No implementation here; the contract is "the
  realtime executor produces a channel of 24 kHz PCM16 frames + a
  first-frame trace stamp."

Net: one new Go file + a persona-helper port + reuse of the existing
conductor gate, output-capture path, audio resampler, and executor-
selection contract. Additive; the cascade is untouched.

---

## 7. Risks / open questions

Feasibility is settled (GO). The remaining unknowns are integration- and
latency-shaped, and are already enumerated in
`docs/internal/design/voice-432-conductor-response-gate.md` section 6. The ones specific
to the **Go** realization:

1. **Audio-out throughput on `nhooyr.io/websocket`.** The transport
   already streams realtime audio *in* in production; *out* is symmetric
   and lower-rate (one speaker, not N humans). Low risk, but the live
   harness (section 5) should confirm no frame starvation under a real
   `response.output_audio.delta` stream.
2. **GA vs preview event spelling.** GA renamed audio/transcript delta
   events. Mitigated by targeting GA (`?model=gpt-realtime`) and an
   optional alias map. No blocker.
3. **`response_id` lifecycle races** (create -> cancel -> create on fast
   interruptions). Same discipline `handleVoiceAgentTurnRequest` already
   enforces via a turn key + sentinel; carry it forward. (#432 section 6
   item 5.)
4. **Cancel vs in-flight tool** (section 5 step 5 / #435 / #432 section 6
   item 6). Protocol supports it; semantics are a #457 decision.
5. **Live decision->first-audio latency.** The headline metric T5-T2 from
   #432 section 5 still requires a credentialed room; unchanged by the
   Go port and explicitly that spike's flagged follow-up.

None of these gate the verdict; they are the #457 build-out's known
surface.

---

## 8. Acceptance criteria mapping (issue #453)

- [x] **"Go client opens a gpt-realtime session, sends audio, receives
  audio, with `turn_detection:null`."** -> Design grounded in the
  existing in-production `integrations/openai/asr.go` realtime websocket
  client; the speech-to-speech variant is the same client with the
  conversation session shape, `turn_detection: null` (section 3.1),
  `input_audio_buffer.append` (3.2), and a `response.output_audio.delta`
  audio-out channel (3.5). **Live send/receive demonstration requires a
  credentialed `OPENAI_API_KEY` and is the flagged follow-up** (the same
  live-infra caveat #432 carries) -- the protocol path is fully
  specified.

- [x] **"`response.create` / `response.cancel` driven explicitly from
  Go."** -> sections 3.3 and 3.4; the single-`response.create`-emitter
  invariant is the conductor gate already implemented in Go
  (`cognition_handler.go` / `conductor.go`, #432 section 4).

- [x] **"Async tool call demonstrated without blocking audio."** ->
  section 5: tool calls dispatch to a worker goroutine off the reader
  goroutine, the guarded-writer + buffered-audio-channel model (section
  4) keeps `response.output_audio.delta` flowing during tool execution.
  A standalone Go harness with a slow stub tool isolates the proof
  (live-key follow-up).

- [x] **"Decision: go-openai vs hand-rolled ws client. Findings doc under
  `docs/voice/`."** -> **Hand-rolled on `nhooyr.io/websocket`, no new
  dependency** (section 2); this document is the findings doc.

Legend: [x] satisfied by this spike's design + decision; live send/recv
and the async-tool live proof are the flagged credentialed follow-ups
inherited from #432, to be executed under #457.
