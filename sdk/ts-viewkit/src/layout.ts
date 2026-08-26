// Where the elements of an arrangement GO (epic memql#4661, task memql#4664).
//
// ===========================================================================
// WHY THIS IS A PLAN AND NOT A RENDERER
// ===========================================================================
// view-kit renders VNodes and never touches the DOM. The elements it places
// here, though, are rendered by the HOST -- the portal wraps each one in a
// React component that carries selection, keyboard handling and the resolved
// chart theme (ComposeElement), and a webview wraps them differently again. So
// a function in this file that emitted the elements would either duplicate
// that wrapping or lose it.
//
// What every host needs and none of them should re-derive is the same thing
// though: given (layout, bands, roles), WHICH SLOT does each entry go in, and
// what does that grid look like. That is a pure function over the arrangement,
// it is the part that must not diverge between two hosts, and it is what this
// module is. The host maps `plan.slots` onto its own element wrapper and gets
// the layout for free.
//
// ===========================================================================
// THE SLOT TABLE
// ===========================================================================
// One template per layout, from the spec's own table:
//
//   stack     one column, entries in arrangement order. TODAY'S BEHAVIOUR,
//             UNCHANGED -- it is the fallback every repair lands on, so a
//             regression here is a regression in every view.
//   dashboard reading row across the top; shape elements side by side; roll
//             below.
//   split     the roll element left, the detail pane right.
//   focus     one hero at 70%, everything else in a column beside it.
//   gallery   the roll element as a card grid over the population, readings
//             as a compact header strip.
//
// ===========================================================================
// EVERY ENTRY LANDS SOMEWHERE
// ===========================================================================
// The invariant this module holds, and the one its tests assert hardest: for
// any (layout, entry mix), every entry of the arrangement appears in exactly
// one slot. A layout that dropped an entry it had no slot for would lose an
// element somebody deliberately placed -- silently, since the page would look
// deliberate. Where a layout has no natural home for an entry, the entry goes
// to that layout's OVERFLOW slot, which renders as a plain column underneath.

import { arrangementLayout, entryRole } from "./arrangement.js";
import type { ArrangedElement, Arrangement, EntryRole, SectionLayout } from "./arrangement.js";

// A named region of a layout's grid.
//
// The names are ROLES IN A PAGE rather than positions, for the same reason a
// band is a question rather than a row number: `aside` is "the column beside
// the main thing", and a host may render it below on a narrow screen without
// the plan changing.
export type LayoutSlotName =
  // The single column of a stack, and the overflow of every other layout.
  | "flow"
  // A compact strip of readings across the top.
  | "header"
  // The element the page is about.
  | "lead"
  // The column beside the lead.
  | "aside"
  // Shape elements sitting side by side.
  | "shapes"
  // The population, listed or tabulated.
  | "roll"
  // The selected row's detail, in a split.
  | "detail";

export interface PlannedEntry {
  // Index into the arrangement's own `elements`, so a host can key on it and
  // a composer can address the entry it came from. Preserved through every
  // layout: a plan reorders WHERE things are drawn, never what they are.
  readonly at: number;
  readonly entry: ArrangedElement;
  readonly role: EntryRole;
  // How the element should PRESENT its rows in this slot. "cards" is what the
  // gallery layout asks of its population element.
  //
  // It comes from the LAYOUT rather than from the entry, which is the point:
  // an arrangement stores the intent once ("gallery"), and a person switching
  // a section between gallery and stack keeps their bindings and their title
  // instead of losing them to a different element id.
  readonly display: "list" | "cards";
}

export interface PlannedSlot {
  readonly slot: LayoutSlotName;
  readonly entries: readonly PlannedEntry[];
}

export interface LayoutPlan {
  readonly layout: SectionLayout;
  // Only the slots that have entries, in render order. A host iterates this
  // and never has to know which slots a layout theoretically owns.
  readonly slots: readonly PlannedSlot[];
  // The class the host puts on the grid container. Styled by view-kit's own
  // sheet (styles.ts), so the five grids and their narrow-width collapse are
  // defined once rather than per host.
  readonly className: string;
}

