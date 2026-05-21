"""Centralized env-var loading for the voice-agent process."""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class Config:
    """Resolved runtime configuration. Read once at startup."""

    # LiveKit
    livekit_url: str
    livekit_api_key: str
    livekit_api_secret: str

    # Deepgram (STT + TTS)
    deepgram_api_key: str

    # memql gRPC
    memql_grpc_addr: str
    # Identity-issued class="voice_agent" JWT bearer presented on
    # every MemqlService.Stream dial. Minted via
    # JWTIssuer.IssueVoiceAgentAccessToken on the cluster side; see
    # docs/auth/voice-agent-jwt.md.
    voice_agent_token: str

    # Avatar
    avatar_vendor: str  # 'anam' | 'simli' | 'none'
    anam_api_key: str | None
    simli_api_key: str | None
    # Default Anam face/persona to use when the agent record has no
    # `avatarPersonaId` stamped. Anam treats personas and avatars as
    # distinct entity types -- a persona bundles (face + voice + LLM
    # + system prompt) under one id; an avatar is just the face
    # model. Both fields are accepted; the persona-id path takes
    # precedence when both are set since a persona supplies more
    # context.
    anam_default_persona_id: str | None    # full Anam persona
    anam_default_avatar_id: str | None     # bare avatar face id
    anam_default_persona_name: str         # display name when using avatar-id path

    # Logging
    log_level: str

    # Deepgram tuning -- inherited from the shipped Bridge Agent's
    # production-tuned defaults so the LiveKit Agents path picks up
    # the same EOU semantics rather than the upstream defaults.
    dg_asr_model: str
    dg_tts_model: str
    dg_language: str
    dg_endpointing_ms: int
    dg_utterance_end_ms: int

    @property
    def avatar_enabled(self) -> bool:
        return self.avatar_vendor in ("anam", "simli")


def _get(env: str, default: str = "") -> str:
    return os.environ.get(env, default).strip()


def _get_required(env: str) -> str:
    val = os.environ.get(env, "").strip()
    if not val:
        raise RuntimeError(f"required env var {env} is unset")
    return val


def _get_int(env: str, default: int) -> int:
    raw = os.environ.get(env, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        logger.warning("env %s=%r is not an int, falling back to %d", env, raw, default)
        return default


def load_config() -> Config:
    """Load + validate config. Raises if required vars are missing."""
    cfg = Config(
        livekit_url=_get_required("LIVEKIT_URL"),
        livekit_api_key=_get_required("LIVEKIT_API_KEY"),
        livekit_api_secret=_get_required("LIVEKIT_API_SECRET"),
        deepgram_api_key=_get_required("MEMQL_DEEPGRAM_API_KEY"),
        memql_grpc_addr=_get_required("MEMQL_GRPC_ADDR"),
        voice_agent_token=_get_required("VOICE_AGENT_TOKEN"),
        avatar_vendor=_get("MEMQL_AVATAR_VENDOR", "anam").lower(),
        anam_api_key=_get("ANAM_API_KEY") or None,
        simli_api_key=_get("SIMLI_API_KEY") or None,
        anam_default_persona_id=_get("ANAM_DEFAULT_PERSONA_ID") or None,
        anam_default_avatar_id=_get("ANAM_DEFAULT_AVATAR_ID") or None,
        anam_default_persona_name=_get("ANAM_DEFAULT_PERSONA_NAME", "Assistant"),
        log_level=_get("VOICE_AGENT_LOG_LEVEL", "INFO").upper(),
        dg_asr_model=_get("POLYPHON_DEEPGRAM_ASR_MODEL", "nova-3"),
        dg_tts_model=_get("POLYPHON_DEEPGRAM_TTS_MODEL", "aura-2"),
        dg_language=_get("POLYPHON_DEEPGRAM_LANGUAGE", "en"),
        # Default intentionally errs on "let the user think." Voice
        # users pause mid-sentence to gather a thought; firing a final
        # transcript on a 500ms thinking gap (the old baseline) makes
        # the agent respond to a fragment and the user has to start
        # the sentence over. 1500ms of phrase-level silence gives
        # natural conversational pauses room to ride through; snappier
        # users can dial back via env. NB: the LK Deepgram plugin
        # (1.5) does not forward `utterance_end_ms` to the Deepgram
        # WebSocket -- the field below is parsed and logged for
        # future use (patching the plugin, or switching to a raw
        # Deepgram client) but is NOT honored on the wire today.
        # See docs/voice/eou-tuning.md.
        dg_endpointing_ms=_get_int("POLYPHON_DEEPGRAM_ENDPOINTING_MS", 2000),
        dg_utterance_end_ms=_get_int("POLYPHON_DEEPGRAM_UTTERANCE_END_MS", 0),
    )

    if cfg.avatar_vendor not in ("anam", "simli", "none"):
        raise RuntimeError(
            f"MEMQL_AVATAR_VENDOR={cfg.avatar_vendor!r} -- must be 'anam', 'simli', or 'none'"
        )
    if cfg.avatar_vendor == "anam" and not cfg.anam_api_key:
        logger.warning(
            "MEMQL_AVATAR_VENDOR=anam but ANAM_API_KEY is unset -- avatar will be disabled"
        )
    if cfg.avatar_vendor == "simli" and not cfg.simli_api_key:
        logger.warning(
            "MEMQL_AVATAR_VENDOR=simli but SIMLI_API_KEY is unset -- avatar will be disabled"
        )

    return cfg


def setup_logging(level: str) -> None:
    """Configure structured logging. Called once at startup."""
    logging.basicConfig(
        level=getattr(logging, level, logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
    )
