import { useCallback, useEffect, type KeyboardEvent, type ReactNode } from "react";
import { renderRowList } from "@znasllc-io/memql-view-kit";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { vnodeToReact } from "../viewkit/react";
import { flattenForList } from "../viewkit/rows";
import { ensureViewKitStyles } from "../viewkit/styles";

// A concept's rows, rendered THROUGH VIEW-KIT rather than by this component.
//
// renderRowList returns a VNode tree, vnodeToReact walks it into React
// elements, and the class contract view-kit emits (vk-row / vk-row-primary /
// vk-row-status / ...) is styled by view-kit's own sheet, themed through the
// --vk-* tokens the portal defines in src/styles/tokens.css. The VS Code panel
// and this page therefore render an unknown concept's rows the same way, from
// one implementation.
//
// ===========================================================================
// THERE IS NO CONCEPT-SPECIFIC CODE HERE AND THERE MUST NEVER BE.
// ===========================================================================
// Which fields appear comes from the concept's @displayCard hints, and from
// view-kit's documented inference (sdk/ts-viewkit/src/displayCard.ts,
// docs/public/concepts/display-cards.md) when a concept declares none -- so a
// concept declared tomorrow renders today, with no edit here. The rule is
// enforced mechanically, not by review: portal_render_path_test.go in the repo
// root fails the build if any concept-id literal appears in this file or any
// other module on the browse path.
//
// SELECTION is likewise attribute-driven. The rows carry `data-row-id`;
// `enhance` turns every element carrying one into a focusable, clickable,
// Enter/Space-operable control. It keys off the attribute, never off the tag
// or the content, so it decorates whatever view-kit emits without knowing its
// shape.

export interface RowListProps {
  rows: Row[];
  concept: Concept;
  selectedRowId?: string;
  // Called with the clicked row's id. Optional: a read-only list (the live
  // band on a disconnected pane) simply omits it and stays inert.
  onSelect?: (rowId: string) => void;
}

export function RowList({
  rows,
  concept,
  selectedRowId,
  onSelect,
}: RowListProps): ReactNode {
  // The sheet is a string export, so it has to be installed into the
  // document; doing it in an effect keeps it out of render and idempotent
  // (see src/viewkit/styles.ts).
  useEffect(() => {
    ensureViewKitStyles();
  }, []);

  const enhance = useCallback(
    (attrs: Readonly<Record<string, string>>) => {
      const rowId = attrs["data-row-id"];
      if (!rowId || !onSelect) return undefined;
      return {
        role: "button",
        tabIndex: 0,
        onClick: () => onSelect(rowId),
        onKeyDown: (event: KeyboardEvent) => {
          // Enter and Space are what a role="button" is required to answer to
          // (WAI-ARIA). Without this the whole list is unreachable by
          // keyboard, which in an operations console is not a nicety.
          if (event.key !== "Enter" && event.key !== " ") return;
          event.preventDefault();
          onSelect(rowId);
        },
      };
    },
    [onSelect],
  );

  const tree = renderRowList(rows.map(flattenForList), concept, selectedRowId);
  return <>{vnodeToReact(tree, { enhance })}</>;
}
