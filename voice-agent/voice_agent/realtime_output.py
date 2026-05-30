"""Capture the Realtime model's spoken output into memql utterances (#437).

Why this module exists
----------------------
In the cascade voice path memql owns the assistant turn end to end:
``MemqlLLM`` opens a ``VoiceAgentTurnRequest``, cognition runs the agent
loop and INSERTS the SI reply utterance itself (with structured
citations off the ``respondToUser`` envelope). Chat, canvas, the
conductor's history, and audit all read that one utterance row.

The Realtime executor (#434) replaces that: OpenAI's ``gpt-realtime``
model both thinks and speaks, emitting its output transcript directly --
it never routes through ``VoiceAgentTurnRequest``. So nothing inserts the
SI utterance, and every memql-utterance-backed surface goes dark for
voice turns.

This module closes that gap. It subscribes to the ``AgentSession``'s
``conversation_item_added`` event, picks off each completed
assistant-role item (the realtime model's final output transcript),
derives structured citations, mints a stable ``reply_id`` (the chat
panel's streaming-replyId contract), and forwards the pair to memql via
``VoiceAgentRealtimeOutput``. The server (``handleVoiceAgentRealtimeOutput``)
inserts an SI utterance whose wire shape is byte-identical to a
text/cascade reply, so chat (streaming reveal + role label + citation
chips), canvas, conductor history, and audit render a realtime voice
turn exactly like a typed one.

Citation strategy
-----------------
The realtime model speaks, so citations cannot ride a ``respondToUser``
envelope as they do in the cascade. The strategy here is **post-hoc
phrase matching against the injected grounding context** (the same
grounding #436 injects into the realtime session instructions):

* A :class:`CitationResolver` is handed the grounding context as a list
  of ``(domain_id, phrases)`` entries.
* For each completed assistant transcript, it scans for the longest
  verbatim ``phrase`` occurrences and emits one
  ``{domain_id, matched_phrase}`` citation per hit -- the exact shape
  the cascade persists and the frontend's ``splitTextAtCitations``
  renderer already wraps in chips.

The default resolver is a no-op (empty context -> no citations), so an
un-grounded realtime reply lands byte-identical to an un-grounded text
reply. #436 supplies the grounding context that makes this fire; the
matcher itself is grounding-source-agnostic.
"""

from __future__ import annotations

import logging
import re
import time
import uuid
from dataclasses import dataclass, field
from typing import Any

from voice_agent.grpc_client import MemqlGrpcClient

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class GroundingDomain:
    """One knowledge domain's grounding context for citation matching.

    ``domain_id`` is the bare slug the frontend deep-links to
    (``/space?panel=knowledge&domain=<domain_id>``). ``phrases`` are
    verbatim snippets from that domain's injected grounding -- the
    matcher looks for these substrings in the model's spoken transcript.
    """

    domain_id: str
    phrases: tuple[str, ...]


@dataclass
class CitationResolver:
    """Derive structured citations from an assistant transcript.

    Post-hoc phrase matcher: for each :class:`GroundingDomain`, find the
    domain's phrases verbatim in the transcript and emit one citation per
    distinct matched phrase. Matching is case-insensitive on a normalized
    whitespace view but the emitted ``matched_phrase`` is the verbatim
    substring AS IT APPEARS in the transcript, so the frontend's
    ``splitTextAtCitations`` (which does an exact ``indexOf``) finds it.

    The default (empty ``domains``) emits no citations -- an un-grounded
    realtime reply is then byte-identical to an un-grounded text reply.
    #436 populates ``domains`` from the injected grounding context.
    """

    domains: tuple[GroundingDomain, ...] = ()
    # Minimum phrase length (chars) worth citing -- guards against a
    # one-word phrase like "the" matching everything.
    min_phrase_len: int = 8

    def resolve(self, transcript: str) -> list[dict[str, str]]:
        text = transcript or ""
        if not text or not self.domains:
            return []
        haystack = text.lower()
        seen: set[tuple[str, str]] = set()
        citations: list[dict[str, str]] = []
        for domain in self.domains:
            domain_id = (domain.domain_id or "").strip()
            if not domain_id:
                continue
            # Longest phrases first so a longer, more specific match
            # wins before a shorter substring of it is considered.
            for phrase in sorted(domain.phrases, key=len, reverse=True):
                phrase = (phrase or "").strip()
                if len(phrase) < self.min_phrase_len:
                    continue
                idx = haystack.find(phrase.lower())
                if idx < 0:
                    continue
                # Emit the verbatim substring from the transcript so the
                # frontend's exact indexOf wrap succeeds even when the
                # grounding phrase differs in case.
                matched = text[idx : idx + len(phrase)]
                key = (domain_id, matched)
                if key in seen:
                    continue
                seen.add(key)
                citations.append({"domain_id": domain_id, "matched_phrase": matched})
        return citations


