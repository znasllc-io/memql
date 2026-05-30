# Latency, gate-cost & fidelity measurement (#484)

Phase 3 validation for epic #475. The v1 spikes (#432/#477) flagged that the
cheap-gate / overlapping-latency assumptions were never measured live. This
document is the **measurement harness**: the instrumentation that makes the
numbers capturable, the methodology, and the extraction recipe. The actual
numbers require a live credentialed room (OpenAI Realtime + LiveKit + Deepgram)
and a human speaking a few turns -- run the recipe below against a real session
to populate the results table.

## 1. The trace stamps (decision -> first audio)

All four stamps ride the existing structured voice-trace convention -- a
`"voice trace: turntaking event"` log line with a `stage` field -- so they sort
by the slog `time=` timestamp. Per turn, in order:

| Stamp | `stage`                       | Where (code)                                   | Meaning |
|-------|-------------------------------|------------------------------------------------|---------|
| **T0** | `voice.final`                | `realtime_executor.go` `forwardFinal`          | the human turn committed; the window opens |
| **T1** | `cognition.gate.engage`      | `cognition_handler.go` (voice gate, #479)      | the conductor gate decided engage (mode+brevity) |
| **T2** | `turntaking.assistant.speak` | `realtime_executor.go` `onAssistantStart`      | `response.create` is sent (executor-driven) |
| **T3** | `realtime.audio.first`       | `realtime_executor.go` `drainAudioOut`         | the first assistant audio frame is published |

Derived metrics:

- **Headline -- decision -> first audio:** `T3 - T1`. The number epic #475 is
  chasing: from the gate's decision to the first audible word.
- **Gate cost:** `T1 - T0`. Expected sub-millisecond on heuristic hits, bounded
  by one cached classifier call on the ambiguous residue (#477 section 3/4).
- **Model native TTFB:** `T3 - T2`. The realtime model's own first-audio budget
  (~170ms target).
- **End-to-end:** `T3 - T0`.

### Mode differences

- **Native 1-on-1 (#478, `turnModeNative`)**: the model owns the turn
  (`semantic_vad` + `create_response:true`), so there is **no conductor gate** --
  T1 is absent. The model self-triggers; the meaningful number is `T3 - T0`
  (commit -> first audio), which is dominated by the model's native TTFB. The
  defer/silence case emits **no** `realtime.audio.first` -- correctness by
  absence.
- **Multi-party gate (#481, `turnModeGatedSemanticVad`)** and the
  conductor-gated cascade path: all four stamps fire; `T3 - T1` is the headline,
  `T1 - T0` is the gate cost.
- **Baseline (v1 authoring path)**: the legacy path where cognition authored the
  reply text and the model re-voiced it; `T1 - T0` there is dominated by the
  ~1-1.5s authoring round-trip (`cognition_handler.go` comment). Compare the new
  `T1 - T0` against this baseline to confirm the authoring term is gone.

## 2. Extraction recipe

Stamps land in the voice node + voice-agent logs. Capture a session and run:

```bash
# Capture the voice-trace lines for a live session (a few spoken turns).
make dev-logs 2>&1 | grep "voice trace" > /tmp/voicetrace.log
# Or per-container:
#   docker logs polyphon-voice-agent 2>&1 | grep "voice trace" >> /tmp/voicetrace.log
#   docker logs memql-cognition     2>&1 | grep "voice trace" >> /tmp/voicetrace.log

# Compute per-turn deltas (T3-T1 headline, T1-T0 gate, T3-T2 model TTFB).
bash scripts/voice/latency.sh /tmp/voicetrace.log
```

`scripts/voice/latency.sh` pairs consecutive `voice.final` -> `cognition.gate.engage`
-> `turntaking.assistant.speak` -> `realtime.audio.first` stamps per space and
prints the deltas + a summary (median / p95). No silent caps -- it reports the
turn count it matched and flags any turn missing a stamp.

## 3. Fidelity (spoken == shown; user transcript accuracy)

- **Spoken == shown** is structural, not sampled (#482): the captured assistant
  utterance IS the model's spoken-audio transcript, forwarded verbatim
  (`captureOutput`), pinned by `TestRealtimeExecutor_SpokenEqualsShown`. The
  live check is a spot audit: confirm a handful of chat bubbles match the audio
  word-for-word. A divergence would be a code regression, not a rate.
- **User transcript accuracy** (native input transcription vs. truth): on the
  native/semantic_vad path the user utterance is the model's native input
  transcript. Deepgram still runs (attribution / fallback), so its final for the
  same turn is a convenient ground-truth comparison -- log both and diff. Report
  the agreement rate; sub-perfect agreement is a tuning question (#478 notes),
  not an architecture gap.

## 4. Results (populate from a live run)

| Metric | Native 1-on-1 | Multi-party gate | v1 baseline |
|---|---|---|---|
| decision -> first audio (T3-T1 / T3-T0 native) | _run_ | _run_ | ~1.2-1.7s |
| gate cost (T1-T0) | n/a (no gate) | _run_ | ~1-1.5s (authoring) |
| model native TTFB (T3-T2) | _run_ | _run_ | ~170ms (re-voice) |
| spoken==shown | structural (pinned) | structural | structural |
| user transcript agreement vs Deepgram | _run_ | _run_ | n/a |

> The instrumentation + harness are in place; the live numbers are the
> operational run (a credentialed room + a few spoken turns). The cluster and the
> auto-join dispatcher make that runnable: bring up the stack, open a space,
> speak a few turns, then run the extraction recipe.
