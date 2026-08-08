import type { ReactNode } from "react";
import {
  PROPORTION_BAR_ELEMENT,
  STAT_TILE_ELEMENT,
  TABLE_ELEMENT,
} from "@znasllc-io/memql-view-kit";

import { ViewElement } from "./ViewElement";
import { Band, type ViewProps } from "./ViewLayout";

// Customers: the businesses this operator runs memQL for.
//
// LAYOUT RATIONALE. The quietest of the five, on purpose. A customer list is
// a ledger: an operator opens it to find one row, or to check that the
// lifecycle column says what they expect. So the reading is a count, the
// shape is the lifecycle split -- the one number a book of business is judged
// on is how many are still active -- and the roll is a plain table sorted by
// recency, because "who did I touch last" is how someone re-finds the account
// they were working on.
//
// THE READ IS ALREADY SCOPED, and not by this page. v1:identity:account
// declares @rowAuthz(owner="ownerUserId"), so the engine ANDs
// ownerUserId == actor.userId into every read of it
// (component/memql/rowauthz_enforce.go). This view shows what the cluster
// returns; it does not filter, and filtering here would imply the isolation
// lives in the browser.
//
// MANAGEMENT IS NOT HERE. Creating, editing, archiving and token issuance are
// memql#3322's surface, built on top of this one. The seam is deliberate: this
// module reads and lays out, and the actions land in the header's `actions`
// slot (see ViewPage) without touching a band.

export function CustomersView({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: ViewProps): ReactNode {
  const selection = selectedRowId ? { selectedRowId } : {};

  return (
    <>
      <Band>
        <ViewElement
          element={STAT_TILE_ELEMENT}
          rows={rows}
          concept={concept}
          options={{ bindings: { metric: [] } }}
        />
      </Band>

      <Band title="By lifecycle" meta="archived customers are kept, not deleted">
        <ViewElement
          element={PROPORTION_BAR_ELEMENT}
          rows={rows}
          concept={concept}
          // No binding: the concept declares status="status", and the rail
          // prefers whatever a concept calls its status. Naming the field
          // here would be this view deciding something the concept already
          // decided.
          options={{ bindings: { value: [] } }}
        />
      </Band>

      <Band title="Every customer" panel>
        <ViewElement
          element={TABLE_ELEMENT}
          rows={rows}
          concept={concept}
          options={{ ...selection, sort: { field: "updatedAt", direction: "desc" } }}
          onSelect={onSelect}
        />
      </Band>
    </>
  );
}
