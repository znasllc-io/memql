// The element library -- every way view-kit knows how to render a row set.
//
// A view (#3319 predefined, #3320 user-composed) is one or more of these
// bound to a concept. The library is a plain array so a view runtime can ask
// `fitElements(VIEW_KIT_ELEMENTS, profile)` and get a ranked, explained list
// of what will work for a concept nobody wrote code for.
//
// The row list and the detail pane are in here as elements with NO
// requirements: they render anything, which makes them the universal
// fallbacks -- and makes them sort last among equals, since fitElements ranks
// by how much of a concept's shape an element actually engaged with.

import { renderRowList } from "./rowList.js";
import { renderValueView } from "./valueView.js";
import { TABLE_ELEMENT } from "./table.js";
import { CALENDAR_ELEMENT } from "./calendar.js";
import { CHECKLIST_ELEMENT } from "./checklist.js";
import { TIMELINE_ELEMENT } from "./timeline.js";
import { STAT_TILE_ELEMENT } from "./statTile.js";
import { KANBAN_ELEMENT } from "./kanban.js";
import { MAP_ELEMENT } from "./map.js";
import {
  BAR_CHART_ELEMENT,
  LINE_CHART_ELEMENT,
  PIE_CHART_ELEMENT,
  PROPORTION_BAR_ELEMENT,
} from "./chart.js";
import { h, text } from "./vnode.js";
import type { ElementSpec } from "./fitness.js";

export const ROW_LIST_ELEMENT: ElementSpec = {
  id: "rowList",
  title: "List",
  summary: "One line per row, through the concept's display card.",
  requires: [],
  render: ({ rows, concept, options }) =>
    renderRowList([...rows], concept, options?.selectedRowId, options?.display ?? "list"),
};

export const DETAIL_ELEMENT: ElementSpec = {
  id: "detail",
  title: "Record",
  summary: "One row's full nested shape.",
  requires: [],
  // The split layout's right-hand pane, and the only element in the library
  // that is one: it renders the SELECTED row rather than the population.
  detail: true,
  // ...which is also why it cannot be a page's hero. A hero is the element
  // the page is about; this one is about whatever the page's hero is
  // currently pointing at, and promoting it inverts that.
  roles: ["supporting", "standard"],
  render: ({ rows, options }) => {
    // The selected row when there is one, otherwise the first: a detail pane
    // with no selection still has something honest to show.
    const selected =
      options?.selectedRowId === undefined
        ? undefined
        : rows.find((row) => row["id"] === options.selectedRowId);
    const row = selected ?? rows[0];
    return row === undefined
      ? h("div", { class: "vk-empty" }, [text("No row selected.")])
      : renderValueView(row);
  },
};

// ---------------------------------------------------------------------------
// The two HOSTED element kinds (epic memql#4661)
// ---------------------------------------------------------------------------
//
// A `scene` is a WebGL surface and a `widget` is an interactive portal
// component. Both are named here and rendered NOWHERE here, which is the whole
// design: view-kit must not import three.js (the portal's lazy-chunk rule,
// nexusMap.test.tsx) and has no React to mount a widget into. What view-kit
// owns is the GRAMMAR -- an arrangement can place one, sanitize can drop one
// that does not exist, and the fitness contract can rank one -- while the host
// supplies the renderer.
//
// The render function below is therefore not a stub awaiting an implementation.
// It is the correct answer for a host that registered no scenes: view-kit
// itself, a server-side render, a test. A host that has scenes never reaches
// it, because sanitize drops the entry before it renders and the host's own
// renderer runs first.
export const SCENE_ELEMENT_ID = "scene";
export const WIDGET_ELEMENT_ID = "widget";

export const SCENE_ELEMENT: ElementSpec = {
  id: SCENE_ELEMENT_ID,
  title: "Scene",
  summary: "A three-dimensional reading of this concept's rows.",
  // The natural hero: a scene is the element a page is ABOUT when it has one.
  band: "shape",
  requires: [],
  // Named by an arrangement, never discovered by the scan -- a scene is
  // nothing without a sceneId.
  placedOnly: true,
  // A scene has nothing to draw with no rows -- and unlike a table, an empty
  // one is not an honest empty state, it is a black rectangle.
  minRows: 1,
  render: () =>
    h("div", { class: "vk-empty" }, [
      text("This surface does not render scenes."),
    ]),
};

export const WIDGET_ELEMENT: ElementSpec = {
  id: WIDGET_ELEMENT_ID,
  title: "Control",
  summary: "A registered control from the host application.",
  band: "reading",
  requires: [],
  placedOnly: true,
  // A widget is a CONTROL, not a reading of the population, so it renders the
  // same whether the concept has rows or none. Zero is the honest minimum.
  minRows: 0,
  // Never a page's hero: a form is not what a page is about, it is what the
  // page lets you do. Promoting one to hero would give the page a big empty
  // box where its subject should be.
  roles: ["supporting", "standard"],
  render: () =>
    h("div", { class: "vk-empty" }, [
      text("This surface does not render controls."),
    ]),
};

// Declaration order is the last tiebreak in fitElements, so it reads
// most-specific first: an element that engaged with the concept's shape
// outranks one that will render anything.
export const VIEW_KIT_ELEMENTS: readonly ElementSpec[] = [
  CALENDAR_ELEMENT,
  CHECKLIST_ELEMENT,
  KANBAN_ELEMENT,
  TIMELINE_ELEMENT,
  MAP_ELEMENT,
  LINE_CHART_ELEMENT,
  BAR_CHART_ELEMENT,
  PIE_CHART_ELEMENT,
  PROPORTION_BAR_ELEMENT,
  TABLE_ELEMENT,
  STAT_TILE_ELEMENT,
  // The two hosted kinds sort BELOW every element that engaged with the
  // concept's shape, and deliberately below the universal fallbacks too: they
  // are placed by a person or a manifest naming a specific module, never
  // discovered by a scan. A scene that auto-proposed itself for every concept
  // would be a black rectangle on every page nobody asked for one on.
  SCENE_ELEMENT,
  WIDGET_ELEMENT,
  ROW_LIST_ELEMENT,
  DETAIL_ELEMENT,
];

export function elementById(id: string): ElementSpec | undefined {
  return VIEW_KIT_ELEMENTS.find((element) => element.id === id);
}
