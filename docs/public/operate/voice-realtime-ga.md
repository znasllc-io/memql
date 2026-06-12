---
title: OpenAI Realtime API (GA) production reference
audience: public
status: stable
area: operate
sinceVersion: 0.9.45
owner: znas
---

# OpenAI Realtime API (GA) production reference

The voice-agent's realtime executor speaks OpenAI's GA Realtime API
(`gpt-realtime` over WebSocket). This is the production configuration
reference for that surface: what the API offers, what we set, and why.
Every claim below was verified against the current official docs
(developers.openai.com) in June 2026 via an adversarially-checked
research sweep (memql#1425). Where a recommendation is an inference
rather than a documented value, it is marked as such.

Companion doc: [voice-eou-tuning.md](voice-eou-tuning.md) covers
hands-on tuning of the end-of-utterance VAD knobs specifically.

## Doc-currency rule

The Realtime API reached GA on 2025-08-28; the beta protocol was
deprecated 2025-09-15 and fully REMOVED on 2026-05-12 (beta
connections are rejected with `beta_api_shape_disabled`). Consequences:

- Any documentation, blog post, or sample code published before
  2025-08-28 is suspect; anything using un-prefixed event names
  (`response.audio_transcript.delta`, `response.text.delta`) is
  beta-era and non-functional.
- GA `session.update` requires `session.type: "realtime"` (or
  `"transcription"` for transcription-only sessions) and nests
  audio/turn-detection config under the `audio` object. Our client
  (`integrations/openai/realtime.go`, `realtime_events.go`) is
  GA-shaped since #1382/#1383.

Sources: developers.openai.com/api/docs/changelog,
/api/docs/deprecations.

## Turn detection (VAD)

Two modes, configured per session under `audio.input.turn_detection`
(`null` disables model VAD entirely — our conductor-gated multi-party
posture):

| Mode | How it ends a turn | Tunables |
| --- | --- | --- |
| `server_vad` | Energy gate: turn starts when input crosses `threshold`, commits after `silence_duration_ms` below it | `threshold` (0.0-1.0, default 0.5), `prefix_padding_ms` (default 300), `silence_duration_ms` (default 500), `idle_timeout_ms` (5000-30000, server_vad only) |
| `semantic_vad` | Semantic classifier on the user's words | `eagerness`: `low` / `medium` / `high` / `auto` (= medium) |

Both modes share `create_response` and `interrupt_response` booleans.

The documented anti-noise knob is raising the `server_vad` threshold:
"a higher threshold will require louder audio to activate the model,
and thus might perform better in noisy environments." We run
`server_vad` deliberately on the native 1-on-1 path — `semantic_vad`
stalled 6-15s before committing finished turns and broke barge-in
(see `integrations/voice/agent/realtime_vad.go`).

Source: developers.openai.com/api/docs/guides/realtime-vad,
/api/reference/resources/realtime/client-events.

## Input noise reduction

`audio.input.noise_reduction` has exactly two modes; it filters audio
BEFORE it reaches VAD and the model, with the documented effect of
"reducing false positives" in turn detection:

- `near_field` — close-talking mics (headphones).
- `far_field` — "far-field microphones such as laptop or conference
  room microphones."

For our primary use case (users on laptop speakers/mics), `far_field`
is the directly documented mitigation for phantom turn activation.
This is the first server-side line of defense against speaker echo;
client-side acoustic echo cancellation on the published mic track is
the zeroth (see memql#1431).

Source: developers.openai.com/api/reference/resources/realtime/client-events.

## Input transcription (user utterances)

Input transcription is NOT native to the realtime model. It runs
asynchronously on a separate ASR model, so transcription events may
arrive before or after Response events, and OpenAI explicitly states
the transcript "may diverge somewhat from the model's interpretation,
and should be treated as a rough guide." User transcripts are
non-authoritative BY DESIGN — render them as best-effort, never treat
them as ground truth of what the model heard.

Models (current): `gpt-realtime-whisper` (natively streaming, added
~2026-05), `gpt-4o-transcribe` (higher accuracy), 
`gpt-4o-mini-transcribe` (lower cost — our default via
`MEMQL_REALTIME_TRANSCRIPTION_MODEL`), `gpt-4o-transcribe-diarize`,
`whisper-1` (legacy).

Confidence gating: per-token logprobs are available via the session
`include` option with value `item.input_audio_transcription.logprobs`
— documented as usable "to determine how confident the model is in
the transcription." Two caveats: logprobs are only documented for the
gpt-4o-transcribe family (not whisper-1), and dropping low-confidence
transcripts is an application pattern built on the signal, not a
built-in gate. Community reports note logprobs are intermittently
missing; the gate must tolerate absence.

Events: `conversation.item.input_audio_transcription.delta` /
`.completed` / `.failed` (our client already consumes the GA names).

Source: developers.openai.com/api/docs/guides/realtime-transcription,
/api/reference/resources/realtime/server-events.

## Output transcript (what the assistant actually said)

The transcript of the assistant's SPOKEN audio streams as:

- `response.output_audio_transcript.delta` — append-only chunks.
- `response.output_audio_transcript.done` — finalizes; ALSO emitted
  when a response is interrupted, incomplete, or cancelled.

The canonical rendering pattern is append-on-delta, seal-on-done.
This event stream — not the text channel, not any post-processed
authored message — is the single source of truth for the assistant's
voice bubble (memql#1427).

Barge-in caveat: transcript and audio cannot be precisely aligned, so
streamed deltas may include text for audio the user never heard.
`conversation.item.truncate` is what reconciles the server context
(below).

Source: developers.openai.com/api/reference/resources/realtime/server-events,
/api/docs/guides/realtime-conversations.

## Function calling (async, non-blocking)

The GA round trip: the model emits an output item of type
`function_call` (name, JSON-string `arguments`, `call_id`; arguments
streamable via `response.function_call_arguments.delta`), the
application executes, then injects:

1. `conversation.item.create` with
   `{type: "function_call_output", call_id: <matching>, output: <JSON string>}`
2. `response.create` to trigger continued inference.

The async pattern — acknowledge in voice immediately, run the tool in
the background, inject the result when it lands — is explicitly
supported: conversation items can be added mid-stream, the session
continues while a call is pending, and responses can run out-of-band
with `conversation: "none"`. Multiple responses can run in parallel,
but only ONE response can write to the default conversation at a
time. The voice prompt must teach acknowledge-first behavior; the
protocol supports it but does not produce it by itself (memql#1430).

No documented hard limit on pending tool calls was found. Behavior
when the user barges in before a tool result lands is only indirectly
documented (the pending call_id survives in conversation history);
verify on staging.

Source: developers.openai.com/api/docs/guides/realtime-conversations#function-calling,
/api/reference/resources/realtime/client-events,
developers.openai.com/blog/realtime-api.

## Interruption / barge-in (WebSocket transport)

Over WebSocket — our transport, since the voice-agent bridges
LiveKit — the CLIENT manages playback, so on barge-in
(`input_audio_buffer.speech_started`) the client must:

1. Stop local playout immediately.
2. Send `conversation.item.truncate`
   `{item_id, content_index: 0, audio_end_ms: <played ms>}`. The
   server truncates the audio AND deletes the server-side text
   transcript "to ensure there is not text in the context that hasn't
   been heard by the user", confirming with
   `conversation.item.truncated`. An `audio_end_ms` greater than the
   actual audio duration is an error.
3. Send `response.cancel` if a response is still in progress
   (`server_vad` with `interrupt_response: true` auto-cancels).

Skipping the truncate step leaves unheard text in the model's
context: the model then believes the user heard things they did not,
which corrupts subsequent turns. Our executor currently cancels and
cuts playout but does NOT send truncate — tracked in memql#1427's
audit.

(`output_audio_buffer.clear` is WebRTC/SIP-only; not applicable to
us.)

Source: developers.openai.com/api/reference/resources/realtime/client-events.

## Session lifecycle

- Hard server-side cap: 60 minutes per realtime session (raised from
  the beta-era 30 at GA). A production agent must rotate sessions and
  carry state forward; there is NO documented official resume
  mechanism — state carryover is application-level
  (`conversation.item.create` replay or summary injection).
- Our self-imposed cap is `MEMQL_REALTIME_MAX_SESSION_SEC` (default
  1800), below the API ceiling.
- Documented rate limits and built-in context-trimming knobs for long
  sessions were NOT found in the verified doc set; treat token growth
  as an application concern (`MEMQL_REALTIME_MAX_AUDIO_TOKENS`
  bounds ours).

Source: developers.openai.com/api/docs/guides/realtime-conversations,
developers.openai.com/blog/realtime-api.

## Recommended settings (laptop-speaker users, LiveKit-bridged)

Synthesized from the documentation above; numeric values marked (inf)
are inferences from documented tuning direction, not OpenAI-published
numbers.

| Setting | Value | Why |
| --- | --- | --- |
| `session.type` / model | `realtime` / `gpt-realtime` | GA shape |
| `audio.input.noise_reduction` | `far_field` | Documented for laptop mics; reduces VAD false positives |
| `turn_detection.type` | `server_vad` | Deterministic; semantic_vad stalled 6-15s for us |
| `turn_detection.threshold` | 0.6-0.7 (inf) | Documented anti-noise direction; we ship 0.6 |
| `turn_detection.silence_duration_ms` | 500-800 (inf) | Higher tolerates echo tails; trade against end-of-turn lag (see voice-eou-tuning.md) |
| `turn_detection.prefix_padding_ms` | 300 | API default |
| `turn_detection.idle_timeout_ms` | unset | With phantom-transcript concerns, silence must never auto-trigger |
| `interrupt_response` / `create_response` | `true` / `true` | Native barge-in + auto-author on the 1-on-1 path |
| Input transcription | `gpt-4o-mini-transcribe` + `include: ["item.input_audio_transcription.logprobs"]` | Logprob gating requires the gpt-4o-transcribe family |
| Transcript gate | Drop finals below a logprob floor; tolerate missing logprobs | Application-level; belt to the VAD threshold's suspenders |
| Assistant bubble | Append `response.output_audio_transcript.delta`, seal on `.done` | The only stream that matches spoken audio |
| Barge-in | Stop playout + `conversation.item.truncate` + `response.cancel` | WebSocket transport: client owns playback truth |
| Tools | Acknowledge first, background execution, `function_call_output` + `response.create` on completion | Non-blocking conversation |
| Session rotation | Before the 60-min hard cap, state carried forward | No official resume mechanism |

## Current memql posture vs this reference

Where the code stands as of 0.9.45 (gap details and ownership in the
epic, memql#1424):

- GA event names, session shape, server_vad 0.6/300/500: in place
  (`integrations/openai/realtime_events.go`,
  `integrations/voice/agent/realtime_vad.go`).
- `noise_reduction`: NOT SET — no code path emits it (memql#1431).
- Logprob `include` + confidence gate on the native path: NOT SET;
  the #1199 post-hoc transcript filter is the only guard (memql#1431).
- `conversation.item.truncate` on barge-in: NOT SENT (memql#1427).
- Async acknowledge-first tool prompting: not yet taught
  (memql#1430).
