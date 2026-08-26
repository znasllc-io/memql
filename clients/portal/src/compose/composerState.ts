import {
  elementBand,
  elementById,
  proposeArrangement,
  sanitizeArrangement,
  type Arrangement,
  type ArrangedElement,
  type BandRole,
  type ConceptProfile,
  type EntryRole,
  type SectionLayout,
} from "@znasllc-io/memql-view-kit";

import type { SavedView, ViewOrigin } from "./savedViews";

// The composer's editable state, as a pure reducer.
//
// WHY A REDUCER AND NOT useState IN THE PAGE. Everything a person can do to a
// composed view -- add an element, drop one, move it, rebind a slot, accept a
// model's proposal, start over -- is a transition on one value, and the value
// is the thing that gets saved. Held as a reducer, the whole of "what the
// composer does" is a pure function that can be tested with no React, no
// cluster and no provider. That matters more here than usual, because the
// property the issue turns on ("a model is optional") is a property of these
// transitions: `seeded` and `applied` produce the same KIND of value, so
// nothing downstream can behave differently depending on which one ran.
//
// THE DRAFT IS PER-CONCEPT. A composed view may stack several concepts as
// independent sections (see the multi-concept note in ComposerPage), so the
// arrangements are keyed by concept id, and `conceptIds` holds the section
// ORDER that the keys cannot.

export interface ComposerDraft {
  readonly name: string;
  readonly description: string;
  readonly conceptIds: readonly string[];
  readonly arrangements: Readonly<Record<string, Arrangement>>;
  readonly origin: ViewOrigin;
  // The saved view being edited, or "" for a new one. Decides create vs
  // update at save time, and nothing else.
  readonly viewId: string;
  // Set once anything has been changed by hand, so a page can warn before
  // navigating away and so an accepted proposal that is then edited is still
  // recorded as having come from a model.
  readonly dirty: boolean;
}

// draftFromSuggestion opens a model's whole-view draft in the composer (epic
// memql#4661, task memql#4670).
//
// It is `emptyDraft` plus the sections the model chose, marked `suggested` --
// the SAME shape draftFromSavedView produces, which is the property that keeps
// the AI optional rather than structural. Nothing downstream can tell a
// described draft from a reopened one, because there is nothing to tell apart.
//
// The arrangements go in UNVALIDATED, exactly as a saved view's do: validation
// needs a profile and this function has none, so each section is repaired by
// `seeded` when its rows arrive. That is the same path, not a parallel one.
export function draftFromSuggestion(draft: {
  readonly name: string;
  readonly conceptIds: readonly string[];
  readonly arrangements: readonly Arrangement[];
}): ComposerDraft {
  const arrangements: Record<string, Arrangement> = {};
  for (const arrangement of draft.arrangements) {
    arrangements[arrangement.conceptId] = arrangement;
  }
  return {
    name: draft.name,
    description: "",
    conceptIds: [...draft.conceptIds],
    arrangements,
    origin: "suggested",
    viewId: "",
    // NOT dirty. Nothing has been changed by hand yet, and marking it so would
    // make "close without saving" warn about work nobody did.
    dirty: false,
  };
}

export function emptyDraft(conceptIds: readonly string[]): ComposerDraft {
  return {
    name: "",
    description: "",
    conceptIds: [...conceptIds],
    arrangements: {},
    origin: "manual",
    viewId: "",
    dirty: false,
  };
}

// draftFromSavedView reopens a stored view for editing. The arrangements come
// back as they were stored; they are re-validated per section as each
// section's rows load (see `seeded` below), not here, because validation needs
// a profile and this function has none.
export function draftFromSavedView(view: SavedView): ComposerDraft {
  const arrangements: Record<string, Arrangement> = {};
  for (const arrangement of view.arrangements) {
    arrangements[arrangement.conceptId] = arrangement;
  }
  return {
    name: view.name,
    description: view.description,
    conceptIds: [...view.conceptIds],
    arrangements,
    origin: view.origin,
    viewId: view.id,
    dirty: false,
  };
}