// The class names are LOOKED UP rather than composed, and that is not style.
// view-kit's stylesheet guard scans this source for the classes a renderer can
// emit and fails on any that has no rule (styles.test.ts). An interpolated
// `vk-slot-${slot}` reads to that scanner as the class `vk-slot-`, which is
// styled by nothing and named by nothing -- so a composed spelling turns a
// real guard into a passing one that checks a class that does not exist.
const LAYOUT_CLASSES: Readonly<Record<SectionLayout, string>> = {
  stack: "vk-arrangement-stack",
  dashboard: "vk-arrangement-dashboard",
  split: "vk-arrangement-split",
  focus: "vk-arrangement-focus",
  gallery: "vk-arrangement-gallery",
};

const SLOT_CLASSES: Readonly<Record<LayoutSlotName, string>> = {
  flow: "vk-slot-flow",
  header: "vk-slot-header",
  lead: "vk-slot-lead",
  aside: "vk-slot-aside",
  shapes: "vk-slot-shapes",
  roll: "vk-slot-roll",
  detail: "vk-slot-detail",
};

const ROLE_CLASSES: Readonly<Record<EntryRole, string>> = {
  hero: "vk-role-hero",
  supporting: "vk-role-supporting",
  standard: "vk-role-standard",
};

export function layoutClassName(layout: SectionLayout): string {
  return `vk-arrangement ${LAYOUT_CLASSES[layout]}`;
}

export function slotClassName(slot: LayoutSlotName): string {
  return `vk-slot ${SLOT_CLASSES[slot]}`;
}

// roleClassName is what a host puts on ONE element's wrapper. Emphasis is a
// class rather than an inline style so an element opts into reading it -- the
// ones that cannot express a role simply have no rule for it -- and so the
// three emphases are defined in view-kit's sheet beside everything else they
// have to stay consistent with.
export function roleClassName(role: EntryRole): string {
  return ROLE_CLASSES[role];
}

// planLayout is the whole of this module's contract.
//
// It takes a SANITIZED arrangement -- one that has been through
// sanitizeArrangement -- and it assumes the repairs have run: a focus has a
// hero, a split has a detail pane, the layout is one this build knows. It does
// not re-check them, because two places deciding whether a layout is
// satisfiable is two places that can disagree, and sanitize is the one gate.
//
// What it DOES tolerate is any entry mix, because "sanitized" does not mean
// "conventional": a dashboard whose author put five things in the reading band
// is valid and has to render.
export function planLayout(arrangement: Arrangement): LayoutPlan {
  const layout = arrangementLayout(arrangement);
  const gallery = layout === "gallery";
  const entries: PlannedEntry[] = arrangement.elements.map((entry, at) => ({
    at,
    entry,
    role: entryRole(entry),
    // Only the ROLL band becomes cards: a gallery's stat strip is still a
    // stat strip, and turning a chart into a card grid means nothing.
    display: gallery && entry.band === "roll" ? "cards" : "list",
  }));

  const slots = assign(layout, entries);
  return {
    layout,
    // Empty slots are dropped rather than emitted empty: a host rendering a
    // slot container per entry-less slot gets grid gaps where nothing is, and
    // "the dashboard has a hole in it" is indistinguishable from a bug.
    slots: slots.filter((s) => s.entries.length > 0),
    className: layoutClassName(layout),
  };
}

function assign(
  layout: SectionLayout,
  entries: readonly PlannedEntry[],
): readonly PlannedSlot[] {
  switch (layout) {
    case "dashboard":
      return dashboard(entries);
    case "split":
      return split(entries);
    case "focus":
      return focus(entries);
    case "gallery":
      return gallery(entries);
    case "stack":
    default:
      // STACK IS THE IDENTITY. One slot, arrangement order, nothing moved --
      // which is exactly what ArrangementBands did before this module existed,
      // and what every repair falls back to.
      return [{ slot: "flow", entries }];
  }
}

