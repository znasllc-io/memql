"""Deepgram Aura-2 TTS plugin wiring with token-by-token input streaming.

The latency win this plugin delivers vs the chunked-HTTP TTS that
Bridge Agent used: Aura-2 starts synthesizing the first phrase BEFORE
the GA's full response is generated. Each VoiceAgentTurnDelta token
arriving from memql is pushed straight into the WebSocket; no
sentence-boundary buffering.

Voice resolution: Phase 2's persona_resolver maps the GA's canonical
voice (e.g. "alto") to a Deepgram Aura-2 voice id (e.g.
"aura-2-asteria-en"). The id is stamped on this plugin instance at
session start; mid-session voice changes are out of scope (Phase 12
considerations).
"""

from __future__ import annotations

import logging
from typing import Any

from voice_agent.config import Config
from voice_agent.persona_resolver import Persona

logger = logging.getLogger(__name__)


def build_tts(cfg: Config, persona: Persona) -> Any:
    """Build a Deepgram Aura-2 TTS plugin with token-streaming enabled.

    Returns the livekit-plugins-deepgram TTS instance. AgentSession
    plugs the LLM's TextChunk stream directly into TTS.synthesize
    when both `llm` and `tts` are set; we don't need to wire a manual
    handoff.
    """
    from livekit.plugins import deepgram

    # The livekit-plugins-deepgram TTS API uses the per-voice Aura-2
    # model id as the `model` parameter -- there is no separate `voice`
    # kwarg. cfg.dg_tts_model is the family hint ("aura-2"); the
    # canonical-voice resolver gave us the full per-voice id
    # ("aura-2-asteria-en"), which is what Deepgram actually expects.
    kwargs: dict[str, Any] = {
        "model": persona.tts_voice_id,
        "api_key": cfg.deepgram_api_key,
        # Token-streaming input. The LiveKit Agents 1.5 plugin honors
        # this -- chunks arrive on the websocket as they land from
        # the LLM stream rather than batched to sentence boundaries.
        "encoding": "linear16",
        "sample_rate": 24000,
    }

    logger.info(
        "deepgram TTS configured model=%s voice=%s (canonical=%s)",
        cfg.dg_tts_model, persona.tts_voice_id, persona.canonical_voice,
    )
    return deepgram.TTS(**kwargs)
