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
        # memql#342 self-bootstrap path env vars; default-clear so
        # legacy tests don't accidentally trigger the bootstrap call.
        "MEMQL_NODE_BOOTSTRAP_TOKEN", "IDENTITY_VERIFIER_BASE_URL",
        "MEMQL_VOICE_AGENT_INSTANCE_ID",
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


# memql#342 -- voice-agent self-bootstrap path tests.


def _seed_required_without_token(monkeypatch: pytest.MonkeyPatch) -> None:
    """Seed every required env var EXCEPT VOICE_AGENT_TOKEN, then
    let the test choose whether to set it directly or trigger
    self-bootstrap via MEMQL_NODE_BOOTSTRAP_TOKEN."""
    monkeypatch.setenv("LIVEKIT_URL", "ws://livekit:7880")
    monkeypatch.setenv("LIVEKIT_API_KEY", "devkey")
    monkeypatch.setenv("LIVEKIT_API_SECRET", "secret")
    monkeypatch.setenv("MEMQL_DEEPGRAM_API_KEY", "dg-key")
    monkeypatch.setenv("MEMQL_GRPC_ADDR", "bff:50051")


def test_bootstrap_not_triggered_when_token_provided(monkeypatch: pytest.MonkeyPatch) -> None:
    """Operator-provisioned VOICE_AGENT_TOKEN wins -- the bootstrap
    code path is NEVER consulted when the token is set. Pins the
    'production path stays untouched' invariant."""
    _seed_required_without_token(monkeypatch)
    monkeypatch.setenv("VOICE_AGENT_TOKEN", "operator-provisioned-jwt")
    # Even with bootstrap secret + URL set, the direct token wins.
    monkeypatch.setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
    monkeypatch.setenv("IDENTITY_VERIFIER_BASE_URL", "http://identity:8081")

    cfg = load_config()
    assert cfg.voice_agent_token == "operator-provisioned-jwt"


def test_bootstrap_skipped_when_secret_unset(monkeypatch: pytest.MonkeyPatch) -> None:
    """When VOICE_AGENT_TOKEN is empty AND MEMQL_NODE_BOOTSTRAP_TOKEN
    is empty, the self-bootstrap path is opted out -- load_config
    must raise the canonical 'token unset' error so production deploys
    that leave the bootstrap secret unset see a clear diagnostic."""
    _seed_required_without_token(monkeypatch)
    with pytest.raises(RuntimeError, match="VOICE_AGENT_TOKEN"):
        load_config()


def test_bootstrap_requires_identity_url(monkeypatch: pytest.MonkeyPatch) -> None:
    """Self-bootstrap requires IDENTITY_VERIFIER_BASE_URL -- without
    it, we have no endpoint to POST to. Missing-URL raises a
    'cannot reach identity service' error pointing the operator at
    the misconfiguration."""
    _seed_required_without_token(monkeypatch)
    monkeypatch.setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
    monkeypatch.setenv("MEMQL_VOICE_AGENT_INSTANCE_ID", "voice-agent-local")
    # IDENTITY_VERIFIER_BASE_URL deliberately left unset.

    with pytest.raises(RuntimeError, match="IDENTITY_VERIFIER_BASE_URL"):
        load_config()


def test_bootstrap_requires_instance_id(monkeypatch: pytest.MonkeyPatch) -> None:
    """Self-bootstrap requires MEMQL_VOICE_AGENT_INSTANCE_ID so the
    JWT carries an operator-chosen audit label. Missing it raises a
    pointed error rather than silently stamping a random / empty id."""
    _seed_required_without_token(monkeypatch)
    monkeypatch.setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
    monkeypatch.setenv("IDENTITY_VERIFIER_BASE_URL", "http://identity:8081")
    # MEMQL_VOICE_AGENT_INSTANCE_ID deliberately left unset.

    with pytest.raises(RuntimeError, match="MEMQL_VOICE_AGENT_INSTANCE_ID"):
        load_config()


def test_bootstrap_posts_correct_request(monkeypatch: pytest.MonkeyPatch) -> None:
    """When all preconditions are present, self-bootstrap POSTs to
    /node/bootstrap with tokenClass='voice_agent' + instanceId, and
    presents the bootstrap secret via the Authorization: Bootstrap
    header. The minted plainToken lands on Config.voice_agent_token."""
    import json
    import urllib.request

    _seed_required_without_token(monkeypatch)
    monkeypatch.setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "shared-bootstrap-secret")
    monkeypatch.setenv("IDENTITY_VERIFIER_BASE_URL", "http://identity:8081/")  # trailing slash gets normalised
    monkeypatch.setenv("MEMQL_VOICE_AGENT_INSTANCE_ID", "voice-agent-test")

    captured: dict = {}

    class _FakeResp:
        status = 200

        def __enter__(self) -> "_FakeResp":
            return self

        def __exit__(self, *_: object) -> None:
            return None

        def read(self) -> bytes:
            return json.dumps({
                "success": True,
                "plainToken": "minted-voice-agent-jwt",
                "identityId": "v1:identity:identity:voice_agent:voice-agent-test",
                "nodeId": "voice-agent-test",
                "expiresAt": "2027-01-01T00:00:00Z",
            }).encode()

    def fake_urlopen(req: urllib.request.Request, timeout: int) -> _FakeResp:
        captured["url"] = req.full_url
        captured["headers"] = dict(req.header_items())
        captured["body"] = json.loads(req.data.decode())
        captured["timeout"] = timeout
        return _FakeResp()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    cfg = load_config()
    assert cfg.voice_agent_token == "minted-voice-agent-jwt"

    # URL has the trailing slash stripped + /node/bootstrap appended.
    assert captured["url"] == "http://identity:8081/node/bootstrap"
    # Bootstrap secret presented per the endpoint contract.
    headers = {k.lower(): v for k, v in captured["headers"].items()}
    assert headers["authorization"] == "Bootstrap shared-bootstrap-secret"
    assert headers["content-type"] == "application/json"
    # Body asks for the voice_agent class + carries the instance id.
    assert captured["body"] == {
        "tokenClass": "voice_agent",
        "instanceId": "voice-agent-test",
    }


def test_bootstrap_surfaces_identity_error(monkeypatch: pytest.MonkeyPatch) -> None:
    """When identity rejects the request (e.g. wrong secret), the
    structured errorCode is surfaced in the RuntimeError message so
    the operator can distinguish 'wrong secret' from 'endpoint
    disabled' / 'bad request' without grepping identity logs."""
    import json
    import urllib.request

    _seed_required_without_token(monkeypatch)
    monkeypatch.setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "wrong-secret")
    monkeypatch.setenv("IDENTITY_VERIFIER_BASE_URL", "http://identity:8081")
    monkeypatch.setenv("MEMQL_VOICE_AGENT_INSTANCE_ID", "voice-agent-test")

    class _FakeResp:
        status = 401

        def __enter__(self) -> "_FakeResp":
            return self

        def __exit__(self, *_: object) -> None:
            return None

        def read(self) -> bytes:
            return json.dumps({
                "success": False,
                "error": "invalid bootstrap secret",
                "errorCode": "invalid_bootstrap_secret",
            }).encode()

    def fake_urlopen(*_args: object, **_kwargs: object) -> _FakeResp:
        return _FakeResp()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    with pytest.raises(RuntimeError, match="invalid_bootstrap_secret"):
        load_config()
