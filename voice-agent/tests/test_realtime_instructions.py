"""Tests for persona + graph-grounding injection into the Realtime session (#436).

These cover the pure, deterministic surface that feeds the Realtime session
``instructions`` / ``voice`` and the per-turn grounding ``conversation.item``s,
without standing up a real Realtime session:

* canonical voice -> gpt-realtime voice mapping (gender-matched + fallback),
* persona fields (role / description / style) land in the instructions,
* a neutral default persona renders when the persona is unset,
* grounding facts are injected as system conversation items (and nothing is
  injected when there is no grounding).
"""

from __future__ import annotations

from voice_agent.persona_resolver import Persona
from voice_agent.realtime_instructions import (
    DEFAULT_PERSONA_NAME,
    DEFAULT_PERSONA_ROLE,
    DEFAULT_REALTIME_VOICE,
    OPENAI_REALTIME_VOICES,
    GroundingContext,
    GroundingFact,
    build_grounding_items,
    build_grounding_message,
    build_persona_instructions,
    build_session_persona,
    resolve_realtime_voice,
)


def _persona(
    *,
    canonical_voice: str = "alto",
    display_name: str = "",
    role: str = "",
    description: str = "",
    style: str = "",
) -> Persona:
    return Persona(
        canonical_voice=canonical_voice,
        tts_voice_id="aura-2-asteria-en",
        avatar_persona_id=None,
        avatar_vendor="",
        initial_audio_mode="always_on",
        initial_video_mode="always_off",
        display_name=display_name,
        role=role,
        description=description,
        style=style,
    )


# --- voice mapping ---


def test_female_canonical_maps_to_female_realtime_voice() -> None:
    assert resolve_realtime_voice("alto") == OPENAI_REALTIME_VOICES["alto"]
    assert resolve_realtime_voice("soprano") == OPENAI_REALTIME_VOICES["soprano"]


def test_male_canonical_maps_to_male_realtime_voice() -> None:
    assert resolve_realtime_voice("tenor") == OPENAI_REALTIME_VOICES["tenor"]
    assert resolve_realtime_voice("bass") == OPENAI_REALTIME_VOICES["bass"]


def test_voice_mapping_is_case_insensitive() -> None:
    assert resolve_realtime_voice("ALTO") == OPENAI_REALTIME_VOICES["alto"]


def test_unknown_voice_falls_back_to_default() -> None:
    assert resolve_realtime_voice("nonsense") == DEFAULT_REALTIME_VOICE
    assert resolve_realtime_voice("") == DEFAULT_REALTIME_VOICE


# --- persona instructions ---


def test_persona_fields_land_in_instructions() -> None:
    persona = _persona(
        display_name="Sofia",
        role="Tax Specialist",
        description="Helps with US tax filing questions.",
        style="Warm, precise, never condescending.",
    )
    text = build_persona_instructions(persona)
    assert "Sofia" in text
    assert "Tax Specialist" in text
    assert "Helps with US tax filing questions." in text
    assert "Warm, precise, never condescending." in text
    # Voice constraints are always present.
    assert "Constraints:" in text


def test_default_persona_when_unset() -> None:
    # No role / style stamped -> the neutral default identity renders, and the
    # instructions are still non-empty so the realtime voice stays on-task.
    text = build_persona_instructions(_persona())
    assert text.strip()
    assert DEFAULT_PERSONA_NAME in text
    assert DEFAULT_PERSONA_ROLE in text


def test_instructions_reference_per_turn_directive_composition() -> None:
    # The static persona must yield to the conductor's per-turn directive
    # (#432) -- the instructions say so explicitly so the two compose.
    text = build_persona_instructions(_persona(role="Analyst"))
    assert "per-turn directive" in text.lower()


def test_style_omitted_section_when_unset() -> None:
    text = build_persona_instructions(_persona(role="Analyst"))
    assert "Style and personality:" not in text


# --- session persona (instructions + voice) ---


def test_build_session_persona_combines_instructions_and_voice() -> None:
    persona = _persona(canonical_voice="tenor", role="Coach", display_name="Max")
    resolved = build_session_persona(persona)
    assert resolved.voice == OPENAI_REALTIME_VOICES["tenor"]
    assert "Max" in resolved.instructions
    assert "Coach" in resolved.instructions


# --- grounding injection ---


def test_grounding_items_inject_facts_as_system_message() -> None:
    grounding = GroundingContext(
        facts=(
            GroundingFact(
                text="Q3 revenue was 4.2M.",
                domain_name="Finance",
                source="q3-report",
            ),
            GroundingFact(text="Headcount grew 12% YoY.", domain_name="HR"),
        )
    )
    items = build_grounding_items(grounding)
    assert len(items) == 1
    item = items[0]
    assert item["role"] == "system"
    assert item["type"] == "message"
    content = item["content"]
    assert isinstance(content, list)
    text = content[0]["text"]
    assert "Q3 revenue was 4.2M." in text
    assert "Headcount grew 12% YoY." in text
    # Attribution is rendered so the model can ground / cite the domain.
    assert "Finance" in text
    assert "HR" in text


def test_empty_grounding_injects_nothing() -> None:
    assert build_grounding_items(GroundingContext()) == []
    assert build_grounding_message(GroundingContext()) == ""
    # Whitespace-only facts are treated as empty.
    blank = GroundingContext(facts=(GroundingFact(text="   "),))
    assert blank.is_empty is True
    assert build_grounding_items(blank) == []


def test_grounding_message_numbers_facts() -> None:
    grounding = GroundingContext(
        facts=(
            GroundingFact(text="First fact."),
            GroundingFact(text="Second fact."),
        )
    )
    message = build_grounding_message(grounding)
    assert "[1]" in message
    assert "[2]" in message