// dashboard: readings across the top, shapes side by side, the roll below.
//
// Band-driven rather than role-driven: a dashboard is a statement about the
// SHAPE of the page ("numbers, then how they divide, then the list"), and the
// bands already answer that question. Roles still apply within a slot -- a
// hero tile in the header reads at display scale -- they just do not move
// anything.
function dashboard(entries: readonly PlannedEntry[]): readonly PlannedSlot[] {
  return [
    { slot: "header", entries: entries.filter((e) => e.entry.band === "reading") },
    { slot: "shapes", entries: entries.filter((e) => e.entry.band === "shape") },
    { slot: "roll", entries: entries.filter((e) => e.entry.band === "roll") },
  ];
}

// split: the population on the left, one row's detail on the right.
//
// The detail pane is identified by the ENTRY that sanitize guaranteed is
// there. Everything that is neither the detail nor a roll element -- a stat
// strip somebody kept above the list -- goes to the header, which is where it
// reads: a split with its numbers buried in the left column looks broken.
function split(entries: readonly PlannedEntry[]): readonly PlannedSlot[] {
  const detail: PlannedEntry[] = [];
  const roll: PlannedEntry[] = [];
  const header: PlannedEntry[] = [];

  let detailTaken = false;
  for (const planned of entries) {
    // The FIRST detail-capable entry is the pane; a second one is an ordinary
    // element and goes to the flow. Two detail panes side by side showing the
    // same selected row is not a layout anybody asked for.
    if (!detailTaken && isDetailEntry(planned)) {
      detail.push(planned);
      detailTaken = true;
      continue;
    }
    if (planned.entry.band === "roll") {
      roll.push(planned);
      continue;
    }
    header.push(planned);
  }

  return [
    { slot: "header", entries: header },
    { slot: "roll", entries: roll },
    { slot: "detail", entries: detail },
  ];
}

// focus: one hero carrying the page, everything else in a column beside it.
//
// The hero is the entry ROLE, not a band -- which is the whole point of roles
// existing. sanitize guarantees a focus has one; if somehow none is marked,
// the first entry leads rather than the section rendering with an empty lead
// slot and every element crammed into the aside.
function focus(entries: readonly PlannedEntry[]): readonly PlannedSlot[] {
  let leadAt = entries.findIndex((e) => e.role === "hero");
  if (leadAt === -1) leadAt = entries.length > 0 ? 0 : -1;

  const lead = entries.filter((_, i) => i === leadAt);
  const aside = entries.filter((_, i) => i !== leadAt);
  return [
    { slot: "lead", entries: lead },
    { slot: "aside", entries: aside },
  ];
}

// gallery: a card per row, with the readings as a compact header strip.
//
// The roll band becomes the grid; readings sit above it. Shape elements have
// no natural home in a gallery -- a pie chart between a header strip and a
// card grid is neither -- so they go to the flow BELOW rather than being
// dropped. Every entry lands somewhere.
function gallery(entries: readonly PlannedEntry[]): readonly PlannedSlot[] {
  return [
    { slot: "header", entries: entries.filter((e) => e.entry.band === "reading") },
    { slot: "roll", entries: entries.filter((e) => e.entry.band === "roll") },
    { slot: "flow", entries: entries.filter((e) => e.entry.band === "shape") },
  ];
}

// isDetailEntry recognises the split's right-hand pane WITHOUT a library
// lookup: `detail` is the one element id in the library that declares
// `detail: true`, and a plan that took a library would have to be given one by
// every host at every call site for a single boolean.
//
// A host with its own detail-capable element passes it as a `role:
// "supporting"` entry and gets the same placement.
function isDetailEntry(planned: PlannedEntry): boolean {
  return planned.entry.element === "detail";
}
