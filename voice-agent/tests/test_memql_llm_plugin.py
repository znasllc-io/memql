"""memql LLM plugin extraction tests.

End-to-end streaming requires a live gRPC server, so we cover the
deterministic surface: the chat-context parser that pulls the latest
user turn out of `livekit.agents.llm.ChatContext` and the ABC-subclass
invariant that gates whether the framework will invoke us at all.
"""

from __future__ import annotations

import pytest

pytest.importorskip("livekit.agents")

from livekit.agents.llm import ChatContext, ChatMessage, LLM, LLMStream

from voice_agent.memql_llm_plugin import MemqlLLM, _MemqlLLMStream, _latest_user_turn


def _ctx(messages: list[ChatMessage]) -> ChatContext:
    ctx = ChatContext.empty()
    for m in messages:
        ctx.insert(m)
    return ctx


def test_subclass_invariants() -> None:
    # The framework's agent_activity uses isinstance(llm, llm.LLM) /
    # isinstance(stream, llm.LLMStream) gates. If MemqlLLM ever stops
    # being a real subclass, the AgentSession silently no-ops.
    assert issubclass(MemqlLLM, LLM)
    assert issubclass(_MemqlLLMStream, LLMStream)


def test_extract_returns_empty_for_none_context() -> None:
    assert _latest_user_turn(None) == ("", "")  # type: ignore[arg-type]


def test_extract_returns_empty_for_empty_context() -> None:
    assert _latest_user_turn(ChatContext.empty()) == ("", "")


def test_extract_picks_most_recent_user_message() -> None:
    ctx = _ctx(
        [
            ChatMessage(role="user", content=["first"], extra={"identity": "u-1"}),
            ChatMessage(role="assistant", content=["hi"]),
            ChatMessage(role="user", content=["second"], extra={"identity": "u-2"}),
            ChatMessage(role="assistant", content=["thanks"]),
        ]
    )
    text, speaker = _latest_user_turn(ctx)
    assert text == "second"
    assert speaker == "u-2"


def test_extract_joins_multipart_text_content() -> None:
    ctx = _ctx(
        [
            ChatMessage(
                role="user",
                content=["hello", "world"],
                extra={"identity": "u-9"},
            )
        ]
    )
    text, speaker = _latest_user_turn(ctx)
    # ChatMessage.text_content joins with newline, not space.
    assert text == "hello\nworld"
    assert speaker == "u-9"


def test_extract_empty_speaker_when_no_identity_in_extra() -> None:
    ctx = _ctx([ChatMessage(role="user", content=["hey"])])
    text, speaker = _latest_user_turn(ctx)
    assert text == "hey"
    assert speaker == ""
