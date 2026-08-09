import type { ReactNode } from "react";
import {
  PROPORTION_BAR_ELEMENT,
  STAT_TILE_ELEMENT,
  TABLE_ELEMENT,
} from "@znasllc-io/memql-view-kit";

import { AccountConsole } from "../accounts/AccountConsole";
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
// MANAGEMENT IS ONE COMPONENT, FROM ELSEWHERE. Creating, editing, archiving
// and credential issuance are memql#3322's surface, and they live in
// src/accounts/AccountConsole.tsx rather than here. That is not tidying: this
// module's contract is "compose elements, render no rows of my own", which
// portal_view_composition_test.go enforces, and a management form is neither
// -- it is inputs, a confirmation and a one-time secret. Putting it here would
// mean either bending the guard or hiding a form inside a module that promises
// not to draw one.
//
// It lands as a final band rather than in the header's `actions` slot: the
// surface is a form plus a credential list, which is too much for a header
// strip, and a band keeps the page's reading-shape-roll grammar intact by
// simply coming after it. What management renders is still composed -- the
// credential list goes through the shared TABLE element, because a token list
// is a row set and row sets are what the library is for.

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

      <Band title="Manage" meta="creating, editing and credentials">
        <AccountConsole concept={concept} rows={rows} selectedRowId={selectedRowId} />
      </Band>
    </>
  );
}
