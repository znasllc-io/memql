"""Tests for the pluggable Realtime voice executor + cascade fallback (#434).

These cover the deterministic selection surface without standing up a
real OpenAI Realtime session:

* the default is the cascade (no regression),
* an explicit cascade selection stays cascade,
* a realtime selection builds the realtime model,
* a realtime selection FALLS BACK to the cascade when the model cannot
  be built (missing key, plugin import failure, construction error),
* the realtime model is constructed in the conductor-gate posture
  (turn_detection=None, #432 option A).

The OpenAI plugin is not installed in CI, so ``build_realtime_executor``
is exercised via a monkeypatched ``livekit.plugins.openai`` module rather
than the real one.
"""

from __future__ import annotations

import sys
import types

import pytest

from voice_agent.config import Config
from voice_agent.persona_resolver import Persona
from voice_agent.realtime_executor import (
    ExecutorKind,
    RealtimeExecutorError,
    VoiceExecutorPlan,
    build_realtime_executor,
    select_voice_executor,
)


def _config(*, voice_executor: str = "cascade", openai_api_key: str | None = "sk-test") -> Config:
    """A minimal Config for executor-selection tests.

    Only the fields the executor logic reads matter; the rest are filled
    with inert placeholders.
    """
    return Config(
        livekit_url="ws://livekit:7880",
        livekit_api_key="devkey",
        livekit_api_secret="secret",
        deepgram_api_key="dg-key",
        voice_executor=voice_executor,
        openai_api_key=openai_api_key,
        realtime_model="gpt-realtime",
        memql_grpc_addr="bff:50051",
        voice_agent_token="token",
        avatar_vendor="none",
        anam_api_key=None,
        simli_api_key=None,
        anam_default_persona_id=None,
        anam_default_avatar_id=None,
        anam_default_persona_name="Assistant",
        log_level="INFO",
        dg_asr_model="nova-3",
        dg_tts_model="aura-2",
        dg_language="en",
        dg_endpointing_ms=2000,
        dg_utterance_end_ms=0,
    )


def _persona() -> Persona:
    return Persona(
        canonical_voice="alto",
        tts_voice_id="aura-2-asteria-en",
        avatar_persona_id=None,
        avatar_vendor="",
        initial_audio_mode="always_on",
        initial_video_mode="always_off",
    )


class _FakeRealtimeModel:
    """Records the kwargs a real RealtimeModel would be built with."""

    def __init__(self, **kwargs: object) -> None:
        self.kwargs = kwargs


def _install_fake_openai_plugin(monkeypatch: pytest.MonkeyPatch, model_cls: type = _FakeRealtimeModel) -> None:
    """Inject a fake ``livekit.plugins.openai`` so the lazy import inside
    ``build_realtime_executor`` resolves without the real plugin."""
    realtime_mod = types.ModuleType("livekit.plugins.openai.realtime")
    realtime_mod.RealtimeModel = model_cls  # type: ignore[attr-defined]
    openai_mod = types.ModuleType("livekit.plugins.openai")
    openai_mod.realtime = realtime_mod  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "livekit.plugins.openai", openai_mod)
    monkeypatch.setitem(sys.modules, "livekit.plugins.openai.realtime", realtime_mod)


# --- selection: cascade is the default / no-regression invariant ---


def test_default_selection_is_cascade() -> None:
    plan = select_voice_executor(_config(voice_executor="cascade"), _persona())
    assert plan.kind is ExecutorKind.CASCADE
    assert plan.is_realtime is False
    assert plan.realtime_model is None
    assert plan.fallback_reason is None


def test_cascade_selection_never_touches_realtime(monkeypatch: pytest.MonkeyPatch) -> None:
    # Even with a valid realtime config available, an explicit cascade
    # selection must not build the realtime model.
    _install_fake_openai_plugin(monkeypatch)
    built: list[bool] = []

    def _spy(*_a: object, **_k: object) -> object:
        built.append(True)
        return _FakeRealtimeModel()

    monkeypatch.setattr("voice_agent.realtime_executor.build_realtime_executor", _spy)
    plan = select_voice_executor(_config(voice_executor="cascade"), _persona())
    assert plan.kind is ExecutorKind.CASCADE
    assert built == []


