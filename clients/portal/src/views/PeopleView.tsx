import type { ReactNode } from "react";
import {
  ROW_LIST_ELEMENT,
  PROPORTION_BAR_ELEMENT,
  STAT_TILE_ELEMENT,
  TABLE_ELEMENT,
} from "@znasllc-io/memql-view-kit";

import { useViewRows } from "../cluster/useViewRows";
import { ViewElement } from "./ViewElement";
import { Band, type ViewProps } from "./ViewLayout";

// People: the operator's own organisation.
//
// LAYOUT RATIONALE. Roles are the thing an operator scans this page for --
// "who can deploy, who can only read" -- so the shape band is the role split,
// and the roll is a TABLE rather than a list: a person is compared against
// other people (role against role, last-seen against last-seen), and
// comparison is what a sortable grid is for. Sessions are a second population
// on the same page rather than a page of their own, because "who is signed in
// right now" is a question about people, and a session on its own screen is a
// row nobody would go looking for.
//
// Every band is an element. The bindings below are the whole of what this
// view "designs": which field the rail divides on, and which slots the table
// leads with. The fields themselves come out of the concept's own display
// card via the fitness profile -- this module names no field it did not
// deliberately choose, and reads none off a row.

// The second population. Sessions carry no display-card status worth a rail,
// so they get the compact list rather than a grid: an operator reads them
// for presence, not for comparison.
const SESSION_CONCEPT_ID = "v1:identity:authSession";

export function PeopleView({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: ViewProps): ReactNode {
  const sessions = useViewRows(SESSION_CONCEPT_ID);
  const selection = selectedRowId ? { selectedRowId } : {};

  return (
    <>
      <Band>
        <ViewElement
          element={STAT_TILE_ELEMENT}
          rows={rows}
          concept={concept}
          // No measures. A user row's only top-level number is
          // revocationEpoch, and its sum is a true, meaningless figure.
          options={{ bindings: { metric: [] } }}
        />
      </Band>

      <Band title="By role" meta="Cluster-wide role, not per-partition grants">
        <ViewElement
          element={PROPORTION_BAR_ELEMENT}
          rows={rows}
          concept={concept}
          // The rail would otherwise divide on the concept's declared status
          // slot (active / not active), which is the less interesting of the
          // two splits: an operator already knows roughly who is active.
          options={{ bindings: { category: "role", value: [] } }}
        />
      </Band>

      <Band title="Everyone" panel>
        <ViewElement
          element={TABLE_ELEMENT}
          rows={rows}
          concept={concept}
          options={{ ...selection, sort: { field: "lastSeenAt", direction: "desc" } }}
          onSelect={onSelect}
        />
      </Band>

      <Band
        title="Open sessions"
        meta={
          sessions.concept === undefined
            ? "not published by this node"
            : `${sessions.rows.length} loaded`
        }
      >
        {sessions.concept === undefined ? (
          <p className="text-sm text-subtle">
            This node does not publish the session concept, so sessions cannot be
            listed here.
          </p>
        ) : (
          <ViewElement
            element={ROW_LIST_ELEMENT}
            rows={sessions.rows}
            concept={sessions.concept}
          />
        )}
      </Band>
    </>
  );
}
