// The keyset walk (src/concepts/rowWalk.ts).
//
// The acceptance bar on memql#3316 is that a walk over a large concept yields
// every row exactly once -- NO GAPS, NO DUPLICATES -- and that a cursor which
// resets does not silently restart the walk. Both are properties of the whole
// walk, not of any single transition, so these tests RUN one: a fake transport
// serves N pages, the machine is driven to completion against it, and the
// resulting row set is checked for count, distinctness and order.
//
// Driven at the reducer, deliberately, not through render() + waitFor(). A
// paging invariant asserted through React, jsdom and a fake WebSocket is
// asserted through three layers that can each fail for unrelated reasons; the
// integration is covered separately in conceptBrowser.test.tsx.

import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  CURSOR_LOOP_ERROR,
  INITIAL_WALK,
  canRequest,
  runWalkToCompletion,
  walkReducer,
  type WalkPage,
  type WalkState,
} from "../src/concepts/rowWalk";

const PAGE_SIZE = 25;
const PAGE_COUNT = 8;

// pagedTransport serves PAGE_COUNT pages of PAGE_SIZE distinct rows, keyed by
// an opaque cursor exactly as the engine does: the cursor is meaningless to
// the client, and the last page returns "" to mean exhausted.
function pagedTransport(pageCount = PAGE_COUNT, pageSize = PAGE_SIZE) {
  const calls: string[] = [];
  const fetchPage = async (cursor: string): Promise<WalkPage> => {
    calls.push(cursor);
    const index = cursor === "" ? 0 : Number(cursor.replace("cursor-", ""));
    const rows: Row[] = [];
    for (let i = 0; i < pageSize; i++) {
      const n = index * pageSize + i;
      rows.push({ id: `row-${String(n).padStart(4, "0")}`, payload: { seq: n } });
    }
    const last = index === pageCount - 1;
    return { rows, nextCursor: last ? "" : `cursor-${index + 1}` };
  };
  return { fetchPage, calls };
}

function ids(state: WalkState): string[] {
  return state.rows.map((row) => String(row["id"]));
}

