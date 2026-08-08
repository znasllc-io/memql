// Checklist -- rows with a done/not-done flag.
//
// Built against dsl/todos/concepts.memql: title / done / dueAt / priority.
// The `done` slot prefers whatever the concept declared as its display-card
// status, which is how the to-do concept's `done` field is found without this
// file knowing the concept exists -- and how any other concept with a boolean
// lifecycle flag gets a checklist for free.
//
// The checkbox is a `role="checkbox"` span carrying aria-checked, not an
// <input>: view-kit emits no interactivity (see vnode.ts), and a real input
// would promise a click handler that is not there. The host binds the state
// change through data-row-id like every other element.

import { h, text, type VNode } from "./vnode.js";
import { rowDisplayId } from "./rowList.js";
import { booleanValue, formatDate, instantValue, scalarText } from "./format.js";
import { statusFieldLabel } from "./displayCard.js";
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

export const CHECKLIST_ELEMENT: ElementSpec = {
  id: "checklist",
  title: "Checklist",
  summary: "Rows as tickable items, open ones first.",
  requires: [
    {
      slot: "done",
      description: "whether an item is finished",
      kinds: ["boolean"],
      min: 1,
      prefer: ["status"],
      preferNames: ["done", "completed", "complete", "checked", "resolved", "finished"],
    },
    {
      slot: "label",
      description: "the text of each item",
      kinds: ["text"],
      min: 1,
      prefer: ["primary"],
    },
    {
      slot: "due",
      description: "each item's deadline",
      kinds: ["datetime"],
      min: 0,
      explicitOnly: true,
      preferNames: ["dueAt", "dueDate", "due", "deadline", "expiresAt", "scheduledAt"],
      degraded: "items show no deadline",
    },
    {
      slot: "group",
      description: "the heading items are grouped under",
      kinds: ["text"],
      min: 0,
      prefer: ["secondary"],
      distinctMax: CATEGORICAL_MAX_DISTINCT,
      preferFewestDistinct: true,
      degraded: "items are listed flat",
    },
  ],
  render: (input) => draw(input),
};

const UNGROUPED = "";

function draw({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const doneField = boundField(fit, "done");
  const labelField = boundField(fit, "label");
  const dueField = boundField(fit, "due");
  const groupField = boundField(fit, "group");

  if (rows.length === 0 || !doneField || !labelField) {
    return h("div", { class: "vk-empty" }, [
      text(`No checklist items for ${concept.entity}.`),
    ]);
  }

  // Group order is first-seen, so it follows whatever order the query
  // returned rather than an alphabetisation nobody asked for. Rows with no
  // group value collect under one unlabelled section at the end.
  const groups = new Map<string, RowLike[]>();
  for (const row of rows) {
    const key = groupField ? scalarText(row, groupField) : UNGROUPED;
    const list = groups.get(key) ?? [];
    list.push(row);
    groups.set(key, list);
  }
  const keys = [...groups.keys()].sort((a, b) =>
    a === UNGROUPED ? 1 : b === UNGROUPED ? -1 : 0,
  );

  const sections: VNode[] = [];
  for (const key of keys) {
    const items = groups.get(key)!;
    // Open before done, stable within each half: the point of a checklist is
    // what is left to do.
    const ordered = [
      ...items.filter((r) => booleanValue(r, doneField) !== true),
      ...items.filter((r) => booleanValue(r, doneField) === true),
    ];
    if (key !== UNGROUPED) {
      sections.push(h("li", { class: "vk-checklist-group" }, [text(key)]));
    }
    for (const row of ordered) {
      sections.push(item(row, { doneField, labelField, dueField, options }));
    }
  }

  // The count reads off the field's own name -- "5 of 12 done" for a `done`
  // field, "5 of 12 resolved" for a `resolved` one -- the same rule
  // statusText follows for a boolean badge (memql#3303): the label is the
  // field name, the value only decides the count.
  const closed = rows.filter((r) => booleanValue(r, doneField) === true).length;
  return h("div", { class: "vk-checklist" }, [
    h("div", { class: "vk-checklist-summary" }, [
      text(`${closed} of ${rows.length} ${statusFieldLabel(doneField)}`),
    ]),
    h("ul", { class: "vk-checklist-items" }, sections),
  ]);
}

function item(
  row: RowLike,
  ctx: {
    doneField: string;
    labelField: string;
    dueField?: string;
    options?: ElementOptions;
  },
): VNode {
  const id = rowDisplayId(row);
  const done = booleanValue(row, ctx.doneField) === true;
  const attrs: Record<string, string> = {
    class: "vk-checklist-item",
    "data-row-id": id,
    "data-vk-done": String(done),
  };
  if (ctx.options?.selectedRowId !== undefined && id === ctx.options.selectedRowId) {
    attrs["data-selected"] = "true";
  }

  const children: VNode[] = [
    h(
      "span",
      {
        class: "vk-checklist-box",
        role: "checkbox",
        "aria-checked": String(done),
      },
      [text(done ? "[x]" : "[ ]")],
    ),
    h("span", { class: "vk-checklist-label" }, [
      text(scalarText(row, ctx.labelField) || id),
    ]),
  ];
  const due = instantValue(row, ctx.dueField);
  if (due) {
    children.push(h("span", { class: "vk-checklist-due" }, [text(formatDate(due))]));
  }
  return h("li", attrs, children);
}

export function renderChecklist(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(CHECKLIST_ELEMENT, profileConcept(concept, rows), options);
  return draw({ rows, concept, fit, options });
}
