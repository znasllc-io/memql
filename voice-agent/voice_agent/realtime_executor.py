"""Pluggable Realtime voice executor behind the MemqlLLM seam (#434).

This module is the spine of the Hybrid Realtime Voice epic (#440,
Option B: memql is the director, OpenAI ``gpt-realtime`` is the fast
voice executor). It introduces the Realtime speech-to-speech model as
an alternative to the existing cascade (Deepgram STT -> memql cognition
-> Deepgram TTS) *behind the same seam* the cascade plugs into, and a
selection mechanism with a clean, automatic fallback to the cascade.

Selection model
---------------
``AgentSession`` in LiveKit Agents 1.5 accepts ``llm=...`` and
``turn_detection=...``. The cascade builds the session as
``AgentSession(vad, stt, llm=MemqlLLM(...), tts=build_tts(...))`` -- the
LLM emits text and the TTS speaks it. The realtime variant replaces the
``MemqlLLM`` + TTS *pair* with a single speech-to-speech
``RealtimeModel`` (the model both "thinks" and speaks), wired as
``AgentSession(..., llm=RealtimeModel(...), turn_detection=None)``.

``select_voice_executor`` returns a :class:`VoiceExecutorPlan` that tells
``main.py`` which session shape to build. The default is the cascade --
realtime is strictly opt-in via ``MEMQL_VOICE_EXECUTOR=realtime`` -- so
there is no regression to the shipped cascade path (epic #440 non-goal:
"No regression to the existing cascade voice path").

Conductor-gate posture (#432)
------------------------------
Per the merged spike ``docs/voice/432-conductor-response-gate.md``
section 2.2, the realtime session is created with ``turn_detection=None``
(the spike's RECOMMENDED **option A**). The model never runs input VAD,
never commits the input buffer on its own, and never auto-creates a
response: memql's conductor becomes the sole driver of
``response.create``. This stands up the session in the correct posture;
the conductor actually DRIVING ``response.create`` / ``response.cancel``
is a downstream issue (#432's integration steps), not #434.

The chosen defaults on the spikes' OPEN QUESTIONS:

* Interruption strategy (#432 open question 2): **option A**
  (conductor-driven ``response.cancel``), i.e. ``turn_detection=None``.
  Option B (server-VAD-for-interruption-only) is documented as a tuning
  lever but not the default, because in a polyphon room option B would
  let unrelated human cross-talk cut the assistant off.
* Audio routing (#433 open question 1): **active-speaker** audio (not a
  full mix), chosen for cleaner prosody + exact per-track attribution.
  The routing module itself is #433's deliverable; this executor only
  stands the session up in a posture compatible with it.

Out of scope (TODO seams below, owned by sibling issues):

* MCP tool bridge -> #435
* persona / graph grounding injection -> #436
* output capture / utterances / citations -> #437
* avatar drive -> #438
* session lifecycle / cost guardrails -> #439
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from enum import Enum
from typing import Any

from voice_agent.config import Config
from voice_agent.persona_resolver import Persona
from voice_agent.realtime_instructions import build_session_persona

logger = logging.getLogger(__name__)


# Selected option-A posture from docs/voice/432-conductor-response-gate.md
# section 2.2: the model never self-triggers. memql's conductor owns
# response.create / response.cancel. ``None`` disables the Realtime
# model's built-in turn detection entirely.
REALTIME_TURN_DETECTION: None = None


class ExecutorKind(str, Enum):
    """Which voice executor a session is built around."""

    CASCADE = "cascade"
    REALTIME = "realtime"


@dataclass(frozen=True)
class VoiceExecutorPlan:
    """The resolved decision of which executor to build a session with.

    ``kind`` is authoritative. When ``kind == REALTIME`` the caller wires
    ``realtime_model`` as the ``AgentSession.llm`` with
    ``turn_detection=None``; the cascade ``MemqlLLM`` + TTS are not built.
    When ``kind == CASCADE`` ``realtime_model`` is ``None`` and the caller
    builds the existing cascade session.

    ``fallback_reason`` is set only when realtime was requested but could
    not be stood up -- it explains, in one line, why the safe cascade
    default was chosen instead. It is ``None`` on the happy path of either
    executor.
    """

    kind: ExecutorKind
    realtime_model: Any | None = None
    fallback_reason: str | None = None

    @property
    def is_realtime(self) -> bool:
        return self.kind is ExecutorKind.REALTIME


class RealtimeExecutorError(RuntimeError):
    """Raised when the Realtime executor cannot be stood up.

    Carries enough context for ``select_voice_executor`` to log a clear
    fallback reason. Never escapes ``select_voice_executor`` -- it is
    caught there and collapsed into a cascade fallback.
    """


def build_realtime_executor(cfg: Config, persona: Persona) -> Any:
    """Build the OpenAI ``gpt-realtime`` model in the conductor-gate posture.

    Returns a ``livekit.plugins.openai.realtime.RealtimeModel`` configured
    with ``turn_detection=None`` (option A, #432). The plugin is imported
    lazily so the cascade path -- and the test suite, which does not
    install the OpenAI plugin -- never pays for it.

    Raises :class:`RealtimeExecutorError` on any failure (missing API
    key, plugin not installed, model construction error) so the caller
    can fall back to the cascade cleanly.
    """
    if not cfg.openai_api_key:
        raise RealtimeExecutorError(
            "OPENAI_API_KEY is unset -- cannot build the realtime executor"
        )

    try:
        # Lazy import: the openai plugin is only loaded on the realtime
        # path. Keeping it out of module scope means the cascade path and
        # the unit tests (which monkeypatch this function) do not require
        # livekit-plugins-openai to be importable.
        from livekit.plugins import openai  # type: ignore
    except Exception as err:  # noqa: BLE001 -- any import failure -> fallback
        raise RealtimeExecutorError(
            f"livekit-plugins-openai is not importable: {err}"
        ) from err

    # #436: resolve the static persona (instructions + gpt-realtime voice)
    # from the assistant's personaProfile. This is the realtime analog of
    # the cognition agent prompt -- identity + style + constraints, trimmed
    # so the conductor's per-turn directive (#432) composes on top. Grounding
    # conversation.items are built per-turn (build_grounding_items) by the
    # conductor wiring, not at session build. See realtime_instructions.py.
    session_persona = build_session_persona(persona)

    try:
        # turn_detection=None is the conductor-gate posture (option A,
        # docs/voice/432-conductor-response-gate.md section 2.2): the
        # model never self-triggers a response; memql's conductor will
        # drive response.create / response.cancel from a downstream issue.
        #
        # instructions + voice carry the static persona (#436). The
        # per-turn conductor directive (#432) layers on top via the
        # per-response instructions; grounding (#436) is injected as
        # conversation.items before a response is triggered.
        # MCP tool bridge (#435): the low-risk read-tool allowlist is
        #   exposed to this model and every model-driven call is mirrored
        #   into cognition by voice_agent.mcp_tool_bridge.
        #   wire_realtime_mcp_tools(...), called from main.py once the
        #   gRPC client + space/agent context exist. Privileged tools are
        #   never exposed here (default-deny). The model is built tool-less;
        #   the bridge registers tools post-construction.
        # TODO(#437): capture the model's output (transcript + audio) into
        #   utterances + citations. Not wired here.
        # TODO(#438): drive the avatar lip-sync from the realtime audio
        #   output. The cascade wires the avatar in main.py; the realtime
        #   output-audio -> avatar hookup is #438.
        # TODO(#439): session lifecycle + cost guardrails (max duration,
        #   token ceilings, kill-switch integration). Not wired here.
        realtime_model = openai.realtime.RealtimeModel(
            model=cfg.realtime_model,
            api_key=cfg.openai_api_key,
            turn_detection=REALTIME_TURN_DETECTION,
            instructions=session_persona.instructions,
            voice=session_persona.voice,
        )
    except Exception as err:  # noqa: BLE001 -- any build failure -> fallback
        raise RealtimeExecutorError(
            f"RealtimeModel construction failed: {err}"
        ) from err

    logger.info(
        "realtime executor built model=%s turn_detection=None "
        "(conductor-gate posture, #432 option A) canonical_voice=%s "
        "realtime_voice=%s persona_role=%s",
        cfg.realtime_model,
        persona.canonical_voice,
        session_persona.voice,
        persona.role or "default",
    )
    return realtime_model


def select_voice_executor(cfg: Config, persona: Persona) -> VoiceExecutorPlan:
    """Resolve which voice executor to build the session around.

    The default is the cascade (``MEMQL_VOICE_EXECUTOR`` unset or
    ``"cascade"``) -- realtime is strictly opt-in, so there is no
    regression risk for existing deployments.

    When realtime is requested, this builds the realtime model; if that
    fails for ANY reason (no OpenAI key, plugin missing, construction
    error) the failure is caught and the plan falls back to the cascade
    with a recorded ``fallback_reason``. The voice path therefore always
    comes up: a misconfigured realtime selection degrades to the proven
    cascade instead of failing the session.
    """
    if cfg.voice_executor != ExecutorKind.REALTIME.value:
        # Cascade is the default and the only other valid value (config
        # validation in load_config rejects anything else).
        return VoiceExecutorPlan(kind=ExecutorKind.CASCADE)

    try:
        realtime_model = build_realtime_executor(cfg, persona)
    except RealtimeExecutorError as err:
        logger.warning(
            "realtime executor requested but unavailable (%s) -- "
            "falling back to the cascade voice path",
            err,
        )
        return VoiceExecutorPlan(
            kind=ExecutorKind.CASCADE,
            fallback_reason=str(err),
        )
    except Exception as err:  # noqa: BLE001 -- never let a surprise abort voice
        # Defensive: build_realtime_executor wraps its failures in
        # RealtimeExecutorError, but a future change (or a plugin raising
        # something exotic at import side effects) must not take the voice
        # path down. Any escape collapses to the cascade.
        logger.exception(
            "unexpected error building the realtime executor -- "
            "falling back to the cascade voice path"
        )
        return VoiceExecutorPlan(
            kind=ExecutorKind.CASCADE,
            fallback_reason=f"unexpected error: {err}",
        )

    logger.info("voice executor selected: realtime (gpt-realtime, #434)")
    return VoiceExecutorPlan(
        kind=ExecutorKind.REALTIME,
        realtime_model=realtime_model,
    )
