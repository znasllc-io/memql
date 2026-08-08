import { useEffect, type ReactNode } from "react";
import { renderRowList } from "@znasllc-io/memql-view-kit";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { vnodeToReact } from "../viewkit/react";
import { flattenForList } from "../viewkit/rows";
import { ensureViewKitStyles } from "../viewkit/styles";

// A concept's rows, rendered THROUGH VIEW-KIT rather than by this component.
//
// This is the proof the shared renderer works in React: renderRowList returns
// a VNode tree, vnodeToReact walks it into React elements, and the class
// contract view-kit emits (vk-row / vk-row-primary / vk-row-status / ...) is
// styled by view-kit's own sheet, themed through the --vk-* tokens the portal
// defines in src/styles/tokens.css. The VS Code panel and this page therefore
// render an unknown concept's rows the same way, from one implementation.
//
// There is no concept-specific code here and there must never be: which
// fields appear comes from the concept's @displayCard hints, so a concept
// declared tomorrow renders today.

export interface RowListProps {
  rows: Row[];
  concept: Concept;
  selectedRowId?: string;
}

export function RowList({ rows, concept, selectedRowId }: RowListProps): ReactNode {
  // The sheet is a string export, so it has to be installed into the
  // document; doing it in an effect keeps it out of render and idempotent
  // (see src/viewkit/styles.ts).
  useEffect(() => {
    ensureViewKitStyles();
  }, []);

  const tree = renderRowList(
    rows.map(flattenForList),
    concept,
    selectedRowId,
  );
  return <>{vnodeToReact(tree)}</>;
}
