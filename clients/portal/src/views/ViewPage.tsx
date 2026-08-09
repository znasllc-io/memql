import { useCallback, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { useCluster } from "../cluster/ClusterProvider";
import { useViewRows } from "../cluster/useViewRows";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { conceptPath } from "../concepts/urls";
import { AgentsView } from "./AgentsView";
import { AuditView } from "./AuditView";
import { CustomersView } from "./CustomersView";
import { DeploymentsView } from "./DeploymentsView";
import { PeopleView } from "./PeopleView";
import { viewById, type ViewDefinition } from "./registry";
import { PopulationMeta, RowAside, ViewFrame, type ViewProps } from "./ViewLayout";
import { viewRowPath } from "./urls";

// The route element behind /views/:viewId -- resolve the slug, own the walk,
// and hand the body a population.
//
// EVERYTHING BEFORE THE BODY IS THE SAME FOR ALL FIVE, and that is the point
// of having a page rather than five routes: the not-connected state, the
// registry error, the "this cluster declares no such concept" state, the
// header, the walk footer and the row-detail aside are one implementation.
// What differs between views is which elements go in which band, which is
// exactly what each body module contains and nothing else.

// Slug -> body. A lookup rather than a switch so adding a view is one row in
// registry.ts and one row here, and so a slug with no body fails loudly
// instead of rendering an empty frame.
const BODIES: Readonly<Record<string, (props: ViewProps) => ReactNode>> = {
  people: PeopleView,
  agents: AgentsView,
  customers: CustomersView,
  deployments: DeploymentsView,
  audit: AuditView,
};

export function ViewPage(): ReactNode {
  const { viewId = "", rowId = "" } = useParams<{ viewId: string; rowId: string }>();
  const { status } = useCluster();
  const navigate = useNavigate();

  const view = viewById(viewId);
  // Hooks run before any early return -- hook order cannot vary between
  // renders. An empty concept id parks the walk in idle and issues no request,
  // which is the right behaviour for an unknown slug anyway.
  const data = useViewRows(view?.conceptId ?? "");

  const onSelect = useCallback(
    (id: string) => navigate(viewRowPath(viewId, id)),
    [navigate, viewId],
  );

  if (view === undefined) {
    return (
      <section className="mx-auto max-w-2xl">
        <h1 className="text-xl font-semibold tracking-tight">No such view</h1>
        <p className="mt-2 text-sm text-muted">
          The portal has no view called “{viewId}”. Views are a fixed set; every
          other concept is browsable in the registry.
        </p>
      </section>
    );
  }

  if (status !== "connected" && data.rows.length === 0 && data.concept === undefined) {
    return (
      <Empty>Not connected to a cluster. See the connection state in the header.</Empty>
    );
  }
  if (data.registryError) {
    return <ErrorMessage>Failed to list concepts: {data.registryError}</ErrorMessage>;
  }
  if (data.concept === undefined) {
    if (data.registryLoading) return <Loading what="the concept registry" />;
    return <MissingConcept view={view} />;
  }

  const Body = BODIES[view.id];
  if (Body === undefined) {
    return (
      <ErrorMessage>
        The view “{view.id}” is declared but has no body. This is a bug in the
        portal, not a problem with your cluster.
      </ErrorMessage>
    );
  }

  return (
    <ViewFrame
      view={view}
      conceptId={view.conceptId}
      meta={
        <PopulationMeta
          count={data.rows.length}
          status={data.walk.status}
          error={data.walk.error}
          onLoadMore={data.loadMore}
          onRetry={data.retry}
        />
      }
      aside={
        rowId ? <RowAside view={view} conceptId={view.conceptId} rowId={rowId} /> : undefined
      }
    >
      <Body
        view={view}
        concept={data.concept}
        rows={data.rows}
        selectedRowId={rowId}
        onSelect={onSelect}
      />
    </ViewFrame>
  );
}

// The honest state when a view's concept is not on this cluster.
//
// It is a real possibility rather than a defensive branch: a node mounts the
// product bundles it was given, and a portal built against the engine can be
// pointed at a cluster that publishes a different set. Saying which concept is
// missing -- and offering the registry, which shows what IS published -- beats
// an empty page that reads as "you have no customers".
function MissingConcept({ view }: { view: ViewDefinition }): ReactNode {
  return (
    <section className="mx-auto max-w-2xl">
      <h1 className="text-xl font-semibold tracking-tight">{view.title}</h1>
      <p className="mt-2 text-sm text-muted">
        This cluster publishes no concept called{" "}
        <code className="font-mono break-all">{view.conceptId}</code>, so there is
        nothing for this view to show. The node may not mount the bundle that
        declares it.
      </p>
      <Link
        to={conceptPath(view.conceptId)}
        className="mt-4 inline-block text-sm text-accent hover:underline"
      >
        Look it up in the registry
      </Link>
    </section>
  );
}
