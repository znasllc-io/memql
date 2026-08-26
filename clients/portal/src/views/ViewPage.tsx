import { useCallback, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useRowDetail } from "../cluster/useConceptRows";
import { useViewRows } from "../cluster/useViewRows";
import { LiveBandPanel } from "../concepts/LiveBandPanel";
import { RowDetailDialog } from "../components/RowDetailDialog";
import { InvitePerson, PendingInvitations } from "../people/InvitePerson";
import { PersonActions } from "../people/PersonActions";
import { usePendingInvitations } from "../people/usePendingInvitations";
import { useAdminAccess, useAdminWrites } from "../admin/useAdminConsole";
import { Empty } from "../components/StatusMessage";
import { Band, Container, EmptyState, ErrorNotice, PageHeader, Skeleton } from "../ui";
import { conceptPath, conceptsPath } from "../concepts/urls";
import { AccountsView } from "./AccountsView";
import { AgentsView } from "./AgentsView";
import { AuditView } from "./AuditView";
import { DeploymentsView } from "./DeploymentsView";
import { UsersView } from "./UsersView";
import { viewById, type ViewDefinition } from "./registry";
import { PopulationMeta, ViewFrame, type ViewProps } from "./ViewLayout";
import { viewPath, viewRowPath } from "./urls";

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
  users: UsersView,
  agents: AgentsView,
  accounts: AccountsView,
  deployments: DeploymentsView,
  audit: AuditView,
};

export function ViewPage(): ReactNode {
  const { canAdminister } = useAdminAccess();
  const { viewId = "", rowId = "" } = useParams<{ viewId: string; rowId: string }>();
  // The Users view is where a person is ADDED as well as read (memql#4272).
  // The verbs live outside src/views/ for the reason PersonActions states; the
  // view supplies the slot.
  const usersAdmin = canAdminister && viewId === "users";
  const invitations = usePendingInvitations(usersAdmin);
  const inviteWrites = useAdminWrites();
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
  const onCloseRow = useCallback(() => navigate(viewPath(viewId)), [navigate, viewId]);
  // Deployments spends rowId on deploy/rollback. That view hosts its own
  // Dialog from a trailing View control; do not open one just because the
  // URL carries a selection.
  const rowDialog = view?.id !== "deployments" ? rowId : "";
  const detail = useRowDetail(view?.conceptId ?? "", rowDialog);

  if (view === undefined) {
    return (
      <Container>
        <section className="flex flex-col gap-6">
          <PageHeader title="No such view" />
          <EmptyState
            statement={
              <>
                The portal has no view called “{viewId}”. The predefined views are
                a fixed set; every other concept is browsable in the registry.
              </>
            }
            action={
              <Link to={conceptsPath()} className="text-sm text-accent hover:underline">
                Browse the concept registry
              </Link>
            }
          />
        </section>
      </Container>
    );
  }

  if (status !== "connected" && data.rows.length === 0 && data.concept === undefined) {
    return (
      <Empty>Not connected to a cluster. See the connection state in the header.</Empty>
    );
  }
  if (data.registryError) {
    return <ErrorNotice sentence="Could not read the concept registry, so this view cannot be drawn." detail={data.registryError} />;
  }
  if (data.concept === undefined) {
    if (data.registryLoading) return <Skeleton variant="rows" rows={4} />;
    return <MissingConcept view={view} />;
  }

  const Body = BODIES[view.id];
  if (Body === undefined) {
    return (
      <ErrorNotice
        sentence={<>The view “{view.id}” is declared but has no body.</>}
        next="This is a bug in the portal, not a problem with your cluster."
      />
    );
  }

  return (
    <ViewFrame
      view={view}
      meta={
        <PopulationMeta
          count={data.rows.length}
          status={data.walk.status}
          error={data.walk.error}
          onLoadMore={data.loadMore}
          onRetry={data.retry}
        />
      }
      {...(usersAdmin ? { actions: <InvitePerson onInvited={invitations.reload} /> } : {})}
    >
      {/* LIVE (memql#4539). This view already held a CDC subscription and
          discarded every event; the band is what it was for. It sits ABOVE the
          body deliberately -- an arrival is news, and news at the bottom of a
          population is news nobody sees. */}
      {data.liveDegraded !== "" ? (
        <p className="mb-3 rounded-lg border border-warn bg-warn-subtle/40 px-3 py-1.5 text-xs text-fg">
          Live updates are off for this view: {data.liveDegraded}. Rows already loaded are
          still correct; use Reload to see what has changed since.
        </p>
      ) : null}
      <LiveBandPanel
        band={data.live}
        concept={data.concept}
        onSelect={onSelect}
        onReload={data.reload}
        selectedRowId={rowId}
      />
      <Body
        view={view}
        concept={data.concept}
        rows={data.rows}
        selectedRowId={rowId}
        onSelect={onSelect}
      />
      {/* AFTER the body, deliberately. A predefined view's bands read
          reading -> shape -> roll, and who is on their way IN is an
          administrative addendum to that population rather than part of its
          grammar -- putting it on top would push the reading below the fold
          for the operator who came to look at users. */}
      {usersAdmin ? (
        <Band title="Invited" meta="pending invitations, newest first — live">
          {invitations.error !== "" ? (
            <ErrorNotice sentence="Could not read the outstanding invitations." detail={invitations.error} />
          ) : (
            <PendingInvitations
              rows={invitations.rows}
              writes={inviteWrites}
              onChanged={invitations.reload}
            />
          )}
        </Band>
      ) : null}

      <RowDetailDialog
        open={rowDialog !== ""}
        onClose={onCloseRow}
        rowId={rowDialog}
        row={detail.row}
        loading={detail.loading}
        error={detail.error}
        missing={detail.missing}
        {...(view.id === "users" && canAdminister && detail.row !== null
          ? {
              actions: (
                <PersonActions person={detail.row as Row} onChanged={data.retry} />
              ),
            }
          : {})}
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
// an empty page that reads as "you have no accounts".
function MissingConcept({ view }: { view: ViewDefinition }): ReactNode {
  return (
    <Container>
      <section className="flex flex-col gap-6">
        <PageHeader title={view.title} />
        <EmptyState
          statement={
            <>
              This cluster publishes no concept called{" "}
              <code className="font-mono break-all">{view.conceptId}</code>, so
              there is nothing for this view to show. The node may not mount the
              bundle that declares it.
            </>
          }
          action={
            <Link
              to={conceptPath(view.conceptId)}
              className="text-sm text-accent hover:underline"
            >
              Look it up in the registry
            </Link>
          }
        />
      </section>
    </Container>
  );
}
