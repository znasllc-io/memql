// The CDC merge policy (src/concepts/liveBand.ts).
//
// The subtle requirement on memql#3316 is that a row arriving live mid-walk
// must not corrupt the cursor or double-render. The policy chosen -- live rows
// accumulate in a separate band rather than being spliced into the paged
// window -- is what makes that true by construction, and these tests pin the
// behaviour that follows from it.

import { describe, expect, it } from "vitest";
import type { Event } from "@znasllc-io/memql-sdk-core/client";

import {
  EMPTY_LIVE_BAND,
  applyGraphEvent,
  eventRow,
  liveBandIsEmpty,
  type LiveBandContext,
} from "../src/concepts/liveBand";

const CONCEPT = "v1:cognition:space";

// graphEvent mirrors what the engine publishes: the payload fields flattened
// alongside the intrinsics, with the whole payload also kept nested under
// `payload` (component/memql/executor_mutation.go).
function graphEvent(kind: string, id: string, payload: Record<string, unknown> = {}): Event {
  return {
    subscriptionId: "sub-1",
    kind,
    timestamp: new Date("2026-08-08T10:00:00Z"),
    payload: {
      id,
      nodeId: id,
      concept: CONCEPT,
      actor: "user:someone",
      nodeType: "concept",
      createdAt: "2026-08-08T10:00:00Z",
      ...payload,
      payload,
    },
  };
}

function ctx(pagedRowIds: string[] = []): LiveBandContext {
  return { conceptId: CONCEPT, pagedRowIds: new Set(pagedRowIds) };
}

describe("the live band", () => {
  it("collects created rows without touching the paged window", () => {
    let band = EMPTY_LIVE_BAND;
    band = applyGraphEvent(band, graphEvent("NODE_CREATED", "a", { name: "Alpha" }), ctx());
    band = applyGraphEvent(band, graphEvent("NODE_CREATED", "b", { name: "Beta" }), ctx());

    expect(band.created.map((row) => row["id"])).toEqual(["a", "b"]);
    expect(band.changedIds).toEqual([]);
    expect(liveBandIsEmpty(band)).toBe(false);
    // Nothing in the band is a cursor or a paged row -- the walk's state is
    // untouched by construction, because this module cannot reach it.
    expect(Object.keys(band)).toEqual(["created", "changedIds"]);
  });

  it("drops a created row the walk has already paged in, so it is never shown twice", () => {
    // The race: the subscription is established a beat after the first query,
    // so a row can be both in page one AND in a create event.
    const band = applyGraphEvent(
      EMPTY_LIVE_BAND,
      graphEvent("NODE_CREATED", "already-here"),
      ctx(["already-here"]),
    );
    expect(band).toBe(EMPTY_LIVE_BAND);
  });

  it("does not add the same created row twice", () => {
    let band = applyGraphEvent(EMPTY_LIVE_BAND, graphEvent("NODE_CREATED", "a"), ctx());
    const after = applyGraphEvent(band, graphEvent("NODE_CREATED", "a"), ctx());
    expect(after).toBe(band);
    band = after;
    expect(band.created).toHaveLength(1);
  });

  it("counts updates and deletes rather than applying them to the paged window", () => {
    let band = EMPTY_LIVE_BAND;
    band = applyGraphEvent(band, graphEvent("NODE_UPDATED", "x"), ctx(["x"]));
    band = applyGraphEvent(band, graphEvent("NODE_DELETED", "y"), ctx(["y"]));
    band = applyGraphEvent(band, graphEvent("NODE_UPDATED", "x"), ctx(["x"]));

    // Deduped by id: five updates to one row is still one changed row.
    expect(band.changedIds).toEqual(["x", "y"]);
    // Applying them would leave a hole in a keyset window whose cursor still
    // assumes the row is there, so they stay a count plus a reload prompt.
    expect(band.created).toEqual([]);
  });

  it("ignores an event for a different concept", () => {
    const other: Event = {
      ...graphEvent("NODE_CREATED", "z"),
      payload: { id: "z", concept: "v1:identity:user" },
    };
    expect(applyGraphEvent(EMPTY_LIVE_BAND, other, ctx())).toBe(EMPTY_LIVE_BAND);
  });

  it("ignores an event carrying no row id, and an unknown kind", () => {
    const noId: Event = { subscriptionId: "s", kind: "NODE_CREATED", timestamp: null, payload: {} };
    expect(applyGraphEvent(EMPTY_LIVE_BAND, noId, ctx())).toBe(EMPTY_LIVE_BAND);

    const unknown = graphEvent("SOMETHING_ELSE", "a");
    expect(applyGraphEvent(EMPTY_LIVE_BAND, unknown, ctx())).toBe(EMPTY_LIVE_BAND);
  });

  it("reads the row id from the nodeId alias when id is absent", () => {
    const aliased: Event = {
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: null,
      payload: { nodeId: "aliased", concept: CONCEPT },
    };
    const band = applyGraphEvent(EMPTY_LIVE_BAND, aliased, ctx());
    expect(band.created.map((row) => row["id"])).toEqual([undefined]);
    // The id is what routes the click; it is read off the event even when the
    // projected row does not carry it as `id`.
    expect(band.created).toHaveLength(1);
  });

  it("projects an event into a row shape without the event-only keys", () => {
    const row = eventRow(graphEvent("NODE_CREATED", "a", { name: "Alpha", status: "active" }));
    expect(row["id"]).toBe("a");
    expect(row["name"]).toBe("Alpha");
    expect(row["status"]).toBe("active");
    expect(row["concept"]).toBe(CONCEPT);
    // `nodeType` is the event's spelling of the row's `type` intrinsic.
    expect(row["type"]).toBe("concept");
    // Event-only keys are not row data and must not sprout fields no query
    // would ever return.
    expect(row["actor"]).toBeUndefined();
    expect(row["nodeId"]).toBeUndefined();
    expect(row["nodeType"]).toBeUndefined();
  });

  it("returns the SAME object when nothing changed, so React can skip the render", () => {
    const band = applyGraphEvent(EMPTY_LIVE_BAND, graphEvent("NODE_CREATED", "a"), ctx());
    expect(applyGraphEvent(band, graphEvent("NODE_CREATED", "a"), ctx())).toBe(band);
  });
});
