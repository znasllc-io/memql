import { useCallback, useEffect, type KeyboardEvent, type ReactNode } from "react";
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
import { vnodeToReact } from "../viewkit/react";
import { ensureViewKitStyles } from "../viewkit/styles";

// THE ONLY WAY A COMPOSED VIEW PUTS ROWS ON SCREEN.
//
// Deliberately the same shape as the predefined views' <ViewElement>: a view
// says "these rows, through this element, bound like so" and stops. A composed
// view has even less licence to do otherwise than a designed one -- the whole
// claim of memql#3320 is that a concept nobody wrote code for renders through
// the shared library, and markup written here for a row would be markup that
// only exists for views composed in this release.
//
// It is a COPY rather than an import because the predefined-view tree is a
// closed, guarded directory (portal_view_composition_test.go scans it and
// counts its files); reaching into it for a component would couple a surface
// under active parallel development to that guard's file list. The two are
// twelve lines of identical plumbing and one shared contract, and the contract
// -- renderElement, explainFit -- lives in view-kit where both read it.
//
// AN UNFIT ELEMENT EXPLAINS ITSELF, which matters more here than in a designed
// view. In a designed view an unfit element is a mistake somebody made once. In
// a composer it is a normal, expected state -- a person is trying elements
// against a concept, and the honest answer to "why is this one empty" is
// view-kit's own sentence, built from the element author's requirement prose.

export interface ComposeElementProps {
  element: ElementSpec;
  rows: readonly RowLike[];
  concept: ConceptLike;
  options?: ElementOptions;
  onSelect?: (rowId: string) => void;
}

export function ComposeElement({
  element,
  rows,
  concept,
  options,
  onSelect,
}: ComposeElementProps): ReactNode {
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
    [onSelect],
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
