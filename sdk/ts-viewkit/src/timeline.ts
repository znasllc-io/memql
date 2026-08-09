// Timeline -- rows on a vertical time rail, newest first.
//
// Built against v1:deployment:deployment (a status timeline: one row per
// transition, updatedAt carrying the moment) and v1:identity:auditEvent (an
// append-only log keyed on occurredAt). Those two shapes are what the slot
// list below is derived from: an instant, a label, a supporting line, and a
// lifecycle badge.
//
// The badge goes through statusText / statusValue rather than printing the
// raw value, so a boolean status reads "not active" instead of "false" and a
// host colours it off data-status -- the same contract the row list uses.

import { h, text, type VNode } from "./vnode.js";
import { rowDisplayId } from "./rowList.js";
import { formatDate, formatTime, instantValue, scalarText } from "./format.js";
import { statusText, statusValue } from "./displayCard.js";
import {
  boundField,
  fitElement,
  profileConcept,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
} from "./fitness.js";
import type { ConceptLike, RowLike } from "./types.js";

export const TIMELINE_ELEMENT: ElementSpec = {
  id: "timeline",
  title: "Timeline",
  summary: "Rows in time order on a dated rail, newest first.",
  requires: [
    {
      slot: "at",
      description: "when each row happened",
      kinds: ["datetime"],
      min: 1,
      preferNames: [
        "occurredAt",
        "happenedAt",
        "timestamp",
        "updatedAt",
        "startsAt",
        "createdAt",
        "at",
        "time",
      ],
    },
    {
      slot: "label",
      description: "what to call each entry",
      kinds: ["text"],
      min: 0,
      prefer: ["primary"],
      explicitOnly: true,
      degraded: "entries are labelled by their id",
    },
    {
      slot: "detail",
      description: "a supporting line on each entry",
      kinds: ["text"],
      min: 0,
      prefer: ["secondary", "tertiary"],
      explicitOnly: true,
      degraded: "entries show only their label",
    },
    {
      slot: "status",
      description: "the outcome badge on each entry",
      kinds: ["text", "boolean"],
      min: 0,
      prefer: ["status"],
      explicitOnly: true,
      degraded: "entries carry no outcome badge",
    },
  ],
  render: (input) => draw(input),
};

function draw({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const atField = boundField(fit, "at");
  const labelField = boundField(fit, "label");
  const detailField = boundField(fit, "detail");
  const statusField = boundField(fit, "status");

  const dated = rows
    .map((row) => ({ row, at: instantValue(row, atField) }))
    .filter((e): e is { row: RowLike; at: Date } => e.at !== undefined)
    .sort((a, b) => b.at.getTime() - a.at.getTime());

  if (dated.length === 0) {
    return h("div", { class: "vk-empty" }, [
      text(`No dated rows for ${concept.entity}.`),
    ]);
  }

  const items: VNode[] = [];
  let currentDate = "";
  for (const { row, at } of dated) {
    const date = formatDate(at);
    if (date !== currentDate) {
      currentDate = date;
      items.push(h("li", { class: "vk-timeline-date" }, [text(date)]));
    }

    const id = rowDisplayId(row);
    const attrs: Record<string, string> = {
      class: "vk-timeline-entry",
      "data-row-id": id,
    };
    if (options?.selectedRowId !== undefined && id === options.selectedRowId) {
      attrs["data-selected"] = "true";
    }

    const children: VNode[] = [
      h("span", { class: "vk-timeline-time" }, [text(formatTime(at))]),
      // The rail dot. Decorative: the time beside it is the accessible
      // rendering of the same fact.
      h("span", { class: "vk-timeline-marker", "aria-hidden": "true" }, []),
      h("span", { class: "vk-timeline-label" }, [
        text(scalarText(row, labelField) || id),
      ]),
    ];
    const detail = scalarText(row, detailField);
    if (detail) {
      children.push(h("span", { class: "vk-timeline-detail" }, [text(detail)]));
    }
    if (statusField !== undefined) {
      const raw = row[statusField];
      const label = statusText(statusField, raw);
      if (label) {
        children.push(
          h("span", { class: "vk-row-status", "data-status": statusValue(raw) }, [
            text(label),
          ]),
        );
      }
    }
    items.push(h("li", attrs, children));
  }

  return h("ol", { class: "vk-timeline" }, items);
}

export function renderTimeline(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(TIMELINE_ELEMENT, profileConcept(concept, rows), options);
  return draw({ rows, concept, fit, options });
}
