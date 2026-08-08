// Kanban -- rows as cards in columns, one column per value of a grouping
// field.
//
// Built against v1:cluster:node, whose `health` enum (connecting / healthy /
// degraded / draining / offline / stopped) is the concept's declared
// display-card status. The grouping slot prefers that slot rather than any
// particular field name, so a board appears for any concept with a lifecycle
// field, and the same board renders a to-do by priority when the display card
// points there.
//
// Column order is first-seen: the query's order carries information (a status
// query usually returns the interesting state first) and alphabetising it
// would throw that away for no gain.

import { h, text, type VNode } from "./vnode.js";
import { rowDisplayId } from "./rowList.js";
import { scalarText } from "./format.js";
import { statusText, statusValue } from "./displayCard.js";
import {
  CATEGORICAL_MAX_DISTINCT,
  boundField,
  fitElement,
  profileConcept,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
} from "./fitness.js";
import type { ConceptLike, RowLike } from "./types.js";

export const KANBAN_ELEMENT: ElementSpec = {
  id: "kanban",
  title: "Board",
  summary: "Cards in columns, grouped by a status-like field.",
  requires: [
    {
      slot: "group",
      description: "the field the columns are grouped by",
      kinds: ["text", "boolean"],
      min: 1,
      prefer: ["status"],
      distinctMax: CATEGORICAL_MAX_DISTINCT,
      preferFewestDistinct: true,
    },
    {
      slot: "label",
      description: "the card title",
      kinds: ["text"],
      min: 0,
      prefer: ["primary"],
      explicitOnly: true,
      degraded: "cards are labelled by their id",
    },
    {
      slot: "detail",
      description: "a supporting line on each card",
      kinds: ["text"],
      min: 0,
      prefer: ["secondary", "tertiary"],
      explicitOnly: true,
      degraded: "cards show only their title",
    },
  ],
  render: (input) => draw(input),
};

function draw({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const groupField = boundField(fit, "group");
  const labelField = boundField(fit, "label");
  const detailField = boundField(fit, "detail");
  if (!groupField || rows.length === 0) {
    return h("div", { class: "vk-empty" }, [text(`No rows for ${concept.entity}.`)]);
  }

  // Keyed by the RAW value so a boolean column keeps its data-status, while
  // the heading goes through statusText -- "not active" rather than "false",
  // the same rule the row badge follows.
  const columns = new Map<string, { heading: string; rows: RowLike[] }>();
  const order: string[] = [];
  for (const row of rows) {
    const raw = row[groupField];
    const key = statusValue(raw);
    if (!columns.has(key)) {
      columns.set(key, {
        heading: statusText(groupField, raw) || `no ${groupField}`,
        rows: [],
      });
      order.push(key);
    }
    columns.get(key)!.rows.push(row);
  }

  return h(
    "div",
    { class: "vk-board", "data-vk-group-field": groupField },
    order.map((key) => {
      const column = columns.get(key)!;
      return h("div", { class: "vk-board-column", "data-status": key }, [
        h("div", { class: "vk-board-heading" }, [
          h("span", { class: "vk-board-heading-label" }, [text(column.heading)]),
          h("span", { class: "vk-board-count" }, [text(String(column.rows.length))]),
        ]),
        h(
          "ul",
          { class: "vk-board-cards" },
          column.rows.map((row) => card(row, labelField, detailField, options)),
        ),
      ]);
    }),
  );
}

function card(
  row: RowLike,
  labelField: string | undefined,
  detailField: string | undefined,
  options: ElementOptions | undefined,
): VNode {
  const id = rowDisplayId(row);
  const attrs: Record<string, string> = { class: "vk-board-card", "data-row-id": id };
  if (options?.selectedRowId !== undefined && id === options.selectedRowId) {
    attrs["data-selected"] = "true";
  }
  const children: VNode[] = [
    h("span", { class: "vk-board-card-label" }, [text(scalarText(row, labelField) || id)]),
  ];
  const detail = scalarText(row, detailField);
  if (detail) {
    children.push(h("span", { class: "vk-board-card-detail" }, [text(detail)]));
  }
  return h("li", attrs, children);
}

export function renderKanban(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(KANBAN_ELEMENT, profileConcept(concept, rows), options);
  return draw({ rows, concept, fit, options });
}
