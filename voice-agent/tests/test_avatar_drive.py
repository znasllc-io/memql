"""Tests for the audio-source-agnostic avatar drive (#438).

These verify, without standing up LiveKit / OpenAI / Anam, that:

* the avatar is started against the active session for BOTH executors
  (cascade and realtime), so it is audio-source-agnostic;
* starting the avatar reassigns ``session.output.audio`` to the avatar
  sink -- the mechanism by which the avatar lip-syncs whatever the
  active executor wrote into ``output.audio`` (realtime audio when the
  realtime executor is active, cascade TTS otherwise);
* the cascade path is unaffected -- the same single start path runs and
  the same reassignment happens, so there is no regression;
* the source label names the realtime audio vs the cascade TTS audio;
* a missing avatar is a no-op (audio-only), and a failed avatar start is
  non-fatal (audio-only fallback), never propagating.

The avatar + session are mocked. The realtime/cascade distinction is
expressed through ``VoiceExecutorPlan`` exactly as ``main.py`` passes it.
"""

from __future__ import annotations

import pytest

from voice_agent.avatar_drive import audio_source_label, start_avatar
from voice_agent.realtime_executor import ExecutorKind, VoiceExecutorPlan


class _FakeAudioOutput:
    """Stands in for a DataStreamAudioOutput sink (the avatar sink)."""


class _FakeSessionOutput:
    def __init__(self) -> None:
        self.audio: object | None = None


class _FakeSession:
    """Minimal AgentSession stand-in -- only ``output.audio`` matters."""

    def __init__(self) -> None:
        self.output = _FakeSessionOutput()


class _FakeAvatar:
    """Mock avatar whose ``start`` reassigns ``session.output.audio``.

    Mirrors the real Anam/Simli plugin contract: ``AvatarSession.start``
    sets ``agent_session.output.audio`` to a ``DataStreamAudioOutput``
    pointed at the avatar participant. That reassignment is precisely how
    the avatar captures whatever audio the active executor forwards into
    the sink.
    """

    def __init__(self, *, fail: bool = False) -> None:
        self.fail = fail
        self.started_with: tuple[object, object] | None = None
        self.sink = _FakeAudioOutput()

    async def start(self, session: _FakeSession, room: object) -> None:
        self.started_with = (session, room)
        if self.fail:
            raise RuntimeError("anam engine session failed")
        # The real plugin reassigns output.audio to its DataStreamAudioOutput.
        session.output.audio = self.sink


def _plan(kind: ExecutorKind, *, realtime_model: object | None = None) -> VoiceExecutorPlan:
    return VoiceExecutorPlan(kind=kind, realtime_model=realtime_model)


# --- audio-source-agnostic: realtime audio is routed to the avatar sink ---


async def test_realtime_audio_routed_to_avatar_sink() -> None:
    # When the realtime executor is active, starting the avatar must
    # reassign output.audio to the avatar sink -- that sink is what the
    # framework forwards the realtime model's audio into, so the avatar
    # lip-syncs the realtime voice.
    session = _FakeSession()
    avatar = _FakeAvatar()
    room = object()

    started = await start_avatar(
        avatar=avatar,
        session=session,
        room=room,
        plan=_plan(ExecutorKind.REALTIME, realtime_model=object()),
    )

    assert started is True
    assert avatar.started_with == (session, room)
    # The avatar now owns output.audio: realtime frames forwarded by the
    # framework into output.audio reach the avatar for lip-sync.
    assert session.output.audio is avatar.sink


async def test_cascade_path_unaffected_same_routing() -> None:
    # The cascade path uses the identical single start path and the
    # identical output.audio reassignment -- no regression.
    session = _FakeSession()
    avatar = _FakeAvatar()
    room = object()

    started = await start_avatar(
        avatar=avatar,
        session=session,
        room=room,
        plan=_plan(ExecutorKind.CASCADE),
    )

    assert started is True
    assert avatar.started_with == (session, room)
    assert session.output.audio is avatar.sink


# --- no-op + failure semantics ---


async def test_no_avatar_is_noop() -> None:
    # Video disabled for this session: no avatar built. start_avatar is a
    # no-op, audio rides the assistant's own participant track, and
    # output.audio is left untouched.
    session = _FakeSession()

    started = await start_avatar(
        avatar=None,
        session=session,
        room=object(),
        plan=_plan(ExecutorKind.REALTIME, realtime_model=object()),
    )

    assert started is False
    assert session.output.audio is None


async def test_avatar_start_failure_is_non_fatal() -> None:
    # A failed avatar start must not propagate -- the voice path stays up
    # in audio-only mode.
    session = _FakeSession()
    avatar = _FakeAvatar(fail=True)

    started = await start_avatar(
        avatar=avatar,
        session=session,
        room=object(),
        plan=_plan(ExecutorKind.REALTIME, realtime_model=object()),
    )

    assert started is False
    # The reassignment never completed, so output.audio stays unset.
    assert session.output.audio is None


# --- source labelling (logging only; avatar plugin never branches on it) ---


def test_audio_source_label_realtime() -> None:
    label = audio_source_label(_plan(ExecutorKind.REALTIME, realtime_model=object()))
    assert "realtime" in label.lower()
    assert "gpt-realtime" in label


def test_audio_source_label_cascade() -> None:
    label = audio_source_label(_plan(ExecutorKind.CASCADE))
    assert "cascade" in label.lower()
    assert "tts" in label.lower()


@pytest.mark.parametrize(
    ("kind", "model"),
    [(ExecutorKind.REALTIME, object()), (ExecutorKind.CASCADE, None)],
)
async def test_start_avatar_is_source_agnostic_about_the_start_call(
    kind: ExecutorKind, model: object | None
) -> None:
    # Regardless of executor, the avatar is started identically against
    # the live session -- the avatar plugin never sees the executor kind.
    session = _FakeSession()
    avatar = _FakeAvatar()
    room = object()

    started = await start_avatar(
        avatar=avatar, session=session, room=room, plan=_plan(kind, realtime_model=model)
    )

    assert started is True
    assert avatar.started_with == (session, room)
