# voice-agent: handoff for the next debugging session

> **Status (2026-05-12):** Initiative C is shipped end-to-end -- gRPC contract,
> Python scaffold, plugin wiring, server interceptor, copresent UI, retired
> Bridge Agent, refreshed docs. Auth works. Dispatch works. Persona resolution
> works. **Voice input does not work**: user speaks, no transcripts produced,
> no GA reply via voice. Text path (greet-on-join, chat replies) works.
>
> Root cause (high confidence): `MemqlLLM` does not properly subclass
> `livekit.agents.llm.LLM`. Secondary investigation needed: audio routing
> from the LiveKit room into Deepgram STT.
>
> **This document is the handoff.** Delete it once voice input is producing
> transcripts AND the GA replies audibly. See "Definition of done" at the bottom.

---

## 0. Hard constraints from the user

These are non-negotiable. Do NOT relitigate.

1. **memql is the brain.** Do not swap `MemqlLLM` for a stock OpenAI / Anthropic
   LLM plugin "to make it work." The user explicitly rejected that path. memql's
   cognition pipeline owns the conductor + agent tool loop; voice-agent must
   call memql via the `VoiceAgent*` gRPC contract for the GA's reply.
2. **The current Initiative-C architecture stays.** No re-architecting. The
   Python voice-agent is the right shape; the LLM plugin just needs to conform
   to LiveKit Agents 1.5's ABC contract.
3. **Operate within the project's standing rules**:
   - Commit directly to `main` on both repos. No feature branches.
   - Stage files with `git add <path>` per file. Never `git add -A`.
   - Pre-production: delete cleanly, no `@deprecated` shims, no fallback flags.
   - No emojis in any output.
   - Memql says "SI"; copresent says "AI" in user-facing copy.

---

## 1. Where we are -- what works

### 1.1 Shipped + verified working
- Voice-agent Docker image builds and the worker registers with LiveKit on
  startup (`registered worker` log line).
- Job dispatch: when a user joins a LiveKit room, the worker receives a job
  and the entrypoint subprocess starts. Log: `received job request` followed
  by `agent entrypoint job_id=...`.
- Shared-secret auth: voice-agent's bearer (`Bearer mql_va_<secret>`) is
  admitted by the BFF's `voice_agent_stream_interceptor.go` and pinned to
  the `VoiceAgent*` message surface. BFF log: `gRPC stream opened
  subject=voice-agent`.
- `VoiceAgentSessionStart` round-trip works: voice-agent gets back
  canonical voice (`alto` → `aura-2-asteria-en`), initial gate state, etc.
  BFF log: `voice-agent session start`.
- `persona resolved canonical_voice=alto tts_voice=aura-2-asteria-en` fires.
- Deepgram STT plugin instantiates with correct params (Nova-3, en, 500ms
  endpointing).
- Deepgram TTS plugin instantiates with correct model id.
- Anam avatar plugin disabled gracefully when no persona id is set.
- Lifecycle: entrypoint blocks on `await asyncio.Future()` until participant
  disconnects, then `finally:` fires `VoiceAgentSessionEnd` cleanly.
- `wait_for_participant()` returns once the user joins.
- The GA introduces herself in chat on space-join (greet-on-join automation;
  cognition path, separate from voice-agent).

### 1.2 NOT working
- User publishes mic audio to the LiveKit room → voice-agent receives the
  job and enters the room → **but no Deepgram transcripts ever fire**. No
  `user_input_transcribed` event on the AgentSession. No partial/final
  transcripts forwarded to memql.
- Consequence: no `VoiceAgentTurnRequest` is ever sent → cognition is never
  asked for a voice reply → no audio response from the GA.

---

## 2. Root cause: high-confidence + secondary suspects

### 2.1 Primary suspect: MemqlLLM doesn't conform to the ABC contract

`livekit.agents.llm.LLM` is an ABC. So is `LLMStream`. My current
implementation in `voice-agent/voice_agent/memql_llm_plugin.py` is just a
duck-typed plain class. The framework may be silently rejecting it during
session start, which would explain why none of the downstream pipeline
(STT → LLM → TTS) ever engages.