export type ComposerAction =
  // A section's rows arrived. Supplies the arrangement to start from: the
  // stored one repaired against the live rows when there is one, else the
  // deterministic proposal. Idempotent -- a section that already has an
  // arrangement is left alone, so a re-render does not undo an edit.
  | { kind: "seeded"; conceptId: string; profile: ConceptProfile }
  | { kind: "named"; name: string }
  | { kind: "described"; description: string }
  | { kind: "elementAdded"; conceptId: string; element: string; band?: BandRole }
  | { kind: "elementRemoved"; conceptId: string; at: number }
  | { kind: "elementMoved"; conceptId: string; at: number; by: 1 | -1 }
  // Move an entry to an absolute position. What a POINTER DRAG produces --
  // "this one goes there" -- where the buttons produced "this one goes one
  // place up". Both survive, because the buttons' behaviour is what the
  // keyboard path still needs: a drag is not operable from a keyboard, and
  // replacing the buttons without keeping their transition would have removed
  // reordering from anybody not using a mouse.
  | { kind: "elementReordered"; conceptId: string; from: number; to: number }
  // The section's layout. Absent on the arrangement means stack, so choosing
  // stack REMOVES the key rather than writing it -- otherwise a person who
  // tried a dashboard and went back would leave a row that is not equal to
  // the row they started with.
  | { kind: "layoutChosen"; conceptId: string; layout: SectionLayout }
  | { kind: "roleChosen"; conceptId: string; at: number; role: EntryRole }
  | { kind: "titled"; conceptId: string; at: number; title: string }
  | { kind: "slotBound"; conceptId: string; at: number; slot: string; fields: readonly string[] }
  | { kind: "slotCleared"; conceptId: string; at: number; slot: string }
  // A proposal was accepted. The arrangement has already been read and
  // repaired by view-kit's readArrangement, so what lands here is a value a
  // person could have built by hand.
  | { kind: "applied"; conceptId: string; arrangement: Arrangement }
  // Throw away this section's edits and go back to the deterministic match.
  | { kind: "reset"; conceptId: string; profile: ConceptProfile };

export function composerReducer(state: ComposerDraft, action: ComposerDraft | ComposerAction): ComposerDraft {
  if (!("kind" in action)) return action;

  switch (action.kind) {
    case "seeded": {
      if (state.arrangements[action.conceptId] !== undefined) return state;
      const stored = storedFor(state, action.conceptId);
      const arrangement =
        stored === undefined
          ? proposeArrangement(action.profile)
          : sanitizeArrangement(stored, action.profile);
      return withArrangement(state, action.conceptId, arrangement, false);
    }

    case "reset":
      return withArrangement(
        state,
        action.conceptId,
        proposeArrangement(action.profile),
        true,
      );

    case "applied":
      return withArrangement(state, action.conceptId, action.arrangement, true);

    case "named":
      return { ...state, name: action.name, dirty: true };

    case "described":
      return { ...state, description: action.description, dirty: true };

    case "elementAdded": {
      const current = state.arrangements[action.conceptId];
      if (current === undefined) return state;
      const spec = elementById(action.element);
      if (spec === undefined) return state;
      const band = action.band ?? elementBand(spec);
      // An element already in that band is not added twice. A person clicking
      // the same offer again means "yes, that one", not "two of them".
      if (current.elements.some((e) => e.element === action.element && e.band === band)) {
        return state;
      }
      return withArrangement(
        state,
        action.conceptId,
        { ...current, elements: [...current.elements, { element: action.element, band }] },
        true,
      );
    }

    case "elementRemoved": {
      const current = state.arrangements[action.conceptId];
      if (current === undefined) return state;
      const elements = current.elements.filter((_, i) => i !== action.at);
      if (elements.length === current.elements.length) return state;
      return withArrangement(state, action.conceptId, { ...current, elements }, true);
    }

    case "elementReordered": {
      const current = state.arrangements[action.conceptId];
      if (current === undefined) return state;
      const { from, to } = action;
      if (from === to) return state;
      if (from < 0 || from >= current.elements.length) return state;
      if (to < 0 || to >= current.elements.length) return state;
      const elements = [...current.elements];
      const moved = elements.splice(from, 1)[0];
      if (moved === undefined) return state;
      elements.splice(to, 0, moved);
      return withArrangement(state, action.conceptId, { ...current, elements }, true);
    }

    case "layoutChosen": {
      const current = state.arrangements[action.conceptId];
      if (current === undefined) return state;
      if (action.layout === "stack") {
        // REMOVED, not written. Absent means stack, so writing it would make
        // "I tried a dashboard and went back" produce a row that is not equal
        // to the one somebody started with -- and every stored row predates
        // layouts, so absent has to keep meaning what it always meant.
        const { layout: _dropped, ...rest } = current;
        return withArrangement(state, action.conceptId, rest, true);
      }
      return withArrangement(state, action.conceptId, { ...current, layout: action.layout }, true);
    }

    case "roleChosen":
      return editEntry(state, action.conceptId, action.at, (entry) => {
        // Same rule one level down: standard is what absence means.
        if (action.role === "standard") {
          const { role: _dropped, ...rest } = entry;
          return rest;
        }
        return { ...entry, role: action.role };
      });

    case "titled":
      return editEntry(state, action.conceptId, action.at, (entry) => {
        const trimmed = action.title.trim();
        if (trimmed === "") {
          // An empty caption is the ELEMENT's own title, which is a different
          // thing from a caption that is the empty string.
          const { title: _dropped, ...rest } = entry;
          return rest;
        }
        return { ...entry, title: trimmed };
      });

    case "elementMoved": {
      const current = state.arrangements[action.conceptId];
      if (current === undefined) return state;
      const to = action.at + action.by;
      if (action.at < 0 || action.at >= current.elements.length) return state;
      if (to < 0 || to >= current.elements.length) return state;
      const elements = [...current.elements];
      const moved = elements.splice(action.at, 1)[0];
      if (moved === undefined) return state;
      elements.splice(to, 0, moved);
      return withArrangement(state, action.conceptId, { ...current, elements }, true);
    }

    case "slotBound":
      return editEntry(state, action.conceptId, action.at, (entry) => ({
        ...entry,
        bindings: { ...(entry.bindings ?? {}), [action.slot]: [...action.fields] },
      }));

    case "slotCleared":
      // Removing the OVERRIDE, not declining the slot. Deleting the key hands
      // the slot back to the automatic resolution; binding it to [] is the
      // separate gesture that declines it, and `slotBound` with an empty list
      // is how a person makes that one.
      return editEntry(state, action.conceptId, action.at, (entry) => {
        if (entry.bindings === undefined) return entry;
        const bindings = { ...entry.bindings };
        delete bindings[action.slot];
        return Object.keys(bindings).length === 0
          ? stripBindings(entry)
          : { ...entry, bindings };
      });
  }
}

