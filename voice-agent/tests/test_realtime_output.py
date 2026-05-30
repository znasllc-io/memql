"""Tests for realtime output capture -> memql SI utterances (#437).

These cover the deterministic surface of the output-capture path without
standing up a real OpenAI Realtime session or a live gRPC server:

* the citation resolver derives structured {domain_id, matched_phrase}
  citations from a transcript by post-hoc phrase matching against the
  injected grounding context (the #436 contract), and is a strict no-op
  when un-grounded so a realtime reply is byte-identical to a text reply;
* build_citation_resolver accepts the grounding shapes #436 may hand it
  and degrades to a no-op resolver on anything unrecognized;
* RealtimeOutputForwarder.forward builds the VoiceAgentRealtimeOutput
  wire message with the GA attribution + reply_id + derived citations and
  returns the server-acked utterance id (the chat/canvas parity row);
* the conversation_item_added handler forwards ONLY assistant items (user
  items are captured by the parallel Deepgram STT path, not here) and
  pulls text off the LiveKit conversation-item shapes.

The gRPC client is faked; the proto stubs are real (generated from
component/grpc/memql.proto), so the wire message is exercised for real.
"""

from __future__ import annotations

import asyncio
from typing import Any

import pytest

from voice_agent.realtime_output import (
    CitationResolver,
    GroundingDomain,
    RealtimeOutputForwarder,
    attach_realtime_output_capture,
    build_citation_resolver,
    _is_assistant_item,
    _item_text,
)

pytest.importorskip("voice_agent.proto.memql_pb2", reason="proto stubs not generated")


# --- citation resolver ---------------------------------------------------


def test_resolver_no_grounding_emits_no_citations() -> None:
    resolver = CitationResolver()
    assert resolver.resolve("anything the model says") == []


def test_resolver_matches_grounding_phrase() -> None:
    resolver = CitationResolver(
        domains=(
            GroundingDomain(
                domain_id="customer_relations",
                phrases=("escalation policy", "refund window"),
            ),
        )
    )
    citations = resolver.resolve("Per the escalation policy you should loop in a manager first.")
    assert citations == [{"domain_id": "customer_relations", "matched_phrase": "escalation policy"}]


def test_resolver_matched_phrase_is_verbatim_from_transcript() -> None:
    # The grounding phrase differs in case; the emitted matched_phrase
    # must be the verbatim substring AS IT APPEARS so the frontend's
    # exact indexOf wrap succeeds.
    resolver = CitationResolver(
        domains=(GroundingDomain(domain_id="hist", phrases=("Ancient History",)),)
    )
    citations = resolver.resolve("I studied ancient history at length.")
    assert citations == [{"domain_id": "hist", "matched_phrase": "ancient history"}]


def test_resolver_dedupes_identical_hits() -> None:
    resolver = CitationResolver(
        domains=(GroundingDomain(domain_id="d", phrases=("knowledge base",)),)
    )
    citations = resolver.resolve("knowledge base ... knowledge base again")
    assert len(citations) == 1


def test_resolver_skips_short_phrases() -> None:
    resolver = CitationResolver(
        domains=(GroundingDomain(domain_id="d", phrases=("the",)),),
        min_phrase_len=8,
    )
    assert resolver.resolve("the quick brown fox") == []


def test_resolver_no_match_emits_no_citations() -> None:
    resolver = CitationResolver(
        domains=(GroundingDomain(domain_id="d", phrases=("nonexistent phrase here",)),)
    )
    assert resolver.resolve("completely unrelated text") == []


# --- build_citation_resolver shapes -------------------------------------


def test_build_resolver_none_is_noop() -> None:
    assert build_citation_resolver(None).resolve("escalation policy text") == []


def test_build_resolver_from_mapping() -> None:
    resolver = build_citation_resolver({"d": ["escalation policy"]})
    assert resolver.resolve("our escalation policy applies") == [
        {"domain_id": "d", "matched_phrase": "escalation policy"}
    ]


