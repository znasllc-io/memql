import { Link } from "react-router-dom";
import type { ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { useConcepts } from "../cluster/useConcepts";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";

// The concept registry, straight off ConceptsListMsg.
//
// Concept-agnostic by construction: nothing here knows the name of any
// concept. Whatever the cluster's DSL declares -- core domains, a product
// bundle mounted at MEMQL_DSL_PATH -- shows up the moment it is declared,
// which is the same property view-kit enforces one level down for rows.

export function ConceptsPage(): ReactNode {
  const { status } = useCluster();
  const { concepts, loading, error } = useConcepts();

  if (status !== "connected") {
    return (
      <Empty>Not connected to a cluster. See the connection state in the header.</Empty>
    );
  }
  if (error) return <ErrorMessage>Failed to list concepts: {error}</ErrorMessage>;
  if (loading) return <Loading what="concepts" />;
  if (concepts.length === 0) return <Empty>This cluster declares no concepts.</Empty>;

  return (
    <section>
      <h1 className="text-xl font-semibold tracking-tight">Concepts</h1>
      <p className="mt-1 text-sm text-muted">
        {concepts.length} declared on this cluster.
      </p>

      <ul className="mt-4 divide-y divide-line overflow-hidden rounded-lg border border-line bg-surface">
        {concepts.map((concept) => (
          <li key={concept.id}>
            <Link
              to={`/concepts/${encodeURIComponent(concept.id)}`}
              className="flex items-baseline gap-3 px-3 py-2 hover:bg-raised"
            >
              <span className="font-mono text-sm text-fg">{concept.entity}</span>
              <span className="font-mono text-xs text-subtle">{concept.id}</span>
              {concept.description ? (
                <span className="ml-auto max-w-md truncate text-xs text-muted">
                  {concept.description}
                </span>
              ) : null}
            </Link>
          </li>
        ))}
      </ul>
    </section>
  );
}
