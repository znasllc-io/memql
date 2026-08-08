import { useEffect, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useConcepts } from "../cluster/useConcepts";
import { RowList } from "../components/RowList";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";

// One concept's rows.
//
// The URL carries the concept id, so this page is deep-linkable and
// refresh-survivable -- which is a hard requirement of #3316 and the reason
// the shell routes at all rather than holding a selection in state. The Go
// handler's SPA fallback is the other half: a hard refresh on
// /portal/concepts/v1:cluster:node returns index.html and the router resolves
// the path client-side.

const PAGE_SIZE = 200;

export function ConceptRowsPage(): ReactNode {
  const { conceptId = "" } = useParams<{ conceptId: string }>();
  const { query, status } = useCluster();
  const { concepts, loading: conceptsLoading, error: conceptsError } = useConcepts();
  const [rows, setRows] = useState<Row[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  // The concept descriptor is needed for its @displayCard hints, which is
  // what makes the row list render field values rather than bare ids. It
  // comes from the same registry the list page shows; there is no
  // per-concept metadata fetch.
  const concept = concepts.find((c) => c.id === conceptId);

  useEffect(() => {
    if (query === null || conceptId === "") {
      setRows([]);
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void browseConceptPage(query, conceptId, { pageSize: PAGE_SIZE })
      .then((page) => {
        if (live) setRows(page.rows);
      })
      .catch((err: unknown) => {
        if (!live) return;
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, conceptId]);

  if (status !== "connected") {
    return (
      <Empty>Not connected to a cluster. See the connection state in the header.</Empty>
    );
  }

  return (
    <section>
      <Link to="/concepts" className="text-sm text-muted hover:text-fg">
        ← Concepts
      </Link>
      <h1 className="mt-2 font-mono text-xl font-semibold tracking-tight">{conceptId}</h1>
      {concept?.description ? (
        <p className="mt-1 text-sm text-muted">{concept.description}</p>
      ) : null}

      <div className="mt-4 rounded-lg border border-line bg-surface p-3">
        {conceptsError ? (
          <ErrorMessage>Failed to list concepts: {conceptsError}</ErrorMessage>
        ) : error ? (
          <ErrorMessage>Failed to load rows: {error}</ErrorMessage>
        ) : conceptsLoading || loading ? (
          <Loading what="rows" />
        ) : concept === undefined ? (
          <Empty>This cluster declares no concept with that id.</Empty>
        ) : (
          <RowList rows={rows} concept={concept} />
        )}
      </div>

      {/* Deliberately not a "Load more" button yet: paging, filtering and the
          detail pane are #3316. This page exists to prove the connection and
          the view-kit reuse end to end. */}
      {rows.length === PAGE_SIZE ? (
        <p className="mt-2 text-xs text-subtle">
          Showing the first {PAGE_SIZE} rows. Paging arrives with the full concept
          browser.
        </p>
      ) : null}
    </section>
  );
}
