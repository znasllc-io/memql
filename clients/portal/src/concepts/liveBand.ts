// The CDC merge policy: what happens to a row that appears WHILE you are
// paging through the concept it belongs to.
//
// ===========================================================================
// THE POLICY, and the reasoning it rests on. Read this before "just splicing
// the new row into the list".
// ===========================================================================
//
// Live rows accumulate in a SEPARATE BAND -- "new since you opened this" --
// above the paged window. They are never inserted into the paged rows, and
// they never touch the cursor.
//
// WHY NOT SPLICE. The walk is ordered `createdAt ASC, id ASC` and the cursor
// encodes a position in exactly that ordering (see browseConceptPage). A row
// created right now sorts AFTER everything the walk has already returned, so
// it is a row the walk HAS NOT REACHED YET. Splicing it into the rendered
// list does not save the walk from returning it later; it guarantees a
// duplicate the moment paging catches up. The alternatives are worse:
//
//   * Splice, then suppress it when the walk reaches it. That is
//     re-implementing the server's ordering and dedup in the browser, on a
//     cursor whose ordering is deliberately opaque. It also silently changes
//     what "page 7" means.
//   * Splice and restart the walk. Throws away every page already fetched
//     and re-fetches them, on every insert, for a concept that may be taking
//     writes continuously.
//   * Ignore the event. Honest but useless -- the console then goes quiet
//     while the system is busy, which is the failure an operations console
//     exists to not have.
//
// The band keeps all three properties: the paged window stays exactly what
// the cursor walk returned (no gaps, no duplicates, cursor untouched), the
// operator sees liveness immediately, and "these arrived after you loaded
// this" is stated rather than implied by a row quietly appearing in the
// middle of a list they were reading.
//
// UPDATES AND DELETES are counted, not applied. An update to a row in the
// paged window would have to be matched against a projection the browser did
// not compute, and a delete would leave a hole in a keyset window whose
// cursor still assumes the row is there. Both are recorded as "N rows changed
// since you loaded this" with a reload affordance -- the reload restarts the
// walk, which is the only operation that is unambiguously correct. Counting
// is not a placeholder for something better; applying them in place is the
// thing that would be wrong.
//
// A row that arrives live and is ALSO already in the paged window (the walk
// raced past it between the query and the subscription being established) is
// dropped from the band -- it is already on screen, and showing it twice is
// the duplicate this whole module exists to avoid.

import type { Event, Row } from "@znasllc-io/memql-sdk-core/client";

// The CDC verbs, as they appear on Event.kind after the SDK strips the
// EVENT_KIND_ prefix (see sdk/ts/src/client/types.ts eventFromWire).
const KIND_CREATED = "NODE_CREATED";
const KIND_UPDATED = "NODE_UPDATED";
const KIND_DELETED = "NODE_DELETED";

export interface LiveBandState {
  // Rows created since the view opened, oldest first. Each carries the
  // event's own flattened payload -- enough to render a row card. Opening one
  // navigates to the row detail, which re-reads it authoritatively.
  readonly created: readonly Row[];
  // Ids of rows updated or deleted since the view opened. A SET semantically;
  // an array because state must stay comparable and serializable.
  readonly changedIds: readonly string[];
}

export const EMPTY_LIVE_BAND: LiveBandState = { created: [], changedIds: [] };

// eventRowId reads the row id off a graph CDC payload. The engine publishes
// `id` and its `nodeId` alias (component/memql/executor_mutation.go), so both
// are accepted -- reading only one would make the band depend on which
// write path emitted the event.
export function eventRowId(event: Event): string {
  const payload = event.payload;
  if (!payload) return "";
  const id = payload["id"];
  if (typeof id === "string" && id !== "") return id;
  const nodeId = payload["nodeId"];
  if (typeof nodeId === "string" && nodeId !== "") return nodeId;
  return "";
}

// eventConceptId reads the concept the event is about. The subscription is
// already server-side filtered to one concept, so this is a belt-and-braces
// check for the "subscribed to everything" case -- it is not a substitute for
// filtering on the wire.
export function eventConceptId(event: Event): string {
  const concept = event.payload?.["concept"];
  return typeof concept === "string" ? concept : "";
}

// eventRow projects a CDC payload into the row shape view-kit renders.
//
// The engine flattens the node's payload fields into the event alongside the
// intrinsics, and also keeps the whole payload under `payload`. That is
// almost the list projection already; the only fixups are naming (`nodeType`
// is the event's spelling of the `type` intrinsic) and dropping the
// event-only keys that are not row data, so the card does not sprout an
// "actor" field no query would ever return.
export function eventRow(event: Event): Row {
  const payload = event.payload ?? {};
  const out: Row = {};
  for (const [key, value] of Object.entries(payload)) {
    if (key === "actor" || key === "nodeId" || key === "nodeType" || key === "payload") {
      continue;
    }
    out[key] = value;
  }
  const nodeType = payload["nodeType"];
  if (typeof nodeType === "string" && nodeType !== "" && !("type" in out)) {
    out["type"] = nodeType;
  }
  return out;
}

export interface LiveBandContext {
  // The concept this view is browsing. An event for anything else is ignored.
  conceptId: string;
  // Ids already present in the paged window. A created row that is already
  // on screen is dropped rather than shown twice -- see the header.
  pagedRowIds: ReadonlySet<string>;
}

// applyGraphEvent folds one CDC event into the band. Pure: same state + same
// event always produces the same result, and an event that changes nothing
// returns the SAME object so React can skip the render.
export function applyGraphEvent(
  state: LiveBandState,
  event: Event,
  ctx: LiveBandContext,
): LiveBandState {
  const rowId = eventRowId(event);
  if (rowId === "") return state;

  const concept = eventConceptId(event);
  if (concept !== "" && concept !== ctx.conceptId) return state;

  switch (event.kind) {
    case KIND_CREATED: {
      if (ctx.pagedRowIds.has(rowId)) return state;
      if (state.created.some((row) => row["id"] === rowId)) return state;
      return { ...state, created: [...state.created, eventRow(event)] };
    }
    case KIND_UPDATED:
    case KIND_DELETED: {
      if (state.changedIds.includes(rowId)) return state;
      return { ...state, changedIds: [...state.changedIds, rowId] };
    }
    default:
      // Not a graph CDC verb. The subscription asks for the three above, so
      // anything else here is a kind the server added; ignoring it is
      // forward-compatible, and counting it would inflate a number an
      // operator uses to decide whether to reload.
      return state;
  }
}

export function liveBandIsEmpty(state: LiveBandState): boolean {
  return state.created.length === 0 && state.changedIds.length === 0;
}
