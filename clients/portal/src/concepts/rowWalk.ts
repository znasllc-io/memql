// The keyset walk: a pure state machine for paging one concept's rows.
//
// `browseConceptPage` is keyset-based -- each page comes back with an opaque
// cursor bound to the query's `sort("createdAt","asc")` ordering, and passing
// that cursor back continues from the encoded position instead of re-scanning
// by offset. The acceptance bar on memql#3316 is that a walk over a large
// concept yields every row exactly once: NO GAPS, NO DUPLICATES. That is a
// state-machine property, so it lives in a state machine that can be tested
// without a browser, a server, or React -- drive `walkReducer` with a fake
// transport and assert the whole walk.
//
// ===========================================================================
// THE FOUR THINGS THIS GUARDS, and why each needs guarding
// ===========================================================================
//
// 1. CONCURRENCY. Two "Load more" clicks landing before the first response
//    returns would issue the SAME cursor twice and append the same page
//    twice -- a duplicate that no amount of server correctness prevents.
//    `inFlight` refuses the second request outright rather than racing it.
//
// 2. STALENESS. Switching concepts, or reconnecting, must discard a response
//    already in the air. Every request carries the `generation` it was issued
//    under AND a monotonic `requestId`; a settle whose pair does not match
//    the machine's current one is dropped silently instead of appending
//    another concept's rows -- or an abandoned attempt's rows -- to this
//    walk.
//
// 3. A CURSOR THAT RESETS. This is the failure the issue calls out by name.
//    If the server (or a replica, or a proxy) ever hands back a cursor it has
//    already issued for this walk, following it silently restarts the walk
//    from that point -- the operator sees the same rows again, believes the
//    concept contains them twice, and no error is ever raised. `seenCursors`
//    makes that detectable, and the machine STOPS with an explicit failure
//    instead. Refusing loudly is the whole point: a browser that quietly
//    re-walks is indistinguishable from a concept with duplicate rows.
//
// 4. FAILING MID-WALK. A page that errors must not discard the pages already
//    collected, and must not look like "exhausted". The machine keeps the
//    rows and the cursor that failed, and enters `failed` -- so `retry`
//    resumes the SAME walk rather than starting a new one.
//
// The status set is deliberately honest about all of it: an operator can tell
// "still loading", "there is more", "that is everything", and "this broke
// part-way" apart, because collapsing any two of those is how a truncated
// list reads as a complete one.
//
// WHY A REDUCER AND NOT A CLASS WITH REFS. The React host drives the fetch
// from an effect that watches `inFlight` + `requestId`, so the machine
// decides WHETHER to fetch and the host only carries it out. Nothing reads
// "current" state out of a ref at settle time, which is the shape that
// silently drops a good page when a state update has been scheduled but not
// yet committed.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

export type WalkStatus =
  // No request outstanding and one is wanted: the host's kick effect turns
  // this into a request. Both the initial state and where `retry` lands.
  | "idle"
  // A page is in the air. `rows` may already hold earlier pages.
  | "loading"
  // At rest with more to fetch: `cursor` is the next continuation token,
  // awaiting an explicit "load more".
  | "ready"
  // At rest with the set fully walked -- the server returned an empty cursor.
  | "exhausted"
  // Stopped part-way. `rows` holds what was collected; `error` says why.
  | "failed";

export interface WalkState {
  // Bumped by `reset`. Stamped on every request; a settle carrying a stale
  // generation is discarded.
  readonly generation: number;
  readonly rows: readonly Row[];
  // The continuation token for the NEXT request. "" means "start from the
  // beginning" before the first page and "the set is exhausted" after it --
  // unambiguous only because `status` distinguishes the two.
  readonly cursor: string;
  // Every cursor already CONSUMED by this walk (a request that came back),
  // "" (the first page) included. Guard 3 above.
  readonly seenCursors: readonly string[];
  readonly status: WalkStatus;
  readonly error: string;
  readonly inFlight: boolean;
  // Monotonic id of the in-flight request, and the cursor it carries. The
  // host reads both to perform the fetch and echoes `requestId` back on the
  // settle. Zero when nothing has ever been requested in this generation.
  readonly requestId: number;
  readonly requestCursor: string;
}

export interface WalkPage {
  rows: Row[];
  nextCursor: string;
}

export type WalkAction =
  // A new walk. Bumps the generation, so anything in the air is discarded
  // when it settles.
  | { kind: "reset" }
  // Ask for a page at `cursor`. Rejected (state returned unchanged) when a
  // request is already in flight, when the set is exhausted, or when the walk
  // has failed and not been retried.
  | { kind: "requested"; generation: number; cursor: string }
  | { kind: "arrived"; generation: number; requestId: number; page: WalkPage }
  | { kind: "failed"; generation: number; requestId: number; error: string }
  // Clear the error and return to a requestable state WITHOUT touching the
  // rows or the cursor -- retry resumes, it does not restart.
  | { kind: "retry" };

