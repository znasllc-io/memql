import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  profileConcept,
  sanitizeArrangement,
  type Arrangement,
  type ConceptLike,
  type PlannedEntry,
  type RowLike,
} from "@znasllc-io/memql-view-kit";

import { useCluster } from "../cluster/ClusterProvider";
import { useRowDetail } from "../cluster/useConceptRows";
import { useViewRows } from "../cluster/useViewRows";
import { RowDetailDialog } from "../components/RowDetailDialog";
import { ArrangementLayout } from "../compose/ArrangementLayout";
import { SectionHeader } from "../compose/ComposeLayout";
import { LiveBandPanel } from "../concepts/LiveBandPanel";
import { Empty, ErrorMessage } from "../components/StatusMessage";
import { PopulationMeta, Skeleton } from "../ui";
import { WIDGET_IDS, renderWidget } from "../widgets/registry";
import { SCENE_IDS, renderScene } from "../nexus/scene/registry";
import type { PageSection } from "./manifest";

// ONE SECTION of an arranged page: one concept, one walk, one arrangement.
//
// ===========================================================================
// THE SAME COMPONENT FOR A DESIGNED PAGE AND A COMPOSED ONE
// ===========================================================================
// This is what "predefined views become data" actually means at the code
// level. There is no branch here for "is this a designed view or a composed
// one" -- there is nowhere to put such a branch, because both hand this the
// same value: a concept id and an arrangement.
//
// ===========================================================================
// THE REGISTRIES ARE PASSED, NOT IMPORTED BY VIEW-KIT
// ===========================================================================
// sanitizeArrangement drops a `scene` or `widget` entry naming a module the
// build does not carry, and it can only do that if it is TOLD what the build
// carries. view-kit cannot know: it must not import three.js, and it has no
// React to mount a widget into. So the two registries are handed down here,
// which also means a surface that deliberately hosts neither (a preview, a
// server render) gets those entries repaired away rather than rendering
// view-kit's placeholder.

export interface ArrangedSectionProps {
  section: PageSection;
  // The arrangement to render -- the caller's resolved answer (an override
  // version, or the section's seed). Passed in rather than read here so
  // resolution happens ONCE per page rather than once per section, and so a
  // version strip previewing an old version needs no second code path.
  arrangement: Arrangement;
  // Whether this section owns the page's row selection. A page with several
  // sections has one selection, held above.
  selectedRowId: string;
  onSelect: (rowId: string) => void;
  // Rendered above the elements, below the section heading. What a page adds
  // that is not part of the arrangement -- the version strip, a notice.
  children?: ReactNode;
  // Shown when this section's concept is not published by the cluster.
  // Absent renders the standard sentence.
  missingStatement?: ReactNode;
  // Reports the concept and the rows this section actually loaded, upward.
  //
  // Regeneration needs a PROFILE, and a profile needs the rows the page
  // loaded -- which only the section knows. Reporting rather than lifting the
  // walk keeps one walk per section (a page with two sections has two, which
  // is right) instead of centralising a read that is genuinely per-section.
  onLoaded?: (conceptId: string, concept: ConceptLike, rows: readonly RowLike[]) => void;
}

