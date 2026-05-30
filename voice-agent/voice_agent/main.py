"""LiveKit Agents 1.5 worker entry point.

Each connection to a memql space spins up an AgentSession with:

- Deepgram Nova-3 STT (Phase 3)
- memql LLM custom plugin (Phase 4)
- Deepgram Aura-2 TTS (Phase 5)
- Anam (or Simli) avatar (Phase 9)

Phase 2 (this commit) ships the worker that connects to a room and
opens a memql gRPC session; the plugins are stubbed until later phases.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any

from livekit.agents import (
    Agent,
    AgentSession,
    JobContext,
    WorkerOptions,
    cli,
)

from voice_agent.avatar_drive import start_avatar
from voice_agent.avatar_plugin import build_avatar
from voice_agent.config import Config, load_config, setup_logging
from voice_agent.grpc_client import MemqlGrpcClient
from voice_agent.memql_llm_plugin import MemqlLLM
from voice_agent.persona_resolver import Persona, resolve_persona
from voice_agent.realtime_executor import select_voice_executor
from voice_agent.realtime_lifecycle import (
    RealtimeSessionLifecycle,
    TeardownReason,
    build_realtime_lifecycle,
)
from voice_agent.stt_plugin import attach_transcript_forwarding, build_stt
from voice_agent.thread_context import ThreadContext
from voice_agent.transcript_forwarder import TranscriptForwarder
from voice_agent.tts_plugin import build_tts

logger = logging.getLogger(__name__)


# Per-process config cache. LiveKit Agents 1.5 spawns each job in a
# FRESH subprocess (via job_proc_lazy_main), so the module is imported
# clean per job and a module-level _CONFIG set in `run()` does not
# survive the fork. We load lazily on first access -- env vars are
# inherited across the subprocess boundary, so the resolved config
# is identical to what the parent worker saw.
_CONFIG: Config | None = None


def _config() -> Config:
    global _CONFIG  # noqa: PLW0603 -- per-process singleton
    if _CONFIG is None:
        _CONFIG = load_config()
        setup_logging(_CONFIG.log_level)
    return _CONFIG


async def entrypoint(ctx: JobContext) -> None:
    """LiveKit Agents per-room entrypoint.

    Runs once per LiveKit job (one per room the worker is dispatched to).
    """
    cfg = _config()
    logger.info("agent entrypoint job_id=%s room=%s", ctx.job.id, ctx.room.name)

    await ctx.connect()

    # Wire a track-subscribed listener BEFORE wait_for_participant so we
    # see every published audio track, including ones that arrive after
    # we return from wait_for_participant. Diagnostic for the case
    # where audio never reaches Deepgram: if we never see an audio
    # track_subscribed event, the room/token is the problem and no
    # amount of plugin rework will fix it.
    def _on_track_subscribed(track: Any, publication: Any, participant: Any) -> None:
        logger.info(
            "track_subscribed kind=%s identity=%s source=%s name=%s muted=%s",
            getattr(track, "kind", "?"),
            getattr(participant, "identity", "?"),
            getattr(publication, "source", "?"),
            getattr(publication, "name", "?"),
            getattr(publication, "muted", "?"),
        )
    ctx.room.on("track_subscribed", _on_track_subscribed)

    def _on_track_published(publication: Any, participant: Any) -> None:
        # Fires when a remote participant publishes a track. The worker
        # may or may not subscribe to it depending on RoomInputOptions
        # auto-subscribe defaults. If we see a publish but never a
        # subsequent track_subscribed for the same sid, the worker is
        # actively choosing not to subscribe -- usually because
        # set_participant() didn't match the publisher.
        logger.info(
            "track_published kind=%s identity=%s source=%s name=%s",
            getattr(publication, "kind", "?"),
            getattr(participant, "identity", "?"),
            getattr(publication, "source", "?"),
            getattr(publication, "name", "?"),
        )
    ctx.room.on("track_published", _on_track_published)

    # Wait for at least one remote participant before starting the
    # session, so the AgentSession's auto-subscribe picks up their
    # audio track. Without this, session.start() can race ahead of
    # participant join and the STT pipeline ends up with no audio
    # source -- speaking produces no transcripts even though every
    # other piece of the stack is wired correctly.
    participant = await ctx.wait_for_participant()
    _log_participant(participant)

    # The room name is `polyphon-<spaceSlug>` set by memql when it mints
    # the LiveKit token (see component/polyphon/localroom.go). We strip
    # the prefix so what we send to memql matches the participant row's
    # spaceID field exactly -- the cognition utterance validator does a
    # canonical id compare, and `polyphon-daily-...` canonicalizes to a
    # different concept-id than the participant row's `daily-...`,
    # causing the utterance insert to fail with "participant does not
    # belong to space".
    room_name = ctx.room.name
    space_id = room_name.removeprefix("polyphon-") if room_name.startswith("polyphon-") else room_name
    # Phase 2 stub -- Phase 6 wires this from the LiveKit token metadata or
    # via an extra ParticipantConnected lookup.
    ga_agent_id = f"{space_id}-ga"

    client = MemqlGrpcClient(cfg.memql_grpc_addr, cfg.voice_agent_token)
    await client.connect()
    persona = await resolve_persona(
        client=client,
        space_id=space_id,
        ga_agent_id=ga_agent_id,
        room_name=room_name,
        avatar_vendor=cfg.avatar_vendor,
    )
    logger.info(
        "persona resolved canonical_voice=%s tts_voice=%s avatar_persona=%s "
        "initial_audio=%s initial_video=%s",
        persona.canonical_voice,
        persona.tts_voice_id,
        persona.avatar_persona_id,
        persona.initial_audio_mode,
        persona.initial_video_mode,
    )

    thread = ThreadContext(space_id=space_id)
    forwarder = TranscriptForwarder(client=client, space_id=space_id)

    # VAD: Silero. Required by AgentSession to gate the STT pipeline --
    # without it, audio frames never trigger Deepgram recognition. The
    # plugin loads a small ONNX model into the subprocess once at
    # session start; subsequent re-imports in the same process reuse it.
    from livekit.plugins import silero  # type: ignore
    vad = silero.VAD.load()
    # Phase 3: Deepgram Nova-3 STT.
    stt = build_stt(cfg)
    # Select the voice executor (#434). Default is the cascade -- realtime
    # is opt-in via MEMQL_VOICE_EXECUTOR=realtime and falls back to the
    # cascade automatically when unavailable, so the voice path always
    # comes up. See voice_agent/realtime_executor.py.
    executor_plan = select_voice_executor(cfg, persona)
    if executor_plan.fallback_reason:
        logger.warning(
            "voice executor fell back to cascade: %s",
            executor_plan.fallback_reason,
        )

    # Phase 4: memql LLM custom plugin -- VoiceAgentTurnRequest in,
    # VoiceAgentTurnDelta stream out. Specialists handled server-side.
    # Built for the cascade path; not used when the realtime executor is
    # selected (the realtime model is itself the speech-to-speech LLM).
    llm = MemqlLLM(
        client=client,
        space_id=space_id,
        ga_agent_id=ga_agent_id,
        thread=thread,
    )
    # Phase 5: Deepgram Aura-2 TTS with token-streaming input. Cascade
    # only -- the realtime model speaks its own audio output.
    tts = build_tts(cfg, persona)
    # Phase 9: Anam (or Simli) avatar.
    #
    # Lifecycle is intentionally DECOUPLED from the mic-click that
    # starts this session. Clicking the mic enables the user's audio
    # (and the agent's mirrored audio); the avatar should only spin
    # up when video is explicitly requested. So we build the avatar
    # ONLY when initial_video_mode is "always_on" at session start.
    #
    # "mirror_user" + user-camera-off and "always_off" both leave the
    # avatar disabled. Audio still flows via voice-agent's own
    # LiveKit participant track in those cases -- Sofia's voice
    # plays through the regular audio output, no Anam involved.
    #
    # To bring up the avatar: toggle Sofia's video icon to always_on
    # (orb-corner toggle, or long-press menu). That writes a
    # v1:cognition:videooverride row, which the BFF reads when
    # responding to the next VoiceAgentSessionStart. Mid-session
    # toggling requires re-opening the room today; dynamic avatar
    # start/stop driven by override events is a Phase 11 follow-up.
    avatar = None
    if persona.initial_video_mode == "always_on":
        try:
            avatar = build_avatar(cfg, persona)
        except RuntimeError as err:
            logger.warning("avatar disabled: %s", err)
    else:
        logger.info(
            "avatar deferred -- video mode is %r (only 'always_on' builds at session start)",
            persona.initial_video_mode,
        )

    # AgentSession.__init__ does NOT accept an `avatar` kwarg in
    # lk-agents 1.5 -- the AvatarSession plugin is started
    # separately via `await avatar.start(session, room, ...)` after
    # the AgentSession itself is up. Passing avatar in here was
    # silently dead code (the build_avatar() guard masked it) and
    # would have crashed the moment a persona id was available.
    if executor_plan.is_realtime:
        # Realtime executor (#434): the gpt-realtime model is wired as the
        # speech-to-speech LLM with turn_detection=None (conductor-gate
        # posture, #432 option A). No separate TTS -- the model speaks its
        # own audio. Deepgram STT stays attached (cascade-parity) so
        # transcripts still flow to memql for conductor scoring, citations
        # and chat parity per docs/voice/433-multiparty-audio-routing.md.
        #
        # TODO(#432): drive response.create / response.cancel from the
        #   conductor. This session is stood up in the gated posture only;
        #   the model will not self-respond until the conductor wiring lands.
        # TODO(#433): replace the single implicit audio input with the
        #   active-speaker multi-party router so >1 human attributes
        #   correctly.
        session = AgentSession(
            vad=vad,
            stt=stt,
            llm=executor_plan.realtime_model,
            turn_detection=None,
        )
        logger.info(
            "agent session built with realtime executor (turn_detection=None, #432)"
        )
    else:
        session = AgentSession(vad=vad, stt=stt, llm=llm, tts=tts)
        logger.info("agent session built with cascade executor (STT->cognition->TTS)")

    # Register the unsolicited VoiceAgentSpeak handler. memql pushes
    # one Speak per SI reply utterance that lands in this space while
    # audio output is enabled (always_on override) and no
    # VoiceAgentTurnRequest is in flight. The handler calls
    # AgentSession.say(text) which drives the existing Aura-2 TTS +
    # avatar lip-sync pipeline. This is how chat-typed user messages
    # produce audible replies: the agent reply utterance lands in
    # memql, BFF's session-long subscriber matches it, we synthesize
    # here. STT-driven turns still flow through the LLM plugin via
    # TurnRequest/TurnDelta and are skipped server-side by the
    # in-flight-turn dedup.
    async def _on_voice_agent_speak(envelope: Any) -> None:
        speak = envelope.voice_agent_speak
        text = (speak.text or "").strip()
        if not text:
            return
        logger.info(
            "voice-agent speak request_id=%s utterance_id=%s text_len=%d",
            speak.request_id, speak.utterance_id, len(text),
        )
        # Diagnostic: confirm we have a viable AgentSession output before
        # firing TTS. session.say can return without raising even when
        # no audio is published (e.g. when output isn't bound).
        try:
            audio_out = getattr(getattr(session, "output", None), "audio", None)
            logger.info(
                "voice-agent speak preflight output.audio=%s",
                "wired" if audio_out is not None else "MISSING",
            )
        except Exception:  # noqa: BLE001
            logger.exception("voice-agent speak preflight introspect failed")
        # Schedule on the event loop instead of awaiting inline. session.say
        # returns a SpeechHandle whose await completes only when playback
        # finishes -- blocking the read loop on it would prevent subsequent
        # server pushes from being dispatched and would log a flat
        # success/failure line. By launching a task we get prompt log
        # output and can also await the handle separately.
        async def _do_say(req_id: str, body: str) -> None:
            try:
                logger.info("voice-agent speak invoking session.say req_id=%s", req_id)
                handle = session.say(body)
                logger.info(
                    "voice-agent speak got handle req_id=%s handle=%s",
                    req_id, type(handle).__name__,
                )
                if handle is not None and hasattr(handle, "wait_for_playout"):
                    try:
                        await handle.wait_for_playout()
                        logger.info("voice-agent speak playout complete req_id=%s", req_id)
                    except Exception:  # noqa: BLE001
                        logger.exception(
                            "voice-agent speak playout wait failed req_id=%s",
                            req_id,
                        )
            except Exception:  # noqa: BLE001
                logger.exception(
                    "session.say failed request_id=%s", req_id
                )

        asyncio.create_task(_do_say(speak.request_id, text))

    client.set_push_handler("voice_agent_speak", _on_voice_agent_speak)

    transcript_task = await attach_transcript_forwarding(
        session=session,
        forwarder=forwarder,
        speaker_user_id_provider=_default_speaker_provider(ctx),
    )

    agent = Agent(
        instructions=(
            "You are the General Assistant for this memql space. The actual "
            "reasoning runs in memql's cognition service via the custom LLM "
            "plugin -- these instructions are a no-op placeholder until the "
            "LiveKit Agents framework needs a top-level agent prompt."
        ),
    )

    # Realtime session lifecycle + cost guardrails (#439). Only the
    # realtime executor stands up a speech-to-speech session whose
    # standing audio cost must be bounded; the cascade path leaves this
    # None. Started AFTER session.start() below so the guardrails arm
    # around a live session. See voice_agent/realtime_lifecycle.py.
    lifecycle: RealtimeSessionLifecycle | None = None

    try:
        # In LiveKit Agents 1.5, session.start() returns once IO is
        # set up -- it does NOT block until the session ends. To keep
        # the entrypoint (and thus the gRPC client connection) alive
        # for the full session lifetime, we await an unresolved future
        # after starting. The framework cancels this task when the
        # participant disconnects, which raises CancelledError and
        # routes us to the finally block for cleanup.
        await session.start(agent=agent, room=ctx.room)
        # Start the avatar AFTER the AgentSession is up. The Anam plugin
        # mints its own LiveKit token (with ATTRIBUTE_PUBLISH_ON_BEHALF
        # pointing at our local participant) and joins the room as a
        # separate participant whose video track is lip-synced to the
        # assistant's audio. The framework wires the lip-sync via
        # `agent_session.output.audio` reassignment inside
        # `avatar.start(...)` -- and BOTH executors forward into that same
        # sink, so the avatar is audio-source-agnostic (#438): it
        # lip-syncs the cascade Aura-2 TTS or the realtime gpt-realtime
        # audio depending only on which executor `executor_plan` selected.
        # `start_avatar` owns the single start seam and names the audio
        # source in the log; errors are non-fatal (audio-only fallback).
        await start_avatar(
            avatar=avatar,
            session=session,
            room=ctx.room,
            plan=executor_plan,
        )
        # Arm the realtime lifecycle + cost guardrails (#439). The
        # realtime session is built in the warm-but-muted posture
        # (turn_detection=None, #432 option A); this bounds its standing
        # cost: idle teardown, max-duration cap, per-session audio-token
        # budget, and immediate teardown when the room empties of humans.
        # On a guardrail trip the lifecycle stops the session and the
        # voice path can degrade to the cascade (lifecycle.should_degrade_
        # to_cascade). The cascade path never builds a lifecycle.
        if executor_plan.is_realtime:
            async def _stop_realtime(reason: TeardownReason) -> None:
                # Bring the realtime session down deterministically. Closing
                # the AgentSession releases the model connection so audio
                # tokens stop accruing; disconnecting the room unblocks the
                # entrypoint's await-Future and routes to the finally
                # cleanup. Errors here must not wedge teardown.
                logger.info(
                    "stopping realtime session space=%s reason=%s (#439)",
                    space_id, reason.value,
                )
                try:
                    aclose = getattr(session, "aclose", None)
                    if aclose is not None:
                        await aclose()
                except Exception:  # noqa: BLE001
                    logger.exception("realtime session aclose failed reason=%s", reason.value)
                try:
                    await ctx.room.disconnect()
                except Exception:  # noqa: BLE001
                    logger.exception("room disconnect failed reason=%s", reason.value)

            lifecycle = build_realtime_lifecycle(
                cfg, stop=_stop_realtime, space_id=space_id,
            )
            # Seed the human population from the participants already in the
            # room (the entrypoint waited for at least one before building
            # the session). STANDARD (human) kind only -- the avatar and any
            # ingress/egress participants are not humans to talk to.
            initial_humans = sum(
                1
                for p in ctx.room.remote_participants.values()
                if int(getattr(p, "kind", 0) or 0) == 0
            )
            await lifecycle.start(human_count=initial_humans)

            def _on_participant_connected(participant: Any) -> None:
                if int(getattr(participant, "kind", 0) or 0) != 0:
                    return  # non-human (avatar / ingress / egress)
                asyncio.create_task(lifecycle.note_human_joined())

            def _on_participant_disconnected(participant: Any) -> None:
                if int(getattr(participant, "kind", 0) or 0) != 0:
                    return
                asyncio.create_task(lifecycle.note_human_left())

            ctx.room.on("participant_connected", _on_participant_connected)
            ctx.room.on("participant_disconnected", _on_participant_disconnected)

        # Log the input wiring so we can confirm RoomIO bound the
        # participant's audio source. session.input.audio is None when
        # RoomIO rejected the participant (e.g. wrong kind, or
        # ATTRIBUTE_PUBLISH_ON_BEHALF pointed at us). Diagnostic only --
        # the framework owns the lifecycle.
        try:
            audio_in = getattr(getattr(session, "input", None), "audio", None)
            logger.info(
                "agent session started input.audio=%s linked_participant=%s",
                "wired" if audio_in is not None else "MISSING",
                getattr(audio_in, "linked_participant", None)
                if audio_in is not None else None,
            )
        except Exception:  # noqa: BLE001
            logger.exception("failed to introspect session.input.audio")
        logger.info("agent session started; waiting for room to close")
        await asyncio.Future()  # blocks until cancelled
    finally:
        transcript_task.cancel()
        # Stop the realtime lifecycle watchdog (#439). aclose only cancels
        # the watchdog task -- it does NOT invoke the stop callback, so it
        # is safe in the cleanup path regardless of whether teardown already
        # fired. The end-reason carries the guardrail trip when one happened
        # so memql audit can attribute the close.
        end_reason = "normal"
        if lifecycle is not None:
            if lifecycle.teardown_reason is not None:
                end_reason = lifecycle.teardown_reason.value
            await lifecycle.aclose()
        # Announce end-of-session to memql so audit + cleanup can fire.
        try:
            from voice_agent.proto import memql_pb2  # type: ignore

            await client.send_request(
                "voice_agent_session_end",
                memql_pb2.VoiceAgentSessionEnd(
                    space_id=space_id,
                    room_name=room_name,
                    reason=end_reason,
                ),
            )
        except Exception:
            logger.exception("voice agent session end notify failed")
        await client.close()


def _log_participant(participant: Any) -> None:
    """Dump everything we know about a participant on join.

    Lets us spot RoomIO-rejection causes at a glance: wrong `kind`
    (anything except STANDARD), or `ATTRIBUTE_PUBLISH_ON_BEHALF`
    pointing at the agent's identity, would both cause the
    participant's audio source never to be bound to the STT pipeline.
    Each published track is also enumerated -- if the participant
    joined without an audio track, that's the root cause, not us.
    """
    identity = getattr(participant, "identity", "?")
    sid = getattr(participant, "sid", "?")
    kind = getattr(participant, "kind", "?")
    attrs = dict(getattr(participant, "attributes", {}) or {})
    tracks = getattr(participant, "track_publications", {}) or {}
    track_summary = [
        {
            "sid": getattr(pub, "sid", "?"),
            "kind": getattr(pub, "kind", "?"),
            "source": getattr(pub, "source", "?"),
            "name": getattr(pub, "name", "?"),
            "muted": getattr(pub, "muted", "?"),
            "subscribed": getattr(pub, "subscribed", "?"),
        }
        for pub in tracks.values()
    ]
    logger.info(
        "participant joined identity=%s sid=%s kind=%s attributes=%s tracks=%s",
        identity, sid, kind, attrs, track_summary,
    )


def _default_speaker_provider(ctx: JobContext) -> Any:
    """Return a callable that yields the current speaker's user id.

    Single-human-per-room assumption: returns the identity of the
    first STANDARD (human) participant in the room. The previous
    impl returned `next(iter(remote_participants))` which, once the
    Anam avatar joined as an AGENT-kind sibling participant, would
    sometimes pick the avatar's identity ("anam-avatar-agent")
    instead of the human's. The transcript-forwarder then tried to
    insert a v1:cognition:utterance under that fake participantId
    and the mutation rejected with "participant not found" -- every
    user utterance failed to land, no transcript appeared in chat,
    no agent dispatch, no audio reply.

    Filtering for STANDARD kind keeps the avatar (and any other
    non-human participant: ingress, egress, etc.) out of speaker
    attribution. Multi-human rooms still need real per-track
    attribution; that's a Phase 6 follow-up.
    """
    def _resolve() -> str:
        if not ctx.room.remote_participants:
            return ""
        for participant in ctx.room.remote_participants.values():
            # rtc.ParticipantInfo.Kind: STANDARD=0 is the human-user
            # kind. AGENT/INGRESS/EGRESS/SIP all surface as non-zero
            # and are skipped here. Compare numerically rather than
            # importing the enum so this works whether the LiveKit
            # SDK returns a plain int or the enum value.
            kind = getattr(participant, "kind", 0)
            try:
                kind_val = int(kind)
            except (TypeError, ValueError):
                kind_val = 0
            if kind_val != 0:
                continue
            return participant.identity or ""
        return ""
    return _resolve


def _persona_unused_marker(_persona: Persona) -> None:
    """Keeps the import alive for type-checkers until Phase 5 uses it."""


def run() -> None:
    """Console-script entry. Loads config once in the parent worker process
    for the WorkerOptions wiring; job subprocesses re-load via _config()
    on first call (env vars are inherited across the subprocess fork).
    """
    cfg = _config()

    logger.info(
        "starting memql voice-agent livekit=%s memql=%s avatar=%s",
        cfg.livekit_url, cfg.memql_grpc_addr, cfg.avatar_vendor,
    )

    cli.run_app(
        WorkerOptions(
            entrypoint_fnc=entrypoint,
            ws_url=cfg.livekit_url,
            api_key=cfg.livekit_api_key,
            api_secret=cfg.livekit_api_secret,
        )
    )


if __name__ == "__main__":
    run()
