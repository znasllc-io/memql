// The arrival cue's brain (spec D7): given successive LiveCollection
// snapshots, decide which rows just ARRIVED and which just CHANGED, and
// decay the ticks. Pure -- time is injected, so the tests own the clock.
//
// A resync is NOT an arrival: when the previous snapshot was not `live`
// (seeding, degraded, disconnected -- or there was none), the new rows are
// a baseline. Re-animating a reconnect's resync would read as "everything
// just changed", which is the lie this reducer exists to avoid.

import type { LiveState } from "@znasllc-io/memql-sdk-core/client";

export const TICK_TTL_MS = 4_000;

export type ArrivalKind = "added" | "updated";

export interface ArrivalTick {
  kind: ArrivalKind;
  at: number;
}

export interface ArrivalState {
  /** Row version fingerprints from the last observed snapshot. */
  seen: Map<string, string>;
  /** Whether the last observed snapshot was `live`. */
  wasLive: boolean;
  /** Decaying ticks by row id. */
  ticks: Map<string, ArrivalTick>;
}

export function emptyArrivals(): ArrivalState {
  return { seen: new Map(), wasLive: false, ticks: new Map() };
}

export interface ArrivalRow {
  id: string;
  /** Anything that changes when the row changes (a version, a JSON string). */
  fingerprint: string;
}

/**
 * Fold the next snapshot. Returns the next state; `ticks` carries only
 * rows that should currently show a cue.
 */
export function observeSnapshot(
  state: ArrivalState,
  rows: ArrivalRow[],
  snapshotState: LiveState,
  now: number,
): ArrivalState {
  const seen = new Map<string, string>();
  const ticks = new Map(decayTicks(state.ticks, now));

  for (const row of rows) {
    if (row.id === "") continue;
    seen.set(row.id, row.fingerprint);
    if (!state.wasLive) continue; // baseline: seed or resync, no cues
    const held = state.seen.get(row.id);
    if (held === undefined) {
      ticks.set(row.id, { kind: "added", at: now });
    } else if (held !== row.fingerprint) {
      ticks.set(row.id, { kind: "updated", at: now });
    }
  }

  // A removed row's tick goes with it.
  for (const id of ticks.keys()) {
    if (!seen.has(id)) ticks.delete(id);
  }

  return { seen, wasLive: snapshotState === "live", ticks };
}

export function decayTicks(ticks: Map<string, ArrivalTick>, now: number): Map<string, ArrivalTick> {
  const next = new Map<string, ArrivalTick>();
  for (const [id, tick] of ticks) {
    if (now - tick.at < TICK_TTL_MS) next.set(id, tick);
  }
  return next;
}
