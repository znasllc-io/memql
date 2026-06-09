---
title: Turn-taking / endpointing orchestration in Go (AgentSession replacement)
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Turn-taking / endpointing orchestration in Go (AgentSession replacement)

Spike deliverable for issue #452, part of epic #449 ("Replace the Python
voice-agent with a Go voice agent"). This is **Phase 0, long pole #2 -- the
riskiest piece** of the rewrite: replacing the single biggest thing the Python
LiveKit Agents framework gives us for free -- the `AgentSession` orchestration
that turns a raw audio stream into clean conversational turns.

Status: design + feasibility verdict, grounded in the current Go code. No
runtime behavior changes in this PR. Live-infra latency/quality validation
(a credentialed LiveKit room + Deepgram + a real speaker) is a flagged
follow-up; this spike says exactly which numbers only live testing can settle.

**VERDICT: GO-WITH-CAVEATS.** See [section 7](#7-feasibility-verdict).

> **Historical note (epic #449 complete).** The Go turn-taking machine this
> spike designed has shipped in the Go voice-agent
> (`integrations/voice/agent/turntaking.go`), and the Python LiveKit Agents
> `AgentSession` it replaced has been deleted. References below to the Python
> agent describe the spike's starting point, not the current tree.

---

## 1. Problem restatement

The Python agent (its `main.py`) built a LiveKit
`AgentSession` and handed it a VAD, an STT, an LLM, and a TTS. The framework
then runs, for free, the orchestration loop that copresent's voice product
depends on:

- gates audio frames through Silero VAD (`main.py:163-164`,
  `silero.VAD.load()`), so STT only sees speech, not room noise;
- runs Deepgram STT and decides, per the framework's turn detector, when a
  run of partials has become a committed user turn
  (`stt_plugin.py`, `endpointing_ms`);
- emits a `user_input_transcribed` event with `is_final`
  (`stt_plugin.py:84-100`) that the rest of the app keys turn boundaries off;
- detects a human starting to speak mid-assistant-reply and interrupts the
  in-flight TTS (barge-in -- the framework's interruption handling);
- exposes `session.say(text)` (`main.py:342`) and `generate_reply(...)` so an
  external caller can drive "speak now."

Epic #449 deletes the Python runtime. Everything in that list must be rebuilt
in Go. This spike de-risks the orchestration itself -- **can a robust enough
turn-taking / endpointing state machine be built in Go** from the signals the
existing Go Deepgram client exposes (plus, if needed, a Go VAD), supporting an
externally-driven (conductor) "speak now" trigger and clean barge-in?

The conductor-gate posture is not optional context -- it is the defining
constraint. Epic #440 already established (see
`docs/internal/design/voice-432-conductor-response-gate.md`) that **memQL's conductor decides
WHEN the assistant speaks**, not raw VAD. A copresent polyphon room is up to 5
humans + 1 assistant; humans talk to each other constantly. The Python agent
runs in exactly this gated posture today: the realtime executor is built with
`turn_detection=None` (`main.py:242-247`) and the assistant speaks only when
memQL pushes a `VoiceAgentSpeak` (`main.py:313-363`) or a turn directive. So
the Go state machine inherits a hard requirement: **it must NOT auto-respond on
every pause.** End-of-utterance detection produces a *transcript boundary* that
flows to cognition; whether the assistant then speaks is a separate, externally
driven decision.

---

## 2. What the Go code already gives us (grounded)

### 2.1 The Deepgram Go client and its events

`integrations/deepgram/deepgram.go` + `integrations/deepgram/asr.go` are a
complete, production-tuned Nova-3 streaming client that speaks the Deepgram
WebSocket directly (no LiveKit plugin in the middle). The dial URL
(`deepgram.go:asrStreamURL`, lines 202-237) already requests every endpointing
signal we need:

```go
q.Set("interim_results", "true")
q.Set("vad_events", "true")        // SpeechStarted events
q.Set("endpointing", strconv.Itoa(c.EndpointingMs))
if c.UtteranceEndMs > 0 {
    q.Set("utterance_end_ms", strconv.Itoa(c.UtteranceEndMs))  // UtteranceEnd
}
```

Critically -- and this is the difference from the Python path documented in
`docs/public/operate/voice-eou-tuning.md` -- the Go client speaks Deepgram raw, so it **does**
honor `utterance_end_ms` (the LK plugin does not). The Go client therefore has
*more* turn-boundary signal available than the Python agent it replaces.

The receive loop (`asr.go:handleEvent`, lines 327-373) sees four Deepgram
event types. Here is exactly what each is and what the client does with it
today:

| Deepgram event (`type`)         | Go symbol / handler                       | Meaning                                                            | What the client does with it TODAY                                              |
|---------------------------------|-------------------------------------------|-------------------------------------------------------------------|---------------------------------------------------------------------------------|
| `SpeechStarted` (`vad_events`)  | `case "SpeechStarted"` (`asr.go:361-367`) | VAD says voice activity just began.                               | **Logged only** (`stage=deepgram.speech_started`). Not surfaced to callers.     |
| `Results` (interim)             | `handleResults` (`asr.go:410-451`)        | A partial hypothesis; `is_final=false`.                          | Updates `lastInterimText`; dispatches `ASRResult{IsFinal:false}`.               |
| `Results` (`is_final=true`)     | `handleResults` (`asr.go:410-451`)        | A phrase has stabilized (driven by `endpointing`).               | Appends to `pendingFinalText`; still dispatches `IsFinal:false` downstream.     |
| `UtteranceEnd`                  | `handleUtteranceEnd` (`asr.go:463-509`)   | VAD-driven end-of-utterance after `utterance_end_ms` of silence. | Flushes accumulator as a single `ASRResult{IsFinal:true}`.                       |
| `Metadata`                      | `case "Metadata"` (`asr.go:368-369`)      | Keepalive / housekeeping.                                        | Debug-logged, ignored.                                                           |

On top of Deepgram's own VAD, the Go client adds a **client-side EOU watchdog**
(`asr.go:watchdogLoop`, lines 235-268; `defaultClientEOUTimeoutMs = 2500`).
This force-fires `handleUtteranceEnd` when the interim transcript has been
static for N ms -- a real-world backstop for Deepgram's documented failure mode
where ambient noise keeps its VAD active and `UtteranceEnd` never fires
(`deepgram.go:70-86` documents observed 11-21 s hangs). This watchdog is a piece
of robustness the Python/LiveKit path did **not** have, and it already lives in
Go. It is load-bearing for the verdict: the riskiest endpointing failure mode is
already mitigated in the code we are building on.

### 2.2 The crucial gap: the client collapses turn structure to one bit

The `polyphon.ASRStream` contract (`component/polyphon/providers.go:19-63`)
exposes exactly one output: a channel of `ASRResult{Text, IsFinal, Confidence,
SpeakerId}`. The Deepgram client maps the rich Deepgram event stream down to
this single `IsFinal` bit:

- `SpeechStarted` is **dropped** (logged, never surfaced) -- so a *consumer of
  the stream cannot see "the human just started talking."* That signal is the
  trigger for barge-in.
- The distinction between `Results.is_final=true` (phrase stabilized) and
  `UtteranceEnd` (turn over) is **collapsed**: both feed `handleUtteranceEnd`,
  and only the EOU produces a downstream `IsFinal:true`.

This is the right shape for the existing transcript-forwarding consumer
(`polyphonASRSession`, `integrations/stt/polyphon_session.go`), which only needs
"interim vs final." It is **not** enough for a turn-taking state machine, which
needs start-of-speech as a first-class event to drive barge-in. Closing this gap
is the core of the spike's build plan (section 5).

### 2.3 The memQL-side contract already exists end to end

The entire memQL-side voice contract is in Go and stays untouched
(epic #449 "what survives"): `component/grpc/voice_agent_handlers.go` (~1,700
LOC) handles `VoiceAgentSessionStart/End`, `VoiceAgentPartialTranscript`,
`VoiceAgentFinalTranscript`, `VoiceAgentTurnRequest/Delta/Complete`, and the
unsolicited `VoiceAgentSpeak` push. The Go agent calls these directly. Key
seams the state machine binds to:

- `handleVoiceAgentFinalTranscript` (`voice_agent_handlers.go:558`) -- where a
  committed user turn lands and fires the cognition automation that runs the
  suppression classifier + conductor consult.
- `startVoiceAgentSpeakSubscriber` (`voice_agent_handlers.go:399`) +
  `VoiceAgentSpeak` (line 482) -- the existing **externally-driven "speak now"**
  push. This is the conductor-gate trigger already wired; the Go state machine's
  "assistant-turn" transition is driven by receiving this message, exactly as
  the Python `_on_voice_agent_speak` handler does today (`main.py:313`).
- `handleVoiceAgentTurnRequest` (`voice_agent_handlers.go:645`) -- the
  one-active-turn discipline (`turnKey` + sentinel, lines 717-743) that the
  barge-in cancellation must reuse so create/cancel/create races stay clean.

---

## 3. The target state machine

A single per-room (per-human-track under #433) state machine owns turn-taking.
States and the events that drive each transition:

```
                  uiRequestControl-free; one machine per attributed audio track

        +-----------+   session.start    +-------------+
        |   idle    | -----------------> |  listening  |
        +-----------+                    +-------------+
              ^                             |        ^
              | session.end                | (A)     | (D) EOU flush, no speak
              |                             v         | directive in window
              |                       +-------------+ |
              |                       | human-turn  |-+
              |                       +-------------+
              |                             |
              |                  (B) conductor "speak now"
              |                     (VoiceAgentSpeak / RealtimeRespond)
              |                             v
        +--------------+  (C) barge-in  +----------------+
        | assistant-   | <------------- | assistant-turn |
        | turn (done)  |  cancel        | (interruptible)|
        +--------------+                +----------------+
              |   playout complete / cancelled               |
              +----------------------------------------------+
                              back to listening
```

State definitions:

- **idle** -- no live session. Entered at construction and on session teardown.
- **listening** -- session live, no human currently speaking. The default
  resting state. STT stream is open; audio frames flow to Deepgram.
- **human-turn** -- a human is actively speaking. Entered on **start-of-speech**
  (transition A). Interim partials accumulate. The machine is recording a turn
  boundary; it is **not** deciding whether the assistant replies.
- **assistant-turn (interruptible)** -- the assistant is producing audio
  (cascade TTS or realtime audio). Entered ONLY on an external "speak now"
  directive (transition B). This is the conductor-gate invariant: a human EOU
  alone never enters this state.
- **assistant-turn (done)** -- the assistant finished or was cancelled; collapses
  back to listening.

Transitions:

- **(A) listening -> human-turn**: Deepgram `SpeechStarted` (or, if VAD is added,
  a VAD speech-onset). This is the event the current client drops and the spike
  must surface (section 2.2).
- **(D) human-turn -> listening**: end-of-utterance flush -- Deepgram
  `UtteranceEnd`, OR the client-side EOU watchdog, OR a `Results.is_final`
  followed by silence. The committed transcript is forwarded as a
  `VoiceAgentFinalTranscript`. **No assistant turn is auto-started here** -- this
  is the difference from the framework's auto-VAD turn detector. The boundary is
  reported; the speak decision is deferred to the conductor.
- **(B) listening / human-turn -> assistant-turn**: an external "speak now"
  arrives -- `VoiceAgentSpeak` (cascade) or `VoiceAgentRealtimeRespond` (realtime,
  #432). This is the conductor's gate decision. The machine begins assistant
  audio output and arms barge-in.
- **(C) assistant-turn -> assistant-turn(done) [barge-in]**: while the assistant
  is speaking, a human start-of-speech (transition A's trigger, evaluated in the
  assistant-turn state) cancels the in-flight assistant turn. Cancellation
  semantics differ by executor:
  - **cascade**: stop pumping TTS chunks to the LiveKit publish track and flush
    the local audio buffer (cut the Aura-2 stream).
  - **realtime**: emit `response.cancel` + `output_audio_buffer.clear` on the
    OpenAI Realtime data channel (per `432-conductor-response-gate.md` section 3).

  Per the polyphon-correctness rule from #432: in a multi-human room, *whether*
  a given human onset should interrupt is ultimately a conductor read
  (floor-change vs side chatter). The state machine provides the **mechanism**
  (it can cancel within one frame budget and exposes "assistant has the floor +
  active response_id"); the **policy** of "is this onset a real interruption"
  is the conductor's, exactly as `432`'s `RealtimeResponseInFlight` /
  floor-change hook describes. For a 1:1 standard space, any human onset during
  assistant-turn is an interruption (the simple, correct default).

The machine is a small explicit-state struct guarded by a mutex, fed by two
input sources: (1) the Deepgram event stream (enriched per section 5.1), and
(2) the external control channel (`VoiceAgentSpeak` / realtime directives). It
emits: `VoiceAgentPartialTranscript`, `VoiceAgentFinalTranscript`, and
assistant-audio start/stop control to the executor. This is a few hundred lines
of ordinary Go -- no framework, no Python.

---

## 4. VAD decision: Deepgram endpointing only vs +Silero

### 4.1 The two signals, and what each actually decides

There are two distinct jobs people lump under "VAD":

1. **End-of-utterance (turn boundary)** -- "the human is done; commit the
   transcript." This is the high-value decision. In the Go path it is owned by
   **Deepgram** (`UtteranceEnd` / `endpointing` / `is_final`) plus the
   **client-side watchdog**. Deepgram's endpointing is *signal-aware* (it knows
   mid-phrase vs done from the acoustic + language model), which is precisely why
   `docs/public/operate/voice-eou-tuning.md` section "Why not just use VAD-based turn detection"
   concluded that frame-level VAD turn detection produced *worse* cut-offs than
   Deepgram endpointing. That conclusion carries straight over to Go.

2. **Frame-level speech/non-speech gating** -- "is this 20 ms frame speech or
   noise." In the Python path Silero did this to gate frames into Deepgram
   (`main.py:163-164`). Two things it bought there are worth examining for Go:
   - **Cost gating**: only send speech frames to Deepgram (fewer billed
     audio-seconds, less ambient-noise confusion).
   - **Barge-in onset**: a fast local "human started" signal.

### 4.2 Decision: ship **Deepgram-endpointing-only** for v1; keep Silero as a flagged option

Rationale:

- **EOU is Deepgram's job and it is better at it than frame VAD.** This is
  settled by the existing tuning doc's own conclusion and by the production
  defaults already baked into `deepgram.go`. Adding Silero does **not** improve
  turn boundaries; the tuning lever is Deepgram's `endpointing` /
  `utterance_end_ms` + the watchdog, all already in Go.

- **Start-of-speech for barge-in is already available from Deepgram** --
  `SpeechStarted` (the client just drops it today). Surfacing that event
  (section 5.1) gives barge-in onset *without* a second model. Deepgram's
  `SpeechStarted` is "good enough" for cutting the assistant: a false onset cuts
  the assistant a beat early; the conductor floor-change read (#432) is the real
  policy gate, so an over-eager acoustic onset does not by itself produce a wrong
  interruption in a multi-human room.

- **Silero-in-Go is a real dependency cost.** The mature Go bindings
  (`streamer45/silero-vad-go`, `plandem/silero-go`) are **CGO + ONNX Runtime**
  (e.g. `streamer45` needs GCC and ONNX Runtime v1.18.1 + the Silero v5 model
  file). That pulls a C toolchain and a native shared library into the
  voice-node build and Docker image -- directly at odds with epic #449's headline
  goal ("eliminate the only non-Go runtime... pull voice behind the same Go
  build-tag node model"). Introducing CGO/ONNX to win a signal Deepgram already
  provides is a poor trade for v1.

- **Cost gating is a second-order optimization, not a turn-taking requirement.**
  If billed Deepgram audio-seconds during silence become a real cost problem, a
  pure-Go energy gate (RMS threshold, ~30 lines, no model) can suppress obvious
  silence before the WebSocket send. That is a far cheaper lever than a CGO model
  and keeps the build pure-Go.

**When to revisit Silero:** if live testing shows Deepgram `SpeechStarted`
onset latency is too slow for crisp barge-in (the one number only live infra can
settle -- see section 7), a Go Silero VAD becomes the fallback for *onset
detection only* (not EOU). Gate it behind a build tag / env flag so the default
voice node stays pure-Go. Candidate libraries, ranked:

| Library                       | Binding | Deps                          | Notes                                                  |
|-------------------------------|---------|-------------------------------|--------------------------------------------------------|
| `streamer45/silero-vad-go`    | CGO     | ONNX Runtime v1.18.1, Silero v5 | Most maintained; used in production speech stacks.     |
| `plandem/silero-go`           | CGO     | `yalue/onnxruntime_go`, miniaudio | Lighter wrapper; real-time + file modes.               |
| pure-Go RMS energy gate (DIY) | none    | none                          | Cost-gate only; cannot do semantic onset. Build first. |

**Net VAD decision: Deepgram endpointing only for v1. No Silero in the default
build.** Start-of-speech comes from surfacing Deepgram's existing
`SpeechStarted`. Silero is a flagged, CGO-gated fallback reserved strictly for
barge-in onset latency if live testing demands it.

---

## 5. Integration plan (file-by-file, real symbols)

New code is additive and lives on the Go voice node (`-tags voice`,
`app/build_voice.go`). The memQL-side gRPC contract
(`voice_agent_handlers.go`) is reused verbatim.

### Step 1 -- Surface start-of-speech and turn structure from the Deepgram client

The client currently drops `SpeechStarted` and collapses turn structure to one
`IsFinal` bit (section 2.2). Two options; the spike recommends **option A** for
backward-compatibility:

- **Option A (recommended) -- enrich `ASRResult` additively.** Add an event-kind
  discriminator to `polyphon.ASRResult` (`component/polyphon/providers.go:58`),
  e.g. a `Kind` field (`speech_started` | `interim` | `final`) defaulting to the
  current interim/final behavior so existing consumers
  (`polyphonASRSession.forwardResults`) are unaffected. In
  `asr.go:handleEvent`, the `case "SpeechStarted"` (line 361) dispatches an
  `ASRResult{Kind: SpeechStarted}` instead of only logging. The turn-taking
  machine consumes `Kind`; the transcript forwarder ignores it.

- **Option B -- a dedicated `Events()` channel** on a turn-taking-specific
  stream interface (start-of-speech, EOU, barge-in-candidate) separate from
  the transcript `Results()` channel. Cleaner separation but more surface area.
  Defer unless option A's overloaded result proves awkward.

Either way, the watchdog (`asr.go:watchdogLoop`) and EOU flush
(`handleUtteranceEnd`) are reused unchanged -- they already produce the correct
turn-end signal.

### Step 2 -- The turn-taking state machine (new package)

New package, e.g. `integrations/voice/turntaking/` (or
`component/polyphon/turntaking`), exposing the section-3 machine:

- `type Machine struct { state State; ... }` with the five states.
- Driven by an input loop that ranges over the enriched Deepgram stream
  (step 1) and a control channel for external directives.
- On (A) `SpeechStarted`: -> human-turn; if currently assistant-turn, raise a
  barge-in candidate (step 4).
- On (D) EOU: forward `VoiceAgentFinalTranscript` via the existing gRPC client
  call; -> listening. **Never** auto-enters assistant-turn.
- On (B) external "speak now": -> assistant-turn; start executor audio.
- Reuses the `turnKey` + sentinel one-active-turn discipline from
  `handleVoiceAgentTurnRequest` (`voice_agent_handlers.go:717-743`) so
  rapid create/cancel/create stays race-free.

### Step 3 -- Wire the external "speak now" trigger (conductor gate)

The Go agent registers a handler for the inbound `VoiceAgentSpeak` server push
(the Go analog of Python's `_on_voice_agent_speak`, `main.py:313-363`). On
receipt, it drives transition (B). For the realtime executor, the #432 messages
(`VoiceAgentRealtimeRespond` / `VoiceAgentRealtimeCancel`, defined in that
design) drive (B) and (C) respectively. **No VAD-derived auto-response path
exists** -- this is the acceptance-critical "externally-driven speak trigger,
no auto-VAD response."

### Step 4 -- Barge-in cancellation

In assistant-turn, a `SpeechStarted` from a human track raises a barge-in
candidate. The machine:

- **cascade**: stops the TTS->publish pump and clears the local audio buffer.
- **realtime**: sends `response.cancel` + `output_audio_buffer.clear`
  (per `432-conductor-response-gate.md` section 3).

Whether the candidate actually fires is conductor-gated in a polyphon room
(floor-change vs side chatter, #432); for a 1:1 standard space the candidate
always fires. The machine exposes "assistant has the floor + active response_id"
state so the cognition-side floor-change hook (#432 step 4) can drive the
decision.

### Step 5 -- LiveKit participation feeds the audio (cross-spike, #450)

The state machine consumes PCM16 frames from the room. Joining the LiveKit room
as a media participant is long-pole #1 (#450, `server-sdk-go`); this spike
assumes #450 delivers a per-track PCM16 frame source. The machine's `SendAudio`
side simply forwards those frames into the existing
`deepgramASRStream.SendAudio` (`asr.go:150-174`).

### Step 6 -- New code / changed symbols summary

| Where | What |
|-------|------|
| `component/polyphon/providers.go` | Additive `Kind` field on `ASRResult` (+ const set). Backward-compatible default. |
| `integrations/deepgram/asr.go` | `case "SpeechStarted"` dispatches `ASRResult{Kind: SpeechStarted}` instead of log-only. Watchdog + EOU flush unchanged. |
| `integrations/voice/turntaking/` (new) | The five-state `Machine`; input loop over the enriched stream + control channel; barge-in mechanism; reuses `turnKey` discipline. |
| Go voice agent entrypoint (#449 skeleton) | Registers the `VoiceAgentSpeak` push handler (transition B); forwards committed turns via `handleVoiceAgentFinalTranscript`'s wire message. |
| (realtime only, from #432) | `VoiceAgentRealtimeRespond` / `VoiceAgentRealtimeCancel` drive B / C. |

Nothing here deletes or alters the memQL-side handlers, the transcript
forwarder, the conductor, or the cascade vs realtime executor selection. The
Deepgram client change is additive (one dropped event becomes a surfaced event).

---

## 6. Latency / quality measurement plan (flagged follow-up)

The numbers that decide whether GO-WITH-CAVEATS becomes plain GO can only be
read on live infra (credentialed Deepgram + a LiveKit room + a real speaker).
Reuse the existing structured voice-trace logging convention (`stage` +
`voiceTrace: voice:<spaceId>`; see `432-conductor-response-gate.md` section 5
and the existing `asr.go` `"voice trace: deepgram event"` lines).

Stamps to emit (monotonic clock):

| Stamp | Where | `stage` |
|-------|-------|---------|
| Human speech onset (acoustic) | enriched `SpeechStarted` dispatch (`asr.go:361`) | `turntaking.speech_started` |
| First interim | first non-empty `handleResults` (`asr.go:410`) | `turntaking.first_interim` |
| EOU flush | `handleUtteranceEnd` (`asr.go:463`) | `turntaking.eou` |
| Barge-in onset detected | (C) in the machine | `turntaking.bargein.detected` |
| Assistant audio cut | executor cancel return | `turntaking.bargein.audio_cut` |

Headline numbers:

1. **EOU quality vs the framework.** False-final rate (user kept talking right
   after EOU) and median tail latency (last word -> EOU), measured against the
   Python/LiveKit baseline on the same utterances. This is THE quality question:
   is Go-orchestrated Deepgram endpointing at least as good as the framework's?
2. **Barge-in latency.** `turntaking.bargein.audio_cut` - human onset. Target:
   assistant audio stops within ~1 frame budget of a real interruption.
3. **Start-of-speech onset latency.** Acoustic onset -> `SpeechStarted`. If this
   is too slow for crisp barge-in, the Silero fallback (section 4.2) is the
   mitigation.

Test scenarios mirror the acceptance checklist: single human turn; rapid-fire
turns (snappy endpointing); long deliberate pauses (no premature split);
mid-assistant interjection (barge-in cancels); pure side-chatter in a multi-human
room (conductor suppresses -- no auto-response). A small Go test harness that
replays canned WAVs through `deepgramASRStream.SendAudio` and scrapes the trace
lines computes these without standing up the full room.

**[WARNING] The live measurement itself requires real Deepgram credentials and a
LiveKit room and is the flagged follow-up; it is not executed within this
spike.**

---

## 7. Feasibility VERDICT

### GO-WITH-CAVEATS

A robust turn-taking / endpointing state machine **can** be built in Go, and
most of the hard parts already exist in the Go tree. This is the riskiest spike,
so the honest breakdown:

**Why GO (the de-risking landed):**

- The hardest signal -- **end-of-utterance** -- is already owned by Deepgram in
  Go, with production-tuned knobs and, crucially, a **client-side EOU watchdog**
  (`asr.go:235-268`) that handles Deepgram's worst real-world failure mode. The
  Go client honors `utterance_end_ms` (the Python LK plugin did **not**), so the
  Go path has *more* turn-boundary signal than the agent it replaces.
- The **externally-driven "speak now"** trigger is already wired end to end:
  `VoiceAgentSpeak` (`voice_agent_handlers.go:399-499`) is exactly the
  conductor-gate push the Python agent consumes today. The Go machine just binds
  the same handler. The conductor-gate posture (no auto-VAD response) is
  therefore *natively* satisfied -- it is the default, not a feature to add.
- The **state machine itself is ordinary Go** -- a few hundred lines, no
  framework, no Python. No novel concurrency beyond what `asr.go` already does.
- **VAD is settled**: Deepgram-endpointing-only, no Silero in the default build.
  No new native dependency, consistent with epic #449's pure-Go goal.

**The CAVEATS (why not unqualified GO):**

1. **One real code gap exists.** The Deepgram client **drops `SpeechStarted`**
   and collapses turn structure to a single `IsFinal` bit
   (section 2.2). Barge-in onset needs that event surfaced. This is a small,
   well-understood additive change (section 5, step 1) -- but it is a genuine gap,
   so the verdict is not "already works," it is "works after this specific,
   bounded change."

2. **EOU quality parity vs the framework is the one thing only live testing can
   settle.** The Python LiveKit turn detector is mature; whether Go-orchestrated
   Deepgram endpointing *feels* as good (false-final rate, tail latency) on real
   speech is unproven until measured (section 6, number 1). The mitigations all
   exist as knobs (`endpointing`, `utterance_end_ms`, watchdog timeout), so the
   risk is "needs tuning," not "needs new architecture" -- but it is real and it
   is why this is the riskiest long pole.

3. **Barge-in onset latency from Deepgram `SpeechStarted` is unmeasured.** If it
   proves too slow for crisp interruption, the fallback (Go Silero VAD, CGO-gated,
   onset-only -- section 4.2) is identified but introduces a native dependency.
   This is a known, bounded fallback, not an open question -- but it is a caveat.

4. **Cross-spike dependencies.** Audio frames depend on LiveKit participation
   (#450); multi-human per-track attribution depends on #433; the realtime
   barge-in path depends on #432's `VoiceAgentRealtimeCancel`. None block the
   state machine's design; all must land for the full system. The state machine
   is buildable and unit-testable today against canned WAVs without any of them.

**No NO-GO findings.** Nothing in the investigation surfaced a signal that
Deepgram cannot provide, a concurrency pattern Go cannot express, or a framework
behavior with no Go equivalent. The single gap (dropped `SpeechStarted`) is
trivially closeable. The verdict is GO-WITH-CAVEATS rather than GO purely
because EOU felt-quality parity and barge-in onset latency are honest
unknowns that only live testing settles -- and on the riskiest spike, those are
worth stating plainly rather than papering over.

---

## 8. Acceptance criteria mapping

Issue #452 checklist, each mapped to where this design satisfies it:

- [x] **"Go state machine produces correct turn boundaries from a live Deepgram
  stream."** -> The five-state machine (section 3) consumes the enriched
  Deepgram stream; EOU is Deepgram `UtteranceEnd` + the existing client watchdog
  (`asr.go:463`, `asr.go:235`), already producing correct boundaries in Go.
  Felt-quality parity is the flagged live measurement (section 6, number 1).
- [x] **"Barge-in cancels an in-flight assistant turn."** -> transition (C),
  section 3 + section 5 step 4; cascade buffer-flush / realtime `response.cancel`
  + `output_audio_buffer.clear`. Requires surfacing `SpeechStarted` (step 1);
  cut latency is the flagged measurement (section 6, number 2).
- [x] **"Externally-driven speak trigger works (no auto-VAD response)."** ->
  transition (B) is driven ONLY by `VoiceAgentSpeak` / `VoiceAgentRealtimeRespond`
  (section 5 step 3). EOU never auto-enters assistant-turn (section 3, transition
  D). This is the conductor-gate posture, already the default.
- [x] **"Decision documented: Deepgram endpointing only vs +Silero VAD; latency
  measured."** -> section 4: Deepgram-endpointing-only for v1, Silero as a
  CGO-gated onset-only fallback, with full rationale. Latency *methodology* +
  instrumentation points are specified (section 6); the live numbers are the
  flagged follow-up (section 7, caveats 2-3).
- [x] **"Findings doc under `docs/voice/`."** -> this document.

Legend: [x] satisfied by design / decision; live-infra measurement is flagged
throughout as the explicit follow-up (sections 6-7).