def test_build_resolver_from_dict_entries() -> None:
    resolver = build_citation_resolver([{"domain_id": "d", "phrases": ["refund window terms"]}])
    assert resolver.resolve("the refund window terms are clear") == [
        {"domain_id": "d", "matched_phrase": "refund window terms"}
    ]


def test_build_resolver_from_grounding_domain_objects() -> None:
    resolver = build_citation_resolver(
        [GroundingDomain(domain_id="d", phrases=("training material",))]
    )
    assert resolver.resolve("see the training material") == [
        {"domain_id": "d", "matched_phrase": "training material"}
    ]


def test_build_resolver_from_grounding_context() -> None:
    # The live #436 surface: a GroundingContext-like object with a
    # `.facts` sequence of facts carrying `text` + `domain_name`. Each
    # fact's body becomes a citable phrase under its domain.
    from voice_agent.realtime_instructions import GroundingContext, GroundingFact

    grounding = GroundingContext(
        facts=(
            GroundingFact(
                text="refund window is thirty days from purchase",
                domain_name="customer_relations",
            ),
            GroundingFact(text="", domain_name="ignored"),  # empty body skipped
        )
    )
    resolver = build_citation_resolver(grounding)
    citations = resolver.resolve(
        "Yes -- the refund window is thirty days from purchase, so you are fine."
    )
    assert citations == [
        {
            "domain_id": "customer_relations",
            "matched_phrase": "refund window is thirty days from purchase",
        }
    ]


def test_build_resolver_empty_grounding_context_is_noop() -> None:
    from voice_agent.realtime_instructions import GroundingContext

    resolver = build_citation_resolver(GroundingContext(facts=()))
    assert resolver.resolve("anything") == []


def test_build_resolver_bad_shape_degrades_to_noop() -> None:
    # A grounding object that raises while iterating must not blow up the
    # turn -- it degrades to a no-op resolver.
    class _Exploding:
        def __iter__(self) -> Any:
            raise RuntimeError("boom")

    resolver = build_citation_resolver(_Exploding())
    assert resolver.resolve("escalation policy") == []


# --- conversation-item helpers ------------------------------------------


class _Item:
    def __init__(self, role: str, text: str | None = None, **extra: Any) -> None:
        self.role = role
        if text is not None:
            self.text = text
        for k, v in extra.items():
            setattr(self, k, v)


def test_is_assistant_item() -> None:
    assert _is_assistant_item(_Item("assistant")) is True
    assert _is_assistant_item(_Item("user")) is False
    assert _is_assistant_item(_Item("system")) is False


def test_item_text_from_text_attr() -> None:
    assert _item_text(_Item("assistant", text="  hello   world ")) == "hello world"


def test_item_text_from_text_content_method() -> None:
    item = _Item("assistant")
    item.text_content = lambda: "spoken words"  # type: ignore[attr-defined]
    assert _item_text(item) == "spoken words"


def test_item_text_from_content_list() -> None:
    item = _Item("assistant", content=["part one", "part two"])
    assert _item_text(item) == "part one part two"


def test_item_text_empty_when_no_text() -> None:
    assert _item_text(_Item("assistant")) == ""


# --- forwarder wire message ---------------------------------------------


class _FakeAck:
    def __init__(self, success: bool, utterance_id: str = "") -> None:
        self.success = success
        self.utterance_id = utterance_id
        self.error_code = "" if success else "insert_failed"
        self.error_message = "" if success else "boom"


class _FakeReply:
    def __init__(self, ack: _FakeAck) -> None:
        self.voice_agent_realtime_output_ack = ack


class _FakeClient:
    """Records the (field, payload) of the last send_request call."""

    def __init__(self, ack: _FakeAck) -> None:
        self._ack = ack
        self.sent: list[tuple[str, Any]] = []

    async def send_request(self, field: str, payload: Any) -> _FakeReply:
        self.sent.append((field, payload))
        return _FakeReply(self._ack)


