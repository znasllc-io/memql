import { useRef, type KeyboardEvent } from "react";

import { StopGlyph, stopStateSentence } from "../../../kit/Rail";
import type { RunStop } from "./stops";

// THE STOPS ACROSS THE TOP (design record D13, layout A: "stops across the top
// (A) over stops down the left (B)", chosen from two rendered mockups).
//
// The add-a-machine device applied to a run: the same marks the vertical rail
// draws, in the same states and colours (`.os-rail-stage[data-state]`), laid on
// one horizontal thread so a run reads left to right in the order the
// pipeline enforces. A stage's name sits under its mark, its state and time
// under that. The thread up to a finished stage carries the accent, so how far
// a run got is a fact of the drawing as well as of the words.
//
// THEY ARE TABS, because that is what they do: one stop is open, and the panel
// beneath shows its steps. So the row is a `tablist` -- arrow keys move along
// it, Home and End go to its ends, one tab stop for the whole row -- and the
// panel is the `tabpanel` it controls. Which stop is open is a SELECTION, drawn
// apart from a stop's state and from keyboard focus (SUPERVISED-VISUAL-
// COMPOSITION.md: "keep them distinct from selection and keyboard focus").

export function StopsAcross({
  stops,
  open,
  onOpen,
  label,
  panelId,
}: {
  stops: readonly RunStop[];
  open: string;
  onOpen: (stopId: string) => void;
  /** The row's accessible name. */
  label: string;
  /** The id of the panel the open stop's steps render in. */
  panelId: string;
}) {
  const refs = useRef<Map<string, HTMLButtonElement>>(new Map());

  function move(event: KeyboardEvent<HTMLButtonElement>, index: number) {
    let next = -1;
    if (event.key === "ArrowRight" || event.key === "ArrowDown") next = (index + 1) % stops.length;
    else if (event.key === "ArrowLeft" || event.key === "ArrowUp") next = (index - 1 + stops.length) % stops.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = stops.length - 1;
    if (next < 0) return;
    event.preventDefault();
    const target = stops[next]!;
    onOpen(target.id);
    refs.current.get(target.id)?.focus();
  }

  return (
    <ol className="os-rail pipeline-stops" role="tablist" aria-label={label} style={{ ["--pipeline-stops" as string]: String(Math.max(stops.length, 1)) }}>
      {stops.map((stop, index) => {
        const selected = stop.id === open;
        return (
          <li key={stop.id} className="os-rail-stage pipeline-stop" data-state={stop.state} data-open={selected ? "true" : undefined} role="presentation">
            <button
              ref={(el) => {
                if (el) refs.current.set(stop.id, el);
                else refs.current.delete(stop.id);
              }}
              type="button"
              role="tab"
              id={`${panelId}-tab-${stop.id}`}
              aria-selected={selected}
              aria-controls={panelId}
              tabIndex={selected ? 0 : -1}
              className="pipeline-stop-button"
              onClick={() => onOpen(stop.id)}
              onKeyDown={(e) => move(e, index)}
            >
              <span className="os-rail-mark" aria-hidden>
                <StopGlyph state={stop.state} size={11} />
              </span>
              <span className="pipeline-stop-name">{stop.name}</span>
              <span className="pipeline-stop-word">
                {stop.word}
                {stop.took !== "" ? <span className="pipeline-stop-took">{stop.took}</span> : null}
              </span>
              <span className="os-visually-hidden">{`, ${stopStateSentence(stop.state)}`}</span>
            </button>
          </li>
        );
      })}
    </ol>
  );
}
