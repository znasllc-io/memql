import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { rowIdOf } from "./useConceptRows";

// Rendering a row of a concept nobody designed a screen for.
//
// ===========================================================================
// WHY THIS IS HERE AND NOT IMPORTED
// ===========================================================================
// `@displayCard(primary=..., secondary=..., tertiary=..., status=...)` is the
// concept's own hint about which of its fields name a row. `sdk/ts-viewkit`
// renders it, and MemQL OS deliberately does not use that package: the OS
// renders React through its own kit, and view-kit renders HTML strings for
// the VS Code webviews. Pulling it in to read four field names would add a
// package dependency to get a lookup.
//
// So the HINT is honoured and the RENDERING is the OS's own, which is the
// same division every other OS surface makes.
//
// ===========================================================================
// THE FALLBACK IS NOT A GUESS ABOUT MEANING
// ===========================================================================
// A concept with no `@displayCard` gets its id as the primary line and
// nothing else invented. The tempting fallback -- pick `name`, then `title`,
// then `label` -- is a guess that reads as a fact: a row whose `name` field
// holds something that is not its name renders under a heading that is
// simply wrong, and nothing on the surface says it was inferred. An id is
// always true.

export interface RowCard {
  id: string;
  primary: string;
  secondary: string;
  tertiary: string;
  status: string;
  /** True when the concept declared no card and the id is standing in. */
  inferred: boolean;
}

function payloadOf(row: Row): Record<string, unknown> {
  const payload = row["payload"];
  if (payload !== null && typeof payload === "object" && !Array.isArray(payload)) {
    return payload as Record<string, unknown>;
  }
  return row;
}

/** A field's value as one line of text. Objects and arrays are summarised
 *  rather than stringified: `[object Object]` on a card is noise. */
export function fieldText(value: unknown): string {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) return value.length === 0 ? "" : `${value.length} items`;
  if (typeof value === "object") return "{ ... }";
  return "";
}

export function cardFor(concept: Concept, row: Row): RowCard {
  const payload = payloadOf(row);
  const id = rowIdOf(row);
  const card = concept.displayCard;

  if (!card || card.primary === "") {
    return { id, primary: id, secondary: "", tertiary: "", status: "", inferred: true };
  }

  const primary = fieldText(payload[card.primary]);
  return {
    id,
    // A declared primary whose value is empty on THIS row falls back to the
    // id rather than rendering a blank line: the card is how a person picks
    // a row out of a list, and a nameless entry is unpickable.
    primary: primary === "" ? id : primary,
    secondary: card.secondary ? fieldText(payload[card.secondary]) : "",
    tertiary: card.tertiary ? fieldText(payload[card.tertiary]) : "",
    status: card.status ? fieldText(payload[card.status]) : "",
    inferred: false,
  };
}
