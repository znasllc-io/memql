"""Realtime session lifecycle + cost guardrails for multi-party rooms (#439).

Part of the Hybrid Realtime Voice epic (#440, Option B: memql is the
director, OpenAI ``gpt-realtime`` is the fast voice executor). This module
bounds a realtime speech-to-speech session so that an always-on model
listening to a polyphon room (up to 5 humans + 1 assistant) cannot burn
audio tokens unbounded. It is the #439 seam tagged in
``realtime_executor.build_realtime_executor``; the executor stays a pure
model factory and this module owns *when* a session lives, mutes, and dies.

Lifecycle decision (acceptance criterion 1)
-------------------------------------------
The issue lays out three lifecycle options:

* (a) session always warm but input-muted until the conductor engages;
* (b) spin up on first engagement, idle-timeout down;
* (c) a hybrid.

The chosen default is **(a) warm-but-muted**, because it composes directly
with the #432 conductor gate. The realtime model is already built with
``turn_detection=None`` (docs/voice/432-conductor-response-gate.md section
2.2, option A): the model never runs input VAD, never commits the input
buffer, and never self-creates a response. memql's conductor is the sole
driver of ``response.create``. That is *exactly* a warm-but-muted posture
already -- a session that has been built but not yet engaged sits with the
model loaded (low warm latency on the first real turn) while consuming no
audio-token cost for generated responses. This module makes the standing
cost of that warm session bounded rather than open-ended:

* **Empty-room teardown** -- when the last human leaves, the warm session
  is torn down immediately. A room with no humans has nobody to talk to;
  keeping the model warm for an empty room is pure waste (acceptance
  criterion 3: "no always-on full-audio session in a multi-listener room
  unless justified").
* **Idle teardown** -- if the conductor never engages the warm session
  within ``realtime_idle_timeout_sec``, it is torn down. A room where the
  assistant is only ever a listener should not hold a warm realtime
  session forever.
* **Max-duration teardown** -- a hard wall-clock backstop
  (``realtime_max_session_sec``) regardless of activity.
* **Token-budget teardown (acceptance criterion 2)** -- the running
  audio-token total is metered per response; crossing
  ``realtime_max_audio_tokens`` tears the session down and signals the
  caller to degrade to the cascade.

Degrade-to-cascade
------------------
Every teardown carries a :class:`TeardownReason`. When the reason is a
cost/idle/duration guardrail (not a normal room close), the lifecycle
exposes ``should_degrade_to_cascade`` so the caller can rebuild the
session on the proven cascade path -- the same "degrade to the cascade
when exceeded" posture the executor selection already uses on a realtime
build failure (``realtime_executor.select_voice_executor``). This module
does not itself rebuild the session; it decides *that* teardown should
happen and *why*, and invokes an injected ``stop`` callback. Wiring the
rebuild is the caller's job in ``main.py``.

Kill-switch composition
-----------------------
The platform-wide computer-use kill switch and per-session controls
(CLAUDE.md "Workers" / "CoPresent Control") already gate agent action at
the memql layer. This module's teardown path is the voice-side analog: a
single ``request_teardown(reason)`` entry point that any external control
signal (a kill-switch push, an operator stop, a session-end envelope) can
call to bring the realtime session down deterministically. The conductor
engaging / a response completing feed the activity clock; nothing here
reaches back into the model except via the injected ``stop`` callback, so
the module stays testable without a live realtime connection.

Out of scope (other epic issues, seams left untouched):

* tool bridge -> #435
* persona / grounding -> #436
* output capture -> #437
* avatar drive -> #438
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from enum import Enum

from voice_agent.config import Config

logger = logging.getLogger(__name__)


class TeardownReason(str, Enum):
    """Why a realtime session was (or is being) torn down.

    The ``*_GUARDRAIL`` / ``EMPTY_ROOM`` reasons are cost-driven and mark a
    session that should degrade to the cascade. ``ROOM_CLOSED`` is a normal
    end-of-call teardown that does NOT degrade -- the room is simply over.
    """

    #: The last human left the room. Cost-driven; degrade to cascade if the
    #: room re-populates (the caller decides whether a re-join rebuilds).
    EMPTY_ROOM = "empty_room"
    #: No conductor engagement within the idle window. Cost-driven.
    IDLE_TIMEOUT = "idle_timeout"
    #: Hard wall-clock cap reached. Cost-driven backstop.
    MAX_DURATION = "max_duration"
    #: Per-session audio-token budget exhausted. The cost guardrail.
    TOKEN_BUDGET = "token_budget"
    #: An external control signal (kill switch / operator stop / session
    #: end) requested teardown. Treated as a normal stop, not a degrade.
    EXTERNAL = "external"
    #: The room/call ended normally. Not cost-driven; no cascade degrade.
    ROOM_CLOSED = "room_closed"

    @property
    def is_cost_guardrail(self) -> bool:
        """True when this teardown is a cost/idle guardrail trip.

        These are the reasons the voice path should consider degrading to
        the cascade rather than simply ending: the realtime session was
        stopped to bound cost, not because the call is over.
        """
        return self in (
            TeardownReason.EMPTY_ROOM,
            TeardownReason.IDLE_TIMEOUT,
            TeardownReason.MAX_DURATION,
            TeardownReason.TOKEN_BUDGET,
        )


@dataclass(frozen=True)
class RealtimeBudget:
    """Resolved cost guardrails for one realtime session.

    Built from :class:`Config` via :meth:`from_config`. A ceiling of ``<= 0``
    means that particular guardrail is disabled; the others still apply. All
    durations are seconds; the token budget is total audio tokens
    (input + output) for the session.
    """

    idle_timeout_sec: int
    max_session_sec: int
    max_audio_tokens: int

    @classmethod
    def from_config(cls, cfg: Config) -> RealtimeBudget:
        return cls(
            idle_timeout_sec=cfg.realtime_idle_timeout_sec,
            max_session_sec=cfg.realtime_max_session_sec,
            max_audio_tokens=cfg.realtime_max_audio_tokens,
        )

    @property
    def idle_enabled(self) -> bool:
        return self.idle_timeout_sec > 0

    @property
    def duration_enabled(self) -> bool:
        return self.max_session_sec > 0

    @property
    def tokens_enabled(self) -> bool:
        return self.max_audio_tokens > 0


# A coroutine that actually stops the realtime session. Injected so the
# lifecycle is testable without a live model: in main.py this awaits the
# AgentSession teardown; in tests it records the call.
StopCallback = Callable[[TeardownReason], Awaitable[None]]

# A monotonic clock source, injectable so tests can advance time without
# sleeping. Defaults to time.monotonic.
Clock = Callable[[], float]


@dataclass
class _State:
    """Mutable lifecycle state, guarded by the manager's lock."""

    started_at: float
    last_engaged_at: float
    human_count: int = 0
    audio_tokens_used: int = 0
    torn_down: bool = False
    teardown_reason: TeardownReason | None = None


