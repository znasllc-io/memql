import { useMemo, useSyncExternalStore } from "react";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

import { Caption } from "../kit/Caption";
import type { ArrivalKind } from "./arrival";
import { useArrivals } from "./useArrivals";

// THE live list primitive (spec D7): every live surface in the OS renders
// through this, so arrival behavior reads identically everywhere -- a new
// row rises and settles with a decaying "new" tick, an updated row pulses
// once, and the LiveState renders as a quiet caption so a frozen list is
// never mistaken for an empty cluster. Reduced motion swaps movement for
// an opacity step (CSS owns that via the tokens).
//
// The source is a minimal seam, not the LiveCollection class: anything
// with the useSyncExternalStore shape plugs in, which is what the tests
// and future non-collection feeds use.

export interface LiveListSource<T> {
  subscribe(listener: () => void): () => void;
  readonly snapshot: LiveSnapshot<T>;
}

const EMPTY_SNAPSHOT: LiveSnapshot<never> = {
  rows: [],
  state: "disconnected",
  error: "",
  version: 0,
};

export function LiveList<T>({
  source,
  rowId,
  fingerprint,
  renderRow,
  label,
  emptyText,
}: {
  /** Null renders the disconnected caption -- never a fake empty list. */
  source: LiveListSource<T> | null;
  rowId: (row: T) => string;
  /** Changes when the row meaningfully changes (drives "updated" pulses). */
  fingerprint: (row: T) => string;
  renderRow: (row: T, tick: ArrivalKind | null) => React.ReactNode;
  label: string;
  emptyText: string;
}) {
  const snapshot = useSyncExternalStore(
    useMemo(() => (source ? source.subscribe.bind(source) : () => () => {}), [source]),
    () => (source ? source.snapshot : (EMPTY_SNAPSHOT as LiveSnapshot<T>)),
  );

  // The cue itself lives in `useArrivals` (live/useArrivals.ts), because the
  // deploy map draws the same rows as a graph and has to announce a change
  // identically. A list and a map that disagreed about what counts as news
  // would be one app contradicting itself.
  const ticks = useArrivals(snapshot, rowId, fingerprint);

  const stateLine =
    snapshot.state === "seeding"
      ? "Loading from the cluster"
      : snapshot.state === "degraded"
        ? "Live updates degraded -- showing the last known rows"
        : snapshot.state === "disconnected"
          ? "Not connected to the cluster"
          : null;

  return (
    <div className="os-livelist" data-os-livelist data-state={snapshot.state}>
      <ul className="os-livelist-rows" aria-label={label}>
        {snapshot.rows.map((row) => {
          const id = rowId(row);
          const tick = ticks.get(id)?.kind ?? null;
          return (
            <li key={id} className="os-livelist-row" data-arrival={tick ?? undefined}>
              {renderRow(row, tick)}
            </li>
          );
        })}
      </ul>
      {snapshot.rows.length === 0 && snapshot.state === "live" ? (
        <Caption>{emptyText}</Caption>
      ) : null}
      {stateLine ? <Caption>{stateLine}</Caption> : null}
      {snapshot.error ? <p className="os-ask-error">{snapshot.error}</p> : null}
    </div>
  );
}
