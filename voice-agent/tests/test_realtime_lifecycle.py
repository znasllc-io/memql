"""Tests for the Realtime session lifecycle + cost guardrails (#439).

These cover the lifecycle decision surface without standing up a real
OpenAI Realtime session or a live LiveKit room:

* empty-room teardown -- the last human leaving stops the session,
* idle teardown -- no conductor engagement within the idle window stops it,
* max-duration teardown -- the hard wall-clock cap stops it,
* token-budget teardown -- the per-session audio-token cap stops it and
  marks the session for cascade degradation,
* engagement resets the idle clock,
* external (kill-switch / operator) teardown,
* normal room-close teardown does NOT degrade to the cascade,
* teardown is single-shot + idempotent (stop callback runs exactly once),
* disabled guardrails (<= 0) do not fire.

The realtime model and the LiveKit room are never constructed: the
lifecycle takes an injected ``stop`` coroutine and a fake monotonic clock,
so the guardrail logic is exercised deterministically and without sleeps
that depend on wall time.
"""

from __future__ import annotations

import asyncio

import pytest

from voice_agent.config import Config
from voice_agent.realtime_lifecycle import (
    RealtimeBudget,
    RealtimeSessionLifecycle,
    TeardownReason,
    build_realtime_lifecycle,
)


class _FakeClock:
    """A manually-advanced monotonic clock for deterministic timing."""

    def __init__(self) -> None:
        self._now = 1000.0

    def __call__(self) -> float:
        return self._now

    def advance(self, seconds: float) -> None:
        self._now += seconds


class _StopRecorder:
    """Records every stop-callback invocation."""

    def __init__(self) -> None:
        self.reasons: list[TeardownReason] = []

    async def __call__(self, reason: TeardownReason) -> None:
        self.reasons.append(reason)

    @property
    def call_count(self) -> int:
        return len(self.reasons)


def _budget(
    *,
    idle: int = 300,
    max_session: int = 1800,
    max_tokens: int = 1_000_000,
) -> RealtimeBudget:
    return RealtimeBudget(
        idle_timeout_sec=idle,
        max_session_sec=max_session,
        max_audio_tokens=max_tokens,
    )


async def _wait_for_teardown(
    lifecycle: RealtimeSessionLifecycle, *, timeout: float = 1.0
) -> None:
    """Poll until the lifecycle reports teardown or the timeout elapses.

    The watchdog runs on its own task; after advancing the fake clock we
    yield to the loop until it fires rather than guessing a sleep duration.
    """
    deadline = asyncio.get_event_loop().time() + timeout
    while not lifecycle.is_torn_down:
        if asyncio.get_event_loop().time() > deadline:
            raise AssertionError("lifecycle did not tear down within timeout")
        await asyncio.sleep(0.005)


# --- empty-room teardown ---


async def test_last_human_leaving_tears_down() -> None:
    stop = _StopRecorder()
    clock = _FakeClock()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0),  # only empty-room active
        stop=stop,
        clock=clock,
    )
    await lifecycle.start(human_count=2)
    assert lifecycle.human_count == 2

    await lifecycle.note_human_left()
    assert lifecycle.is_torn_down is False  # one human remains
    await lifecycle.note_human_left()

    assert lifecycle.is_torn_down is True
    assert lifecycle.teardown_reason is TeardownReason.EMPTY_ROOM
    assert lifecycle.should_degrade_to_cascade is True
    assert stop.reasons == [TeardownReason.EMPTY_ROOM]
    await lifecycle.aclose()


async def test_human_joined_then_left_balances() -> None:
    stop = _StopRecorder()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0), stop=stop, clock=_FakeClock()
    )
    await lifecycle.start(human_count=1)
    await lifecycle.note_human_joined()  # 2
    await lifecycle.note_human_left()  # 1
    assert lifecycle.is_torn_down is False
    await lifecycle.note_human_left()  # 0 -> teardown
    assert lifecycle.teardown_reason is TeardownReason.EMPTY_ROOM
    await lifecycle.aclose()


# --- idle teardown ---


async def test_idle_timeout_tears_down() -> None:
    stop = _StopRecorder()
    clock = _FakeClock()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=60, max_session=0),
        stop=stop,
        clock=clock,
        watchdog_interval_sec=0.01,
    )
    await lifecycle.start(human_count=1)
    clock.advance(61)  # past the idle window with no engagement
    await _wait_for_teardown(lifecycle)

    assert lifecycle.teardown_reason is TeardownReason.IDLE_TIMEOUT
    assert lifecycle.should_degrade_to_cascade is True
    assert stop.reasons == [TeardownReason.IDLE_TIMEOUT]
    await lifecycle.aclose()


