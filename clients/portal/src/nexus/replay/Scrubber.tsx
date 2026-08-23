import type { ReactNode } from "react";

import { Button, DataText } from "../../ui";
import { PLAYBACK_SPEEDS } from "../map/motion";
import { BEFORE_FIRST, type PhaseMark } from "./timeline";

// The scrubber: a range input, the phase marks, and the transport.
//
// A NATIVE <input type="range"> rather than a drawn track, and that is the
// accessibility decision on this page. A range input already has keyboard
// travel (arrows, Home/End, PageUp/PageDown), an announced value, and a
// focus ring the whole product shares. A div with a pointer handler has none
// of those and has to reimplement all of them badly.
//
// The value is an INDEX into the event list, not a timestamp -- see
// timeline.ts on why the URL still carries the timestamp.

export function Scrubber({
  count,
  index,
  onIndex,
  playing,
  onPlaying,
  speed,
  onSpeed,
  marks,
  atLabel,
  live,
}: {
  count: number;
  index: number;
  onIndex: (next: number) => void;
  playing: boolean;
  onPlaying: (next: boolean) => void;
  speed: number;
  onSpeed: (next: number) => void;
  marks: readonly PhaseMark[];
  // The moment the position pins, already formatted. Empty before the first
  // event, which renders as "before the goal" rather than as a blank.
  atLabel: string;
  live: boolean;
}): ReactNode {
  const max = Math.max(0, count - 1);

  return (
    <div className="flex flex-col gap-2 rounded-lg border border-line bg-surface p-3">
      <div className="flex flex-wrap items-center gap-3">
        <Button
          size="xs"
          tone={playing ? "quiet" : "primary"}
          onClick={() => onPlaying(!playing)}
          disabled={count === 0}
          pressed={playing}
        >
          {playing ? "Pause" : "Play"}
        </Button>
        <div className="flex items-center gap-1" role="group" aria-label="Playback speed">
          {PLAYBACK_SPEEDS.map((option) => (
            <Button
              key={option}
              size="xs"
              tone={option === speed ? "primary" : "quiet"}
              pressed={option === speed}
              onClick={() => onSpeed(option)}
            >
              {option}x
            </Button>
          ))}
        </div>
        <Button size="xs" onClick={() => onIndex(BEFORE_FIRST)} disabled={count === 0}>
          Rewind
        </Button>
        <span className="ml-auto text-sm text-muted">
          {count === 0 ? (
            "No dated history"
          ) : live ? (
            <>live &middot; {count} events</>
          ) : (
            <>
              {index + 1} of {count} &middot; <DataText kind="time">{atLabel}</DataText>
            </>
          )}
        </span>
      </div>

      <input
        type="range"
        min={BEFORE_FIRST}
        max={max}
        step={1}
        value={Math.min(Math.max(index, BEFORE_FIRST), max)}
        onChange={(event) => onIndex(Number(event.target.value))}
        disabled={count === 0}
        aria-label="Replay position"
        // The value a screen reader announces is the moment, not the index --
        // "17" says nothing, "2026-08-20T09:14:00Z" is the fact.
        aria-valuetext={index < 0 ? "before the goal" : atLabel}
        className="w-full accent-accent"
      />

      {marks.length === 0 ? null : (
        <div className="relative h-4" aria-hidden="true">
          {marks.map((mark) => (
            <span
              key={mark.name}
              className="absolute -translate-x-1/2 text-[10px] text-subtle"
              style={{ left: `${max === 0 ? 0 : (mark.index / max) * 100}%` }}
            >
              {mark.name}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
