"""Config-loading sanity tests."""

from __future__ import annotations

import os

import pytest

from voice_agent.config import load_config


@pytest.fixture(autouse=True)
def _clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for var in (
        "LIVEKIT_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET",
        "MEMQL_DEEPGRAM_API_KEY", "MEMQL_GRPC_ADDR",
        "VOICE_AGENT_TOKEN", "MEMQL_AVATAR_VENDOR",
        "ANAM_API_KEY", "SIMLI_API_KEY",
        "ANAM_DEFAULT_AVATAR_ID", "ANAM_DEFAULT_PERSONA_NAME",
        "ANAM_DEFAULT_PERSONA_ID",
    ):
        monkeypatch.delenv(var, raising=False)


def _seed_required(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LIVEKIT_URL", "ws://livekit:7880")
    monkeypatch.setenv("LIVEKIT_API_KEY", "devkey")
    monkeypatch.setenv("LIVEKIT_API_SECRET", "secret")
    monkeypatch.setenv("MEMQL_DEEPGRAM_API_KEY", "dg-key")
    monkeypatch.setenv("MEMQL_GRPC_ADDR", "bff:50051")
    monkeypatch.setenv("VOICE_AGENT_TOKEN", "shared-token")


def test_load_config_defaults(monkeypatch: pytest.MonkeyPatch) -> None:
    _seed_required(monkeypatch)
    cfg = load_config()
    assert cfg.avatar_vendor == "anam"
    assert cfg.log_level == "INFO"
    assert cfg.dg_asr_model == "nova-3"
    assert cfg.dg_tts_model == "aura-2"
    assert cfg.dg_endpointing_ms == 2000
    # utterance_end_ms defaults to 0 because the LK Deepgram plugin
    # doesn't forward it to the Deepgram WebSocket; the env var is
    # preserved on Config for the day we patch the plugin or migrate
    # to a raw Deepgram client. See docs/voice/eou-tuning.md.
    assert cfg.dg_utterance_end_ms == 0


def test_load_config_avatar_vendor_validation(monkeypatch: pytest.MonkeyPatch) -> None:
    _seed_required(monkeypatch)
    monkeypatch.setenv("MEMQL_AVATAR_VENDOR", "bogus")
    with pytest.raises(RuntimeError, match="MEMQL_AVATAR_VENDOR"):
        load_config()


def test_load_config_missing_required(monkeypatch: pytest.MonkeyPatch) -> None:
    _seed_required(monkeypatch)
    monkeypatch.delenv("MEMQL_DEEPGRAM_API_KEY")
    with pytest.raises(RuntimeError, match="MEMQL_DEEPGRAM_API_KEY"):
        load_config()


def test_avatar_disabled_when_none(monkeypatch: pytest.MonkeyPatch) -> None:
    _seed_required(monkeypatch)
    monkeypatch.setenv("MEMQL_AVATAR_VENDOR", "none")
    cfg = load_config()
    assert cfg.avatar_enabled is False


def test_anam_default_persona_fallback(monkeypatch: pytest.MonkeyPatch) -> None:
    _seed_required(monkeypatch)
    monkeypatch.setenv("ANAM_DEFAULT_AVATAR_ID", "av-123abc")
    monkeypatch.setenv("ANAM_DEFAULT_PERSONA_NAME", "Sofia")
    cfg = load_config()
    assert cfg.anam_default_avatar_id == "av-123abc"
    assert cfg.anam_default_persona_name == "Sofia"


def test_anam_default_persona_unset(monkeypatch: pytest.MonkeyPatch) -> None:
    _seed_required(monkeypatch)
    cfg = load_config()
    assert cfg.anam_default_avatar_id is None
    assert cfg.anam_default_persona_id is None
    assert cfg.anam_default_persona_name == "Assistant"


def test_anam_default_persona_id(monkeypatch: pytest.MonkeyPatch) -> None:
    _seed_required(monkeypatch)
    monkeypatch.setenv("ANAM_DEFAULT_PERSONA_ID", "25f351b9-7658-4f40-b81f-f98c97fb3f78")
    cfg = load_config()
    assert cfg.anam_default_persona_id == "25f351b9-7658-4f40-b81f-f98c97fb3f78"
