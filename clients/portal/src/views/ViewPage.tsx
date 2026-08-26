import { useCallback, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { RowDetailDialog } from "../components/RowDetailDialog";
import { useRowDetail } from "../cluster/useConceptRows";
import { useAdminAccess } from "../admin/useAdminConsole";
import { ArrangedPage } from "../pages/ArrangedPage";
import { PersonActions } from "../people/PersonActions";
import { Container, EmptyState, PageHeader } from "../ui";
import { conceptsPath } from "../concepts/urls";
import { viewById, viewPageId } from "./registry";
import { viewPath, viewRowPath } from "./urls";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// The route element behind /views/:viewId.
//
// ===========================================================================
// WHAT IS LEFT HERE AFTER THE VIEWS BECAME DATA (epic memql#4661)
// ===========================================================================
// Almost nothing, and that is the result. This module used to resolve a slug
// to one of five REACT BODIES, own the walk, and render a frame around
// whichever body it picked. The bodies are gone; the walk and the frame are
// ArrangedPage's, which is the same component a composed view and a converged
// page render through.
//
// What survives is what is genuinely about THIS ROUTE: resolving the slug,
// the honest state for a slug that names no view, and the row-detail dialog
// the /views/:viewId/:rowId address opens -- plus the one thing that is
// genuinely per-view, the person actions on a user's row detail.
export function ViewPage(): ReactNode {
  const { canAdminister } = useAdminAccess();
  const { viewId = "", rowId = "" } = useParams<{ viewId: string; rowId: string }>();
  const navigate = useNavigate();

  const view = viewById(viewId);
  const onSelect = useCallback(
    (id: string) => navigate(viewRowPath(viewId, id)),
    [navigate, viewId],
  );
  const onCloseRow = useCallback(() => navigate(viewPath(viewId)), [navigate, viewId]);

  // Deployments spends the row selection on deploy/rollback, so its selection
  // must NOT also open a dialog. Hooks run before any early return -- hook
  // order cannot vary between renders -- so this is computed unconditionally
  // and an empty concept id parks the read in idle.
  const dialogRowId = view !== undefined && view.id !== "deployments" ? rowId : "";
  // Bumped by a write from the row's own action panel, so the dialog re-reads
  // the row it just changed and stays open. Closing it instead -- which this
  // did briefly -- makes a second decision about the same person cost a second
  // navigation, and suspending somebody right after changing their role is
  // exactly the two-decision case the panel is built for.
  const [writeNonce, setWriteNonce] = useState(0);
  const detail = useRowDetail(view?.conceptId ?? "", dialogRowId, writeNonce);

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

  return (
    <>
      <ArrangedPage
        manifest={view.seed}
        pageId={viewPageId(view.id)}
        selectedRowId={rowId}
        onSelect={onSelect}
      />
      <RowDetailDialog
        open={dialogRowId !== ""}
        onClose={onCloseRow}
        rowId={dialogRowId}
        row={detail.row}
        loading={detail.loading}
        error={detail.error}
        missing={detail.missing}
        {...(view.id === "users" && canAdminister && detail.row !== null
          ? {
              // onChanged RE-READS the row and leaves the dialog open. The
              // section's own live subscription updates the list underneath;
              // what the dialog needs is the row it just wrote, so the panel
              // reflects it -- a suspend turns the button into "Reinstate"
              // without the person navigating anywhere.
              actions: (
                <PersonActions
                  person={detail.row as Row}
                  onChanged={() => setWriteNonce((n) => n + 1)}
                />
              ),
            }
          : {})}
      />
    </>
  );
}
