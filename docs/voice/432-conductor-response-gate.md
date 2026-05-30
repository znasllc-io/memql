# Conductor-driven response gate for multi-party Realtime turn-taking

Spike deliverable for issue #432, part of the Hybrid Realtime Voice epic #440
(Option B: memQL is the director, OpenAI `gpt-realtime` is the fast voice
executor).

Status: design + integration plan, grounded in the current code. Live-infra
latency validation (LiveKit + OpenAI Realtime + Deepgram with real
credentials and a real room) is a flagged follow-up -- see
[Latency measurement plan](#5-latency-measurement-plan). This document does
not change runtime behavior.

Docs location note: this epic has no pre-existing folder. `docs/voice/`
already holds the cascade-era voice docs (`eou-tuning.md`,
`bringup-verification.md`); those describe the Deepgram STT -> cognition ->
Deepgram TTS path. The Realtime work is a distinct executor, so it gets its
own `docs/realtime-voice/` tree to avoid muddying the cascade docs. The two
sets cross-reference where the EOU / turn-detection seams overlap.

---

## 1. Problem restatement

The OpenAI Realtime API ships two built-in turn-detection modes on the
session's `turn_detection` field:

- `server_vad` -- the server watches the input audio and fires a response
  whenever it detects the user stopped talking (a silence threshold).
- `semantic_vad` -- same, but it uses a model to estimate whether the
  speaker is *semantically* finished, not just silent.

Both assume a **1:1 conversation**: there is exactly one "user", and any time
that user stops, the assistant should answer. The model owns the decision of
*when* to speak.

In a copresent polyphon room that assumption is wrong. A room is **up to 5
humans + 1 assistant** (the owner's single active assistant; see epic #440
non-goals). Humans talk to *each other* constantly. With default VAD the
assistant would barge in every time any human pauses -- on side chatter, on a
human asking another human a question, on a thinking pause. The acceptance
test for this spike states it directly:

> "5 humans talking, one asks the assistant" -> assistant engages only when
> the conductor detects it's addressed/relevant.

memQL already solves exactly this decision for the **text/cascade** path. The
conductor (`integrations/cognition/conductor.go` +
`integrations/cognition/conductor_consult.go`) observes utterances +
presence + Polyphon scoring and decides *who* responds, *whether* anyone
responds at all (silence is a first-class outcome), and *how* (mode +
brevity). The Realtime executor must inherit that decision instead of
re-deriving it from raw VAD.

So the fit question this spike de-risks: **can memQL's conductor drive WHEN
the Realtime model speaks, replacing the model's built-in VAD?** The answer
below is yes, and the mechanism is: disable the model's auto-response, and
drive `response.create` explicitly from the conductor's directive.

---

## 2. Design

### 2.1 The decision already exists in Go

The conductor's per-turn output is `ConductorPlan`
(`integrations/cognition/conductor_consult.go:103`). The load-bearing fields
for this spike:

- `Primary ConductorAgentPlan` -- the one agent that leads. `PrimaryAgentId()`
  returns `""` when the conductor chose **silence** (a legitimate, common
  outcome).
- `ChimeIns []ConductorAgentPlan` / `Sequence []ConductorAgentPlan` -- other
  voices on the turn (only relevant in multi-agent rooms; out of scope under
  the single-assistant model but the plumbing is shared).
- `FitScore float64`, `TurnMode string`, `Reason string` -- routing metadata.

This plan is consumed in `cognition_handler.go` around line 597-620:

```go
var conductorPlan *ConductorPlan
if !isVoiceUtteranceEarly && routeOutcome == nil {
    plan, planErr := c.consultConductor(ctx, scoringUtterance, candidates, ...)
    routeOutcome = routingOutcomeFromConductorPlan(plan, candidates)
    ...
}
```

`routingOutcomeFromConductorPlan` (`conductor_consult.go:224`) collapses the
plan into a `routingOutcome` with a boolean `Respond`. When
`PrimaryAgentId() == ""`, it returns `&routingOutcome{Respond: false}` and the
handler emits idle presence and **returns without inserting a reply**
(`cognition_handler.go:635-653`). That `Respond` boolean is the response gate
we need -- today it gates inserting a chat utterance; for Realtime it must
gate `response.create`.

Per-agent shaping (`Mode` + `Brevity` + `Instruction`) lives on
`AgentParticipationDirective` (`conductor.go:106`) which the dispatch path
builds via `BuildDirective` (`conductor.go:512`) merged with the LLM
conductor's `ConductorAgentPlan.Instruction` / `Brevity`. Today these are
serialized into the `AgentGenerateTurnMsg.Hints` map
(`EncodeDirectiveIntoHints`, `conductor.go:585`) and re-decoded agent-side
(`integrations/agent/prompt_data.go:260 buildDirectiveMap`) to template the
text prompt. For Realtime we render the same directive into the per-response
`instructions` string instead.

### 2.2 Realtime session configuration: disable model auto-response

Two viable session configurations; the spike recommends **option A** and
keeps **option B** as a tuning lever.

**Option A (recommended) -- `turn_detection: null`.** The model never runs
input VAD, never commits the input buffer on its own, and never auto-creates
a response. memQL becomes the sole driver of both:

- input commit -- driven from Deepgram finals (we already have these; see
  2.5), via `input_audio_buffer.commit`.
- response creation -- driven from the conductor, via `response.create`.

This is the cleanest mapping to the acceptance criterion "the model never
self-responds; every response is conductor-triggered." There is exactly one
code path that creates a response, and it is gated on the conductor.

**Option B -- input VAD for interruption only.** Set
`turn_detection: { type: "server_vad", create_response: false,
interrupt_response: true }` (the Realtime API exposes `create_response` and
`interrupt_response` toggles on the VAD object). Here the model still runs
input VAD, but `create_response: false` means a detected end-of-speech does
NOT auto-create a response -- memQL still owns `response.create`. The win is
`interrupt_response: true`: the model auto-cancels its own in-flight audio the
instant it hears a human start, without a round-trip to memQL. The risk is
that "hears a human start" includes humans talking to *each other*, so the
assistant's audio would cut out on unrelated cross-talk. For a polyphon room
that is usually wrong (we want the assistant to keep talking over side
chatter unless the conductor says to yield), so option A is the default and
interruption is conductor-driven (2.3). Option B stays available as a
per-room tuning lever if conductor-round-trip interruption latency proves too
slow in live testing.

Either way, the invariant is: **`create_response` is never left to the model.**

### 2.3 How the conductor drives `response.create`

The mapping from conductor directive to Realtime control messages:

| Conductor outcome (`ConductorPlan` / directive)        | Realtime action                                        |
|--------------------------------------------------------|--------------------------------------------------------|
| `PrimaryAgentId() == ""` (silence)                     | **suppress** -- send nothing; no `response.create`.    |
| `DirectivePrimary`                                     | `response.create` with primary `instructions`.         |
| `DirectiveChimeIn`                                     | `response.create` with chime-in `instructions` (short).|
| `DirectiveBriefAck`                                    | `response.create` with brief-ack `instructions`.       |
| `DirectiveDefer`                                       | **suppress** -- yield the floor; no `response.create`.  |

Under the single-assistant model only `Primary` / `BriefAck` / silence are
expected in practice; chime-in/sequence are multi-agent constructs carried for
parity. The gate is binary at the wire level: **a directive that resolves to
"speak" emits exactly one `response.create`; silence / defer emit nothing.**

This reuses the *exact same* decision the text path already makes. The
`routingOutcome.Respond` boolean and `PrimaryAgentId()` emptiness check are
the gate; we are not adding a second turn-taking brain.

### 2.4 Mapping `Brevity` + `DirectiveMode` into per-response `instructions`

`response.create` accepts a per-response `instructions` string that overrides
the session default for that one response. We render the directive into it.
This is the Realtime analog of what `buildDirectiveMap` +
`integrations/agent/prompt_data.go` do for the text prompt.

Proposed mapping (new helper `RealtimeInstructionsForDirective(d
*AgentParticipationDirective) string` living next to the directive type in
`conductor.go`, so the directive and its renderers stay colocated):

- `Mode`:
  - `DirectivePrimary` -> "Answer the user directly and substantively."
  - `DirectiveChimeIn` -> "Add only your distinct angle. Do not restate what
    was already said." (+ force short)
  - `DirectiveBriefAck` -> "Acknowledge in one short sentence. No agenda."
- `Brevity` (caps spoken length -- this matters more for voice than text):
  - `BrevityShort` -> "Keep it to one short sentence."
  - `BrevityNormal` -> "Keep it to a few sentences."
  - `BrevityDetailed` -> "A longer answer is warranted; stay focused."
- `Instruction` (the LLM conductor's per-turn directive, `conductor.go:144`):
  when non-empty this is THE instruction for the turn and is placed first,
  overriding the generic mode/brevity boilerplate (same precedence the text
  prompt uses).
- `GlobalGuidance` / `Temperature` / `UserIntent`: appended as one-line
  framing so the spoken register matches the room (e.g. "User is frustrated
  -- drop the consultant register").
- The `Skip*` flags (`SkipSelfIntro`, `SkipHandoffOpener`,
  `SkipRoomAnnounce`) map to negative instructions ("Do not introduce
  yourself", "Do not recap who is in the room") -- these matter even more for
  voice, where filler is expensive.

Persona / graph grounding / citations are explicitly out of scope here --
they are issue #436. This spike's `instructions` carry only the
turn-shaping directive; #436 layers persona + retrieved grounding into the
session-level instructions and/or the per-response `instructions` prefix.

### 2.5 Where utterances / presence come from (unchanged inputs)

The conductor's inputs do not change. Deepgram STT keeps running in parallel
for every human track (epic #440: "Deepgram STT keeps running in parallel for
transcripts, conductor scoring, citations"). Per-speaker attribution into the
conductor is issue #433's job; this spike assumes a final transcript with a
`speaker_user_id` lands as a `v1:cognition:utterance` exactly as it does today
(`handleVoiceAgentFinalTranscript`, `voice_agent_handlers.go:558`). That
insert fires the existing cognition automation, which runs the suppression
classifier + conductor consult. The Realtime executor consumes the **result**
of that pipeline; it does not feed the conductor its own audio-derived
end-of-turn signal. This is what lets the conductor, not the model, decide.

---

## 3. Interruption handling

When a human starts speaking mid-assistant-response, the in-flight Realtime
response must be cancelled cleanly via `response.cancel` (followed by
`output_audio_buffer.clear` to flush already-buffered assistant audio so it
stops *immediately*, not just at the next token boundary).

The decision of *whether* a given human interjection should interrupt is,
under option A, a conductor decision -- not raw VAD. Two cases:

1. **The interjecting human is addressing the assistant / the room in a way
   the conductor scores as "the floor changed."** The conductor's next
   consult resolves to silence-for-the-current-response (the assistant should
   yield). Emit `response.cancel`.
2. **The interjecting human is talking to another human (side chatter).** The
   conductor keeps the assistant's `Respond` for the current line of thought;
   do NOT cancel. This is the polyphon-correct behavior default VAD gets
   wrong.

Where the hook lives in code:

- **memQL side.** Today there is exactly one place where a human utterance
  begins a new decision: the cognition automation on
  `graph.node.created.v1:cognition:utterance`, which leads into the
  suppression classifier (`cognition_handler.go:545-590`) and the conductor
  consult. For Realtime, this same entry point must, *before* deciding the
  next turn, check whether an assistant Realtime response is currently
  in-flight for the space and, if the new human utterance scores as a
  floor-change, emit a cancel signal to the executor. The natural home is a
  new method on the per-space session that owns the Realtime connection (see
  the executor in #434), invoked from the same handler that already calls
  `consultConductor`. `ConductorState` (`conductor.go:258`) is the right place
  to track "assistant currently has the floor" -- it already tracks
  `AgentsSpokenThisCycle`, `HumanIsTyping`, and `DispatchHoldUntil`; add a
  `RealtimeResponseInFlight` flag + the active `response_id`, set when we send
  `response.create` and cleared on `response.done`.
- **Wire side.** The cancel is `{ "type": "response.cancel" }` on the Realtime
  data channel, plus `output_audio_buffer.clear`. In the LiveKit-plugin
  framing (option B fast path), `interrupt_response: true` would do this
  locally without the memQL round-trip; we keep option A's explicit cancel as
  the correct-by-default path and treat option B as the latency optimization.

Acceptance criterion "mid-response human interjection cancels the assistant
cleanly" is satisfied by: `response.cancel` + `output_audio_buffer.clear`,
gated on the conductor's floor-change read so unrelated cross-talk does not
spuriously cut the assistant off.

---

## 4. Concrete integration plan

This is the file-by-file plan referencing real symbols. New code is additive;
the cascade path is untouched (epic #440: "No regression to the existing
cascade voice path").

### Step 1 -- Realtime executor behind the `MemqlLLM` seam (depends on #434)

The seam is `voice-agent/voice_agent/memql_llm_plugin.py`. Today `MemqlLLM`
(a real `livekit.agents.llm.LLM` subclass) turns each user-final transcript
into a `VoiceAgentTurnRequest` and streams `VoiceAgentTurnDelta` text back
(the cascade: text -> Deepgram Aura-2 TTS). The Realtime executor is the
alternative implementation selected behind this same seam:

- New plugin module, e.g. `voice_agent/realtime_executor.py`, exposing a class
  that the framework can use in place of `MemqlLLM` (or, more precisely, the
  Realtime model replaces the LLM+TTS pair, since it is speech-to-speech). In
  `main.py:201` the session is built `AgentSession(vad=vad, stt=stt, llm=llm,
  tts=tts)`; the Realtime variant builds the session with the OpenAI Realtime
  model wired as the speech-to-speech executor and `turn_detection=None`
  (option A) instead of the `llm=MemqlLLM(...)` + `tts=build_tts(...)` pair.
  The selection is a config flag (#434's "pluggable executor + cascade
  fallback").
- The Realtime session is created with `turn_detection: null`. The Silero VAD
  load at `main.py:150-151` is retained only for option B / input-commit
  gating; under option A the input buffer is committed from Deepgram finals.

### Step 2 -- A "drive response" control channel from memQL to the executor

Today memQL drives the voice agent through two server-push messages already
defined in `component/grpc/memql.proto` and handled in
`component/grpc/voice_agent_handlers.go`:

- `VoiceAgentTurnDelta` / `VoiceAgentTurnComplete` -- reply to a
  `VoiceAgentTurnRequest` (the STT-initiated path,
  `handleVoiceAgentTurnRequest`, line 645).
- `VoiceAgentSpeak` -- unsolicited "say this" push for chat-typed messages
  (`startVoiceAgentSpeakSubscriber`, line 399; `_on_voice_agent_speak` in
  `main.py:214`).

For Realtime we need a new control message that says "create a response now,
with these instructions" (the conductor's gate decision) and a matching
"cancel the in-flight response." Proposed additions to `memql.proto`:

- `VoiceAgentRealtimeRespond { string request_id; string instructions;
  string mode; string brevity; }` -- server -> voice-agent. The voice-agent
  handler translates this into `response.create` with the carried
  `instructions` on the Realtime data channel.
- `VoiceAgentRealtimeCancel { string request_id; string response_id; }` --
  server -> voice-agent. Translates to `response.cancel` +
  `output_audio_buffer.clear`.

These are the Realtime analogs of `VoiceAgentSpeak`. They hang off the same
`streamSession` push machinery (`sendServerMessage`) used by the existing
voice handlers.

### Step 3 -- Gate the response in cognition (the heart)

In `integrations/cognition/cognition_handler.go`, the path that already calls
`consultConductor` (line 600) and computes `routeOutcome` /
`routingOutcomeFromConductorPlan` is where the gate lives. Today, on a voice
utterance the conductor is skipped for latency
(`isVoiceUtteranceEarly`, line 396; the comment at 590-596 explains the
~1-1.5s sequential conductor call was too slow for the cascade). **This spike
re-introduces the conductor for the Realtime path specifically**, because the
Realtime executor's speed budget is different: the conductor decision overlaps
with the model's own first-token latency rather than serializing in front of a
separate TTS call.

Concretely:

1. Add a branch: when the space's executor is Realtime (config from #434), run
   `consultConductor` even for voice utterances (do not take the
   `isVoiceUtteranceEarly` fast-skip). The cheap suppression classifier
   (`cognition_handler.go:545`, `voice_ack` / `voice_fragment`) still runs
   first as a free pre-filter.
2. Translate the outcome:
   - `routeOutcome.Respond == false` (conductor silence / classifier ack /
     defer) -> emit **nothing** to the executor. The assistant stays quiet.
     This is the acceptance criterion "the model never self-responds."
   - `routeOutcome.Respond == true` -> build the
     `AgentParticipationDirective` for the GA (the existing `BuildDirective` +
     LLM-conductor merge already runs here, line ~1031), render it via the new
     `RealtimeInstructionsForDirective`, and push a
     `VoiceAgentRealtimeRespond` to the executor.
3. Mark `ConductorState.RealtimeResponseInFlight = true` + stash the
   `response_id` when the executor confirms `response.created`, clear on
   `response.done` (new fields on `ConductorState`, `conductor.go:258`).

### Step 4 -- Interruption hook

In the same handler, before computing the next turn for a freshly-landed human
utterance, consult `ConductorState`: if `RealtimeResponseInFlight` and the new
utterance scores as a floor-change (reuse the direct-address /
`AddressedAgentName` signals the classifier already produces,
`cognition_handler.go:551`), push `VoiceAgentRealtimeCancel` for the active
`response_id`. Otherwise leave the assistant talking. This is the
polyphon-correct interruption from section 3.

### Step 5 -- Voice-agent translation layer

In `voice-agent/`, add handlers mirroring `_on_voice_agent_speak`
(`main.py:214`):

- `_on_realtime_respond` -> calls `session.generate_reply(instructions=...)`
  (or the Realtime model's equivalent `response.create`) with the
  conductor-supplied instructions. Registered via
  `client.set_push_handler("voice_agent_realtime_respond", ...)` exactly like
  `set_push_handler("voice_agent_speak", ...)` at `main.py:264`.
- `_on_realtime_cancel` -> calls the Realtime session's interrupt/cancel
  (`response.cancel` + clear).

### Step 6 -- New code/types summary

| Where | What |
|-------|------|
| `integrations/cognition/conductor.go` | `RealtimeInstructionsForDirective(d *AgentParticipationDirective) string`; new `ConductorState` fields `RealtimeResponseInFlight bool` + `RealtimeResponseId string` + setters/getters (mirror the existing mutex-guarded accessor pattern). |
| `integrations/cognition/cognition_handler.go` | Realtime branch: run `consultConductor` on voice utterances when executor==realtime; translate `routeOutcome.Respond` into respond/suppress; interruption check. |
| `component/grpc/memql.proto` + regen | `VoiceAgentRealtimeRespond`, `VoiceAgentRealtimeCancel` server->client messages. |
| `component/grpc/voice_agent_handlers.go` | Push helpers for the two new messages, hung off `streamSession.sendServerMessage`. |
| `voice-agent/voice_agent/realtime_executor.py` (new) | Realtime model plugin selected behind the `MemqlLLM` seam; `turn_detection=None`. |
| `voice-agent/voice_agent/main.py` | Executor selection at session build (line 201); `set_push_handler` for the two new messages. |

Nothing here deletes or alters the cascade path: `MemqlLLM`,
`handleVoiceAgentTurnRequest`, `VoiceAgentSpeak`, the suppression classifier,
and `consultConductor`'s text path all stay exactly as they are.

---

## 5. Latency measurement plan

The metric the issue asks for: **"conductor decides engage" -> first
assistant audio frame.** Live measurement needs real infra (LiveKit room +
OpenAI Realtime credentials + Deepgram), which this spike does not stand up.
What follows is the methodology + the exact instrumentation points so the
measurement can be run as a flagged follow-up once a credentialed room exists.

### 5.1 Timeline + instrumentation points

There is already a structured voice-trace logging convention in the codebase
to extend -- see `voice_streaming.go:127-135` ("voice trace: first sentence
dispatched", keyed `voiceTrace: "voice:"+spaceId`, `stage`,
`firstSentenceMs`). Reuse that exact shape so the new spans join the existing
trace.

Stamps to emit (monotonic clock, all carrying `voiceTrace: voice:<spaceId>`
and `request_id`):

| Stamp | Where | `stage` |
|-------|-------|---------|
| T0 user-final lands | `handleVoiceAgentFinalTranscript` (`voice_agent_handlers.go:558`) | `voice.final` |
| T1 conductor consult start | before `consultConductor` (`cognition_handler.go:600`) | `conductor.consult.start` |
| **T2 conductor decides engage** | right after `routingOutcomeFromConductorPlan` returns `Respond==true` (`cognition_handler.go:605`) | `conductor.decide.engage` |
| T3 `VoiceAgentRealtimeRespond` pushed | new push helper (`voice_agent_handlers.go`) | `realtime.respond.push` |
| T4 `response.create` sent on the data channel | `_on_realtime_respond` (`main.py`) | `realtime.response.create` |
| **T5 first assistant audio frame** | first audio delta from the Realtime model in the executor plugin | `realtime.audio.first` |

The headline number is **T5 - T2** (decision -> first audio). Supporting
splits: T2-T1 (conductor LLM cost -- already ~1-1.5s per the
`cognition_handler.go:590` comment, the dominant term to watch), T4-T3
(memQL->executor push), T5-T4 (model TTFB). For the silence/suppress path the
assertion is simpler: **no `realtime.response.create` is ever emitted** when
`Respond==false` -- that is the gate-correctness check, measured by absence,
not latency.

### 5.2 Test scenarios (matching the acceptance checklist)

1. **Single human asks the assistant** -> exactly one `response.create`; record
   T5-T2.
2. **Two humans cross-talk, neither addresses the assistant** -> zero
   `response.create` over the window. Gate correctness.
3. **Two-three humans, then one addresses the assistant** -> exactly one
   `response.create`, fired only on the addressed utterance.
4. **Mid-response interjection that scores as floor-change** -> one
   `response.cancel` + `output_audio_buffer.clear`; assert assistant audio
   stops within one frame budget of the cancel.
5. **Mid-response side chatter (not a floor-change)** -> no cancel; assistant
   finishes.

### 5.3 Harness

A small Python driver in `voice-agent/` (e.g. `scripts/realtime_latency.py`)
joins a test room, plays canned WAV utterances for N synthetic humans on
separate tracks, and scrapes the structured `voiceTrace` log lines (or
subscribes to a metrics sink) to compute the splits. Because the metric is the
delta between two timestamped log lines that already share `request_id` +
`voiceTrace`, no new transport is needed for measurement -- only the six
stamps above. **[WARNING] This requires live OpenAI Realtime + LiveKit +
Deepgram credentials and is the flagged follow-up; not run as part of this
spike.**

---

## 6. Open questions / risks

These genuinely cannot be settled without running it on live infra:

1. **Conductor latency in front of voice.** The code comment at
   `cognition_handler.go:590-596` records that the conductor's ~1-1.5s
   sequential LLM call "dominated every turn end-to-end" and is *why* voice
   currently skips the conductor. The whole spike bets that with a Realtime
   executor this cost overlaps the model's own latency rather than serializing
   in front of a separate TTS call -- but whether the *felt* decision->audio
   latency is acceptable is exactly what 5.1's T5-T2 must measure. If it is
   not, mitigations: a cheaper/faster conductor model for the gate-only
   decision, a two-tier gate (cheap classifier decides speak/silence
   immediately, full conductor refines instructions in parallel), or option B
   for interruption.
2. **Option A vs option B interruption latency.** Whether conductor-driven
   `response.cancel` (a memQL round-trip) is fast enough, or whether we must
   fall back to option B's local `interrupt_response: true` and accept that
   side chatter sometimes cuts the assistant off. Needs live measurement.
3. **Input-commit cadence under `turn_detection: null`.** With model VAD off,
   memQL owns `input_audio_buffer.commit`. The commit cadence must be driven
   from Deepgram finals -- and the EOU tuning that governs those finals
   (`docs/voice/eou-tuning.md`: `endpointing_ms`, and the note that the LK
   Deepgram 1.5 plugin does NOT expose `utterance_end_ms`) directly affects
   how snappy the gate feels. Open question whether the existing EOU knobs are
   the right commit trigger or whether the Realtime path wants its own.
4. **Multi-party audio into a 1:1-shaped model (#433 overlap).** The Realtime
   model expects "one user". How 5 human tracks + per-speaker attribution are
   mixed/fed is issue #433; this spike assumes #433 delivers an attributed
   utterance stream. If #433's mixing changes when the input buffer is
   committed, the gate timing shifts. Cross-spike dependency.
5. **`response_id` lifecycle / races.** Tracking `RealtimeResponseInFlight` +
   the active `response_id` on `ConductorState` assumes clean
   `response.created` / `response.done` bracketing. Rapid
   create-then-cancel-then-create sequences (fast interruptions) could race;
   needs the same one-active-turn discipline `handleVoiceAgentTurnRequest`
   already enforces via `turnKey` + sentinel (`voice_agent_handlers.go:717`).
6. **Async tool calls mid-response (#435 overlap).** The GA model GA fix is
   async function calling; if a tool runs while a response is in-flight and
   the conductor then wants to cancel, the cancel semantics vs. an in-flight
   tool call need defining. Out of scope here, flagged for #435.

---

## 7. Acceptance criteria mapping

Issue #432 checklist, each mapped to where this design satisfies it:

- [x] **"The model never self-responds; every response is
  conductor-triggered."** -> `turn_detection: null` (section 2.2, option A);
  the single `response.create` emitter is gated on `routeOutcome.Respond` /
  `PrimaryAgentId()` (sections 2.1, 2.3, step 3). Verified in 5.2 scenario 2
  (zero `response.create` on pure cross-talk).
- [x] **"5 humans talking, one asks the assistant -> assistant engages only
  when the conductor detects it's addressed/relevant."** -> the conductor
  consult + direct-address signals decide `Respond` (sections 2.1, 2.5);
  5.2 scenario 3.
- [x] **"Directive (mode + brevity) shapes the response."** ->
  `RealtimeInstructionsForDirective` renders `DirectiveMode` + `Brevity` +
  `Instruction` into per-response `instructions` (section 2.4, step 6).
- [x] **"Mid-response human interjection cancels the assistant cleanly."** ->
  `response.cancel` + `output_audio_buffer.clear`, gated on a conductor
  floor-change read so side chatter does not spuriously cut off (section 3,
  step 4); 5.2 scenarios 4-5.
- [~] **"Decision->first-audio latency measured + documented."** ->
  methodology + the six instrumentation stamps are specified (section 5);
  **the live measurement itself is a flagged follow-up requiring real
  credentials/room** (section 5.3, open question 1). This is the one item the
  design specifies but cannot execute within the spike.

Legend: [x] satisfied by design; [~] design complete, live execution is a
flagged follow-up.