def build_citation_resolver(grounding: Any) -> CitationResolver:
    """Build a :class:`CitationResolver` from a grounding context.

    ``grounding`` is whatever #436 hands us -- accepted shapes:

    * ``None`` / empty            -> no-op resolver (no citations).
    * ``GroundingContext`` from ``realtime_instructions`` (#436): a
      ``.facts`` sequence of ``GroundingFact`` (``text`` + ``domain_name``).
      Each fact's body is the citable material; ``domain_name`` is the
      deep-link slug the frontend prettifies. This is the live #436
      surface -- the per-turn grounding the realtime session injects.
    * ``Sequence[GroundingDomain]``.
    * ``Sequence[Mapping]`` with ``domain_id`` + ``phrases`` keys.
    * ``Mapping[domain_id, Sequence[phrase]]``.

    Anything unrecognized degrades to the no-op resolver rather than
    raising -- a citation derivation failure must never take the voice
    turn down.
    """
    if not grounding:
        return CitationResolver()
    domains: list[GroundingDomain] = []
    try:
        # #436 GroundingContext: duck-typed on a `.facts` sequence whose
        # items carry `text` + `domain_name`. Group each fact's body under
        # its domain so a spoken phrase that lands verbatim from the
        # injected context cites the domain it came from.
        facts = getattr(grounding, "facts", None)
        if facts is not None:
            by_domain: dict[str, list[str]] = {}
            for fact in facts:
                text = str(getattr(fact, "text", "") or "").strip()
                if not text:
                    continue
                domain_id = str(getattr(fact, "domain_name", "") or "").strip()
                if not domain_id:
                    continue
                by_domain.setdefault(domain_id, []).append(text)
            for domain_id, phrases in by_domain.items():
                domains.append(GroundingDomain(domain_id=domain_id, phrases=tuple(phrases)))
        elif isinstance(grounding, dict):
            for domain_id, phrases in grounding.items():
                domains.append(
                    GroundingDomain(
                        domain_id=str(domain_id),
                        phrases=tuple(str(p) for p in (phrases or [])),
                    )
                )
        else:
            for entry in grounding:
                if isinstance(entry, GroundingDomain):
                    domains.append(entry)
                elif isinstance(entry, dict):
                    domains.append(
                        GroundingDomain(
                            domain_id=str(entry.get("domain_id", "")),
                            phrases=tuple(str(p) for p in (entry.get("phrases") or [])),
                        )
                    )
    except Exception:  # noqa: BLE001 -- never fail the turn on grounding shape
        logger.exception("build_citation_resolver: unrecognized grounding shape")
        return CitationResolver()
    return CitationResolver(domains=tuple(domains))


@dataclass
class RealtimeOutputForwarder:
    """Forward the realtime model's spoken output to memql as SI utterances.

    One per session. Holds the space + GA attribution and the citation
    resolver. ``forward`` mints a ``reply_id`` (the chat streaming
    contract id), derives citations, and sends ``VoiceAgentRealtimeOutput``;
    the server inserts the SI utterance and acks with the canonical row id.
    """

    client: MemqlGrpcClient
    space_id: str
    ga_agent_id: str
    citation_resolver: CitationResolver = field(default_factory=CitationResolver)

    async def forward(
        self, text: str, *, reply_to_id: str = "", reply_id: str | None = None
    ) -> str:
        """Forward one completed assistant transcript. Returns the utterance id.

        ``reply_id`` defaults to a freshly-minted id; pass one to reuse
        an in-flight streaming key. Returns "" on a failed insert (the
        caller logs; the voice turn still played).
        """
        from voice_agent.proto import memql_pb2  # type: ignore

        body = (text or "").strip()
        if not body:
            return ""

        rid = reply_id or _mint_reply_id()
        citations = self.citation_resolver.resolve(body)
        proto_citations = [
            memql_pb2.AgentTurnCitation(
                domain_id=c["domain_id"], matched_phrase=c["matched_phrase"]
            )
            for c in citations
        ]

        payload = memql_pb2.VoiceAgentRealtimeOutput(
            space_id=self.space_id,
            ga_agent_id=self.ga_agent_id,
            reply_id=rid,
            text=body,
            reply_to_id=reply_to_id or "",
            citations=proto_citations,
        )

        logger.info(
            "realtime output forward space=%s ga=%s reply_id=%s len=%d citations=%d",
            self.space_id,
            self.ga_agent_id,
            rid,
            len(body),
            len(proto_citations),
        )

        reply = await self.client.send_request("voice_agent_realtime_output", payload)
        ack = reply.voice_agent_realtime_output_ack
        if not ack.success:
            logger.warning(
                "realtime output insert failed reply_id=%s err=%s msg=%s",
                rid,
                ack.error_code,
                ack.error_message,
            )
            return ""
        return ack.utterance_id


