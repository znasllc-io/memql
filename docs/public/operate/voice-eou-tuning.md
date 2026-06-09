---
title: Voice end-of-utterance (EOU) tuning
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Voice end-of-utterance (EOU) tuning

Status: baseline shipped; per-user adaptive endpointing is a design seed,
not built. Read this before re-tuning the Deepgram knobs or starting on
the adaptive layer.

## What "end of utterance" means in the voice path

The Go voice-agent (`integrations/voice/agent/`) treats a chunk of
user speech as "done" the moment Deepgram emits a `final=true`
transcript event for it. That event then becomes a
`VoiceAgentFinalTranscript` to memql, which inserts the user's chat
row and fires `VoiceAgentTurnRequest` to dispatch the agent. From the
user's perspective: the moment Deepgram says "final", the agent will
reply -- so any over-eager "final" cuts the user off mid-thought.

Two Deepgram knobs control the firing:

- **`endpointing_ms`** -- phrase-commit cadence inside an utterance.
  Deepgram waits this much silence before stabilising the current
  in-flight transcript chunk and marking it `is_final=true`. Smaller
  = snappier final transcripts; larger = more tolerance for thinking
  pauses inside a sentence.
- **`utterance_end_ms`** -- hard end-of-utterance silence floor on
  the Deepgram wire protocol. The Go cascade speaks the Deepgram
  streaming WebSocket directly (no LiveKit STT plugin in the middle),
  so it forwards `POLYPHON_DEEPGRAM_UTTERANCE_END_MS` as the
  `utterance_end_ms` query param when non-zero -- unlike the retired
  Python path, where the LK Deepgram plugin did not expose this knob.

An `UtteranceEnd` event from Deepgram causes the in-flight phrase to
be committed as a `final` transcript. So both knobs feed the same
downstream "transcript is final, dispatch the agent" signal.

There is also a frame-level VAD (Silero) gating audio frames into
Deepgram, but Silero only decides "this frame is speech vs noise"
-- it does NOT decide turn boundaries. The EOU decision is
Deepgram's alone.

## Baseline defaults

Defined in `integrations/voice/agent/config.go` (`LoadConfig`):

| Env var                                 | Default | Effect                          |
| --------------------------------------- | ------- | ------------------------------- |
| `POLYPHON_DEEPGRAM_ENDPOINTING_MS`      | `2000`  | Phrase-stable threshold         |
| `POLYPHON_DEEPGRAM_UTTERANCE_END_MS`    | `0`     | Forwarded to Deepgram as `utterance_end_ms` when non-zero (see above) |

These err on "let the user think." A 500ms phrase commit (the old
default, ported forward from the retired Bridge Agent) fires on any
natural conversational pause; users complained their sentences got
cut mid-thought and the agent would either respond to a fragment
or apologise that "your message got cut off." 2000ms gives ~3x more
breathing room and is still well under the perceived-rude threshold
for a back-and-forth.

A snappier user (rapid-fire questions, prefers tight back-and-forth)
can drop the endpointing knob via env. Typical re-tunes:

- Snappy: `endpointing=900`
- Deliberate (default): `endpointing=2000`
- Very deliberate / thinks out loud: `endpointing=3000`

The classifier runs on voice now (cognition_handler.go's `runClassifier`
is no longer gated on `!isVoiceUtteranceEarly`), so `intent=follow_up`
fragments like "um, let me think..." get suppressed BEFORE they cost an
agent reply. The endpointing knob then becomes a less critical safety
net rather than the sole gate on "is this thought done."

`POLYPHON_DEEPGRAM_UTTERANCE_END_MS` is forwarded to Deepgram as a
hard end-of-utterance silence floor when non-zero; set it alongside
endpointing when you have a target floor in mind, otherwise leave it
at 0.

## The adaptive idea (not built)

Each speaker has a distinct cadence -- some pause 200ms between
phrases, some pause 2000ms. A static global default has to pick a
compromise that's wrong for both ends. Long-term, voice-agent should
learn each user's median inter-word and inter-phrase gap from their
own STT history and tune `endpointing_ms` per session.

Sketch of the loop:

1. **Telemetry.** On every `final=true` Deepgram event, log
   `(userId, utteranceText, interimTimestamps[], finalTimestamp,
   audioStartTs, audioEndTs)` to memql as a
   `v1:cognition:speakingprofile:sample` row. The interim timestamps
   give us inter-word gaps; the final-vs-audio-end gap tells us
   whether the user actually was done or paused longer than the
   knob allowed.

2. **Per-user stats.** A nightly automation aggregates the last N
   samples per user into a `v1:cognition:speakingprofile` row with
   `(p50InterWordGapMs, p90InterPhraseGapMs, falseFinalsRate,
   pauseDeliberationScore)`. Falsy finals are the cases where the
   user kept speaking immediately after a `final=true`; those are
   evidence the knob was set too aggressive.

3. **Per-session priming.** When voice-agent starts a session for a
   user, query their `speakingprofile` and set
   `endpointing_ms = max(750, round(p90InterPhraseGapMs * 1.2))`
   and `utterance_end_ms = max(1200, round(p90InterPhraseGapMs * 1.6))`.
   The 1.2/1.6 multipliers give safety headroom above the observed
   gap distribution. Falls back to the baseline defaults if the
   user has fewer than ~20 samples.

4. **Continuous nudging.** Inside a long session, if the
   `falseFinalsRate` over the trailing 10 turns climbs above a
   threshold, the active session's knobs nudge up by 200ms. If the
   user is consistently re-asking the agent to wait (the agent's
   "I think I cut you off, please continue" replies), that's also
   a strong signal to nudge up.

5. **Don't auto-shrink aggressively.** The cost of being too eager
   (cutting the user off) is much higher than being too patient.
   Knobs ratchet up easily, down slowly.

Telemetry storage uses the same partition as the rest of the user's
data so the per-user history lives with them and never crosses
tenants. The aggregation lives in a partition-scoped automation
(daily) -- not in cognition's hot path.

## Why not just use VAD-based turn detection?

A VAD-driven turn-detection mode uses the VAD signal to call the turn
instead of relying on Deepgram's endpointing. We tried this in a prior
session (on the retired LiveKit Agents path); the VAD fires on ambient
room noise (background chatter, fans, music) and produced even worse
cut-offs. Deepgram endpointing is signal-aware (it knows when the user
is mid-phrase vs done) and is the right authority. Adaptive tuning of
THIS signal is the lever.

## Pre-flight before re-tuning

The Go cascade's Deepgram streaming URL is built in
`integrations/deepgram/deepgram.go` (consumed via
`integrations/voice/agent/stt_pipeline.go`). Before changing the
defaults, confirm there that `endpointing` and `utterance_end_ms` are
still mapped to the Deepgram WebSocket query params. Deepgram has
tweaked these between Nova-2 and Nova-3; future model upgrades may
force a re-tune.
