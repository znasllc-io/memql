import type { ReactNode } from "react";
import {
  elementById,
  elementOptions,
  type Arrangement,
  type ConceptLike,
  type RowLike,
} from "@znasllc-io/memql-view-kit";

import { ComposeBand } from "./ComposeLayout";
import { ComposeElement } from "./ComposeElement";

// Rendering an arrangement.
//
// THE SAME COMPONENT RENDERS THE PREVIEW AND THE SAVED VIEW, which is the
// whole point of an arrangement being plain data: what a person sees while
// composing is produced by the identical code path that will render the row
// they save, so "it looked right in the composer" cannot come apart from "it
// looks right when I open it". The composer passes `controls` to get its
// per-entry buttons; the saved view passes nothing.
//
// An entry naming an element this build does not have is SAID OUT LOUD rather
// than skipped. A saved view is durable and the element library is not: a view
// composed against a release that shipped an element since removed should
// report the gap, not quietly render one band fewer and look fine.

export interface ArrangementBandsProps {
  arrangement: Arrangement;
  concept: ConceptLike;
  rows: readonly RowLike[];
  onSelect?: (rowId: string) => void;
  // Per-entry chrome, right-aligned on the band caption. The composer's
  // move / remove / rebind controls; absent when the view is being read.
  controls?: (index: number) => ReactNode;
}

export function ArrangementBands({
  arrangement,
  concept,
  rows,
  onSelect,
  controls,
}: ArrangementBandsProps): ReactNode {
  if (arrangement.elements.length === 0) {
    return (
      <p className="text-sm text-subtle">
        This section has no elements yet. Add one from the list of what fits.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      {arrangement.elements.map((entry, index) => {
        const element = elementById(entry.element);
        const key = `${index}:${entry.band}:${entry.element}`;
        if (element === undefined) {
          return (
            <ComposeBand key={key} title={entry.title ?? entry.element}>
              <p className="text-sm text-subtle">
                This view uses an element called{" "}
                <code className="font-mono">{entry.element}</code>, which this build of
                the portal does not have. The rest of the view is unaffected.
              </p>
            </ComposeBand>
          );
        }
        // The opening reading carries no caption unless the composer wrote
        // one: the numbers are the label, and captioning a stat strip
        // "Stat tiles" adds a word and no information.
        const caption =
          entry.title ?? (entry.band === "reading" ? undefined : element.title);
        return (
          <ComposeBand
            key={key}
            {...(caption === undefined ? {} : { title: caption })}
            {...(controls === undefined ? {} : { meta: controls(index) })}
            panel={entry.band === "roll"}
          >
            <ComposeElement
              element={element}
              rows={rows}
              concept={concept}
              options={elementOptions(entry)}
              {...(onSelect === undefined ? {} : { onSelect })}
            />
          </ComposeBand>
        );
      })}
    </div>
  );
}
