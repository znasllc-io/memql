import { useCallback, useEffect, type KeyboardEvent, type MouseEvent, type ReactNode } from "react";
import {
  explainFit,
  fitElement,
  profileConcept,
  renderElement,
  type ConceptLike,
  type ElementOptions,
  type ElementSpec,
  type RowLike,
} from "@znasllc-io/memql-view-kit";

import { useResolvedTheme } from "../app/useResolvedTheme";
import { vnodeToReact } from "./react";
import { ensureViewKitStyles } from "./styles";

// THE ONLY WAY THE PORTAL PUTS ROWS ON SCREEN THROUGH AN ELEMENT.
//
// A surface says "these rows, through this element, bound like so" and stops.
// Markup written here for a row would be markup that exists only for whichever
// surface wrote it, and the whole claim of the view system is that a concept
// nobody wrote code for renders through the shared library.
//
// There were TWO OF THIS FILE until epic memql#4661 -- views/ViewElement.tsx
// and compose/ComposeElement.tsx, identical but for the exported name. The
// copy existed because the predefined-view tree was a closed, guarded
// directory whose file list a repo-root test counts, and reaching into it for
// a component would have coupled the composer to that guard. Predefined views
// are now DATA rendered through the composer's own path, so the tree they
// lived in is gone and with it the reason for the copy. This file is the
// survivor, moved to a neutral home: neither "the composer's" nor "the
// designed views'", because it is both.
//
// AN UNFIT ELEMENT EXPLAINS ITSELF. In a seeded page an unfit element is a
// mistake somebody made once; in a composer it is a normal, expected state --
// a person is trying elements against a concept, and the honest answer to
// "why is this one empty" is view-kit's own sentence, built from the element
// author's requirement prose.

export interface ElementViewProps {
  element: ElementSpec;
  rows: readonly RowLike[];
  concept: ConceptLike;
  options?: ElementOptions;
  onSelect?: (rowId: string) => void;
  onRowAction?: (action: string, rowId: string) => void;
}

export function ElementView({
  element,
  rows,
  concept,
  options,
  onSelect,
  onRowAction,
}: ElementViewProps): ReactNode {
  useEffect(() => {
    ensureViewKitStyles();
  }, []);

  // Attribute-driven, exactly as the browser's row list and the predefined
  // views do it: anything view-kit emits carrying `data-row-id` becomes
  // focusable and keyboard-operable, whatever its tag. That is what lets one
  // callback serve a table row, a board card, a timeline entry and a list item
  // -- including elements added to the library after this file was written.
  const enhance = useCallback(
    (attrs: Readonly<Record<string, string>>) => {
      const action = attrs["data-vk-row-action"];
      const actionRowId = attrs["data-vk-action-row-id"];
      if (action && actionRowId && onRowAction) {
        return {
          onClick: (event: MouseEvent) => {
            event.stopPropagation();
            onRowAction(action, actionRowId);
          },
          onKeyDown: (event: KeyboardEvent) => {
            if (event.key !== "Enter" && event.key !== " ") return;
            event.preventDefault();
            event.stopPropagation();
            onRowAction(action, actionRowId);
          },
        };
      }
      const rowId = attrs["data-row-id"];
      if (!rowId || !onSelect) return undefined;
      return {
        role: "button",
        tabIndex: 0,
        onClick: () => onSelect(rowId),
        onKeyDown: (event: KeyboardEvent) => {
          if (event.key !== "Enter" && event.key !== " ") return;
          event.preventDefault();
          onSelect(rowId);
        },
      };
    },
    [onSelect, onRowAction],
  );

  // The chart palette is the one thing view-kit cannot resolve from CSS alone.
  // Stamping the RESOLVED theme here means a composed view's charts are correct
  // in both, and no composer has to remember.
  const theme = useResolvedTheme();
  const resolved: ElementOptions = { theme, ...options };

  const tree = renderElement(element, rows, concept, resolved);
  if (tree === undefined) {
    const profile = profileConcept(concept, rows);
    return (
      <p className="text-sm text-subtle">
        {explainFit(element, fitElement(element, profile, resolved), profile)}
      </p>
    );
  }

  return <>{vnodeToReact(tree, { enhance })}</>;
}
