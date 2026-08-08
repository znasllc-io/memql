import { useEffect, type ReactNode } from "react";
import { renderDetail } from "@znasllc-io/memql-view-kit";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { vnodeToReact } from "../viewkit/react";
import { ensureViewKitStyles } from "../viewkit/styles";

// One row, in full.
//
// ===========================================================================
// THE NESTING IS PRESERVED, DELIBERATELY. Do not flatten this.
// ===========================================================================
// The row list flattens (src/viewkit/rows.ts) because a @displayCard slot
// names a payload field directly and a card has one line to spend. The DETAIL
// pane is the opposite case: an operator opens it precisely to read the things
// flattening destroys -- which values are payload and which are engine
// intrinsics (id / concept / type / createdAt / createdBy / schema), and what
// `provenance` says about where the row came from. Merge those into one flat
// map and a payload field named `type` silently replaces the row's type, and
// the reader has no way to tell which layer any value came from.
//
// This is also why the browse query returns rawNodes() rather than the
// flattened rows(): the nesting has to survive the wire to be shown here.
//
// renderDetail does the recursive walk, from view-kit, with no concept
// knowledge anywhere in it -- the same renderer the VS Code panel uses.

export interface RowDetailProps {
  row: Row;
}

export function RowDetail({ row }: RowDetailProps): ReactNode {
  useEffect(() => {
    ensureViewKitStyles();
  }, []);

  return <>{vnodeToReact(renderDetail(row))}</>;
}