async def test_engagement_resets_idle_clock() -> None:
    stop = _StopRecorder()
    clock = _FakeClock()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=60, max_session=0),
        stop=stop,
        clock=clock,
        watchdog_interval_sec=0.01,
    )
    await lifecycle.start(human_count=1)
    clock.advance(50)
    await lifecycle.note_engaged()  # reset before the 60s window elapses
    clock.advance(50)  # 50s since the engagement -- still under 60
    await asyncio.sleep(0.05)  # let the watchdog tick a few times
    assert lifecycle.is_torn_down is False

    clock.advance(11)  # now 61s since last engagement
    await _wait_for_teardown(lifecycle)
    assert lifecycle.teardown_reason is TeardownReason.IDLE_TIMEOUT
    await lifecycle.aclose()


# --- max-duration teardown ---


async def test_max_duration_tears_down() -> None:
    stop = _StopRecorder()
    clock = _FakeClock()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=120),
        stop=stop,
        clock=clock,
        watchdog_interval_sec=0.01,
    )
    await lifecycle.start(human_count=1)
    # Keep engaging so idle would never fire even if it were enabled --
    # duration must still trip.
    clock.advance(60)
    await lifecycle.note_engaged()
    clock.advance(61)  # 121s total wall clock
    await _wait_for_teardown(lifecycle)

    assert lifecycle.teardown_reason is TeardownReason.MAX_DURATION
    assert lifecycle.should_degrade_to_cascade is True
    await lifecycle.aclose()


# --- token-budget teardown (the cost guardrail) ---


async def test_token_budget_tears_down_and_degrades() -> None:
    stop = _StopRecorder()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0, max_tokens=1000),
        stop=stop,
        clock=_FakeClock(),
    )
    await lifecycle.start(human_count=1)
    await lifecycle.note_audio_tokens(400)
    assert lifecycle.is_torn_down is False
    await lifecycle.note_audio_tokens(400)
    assert lifecycle.is_torn_down is False
    assert lifecycle.audio_tokens_used == 800

    await lifecycle.note_audio_tokens(300)  # 1100 >= 1000
    assert lifecycle.is_torn_down is True
    assert lifecycle.teardown_reason is TeardownReason.TOKEN_BUDGET
    assert lifecycle.should_degrade_to_cascade is True
    assert stop.reasons == [TeardownReason.TOKEN_BUDGET]
    await lifecycle.aclose()


async def test_token_budget_exact_boundary_trips() -> None:
    stop = _StopRecorder()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0, max_tokens=1000),
        stop=stop,
        clock=_FakeClock(),
    )
    await lifecycle.start(human_count=1)
    await lifecycle.note_audio_tokens(1000)  # exactly at the ceiling
    assert lifecycle.teardown_reason is TeardownReason.TOKEN_BUDGET
    await lifecycle.aclose()


async def test_negative_token_counts_clamped() -> None:
    stop = _StopRecorder()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0, max_tokens=1000),
        stop=stop,
        clock=_FakeClock(),
    )
    await lifecycle.start(human_count=1)
    await lifecycle.note_audio_tokens(-500)
    assert lifecycle.audio_tokens_used == 0
    assert lifecycle.is_torn_down is False
    await lifecycle.aclose()


# --- external + normal teardown ---


async def test_external_teardown_does_not_degrade() -> None:
    stop = _StopRecorder()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(), stop=stop, clock=_FakeClock()
    )
    await lifecycle.start(human_count=1)
    await lifecycle.request_teardown()  # defaults to EXTERNAL
    assert lifecycle.teardown_reason is TeardownReason.EXTERNAL
    assert lifecycle.should_degrade_to_cascade is False
    assert stop.reasons == [TeardownReason.EXTERNAL]
    await lifecycle.aclose()


async def test_room_closed_teardown_does_not_degrade() -> None:
    stop = _StopRecorder()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(), stop=stop, clock=_FakeClock()
    )
    await lifecycle.start(human_count=1)
    await lifecycle.request_teardown(TeardownReason.ROOM_CLOSED)
    assert lifecycle.teardown_reason is TeardownReason.ROOM_CLOSED
    assert lifecycle.should_degrade_to_cascade is False
    await lifecycle.aclose()


# --- idempotency: single-shot teardown ---


async def test_teardown_is_single_shot() -> None:
    stop = _StopRecorder()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0, max_tokens=1000),
        stop=stop,
        clock=_FakeClock(),
    )
    await lifecycle.start(human_count=1)
    await lifecycle.note_audio_tokens(2000)  # trips token budget
    # Subsequent events must not re-fire the stop callback.
    await lifecycle.note_audio_tokens(5000)
    await lifecycle.request_teardown(TeardownReason.EXTERNAL)
    await lifecycle.note_human_left()

    assert stop.call_count == 1
    assert lifecycle.teardown_reason is TeardownReason.TOKEN_BUDGET  # first reason wins
    await lifecycle.aclose()


