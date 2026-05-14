"""Forward LiveKit Agents transcript events into memql via gRPC.

Subscribes to the LiveKit Agents `SpeechEvent` stream. For each partial
emits a VoiceAgentPartialTranscript; for each final emits a
VoiceAgentFinalTranscript. memql turns these into a graph event +
canonical chat-row insert respectively.

Sequence numbers are monotonic per (space, user); memql does not
interpret them but the frontend uses them to order out-of-order
partials correctly.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field

from voice_agent.grpc_client import MemqlGrpcClient

logger = logging.getLogger(__name__)


@dataclass
class TranscriptForwarder:
    """Per-session forwarder. Holds a sequence counter per speaker."""

    client: MemqlGrpcClient
    space_id: str
    _counters: dict[str, int] = field(default_factory=dict)

    async def forward_partial(self, speaker_user_id: str, text: str) -> None:
        """Send a streaming partial. Returns immediately on ack."""
        from voice_agent.proto import memql_pb2  # type: ignore

        seq = self._counters.get(speaker_user_id, 0) + 1
        self._counters[speaker_user_id] = seq

        payload = memql_pb2.VoiceAgentPartialTranscript(
            space_id=self.space_id,
            speaker_user_id=speaker_user_id,
            partial_text=text,
            sequence=seq,
        )
        reply = await self.client.send_request("voice_agent_partial_transcript", payload)
        ack = reply.voice_agent_partial_ack
        if not ack.success:
            logger.warning(
                "partial forward failed seq=%d err=%s msg=%s",
                seq, ack.error_code, ack.error_message,
            )

    async def forward_final(self, speaker_user_id: str, text: str, confidence: float = 0.0) -> str:
        """Send the final transcript. Returns the canonical utterance id."""
        from voice_agent.proto import memql_pb2  # type: ignore

        payload = memql_pb2.VoiceAgentFinalTranscript(
            space_id=self.space_id,
            speaker_user_id=speaker_user_id,
            final_text=text,
            confidence=confidence,
            ended_at_ms=int(time.time() * 1000),
        )
        reply = await self.client.send_request("voice_agent_final_transcript", payload)
        ack = reply.voice_agent_final_ack
        if not ack.success:
            raise RuntimeError(
                f"final transcript insert failed: {ack.error_code} {ack.error_message}"
            )
        # Reset the counter; next utterance starts a fresh partial sequence.
        self._counters[speaker_user_id] = 0
        return ack.utterance_id
