"""Persona + graph-grounding injection for the Realtime session (#436).

This module is the Realtime analog of memql's cognition agent prompt. When
OpenAI ``gpt-realtime`` generates the voice directly we lose memql's tightly
controlled prompt, so the assistant must still carry its ``personaProfile``
(role / style / voice) and stay grounded in the space's knowledge domains and
graph facts -- the basis for citations on the cascade path.

It owns three concerns, kept separate from the executor wiring so the shared
``build_realtime_executor`` seam (concurrently edited by sibling issues) takes
only a single localized hook call:

1. **Voice selection** -- map the assistant's canonical voice (alto / tenor /
   ...) to a ``gpt-realtime`` voice id. Mirrors the Deepgram catalog in
   ``persona_resolver.py`` but targets OpenAI's voice set.
2. **Persona instructions** -- compose the static session ``instructions``
   string from the persona's role / description / style, trimmed to identity
   + style + constraints. The conductor supplies per-turn directives (mode /
   brevity / angle) on top of this (#432), so the static instructions stay
   small. This is the realtime analog of the cognition prompt's
   "YOUR IDENTITY" / "Personality & Instructions" blocks
   (``dsl/cognition/prompts/cognitionReply.tmpl``).
3. **Grounding items** -- render topic-relevant graph facts + retrieved
   knowledge chunks into ``conversation.item`` payloads that are injected
   before a response is triggered, so grounded answers do not depend solely on
   a tool round-trip.

Scope boundaries (owned by sibling issues, NOT here):

* per-turn conductor directive rendering -> #432 (composes ON TOP of the
  static persona instructions produced here);
* MCP tool bridge -> #435;
* output capture / utterances / citations -> #437.

Everything here is pure / deterministic and unit-tested without standing up a
real Realtime session.
"""

from __future__ import annotations

import logging
from collections.abc import Sequence
from dataclasses import dataclass, field

from voice_agent.persona_resolver import Persona

logger = logging.getLogger(__name__)


# OpenAI gpt-realtime voice catalog, split by perceived gender so the
# canonical voice (alto / soprano / mezzo -> female; tenor / baritone /
# bass -> male) maps onto a gender-matched realtime voice. Mirrors the
# split in ``persona_resolver.DEEPGRAM_AURA2_VOICES`` but targets OpenAI's
# voice set. Kept local for the same reason the Deepgram catalog is: no
# network round-trip to translate canonical -> provider id.
OPENAI_REALTIME_VOICES: dict[str, str] = {
    # Female canonical voices.
    "alto": "marin",
    "soprano": "shimmer",
    "mezzo": "coral",
    # Male canonical voices.
    "tenor": "cedar",
    "baritone": "ash",
    "bass": "verse",
}

# Used when the canonical voice is unknown / unset. ``marin`` is a neutral,
# widely-available gpt-realtime voice; matches the cascade's alto default.
DEFAULT_REALTIME_VOICE = "marin"

# Neutral persona used when the assistant has no stamped personaProfile yet
# (e.g. the ack proto has not been extended to carry role / style). Keeps the
# realtime voice on-task without inventing a specific persona.
DEFAULT_PERSONA_NAME = "Assistant"
DEFAULT_PERSONA_ROLE = "General Assistant"


def resolve_realtime_voice(canonical_voice: str) -> str:
    """Map a canonical voice (alto / tenor / ...) to a gpt-realtime voice id.

    Falls back to :data:`DEFAULT_REALTIME_VOICE` for an unknown or empty
    canonical voice, logging a warning so a drifted catalog is visible.
    """
    voice = OPENAI_REALTIME_VOICES.get((canonical_voice or "").lower(), "")
    if not voice:
        logger.warning(
            "unknown canonical voice %r -- falling back to realtime voice %s",
            canonical_voice,
            DEFAULT_REALTIME_VOICE,
        )
        return DEFAULT_REALTIME_VOICE
    return voice


@dataclass(frozen=True)
class GroundingFact:
    """A single graph fact or retrieved knowledge chunk to inject.

    ``text`` is the chunk / fact body. ``domain_name`` and ``source`` are
    optional attribution used to label the chunk so the model can ground its
    answer in (and, downstream in #437, cite) the right domain.
    """

    text: str
    domain_name: str = ""
    source: str = ""


@dataclass(frozen=True)
class GroundingContext:
    """Topic-relevant grounding for the active turn.

    Mirrors the cascade's grounding inputs: a set of graph facts / retrieved
    knowledge chunks scoped to the active topic. Empty is a first-class state
    -- a turn with no grounding injects nothing and the model answers from the
    persona + conversation alone.
    """

    facts: Sequence[GroundingFact] = field(default_factory=tuple)

    @property
    def is_empty(self) -> bool:
        return not any(f.text.strip() for f in self.facts)


