// The scrubber's state machine, as arithmetic.
//
// Pure and separate from the components for the same reason the scene library
// is separate from the canvas: "does `?at=` round-trip", "does the scrub stop
// at the ends", "does the arrow key move one event" are claims about
// positions in a list, and asserting them through render() and fireEvent
// tests three layers to check one.
//
// ===========================================================================
// THE SCRUBBER MOVES OVER EVENTS, NOT OVER WALL-CLOCK TIME
// ===========================================================================
// A goal can span four seconds or nine hours, and a scrubber whose travel is
// proportional to elapsed time makes the second one useless -- forty events
// crammed into the first two pixels and an hour of nothing after them.
//
// So a position is an INDEX into the ordered event list, and playback is one
// event per tick rather than a clock. That is also what the prototype does
// (0.45s per arrival at 1x), and it is why the speeds are 1 / 4 / 16 rather
// than a time multiplier: they multiply ARRIVALS PER SECOND.
//
// The URL still carries a TIMESTAMP rather than an index (`?at=<rfc3339>`),
// because an index is meaningless to anyone but this list -- a link would
// break the moment one more event landed. `indexForAt` converts on the way
// in, `atForIndex` on the way out. A moment is a URL; a position is not.

import type { SceneEvent } from "../scene/events";

// The position before the first event -- the empty scene the goal starts
// from. A real value rather than a special case, so "rewind to the start"
// and "step back from event 0" land in the same place.
export const BEFORE_FIRST = -1;

export interface TimelineState {
  index: number;
  playing: boolean;
  speed: number;
}

export const INITIAL_TIMELINE: TimelineState = {
  // Live, at the end: opening Replay shows the goal as it stands and the
  // operator scrubs BACK. Starting at the beginning would make the default
  // view of a finished goal an empty scene.
  index: Number.POSITIVE_INFINITY,
  playing: false,
  speed: 1,
};

export function clampIndex(events: readonly SceneEvent[], index: number): number {
  if (events.length === 0) return BEFORE_FIRST;
  if (index < BEFORE_FIRST) return BEFORE_FIRST;
  if (index > events.length - 1) return events.length - 1;
  return index;
}

// indexForAt finds the last event at or before `at`.
//
// A LINEAR scan rather than a binary search, deliberately: an event list is
// hundreds long, this runs on navigation rather than per frame, and a binary
// search over a list whose ordering has a tie-break on four fields is a place
// to be subtly wrong for no measurable gain.
export function indexForAt(events: readonly SceneEvent[], at: string): number {
  const moment = at.trim();
  if (moment === "" || events.length === 0) return BEFORE_FIRST;
  let found = BEFORE_FIRST;
  for (let i = 0; i < events.length; i += 1) {
    if ((events[i]?.at ?? "") <= moment) found = i;
    else break;
  }
  return found;
}

// atForIndex is the inverse: the moment a position pins.
//
// BEFORE_FIRST has no moment -- there is no timestamp for "before anything
// happened" that is not made up -- so it returns "", and the caller drops
// `?at=` from the URL rather than writing a fabricated one.
export function atForIndex(events: readonly SceneEvent[], index: number): string {
  if (index < 0 || events.length === 0) return "";
  const clamped = clampIndex(events, index);
  return events[clamped]?.at ?? "";
}

// The moment the SCENE is rendered at, which is not the same as the moment
// the URL carries when the scrubber is live at the end. At the end the scene
// shows NOW (scene.ts's own sentinel), so a goal that is still running keeps
// moving rather than freezing at its last event.
export function sceneMomentFor(events: readonly SceneEvent[], index: number, live: boolean): string {
  if (live) return "";
  if (index < 0) {
    // Before the first event: a moment strictly earlier than everything, so
    // the scene filters to nothing. The first event's own timestamp would
    // INCLUDE that event, which is the off-by-one that makes a rewind stop
    // one frame short of empty.
    return "0000-01-01T00:00:00Z";
  }
  return atForIndex(events, index);
}

// isLive says whether the scrubber is parked at the end. Stored as a derived
// fact rather than a flag, because "the user dragged to the last event" and
// "the user has not touched the scrubber" have to behave identically -- both
// mean "show me the goal as it is".
export function isLive(events: readonly SceneEvent[], index: number): boolean {
  return events.length === 0 || index >= events.length - 1;
}

// The milliseconds between two arrivals at a given speed. Derived from the
// prototype's 0.45s (map/motion.ts) so the replay and the live map agree on
// what an arrival feels like.
export function stepMs(speed: number, arrivalSeconds: number): number {
  const safe = speed <= 0 ? 1 : speed;
  return Math.max(16, (arrivalSeconds * 1000) / safe);
}

// The phase boundaries, as positions on the scrubber. Marked so a reader can
// see the shape of the goal -- three phases with the middle one twice as long
// as the others is a fact about the work, visible in one glance and in no
// other way. A phase with no event of its own gets no mark rather than a
// mark in the wrong place.
export interface PhaseMark {
  name: string;
  index: number;
}

export function phaseMarks(
  events: readonly SceneEvent[],
  nodes: ReadonlyMap<string, { phase: string }>,
  phases: readonly string[],
): PhaseMark[] {
  const marks: PhaseMark[] = [];
  for (const name of phases) {
    if (name === "") continue;
    // The first event whose NODE belongs to the phase. Resolved through the
    // layout rather than by matching the event's label text: an event carries
    // no phase, and adding one would put a presentation concern into the
    // history. The layout already knows which column every node is in, and
    // that is the same answer the scrubber's tick is about.
    const index = events.findIndex((event) => nodes.get(event.nodeId)?.phase === name);
    if (index < 0) continue;
    marks.push({ name, index });
  }
  return marks;
}
