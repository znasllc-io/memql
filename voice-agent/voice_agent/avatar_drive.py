"""Audio-source-agnostic avatar drive for cascade and realtime (#438).

Part of the Hybrid Realtime Voice epic (#440). Sibling #434 introduced
the pluggable Realtime voice executor: when ``MEMQL_VOICE_EXECUTOR=
realtime`` the assistant's audio is emitted by OpenAI ``gpt-realtime``
rather than by the Deepgram Aura-2 TTS of the cascade. The avatar must
lip-sync *that* audio.

The good news -- established by reading the LiveKit Agents 1.5 internals
-- is that the avatar is already audio-source-agnostic *at the framework
level*, and this module's job is to make that invariant explicit, name
the audio source in the logs, and own the single avatar-start seam so
the cascade and realtime paths share one code path.

Why a single start path is correct (the invariant)
--------------------------------------------------
Both executors funnel the assistant's speech through the SAME sink:
``AgentSession.output.audio``.

* Cascade: the Aura-2 TTS frames are forwarded into ``output.audio`` via
  ``perform_audio_forwarding(audio_output=session.output.audio, ...)``
  inside the framework's generation task.
* Realtime: the ``gpt-realtime`` model's output frames
  (``msg.audio_stream``) pass through ``Agent.realtime_audio_output_node``
  (a passthrough by default) and are forwarded into the SAME
  ``output.audio`` sink via the same ``perform_audio_forwarding`` call in
  ``AgentActivity._realtime_generation_task_impl``.

The Anam plugin lip-syncs by *reassigning* that sink: ``avatar.start()``
sets ``agent_session.output.audio = DataStreamAudioOutput(room=...,
destination_identity=<avatar participant>)`` (see
``anam_persona_session.py`` and the stock ``anam.AvatarSession``).
Because the reassignment targets ``output.audio`` -- the very sink both
executors write to -- the avatar lip-syncs whatever the active executor
produces, with no per-frame plumbing and no executor-aware branching in
the avatar plugin itself. The avatar plugin (``avatar_plugin.py``) stays
agnostic: it constructs the vendor session; it never reads the executor.

Ordering note
-------------
``avatar.start()`` must run after ``AgentSession.start()`` (the avatar
mints its own LiveKit token and reassigns ``output.audio`` against the
live session), which is the existing cascade ordering. The realtime
session is stood up in the conductor-gate posture
(``turn_detection=None``, #432 option A): the model does not self-trigger
a response, so the assistant does not begin speaking until the conductor
drives ``response.create`` downstream. Starting the avatar immediately
after ``session.start()`` therefore reassigns ``output.audio`` before any
realtime audio is generated -- the avatar captures the realtime speech
from the first frame.

Scope (#438): route the realtime audio output to the Anam avatar so it
lip-syncs the realtime voice, with no regression to the cascade avatar
path. Out of scope and left to their owners: tools (#435), grounding
(#436), output capture (#437), session lifecycle (#439).
"""

from __future__ import annotations

import logging
from typing import Any

from voice_agent.realtime_executor import VoiceExecutorPlan

logger = logging.getLogger(__name__)


def audio_source_label(plan: VoiceExecutorPlan) -> str:
    """Human-readable name of the audio source the avatar lip-syncs.

    Used only for logging so an operator can confirm, at a glance, that
    the avatar is being driven by the realtime model's audio rather than
    silently lip-syncing the (unused) cascade TTS -- the exact regression
    #438 guards against. The avatar plugin itself never branches on this.
    """
    return "realtime gpt-realtime audio" if plan.is_realtime else "cascade Aura-2 TTS audio"


async def start_avatar(
    *,
    avatar: Any | None,
    session: Any,
    room: Any,
    plan: VoiceExecutorPlan,
) -> bool:
    """Start the avatar against the active session, audio-source-agnostic.

    ``avatar`` is the vendor session built by ``build_avatar`` (Anam or
    Simli), or ``None`` when video is disabled for this session -- in
    which case this is a no-op and the assistant rides audio-only on its
    own LiveKit participant track.

    Starting the avatar reassigns ``session.output.audio`` to the avatar's
    ``DataStreamAudioOutput``; because BOTH the cascade TTS and the
    realtime model forward their output into that same sink, the avatar
    lip-syncs whichever executor ``plan`` selected -- no executor-aware
    branching here. ``plan`` is consulted only to name the audio source in
    the log line.

    Errors are non-fatal: a failed avatar start leaves the voice path
    alive in audio-only mode, matching the cascade behaviour. Returns
    ``True`` when the avatar started, ``False`` otherwise (no avatar
    configured, or start failed).
    """
    if avatar is None:
        return False

    source = audio_source_label(plan)
    try:
        await avatar.start(session, room)
    except Exception:  # noqa: BLE001 -- avatar is best-effort; voice stays up
        logger.exception(
            "avatar start failed (driving from %s) -- falling back to audio-only",
            source,
        )
        return False

    logger.info(
        "avatar started; lip-syncing %s (executor=%s)",
        source,
        plan.kind.value,
    )
    return True
