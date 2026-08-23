import { useEffect, useRef, type ReactNode } from "react";

import { Badge, DataText } from "../../ui";
import type { SceneEvent } from "../scene/events";

// The event list -- and it is not a sidebar, it is the map's accessible index.
//
// Design 4.4: "the Replay page's event list doubles as a linear, focusable
// index of every node (accessibility is structural here, not a second UI)".
// A WebGL canvas has no accessible tree: there is no element per node for a
// screen reader to reach, no tab order, no name. Building a second, hidden
// list to satisfy that would be a second representation to drift.
//
// So this IS the representation. Every node in the scene appears here, in
// time order, with its moment; Enter opens the node's detail at its own URL;
// the arrow keys move the scrubber. A person who never sees the picture can
// still read the whole goal and open anything in it.

export function EventList({
  events,
  index,
  onIndex,
  onOpen,
}: {
  events: readonly SceneEvent[];
  index: number;
  onIndex: (next: number) => void;
  onOpen: (event: SceneEvent) => void;
}): ReactNode {
  const listRef = useRef<HTMLOListElement>(null);

  // Keep the current position in view as playback advances. `nearest` rather
  // than `center` so a person who has scrolled to read something is not
  // yanked back to the middle on every tick.
  useEffect(() => {
    const node = listRef.current?.querySelector(`[data-event-index="${index}"]`);
    if (node instanceof HTMLElement) node.scrollIntoView({ block: "nearest" });
  }, [index]);

  if (events.length === 0) {
    return (
      <p className="p-3 text-sm text-muted">
        This goal recorded no dated history. Replay reads the rows' own timestamps and invents
        nothing, so a goal whose cluster did not stamp them has nothing to scrub.
      </p>
    );
  }

  return (
    <ol
      ref={listRef}
      // A LISTBOX rather than a list of buttons: exactly one position is
      // current at a time, which is what a listbox means and what makes the
      // arrow keys the expected control rather than a surprise.
      role="listbox"
      aria-label="Goal events"
      tabIndex={0}
      onKeyDown={(keyEvent) => {
        if (keyEvent.key === "ArrowDown" || keyEvent.key === "ArrowRight") {
          keyEvent.preventDefault();
          onIndex(index + 1);
        } else if (keyEvent.key === "ArrowUp" || keyEvent.key === "ArrowLeft") {
          keyEvent.preventDefault();
          onIndex(index - 1);
        } else if (keyEvent.key === "Home") {
          keyEvent.preventDefault();
          onIndex(-1);
        } else if (keyEvent.key === "End") {
          keyEvent.preventDefault();
          onIndex(events.length - 1);
        } else if (keyEvent.key === "Enter" || keyEvent.key === " ") {
          keyEvent.preventDefault();
          const current = events[Math.max(0, Math.min(index, events.length - 1))];
          if (current !== undefined) onOpen(current);
        }
      }}
      className="max-h-[62vh] min-h-40 overflow-y-auto rounded-lg border border-line bg-surface"
    >
      {events.map((event, i) => (
        <li
          key={event.id}
          data-event-index={i}
          role="option"
          aria-selected={i === index}
          onClick={() => onIndex(i)}
          onDoubleClick={() => onOpen(event)}
          className={
            "cursor-pointer border-b border-line px-3 py-1.5 text-sm last:border-b-0 " +
            (i === index ? "bg-raised" : i > index ? "text-subtle" : "")
          }
        >
          <div className="flex items-baseline gap-2">
            <DataText kind="time">{event.at}</DataText>
            <span className="min-w-0 flex-1 truncate">{event.label}</span>
            {event.attempt > 1 ? <Badge tone="warn">retry</Badge> : null}
          </div>
        </li>
      ))}
    </ol>
  );
}