# --- selection: realtime happy path ---


def test_realtime_selection_builds_model(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_openai_plugin(monkeypatch)
    plan = select_voice_executor(_config(voice_executor="realtime"), _persona())
    assert plan.kind is ExecutorKind.REALTIME
    assert plan.is_realtime is True
    assert isinstance(plan.realtime_model, _FakeRealtimeModel)
    assert plan.fallback_reason is None


def test_realtime_model_built_in_conductor_gate_posture(monkeypatch: pytest.MonkeyPatch) -> None:
    # #432 option A: turn_detection must be None so the model never
    # self-triggers a response.
    _install_fake_openai_plugin(monkeypatch)
    model = build_realtime_executor(_config(voice_executor="realtime"), _persona())
    assert isinstance(model, _FakeRealtimeModel)
    assert model.kwargs["turn_detection"] is None
    assert model.kwargs["model"] == "gpt-realtime"
    assert model.kwargs["api_key"] == "sk-test"


def test_realtime_model_carries_persona_instructions_and_voice(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # #436: the static persona (instructions + gpt-realtime voice) must be
    # wired onto the model at build time. The default _persona() has no
    # role/style, so the neutral persona renders -- non-empty instructions
    # and a valid voice still land.
    _install_fake_openai_plugin(monkeypatch)
    model = build_realtime_executor(_config(voice_executor="realtime"), _persona())
    assert isinstance(model, _FakeRealtimeModel)
    instructions = model.kwargs["instructions"]
    assert isinstance(instructions, str) and instructions.strip()
    # alto -> the female realtime voice (see OPENAI_REALTIME_VOICES).
    assert model.kwargs["voice"] == "marin"


# --- selection: realtime falls back to cascade on every failure mode ---


def test_realtime_without_api_key_falls_back_to_cascade(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_openai_plugin(monkeypatch)
    plan = select_voice_executor(
        _config(voice_executor="realtime", openai_api_key=None), _persona()
    )
    assert plan.kind is ExecutorKind.CASCADE
    assert plan.realtime_model is None
    assert plan.fallback_reason is not None
    assert "OPENAI_API_KEY" in plan.fallback_reason


def test_realtime_plugin_import_failure_falls_back_to_cascade(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # Ensure the openai plugin is NOT importable, then assert fallback.
    monkeypatch.setitem(sys.modules, "livekit.plugins.openai", None)
    plan = select_voice_executor(_config(voice_executor="realtime"), _persona())
    assert plan.kind is ExecutorKind.CASCADE
    assert plan.fallback_reason is not None
    assert "openai" in plan.fallback_reason.lower()


def test_realtime_model_construction_error_falls_back_to_cascade(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _ExplodingModel:
        def __init__(self, **_kwargs: object) -> None:
            raise ValueError("boom")

    _install_fake_openai_plugin(monkeypatch, model_cls=_ExplodingModel)
    plan = select_voice_executor(_config(voice_executor="realtime"), _persona())
    assert plan.kind is ExecutorKind.CASCADE
    assert plan.fallback_reason is not None
    assert "construction failed" in plan.fallback_reason.lower()


def test_build_realtime_executor_missing_key_raises() -> None:
    with pytest.raises(RealtimeExecutorError, match="OPENAI_API_KEY"):
        build_realtime_executor(
            _config(voice_executor="realtime", openai_api_key=None), _persona()
        )


# --- plan shape ---


def test_plan_is_realtime_property() -> None:
    assert VoiceExecutorPlan(kind=ExecutorKind.REALTIME).is_realtime is True
    assert VoiceExecutorPlan(kind=ExecutorKind.CASCADE).is_realtime is False