export const INITIAL_WALK: WalkState = {
  generation: 0,
  rows: [],
  cursor: "",
  seenCursors: [],
  status: "idle",
  error: "",
  inFlight: false,
  requestId: 0,
  requestCursor: "",
};

export const CURSOR_LOOP_ERROR =
  "the cluster returned a paging cursor it had already issued for this walk, " +
  "which would replay rows already shown. Stopped rather than listing them twice.";

// canRequest answers "may a page be asked for right now". Exported because
// the UI needs the same answer for enabling the control as the reducer uses
// for accepting the action -- two implementations of that question is how a
// button that does nothing happens.
export function canRequest(state: WalkState): boolean {
  if (state.inFlight) return false;
  return state.status === "idle" || state.status === "ready";
}

function settleApplies(
  state: WalkState,
  action: { generation: number; requestId: number },
): boolean {
  return (
    state.inFlight &&
    action.generation === state.generation &&
    action.requestId === state.requestId
  );
}

export function walkReducer(state: WalkState, action: WalkAction): WalkState {
  switch (action.kind) {
    case "reset":
      return { ...INITIAL_WALK, generation: state.generation + 1 };

    case "requested": {
      if (action.generation !== state.generation) return state;
      if (!canRequest(state)) return state;
      // Guard 3, on the OUTBOUND side: never send a cursor this walk has
      // already consumed. Catches a loop introduced by our own state rather
      // than by the server's reply.
      if (state.seenCursors.includes(action.cursor)) {
        return { ...state, status: "failed", error: CURSOR_LOOP_ERROR, inFlight: false };
      }
      return {
        ...state,
        status: "loading",
        error: "",
        inFlight: true,
        requestId: state.requestId + 1,
        requestCursor: action.cursor,
      };
    }

    case "arrived": {
      if (!settleApplies(state, action)) return state;

      const seenCursors = [...state.seenCursors, state.requestCursor];
      const rows = [...state.rows, ...action.page.rows];

      if (action.page.nextCursor === "") {
        return {
          ...state,
          rows,
          seenCursors,
          cursor: "",
          status: "exhausted",
          error: "",
          inFlight: false,
        };
      }

      // Guard 3, on the INBOUND side: the server handed back a cursor this
      // walk has already followed. Keep the rows legitimately collected,
      // stop, and say so.
      if (seenCursors.includes(action.page.nextCursor)) {
        return {
          ...state,
          rows,
          seenCursors,
          cursor: action.page.nextCursor,
          status: "failed",
          error: CURSOR_LOOP_ERROR,
          inFlight: false,
        };
      }

      return {
        ...state,
        rows,
        seenCursors,
        cursor: action.page.nextCursor,
        status: "ready",
        error: "",
        inFlight: false,
      };
    }

    case "failed": {
      if (!settleApplies(state, action)) return state;
      // The cursor is deliberately NOT recorded in seenCursors: nothing was
      // consumed, so retrying it is resuming, not looping.
      return { ...state, status: "failed", error: action.error, inFlight: false };
    }

    case "retry": {
      if (state.status !== "failed") return state;
      // A cursor-loop failure is NOT retryable by resuming -- the cursor that
      // caused it is the one we would re-send, and the outbound guard would
      // refuse it again. A full reload is the only honest exit, and the UI
      // offers exactly that instead.
      if (state.error === CURSOR_LOOP_ERROR) return state;
      // "idle" rather than "ready": the host's kick effect fires on idle, so
      // resuming needs no second code path.
      return { ...state, status: "idle", error: "" };
    }

    default:
      return state;
  }
}

// runWalkToCompletion drives a walk to `exhausted` (or `failed`) against a
// caller-supplied page fetcher, using the SAME request/settle sequencing the
// React host uses. It exists for the test: "walks a large concept without
// gaps or duplicates" is a property of the whole walk, not of one transition,
// and asserting it means running one.
export async function runWalkToCompletion(
  fetchPage: (cursor: string) => Promise<WalkPage>,
  maxPages = 10_000,
): Promise<WalkState> {
  let state: WalkState = { ...INITIAL_WALK };
  for (let i = 0; i < maxPages; i++) {
    if (!canRequest(state)) break;
    state = walkReducer(state, {
      kind: "requested",
      generation: state.generation,
      cursor: state.cursor,
    });
    // The reducer refused (a loop detected on the outbound side). Stop rather
    // than spinning.
    if (!state.inFlight) break;
    const { generation, requestId, requestCursor } = state;
    try {
      const page = await fetchPage(requestCursor);
      state = walkReducer(state, { kind: "arrived", generation, requestId, page });
    } catch (err) {
      state = walkReducer(state, {
        kind: "failed",
        generation,
        requestId,
        error: err instanceof Error ? err.message : String(err),
      });
    }
  }
  return state;
}
