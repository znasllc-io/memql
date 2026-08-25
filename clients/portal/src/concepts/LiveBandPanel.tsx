import type { ReactNode } from "react";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import { RowList } from "../components/RowList";
import { Button } from "../ui";
import { liveBandIsEmpty, type LiveBandState } from "./liveBand";

// The CDC arrivals band -- the portal's one way of saying "this list moved
// while you were reading it".
//
// SHARED SINCE memql#4539. It lived inside the concept browser's pane, which
// meant the browser was the only surface with a live band at all: every
// predefined view and every composed view opened a subscription through the
// same machinery and DISCARDED the events, so a person watching the Users
// view saw nothing arrive and had no way to know that was a limitation rather
// than a quiet cluster.
//
// WHY A BAND, NOT A SPLICE. The keyset cursor orders by createdAt ascending,
// so a row created now is a row the walk has not reached yet -- inserting it
// into the paged list guarantees a duplicate when paging catches up, and
// changes a count under the operator's eyes mid-scan. See
// src/concepts/liveBand.ts for the full reasoning.
export function LiveBandPanel({
  band,
  concept,
  onSelect,
  onReload,
  selectedRowId,
}: {
  band: LiveBandState;
  concept: Concept;
  onSelect: (rowId: string) => void;
  onReload: () => void;
  selectedRowId: string;
}): ReactNode {
  if (liveBandIsEmpty(band)) return null;

  return (
    <div className="mb-3 overflow-hidden rounded-lg border border-accent bg-accent-subtle/40">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-accent/40 px-3 py-1.5">
        <span className="text-xs font-medium text-fg">
          New since you opened this
          {band.created.length > 0 ? ` — ${band.created.length}` : ""}
          {band.changedIds.length > 0
            ? `, ${band.changedIds.length} existing ${
                band.changedIds.length === 1 ? "row" : "rows"
              } changed`
            : ""}
        </span>
        <Button size="xs" onClick={onReload}>
          Reload the list
        </Button>
      </div>
      {band.created.length > 0 ? (
        // Keyed by the arrival count so each new row re-triggers the accent
        // wash -- a brief background fade that says "this just happened"
        // without stealing the scroll position or re-fetching anything.
        <div key={band.created.length} className="row-wash">
          <RowList
            rows={[...band.created]}
            concept={concept}
            {...(selectedRowId ? { selectedRowId } : {})}
            onSelect={onSelect}
          />
        </div>
      ) : null}
    </div>
  );
}