function stripBindings(entry: ArrangedElement): ArrangedElement {
  const { bindings: _dropped, ...rest } = entry;
  return rest;
}

function storedFor(state: ComposerDraft, conceptId: string): Arrangement | undefined {
  // The seed for a section is only ever the arrangement a SAVED view carried;
  // once `seeded` has run for a concept, `state.arrangements` holds it and the
  // action short-circuits. Kept as its own function so the intent is legible
  // at the call site.
  return state.arrangements[conceptId];
}

function withArrangement(
  state: ComposerDraft,
  conceptId: string,
  arrangement: Arrangement,
  dirty: boolean,
): ComposerDraft {
  return {
    ...state,
    arrangements: { ...state.arrangements, [conceptId]: arrangement },
    dirty: state.dirty || dirty,
  };
}

function editEntry(
  state: ComposerDraft,
  conceptId: string,
  at: number,
  edit: (entry: ArrangedElement) => ArrangedElement,
): ComposerDraft {
  const current = state.arrangements[conceptId];
  if (current === undefined) return state;
  if (at < 0 || at >= current.elements.length) return state;
  const elements = current.elements.map((entry, i) => (i === at ? edit(entry) : entry));
  return withArrangement(state, conceptId, { ...current, elements }, true);
}

// draftArrangements is the ordered list a save writes: one arrangement per
// selected concept, in the selection's order, skipping the sections whose rows
// have not loaded yet. A view saved before its third section loaded is a view
// with two sections -- honest, and re-openable.
export function draftArrangements(draft: ComposerDraft): Arrangement[] {
  const out: Arrangement[] = [];
  for (const conceptId of draft.conceptIds) {
    const arrangement = draft.arrangements[conceptId];
    if (arrangement !== undefined) out.push(arrangement);
  }
  return out;
}

// isSavable is the one gate on the save button. A name, and at least one
// section with at least one element -- anything less is not a view, and
// storing it would put a row in somebody's list that shows nothing.
export function isSavable(draft: ComposerDraft): boolean {
  if (draft.name.trim() === "") return false;
  return draftArrangements(draft).some((a) => a.elements.length > 0);
}
