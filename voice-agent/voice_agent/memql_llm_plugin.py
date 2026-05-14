"""Custom LiveKit Agents LLM plugin that speaks memql's VoiceAgent* gRPC.

The LiveKit Agents framework drives turn-taking and pipes the final
user transcript into the configured `llm` instance, expecting back a
streaming text response. We swap in a custom `LLM` whose `.chat()`
opens a VoiceAgentTurnRequest stream against memql and forwards each
VoiceAgentTurnDelta back as a ChatChunk.

The plugin does NOT call any LLM provider directly. memql owns the
conductor + agent tool loop entirely; voice-agent is a transport.
Specialists' chime-in replies land in chat via memql's normal agent
dispatch path; this plugin never sees them.

MemqlLLM is a real subclass of `livekit.agents.llm.LLM` -- not a
duck-typed lookalike. Earlier shape failed every `isinstance(llm,
llm.LLM)` gate inside `agent_activity.py`, which silently kept the
framework from invoking the LLM at all.
"""

from __future__ import annotations

import logging
from typing import Any

from livekit.agents import llm as lk_llm
from livekit.agents.llm import (
    LLM,
    LLMStream,
    ChatChunk,
    ChatContext,
    ChoiceDelta,
)
from livekit.agents.llm.tool_context import Tool, ToolChoice
from livekit.agents.types import (
    DEFAULT_API_CONNECT_OPTIONS,
    NOT_GIVEN,
    APIConnectOptions,
    NotGivenOr,
)

from voice_agent.grpc_client import MemqlGrpcClient
from voice_agent.thread_context import Thread, ThreadContext

logger = logging.getLogger(__name__)


class MemqlLLM(LLM):
    """Adapter that makes memql's VoiceAgentTurn* RPC look like a
    LiveKit Agents LLM.

    The framework calls `chat(chat_ctx=...)` after each user-final
    transcript and iterates the returned `LLMStream`. We open a
    streaming VoiceAgentTurnRequest against memql, decode each
    incoming VoiceAgentTurnDelta into a `ChatChunk`, and write it to
    the stream's event channel. Tools execute server-side inside
    memql; this plugin never sees tool calls or tool results.
    """

    def __init__(
        self,
        *,
        client: MemqlGrpcClient,
        space_id: str,
        ga_agent_id: str,
        thread: ThreadContext,
    ) -> None:
        super().__init__()
        self._client = client
        self._space_id = space_id
        self._ga_agent_id = ga_agent_id
        self._thread = thread

    @property
    def model(self) -> str:
        return "memql.cognition"

    @property
    def provider(self) -> str:
        return "memql"

    def chat(
        self,
        *,
        chat_ctx: ChatContext,
        tools: list[Tool] | None = None,
        conn_options: APIConnectOptions = DEFAULT_API_CONNECT_OPTIONS,
        parallel_tool_calls: NotGivenOr[bool] = NOT_GIVEN,
        tool_choice: NotGivenOr[ToolChoice] = NOT_GIVEN,
        extra_kwargs: NotGivenOr[dict[str, Any]] = NOT_GIVEN,
    ) -> LLMStream:
        return _MemqlLLMStream(
            llm=self,
            chat_ctx=chat_ctx,
            tools=tools or [],
            conn_options=conn_options,
            client=self._client,
            space_id=self._space_id,
            ga_agent_id=self._ga_agent_id,
            thread=self._thread,
        )


class _MemqlLLMStream(LLMStream):
    """One turn's reply stream. Writes ChatChunks to `self._event_ch`
    as memql emits VoiceAgentTurnDelta messages; closes when the
    matching VoiceAgentTurnComplete arrives (or on error).
    """

    def __init__(
        self,
        *,
        llm: LLM,
        chat_ctx: ChatContext,
        tools: list[Tool],
        conn_options: APIConnectOptions,
        client: MemqlGrpcClient,
        space_id: str,
        ga_agent_id: str,
        thread: ThreadContext,
    ) -> None:
        super().__init__(
            llm=llm,
            chat_ctx=chat_ctx,
            tools=tools,
            conn_options=conn_options,
        )
        self._client = client
        self._space_id = space_id
        self._ga_agent_id = ga_agent_id
        self._thread = thread

    async def _run(self) -> None:
        # The proto stubs only import correctly inside the running
        # container, so keep it inside _run rather than at module load.
        from voice_agent.proto import memql_pb2  # type: ignore

        user_text, speaker_user_id = _latest_user_turn(self._chat_ctx)
        if not user_text:
            logger.info(
                "memql llm: empty user turn -- skipping VoiceAgentTurnRequest"
            )
            return

        current_thread = await self._thread.get()
        proto_thread = (
            memql_pb2.VoiceAgentTurnRequest.THREAD_CONTEXT_GROUP
            if current_thread == Thread.GROUP
            else memql_pb2.VoiceAgentTurnRequest.THREAD_CONTEXT_TEAM
        )

        request = memql_pb2.VoiceAgentTurnRequest(
            space_id=self._space_id,
            speaker_user_id=speaker_user_id,
            utterance_text=user_text,
            ga_agent_id=self._ga_agent_id,
            thread=proto_thread,
        )

        logger.info(
            "memql llm: turn request space=%s speaker=%s thread=%s len=%d",
            self._space_id, speaker_user_id, current_thread.name, len(user_text),
        )

        correlate_id, queue = await self._client.send_stream_request(
            "voice_agent_turn_request", request
        )
        # The framework wants a stable per-chunk id. Reuse the memql
        # request id; the framework just needs uniqueness across
        # concurrent streams, not per-token.
        chunk_id = correlate_id

        try:
            while True:
                envelope = await queue.get()
                kind = envelope.WhichOneof("payload")
                if kind == "voice_agent_turn_delta":
                    delta_text = envelope.voice_agent_turn_delta.text_delta
                    if not delta_text:
                        continue
                    chunk = ChatChunk(
                        id=chunk_id,
                        delta=ChoiceDelta(
                            role="assistant",
                            content=delta_text,
                        ),
                    )
                    self._event_ch.send_nowait(chunk)
                elif kind == "voice_agent_turn_complete":
                    complete = envelope.voice_agent_turn_complete
                    if complete.error_code:
                        logger.error(
                            "voice agent turn failed code=%s msg=%s",
                            complete.error_code, complete.error_message,
                        )
                    return
                else:
                    logger.debug(
                        "memql llm: ignoring unexpected payload kind=%s", kind,
                    )
        finally:
            self._client.release_stream(correlate_id)


def _latest_user_turn(chat_ctx: ChatContext) -> tuple[str, str]:
    """Return the most recent user-role message text + speaker id.

    `ChatContext.items` is the discriminated union of every event in
    the conversation; we filter for `ChatMessage` with role=='user'
    and walk back from the end. Returns ('', '') when the context is
    empty (e.g. when the framework calls `chat()` before any user
    turn -- the caller's empty-text guard short-circuits).
    """
    if chat_ctx is None:
        return ("", "")

    items = getattr(chat_ctx, "items", None)
    if not items:
        return ("", "")

    for item in reversed(list(items)):
        if not isinstance(item, lk_llm.ChatMessage):
            continue
        if getattr(item, "role", "") != "user":
            continue
        text = item.text_content or ""
        text = text.strip()
        identity = (
            item.extra.get("identity")
            or item.extra.get("speaker_id")
            or ""
        ) if isinstance(item.extra, dict) else ""
        return (text, str(identity))

    return ("", "")
