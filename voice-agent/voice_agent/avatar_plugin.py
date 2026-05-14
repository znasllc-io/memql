"""Anam (or Simli) avatar plugin wiring.

The avatar plugin subscribes to TTS audio output and publishes a
lip-synced video track into the LiveKit room as another participant.
The vendor is decoupled from the runtime knob (MEMQL_AVATAR_VENDOR)
so a persona minted against Anam still rides when the runtime defaults
to Simli -- we honour whatever vendor minted the persona.

Vendor selection (Phase 9 of Initiative C):
- `MEMQL_AVATAR_VENDOR` at process start picks the default for new
  personas. The voice-agent never hardcodes a vendor in code; this
  module dispatches by name.
- The persona-resolver query returns the agent's `avatarPersonaId`
  + `avatarVendor`. If `avatarVendor` is set we use that; otherwise
  we fall back to `MEMQL_AVATAR_VENDOR`.

Gating (Phase 7):
- If the effective video mode is 'always_off' the avatar plugin is
  not instantiated -- saves cost. The voice-agent's main flow reads
  the gate and calls `build_avatar(...)` only when video is on.
"""

from __future__ import annotations

import logging
from typing import Any

from voice_agent.config import Config
from voice_agent.persona_resolver import Persona

logger = logging.getLogger(__name__)


def vendor_for_persona(persona: Persona, runtime_vendor: str) -> str:
    """Pick the vendor that should drive THIS persona.

    Honours the vendor stamped at persona-mint time (so personas don't
    silently get re-rendered against a different vendor's model). Falls
    through to the runtime default when the persona record has no
    vendor stamped -- usually means the persona was created when
    avatarVendor was still a future field.
    """
    if getattr(persona, "avatar_vendor", "") in ("anam", "simli"):
        return persona.avatar_vendor  # type: ignore[attr-defined]
    return runtime_vendor


def build_avatar(cfg: Config, persona: Persona) -> Any | None:
    """Construct the avatar plugin instance.

    Returns None when the configured vendor is 'none' or when no
    persona id is available (neither stamped on the agent record nor
    set as a platform-wide default). The voice-agent falls back to
    audio-only in that case. Raises on a vendor-config mismatch
    (e.g., avatarVendor=anam but ANAM_API_KEY unset) so we don't
    silently publish nothing.
    """
    if cfg.avatar_vendor == "none":
        logger.info("avatar disabled -- MEMQL_AVATAR_VENDOR=none")
        return None

    vendor = vendor_for_persona(persona, cfg.avatar_vendor)
    # Per-agent stamped persona wins. When unset, fall back to the
    # platform default for the runtime vendor. Anam has two default
    # paths: a personaId (preferred -- bundles face + voice + LLM
    # + system prompt) or a bare avatarId (just the face model). The
    # agent record only stores one slug; the platform-default split
    # is on the env side. This is the bridge until the agent-edit
    # upload-still->mint-persona pipeline ships.
    if vendor == "anam":
        return _build_anam(cfg, persona)
    if vendor == "simli":
        persona_id = persona.avatar_persona_id or ""
        if not persona_id:
            logger.info(
                "avatar disabled -- simli vendor needs an explicit face_id on the agent record"
            )
            return None
        return _build_simli(cfg, persona_id)
    raise RuntimeError(f"unknown avatar vendor {vendor!r}")


def _build_anam(cfg: Config, persona: Persona) -> Any | None:
    """Build the Anam avatar session for this persona.

    Two Anam paths, chosen by which id is available:
    1. **personaId** (preferred) -- references a fully-configured Anam
       persona (face + voice + LLM + system prompt bundled). Uses
       `AnamPersonaSession` (our wrapper) since the stock LK plugin
       doesn't expose the personaId payload shape.
    2. **avatarId** -- bare face-model id. Uses the stock
       `anam.AvatarSession` with an ephemeral PersonaConfig.

    Per-agent stamped persona wins over the platform default. The
    agent record's `avatarPersonaId` is treated as a personaId if
    `avatarVendor=anam` was minted that way -- a future iteration
    can split the field if we need to disambiguate per-agent which
    Anam entity kind it is.
    """
    if not cfg.anam_api_key:
        raise RuntimeError("ANAM_API_KEY required when avatarVendor=anam")
    from livekit.plugins import anam  # type: ignore

    # 1. Per-agent stamped persona id (treated as personaId).
    if persona.avatar_persona_id:
        from voice_agent.anam_persona_session import AnamPersonaSession

        logger.info(
            "anam avatar configured (per-agent personaId) persona_id=%s",
            persona.avatar_persona_id,
        )
        return AnamPersonaSession(
            api_key=cfg.anam_api_key,
            persona_id=persona.avatar_persona_id,
        )

    # 2. Platform default: personaId path preferred.
    if cfg.anam_default_persona_id:
        from voice_agent.anam_persona_session import AnamPersonaSession

        logger.info(
            "anam avatar configured (default personaId) persona_id=%s",
            cfg.anam_default_persona_id,
        )
        return AnamPersonaSession(
            api_key=cfg.anam_api_key,
            persona_id=cfg.anam_default_persona_id,
        )

    # 3. Platform default: bare avatarId via ephemeral PersonaConfig.
    if cfg.anam_default_avatar_id:
        logger.info(
            "anam avatar configured (default avatarId) persona_name=%s avatar_id=%s",
            cfg.anam_default_persona_name, cfg.anam_default_avatar_id,
        )
        return anam.AvatarSession(
            api_key=cfg.anam_api_key,
            persona_config=anam.PersonaConfig(
                name=cfg.anam_default_persona_name,
                avatarId=cfg.anam_default_avatar_id,
            ),
        )

    logger.info(
        "avatar disabled -- no persona id on agent and no platform default "
        "(set ANAM_DEFAULT_PERSONA_ID or ANAM_DEFAULT_AVATAR_ID to enable)"
    )
    return None


def _build_simli(cfg: Config, persona_id: str) -> Any:
    if not cfg.simli_api_key:
        raise RuntimeError("SIMLI_API_KEY required when avatarVendor=simli")
    from livekit.plugins import simli  # type: ignore

    logger.info(
        "simli avatar configured face_id=%s",
        persona_id,
    )
    return simli.Avatar(
        api_key=cfg.simli_api_key,
        face_id=persona_id,
    )