class RealtimeSessionLifecycle:
    """Owns the lifecycle of one realtime session in a multi-party room.

    Construction does NOT start the session -- the caller builds the model
    (``realtime_executor.build_realtime_executor``) and the
    ``AgentSession`` itself. This object is then started with :meth:`start`,
    which begins the warm session in the muted posture and arms the idle /
    duration watchdog. The caller feeds it room + activity events:

    * :meth:`note_human_joined` / :meth:`note_human_left` -- room
      participant lifecycle. The last human leaving triggers an
      empty-room teardown.
    * :meth:`note_engaged` -- the conductor drove a ``response.create``;
      resets the idle clock.
    * :meth:`note_audio_tokens` -- usage reported for a completed
      response; trips the token-budget guardrail when the running total
      crosses the ceiling.
    * :meth:`request_teardown` -- external stop (kill switch / operator /
      session end).

    Teardown is idempotent and single-shot: the first reason wins, the
    injected ``stop`` callback runs exactly once, and subsequent events are
    no-ops. After teardown, :attr:`should_degrade_to_cascade` tells the
    caller whether to rebuild on the cascade.
    """

    def __init__(
        self,
        *,
        budget: RealtimeBudget,
        stop: StopCallback,
        space_id: str = "",
        clock: Clock = time.monotonic,
        watchdog_interval_sec: float = 5.0,
    ) -> None:
        self._budget = budget
        self._stop = stop
        self._space_id = space_id
        self._clock = clock
        self._watchdog_interval = max(0.1, watchdog_interval_sec)
        self._lock = asyncio.Lock()
        self._state: _State | None = None
        self._watchdog: asyncio.Task[None] | None = None

    # -- lifecycle ---------------------------------------------------------

    async def start(self, *, human_count: int = 0) -> None:
        """Begin the warm session and arm the idle / duration watchdog.

        ``human_count`` seeds the room population (the entrypoint waits for
        at least one participant before building the session, so this is
        normally >= 1). Starting with zero humans is allowed but arms the
        empty-room teardown on the next watchdog tick.
        """
        now = self._clock()
        self._state = _State(
            started_at=now,
            last_engaged_at=now,
            human_count=human_count,
        )
        logger.info(
            "realtime lifecycle started space=%s humans=%d idle_timeout=%ds "
            "max_session=%ds token_budget=%d (warm-but-muted, #439)",
            self._space_id,
            human_count,
            self._budget.idle_timeout_sec,
            self._budget.max_session_sec,
            self._budget.max_audio_tokens,
        )
        if self._budget.idle_enabled or self._budget.duration_enabled:
            self._watchdog = asyncio.create_task(self._watchdog_loop())

    async def aclose(self) -> None:
        """Stop the watchdog without tearing the session down via a reason.

        For caller-side cleanup paths (the entrypoint ``finally`` block)
        that already own session teardown and just need the watchdog task
        cancelled. Does not invoke the ``stop`` callback.
        """
        await self._cancel_watchdog()

    # -- event inputs ------------------------------------------------------

    async def note_human_joined(self) -> None:
        """A human participant joined. Bumps the room population."""
        async with self._lock:
            if self._state is None or self._state.torn_down:
                return
            self._state.human_count += 1
            logger.debug(
                "realtime lifecycle human joined space=%s humans=%d",
                self._space_id,
                self._state.human_count,
            )

    async def note_human_left(self) -> None:
        """A human participant left.

        When the population reaches zero the session is torn down with
        :attr:`TeardownReason.EMPTY_ROOM` -- a room with no humans has
        nobody for the assistant to talk to, so the warm session is pure
        cost.
        """
        async with self._lock:
            if self._state is None or self._state.torn_down:
                return
            self._state.human_count = max(0, self._state.human_count - 1)
            logger.debug(
                "realtime lifecycle human left space=%s humans=%d",
                self._space_id,
                self._state.human_count,
            )
            if self._state.human_count == 0:
                await self._teardown_locked(TeardownReason.EMPTY_ROOM)

    async def note_engaged(self) -> None:
        """The conductor engaged the session (drove a ``response.create``).

        Resets the idle clock: an engaged session is doing useful work and
        should not be idle-reaped out from under an active conversation.
        """
        async with self._lock:
            if self._state is None or self._state.torn_down:
                return
            self._state.last_engaged_at = self._clock()
            logger.debug(
                "realtime lifecycle engaged space=%s -- idle clock reset",
                self._space_id,
            )

    async def note_audio_tokens(self, tokens: int) -> None:
        """Record audio-token usage reported for a completed response.

        ``tokens`` is the input+output audio-token count for one response
        (the realtime model reports usage per response). When the running
        session total crosses ``max_audio_tokens`` the session is torn down
        with :attr:`TeardownReason.TOKEN_BUDGET` and the caller should
        degrade to the cascade. Negative counts are clamped to zero.
        """
        async with self._lock:
            if self._state is None or self._state.torn_down:
                return
            self._state.audio_tokens_used += max(0, tokens)
            if (
                self._budget.tokens_enabled
                and self._state.audio_tokens_used >= self._budget.max_audio_tokens
            ):
                logger.warning(
                    "realtime audio-token budget exhausted space=%s used=%d "
                    "budget=%d -- tearing down + degrading to cascade (#439)",
                    self._space_id,
                    self._state.audio_tokens_used,
                    self._budget.max_audio_tokens,
                )
                await self._teardown_locked(TeardownReason.TOKEN_BUDGET)

    async def request_teardown(self, reason: TeardownReason = TeardownReason.EXTERNAL) -> None:
        """Externally request teardown (kill switch / operator / session end).

        Idempotent: the first teardown reason wins.
        """
        async with self._lock:
            await self._teardown_locked(reason)

    # -- watchdog ----------------------------------------------------------

    async def _watchdog_loop(self) -> None:
        """Poll for idle / max-duration expiry on a fixed interval.

        Polling (rather than one-shot timers) keeps the logic simple and
        robust to engagement events resetting the idle clock: each tick
        re-reads the current deltas against the live budget. The interval
        is small relative to the guardrail windows so the teardown fires
        within one tick of the deadline.
        """
        try:
            while True:
                await asyncio.sleep(self._watchdog_interval)
                async with self._lock:
                    if self._state is None or self._state.torn_down:
                        return
                    now = self._clock()
                    if (
                        self._budget.duration_enabled
                        and now - self._state.started_at >= self._budget.max_session_sec
                    ):
                        logger.warning(
                            "realtime max session duration reached space=%s "
                            "elapsed=%.0fs cap=%ds -- tearing down (#439)",
                            self._space_id,
                            now - self._state.started_at,
                            self._budget.max_session_sec,
                        )
                        await self._teardown_locked(TeardownReason.MAX_DURATION)
                        return
                    if (
                        self._budget.idle_enabled
                        and now - self._state.last_engaged_at
                        >= self._budget.idle_timeout_sec
                    ):
                        logger.warning(
                            "realtime session idle space=%s idle_for=%.0fs "
                            "timeout=%ds -- tearing down (#439)",
                            self._space_id,
                            now - self._state.last_engaged_at,
                            self._budget.idle_timeout_sec,
                        )
                        await self._teardown_locked(TeardownReason.IDLE_TIMEOUT)
                        return
        except asyncio.CancelledError:  # pragma: no cover - cancellation path
            raise

    async def _cancel_watchdog(self) -> None:
        task = self._watchdog
        self._watchdog = None
        if task is None:
            return
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass

    # -- teardown ----------------------------------------------------------

    async def _teardown_locked(self, reason: TeardownReason) -> None:
        """Single-shot teardown. Caller must hold ``self._lock``.

        Records the reason, invokes the injected ``stop`` callback exactly
        once, and cancels the watchdog. A failure in the stop callback is
        logged but does not unwind the torn-down state -- the session is
        considered down regardless so no event re-fires teardown.
        """
        if self._state is None or self._state.torn_down:
            return
        self._state.torn_down = True
        self._state.teardown_reason = reason
        logger.info(
            "realtime lifecycle teardown space=%s reason=%s degrade_to_cascade=%s "
            "tokens_used=%d (#439)",
            self._space_id,
            reason.value,
            reason.is_cost_guardrail,
            self._state.audio_tokens_used,
        )
        try:
            await self._stop(reason)
        except Exception:  # noqa: BLE001 -- stop failure must not wedge teardown
            logger.exception(
                "realtime lifecycle stop callback failed space=%s reason=%s",
                self._space_id,
                reason.value,
            )
        # Cancel the watchdog OUTSIDE the lock would risk re-entrancy with
        # _watchdog_loop awaiting the lock; instead null the handle here and
        # let aclose / the loop's own torn_down check stop it. The loop
        # returns on the next tick when it sees torn_down=True.
        if self._watchdog is not None and self._watchdog is not asyncio.current_task():
            self._watchdog.cancel()
            self._watchdog = None

    # -- introspection -----------------------------------------------------

    @property
    def is_torn_down(self) -> bool:
        return self._state is not None and self._state.torn_down

    @property
    def teardown_reason(self) -> TeardownReason | None:
        return self._state.teardown_reason if self._state is not None else None

    @property
    def should_degrade_to_cascade(self) -> bool:
        """True when the session was torn down by a cost guardrail.

        The caller reads this after teardown to decide whether to rebuild
        the voice path on the cascade (cost guardrail trip) or simply let
        the call end (normal room close / external stop).
        """
        reason = self.teardown_reason
        return reason is not None and reason.is_cost_guardrail

    @property
    def audio_tokens_used(self) -> int:
        return self._state.audio_tokens_used if self._state is not None else 0

    @property
    def human_count(self) -> int:
        return self._state.human_count if self._state is not None else 0


def build_realtime_lifecycle(
    cfg: Config,
    *,
    stop: StopCallback,
    space_id: str = "",
    clock: Clock = time.monotonic,
) -> RealtimeSessionLifecycle:
    """Construct a :class:`RealtimeSessionLifecycle` from runtime config.

    Convenience factory mirroring ``build_realtime_executor`` so ``main.py``
    wires the lifecycle in one call alongside the executor. The caller still
    invokes :meth:`RealtimeSessionLifecycle.start` once the session is up.
    """
    return RealtimeSessionLifecycle(
        budget=RealtimeBudget.from_config(cfg),
        stop=stop,
        space_id=space_id,
        clock=clock,
    )
