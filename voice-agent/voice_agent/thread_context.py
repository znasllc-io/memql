"""Tracks the Team-vs-Group thread state for a space.

Per the chat architecture: when only one human is active in a space,
voice + chat ride the Team thread (private to that user). When a
second human becomes active, voice migrates to the Group thread
(shared with all participants).

The voice-agent stamps the current thread on every VoiceAgentTurnRequest
so memql doesn't re-derive it. Phase 6 wires this to a memql
subscription on v1:cognition:participant.active changes; this scaffold
holds a per-session mutable cell that defaults to TEAM.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from enum import IntEnum


class Thread(IntEnum):
    """Matches VoiceAgentTurnRequest.ThreadContext on the proto side."""

    UNSPECIFIED = 0
    TEAM = 1
    GROUP = 2


@dataclass
class ThreadContext:
    """Per-session thread state. Mutable so a future memql subscription
    callback can flip it without recreating the LiveKit Agents session.
    """

    space_id: str
    thread: Thread = Thread.TEAM
    _lock: asyncio.Lock = asyncio.Lock()

    async def set(self, thread: Thread) -> None:
        async with self._lock:
            self.thread = thread

    async def get(self) -> Thread:
        async with self._lock:
            return self.thread