describe("the keyset walk", () => {
  it("yields exactly pageCount * pageSize rows, distinct and in order", async () => {
    const { fetchPage, calls } = pagedTransport();
    const state = await runWalkToCompletion(fetchPage);

    expect(state.status).toBe("exhausted");
    expect(state.error).toBe("");
    expect(state.rows).toHaveLength(PAGE_COUNT * PAGE_SIZE);

    const seen = ids(state);
    // No duplicates.
    expect(new Set(seen).size).toBe(seen.length);
    // No gaps, and the server's order preserved: the ids are zero-padded, so
    // sorted order IS arrival order when nothing was dropped or reordered.
    expect(seen).toEqual([...seen].sort());
    expect(seen[0]).toBe("row-0000");
    expect(seen.at(-1)).toBe(`row-${String(PAGE_COUNT * PAGE_SIZE - 1).padStart(4, "0")}`);

    // One request per page, first with the empty cursor, then each cursor the
    // server handed back -- no page fetched twice.
    expect(calls).toEqual([
      "",
      ...Array.from({ length: PAGE_COUNT - 1 }, (_, i) => `cursor-${i + 1}`),
    ]);
  });

  it("records every consumed cursor so the walk can tell where it has been", async () => {
    const { fetchPage } = pagedTransport(3);
    const state = await runWalkToCompletion(fetchPage);
    expect(state.seenCursors).toEqual(["", "cursor-1", "cursor-2"]);
  });

  it("refuses to re-walk when the server hands back a cursor it already issued", async () => {
    // The failure the issue names: page 3 points back at page 2's cursor. A
    // machine without the guard walks 2 -> 3 -> 2 -> 3 forever and shows the
    // same rows repeatedly with no error anywhere.
    const fetchPage = async (cursor: string): Promise<WalkPage> => {
      switch (cursor) {
        case "":
          return { rows: [{ id: "a" }], nextCursor: "c1" };
        case "c1":
          return { rows: [{ id: "b" }], nextCursor: "c2" };
        default:
          return { rows: [{ id: "c" }], nextCursor: "c1" }; // resets
      }
    };

    const state = await runWalkToCompletion(fetchPage);

    expect(state.status).toBe("failed");
    expect(state.error).toBe(CURSOR_LOOP_ERROR);
    // The rows legitimately collected are KEPT -- three real rows were
    // returned before the loop was detected, and discarding them would lose
    // data the operator was already looking at.
    expect(ids(state)).toEqual(["a", "b", "c"]);
    // And crucially: it stopped. No fourth page, no repeat of "b".
    expect(state.rows).toHaveLength(3);
  });

  it("stops rather than resuming a cursor loop, because resuming re-sends the same cursor", () => {
    let state: WalkState = { ...INITIAL_WALK };
    state = walkReducer(state, { kind: "requested", generation: 0, cursor: "" });
    state = walkReducer(state, {
      kind: "arrived",
      generation: 0,
      requestId: state.requestId,
      page: { rows: [{ id: "a" }], nextCursor: "" },
    });
    // Force the outbound guard: ask again for a cursor already consumed.
    state = { ...state, status: "ready", cursor: "" };
    state = walkReducer(state, { kind: "requested", generation: 0, cursor: "" });
    expect(state.status).toBe("failed");
    expect(state.error).toBe(CURSOR_LOOP_ERROR);
    expect(state.inFlight).toBe(false);

    // retry is a no-op on a loop: the only exit is a reset.
    expect(walkReducer(state, { kind: "retry" })).toBe(state);
    expect(walkReducer(state, { kind: "reset" }).status).toBe("idle");
  });

  it("refuses a second concurrent request rather than fetching the same page twice", () => {
    let state: WalkState = { ...INITIAL_WALK };
    state = walkReducer(state, { kind: "requested", generation: 0, cursor: "" });
    expect(state.inFlight).toBe(true);
    expect(canRequest(state)).toBe(false);

    // Two "Load more" clicks before the first response lands. Without the
    // guard both capture the same cursor and both append the same page.
    const again = walkReducer(state, { kind: "requested", generation: 0, cursor: "" });
    expect(again).toBe(state);
  });

  it("discards a settle from a superseded generation", () => {
    let state: WalkState = { ...INITIAL_WALK };
    state = walkReducer(state, { kind: "requested", generation: 0, cursor: "" });
    const staleRequestId = state.requestId;

    // The operator switched concepts while the page was in the air.
    state = walkReducer(state, { kind: "reset" });
    expect(state.generation).toBe(1);

    const after = walkReducer(state, {
      kind: "arrived",
      generation: 0,
      requestId: staleRequestId,
      page: { rows: [{ id: "from-the-old-concept" }], nextCursor: "" },
    });
    expect(after).toBe(state);
    expect(after.rows).toHaveLength(0);
  });

  it("discards a settle whose requestId is no longer the outstanding one", () => {
    let state: WalkState = { ...INITIAL_WALK };
    state = walkReducer(state, { kind: "requested", generation: 0, cursor: "" });
    const after = walkReducer(state, {
      kind: "arrived",
      generation: 0,
      requestId: state.requestId + 1,
      page: { rows: [{ id: "x" }], nextCursor: "" },
    });
    expect(after).toBe(state);
  });

  it("keeps the rows already collected when a page fails mid-walk, and resumes on retry", async () => {
    let failOnce = true;
    const fetchPage = async (cursor: string): Promise<WalkPage> => {
      if (cursor === "c1" && failOnce) {
        failOnce = false;
        throw new Error("stream closed: code=1006");
      }
      if (cursor === "") return { rows: [{ id: "a" }, { id: "b" }], nextCursor: "c1" };
      return { rows: [{ id: "c" }], nextCursor: "" };
    };

    const stopped = await runWalkToCompletion(fetchPage);
    expect(stopped.status).toBe("failed");
    expect(stopped.error).toContain("1006");
    // The first page survives -- a mid-walk failure is not an empty list.
    expect(ids(stopped)).toEqual(["a", "b"]);
    // The failed cursor was NOT consumed, so resuming it is not a loop.
    expect(stopped.seenCursors).toEqual([""]);
    expect(stopped.cursor).toBe("c1");

    // Retry resumes from the same cursor rather than restarting.
    let state = walkReducer(stopped, { kind: "retry" });
    expect(state.status).toBe("idle");
    expect(ids(state)).toEqual(["a", "b"]);

    state = walkReducer(state, {
      kind: "requested",
      generation: state.generation,
      cursor: state.cursor,
    });
    const page = await fetchPage(state.requestCursor);
    state = walkReducer(state, {
      kind: "arrived",
      generation: state.generation,
      requestId: state.requestId,
      page,
    });

    expect(state.status).toBe("exhausted");
    expect(ids(state)).toEqual(["a", "b", "c"]);
    expect(new Set(ids(state)).size).toBe(3);
  });

  it("reports an empty concept as exhausted, not as a failure or a pending load", async () => {
    const state = await runWalkToCompletion(async () => ({ rows: [], nextCursor: "" }));
    expect(state.status).toBe("exhausted");
    expect(state.rows).toHaveLength(0);
    expect(state.error).toBe("");
  });

  it("will not request another page once exhausted", async () => {
    const { fetchPage, calls } = pagedTransport(1, 2);
    const state = await runWalkToCompletion(fetchPage);
    expect(state.status).toBe("exhausted");
    expect(canRequest(state)).toBe(false);
    expect(walkReducer(state, { kind: "requested", generation: 0, cursor: "" })).toBe(
      state,
    );
    expect(calls).toEqual([""]);
  });
});
