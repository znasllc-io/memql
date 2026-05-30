# Multi-party audio routing + per-speaker attribution into the Realtime session (#433)

Status: design spike. No runtime code lands with this doc. Full
validation requires live infrastructure (>=3-human LiveKit room +
OpenAI gpt-realtime + Deepgram with real credentials) and is a flagged
follow-up -- see the Validation plan.

Docs location note: this epic's docs already live under `docs/voice/`
(`eou-tuning.md`, `bringup-verification.md`), so this spike doc is
placed alongside them rather than in a new `docs/realtime-voice/`
directory. The issue references "realtime-voice" as the label/epic
name; the on-disk convention is `docs/voice/`.

Part of the Hybrid Realtime Voice epic (#440). Sibling spike #432 owns
the conductor-driven response gate (disable model VAD, drive
`response.create`/`response.cancel`). This spike owns the **audio-input
side**: how a multi-human room becomes a coherent single input stream
for a model that consumes one input audio stream, without losing "who
said what."

---

## 1. Problem restatement

The OpenAI Realtime API (gpt-realtime) is shaped for a 1:1
conversation: it consumes **one** input audio stream and emits one
output audio stream. A polyphon space (`architecture: "polyphon"`)
allows **up to 5 human participants** plus one assistant. LiveKit
delivers each human as a separate participant with a separate audio
track (see `entrypoint` in
`voice-agent/voice_agent/main.py`, the `track_subscribed` /
`track_published` listeners).

Two naive options, both wrong:

- **(a) Mix all human tracks into one stream.** The model hears a
  coherent room but cannot attribute statements to people. It can
  never correctly say "as Maria mentioned..." because the audio it
  heard had no speaker labels. Worse, simultaneous talkers smear into
  unintelligible overlap.
- **(b) Feed only one track.** The model is deaf to everyone except
  one chosen human; cross-talk and barge-in from other humans are
  invisible.

We already run **per-participant Deepgram STT** and need it anyway for
transcripts, conductor scoring, citations, and chat/canvas parity
(epic #440, issues #437/#436). The design below exploits that: text
carries identity, audio carries prosody + barge-in.

The current code is explicitly single-human-per-room. See
`_default_speaker_provider` in `main.py`:

```python
# Single-human-per-room assumption: returns the identity of the
# first STANDARD (human) participant in the room. ...
# Multi-human rooms still need real per-track attribution; that's a
# Phase 6 follow-up.
```

and `_sync_speaker_user_id` in `voice-agent/voice_agent/stt_plugin.py`:

```python
# Wider design: the LiveKit room can have multiple human participants;
# the speaker for a given transcript event is the participant whose
# audio track produced it. Phase 6 wires this via the participant
# identity carried by the underlying STT plugin event.
```

This spike specifies that "Phase 6" per-track attribution for the
Realtime executor path.

---

## 2. Routing design (recommended)

**Hybrid: per-speaker text for identity, active-speaker audio for
prosody + barge-in.**

Two parallel channels feed the Realtime session:

### 2a. What the model READS (labeled text, all speakers)

Keep one **per-participant Deepgram STT stream** per human track
(today's `build_stt` in `stt_plugin.py`, fanned out one-per-track).
When a participant's transcript finalizes, inject it into the Realtime
session as a labeled `conversation.item` of type
`message`/`role: "user"` whose content is prefixed with the speaker's
identity + role, e.g.:

```jsonc
// conversation.item.create
{
  "type": "conversation.item.create",
  "item": {
    "type": "message",
    "role": "user",
    "content": [
      { "type": "input_text",
        "text": "[Maria Lopez · Finance Lead] We should cut the cloud spend first." }
    ]
  }
}
```

Every non-active human's finalized speech reaches the model **as text
the model can read and attribute**, regardless of whether the model
heard the audio. This is what makes "as Maria mentioned..." possible:
the model has a literal, labeled transcript line attributed to Maria
in its conversation context.

This path reuses the existing finalized-transcript producer. Today the
final goes to memql as `VoiceAgentFinalTranscript` (see
`TranscriptForwarder.forward_final` in
`voice-agent/voice_agent/transcript_forwarder.py`). In the Realtime
executor, the same final additionally becomes a
`conversation.item.create`. memql ingestion is unchanged -- the
transcript still lands as `v1:cognition:utterance` via
`mutationSendTextUtterance` (see `handleVoiceAgentFinalTranscript` in
`component/grpc/voice_agent_handlers.go`), so conductor scoring,
citations, and chat parity are preserved.

### 2b. What the model HEARS (live audio, active speaker only)

Stream **one** audio track to the Realtime session's input audio
buffer (`input_audio_buffer.append`): the **active speaker** as
reported by LiveKit, with a short hold so the stream doesn't flap
between speakers mid-phrase. This gives the model:

- **Prosody** for the human currently holding the floor (tone,
  emphasis, hesitation) -- the part text cannot carry.
- **Barge-in signal**: when a human starts talking over the
  assistant, that human becomes active speaker and their audio lands
  in the input buffer immediately (see Barge-in below).

Crucially, with the conductor gate (#432) the model's auto-VAD is
**off** (`turn_detection: null`, or input-VAD for interruption only).
So appending active-speaker audio to the input buffer does **not**
auto-trigger a response -- it only (a) gives the model live prosody
for when the conductor *does* fire `response.create`, and (b) feeds
the interruption detector. The conductor decides when to speak; this
spike just makes sure the model has heard the right human when it does.

### 2c. The audio-vs-text decision, made explicit

| Speaker state | Model HEARS (audio in buffer) | Model READS (conversation.item) |
|---|---|---|
| Active speaker (holds floor) | Yes -- live track appended | Yes -- on finalize, labeled text |
| Non-active human (recently spoke) | No | Yes -- labeled text on finalize |
| Silent human | No | No |
| Barge-in human (interjects mid-response) | Yes -- becomes active, audio appended | Yes -- on finalize |

Rationale: live audio is expensive (tokens + the model only has one
ear) and only the floor-holder's prosody matters at any instant.
Everyone else's *content* still reaches the model losslessly as
labeled text. This is the (b) branch from the issue ("per-participant
STT transcripts injected as labeled context + a mixed/active-speaker
audio stream for prosody + interruption"), with active-speaker chosen
over a full mix (see Open questions for the mix-vs-active tradeoff).

### 2d. LiveKit active-speaker detection

LiveKit publishes active-speaker information natively; no custom DSP:

- **`active_speakers_changed`** room event (Python:
  `ctx.room.on("active_speakers_changed", ...)`) yields the ordered
  list of currently-speaking participants. Loudest/primary is index 0.
- Each `rtc.Participant` exposes `is_speaking` and `audio_level`.

The router subscribes to `active_speakers_changed`, picks the top
**STANDARD-kind** (human) speaker (the same `kind != 0` filter already
used in `_default_speaker_provider`, so the Anam avatar / ingress /
egress participants never become the "active human"), and switches
which participant's decoded audio frames it forwards into the Realtime
input buffer. A small dwell timer (~300-500 ms, candidate knob)
debounces rapid speaker flips so a single utterance is not chopped
across two appends.

If LiveKit reports **no** active human (silence), the router appends
nothing -- the input buffer simply idles. The model keeps the
conversation context (text items) but hears nothing, which is correct.

---

## 3. Speaker attribution

Goal: the assistant can say "as Maria mentioned, the cloud spend is
the place to start."

The data path that carries identity/role:

1. **LiveKit participant identity.** Each human track carries the
   participant `identity` (a memql `v1:cognition:participant` id /
   `v1:identity:user` id). This is already the value
   `_default_speaker_provider` resolves and `TranscriptForwarder`
   stamps as `speaker_user_id`.

2. **Per-track STT tagging.** With one Deepgram stream per track, the
   finalized transcript already knows which participant produced it
   (the producer owns the track -> identity binding). No active-speaker
   guess needed for attribution -- attribution is per-track and exact.
   This removes the single-human shortcut in
   `_sync_speaker_user_id`.

3. **Identity + role -> label.** The label injected into the
   `conversation.item` text prefix is `displayName · role`. Both are
   already resolvable in memql:
   - Display name: `getParticipantDisplayName(ctx, participantId)` in
     `integrations/cognition/cognition_handler.go` and
     `lookupParticipantDisplayName(...)` in
     `integrations/cognition/conductor_consult.go`. The same value
     flows into agent prompts today as `CurrentUserDisplayName`
     (`contextWithCurrentUserDisplayName` /
     `currentUserDisplayNameFromContext` in
     `integrations/cognition/si_responder.go`, threaded in
     `agent_forward.go`).
   - Role: the human's space role, or for the polyphon group the
     participant's labeled role; the assistant's own role label is the
     `personaProfile.role` already surfaced in the SPA.

4. **Where the label is assembled.** Two viable seams; recommend the
   **voice-agent side** to keep the Realtime wire self-contained:
   - The voice-agent resolves `displayName`/`role` once per
     participant at join (one lookup, cached on a
     `ParticipantRoster` keyed by identity) and builds the
     `[name · role]` prefix locally before
     `conversation.item.create`.
   - Alternative: memql stamps a `speaker_display_name` /
     `speaker_role` onto the transcript ack
     (`VoiceAgentFinalAck`) and the voice-agent echoes it. This keeps
     name resolution server-side (single source of truth) at the cost
     of a wider proto. Chosen approach: voice-agent-side roster with a
     server lookup at join; revisit if names drift.

Because attribution is **per-track text**, it is independent of the
active-speaker audio routing. Even if the active-speaker heuristic
picks the wrong human's *audio* for a moment, the *text* (and thus the
attribution the model reads) is always correct.

---

## 4. Barge-in (audio-input side)

Scope boundary: #432 owns the **trigger** (`response.cancel` when a
human interrupts). This spike owns making sure the interrupting
human's audio is **present and routed** so the interruption is
detectable and the model hears the interrupter.

Flow in this routing model:

1. Assistant is mid-response (Realtime is emitting output audio; the
   conductor fired `response.create`).
2. A human starts talking. LiveKit fires
   `active_speakers_changed` with that human now top of the speaking
   list, and their `is_speaking` flips true.
3. The router immediately makes that human the active speaker and
   begins appending their audio frames to `input_audio_buffer`.
4. Detection of the interruption happens via one of:
   - **Realtime input-VAD-for-interruption** -- the session can keep
     server VAD enabled *only* for interruption (no auto-response),
     which raises `input_audio_buffer.speech_started` while the
     assistant is responding. This is the cleanest signal and lives
     entirely on the audio-input side this spike owns.
   - **LiveKit active-speaker edge** -- the
     `active_speakers_changed` -> human-now-speaking transition,
     surfaced to the conductor as an interruption hint.
5. Either signal is handed to the #432 path, which issues
   `response.cancel` (and `output_audio_buffer.clear` to stop
   in-flight TTS playout). The avatar lip-sync (#438) stops with it.

This spike's deliverable for barge-in: (a) the active-speaker router
guarantees the interrupter's track is the one in the input buffer the
instant they speak; (b) the `speech_started` / active-speaker edge is
exposed as a typed interruption event for #432 to consume. We do NOT
issue `response.cancel` here.

Edge case: assistant audio bleed. Because LiveKit treats the
assistant's own published track as a participant, the active-speaker
filter must exclude the assistant's identity (the existing
`kind != 0` / identity filter handles avatar + assistant
participants), otherwise the assistant's own output could register as
"active speaker" and create a feedback loop.

---

## 5. Concrete integration plan (file-by-file)

This plan attaches to the existing voice-agent wiring and the
`MemqlLLM` seam. The Realtime executor lands behind that seam in #434;
this spike specifies the **routing module** it depends on.

### New module: `voice_agent/audio_router.py`

A `MultiPartyAudioRouter` owning:

- `attach(room)` -- subscribes to `track_subscribed`,
  `track_unsubscribed`, `active_speakers_changed`,
  `participant_disconnected`.
- Per human (STANDARD-kind) track: spins up one Deepgram STT stream
  (reusing `build_stt(cfg)` from `stt_plugin.py`) bound to that track,
  tagged with the participant `identity`. Replaces the
  single-`session.input.audio` assumption with explicit per-track
  subscription.
- `active_identity()` -- the current active human identity (debounced),
  used to select which track's decoded frames forward into the
  Realtime input buffer.
- An `on_final(identity, text)` callback and an
  `on_speech_started(identity)` callback that the executor subscribes
  to.

### `voice_agent/main.py` (`entrypoint`)

- Today: `session = AgentSession(vad=vad, stt=stt, llm=llm, tts=tts)`
  with one STT and one implicit audio input. The cascade path is
  unchanged when the Realtime executor is off (fallback per #440).
- Add: when the Realtime executor is selected, construct
  `MultiPartyAudioRouter(room=ctx.room, cfg=cfg, roster=roster)` and
  pass its active-speaker frame source to the Realtime session input.
- Replace `_default_speaker_provider(ctx)` (first-human shortcut) with
  the router's per-track identity for transcript forwarding so
  multi-human attribution is exact. `attach_transcript_forwarding`
  keeps its signature; the provider becomes per-event rather than
  per-room.
- Add a `ParticipantRoster` resolved at join (display name + role
  lookup) for the `[name · role]` labels.

### `voice_agent/stt_plugin.py`

- `attach_transcript_forwarding` already routes finals through
  `TranscriptForwarder`. Extend so each final ALSO drives the
  executor's `conversation.item.create` with the labeled prefix
  (when the Realtime executor is active). The current
  `_sync_speaker_user_id` best-effort guess is removed in favor of the
  per-track identity carried on the event.

### `voice_agent/memql_llm_plugin.py` (`MemqlLLM` seam)

- The Realtime executor slots in here per #434. `MemqlLLM.chat()`
  currently reads the latest user turn from `ChatContext`
  (`_latest_user_turn`, which already pulls `identity` /
  `speaker_id` off `item.extra`). The Realtime executor variant
  consumes the router instead of a single text turn: labeled text
  items in, active-speaker audio in, model audio out. The existing
  `item.extra["identity"]` plumbing is the attribution carrier on the
  text side and is kept.

### Proto (`component/grpc/memql.proto`) -- optional

- No change strictly required for routing (attribution is
  voice-agent-side). If we choose the server-stamped-name variant
  (Section 3.4 alternative), add `speaker_display_name` /
  `speaker_role` to `VoiceAgentFinalAck`. Recommend deferring until a
  name-drift problem is observed.

### memql server (`voice_agent_handlers.go`) -- no change

- `handleVoiceAgentFinalTranscript` and `handleVoiceAgentTurnRequest`
  stay as-is. The transcript still lands as an utterance; the
  conductor still drives dispatch. The Realtime audio routing is
  invisible to memql -- it sees the same `VoiceAgent*` surface.

### Conductor (`integrations/cognition/conductor.go`) -- no change

- The conductor already consumes utterances + presence + per-speaker
  attribution (`getParticipantDisplayName`, the
  `RecordHumanSpoke`/`RecordAgentSpoke` state model). This spike feeds
  it the same data it gets today; the only difference is that
  per-track attribution is now exact in multi-human rooms instead of
  best-effort.

---

## 6. Validation plan

Full validation needs live infrastructure and is a **flagged
follow-up** (cannot run in CI): a LiveKit room with **>=3 real human
participants**, OpenAI gpt-realtime, and Deepgram, all with real
credentials. Methodology + instrumentation:

### Attribution validation

- **Scripted 3-human script.** Three humans read a fixed script where
  each makes a distinct, attributable claim (Maria: cut cloud spend;
  Dev: migrate the database; Priya: hire a contractor). Then a fourth
  prompt asks the assistant to summarize "who proposed what."
- **Pass criterion:** the assistant attributes each claim to the
  correct human by name in >= N/N trials. Record misattributions.
- **Instrumentation:** log every `conversation.item.create` with its
  `[name · role]` prefix and the source participant identity; diff the
  model's named attributions against the ground-truth script.

### Active-speaker routing validation

- **Overlap test.** Two humans talk simultaneously; assert the router
  forwards exactly one track (the LiveKit-top active speaker) and that
  the other human's content still appears as a labeled text item.
- **Instrumentation:** log every active-speaker switch (timestamp,
  from-identity, to-identity, dwell-timer state) and every
  `input_audio_buffer.append` with the source identity. A switch log
  that flaps faster than the dwell timer indicates the debounce knob
  needs tuning.

### Barge-in validation

- **Interject test.** Assistant is mid-response; a human interjects.
  Assert: (a) the interrupter becomes active speaker within one
  `active_speakers_changed` event, (b) `speech_started` (or the
  active-speaker edge) fires, (c) the #432 path cancels the response
  and output audio stops within a target latency.
- **Instrumentation:** timestamp the chain
  `human-speech-onset -> active_speakers_changed ->
  speech_started -> response.cancel -> output_audio_buffer.clear`;
  measure end-to-end interruption latency.

### Latency

- Measure `human-finalize -> conversation.item.create -> (conductor
  fires) -> first assistant audio`. Compare against the cascade
  baseline (Deepgram STT -> cognition -> Deepgram TTS) per #440.

Gate: ship behind the same Realtime-executor feature flag as #434;
default off; cascade remains the fallback.

---

## 7. Open questions / risks

These genuinely cannot be resolved without running it live:

1. **Mixed vs active-speaker audio, under real prosody.** Active
   speaker gives clean single-speaker prosody but the model is "deaf"
   to a second simultaneous talker's *tone* (their content still
   arrives as text). A true mix preserves overlap realism but
   smears prosody and may confuse the model about who is speaking. We
   cannot know which the model handles better for natural turn-taking
   until measured against gpt-realtime with real voices. Recommend
   shipping active-speaker first (cheaper, cleaner attribution) and
   A/B-ing a mix later.

2. **Dwell-timer value.** The active-speaker debounce (~300-500 ms) is
   a guess; too short flaps mid-utterance, too long clips fast
   back-and-forth. Needs live tuning.

3. **Text/audio temporal coherence.** A non-active human's finalized
   text item lands ~`endpointing_ms` after they actually spoke (see
   `eou-tuning.md`). The model reads it slightly out of real-time
   order relative to the active-speaker audio it heard. Whether this
   causes the model to mis-order the conversation is unknown until
   tested.

4. **Per-track Deepgram cost/quota at 5 humans.** Five concurrent
   Deepgram streams per room multiplies STT cost and connection count.
   May need a quota guard (ties into #439 cost guardrails).

5. **Interruption signal source.** Whether to rely on Realtime
   input-VAD-for-interruption (`speech_started`) or the LiveKit
   active-speaker edge as the canonical barge-in trigger is a
   handoff decision with #432; both are exposed by this spike, the
   choice is empirical.

6. **Assistant-audio feedback.** Confirming the assistant's own
   published track never registers as an active human across all
   participant `kind`s (avatar, ingress) needs a live room to verify
   the filter holds.

---

## 8. Acceptance-criteria mapping (issue #433)

| Acceptance criterion | Where addressed |
|---|---|
| Realtime session receives a coherent multi-speaker view (labeled transcripts + audio) from a >=3-human room | Sections 2a (labeled text, all speakers), 2b (active-speaker audio), 2c (the table). Integration in Section 5. |
| Assistant correctly attributes statements to the right speaker | Section 3 (per-track identity -> `[name · role]` label -> `conversation.item`), validated in Section 6 "Attribution validation". |
| Barge-in works (human audio interrupts the assistant) | Section 4 (active-speaker router guarantees interrupter audio is in the input buffer; `speech_started`/active-speaker edge exposed to #432), validated in Section 6 "Barge-in validation". |
| Documented routing design (what's audio vs text, active-speaker handling) | This document; Section 2c table is the audio-vs-text summary; Section 2d is active-speaker handling. |

Live-infrastructure validation (>=3-human room + gpt-realtime +
Deepgram with real credentials) is a flagged follow-up; this spike is
the design + integration plan + instrumentation methodology.