@pytest.mark.asyncio
async def test_forward_builds_wire_message_and_returns_utterance_id() -> None:
    client = _FakeClient(_FakeAck(success=True, utterance_id="utt-committed-1"))
    forwarder = RealtimeOutputForwarder(
        client=client,  # type: ignore[arg-type]
        space_id="space-1",
        ga_agent_id="agent-ga-1",
        citation_resolver=CitationResolver(
            domains=(GroundingDomain(domain_id="d", phrases=("escalation policy",)),)
        ),
    )

    utterance_id = await forwarder.forward(
        "Per the escalation policy, loop in a manager.",
        reply_to_id="utt-user-9",
        reply_id="utt-reply-7",
    )

    assert utterance_id == "utt-committed-1"
    assert len(client.sent) == 1
    field, payload = client.sent[0]
    assert field == "voice_agent_realtime_output"
    assert payload.space_id == "space-1"
    assert payload.ga_agent_id == "agent-ga-1"
    assert payload.reply_id == "utt-reply-7"
    assert payload.reply_to_id == "utt-user-9"
    assert payload.text == "Per the escalation policy, loop in a manager."
    assert len(payload.citations) == 1
    assert payload.citations[0].domain_id == "d"
    assert payload.citations[0].matched_phrase == "escalation policy"


@pytest.mark.asyncio
async def test_forward_mints_reply_id_when_absent() -> None:
    client = _FakeClient(_FakeAck(success=True, utterance_id="utt-x"))
    forwarder = RealtimeOutputForwarder(
        client=client,  # type: ignore[arg-type]
        space_id="s",
        ga_agent_id="g",
    )
    await forwarder.forward("hello")
    _, payload = client.sent[0]
    assert payload.reply_id.startswith("utt-si-")
    # No grounding -> no citations (byte-identical to a text reply).
    assert len(payload.citations) == 0


@pytest.mark.asyncio
async def test_forward_empty_text_is_skipped() -> None:
    client = _FakeClient(_FakeAck(success=True))
    forwarder = RealtimeOutputForwarder(
        client=client,  # type: ignore[arg-type]
        space_id="s",
        ga_agent_id="g",
    )
    assert await forwarder.forward("   ") == ""
    assert client.sent == []


@pytest.mark.asyncio
async def test_forward_failed_insert_returns_empty() -> None:
    client = _FakeClient(_FakeAck(success=False))
    forwarder = RealtimeOutputForwarder(
        client=client,  # type: ignore[arg-type]
        space_id="s",
        ga_agent_id="g",
    )
    assert await forwarder.forward("text") == ""


# --- attach: only assistant items are forwarded -------------------------


class _FakeSession:
    def __init__(self) -> None:
        self._handlers: dict[str, Any] = {}

    def on(self, event: str, handler: Any) -> None:
        self._handlers[event] = handler

    def emit(self, event: str, payload: Any) -> None:
        self._handlers[event](payload)


class _Event:
    def __init__(self, item: Any) -> None:
        self.item = item


@pytest.mark.asyncio
async def test_attach_forwards_only_assistant_items() -> None:
    client = _FakeClient(_FakeAck(success=True, utterance_id="utt-1"))
    forwarder = RealtimeOutputForwarder(
        client=client,  # type: ignore[arg-type]
        space_id="s",
        ga_agent_id="g",
    )
    session = _FakeSession()
    attach_realtime_output_capture(session=session, forwarder=forwarder)

    # User item: must NOT be forwarded (captured by the STT path).
    session.emit("conversation_item_added", _Event(_Item("user", text="hi there")))
    # Assistant item: must be forwarded.
    session.emit("conversation_item_added", _Event(_Item("assistant", text="hello back")))
    # The handler schedules an asyncio task; let it run.
    await asyncio.sleep(0)
    await asyncio.sleep(0)

    assert len(client.sent) == 1
    _, payload = client.sent[0]
    assert payload.text == "hello back"
