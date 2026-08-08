import { type ReactNode } from "react";
import { Link, Outlet, useMatch, useParams, useSearchParams } from "react-router-dom";

import { useCluster } from "../cluster/ClusterProvider";
import { useConcepts } from "../cluster/useConcepts";
import { useConceptRows } from "../cluster/useConceptRows";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import type { ConceptPaneContext } from "./conceptContext";
import {
  SCHEMA_ROUTE_PATTERN,
  conceptPath,
  conceptSchemaPath,
  conceptsPath,
} from "../concepts/urls";

// One concept's workspace: identity, schema, rows, and a row's detail.
//
// THE PARENT OWNS THE DATA. useConceptRows lives here rather than in the rows
// pane because the rows pane is mounted at two routes -- with and without a
// selected row -- and a walk owned by the pane would restart paging every
// time an operator opened a row. Owned above the <Outlet>, the walk survives
// navigation between a concept's panes; only leaving the concept resets it.
//
// The concept DESCRIPTOR comes from the registry the list page already read
// (ConceptsListMsg), not from a per-concept metadata fetch -- there is no such
// call, and inventing one would be a second source of truth for the display
// card.
//
// A deep link straight to this page is the interesting case, and it works:
// the registry load is a hook on the same connection, so arriving here cold
// shows "loading", then either the concept or an honest "this cluster
// declares no concept with that id".

export function ConceptPage(): ReactNode {
  const { conceptId = "" } = useParams<{ conceptId: string }>();
  const { status } = useCluster();
  const { concepts, loading, error } = useConcepts();
  const [params] = useSearchParams();
  const search = params.toString();

  // Hooks run before any early return -- hook order cannot vary between
  // renders, and this one is genuinely wanted even while the registry is
  // still loading: the walk keys off the concept id from the URL, which is
  // known immediately.
  const rows = useConceptRows(conceptId);

  // Which tab is active is decided by the ROUTE, not by NavLink's own
  // matching. NavLink's `end` would light the Rows tab only at the concept
  // root and go dark the moment a row is opened -- but /rows/:rowId IS the
  // rows view, with a detail pane. The schema route is the single
  // discriminator, so ask about it directly.
  const onSchema = useMatch(SCHEMA_ROUTE_PATTERN) !== null;

  const concept = concepts.find((candidate) => candidate.id === conceptId);

  if (status !== "connected" && concepts.length === 0) {
    return (
      <Empty>Not connected to a cluster. See the connection state in the header.</Empty>
    );
  }
  if (error) return <ErrorMessage>Failed to list concepts: {error}</ErrorMessage>;
  if (concept === undefined) {
    if (loading) return <Loading what="the concept registry" />;
    return (
      <section className="mx-auto max-w-2xl">
        <h1 className="font-mono text-lg font-semibold break-all">{conceptId}</h1>
        <p className="mt-2 text-sm text-muted">
          This cluster declares no concept with that id. It may belong to a product
          bundle this node does not mount, or the link may predate a rename.
        </p>
        <Link
          to={conceptsPath()}
          className="mt-4 inline-block text-sm text-accent hover:underline"
        >
          Back to the registry
        </Link>
      </section>
    );
  }

  const context: ConceptPaneContext = { concept, rows };

  return (
    <section className="flex min-h-full flex-col gap-4">
      <nav aria-label="Breadcrumb" className="text-xs text-muted">
        <Link to={conceptsPath(search)} className="hover:text-fg hover:underline">
          Concepts
        </Link>
        <span className="mx-1.5 text-subtle">/</span>
        <span className="text-fg">{concept.domain}</span>
        <span className="mx-1.5 text-subtle">/</span>
        <span className="text-fg">{concept.entity}</span>
      </nav>

      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h1 className="text-xl font-semibold tracking-tight">{concept.entity}</h1>
        <code className="font-mono text-sm break-all text-muted">{concept.id}</code>
        {concept.type ? <Chip>{concept.type}</Chip> : null}
        {concept.version ? <Chip>{concept.version}</Chip> : null}
      </header>

      {concept.description ? (
        <p className="max-w-3xl text-sm text-muted">{concept.description}</p>
      ) : null}

      {/* Tabs are ROUTES, so a schema view is a link someone can send. */}
      <nav aria-label="Concept views" className="flex gap-1 border-b border-line">
        <Tab to={conceptPath(concept.id, search)} active={!onSchema}>
          Rows
        </Tab>
        <Tab to={conceptSchemaPath(concept.id, search)} active={onSchema}>
          Schema
        </Tab>
      </nav>

      <div className="min-h-0 flex-1">
        <Outlet context={context} />
      </div>
    </section>
  );
}

function Chip({ children }: { children: ReactNode }): ReactNode {
  return (
    <span className="rounded-full border border-line px-2 py-0.5 font-mono text-xs text-muted">
      {children}
    </span>
  );
}

function Tab({
  to,
  active,
  children,
}: {
  to: string;
  active: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <Link
      to={to}
      aria-current={active ? "page" : undefined}
      className={
        "-mb-px border-b-2 px-3 py-1.5 text-sm " +
        (active
          ? "border-accent font-medium text-fg"
          : "border-transparent text-muted hover:text-fg")
      }
    >
      {children}
    </Link>
  );
}
