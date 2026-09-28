import { useEffect, useId, useRef, useState, type DragEvent } from "react";
import { Plus, X } from "lucide-react";

import { equip, moveSlot, removeSlot } from "./routeDraft";
import { ReadinessDot, SourceGlyph, readinessWords } from "./SourceGlyph";
import { placementProblem, readSource, servingIndex, trayGroups, type RoutingFacts } from "./sources";
import { sourceLabel } from "./vocabulary";

// THE COMPOSER: slots in a row, a tray of every source beneath them, and a thin
// line -- the circuit -- running from "Request" through the slots.
//
// ===========================================================================
// THE CIRCUIT IS THE ONE MEMORABLE THING, AND IT IS NEVER SIMULATED
// ===========================================================================
// The line is drawn in the accent up to the FIRST slot that is ready right now
// and that slot carries "Serves now"; after it the line is quiet. Which slot
// that is comes from `servingIndex` over real readiness (sources.ts) -- a
// machine online, an app signed in, a model offered, a vendor set up. When an
// edit makes the route connect or disconnect, the line changes once, in a CSS
// transition under 250ms, and under reduced motion it simply changes. No
// perpetual pulse, no sound, no score: the owner asked for the feeling of a
// lock opening, not a game.
//
// ===========================================================================
// EVERY DRAG HAS A CLICK AND A KEY
// ===========================================================================
//   equip     click a tray source: it goes in the CHOSEN slot, or at the end.
//             Drag one onto a slot or onto the trailing "+".
//   choose    click a slot, or Enter on it: the next source replaces it. Enter
//             again (or Escape) lets it go.
//   reorder   drag a slot; or focus it and use the arrow keys.
//   remove    the slot's small x; or Delete / Backspace on the slot.
//
// Choosing is deliberate, not focus: a person tabbing from the slots to the
// tray passes over every slot on the way, and a focus that chose would make
// "append" silently replace the last one.
//
// ===========================================================================
// A REFUSAL IS SAID WHERE IT HAPPENED
// ===========================================================================
// An embeddings-only model in a chat route, a duplicate, a route inside
// itself: the slot that was aimed at says why in one line, and the chain is
// not changed. `placementProblem` decides; this only draws it.

type Drag = { kind: "tray"; entry: string } | { kind: "slot"; from: number } | null;

