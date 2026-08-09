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

// THE ONLY WAY A PREDEFINED VIEW PUTS ROWS ON SCREEN.
//
// A view says "these rows, through this element, bound like so" and stops. It
// never builds markup for a row itself -- that is the constraint that keeps a
// designed view a LAYOUT CHOICE over the shared element library rather than a
// second renderer that drifts. The rule is enforced by
// portal_view_composition_test.go in the repo root; this component is the
// sanctioned way to satisfy it.
//
// @displayCard IS READ HERE, TRANSITIVELY AND ONLY HERE. renderElement
// profiles the concept (resolveDisplayCard -> declared card, else view-kit's
// documented inference), resolves each of the element's requirement slots
// against that profile, and hands the element the concrete field names it
// bound. So a view expresses "put whatever this concept calls its status in
// the badge" as an element requirement, and never as `row.status`.
//
// AN UNFIT ELEMENT EXPLAINS ITSELF. renderElement returns undefined when the
// element cannot render these rows -- a timeline over rows with no timestamp,
// a rail over rows with no category. Rather than a blank panel, the view shows
// view-kit's OWN sentence for why, built from the element author's requirement
// prose (explainFit). That is the honest failure for a predefined view: the
// designer picked an element for a shape the data does not have today, and the
// screen says which requirement went unmet instead of looking broken.

export interface ViewElementProps {
  element: ElementSpec;
  rows: readonly RowLike[];
  concept: ConceptLike;
  // Slot bindings, sort, month, selection. A predefined view uses `bindings`
  // to name the field it designed around ("group these agents by kind"), which
  // is the one thing a designed view knows that a generic one cannot.
  options?: ElementOptions;
  // Called with the clicked row's id. Omit for an element that is a reading
  // rather than a list -- a stat strip has no rows to open.
  onSelect?: (rowId: string) => void;
}

export function ViewElement({
  element,
  rows,
  concept,
  options,
  onSelect,
}: ViewElementProps): ReactNode {
  // view-kit ships its sheet as a string; installing it in an effect keeps it
  // out of render and idempotent (src/viewkit/styles.ts).
  useEffect(() => {
    ensureViewKitStyles();
  }, []);

  // Attribute-driven, exactly as the concept browser's row list does it: any
  // element view-kit emits carrying `data-row-id` becomes focusable and
  // keyboard-operable, whatever its tag. Keying off the attribute rather than
  // the tag is what lets one callback serve a table row, a board card, a
  // timeline entry and a list item.
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

  // The chart palette is the one thing view-kit cannot resolve from CSS alone:
  // it ships a validated palette per theme and picks between them with
  // prefers-color-scheme, which is the wrong answer for an operator who chose
  // dark on a light OS. Stamping the RESOLVED theme here means every chart in
  // every view is correct in both, and no view has to remember.
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
