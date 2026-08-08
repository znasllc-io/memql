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
import { renderDetail } from "./detail.js";
import { TABLE_ELEMENT } from "./table.js";
import { CALENDAR_ELEMENT } from "./calendar.js";
import { CHECKLIST_ELEMENT } from "./checklist.js";
import { TIMELINE_ELEMENT } from "./timeline.js";
import { STAT_TILE_ELEMENT } from "./statTile.js";
import { KANBAN_ELEMENT } from "./kanban.js";
import { MAP_ELEMENT } from "./map.js";
import { BAR_CHART_ELEMENT, LINE_CHART_ELEMENT, PIE_CHART_ELEMENT } from "./chart.js";
import { h, text } from "./vnode.js";
import type { ElementSpec } from "./fitness.js";

export const ROW_LIST_ELEMENT: ElementSpec = {
  id: "rowList",
  title: "List",
  summary: "One line per row, through the concept's display card.",
  requires: [],
  render: ({ rows, concept, options }) =>
    renderRowList([...rows], concept, options?.selectedRowId),
};

export const DETAIL_ELEMENT: ElementSpec = {
  id: "detail",
  title: "Record",
  summary: "One row's full nested shape.",
  requires: [],
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
      : renderDetail(row);
  },
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
  TABLE_ELEMENT,
  STAT_TILE_ELEMENT,
  ROW_LIST_ELEMENT,
  DETAIL_ELEMENT,
];

export function elementById(id: string): ElementSpec | undefined {
  return VIEW_KIT_ELEMENTS.find((element) => element.id === id);
}