export function SourceComposer({
  routeName,
  entries,
  onChange,
  facts,
  readOnly = false,
  onAnnounce,
  onAddMachine,
}: {
  /** The route's engine name, or "" for one not created yet. */
  routeName: string;
  entries: readonly string[];
  onChange: (next: string[]) => void;
  facts: RoutingFacts;
  readOnly?: boolean;
  onAnnounce?: (sentence: string) => void;
  /** Opens the add-a-machine flow; offered where no machine is connected. */
  onAddMachine?: () => void;
}) {
  const hint = useId();
  const [chosen, setChosen] = useState<number | null>(null);
  const [refusal, setRefusal] = useState<{ at: number; message: string } | null>(null);
  const drag = useRef<Drag>(null);
  const slotRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const appendRef = useRef<HTMLButtonElement | null>(null);
  const trayRef = useRef<HTMLDivElement | null>(null);
  const [focusAt, setFocusAt] = useState<number | null>(null);

  // FOCUS FOLLOWS THE SLOT IT WAS ON. A slot moved by the keyboard is a new
  // element in a new place, and a person who pressed an arrow expects to be
  // able to press it again.
  useEffect(() => {
    if (focusAt === null) return;
    (focusAt >= 0 ? slotRefs.current[focusAt] : appendRef.current)?.focus();
    setFocusAt(null);
  }, [focusAt, entries]);

  const readings = entries.map((entry) => readSource(entry, facts));
  const serving = servingIndex(entries, facts).index;
  const target = chosen !== null && chosen < entries.length ? chosen : entries.length;

  function place(entry: string, at: number) {
    const replacing = at < entries.length;
    const problem = placementProblem(entry, entries, routeName, facts, replacing ? at : undefined);
    if (problem !== "") {
      setRefusal({ at, message: problem });
      onAnnounce?.(problem);
      return;
    }
    setRefusal(null);
    setChosen(null);
    onChange(equip(entries, entry, at));
    onAnnounce?.(`${sourceLabel(entry)} is source ${at + 1}.`);
  }

  function move(from: number, to: number) {
    if (to < 0 || to >= entries.length || from === to) return;
    setRefusal(null);
    onChange(moveSlot(entries, from, to));
    if (chosen === from) setChosen(to);
    setFocusAt(to);
    onAnnounce?.(`${sourceLabel(entries[from]!)} moved to position ${to + 1}.`);
  }

  function remove(index: number) {
    const label = sourceLabel(entries[index]!);
    setRefusal(null);
    setChosen(null);
    onChange(removeSlot(entries, index));
    // The next slot takes its place under the cursor; with none left after it,
    // the one before; with none at all, the trailing "+".
    setFocusAt(entries.length - 1 === 0 ? -1 : Math.min(index, entries.length - 2));
    onAnnounce?.(`${label} removed.`);
  }

  function drop(at: number, event: DragEvent) {
    event.preventDefault();
    const held = drag.current;
    drag.current = null;
    if (held === null) return;
    if (held.kind === "slot") {
      move(held.from, Math.min(at, entries.length - 1));
      return;
    }
    place(held.entry, at);
  }

  const allowDrop = (event: DragEvent) => {
    if (readOnly || drag.current === null) return;
    event.preventDefault();
  };

  const groups = readOnly ? [] : trayGroups(facts, routeName);
  const noMachines = facts.machinesRead && facts.machines.length === 0;

  return (
    <div className="fleet-composer">
      <p id={hint} className="os-sr-only">
        Arrow keys move a source. Delete removes it. Enter chooses it, and the next source you pick replaces it.
      </p>
      <div className="fleet-circuit" data-connected={serving >= 0 || undefined}>
        <span className="fleet-circuit-origin">
          <span className="fleet-circuit-node" data-lit={serving >= 0 || undefined} aria-hidden />
          Request
        </span>
        <ol className="fleet-circuit-slots" aria-label="Sources, in the order they are tried">
          {readings.map((reading, i) => {
            const lit = serving >= 0 && i <= serving;
            return (
              <li
                key={`${i}:${reading.entry}`}
                className="fleet-circuit-step"
                data-lit={lit || undefined}
                onDragOver={allowDrop}
                onDrop={(event) => drop(i, event)}
              >
                <span className="fleet-circuit-link" data-lit={lit || undefined} aria-hidden />
                <div
                  className="fleet-slot"
                  data-ready={reading.readiness === "ready" || undefined}
                  data-serving={i === serving || undefined}
                  data-chosen={target === i || undefined}
                  data-refused={refusal?.at === i || undefined}
                >
                  <button
                    ref={(el) => {
                      slotRefs.current[i] = el;
                    }}
                    type="button"
                    className="fleet-slot-body"
                    draggable={!readOnly}
                    aria-pressed={readOnly ? undefined : target === i}
                    aria-describedby={readOnly ? undefined : hint}
                    aria-keyshortcuts={readOnly ? undefined : "ArrowLeft ArrowRight Delete Enter"}
                    aria-label={`${i + 1}. ${reading.label}, ${readinessWords(reading)}${i === serving ? ", serves now" : ""}`}
                    title={[reading.label, reading.readiness === "ready" ? reading.fact : readinessWords(reading)].filter(Boolean).join(" -- ")}
                    onClick={() => {
                      if (!readOnly) setChosen((held) => (held === i ? null : i));
                    }}
                    onKeyDown={(event) => {
                      if (readOnly) return;
                      if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
                        event.preventDefault();
                        move(i, i - 1);
                      } else if (event.key === "ArrowRight" || event.key === "ArrowDown") {
                        event.preventDefault();
                        move(i, i + 1);
                      } else if (event.key === "Delete" || event.key === "Backspace") {
                        event.preventDefault();
                        remove(i);
                      } else if (event.key === "Escape") {
                        setChosen(null);
                      }
                    }}
                    onDragStart={(event) => {
                      drag.current = { kind: "slot", from: i };
                      event.dataTransfer?.setData("text/plain", reading.entry);
                    }}
                    onDragEnd={() => {
                      drag.current = null;
                    }}
                  >
                    <span className="fleet-slot-glyph"><SourceGlyph kind={reading.kind} size={18} /></span>
                    <span className="fleet-slot-name">{reading.label}</span>
                    <span className="fleet-slot-fact">
                      <ReadinessDot readiness={reading.readiness} />
                      {reading.readiness === "ready" ? [reading.fact, "Ready"].filter(Boolean).join(" · ") : reading.readiness === "unknown" ? "Not checked" : "Not ready"}
                    </span>
                  </button>
                  {readOnly ? null : (
                    <button type="button" className="fleet-slot-remove" tabIndex={-1} aria-label={`Remove ${reading.label}`} title={`Remove ${reading.label}`} onClick={() => remove(i)}>
                      <X size={12} aria-hidden />
                    </button>
                  )}
                </div>
                {i === serving ? <span className="fleet-serves-now">Serves now</span> : null}
                {refusal?.at === i ? <span className="fleet-slot-refusal" role="alert">{refusal.message}</span> : null}
              </li>
            );
          })}
          {readOnly ? null : (
            <li className="fleet-circuit-step" onDragOver={allowDrop} onDrop={(event) => drop(entries.length, event)}>
              <span className="fleet-circuit-link" aria-hidden />
              <button
                ref={appendRef}
                type="button"
                className="fleet-slot fleet-slot-empty"
                data-refused={refusal?.at === entries.length || undefined}
                aria-label="Add a source at the end"
                onClick={() => {
                  setChosen(null);
                  trayRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
                }}
              >
                <Plus size={16} aria-hidden />
              </button>
              {refusal?.at === entries.length ? <span className="fleet-slot-refusal" role="alert">{refusal.message}</span> : null}
            </li>
          )}
        </ol>
      </div>

      {readOnly ? null : (
        <div className="fleet-tray" ref={trayRef} aria-label="Sources you can add">
          {groups.map((group) => (
            <div className="fleet-tray-group" key={group.id} role="group" aria-label={group.title}>
              <span className="fleet-tray-title">{group.title}</span>
              <div className="fleet-tray-sources">
                {group.sources.map((reading) => (
                  <button
                    key={reading.entry}
                    type="button"
                    className="fleet-tray-source"
                    data-ready={reading.readiness === "ready" || undefined}
                    draggable
                    aria-label={`${reading.label}: ${readinessWords(reading)}`}
                    title={reading.readiness === "ready" ? reading.fact || reading.label : readinessWords(reading)}
                    onClick={() => place(reading.entry, target)}
                    onDragStart={(event) => {
                      drag.current = { kind: "tray", entry: reading.entry };
                      event.dataTransfer?.setData("text/plain", reading.entry);
                    }}
                    onDragEnd={() => {
                      drag.current = null;
                    }}
                  >
                    <SourceGlyph kind={reading.kind} size={14} />
                    <span className="fleet-tray-name">{reading.label}</span>
                    <ReadinessDot readiness={reading.readiness} />
                  </button>
                ))}
                {group.id === "machines" && noMachines && onAddMachine ? (
                  <button type="button" className="fleet-reading-link" onClick={onAddMachine}>Add a machine</button>
                ) : null}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
