"""STT plugin parameter mapping tests.

We can't import livekit-plugins-deepgram in the unit test environment
(it'd pull a network connection on STT() construction), so we patch
`livekit.plugins.deepgram` with a stub class and assert build_stt
hands it the right kwargs.
"""

from __future__ import annotations

import sys
import types

import pytest

from voice_agent.config import Config


@pytest.fixture
def stub_cfg() -> Config:
    return Config(
        livekit_url="ws://livekit:7880",
        livekit_api_key="devkey",
        livekit_api_secret="secret",
        deepgram_api_key="dg-key",
        memql_grpc_addr="bff:50051",
        voice_agent_shared_token="t",
        avatar_vendor="anam",
        anam_api_key=None,
        simli_api_key=None,
        log_level="INFO",
        dg_asr_model="nova-3",
        dg_tts_model="aura-2",
        dg_language="en-US",
        dg_endpointing_ms=500,
        dg_utterance_end_ms=1000,
    )


def _install_stub() -> dict:
    """Inject a fake livekit.plugins.deepgram module that records kwargs."""
    captured: dict = {}

    class _STT:
        def __init__(self, **kwargs: object) -> None:
            captured.update(kwargs)

    livekit_pkg = types.ModuleType("livekit")
    livekit_plugins_pkg = types.ModuleType("livekit.plugins")
    deepgram_mod = types.ModuleType("livekit.plugins.deepgram")
    deepgram_mod.STT = _STT  # type: ignore[attr-defined]
    livekit_plugins_pkg.deepgram = deepgram_mod  # type: ignore[attr-defined]

    sys.modules["livekit"] = livekit_pkg
    sys.modules["livekit.plugins"] = livekit_plugins_pkg
    sys.modules["livekit.plugins.deepgram"] = deepgram_mod
    return captured


def test_build_stt_passes_production_params(stub_cfg: Config) -> None:
    captured = _install_stub()
    from voice_agent.stt_plugin import build_stt

    build_stt(stub_cfg)
    assert captured["model"] == "nova-3"
    assert captured["api_key"] == "dg-key"
    assert captured["language"] == "en-US"
    assert captured["interim_results"] is True
    assert captured["endpointing_ms"] == 500
    # utterance_end_ms is intentionally NOT forwarded: the LK Agents 1.5
    # Deepgram STT plugin doesn't accept the kwarg (passing it raises
    # TypeError). The env-only knob is preserved on Config for the day
    # the plugin gets patched or we migrate to a raw Deepgram client.
    # See docs/voice/eou-tuning.md.
    assert "utterance_end_ms" not in captured
