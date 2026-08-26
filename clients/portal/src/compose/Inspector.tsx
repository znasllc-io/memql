import { useCallback, useState, type Dispatch, type DragEvent, type ReactNode } from "react";
import {
  ENTRY_ROLES,
  LAYOUT_DESCRIPTIONS,
  SECTION_LAYOUTS,
  arrangementLayout,
  displayColumns,
  elementById,
  entryRole,
  type Arrangement,
  type ConceptProfile,
  type ElementCandidate,
  type EntryRole,
  type SectionLayout,
} from "@znasllc-io/memql-view-kit";

import { Button, Select } from "../ui";
import type { ComposerAction } from "./composerState";

// The composer's INSPECTOR (epic memql#4661, task memql#4670).
//
// ===========================================================================
// TWO PANES, AND WHICH HALF THIS IS
// ===========================================================================
// The composer is the live view on the left and this on the right. The split
// is not cosmetic: before it, every control lived ON the preview, which meant
// the thing a person was judging was covered in the buttons they were judging
// it with. What you see while composing is now what the saved row renders,
// because it is rendered by the same component with nothing added.
//
// ===========================================================================
// DRAG REPLACED THE BUTTONS, AND THE KEYBOARD PATH DID NOT GO WITH THEM
// ===========================================================================
// Reordering is a pointer drag now. The up/down BUTTONS are gone and the
// TRANSITION they dispatched is not: every entry is focusable and responds to
// the arrow keys, because a drag is not operable from a keyboard and removing
// the buttons without keeping the behaviour would have removed reordering from
// everybody not using a mouse. The reducer already had `elementMoved`; what is
// new is `elementReordered`, which is what a drop produces.
//
// ===========================================================================
// NO FREEFORM CANVAS (spec D7)
// ===========================================================================
// Position is BAND plus ORDER, and nothing here can express anything else. A
// saved arrangement therefore survives a redesign of the surface that renders
// it, which is the property the band grammar exists for -- pixel positions
// would pin every saved row to one release's layout.

export interface InspectorProps {
  conceptId: string;
  arrangement: Arrangement;
  profile: ConceptProfile;
  candidates: readonly ElementCandidate[];
  dispatch: Dispatch<ComposerAction>;
  // Which entry the inspector is on, and how to change it. Held by the page so
  // clicking an element in the PREVIEW selects it here.
  selected: number;
  onSelect: (index: number) => void;
  // "Suggest an arrangement" for this section.
  onSuggest: () => void;
  suggesting: boolean;
  onReset: () => void;
}