async def test_stop_callback_failure_does_not_wedge_teardown() -> None:
    async def _boom(_reason: TeardownReason) -> None:
        raise RuntimeError("stop failed")

    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0, max_tokens=10),
        stop=_boom,
        clock=_FakeClock(),
    )
    await lifecycle.start(human_count=1)
    # The stop callback raises, but the session is still marked torn down
    # so no event re-fires teardown.
    await lifecycle.note_audio_tokens(20)
    assert lifecycle.is_torn_down is True
    assert lifecycle.teardown_reason is TeardownReason.TOKEN_BUDGET
    await lifecycle.aclose()


# --- disabled guardrails ---


async def test_all_guardrails_disabled_never_tears_down_on_timers() -> None:
    stop = _StopRecorder()
    clock = _FakeClock()
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(idle=0, max_session=0, max_tokens=0),
        stop=stop,
        clock=clock,
        watchdog_interval_sec=0.01,
    )
    await lifecycle.start(human_count=1)
    clock.advance(100_000)
    await asyncio.sleep(0.05)
    await lifecycle.note_audio_tokens(10_000_000)  # token budget disabled
    assert lifecycle.is_torn_down is False
    # Empty-room teardown is independent of the timer/token guardrails.
    await lifecycle.note_human_left()
    assert lifecycle.teardown_reason is TeardownReason.EMPTY_ROOM
    await lifecycle.aclose()


# --- budget construction + factory ---


def _full_config() -> Config:
    return Config(
        livekit_url="ws://livekit:7880",
        livekit_api_key="devkey",
        livekit_api_secret="secret",
        deepgram_api_key="dg-key",
        voice_executor="realtime",
        openai_api_key="sk-test",
        realtime_model="gpt-realtime",
        realtime_idle_timeout_sec=45,
        realtime_max_session_sec=900,
        realtime_max_audio_tokens=50_000,
        memql_grpc_addr="bff:50051",
        voice_agent_token="token",
        avatar_vendor="none",
        anam_api_key=None,
        simli_api_key=None,
        anam_default_persona_id=None,
        anam_default_avatar_id=None,
        anam_default_persona_name="Assistant",
        log_level="INFO",
        dg_asr_model="nova-3",
        dg_tts_model="aura-2",
        dg_language="en",
        dg_endpointing_ms=2000,
        dg_utterance_end_ms=0,
    )


def test_budget_from_config_maps_fields() -> None:
    budget = RealtimeBudget.from_config(_full_config())
    assert budget.idle_timeout_sec == 45
    assert budget.max_session_sec == 900
    assert budget.max_audio_tokens == 50_000
    assert budget.idle_enabled is True
    assert budget.duration_enabled is True
    assert budget.tokens_enabled is True


def test_budget_disabled_flags() -> None:
    budget = RealtimeBudget(idle_timeout_sec=0, max_session_sec=0, max_audio_tokens=0)
    assert budget.idle_enabled is False
    assert budget.duration_enabled is False
    assert budget.tokens_enabled is False


async def test_factory_builds_lifecycle_from_config() -> None:
    stop = _StopRecorder()
    lifecycle = build_realtime_lifecycle(
        _full_config(), stop=stop, space_id="space-1", clock=_FakeClock()
    )
    assert isinstance(lifecycle, RealtimeSessionLifecycle)
    await lifecycle.start(human_count=1)
    await lifecycle.request_teardown(TeardownReason.ROOM_CLOSED)
    assert lifecycle.should_degrade_to_cascade is False
    await lifecycle.aclose()


def test_teardown_reason_cost_guardrail_classification() -> None:
    cost = {
        TeardownReason.EMPTY_ROOM,
        TeardownReason.IDLE_TIMEOUT,
        TeardownReason.MAX_DURATION,
        TeardownReason.TOKEN_BUDGET,
    }
    not_cost = {TeardownReason.EXTERNAL, TeardownReason.ROOM_CLOSED}
    for reason in cost:
        assert reason.is_cost_guardrail is True
    for reason in not_cost:
        assert reason.is_cost_guardrail is False


def test_introspection_before_start_is_safe() -> None:
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(), stop=_StopRecorder(), clock=_FakeClock()
    )
    assert lifecycle.is_torn_down is False
    assert lifecycle.teardown_reason is None
    assert lifecycle.should_degrade_to_cascade is False
    assert lifecycle.audio_tokens_used == 0
    assert lifecycle.human_count == 0


@pytest.mark.parametrize("interval", [-1.0, 0.0, 0.05])
def test_watchdog_interval_is_floored(interval: float) -> None:
    # Construction must never accept a non-positive watchdog interval (an
    # asyncio.sleep(0) busy-loop). The floor keeps the loop bounded.
    lifecycle = RealtimeSessionLifecycle(
        budget=_budget(),
        stop=_StopRecorder(),
        clock=_FakeClock(),
        watchdog_interval_sec=interval,
    )
    assert lifecycle._watchdog_interval >= 0.1