export function ArrangedSection({
  section,
  arrangement,
  selectedRowId,
  onSelect,
  children,
  missingStatement,
  onLoaded,
}: ArrangedSectionProps): ReactNode {
  const { status } = useCluster();
  const data = useViewRows(section.conceptId);
  const concept = data.concept;
  const [nonce, setNonce] = useState(0);
  // The row a trailing View control opened. SEPARATE from the page's
  // selection, which is the whole point of `rowAction: "view"`: a page that
  // spends row-click on something else (Deployments picks a version to ship)
  // still needs a way to READ a row, and the two gestures must not collide.
  const [actionRowId, setActionRowId] = useState("");
  const onRowAction = useCallback((_action: string, rowId: string) => setActionRowId(rowId), []);
  const closeAction = useCallback(() => setActionRowId(""), []);
  const actionDetail = useRowDetail(section.conceptId, actionRowId);
  const onChanged = useCallback(() => {
    data.retry();
    setNonce((n) => n + 1);
  }, [data]);

  const live = useMemo(() => {
    if (concept === undefined) return undefined;
    const profile = profileConcept(concept, data.rows);
    // REPAIR, WITH THE PAGE'S OWN GUARDRAIL. `required` is what stops a
    // regeneration from producing a page that loads the right rows and no
    // longer does its job.
    return sanitizeArrangement(arrangement, profile, {
      required: section.required ?? [],
      scenes: SCENE_IDS,
      widgets: WIDGET_IDS,
    });
  }, [arrangement, concept, data.rows, section.required]);

  // Reported on every change of either, so a regeneration issued after a
  // "load more" sees the rows on screen rather than the first page.
  useEffect(() => {
    if (concept === undefined || onLoaded === undefined) return;
    onLoaded(section.conceptId, concept, data.rows);
  }, [concept, data.rows, section.conceptId, onLoaded]);

  const renderModule = useCallback(
    (planned: PlannedEntry): ReactNode => {
      if (concept === undefined) return null;
      const { element, options } = planned.entry;
      if (element === "widget") {
        const id = options?.["widgetId"] ?? "";
        return renderWidget(id, {
          concept,
          rows: data.rows,
          selectedRowId,
          onSelect,
          onChanged,
        });
      }
      if (element === "scene") {
        const id = options?.["sceneId"] ?? "";
        return renderScene(id, {
          concept,
          rows: data.rows,
          selectedRowId,
          onSelect,
        });
      }
      return null;
    },
    [concept, data.rows, selectedRowId, onSelect, onChanged],
  );

  if (data.registryError !== "") {
    return <ErrorMessage>Failed to list concepts: {data.registryError}</ErrorMessage>;
  }
  if (concept === undefined || live === undefined) {
    if (status !== "connected" && data.rows.length === 0) {
      return (
        <Empty>Not connected to a cluster. See the connection state in the header.</Empty>
      );
    }
    if (data.registryLoading) return <Skeleton variant="rows" rows={4} />;
    // A SECTION WHOSE CONCEPT IS NOT PUBLISHED IS A REAL STATE, not a
    // defensive branch: a node mounts the bundles it was given, and a portal
    // built against the engine can be pointed at a cluster publishing a
    // different set. Saying which concept is missing beats an empty page that
    // reads as "you have none of these".
    return (
      <Empty>
        {missingStatement ?? (
          <>
            This cluster publishes no concept called{" "}
            <code className="font-mono break-all">{section.conceptId}</code>, so there is
            nothing for this section to show.
          </>
        )}
      </Empty>
    );
  }

  return (
    <section className="flex min-w-0 flex-col gap-4" key={nonce}>
      {section.title === undefined ? null : (
        <SectionHeader
          conceptId={concept.id}
          entity={section.title}
          {...(section.meta === undefined
            ? {}
            : {
                meta: (
                  <span className="text-xs text-subtle">{section.meta}</span>
                ),
              })}
        />
      )}
      <PopulationMeta
        count={data.rows.length}
        status={data.walk.status}
        error={data.walk.error}
        onLoadMore={data.loadMore}
        onRetry={data.retry}
      />
      {children}
      {/* LIVE (memql#4539). Every arranged section holds a CDC subscription;
          the band is what it is for. It sits ABOVE the elements deliberately
          -- an arrival is news, and news at the bottom of a population is news
          nobody sees. */}
      {data.liveDegraded !== "" ? (
        <p className="rounded-lg border border-warn bg-warn-subtle/40 px-3 py-1.5 text-xs text-fg">
          Live updates are off for this section: {data.liveDegraded}. Rows already loaded
          are still correct; use Reload to see what has changed since.
        </p>
      ) : null}
      <LiveBandPanel
        band={data.live}
        concept={concept}
        onSelect={onSelect}
        onReload={data.reload}
        selectedRowId={selectedRowId}
      />
      <ArrangementLayout
        arrangement={live}
        concept={concept}
        rows={data.rows}
        onSelect={onSelect}
        selectedRowId={selectedRowId}
        onRowAction={onRowAction}
        renderModule={renderModule}
        // A band caption is a PAGE's second-level structure, under the page's
        // h1. It is the composer that nests one level deeper, because there a
        // section header already holds h2.
        headingLevel="h2"
      />
      <RowDetailDialog
        open={actionRowId !== ""}
        onClose={closeAction}
        rowId={actionRowId}
        row={actionDetail.row}
        loading={actionDetail.loading}
        error={actionDetail.error}
        missing={actionDetail.missing}
      />
    </section>
  );
}