Read the actual contract here inside the running container:

```
docker exec polyphon-voice-agent sh -c "cat /usr/local/lib/python3.12/site-packages/livekit/agents/llm/llm.py" | less
```

Key abstract methods:
- `LLM.chat(*, chat_ctx, tools, conn_options, parallel_tool_calls, ...)` ->
  returns an `LLMStream`.
- `LLMStream._run()` -- the actual work. Produces `ChatChunk` objects and
  writes them via `self._event_ch.send_nowait(chunk)` (or `await
  self._event_ch.send(chunk)`).

A `ChatChunk` is a pydantic model: `{id: str, delta: ChoiceDelta?, usage:
CompletionUsage?}`. A `ChoiceDelta` is `{role: ChatRole?, content: str?,
tool_calls: list[FunctionToolCall], extra: dict[str, Any]?}`.

The streaming pattern: each token from memql's `VoiceAgentTurnDelta`
becomes a `ChatChunk(id=request_id, delta=ChoiceDelta(content=token))`.
The terminal `VoiceAgentTurnComplete` either fires a final empty-content
chunk or just closes the event channel (whichever lk-agents expects).

### 2.2 Secondary suspect: audio routing

Independent of the LLM, the STT plugin may not be receiving audio frames
at all. Diagnostic check: tail voice-agent logs while the user speaks and
look for ANY Deepgram-side log. If you see nothing from
`livekit.plugins.deepgram` even with a properly-subclassed LLM, the issue
is upstream.

Things to check on the audio path:
- `RoomIO._init_task` should call `set_participant(participant.identity)`
  once a participant joins. Verify this fires.
- The user's participant in the room must have kind `STANDARD` and must
  NOT have the `ATTRIBUTE_PUBLISH_ON_BEHALF` attribute pointing at the
  agent's identity (that would cause RoomIO to skip the participant; see
  `room_io/room_io.py:_on_participant_connected`).
- The LiveKit token minted by memql for the user might be setting
  attributes that cause RoomIO to reject them. Check
  `component/polyphon/localroom.go` for how the token is built.

### 2.3 Tertiary: STT plugin event surface

Even with correct LLM + audio, my event binding may be wrong. The actual
event in lk-agents 1.5 is `user_input_transcribed`, emitting a
`UserInputTranscribedEvent` with fields `{transcript: str, is_final: bool,
speaker_id: str?, language: str?}`. My `stt_plugin.py` already binds the
right name -- but verify it actually fires.

---

## 3. Investigation paths exhausted in this session