def _persona_name(persona: Persona) -> str:
    return (persona.display_name or "").strip() or DEFAULT_PERSONA_NAME


def _persona_role(persona: Persona) -> str:
    return (persona.role or "").strip() or DEFAULT_PERSONA_ROLE


def build_persona_instructions(persona: Persona) -> str:
    """Compose the static Realtime session ``instructions`` from the persona.

    This is the realtime analog of the cognition agent prompt, trimmed to
    identity + style + constraints. The conductor layers the per-turn
    directive (mode / brevity / angle) on top via the per-response
    ``instructions`` (#432); keeping this static block small avoids fighting
    that directive.

    Always returns a non-empty string: when the persona carries no role /
    style the neutral default identity is rendered so the realtime voice stays
    on-task instead of free-wheeling.
    """
    name = _persona_name(persona)
    role = _persona_role(persona)

    lines: list[str] = [
        f"You are {name}, the {role} in a live voice conversation.",
    ]

    description = (persona.description or "").strip()
    if description:
        lines.append(description)

    style = (persona.style or "").strip()
    if style:
        lines.append("")
        lines.append("Style and personality:")
        lines.append(style)

    # Voice-specific constraints. These mirror the cascade prompt's spoken
    # register guidance (concise, no help-desk scaffolding, stay in
    # character) but are tuned for speech: filler is expensive when spoken.
    lines.append("")
    lines.append("Constraints:")
    lines.append(
        "- Stay in character as the persona above across the whole "
        "conversation."
    )
    lines.append(
        "- Speak naturally and concisely. Do not recite your capabilities or "
        "add help-desk scaffolding."
    )
    lines.append(
        "- Ground answers in the conversation and any provided context. When "
        "context does not cover a question, say so rather than inventing "
        "facts."
    )
    lines.append(
        "- A separate per-turn directive may set the mode, brevity, and angle "
        "for a given response. Follow it; it overrides these defaults where "
        "they conflict."
    )

    return "\n".join(lines)


def _format_grounding_fact(index: int, fact: GroundingFact) -> str:
    text = fact.text.strip()
    label_parts: list[str] = []
    domain = (fact.domain_name or "").strip()
    if domain:
        label_parts.append(domain)
    source = (fact.source or "").strip()
    if source:
        label_parts.append(source)
    label = f" [{' / '.join(label_parts)}]" if label_parts else ""
    return f"[{index}]{label} {text}"


def build_grounding_message(grounding: GroundingContext) -> str:
    """Render grounding facts into a single labeled context block.

    Returns ``""`` when there is nothing to inject so the caller can skip the
    injection entirely. Each fact is numbered and attributed to its domain /
    source so the model can ground (and later cite, #437) the right material.
    """
    if grounding.is_empty:
        return ""

    body_lines = [
        _format_grounding_fact(i, fact)
        for i, fact in enumerate(grounding.facts, start=1)
        if fact.text.strip()
    ]
    header = (
        "Relevant context for the current topic (from the space's knowledge "
        "and graph). Use it to ground your answer; cite the domain it came "
        "from when you rely on it:"
    )
    return "\n".join([header, *body_lines])


def build_grounding_items(grounding: GroundingContext) -> list[dict[str, object]]:
    """Build ``conversation.item`` payloads injecting grounding context.

    Returns Realtime ``conversation.item.create`` item dicts to push onto the
    session before triggering a response, so grounded answers do not depend
    solely on a tool round-trip. The grounding is injected as a ``system``
    message (out-of-band context, not something the user said).

    Returns ``[]`` when there is no grounding to inject.
    """
    message = build_grounding_message(grounding)
    if not message:
        return []
    return [
        {
            "type": "message",
            "role": "system",
            "content": [{"type": "input_text", "text": message}],
        }
    ]


@dataclass(frozen=True)
class RealtimeSessionPersona:
    """The resolved persona configuration for a Realtime session.

    Produced once at session build from the :class:`Persona`. ``instructions``
    is the static session-level instruction string; ``voice`` is the
    gpt-realtime voice id. Both are always populated (the persona falls back to
    a neutral default), so the executor can wire them unconditionally.
    """

    instructions: str
    voice: str


def build_session_persona(persona: Persona) -> RealtimeSessionPersona:
    """Resolve the static Realtime session persona (instructions + voice).

    The single entry point the executor calls. Composes the persona
    instructions and resolves the gpt-realtime voice in one step so the shared
    ``build_realtime_executor`` seam takes exactly one hook call.
    """
    return RealtimeSessionPersona(
        instructions=build_persona_instructions(persona),
        voice=resolve_realtime_voice(persona.canonical_voice),
    )