export function Inspector({
  conceptId,
  arrangement,
  profile,
  candidates,
  dispatch,
  selected,
  onSelect,
  onSuggest,
  suggesting,
  onReset,
}: InspectorProps): ReactNode {
  const [dragging, setDragging] = useState(-1);

  const onDrop = useCallback(
    (to: number) => {
      if (dragging === -1 || dragging === to) return;
      dispatch({ kind: "elementReordered", conceptId, from: dragging, to });
      onSelect(to);
      setDragging(-1);
    },
    [dragging, dispatch, conceptId, onSelect],
  );

  const layout = arrangementLayout(arrangement);
  const entry = arrangement.elements[selected];

  return (
    <aside className="flex w-full min-w-0 flex-col gap-5 xl:w-[22rem] xl:shrink-0">
      <section className="flex flex-col gap-2">
        <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">Layout</h3>
        {/* FIVE THUMBNAILS. A layout is a shape, and a person choosing one is
            choosing what the page should EMPHASISE -- which a name alone does
            not convey and a picture does. The sketches are pure CSS boxes, so
            they cost no assets and follow the theme. */}
        <div className="grid grid-cols-3 gap-2">
          {SECTION_LAYOUTS.map((one) => (
            <button
              key={one}
              type="button"
              onClick={() => dispatch({ kind: "layoutChosen", conceptId, layout: one })}
              aria-pressed={layout === one}
              title={LAYOUT_DESCRIPTIONS[one]}
              className={
                layout === one
                  ? "flex flex-col items-center gap-1 rounded border border-accent bg-accent-subtle p-1.5"
                  : "flex flex-col items-center gap-1 rounded border border-line p-1.5 hover:bg-raised"
              }
            >
              <LayoutSketch layout={one} />
              <span className="text-[10px] text-muted">{one}</span>
            </button>
          ))}
        </div>
        <p className="text-xs text-subtle">{LAYOUT_DESCRIPTIONS[layout]}</p>
      </section>

      <section className="flex flex-col gap-2">
        <div className="flex items-center justify-between gap-2">
          <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">Elements</h3>
          <span className="text-[10px] text-subtle">drag to reorder</span>
        </div>
        <ul className="flex flex-col gap-1">
          {arrangement.elements.map((one, index) => {
            const spec = elementById(one.element);
            return (
              <li key={`${index}:${one.element}`}>
                <div
                  role="button"
                  tabIndex={0}
                  draggable
                  aria-current={index === selected ? "true" : undefined}
                  onDragStart={() => setDragging(index)}
                  onDragOver={(event: DragEvent) => event.preventDefault()}
                  onDrop={() => onDrop(index)}
                  onDragEnd={() => setDragging(-1)}
                  onClick={() => onSelect(index)}
                  // THE KEYBOARD PATH. The up/down buttons are gone; what they
                  // dispatched is here, because a drag is not operable from a
                  // keyboard and reordering must not become mouse-only.
                  onKeyDown={(event) => {
                    if (event.key === "ArrowUp" || event.key === "ArrowDown") {
                      event.preventDefault();
                      const by = event.key === "ArrowUp" ? -1 : 1;
                      dispatch({ kind: "elementMoved", conceptId, at: index, by });
                      onSelect(Math.max(0, Math.min(arrangement.elements.length - 1, index + by)));
                      return;
                    }
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      onSelect(index);
                    }
                  }}
                  className={
                    index === selected
                      ? "flex cursor-grab items-center justify-between gap-2 rounded border border-accent bg-accent-subtle px-2 py-1 text-xs"
                      : "flex cursor-grab items-center justify-between gap-2 rounded border border-line px-2 py-1 text-xs hover:bg-raised"
                  }
                >
                  <span className="min-w-0 truncate">
                    {one.title ?? spec?.title ?? one.element}
                  </span>
                  <span className="shrink-0 text-[10px] text-subtle">{one.band}</span>
                </div>
              </li>
            );
          })}
        </ul>
        {arrangement.elements.length === 0 ? (
          <p className="text-xs text-subtle">Nothing here yet. Add one from what fits, below.</p>
        ) : null}
      </section>

      {entry === undefined ? null : (
        <section className="flex flex-col gap-3 rounded border border-line bg-surface p-3">
          <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">
            {elementById(entry.element)?.title ?? entry.element}
          </h3>

          <label className="flex flex-col gap-1 text-xs">
            <span className="text-muted">Caption</span>
            <input
              value={entry.title ?? ""}
              onChange={(e) =>
                dispatch({ kind: "titled", conceptId, at: selected, title: e.target.value })
              }
              placeholder={elementById(entry.element)?.title ?? ""}
              className="rounded border border-line bg-bg px-2 py-1 text-xs text-fg"
            />
          </label>

          <label className="flex flex-col gap-1 text-xs">
            <span className="text-muted">Emphasis</span>
            <Select
              value={entryRole(entry)}
              onChange={(next) =>
                dispatch({
                  kind: "roleChosen",
                  conceptId,
                  at: selected,
                  role: next as EntryRole,
                })
              }
              ariaLabel="Emphasis"
            >
              {ENTRY_ROLES.map((role) => (
                <option key={role} value={role}>
                  {role}
                </option>
              ))}
            </Select>
          </label>

          {/* SCHEMA-DRIVEN FIELD PICKERS. The options come from the concept's
              declared schema (epic memql#4661), so a field no loaded row
              happens to carry is still offerable -- which is exactly the case
              the row-sampled profile could not see. */}
          <FieldPickers
            conceptId={conceptId}
            entryIndex={selected}
            element={entry.element}
            bindings={entry.bindings}
            profile={profile}
            dispatch={dispatch}
          />

          <div className="flex justify-end">
            <Button
              size="xs"
              tone="danger"
              onClick={() => {
                dispatch({ kind: "elementRemoved", conceptId, at: selected });
                onSelect(Math.max(0, selected - 1));
              }}
            >
              Remove
            </Button>
          </div>
        </section>
      )}

      <section className="flex flex-col gap-2">
        <div className="flex items-center gap-2">
          <Button size="xs" onClick={onSuggest} disabled={suggesting}>
            {suggesting ? "Asking…" : "Suggest an arrangement"}
          </Button>
          <Button size="xs" onClick={onReset}>
            Start over
          </Button>
        </div>
        {/* THE FIT EXPLANATIONS STAY. Every element is listed, including the
            ones that cannot render these rows, because the question a person
            has is "why can't I pick the calendar" and the answer is view-kit's
            own sentence rather than an absence. */}
        <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">
          What fits {profile.concept.entity} ({candidates.filter((c) => c.usable).length} of{" "}
          {candidates.length})
        </h3>
        <ul className="flex flex-col gap-2">
          {candidates.map((candidate) => {
            const inView = arrangement.elements.some((e) => e.element === candidate.element.id);
            return (
              <li key={candidate.element.id} className="text-xs">
                <div className="flex items-baseline justify-between gap-2">
                  <span className={candidate.usable ? "text-fg" : "text-subtle"}>
                    {candidate.element.title}
                  </span>
                  {candidate.usable && !inView ? (
                    <Button
                      size="xs"
                      onClick={() =>
                        dispatch({
                          kind: "elementAdded",
                          conceptId,
                          element: candidate.element.id,
                        })
                      }
                    >
                      Add
                    </Button>
                  ) : (
                    // WHY THERE IS NO BUTTON, in two words. An offer with
                    // nothing beside it reads as a control that failed to
                    // render; "does not fit" is a state, and the sentence
                    // below says which requirement it missed.
                    <span className="shrink-0 text-[10px] text-subtle">
                      {inView ? "in this view" : "does not fit"}
                    </span>
                  )}
                </div>
                <p className="text-subtle">{candidate.explanation}</p>
              </li>
            );
          })}
        </ul>
      </section>
    </aside>
  );
}

