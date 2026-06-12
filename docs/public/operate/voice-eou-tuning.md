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
not built. Read this before re-tuning the VAD knobs or starting on the
adaptive layer. For the full GA Realtime API surface these knobs live in
(noise reduction, transcription gating, barge-in, session lifecycle), see
[voice-realtime-ga.md](voice-realtime-ga.md).

## What "end of utterance" means in the voice path

The Go voice-agent (`integrations/voice/agent/`) treats a chunk of
user speech as "done" the moment the ASR emits a `final=true`
transcript event for it. That event then becomes a
`VoiceAgentFinalTranscript` to memql, which inserts the user's chat
row and fires `VoiceAgentTurnRequest` to dispatch the agent. From the
user's perspective: the moment the ASR says "final", the agent will
reply -- so any over-eager "final" cuts the user off mid-thought.

Both voice paths run on OpenAI server-side VAD, with one knob each:

- **Cascade / streaming chat mic** (`integrations/openai/asr.go`,
  Realtime API transcription-only mode) -- the server VAD waits
  `silence_duration_ms` of trailing silence before declaring
  end-of-utterance and committing the transcript. Tuned via
  `POLYPHON_OPENAI_VAD_SILENCE_MS` (default `600`).
- **Realtime executor** (gpt-realtime speech-to-speech, native 1-on-1
  path) -- the session's `server_vad` turn detection uses
  `MEMQL_REALTIME_VAD_SILENCE_DURATION_MS` (default `500`), plus the
  energy gate `MEMQL_REALTIME_VAD_THRESHOLD` (default `0.6`, raised
  from OpenAI's 0.5 baseline so ambient noise does not commit a
  phantom turn) and `MEMQL_REALTIME_VAD_PREFIX_PADDING_MS` (default
  `300`).

## Baseline defaults

| Env var                                  | Default | Effect                                          |
| ---------------------------------------- | ------- | ----------------------------------------------- |
| `POLYPHON_OPENAI_VAD_SILENCE_MS`         | `600`   | Cascade/chat-mic trailing-silence window         |
| `MEMQL_REALTIME_VAD_SILENCE_DURATION_MS` | `500`   | Realtime executor trailing-silence window        |
| `MEMQL_REALTIME_VAD_THRESHOLD`           | `0.6`   | Realtime speech-energy gate (0..1)               |
| `MEMQL_REALTIME_VAD_PREFIX_PADDING_MS`   | `300`   | Audio kept before the detected onset             |

The 600ms cascade default is the measured sweet spot: 500ms splits
natural mid-sentence pauses ("just wanna see how... [breath] how it
works") into two utterances; 800ms adds perceptible end-of-turn lag.

A snappier user (rapid-fire questions, prefers tight back-and-forth)
can drop the silence window via env. Typical re-tunes:

- Snappy: `450-500`
- Default: `600`
- Very deliberate / thinks out loud: `800-1000`

The classifier runs on voice now (cognition_handler.go's `runClassifier`
is no longer gated on `!isVoiceUtteranceEarly`), so `intent=follow_up`
fragments like "um, let me think..." get suppressed BEFORE they cost an
agent reply. The silence knob then becomes a less critical safety net
rather than the sole gate on "is this thought done."

For noisy rooms, raise `MEMQL_REALTIME_VAD_THRESHOLD` toward `0.9` so
low-energy ambient noise stops tripping turns at all (see #1203).

## The adaptive idea (not built)

Each speaker has a distinct cadence -- some pause 200ms between
phrases, some pause 2000ms. A static global default has to pick a
compromise that's wrong for both ends. Long-term, voice-agent should
learn each user's median inter-word and inter-phrase gap from their
own STT history and tune the silence window per session.

Sketch of the loop:

1. **Telemetry.** On every `final=true` ASR event, log
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
   `silence_duration_ms = max(450, round(p90InterPhraseGapMs * 1.2))`.
   The 1.2 multiplier gives safety headroom above the observed gap
   distribution. Falls back to the baseline defaults if the user has
   fewer than ~20 samples.

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

## Pre-flight before re-tuning

The cascade's session config is built in
`integrations/openai/asr.go` (`sendSessionConfig`, consumed via
`integrations/voice/agent/stt_pipeline.go`); the realtime executor's
`server_vad` block is built in
`integrations/voice/agent/realtime_vad.go`. Before changing the
defaults, confirm there that the silence/threshold knobs are still
mapped to the session config -- OpenAI has revised the Realtime
session schema before; future API upgrades may force a re-tune.
