import { type ReactNode } from "react";
import { Link, Outlet, useMatch, useParams, useSearchParams } from "react-router-dom";

import { useAuth } from "../auth/AuthProvider";
import { useCluster } from "../cluster/ClusterProvider";
import { clusterDomainFor } from "../cluster/editorLink";
import { useConcepts } from "../cluster/useConcepts";
import { useConceptRows } from "../cluster/useConceptRows";
import { OpenInVsCode } from "../components/OpenInVsCode";
import { Empty } from "../components/StatusMessage";
import { Breadcrumbs, Callout, DataText, ErrorNotice, Skeleton, Tabs } from "../ui";
import { OriginBadge, isMirror } from "../dataorigins/OriginBadge";
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
  const { config } = useAuth();
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
  if (error) return <ErrorNotice sentence="Could not read this cluster's concepts." next="Reload the page; if it keeps failing the connection is the place to look." detail={error} />;
  if (concept === undefined) {
    if (loading) return <Skeleton variant="rows" rows={8} />;
    return (
      <section className="flex flex-col gap-4">
        <h1 className="text-xl font-semibold tracking-tight break-all"><DataText kind="id">{conceptId}</DataText></h1>
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
      <Breadcrumbs
        items={[
          { label: "Concepts", to: conceptsPath(search) },
          { label: concept.domain },
          { label: concept.entity },
        ]}
      />

      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h1 className="text-xl font-semibold tracking-tight">{concept.entity}</h1>
        <code className="font-mono text-sm break-all text-muted">{concept.id}</code>
        {concept.type ? <Chip>{concept.type}</Chip> : null}
        {concept.version ? <Chip>{concept.version}</Chip> : null}
        {/* What MemQL's relationship to this data IS (epic memql#4378).
            Rendered from the registry descriptor, never from the concept id
            -- a name convention would be a second answer to a question the
            server already answers. Renders nothing against a server that
            predates the fields. */}
        <OriginBadge
          dataState={concept.dataState}
          dataOrigin={concept.dataOrigin}
          dataMirroredTo={concept.dataMirroredTo}
        />
        <span className="basis-full" />
        <OpenInVsCode domain={clusterDomainFor(config)} kind="concept" name={concept.id} />
      </header>

      {concept.description ? (
        <p className="max-w-3xl text-sm text-muted">{concept.description}</p>
      ) : null}

      {/* A MIRROR IS READ-ONLY BY CONSTRUCTION (epic memql#4378). The engine
          refuses every write to it that does not come from its connector, so
          this says WHY before an operator finds out by being refused. The
          server is the enforcement; this is the courtesy. */}
      {isMirror(concept) ? (
        <Callout tone="warn" title={`Read-only: a mirror of ${concept.dataOrigin ?? ""}`}>
          MemQL holds a faithful copy and does not own it — change the record at the origin and
          MemQL&apos;s copy follows. Only that system&apos;s connector writes these rows, so an edit
          here would be refused.
        </Callout>
      ) : null}

      {/* Tabs are ROUTES, so a schema view is a link someone can send. Active
          state is passed explicitly: /rows/:rowId IS the rows view, which
          path matching alone cannot express (see the useMatch above). */}
      <Tabs
        label="Concept views"
        items={[
          { to: conceptPath(concept.id, search), label: "Rows", active: !onSchema },
          { to: conceptSchemaPath(concept.id, search), label: "Schema", active: onSchema },
        ]}
      />

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
