import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { sanitizeArrangement, profileConcept, type Arrangement } from "@znasllc-io/memql-view-kit";

import { useCluster } from "../cluster/ClusterProvider";
import { useViewRows } from "../cluster/useViewRows";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { conceptPath } from "../concepts/urls";
import { ArrangementBands } from "./ArrangementBands";
import { PopulationMeta, SectionHeader } from "./ComposeLayout";
import { composedViewEditPath } from "./urls";
import { useSavedView } from "./useSavedViews";
import { VIEW_CONCEPT_ID, type SavedView } from "./savedViews";

// A saved view, opened.
//
// It renders through exactly the components the composer previewed with, over
// exactly the rows the concept browser would show, so nothing about a view
// changes between composing it and reading it back.
//
// THE STORED ARRANGEMENT IS REPAIRED, NOT TRUSTED. A saved view is durable and
// the world around it is not: rows change shape as a product evolves, an
// element can be removed from the library, a concept can stop being published.
// So each section re-validates its stored arrangement against the LIVE rows
// (sanitizeArrangement) before rendering -- an entry that cannot render is
// dropped, and a section left with nothing falls back to the deterministic
// arrangement for those rows. A view composed a year ago still opens.
//
// The row is NOT rewritten by that repair. What is stored is what the person
// saved; the repair is a rendering decision, and re-opening the view in the
// composer is where a person chooses to make it permanent.

export function ComposedViewPage(): ReactNode {
  const { viewId = "" } = useParams<{ viewId: string }>();
  const { view, loading, error, missing } = useSavedView(viewId);

  if (error) return <ErrorMessage>Failed to read the view: {error}</ErrorMessage>;
  if (loading) return <Loading what="the view" />;
  if (missing || view === null) {
    return (
      <section className="mx-auto max-w-2xl">
        <h1 className="text-xl font-semibold tracking-tight">No such view</h1>
        <p className="mt-2 text-sm text-muted">
          You have no saved view with that id. Composed views are owned rows, so a
          link to somebody else&rsquo;s view will land here too.
        </p>
      </section>
    );
  }

  return (
    <section className="flex min-h-full flex-col gap-8 pb-8">
      <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3 border-b border-line pb-4">
        <div className="min-w-0">
          <p className="font-mono text-xs break-all text-subtle">{VIEW_CONCEPT_ID}</p>
          <h1 className="mt-1 text-xl font-semibold tracking-tight">{view.name}</h1>
          {view.description === "" ? null : (
            <p className="mt-1 max-w-2xl text-sm text-muted">{view.description}</p>
          )}
          <p className="mt-1 text-xs text-subtle">
            Composed by you
            {view.origin === "suggested" ? ", from a suggested arrangement" : ""}
            {view.updatedAt === "" ? "" : ` · updated ${view.updatedAt}`}
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-3 text-sm">
          <Link to={composedViewEditPath(view.id)} className="text-accent hover:underline">
            Edit
          </Link>
          {/* The saved view is a row, and saying so with a working link is the
              difference between claiming the portal reads its own data back and
              demonstrating it. */}
          <Link
            to={conceptPath(VIEW_CONCEPT_ID)}
            className="text-muted hover:text-fg hover:underline"
          >
            See it as a row
          </Link>
        </div>
      </header>

      {view.arrangements.length === 0 ? (
        <Empty>This view has no sections.</Empty>
      ) : (
        view.arrangements.map((arrangement) => (
          <SavedSection key={arrangement.conceptId} arrangement={arrangement} view={view} />
        ))
      )}
    </section>
  );
}

// One concept's section of a saved view. Its own component for the same reason
// the composer's is: it owns a keyset walk, and a hook cannot be called in a
// loop of varying length.
function SavedSection({
  arrangement,
  view,
}: {
  arrangement: Arrangement;
  view: SavedView;
}): ReactNode {
  const { status } = useCluster();
  const data = useViewRows(arrangement.conceptId);
  const concept = data.concept;

  if (data.registryError) {
    return <ErrorMessage>Failed to list concepts: {data.registryError}</ErrorMessage>;
  }
  if (concept === undefined) {
    if (status !== "connected") {
      return <Empty>Not connected to a cluster. See the connection state in the header.</Empty>;
    }
    if (data.registryLoading) return <Loading what="the concept registry" />;
    return (
      <Empty>
        This cluster no longer publishes{" "}
        <code className="font-mono break-all">{arrangement.conceptId}</code>, so this
        section of &ldquo;{view.name}&rdquo; has nothing to show.
      </Empty>
    );
  }

  const profile = profileConcept(concept, data.rows);
  const live = sanitizeArrangement(arrangement, profile);

  return (
    <section className="flex min-w-0 flex-col gap-5">
      <SectionHeader
        conceptId={concept.id}
        entity={concept.entity}
        meta={
          <PopulationMeta
            count={data.rows.length}
            status={data.walk.status}
            error={data.walk.error}
            onLoadMore={data.loadMore}
            onRetry={data.retry}
          />
        }
      />
      <ArrangementBands arrangement={live} concept={concept} rows={data.rows} />
    </section>
  );
}
