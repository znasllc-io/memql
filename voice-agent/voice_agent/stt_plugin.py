"""Deepgram Nova-3 STT plugin wiring.

Ports the production-tuned parameters from the Go Bridge Agent
(`integrations/deepgram/deepgram.go`) so the LiveKit Agents path
inherits the same EOU semantics rather than picking up
livekit-plugins-deepgram defaults.

Per-utterance flow:

  1. AgentSession's microphone subscription pumps user audio into
     the Deepgram WebSocket.
  2. The plugin emits `transcript_event` deltas (interim + final).
  3. `attach_transcript_forwarding` subscribes to those deltas and
     forwards each through TranscriptForwarder:
       - interim   -> VoiceAgentPartialTranscript  (event-only on memql)
       - final     -> VoiceAgentFinalTranscript    (chat row insert)
  4. The final's reply id is what triggers VoiceAgentTurnRequest in
     the memql LLM custom plugin (Phase 4).
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any

from voice_agent.config import Config
from voice_agent.transcript_forwarder import TranscriptForwarder

logger = logging.getLogger(__name__)


def build_stt(cfg: Config) -> Any:
    """Build a Deepgram Nova-3 STT plugin instance.

    `endpointing_ms` is the only EOU knob the LiveKit Deepgram
    plugin (livekit-plugins-deepgram 1.5) exposes. The Deepgram
    WebSocket API also accepts `utterance_end_ms` (a hard silence
    floor for the UtteranceEnd event), but the LK plugin doesn't
    forward it to the connection -- passing it raises
    `TypeError: STT.__init__() got an unexpected keyword argument
    'utterance_end_ms'`. We dropped the kwarg here and rely
    entirely on endpointing_ms; the env-only knob is preserved on
    Config for the day we either patch the plugin or migrate to a
    raw Deepgram client and want a hard EOU floor back.
    """
    # Lazy import so the rest of voice_agent imports without the
    # livekit-plugins-deepgram package being installed (Phase 2 test
    # suite runs config tests without livekit).
    from livekit.plugins import deepgram

    kwargs: dict[str, Any] = {
        "model": cfg.dg_asr_model,
        "api_key": cfg.deepgram_api_key,
        "language": cfg.dg_language,
        "interim_results": True,
        "endpointing_ms": cfg.dg_endpointing_ms,
    }

    logger.info(
        "deepgram STT configured model=%s lang=%s endpointing=%dms "
        "(utterance_end_ms=%d unused -- plugin does not forward it)",
        cfg.dg_asr_model, cfg.dg_language,
        cfg.dg_endpointing_ms, cfg.dg_utterance_end_ms,
    )
    return deepgram.STT(**kwargs)


async def attach_transcript_forwarding(
    session: Any,
    forwarder: TranscriptForwarder,
    speaker_user_id_provider: Any,
) -> asyncio.Task[None]:
    """Wire LiveKit Agents transcript events to TranscriptForwarder.

    `session` is the AgentSession; `speaker_user_id_provider` is a
    callable returning the current speaker's v1:identity:user.id.
    memql trusts whatever the voice-agent stamps here because the
    service-account interceptor has already verified the caller.

    Returns the background task; cancel it at session teardown.
    """

    def _on_user_input_transcribed(event: Any) -> None:
        # livekit.agents.voice.UserInputTranscribedEvent surfaces
        # both interim and final transcripts on the same event,
        # discriminated by the is_final flag.
        text = (getattr(event, "transcript", "") or "").strip()
        if not text:
            return
        is_final = bool(getattr(event, "is_final", False))
        speaker = _sync_speaker_user_id(speaker_user_id_provider)
        logger.info(
            "user_input_transcribed final=%s speaker=%s len=%d",
            is_final, speaker, len(text),
        )
        if is_final:
            asyncio.create_task(_forward_final(forwarder, speaker, text))
        else:
            asyncio.create_task(_forward_partial(forwarder, speaker, text))

    if session is not None:
        try:
            session.on("user_input_transcribed", _on_user_input_transcribed)
            logger.info("bound transcript forwarder to user_input_transcribed")
        except Exception as err:  # noqa: BLE001
            logger.error("failed to bind user_input_transcribed: %s", err)

    async def _idle() -> None:
        # Returned task -- holds the binding alive for the session.
        # Cancelled by the entrypoint's finally block.
        while True:
            await asyncio.sleep(3600)

    return asyncio.create_task(_idle())


async def _forward_partial(forwarder: TranscriptForwarder, speaker: str, text: str) -> None:
    try:
        await forwarder.forward_partial(speaker, text)
    except Exception:
        logger.exception("forward_partial failed")


async def _forward_final(forwarder: TranscriptForwarder, speaker: str, text: str) -> None:
    try:
        await forwarder.forward_final(speaker, text)
    except Exception:
        logger.exception("forward_final failed")


def _sync_speaker_user_id(provider: Any) -> str:
    """Resolve speaker_user_id synchronously from the provider.

    Wider design: the LiveKit room can have multiple human participants;
    the speaker for a given transcript event is the participant whose
    audio track produced it. Phase 6 wires this via the participant
    identity carried by the underlying STT plugin event. For now the
    provider is a callable returning a best-effort guess so the
    interface is in place.
    """
    if callable(provider):
        try:
            value = provider()
            if isinstance(value, str):
                return value
        except Exception:  # noqa: BLE001
            return ""
    if isinstance(provider, str):
        return provider
    return ""
