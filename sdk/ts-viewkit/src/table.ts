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
import { cellAttrs, cellContent, cellKind, DISPLAY_COLUMN_CAP } from "./cell.js";
import { compareScalars, isMissing } from "./format.js";
import {
  boundFields,
  fitElement,
  profileConcept,
  type ConceptProfile,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
  type FieldProfile,
} from "./fitness.js";

import type { ConceptLike, RowLike } from "./types.js";

// Paragraph-shaped columns. Marked by FIELD NAME, not by the cell's string
// length: a one-word description is still a leftover-width column. Exact
// match, case-insensitive -- "notes" is long, "footnotes" is not.
const LONG_TABLE_FIELDS = new Set([
  "description",
  "personality",
  "notes",
  "note",
  "bio",
  "comment",
  "body",
  "prompt",
  "systemprompt",
  "message",
  "instructions",
  "summary",
]);

export function isLongTableField(field: string): boolean {
  return LONG_TABLE_FIELDS.has(field.toLowerCase());
}


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
      // CAPPED, since schema-first profiling made the field list COMPLETE and
      // complete is not the same as useful: a concept with forty declared
      // fields would render a forty-column table where the row-sampled
      // profile used to show the dozen the loaded rows happened to carry.
      //
      // autoMax rather than max, so the cap binds only the AUTOMATIC choice.
      // A person who names more columns in a binding gets all of them --
      // naming a slot settles it, and second-guessing an explicit choice is
      // what this must never do.
      autoMax: DISPLAY_COLUMN_CAP,
      // Among otherwise-equal candidates, a field the schema insists every
      // row carries comes before one that is elaboration.
      preferRequired: true,
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

function viewAction(id: string): VNode {
  return h(
    "button",
    {
      class: "vk-row-action",
      type: "button",
      "data-vk-row-action": "view",
      "data-vk-action-row-id": id,
    },
    [text("View")],
  );
}

// The per-column rendering rules, resolved ONCE for the whole table rather
// than per cell. A rule cannot vary between rows of one field -- it is derived
// from the field's profile, not from the value -- and deriving it per cell
// would be one profile lookup per cell for an answer that is constant down the
// column.
interface ColumnRules {
  readonly field: FieldProfile;
  readonly kind: ReturnType<typeof cellKind>;
}

function columnPlan(
  profile: ConceptProfile,
  columns: readonly string[],
): ReadonlyMap<string, ColumnRules> {
  const byName = new Map(profile.fields.map((f) => [f.field, f]));
  const out = new Map<string, ColumnRules>();
  for (const column of columns) {
    const field = byName.get(column);
    // A bound column with no profile entry is a caller override naming a
    // field nothing carries. It stays a column -- an override settles the
    // slot -- and renders plainly, which is what an unknown field is.
    if (field !== undefined) out.set(column, { field, kind: cellKind(field) });
  }
  return out;
}

function draw({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const columns = boundFields(fit, "column");
  if (rows.length === 0 || columns.length === 0) {
    return h("div", { class: "vk-empty" }, [text(`No rows for ${concept.entity}.`)]);
  }

  const plan = columnPlan(profileConcept(concept, rows), columns);

  const sort = options?.sort;
  const showView = options?.rowAction === "view";
  const headCells = columns.map((field) => {
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
  });
  if (showView) {
    headCells.push(h("th", { class: "vk-table-action-head" }, [text("")]));
  }
  const head = h("tr", {}, headCells);

  const body = sortRows(rows, sort).map((row) => {
    const id = rowDisplayId(row);
    const attrs: Record<string, string> = { class: "vk-table-row", "data-row-id": id };
    if (options?.selectedRowId !== undefined && id === options.selectedRowId) {
      attrs["data-selected"] = "true";
    }
    const cells = columns.map((field) => {
      const rules = plan.get(field);
      const cell: Record<string, string> = { class: "vk-table-cell" };
      if (isLongTableField(field)) cell["data-vk-cell"] = "long";
      else if (rules !== undefined) Object.assign(cell, cellAttrs(rules.kind));
      return h("td", cell, [
        rules === undefined
          ? text(String(row[field] ?? ""))
          : cellContent(row, rules.field, rules.kind, options?.resolveRef),
      ]);
    });
    if (showView) {
      cells.push(h("td", { class: "vk-table-cell vk-table-action" }, [viewAction(id)]));
    }
    return h("tr", attrs, cells);
  });

  return h("div", { class: "vk-table-wrap" }, [
    h("table", { class: "vk-table" }, [
      h("thead", {}, [head]),
      h("tbody", {}, body),
    ]),
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
