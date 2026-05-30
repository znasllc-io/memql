"""memql gRPC stream wrapper.

Owns one bidirectional MemqlService.Stream connection for a voice-agent
session. Send-side methods serialize VoiceAgent* messages onto the
shared queue; the read-loop coroutine fans incoming server messages
out to per-(correlate_to) asyncio futures so callers can `await` a
specific reply by message id.

Phase 4 hooks the memql_llm_plugin into this surface to make
TurnRequest -> TurnDelta streaming look like an LLM completion.
"""

from __future__ import annotations

import asyncio
import logging
import uuid
from collections.abc import AsyncIterator, Awaitable, Callable
from typing import Any

import grpc

logger = logging.getLogger(__name__)


# Callback signature for server-pushed messages with no client-side
# correlation -- currently used for VoiceAgentSpeak. The callback is
# awaited inline in the read loop, so it must be fast or schedule its
# own task for any real work.
PushHandler = Callable[[Any], Awaitable[None]]


class MemqlGrpcClient:
    """Async wrapper around the bidirectional MemqlService.Stream RPC.

    Authentication is via metadata header
    `authorization: Bearer <voice_agent_token>`. The voice_agent_token
    is an identity-issued class="voice_agent" JWT; the memql side
    verifies it via the cluster's JWKS endpoint and admits the
    resulting identity to the VoiceAgent* message surface only.
    See docs/auth/voice-agent-jwt.md for the provisioning flow.
    """

    def __init__(self, addr: str, voice_agent_token: str) -> None:
        self.addr = addr
        self.voice_agent_token = voice_agent_token
        self.channel: grpc.aio.Channel | None = None
        self.stream: Any = None  # bidi call -- type depends on generated stub
        self._send_queue: asyncio.Queue[Any] = asyncio.Queue(maxsize=256)
        self._waiters: dict[str, asyncio.Future[Any]] = {}
        self._stream_waiters: dict[str, asyncio.Queue[Any]] = {}
        self._push_handlers: dict[str, PushHandler] = {}
        self._read_task: asyncio.Task[None] | None = None
        self._write_task: asyncio.Task[None] | None = None
        self._connected = asyncio.Event()
        self._closed = False

    def set_push_handler(self, payload_field: str, handler: PushHandler) -> None:
        """Register an unsolicited-message handler keyed by oneof field name.

        Server-pushed messages (e.g. VoiceAgentSpeak) arrive with no
        correlate_to set on the read loop, so the per-request future map
        cannot dispatch them. Callers register a handler keyed by the
        MemqlServerMessage oneof field name (e.g. "voice_agent_speak").
        The read loop dispatches matching messages here; unmatched
        unsolicited messages fall through to the debug-log path.
        """
        self._push_handlers[payload_field] = handler

    async def connect(self) -> None:
        """Open the gRPC channel + bidi stream. Idempotent."""
        if self.channel is not None:
            return

        # Import lazily so importing this module doesn't require the
        # generated proto stubs to exist (they're built by `make voice-agent`).
        from voice_agent.proto import memql_pb2_grpc  # type: ignore

        logger.info("connecting to memql gRPC at %s", self.addr)
        self.channel = grpc.aio.insecure_channel(self.addr)
        stub = memql_pb2_grpc.MemqlServiceStub(self.channel)
        # The voice-agent stream interceptor on the memql side
        # verifies the bearer as a class="voice_agent" JWT and
        # admits it to the VoiceAgent* message surface only.
        self.stream = stub.Stream(
            self._send_iter(),
            metadata=[("authorization", f"Bearer {self.voice_agent_token}")],
        )
        self._read_task = asyncio.create_task(self._read_loop())
        self._connected.set()

    async def close(self) -> None:
        """Tear down the stream + channel."""
        self._closed = True
        if self._read_task:
            self._read_task.cancel()
        if self.channel:
            await self.channel.close()
        self.channel = None

    async def _send_iter(self) -> AsyncIterator[Any]:
        """Drain the send queue into the bidi stream."""
        while not self._closed:
            msg = await self._send_queue.get()
            if msg is None:
                break
            yield msg

    async def _read_loop(self) -> None:
        """Dispatch server messages to per-correlation waiters."""
        assert self.stream is not None
        try:
            async for envelope in self.stream:
                correlate = envelope.correlate_to
                # Stream-shaped waiters take precedence -- TurnDelta /
                # TurnComplete fire on a queue so the consumer can iterate.
                if correlate in self._stream_waiters:
                    await self._stream_waiters[correlate].put(envelope)
                    continue
                fut = self._waiters.pop(correlate, None)
                if fut is not None and not fut.done():
                    fut.set_result(envelope)
                    continue
                # Unsolicited / server-pushed message. Dispatch by
                # oneof field name (e.g. "voice_agent_speak") if a
                # handler is registered; otherwise debug-log.
                payload_field = envelope.WhichOneof("payload") or ""
                handler = self._push_handlers.get(payload_field)
                if handler is not None:
                    try:
                        await handler(envelope)
                    except Exception:  # noqa: BLE001
                        logger.exception(
                            "push handler crashed payload=%s", payload_field
                        )
                    continue
                logger.debug(
                    "unawaited server message correlate_to=%s payload=%s",
                    correlate, payload_field,
                )
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("memql gRPC read loop crashed")
            raise

    async def send_request(self, payload_field: str, payload: Any) -> Any:
        """Send a one-shot request, await the single matching reply.

        `payload_field` is the oneof field name on MemqlClientMessage,
        e.g. "voice_agent_session_start". `payload` is the typed message.
        Returns the matching MemqlServerMessage.
        """
        from voice_agent.proto import memql_pb2  # type: ignore

        # `partition` was dropped from the MemqlClientMessage wire in
        # #56 phase 8 (proto reserves the name); don't set it.
        envelope = memql_pb2.MemqlClientMessage()
        envelope.message_id = uuid.uuid4().hex
        getattr(envelope, payload_field).CopyFrom(payload)

        future: asyncio.Future[Any] = asyncio.get_event_loop().create_future()
        self._waiters[envelope.message_id] = future
        await self._send_queue.put(envelope)
        return await future

    async def send_stream_request(
        self, payload_field: str, payload: Any
    ) -> tuple[str, asyncio.Queue[Any]]:
        """Send a request that streams replies; returns the correlation id and
        a queue the caller drains until the terminal message arrives.
        """
        from voice_agent.proto import memql_pb2  # type: ignore

        # `partition` was dropped from the MemqlClientMessage wire in
        # #56 phase 8 (proto reserves the name); don't set it.
        envelope = memql_pb2.MemqlClientMessage()
        envelope.message_id = uuid.uuid4().hex
        getattr(envelope, payload_field).CopyFrom(payload)

        queue: asyncio.Queue[Any] = asyncio.Queue(maxsize=128)
        self._stream_waiters[envelope.message_id] = queue
        await self._send_queue.put(envelope)
        return envelope.message_id, queue

    def release_stream(self, correlate_id: str) -> None:
        """Caller signals the stream is exhausted; remove the queue."""
        self._stream_waiters.pop(correlate_id, None)