def attach_realtime_output_capture(
    session: Any,
    forwarder: RealtimeOutputForwarder,
) -> None:
    """Wire the AgentSession's assistant-output transcript to ``forwarder``.

    Subscribes to ``conversation_item_added`` and forwards every completed
    assistant-role item's final transcript. This is the realtime parallel
    to ``attach_transcript_forwarding`` (which captures the USER side off
    ``user_input_transcribed``): together they keep memql utterances the
    source of truth for both sides of a realtime turn.

    Binding failures are logged, not raised -- a missing event surface
    must not take the voice session down (it degrades to "voice plays but
    chat goes dark", same as the pre-#437 state).
    """
    if session is None:
        return

    def _on_conversation_item_added(event: Any) -> None:
        item = getattr(event, "item", event)
        if not _is_assistant_item(item):
            return
        text = _item_text(item)
        if not text:
            return
        logger.info("realtime assistant item added len=%d", len(text))
        _schedule_forward(forwarder, text)

    try:
        session.on("conversation_item_added", _on_conversation_item_added)
        logger.info("bound realtime output capture to conversation_item_added")
    except Exception as err:  # noqa: BLE001
        logger.error("failed to bind conversation_item_added: %s", err)


def _schedule_forward(forwarder: RealtimeOutputForwarder, text: str) -> None:
    import asyncio

    async def _run() -> None:
        try:
            await forwarder.forward(text)
        except Exception:  # noqa: BLE001
            logger.exception("realtime output forward failed")

    asyncio.create_task(_run())


def _is_assistant_item(item: Any) -> bool:
    """True when a conversation item is the assistant's spoken output.

    LiveKit Agents 1.5 surfaces conversation items as ``ChatMessage``-like
    objects carrying a ``role``. We forward only ``assistant`` items --
    user items are already captured by ``attach_transcript_forwarding``
    off the parallel Deepgram STT, and forwarding them here would
    double-insert the human turn.
    """
    role = getattr(item, "role", "") or ""
    return str(role).lower() == "assistant"


def _item_text(item: Any) -> str:
    """Extract the final transcript text off a conversation item.

    Probes the common LiveKit Agents shapes: ``text_content`` (a method
    or property), ``text``, or a ``content`` list of strings. Whitespace-
    normalized; empty when the item carries no text yet (e.g. an
    audio-only item whose transcript hasn't resolved).
    """
    text_content = getattr(item, "text_content", None)
    if callable(text_content):
        try:
            value = text_content()
            if isinstance(value, str) and value.strip():
                return _normalize(value)
        except Exception:  # noqa: BLE001
            pass
    elif isinstance(text_content, str) and text_content.strip():
        return _normalize(text_content)

    text = getattr(item, "text", None)
    if isinstance(text, str) and text.strip():
        return _normalize(text)

    content = getattr(item, "content", None)
    if isinstance(content, (list, tuple)):
        parts = [c for c in content if isinstance(c, str)]
        if parts:
            return _normalize(" ".join(parts))

    return ""


_WS_RE = re.compile(r"\s+")


def _normalize(text: str) -> str:
    return _WS_RE.sub(" ", text).strip()


def _mint_reply_id() -> str:
    """Mint a stable reply/utterance id.

    Flat ``utt-si-<nanos>-<hex>`` shape -- never embeds a participant id
    (the engine's canonicalizer would mis-parse an embedded
    ``:v1:cognition:participant:`` as the type tag and reject the
    insert). The server accepts this as the committed utterance id so it
    matches the chat panel's streaming replyId.
    """
    return f"utt-si-{time.time_ns()}-{uuid.uuid4().hex[:8]}"