// FieldPickers offers the concept's declared fields for each of the element's
// requirement slots.
//
// Only the slots the element DECLARES, and only the fields the concept
// declares: a picker over "every string on any row" is a picker nobody can use
// and is what the row-sampled profile could offer. An EMPTY choice is
// meaningful and is offered explicitly -- naming a slot with no fields is how
// a view declines it, which is how a stat strip asks for a row count and no
// totals.
function FieldPickers({
  conceptId,
  entryIndex,
  element,
  bindings,
  profile,
  dispatch,
}: {
  conceptId: string;
  entryIndex: number;
  element: string;
  bindings: Readonly<Record<string, readonly string[]>> | undefined;
  profile: ConceptProfile;
  dispatch: Dispatch<ComposerAction>;
}): ReactNode {
  const spec = elementById(element);
  if (spec === undefined || spec.requires.length === 0) return null;
  const fields = displayColumns(profile, 40);

  return (
    <div className="flex flex-col gap-2">
      {spec.requires.map((requirement) => {
        const bound = bindings?.[requirement.slot];
        const value = bound === undefined ? "" : (bound[0] ?? "__none");
        return (
          <label key={requirement.slot} className="flex flex-col gap-1 text-xs">
            <span className="text-muted" title={requirement.description}>
              {requirement.slot}
            </span>
            <Select
              value={value}
              onChange={(next) => {
                if (next === "") {
                  dispatch({ kind: "slotCleared", conceptId, at: entryIndex, slot: requirement.slot });
                  return;
                }
                dispatch({
                  kind: "slotBound",
                  conceptId,
                  at: entryIndex,
                  slot: requirement.slot,
                  fields: next === "__none" ? [] : [next],
                });
              }}
              ariaLabel={`${requirement.slot} field`}
            >
              <option value="">Whatever fits</option>
              <option value="__none">Nothing (decline this slot)</option>
              {fields
                .filter((field) => requirement.kinds.includes(field.kind))
                .map((field) => (
                  <option key={field.field} value={field.field}>
                    {field.field}
                  </option>
                ))}
            </Select>
          </label>
        );
      })}
    </div>
  );
}

// A five-box sketch of what each layout does with a page. Pure CSS, so it
// costs no assets and follows the theme; and it is a SHAPE rather than a name,
// which is the thing a person is actually choosing between.
function LayoutSketch({ layout }: { layout: SectionLayout }): ReactNode {
  const box = "rounded-[2px] bg-muted/50";
  switch (layout) {
    case "dashboard":
      return (
        <span className="flex h-6 w-8 flex-col gap-[2px]" aria-hidden="true">
          <span className={`${box} h-1 w-full`} />
          <span className="flex flex-1 gap-[2px]">
            <span className={`${box} w-1/2`} />
            <span className={`${box} w-1/2`} />
          </span>
          <span className={`${box} h-1.5 w-full`} />
        </span>
      );
    case "split":
      return (
        <span className="flex h-6 w-8 gap-[2px]" aria-hidden="true">
          <span className={`${box} w-3/5`} />
          <span className={`${box} w-2/5`} />
        </span>
      );
    case "focus":
      return (
        <span className="flex h-6 w-8 gap-[2px]" aria-hidden="true">
          <span className={`${box} w-[70%]`} />
          <span className="flex w-[30%] flex-col gap-[2px]">
            <span className={`${box} flex-1`} />
            <span className={`${box} flex-1`} />
          </span>
        </span>
      );
    case "gallery":
      return (
        <span className="grid h-6 w-8 grid-cols-3 gap-[2px]" aria-hidden="true">
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <span key={i} className={box} />
          ))}
        </span>
      );
    case "stack":
    default:
      return (
        <span className="flex h-6 w-8 flex-col gap-[2px]" aria-hidden="true">
          <span className={`${box} h-1 flex-none`} />
          <span className={`${box} flex-1`} />
          <span className={`${box} flex-1`} />
        </span>
      );
  }
}
