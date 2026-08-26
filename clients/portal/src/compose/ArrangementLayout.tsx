import type { ReactNode } from "react";
import {
  elementById,
  elementOptions,
  entryRole,
  planLayout,
  roleClassName,
  slotClassName,
  type Arrangement,
  type ConceptLike,
  type PlannedEntry,
  type RowLike,
} from "@znasllc-io/memql-view-kit";

import { ComposeBand } from "./ComposeLayout";
import { ComposeElement } from "./ComposeElement";

// Rendering an arrangement, in whichever of the five layouts it names.
//
// ===========================================================================
// THE SAME COMPONENT RENDERS THE PREVIEW AND THE SAVED VIEW
// ===========================================================================
// ...which is the whole point of an arrangement being plain data: what a
// person sees while composing is produced by the identical code path that will
// render the row they save, so "it looked right in the composer" cannot come
// apart from "it looks right when I open it". The composer passes `controls`
// to get its per-entry buttons; a saved view passes nothing.
//
// ===========================================================================
// WHERE THINGS GO IS NOT DECIDED HERE
// ===========================================================================
// view-kit's planLayout answers it, and it has to, because a second host --
// the VS Code panel, a server render -- placing entries by its own rules would
// mean "a dashboard" meant two different things in one product. What THIS file
// owns is the half view-kit cannot do: React, selection, keyboard handling,
// the resolved chart theme, and the band captions.
//
// This component replaced ArrangementBands, whose behaviour survives exactly
// as the `stack` layout -- the one every repair falls back to, so a regression
// in it is a regression in every view.
//
// An entry naming an element this build does not have is SAID OUT LOUD rather
// than skipped. A saved view is durable and the element library is not: a view
// composed against a release that shipped an element since removed should
// report the gap, not quietly render one band fewer and look fine.

export interface ArrangementLayoutProps {
  arrangement: Arrangement;
  concept: ConceptLike;
  rows: readonly RowLike[];
  onSelect?: (rowId: string) => void;
  selectedRowId?: string;
  // Per-entry chrome, right-aligned on the band caption. The composer's
  // move / remove / rebind controls; absent when the view is being read.
  controls?: (index: number) => ReactNode;
  // Notified when an element inside is clicked, with the entry's index. The
  // composer uses it to select an entry in the inspector; a saved view passes
  // nothing and the click does what it always did.
  onSelectEntry?: (index: number) => void;
  // The entry the inspector is currently on, outlined in the preview.
  selectedEntry?: number;
  // Scenes and widgets are HOSTED: view-kit names the element kind and this
  // application renders it. Undefined means this surface has none, which is
  // the honest state for a preview that has not been given the registries.
  renderModule?: (planned: PlannedEntry) => ReactNode;
}

export function ArrangementLayout({
  arrangement,
  concept,
  rows,
  onSelect,
  selectedRowId,
  controls,
  onSelectEntry,
  selectedEntry,
  renderModule,
}: ArrangementLayoutProps): ReactNode {
  if (arrangement.elements.length === 0) {
    return (
      <p className="text-sm text-subtle">
        This section has no elements yet. Add one from the list of what fits.
      </p>
    );
  }

  const plan = planLayout(arrangement);

  return (
    <div className={plan.className}>
      {plan.slots.map((slot) => (
        <div key={slot.slot} className={slotClassName(slot.slot)}>
          {slot.entries.map((planned) => (
            <ArrangedEntry
              key={`${planned.at}:${planned.entry.band}:${planned.entry.element}`}
              planned={planned}
              concept={concept}
              rows={rows}
              {...(onSelect === undefined ? {} : { onSelect })}
              {...(selectedRowId === undefined ? {} : { selectedRowId })}
              {...(controls === undefined ? {} : { controls })}
              {...(onSelectEntry === undefined ? {} : { onSelectEntry })}
              {...(selectedEntry === undefined ? {} : { selectedEntry })}
              {...(renderModule === undefined ? {} : { renderModule })}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

function ArrangedEntry({
  planned,
  concept,
  rows,
  onSelect,
  selectedRowId,
  controls,
  onSelectEntry,
  selectedEntry,
  renderModule,
}: {
  planned: PlannedEntry;
  concept: ConceptLike;
  rows: readonly RowLike[];
  onSelect?: (rowId: string) => void;
  selectedRowId?: string;
  controls?: (index: number) => ReactNode;
  onSelectEntry?: (index: number) => void;
  selectedEntry?: number;
  renderModule?: (planned: PlannedEntry) => ReactNode;
}): ReactNode {
  const { entry, at } = planned;
  const element = elementById(entry.element);
  const chosen = selectedEntry === at;

  const frame = (children: ReactNode, caption?: string): ReactNode => (
    <div
      className={roleClassName(entryRole(entry))}
      // The composer's click-to-select. A plain onClick rather than a button:
      // the element inside is already interactive (rows select, charts have
      // titles), and wrapping it in a button would swallow those. This
      // listens on the way UP, so an element that handled the click first
      // wins -- selecting a row does not also re-select the entry.
      {...(onSelectEntry === undefined
        ? {}
        : {
            onClick: () => onSelectEntry(at),
            "data-composer-entry": String(at),
          })}
    >
      <ComposeBand
        {...(caption === undefined ? {} : { title: caption })}
        {...(controls === undefined ? {} : { meta: controls(at) })}
        panel={entry.band === "roll"}
      >
        {chosen ? (
          <div className="rounded-lg outline-2 outline-offset-4 outline-accent">{children}</div>
        ) : (
          children
        )}
      </ComposeBand>
    </div>
  );

  if (element === undefined) {
    return frame(
      <p className="text-sm text-subtle">
        This view uses an element called{" "}
        <code className="font-mono">{entry.element}</code>, which this build of the
        portal does not have. The rest of the view is unaffected.
      </p>,
      entry.title ?? entry.element,
    );
  }

  // The opening reading carries no caption unless the composer wrote one: the
  // numbers are the label, and captioning a stat strip "Stat tiles" adds a
  // word and no information.
  const caption = entry.title ?? (entry.band === "reading" ? undefined : element.title);

  // A HOSTED kind -- a scene or a widget -- is rendered by the application,
  // never by view-kit. Falling through to ComposeElement would render
  // view-kit's own "this surface does not render scenes" placeholder, which is
  // correct for a host that HAS no scenes and misleading on one that does.
  const hosted = renderModule?.(planned);
  if (hosted !== undefined && hosted !== null) {
    return frame(hosted, caption);
  }

  return frame(
    <ComposeElement
      element={element}
      rows={rows}
      concept={concept}
      options={{
        ...elementOptions(entry),
        ...(selectedRowId === undefined || selectedRowId === ""
          ? {}
          : { selectedRowId }),
      }}
      {...(onSelect === undefined ? {} : { onSelect })}
    />,
    caption,
  );
}
