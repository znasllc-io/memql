"""Per-session persona lookup against memql.

Resolves a (space_id, ga_agent_id) into the runtime configuration the
LiveKit plugins need:

- canonical voice (alto / soprano / tenor / ...) -> Deepgram Aura-2 voice id
- avatar persona id (Anam or Simli)
- initial audio / video gate (always_on / always_off / mirror_user)

The lookup is one gRPC round-trip via VoiceAgentSessionStart at session
start; the resulting Persona is held for the duration of the LiveKit
room. State changes mid-session (e.g. the user flipping the GA mic
toggle) ride a separate subscription on v1:cognition:audiooverride /
v1:cognition:videooverride.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass

from voice_agent.grpc_client import MemqlGrpcClient

logger = logging.getLogger(__name__)


# Provider voice catalog. Mirrors integrations/voice/voices.go on the
# Go side; kept local here so persona_resolver doesn't need a network
# call to translate canonical -> provider id at every TTS instantiation.
# Phase 6 can swap this for a fetch from memql if drift becomes a
# real problem -- today the catalog is small and stable.
DEEPGRAM_AURA2_VOICES = {
    # Female voices
    "alto":    "aura-2-asteria-en",
    "soprano": "aura-2-luna-en",
    "mezzo":   "aura-2-stella-en",
    # Male voices
    "tenor":    "aura-2-orion-en",
    "baritone": "aura-2-arcas-en",
    "bass":     "aura-2-perseus-en",
}


@dataclass(frozen=True)
class Persona:
    canonical_voice: str
    tts_voice_id: str
    avatar_persona_id: str | None
    avatar_vendor: str  # 'anam' | 'simli' | '' (unstamped legacy persona)
    initial_audio_mode: str
    initial_video_mode: str


def _resolve_tts_voice(canonical: str) -> str:
    voice_id = DEEPGRAM_AURA2_VOICES.get(canonical.lower(), "")
    if not voice_id:
        logger.warning(
            "unknown canonical voice %r -- falling back to alto", canonical
        )
        return DEEPGRAM_AURA2_VOICES["alto"]
    return voice_id


async def resolve_persona(
    client: MemqlGrpcClient,
    space_id: str,
    ga_agent_id: str,
    room_name: str,
    avatar_vendor: str,
) -> Persona:
    """Open the voice-agent session and resolve runtime persona config."""
    from voice_agent.proto import memql_pb2  # type: ignore

    payload = memql_pb2.VoiceAgentSessionStart(
        space_id=space_id,
        ga_agent_id=ga_agent_id,
        room_name=room_name,
        avatar_vendor=avatar_vendor,
    )
    reply = await client.send_request("voice_agent_session_start", payload)
    ack = reply.voice_agent_session_ack
    if not ack.success:
        raise RuntimeError(
            f"voice agent session start failed: "
            f"{ack.error_code} {ack.error_message}"
        )

    canonical = ack.ga_canonical_voice or "alto"
    # avatar_vendor is not on VoiceAgentSessionAck today -- a Phase 11
    # follow-up will stamp it when the agent record's avatarVendor
    # field is populated. Until then we trust the runtime vendor
    # (passed in by the caller) to drive plugin selection.
    return Persona(
        canonical_voice=canonical,
        tts_voice_id=_resolve_tts_voice(canonical),
        avatar_persona_id=ack.ga_avatar_persona_id or None,
        avatar_vendor=avatar_vendor,
        initial_audio_mode=ack.initial_audio_mode or "mirror_user",
        initial_video_mode=ack.initial_video_mode or "mirror_user",
    )
