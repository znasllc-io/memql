// Row-list rendering.
//
// The rule this file exists to enforce: there is NO concept-specific rendering
// code, anywhere. A row is projected through whatever @displayCard slots its
// concept declares, and through the stated fallback contract (displayCard.ts)
// when it declares none. That is what lets a newly declared concept render the
// day it is declared, with no renderer update.

import { h, text, type VNode } from "./vnode.js";
import { resolveDisplayCard, statusText, statusValue } from "./displayCard.js";
import type { ConceptLike, RowLike } from "./types.js";

// scalarField reads a row field and renders it as a display string. Non-scalar
// values (objects, arrays) are deliberately NOT stringified here -- a display
// card naming an object field is an authoring mistake, and showing
// "[object Object]" in a row label hides it. Returning empty makes the slot
// fall through to its fallback instead.
function scalarField(row: RowLike, field: string | undefined): string {
  if (!field) return "";
  const v = row[field];
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return "";
}

export function rowDisplayId(row: RowLike): string {
  const id = row["id"];
  return typeof id === "string" ? id : "";
}

export function renderRowList(
  rows: RowLike[],
  concept: ConceptLike,
  selectedRowId?: string,
): VNode {
  if (rows.length === 0) {
    return h("div", { class: "vk-empty" }, [
      text(`No rows for ${concept.entity}.`),
    ]);
  }

  // Resolved once for the whole set: a declared card verbatim, otherwise the
  // inferred one, so every row in the list labels itself off the same field.
  const card = resolveDisplayCard(concept, rows);
  const items = rows.map((row) => {
    const id = rowDisplayId(row);
    const attrs: Record<string, string> = { class: "vk-row", "data-row-id": id };
    if (selectedRowId !== undefined && id === selectedRowId) {
      attrs["data-selected"] = "true";
    }

    const children: VNode[] = [];

    // Primary falls back to the row id so a row is always clickable and always
    // identifiable, even with no display card at all.
    const primary = scalarField(row, card.primary) || id;
    children.push(h("span", { class: "vk-row-primary" }, [text(primary)]));

    const secondary = scalarField(row, card.secondary);
    if (secondary) {
      children.push(h("span", { class: "vk-row-secondary" }, [text(secondary)]));
    }

    const tertiary = scalarField(row, card.tertiary);
    if (tertiary) {
      children.push(h("span", { class: "vk-row-tertiary" }, [text(tertiary)]));
    }

    // The badge carries prose for the reader and the raw value for the host's
    // stylesheet -- see statusText / statusValue for why those differ.
    const statusRaw = card.status === undefined ? undefined : row[card.status];
    const status = card.status === undefined ? "" : statusText(card.status, statusRaw);
    if (status) {
      children.push(
        h("span", { class: "vk-row-status", "data-status": statusValue(statusRaw) }, [
          text(status),
        ]),
      );
    }

    return h("li", attrs, children);
  });

  return h("ul", { class: "vk-rows" }, items);
}
