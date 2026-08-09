// Table -- rows as a grid of sortable columns.
//
// Built against v1:cluster:node and v1:identity:user: the two concepts an
// operator most often wants side by side rather than one at a time. Neither
// is named here; the columns come from whatever scalar fields the rows carry,
// led by the concept's display-card slots so the most identifying column is
// leftmost.
//
// SORTING IS SPLIT. view-kit does the ordering (a pure function of rows +
// options.sort) and emits `data-vk-sort-field` / `data-vk-sort-dir` on the
// headers; the host attaches its one delegated listener, reads the attribute
// back, and re-renders with a new options.sort. That keeps the comparison
// rules -- numeric vs lexical, where empty cells go -- in one place for every
// consumer, without view-kit touching an event.

import { h, text, type VNode } from "./vnode.js";
import { rowDisplayId } from "./rowList.js";
import { compareScalars, isMissing, scalarText } from "./format.js";
import {
  boundFields,
  fitElement,
  profileConcept,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
} from "./fitness.js";
import type { ConceptLike, RowLike } from "./types.js";

export const TABLE_ELEMENT: ElementSpec = {
  id: "table",
  title: "Table",
  summary: "Every row as a line, every field as a sortable column.",
  requires: [
    {
      slot: "column",
      description: "the columns, one per field",
      kinds: ["text", "number", "boolean", "datetime"],
      min: 1,
      max: "all",
      // The display card orders the leading columns: whatever the concept
      // calls itself belongs at the left edge.
      prefer: ["primary", "secondary", "tertiary", "status"],
    },
  ],
  render: (input) => draw(input),
};

function sortRows(
  rows: readonly RowLike[],
  sort: ElementOptions["sort"],
): readonly RowLike[] {
  if (!sort) return rows;
  const direction = sort.direction === "desc" ? -1 : 1;
  return [...rows].sort((ra, rb) => {
    const a = ra[sort.field];
    const b = rb[sort.field];
    // Missing last in both directions -- see compareScalars' header.
    if (isMissing(a) || isMissing(b)) {
      return isMissing(a) && isMissing(b) ? 0 : isMissing(a) ? 1 : -1;
    }
    return direction * compareScalars(a, b);
  });
}

function draw({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const columns = boundFields(fit, "column");
  if (rows.length === 0 || columns.length === 0) {
    return h("div", { class: "vk-empty" }, [text(`No rows for ${concept.entity}.`)]);
  }

  const sort = options?.sort;
  const head = h(
    "tr",
    {},
    columns.map((field) => {
      const attrs: Record<string, string> = {
        class: "vk-table-head",
        "data-vk-sort-field": field,
        // A header is a sort control even before it is the active one, so the
        // host's delegated listener has something to match on every column.
        "aria-sort":
          sort?.field === field
            ? sort.direction === "desc"
              ? "descending"
              : "ascending"
            : "none",
      };
      if (sort?.field === field) attrs["data-vk-sort-dir"] = sort.direction;
      return h("th", attrs, [text(field)]);
    }),
  );

  const body = sortRows(rows, sort).map((row) => {
    const id = rowDisplayId(row);
    const attrs: Record<string, string> = { class: "vk-table-row", "data-row-id": id };
    if (options?.selectedRowId !== undefined && id === options.selectedRowId) {
      attrs["data-selected"] = "true";
    }
    return h(
      "tr",
      attrs,
      columns.map((field) =>
        h("td", { class: "vk-table-cell" }, [text(scalarText(row, field))]),
      ),
    );
  });

  return h("table", { class: "vk-table" }, [
    h("thead", {}, [head]),
    h("tbody", {}, body),
  ]);
}

export function renderTable(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(TABLE_ELEMENT, profileConcept(concept, rows), options);
  return draw({ rows, concept, fit, options });
}