Squashed bugs (don't redo):

1. `MEMQL_DEEPGRAM_API_KEY` shadowed empty in voice-agent container via the
   `environment:` vs `env_file:` precedence -- fixed by dropping the
   re-declaration. Commit `e9d1013`.
2. `_CONFIG` module-level global didn't survive LiveKit Agents subprocess
   fork -- fixed by lazy-loading in `_config()`. Commit `77674cd`.
3. `MEMQL_VOICE_AGENT_SHARED_TOKEN` shadowed empty on BFF (same env_file
   footgun) -- fixed by adding it to `.env.local` directly. Commit
   `2f3ce64`.
4. Voice-agent interceptor was calling `base()` instead of `handler()`,
   routing the shared-secret token through the JWT verifier which rejected
   it -- fixed to call `handler()` directly. Commit `f0d19c2`.
5. Deepgram TTS API mismatch (`voice` kwarg doesn't exist; the per-voice id
   IS the model) -- fixed. Commit `b681e70`.
6. `avatarVendor=""` failed enum validation in `mutationCreateAgent`,
   blocking GA seeding -- fixed by switching to shorthand
   `args.avatarVendor` so the field omits when unset. Commit `b681e70`.
7. `await session.start()` returns immediately in lk-agents 1.5 (does NOT
   block until session ends); my entrypoint's `finally:` fired in ~15ms,
   closing the gRPC client and tearing down the path -- fixed by adding
   `await asyncio.Future()` after start. Commit `6600646`.
8. STT event handler was bound to non-existent
   `user_speech_committed` event -- fixed to bind only
   `user_input_transcribed` (which carries `is_final` for both interim
   and final). Commit `169eaa5`.
9. Missing VAD on AgentSession; `livekit-plugins-silero` was in
   pyproject.toml but unused. Added `vad=silero.VAD.load()`. Commit
   `fedcca5`.
10. Race between session.start() and participant join -- added
    `await ctx.wait_for_participant()` before session.start(). Commit
    `ba295dc`.

After all of these, voice-agent is structurally correct enough that the
session runs to completion when the user disconnects (no crash, clean
SessionEnd, clean shutdown). But STT events still don't fire.

---

## 4. Recommended next-session approach

Do NOT one-shot edit. Do this in order:

### 4.1 Verify the audio pipeline first (before LLM rework)

Even if MemqlLLM is broken, the AgentSession should still emit
`user_input_transcribed` events when the user speaks -- those fire from
the STT side of the pipeline, upstream of the LLM. Their absence is the
real puzzle.

Add WARNING-level logging at every step of the audio path inside
voice-agent. Concretely:

1. In `entrypoint()` after `wait_for_participant`, log every track the
   participant has published and its kind.
2. Subscribe directly to `ctx.room.on("track_subscribed", ...)` in
   `entrypoint()` and log every track-subscribed event with the
   participant identity and track kind.
3. After `session.start()`, log `session.input.audio` to confirm RoomIO
   wired an audio source.
4. Look for ANY logging from `livekit.agents.voice.room_io._input` or
   `livekit.plugins.deepgram` at INFO/DEBUG level. If silent, audio
   isn't reaching them.

If audio isn't reaching Deepgram, the LLM fix won't help -- the input
side is broken. Focus there first.

### 4.2 Then rewrite MemqlLLM properly

Once audio reaches STT and you see `user_input_transcribed` events,
rewrite `voice-agent/voice_agent/memql_llm_plugin.py`:

```python
from livekit.agents import llm
from livekit.agents.llm import (
    LLM, LLMStream, ChatChunk, ChoiceDelta, ChatContext,
)

class MemqlLLM(LLM):
    def __init__(self, client, space_id, ga_agent_id, thread):
        super().__init__()
        self._client = client
        ...

    def chat(self, *, chat_ctx, tools=None, conn_options=..., **kwargs):
        return MemqlLLMStream(
            llm=self,
            chat_ctx=chat_ctx,
            tools=tools or [],
            conn_options=conn_options,
            memql_client=self._client,
            space_id=self._space_id,
            ...
        )


class MemqlLLMStream(LLMStream):
    def __init__(self, *, llm, chat_ctx, tools, conn_options,
                 memql_client, space_id, ga_agent_id, thread):
        super().__init__(llm=llm, chat_ctx=chat_ctx, tools=tools,
                         conn_options=conn_options)
        self._memql_client = memql_client
        ...

    async def _run(self) -> None:
        # 1. Extract latest user message from self._chat_ctx
        # 2. Determine current thread state
        # 3. Send VoiceAgentTurnRequest via self._memql_client
        # 4. For each VoiceAgentTurnDelta:
        #      chunk = ChatChunk(id=request_id,
        #                        delta=ChoiceDelta(role="assistant",
        #                                          content=text_delta))
        #      self._event_ch.send_nowait(chunk)
        # 5. On VoiceAgentTurnComplete: nothing to send; the channel
        #    closes when _run returns and _main_task finishes.
```

Things to double-check while writing this:
- Whether `_event_ch.send_nowait` or `await _event_ch.send` is the right
  API (look at how openai/anthropic plugins do it).
- Whether `ChatChunk.id` needs a unique uuid per chunk or one per turn.
- How to surface `tool_calls` from memql back through `ChoiceDelta` --
  but actually we don't need this; memql executes tools server-side
  before the prose ever reaches voice-agent.

### 4.3 Then test end-to-end

`make dev-refresh` (full wipe) once both changes land. Speak. Confirm:
- voice-agent log: `user_input_transcribed final=True speaker=... len=N`
- BFF log: `voice-agent partial`, `voice-agent final`, `voice-agent turn
  request`
- Audio plays back from the GA's Aura-2 TTS

### 4.4 Delete this doc

Per the no-stale-docs rule. The wire contract lives in
`component/grpc/memql.proto`, the implementation lives in
`voice-agent/voice_agent/`, both self-document from there.

---

## 5. Code surface to focus on

```
voice-agent/voice_agent/main.py              # entrypoint; lifecycle is correct now
voice-agent/voice_agent/memql_llm_plugin.py  # NEEDS REWRITE -- ABC subclass
voice-agent/voice_agent/stt_plugin.py        # event binding looks correct
voice-agent/voice_agent/tts_plugin.py        # model param fix applied; verify
voice-agent/voice_agent/grpc_client.py       # works
voice-agent/voice_agent/persona_resolver.py  # works
voice-agent/voice_agent/transcript_forwarder.py # works

component/grpc/voice_agent_handlers.go       # PartialTranscript / FinalTranscript / TurnRequest stubs
component/grpc/voice_agent_stream_interceptor.go  # auth works
component/grpc/memql.proto                   # VoiceAgent* family at line ~1500
```

LK-Agents 1.5 source files to read inside the running container:

```
/usr/local/lib/python3.12/site-packages/livekit/agents/llm/llm.py
/usr/local/lib/python3.12/site-packages/livekit/agents/voice/agent_session.py
/usr/local/lib/python3.12/site-packages/livekit/agents/voice/agent.py
/usr/local/lib/python3.12/site-packages/livekit/agents/voice/room_io/room_io.py
/usr/local/lib/python3.12/site-packages/livekit/agents/voice/room_io/_input.py
/usr/local/lib/python3.12/site-packages/livekit/agents/voice/events.py
```

Compare against a known-working integration:

```
/usr/local/lib/python3.12/site-packages/livekit/plugins/deepgram/   # STT/TTS adapter pattern
```

---

## 6. Definition of done

The work is complete -- and this document is deleted -- when:

1. User joins a copresent space, clicks the mic, speaks a sentence.
2. Voice-agent log shows
   `user_input_transcribed final=True speaker=<identity> len=<N>`.
3. BFF log shows `voice-agent final transcript ... thread=group` and
   `voice-agent turn request`.
4. The user hears the GA reply audibly through their browser.
5. The transcribed user utterance AND the GA's reply both appear in
   chat as v1:cognition:utterance rows.

When all five are true, delete this doc in the same commit that ships
the fixes, and update memql/CLAUDE.md's "Voice + Video Pipeline" section
if any behavior changed materially from what Phase 12 documented.

---

## 7. Test environment

User runs `make dev-refresh` from `~/projects/memql`. That brings up the
full cluster including `polyphon-voice-agent`. Local domain is
`local.znas.io`. CoPresent dev server runs separately at
`~/projects/copresent` -- the user starts that themselves with
`npm run dev`.

Required env in `~/projects/memql/.env.local` (already set by the user):
- `MEMQL_DEEPGRAM_API_KEY`
- `VOICE_AGENT_SHARED_TOKEN`
- `MEMQL_VOICE_AGENT_SHARED_TOKEN` (same value as above)
- `ANAM_API_KEY`
- `SIMLI_API_KEY`
- `MEMQL_SI_OPENAI_API_KEY`, `MEMQL_SI_ANTHROPIC_API_KEY`

For fast iteration on voice-agent only (preserves DB state):

```
cd ~/projects/memql && git pull && \
  docker compose -f docker/docker-compose.full.yml \
                 -f docker/docker-compose.polyphon.yml \
  up -d --build --force-recreate --no-deps voice-agent
```

Logs to watch:
```
docker logs -f polyphon-voice-agent
docker logs -f memql-bff | grep -iE "voice-agent|VoiceAgent"
```
