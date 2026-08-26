import { useCallback, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import {
  CHECKLIST_ELEMENT,
  PROPORTION_BAR_ELEMENT,
  TABLE_ELEMENT,
  TIMELINE_ELEMENT,
} from "@znasllc-io/memql-view-kit";

import {
  COMPONENT_CONCEPT,
  GATE_LEG_CONCEPT,
  ROLLOUT_CONCEPT,
  componentRows,
  gateLegRows,
  rolloutRows,
} from "../deploy/rows";
import { DeploymentOps } from "../deploy/DeploymentOps";
import { ReleasesCard } from "../deploy/releases/ReleasesCard";
import { useReleases } from "../deploy/releases/useReleases";
import { useDeployConsole } from "../deploy/useDeployConsole";
import { useRowDetail } from "../cluster/useConceptRows";
import { RowDetailDialog } from "../components/RowDetailDialog";
import { overlayAbsenceOf, overlayAbsenceStatement } from "../deploy/noOverlay";
import { DataText, ErrorNotice } from "../ui";
import { ViewElement } from "./ViewElement";
import { Band, type ViewProps } from "./ViewLayout";

// Deployments: what is running, what shipped before it, and whether it is
// safe to ship again.
//
// LAYOUT RATIONALE. The only view built from TWO sources, and the bands are
// ordered by how urgent each is rather than by where it comes from. What is
// live right now (the gate, then the images) sits above the history, because
// an operator opening this page mid-incident needs "did the gate pass" before
// they need "what shipped in June". The history is a TIMELINE for the same
// reason the audit trail is: a deployment record is append-only and every
// status transition appends to one row's timeline, so the sequence IS the
// data.
//
// THE GATE IS A CHECKLIST, and that is the whole argument for adapting
// DeployControlService's payloads into row sets (src/deploy/rows.ts): a gate
// leg is a named thing that passed or did not, which is exactly what the
// checklist element renders, so it renders here through the same element,
// with the same theming, instead of a bespoke list of ticks.
//
// AUTHORIZATION. Three floors, all enforced server-side
// (component/deploycontrol/service.go) and all only HIDDEN here: view is
// admin+, cut and deploy are developer+, rollback is owner only. A caller
// below a floor sees the action absent rather than present-and-refused. The
// gate itself lives in src/deploy/useDeployConsole.ts, with a longer note on
// why a copy of the matrix in the UI is a courtesy and not a control.

export function DeploymentsView({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: ViewProps): ReactNode {
  const console_ = useDeployConsole();
  // Cutting a version of MemQL ITSELF (epic memql#4434), which is a different
  // axis from everything else on this page: the bands below move THIS cluster
  // onto a version that exists, and this one brings a version into existence
  // for every installation. Same page because an operator asks both questions
  // in one sitting -- "what is running" and "is there something newer to run".
  const releases = useReleases();

  const { permissions, status, loading, error, actionMessage, actionError, actionAuditEventId } =
    console_;
  // Which of the two "no overlay" situations this node is in, "" when neither.
  const overlayAbsence = overlayAbsenceOf(error);
  const selection = selectedRowId ? { selectedRowId } : {};

  // Whether the SELECTED deployment is a legitimate rollback target
  // (memql#4264). The retired cluster-ops timeline enforced this per row --
  // `entry.status === "succeeded" && entry.kind !== "repair" && !isCurrent` --
  // and the rule has to survive the move, because it is a safety property
  // rather than a cosmetic one:
  //
  //   a REPAIR record pins no version. It re-converges the cluster onto what is
  //   already committed, so "roll back to this repair" names nothing to roll to.
  //   a record that did not SUCCEED never put that version anywhere.
  //   the CURRENT deployment is where the cluster already is.
  //
  // Computed here rather than inside DeploymentOps because the view is what
  // holds the rows.
  const selectedRow = rows.find((row) => row.id === selectedRowId);
  const canRollBackToSelection = isRollbackTarget(selectedRow, status?.version ?? "");
  const legs = gateLegRows(status);
  const images = componentRows(status);
  const rollouts = rolloutRows(status);

  // Full-row read is a separate gesture from deploy/rollback selection.
  const [viewRowId, setViewRowId] = useState("");
  const onRowAction = useCallback((_action: string, id: string) => setViewRowId(id), []);
  const onLocalSelect = useCallback((id: string) => setViewRowId(id), []);
  const onCloseView = useCallback(() => setViewRowId(""), []);
  const historyDetail = useRowDetail(concept.id, viewRowId);
  const imageRow = images.find((row) => row.id === viewRowId);
  const rolloutRow = rollouts.find((row) => row.id === viewRowId);
  const historyHit = viewRowId !== "" && rows.some((row) => row.id === viewRowId);
  const localRow = (imageRow ?? rolloutRow) as Row | undefined;

  return (
    <>
      <Band>
        <div className="flex flex-wrap items-center gap-3">
          <ReleaseReading
            version={status?.version ?? ""}
            engineVersion={status?.engineVersion ?? ""}
            sync={status?.argocd.syncStatus ?? ""}
            health={status?.argocd.healthStatus ?? ""}
            outOfSync={status?.argocd.outOfSync === true}
            loading={loading}
            canView={permissions.canView}
          />
        </div>
        {/* Errors reuse the shared banner so a deploy failure reads exactly
            like every other failure in the portal. The success note is its
            mirror -- a tinted panel with full-contrast text, never coloured
            small text: --memql-ok on the page ground is about 3.4:1 in light
            mode, under the floor for 13px. */}
        {/* A PRECONDITION is not a fault: a local cluster has nothing to
            deploy, and a node with no checkout cannot read what is pinned.
            Both are true statements about how this installation runs, so they
            are stated plainly rather than rendered as a failed read
            (memql#4265). */}
        {overlayAbsence !== "" ? (
          <p className="mt-3 rounded border border-line bg-raised px-3 py-2 text-sm text-muted">
            {overlayAbsenceStatement(overlayAbsence)}
          </p>
        ) : error ? (
          <div className="mt-3">
            <ErrorNotice sentence="Could not read this deployment." detail={error} />
          </div>
        ) : null}
        {actionError ? (
          <div className="mt-3">
            <ErrorNotice
              sentence="That deployment action did not run."
              next={<RefusalAuditLink id={actionAuditEventId} />}
              detail={actionError}
            />
          </div>
        ) : null}
        {actionMessage ? (
          <p
            role="status"
            className="mt-3 rounded border border-ok bg-ok-subtle px-3 py-2 text-sm text-fg"
          >
            {actionMessage}
            {/* The audit id on SUCCESS too, not only on a refusal
                (memql#4264). The retired cluster-ops page showed both; this
                band showed it only when the action was blocked. An operator
                reconciling "what did I just do to this cluster" against the
                audit trail needs the id of the thing that HAPPENED at least as
                much as the id of the thing that did not. */}
            {actionAuditEventId === "" ? null : (
              <>
                {" "}
                (audit <DataText kind="id">{actionAuditEventId}</DataText>)
              </>
            )}
          </p>
        ) : null}
      </Band>

      {/* Ship is hidden, not disabled, when the overlay cannot be read: every
          one of these actions reads the same missing file, so offering them
          would be offering a refusal.

          This IS the operations surface now (memql#4264). There used to be a
          second one at /cluster-ops with the same four verbs, and the two
          disagreed about how dangerous they are: these buttons fired deploy
          and roll back on a single click, while that page confirmed every one
          of them. An operator's protection depended on which door they had
          walked through. The careful set won; DeploymentOps carries it. */}
      {/* ABSENT for a non-owner, never disabled -- instanceActions' doctrine.
          The engine enforces the same rule independently (the Go owner wall in
          integrations/release, before any network call), so this decides only
          what is OFFERED.

          Gated on accessResolved as well as on the role: rendering it while
          the connection's identity is still unknown would flash a
          release-cutting form at everyone for one paint. */}
      {releases.accessResolved && releases.isOwner ? (
        <Band
          title="Releases"
          meta="Cut a version of MemQL itself"
        >
          <ReleasesCard state={releases} />
        </Band>
      ) : null}

      {overlayAbsence === "" && (permissions.canShip || permissions.canRollBack || permissions.canRepair) ? (
        <Band
          title="Ship"
          meta={
            selectedRowId
              ? "Acting on the selected deployment"
              : "Select a deployment below to deploy or roll back to it"
          }
        >
          <DeploymentOps
            console_={console_}
            selectedRowId={selectedRowId}
            canRollBackToSelection={canRollBackToSelection}
          />
        </Band>
      ) : null}

      {permissions.canView ? (
        <Band
          title="Last gate"
          meta={
            status?.gateResult.ranAt
              ? `${status.gateResult.result || "unknown"}, ran ${status.gateResult.ranAt}`
              : "no gate result recorded"
          }
        >
          {legs.length === 0 ? (
            <p className="text-sm text-subtle">
              {loading ? "Reading the gate…" : "No gate has run yet."}
            </p>
          ) : (
            <ViewElement element={CHECKLIST_ELEMENT} rows={legs} concept={GATE_LEG_CONCEPT} />
          )}
        </Band>
      ) : (
        <Band title="Live state">
          <p className="text-sm text-subtle">
            Reading a deployment's live state needs the admin or owner role. Your
            role is {permissions.role || "not established on this connection"}. The
            deployment history below is what the cluster returns for you.
          </p>
        </Band>
      )}

      {permissions.canView && images.length > 0 ? (
        <Band title="Images in force" meta="the cloud overlay, resolved digests" panel>
          <ViewElement
            element={TABLE_ELEMENT}
            rows={images}
            concept={COMPONENT_CONCEPT}
            onSelect={onLocalSelect}
          />
        </Band>
      ) : null}

      {permissions.canView && rollouts.length > 0 ? (
        <Band title="Rollouts in flight" panel>
          <ViewElement
            element={TABLE_ELEMENT}
            rows={rollouts}
            concept={ROLLOUT_CONCEPT}
            onSelect={onLocalSelect}
          />
        </Band>
      ) : null}

      <Band title="By outcome" meta="every deployment recorded">
        <ViewElement
          element={PROPORTION_BAR_ELEMENT}
          rows={rows}
          concept={concept}
          // The concept declares status="status"; the rail finds it through
          // the display card.
          options={{ bindings: { value: [] } }}
        />
      </Band>

      <Band title="History" meta="Newest first, by last status transition" panel>
        <ViewElement
          element={TIMELINE_ELEMENT}
          rows={rows}
          concept={concept}
          options={{
            ...selection,
            rowAction: "view",
            bindings: {
              at: "updatedAt",
              label: "version",
              detail: "provider",
              status: "status",
            },
          }}
          onSelect={onSelect}
          onRowAction={onRowAction}
        />
      </Band>
      <RowDetailDialog
        open={viewRowId !== ""}
        onClose={onCloseView}
        rowId={viewRowId}
        row={historyHit ? historyDetail.row : (localRow ?? null)}
        loading={historyHit ? historyDetail.loading : false}
        error={historyHit ? historyDetail.error : ""}
        missing={historyHit ? historyDetail.missing : viewRowId !== "" && localRow === undefined}
      />
    </>
  );
}

// The reference for a REFUSED action (memql#3334).
//
// A denial is an audited event, so the operator who was told "no" gets the
// same handle the operator who was told "yes" gets: an id to quote and a way
// into the trail. Until #3334 the deploy console had nothing to render here
// and said nothing rather than inventing an id -- correct behaviour for a
// surface that was not being given one, and exactly the gap the issue names.
//
// Renders NOTHING when the id is empty, which is the common case for a failure
// that was not a refusal: an invalid argument is rejected before the gate, and
// an UNAVAILABLE never reached the identity node, so neither wrote an event.
// Showing "audited as (none)" there would be worse than silence.
//
// Deliberately the same shape as the admin console's AuditLink
// (src/admin/WriteOutcome.tsx) -- one convention for "here is the row that
// records what just happened", across both gated write surfaces.
function RefusalAuditLink({ id }: { id: string }): ReactNode {
  if (id === "") return null;
  return (
    <>
      {" "}
      <span className="text-xs text-muted">
        Audited as <span className="font-mono break-all">{id}</span> —{" "}
        <Link to="/views/audit" className="underline hover:text-fg">
          open the trail
        </Link>
      </span>
    </>
  );
}

// The headline reading. Deliberately NOT stat tiles: a release is one record,
// not a population, and the four facts on it are strings -- a version, an
// engine version, a sync state and a health state. Tiles would render "1
// deployment" and tell an operator nothing.
function ReleaseReading({
  version,
  engineVersion,
  sync,
  health,
  outOfSync,
  loading,
  canView,
}: {
  version: string;
  engineVersion: string;
  sync: string;
  health: string;
  outOfSync: boolean;
  loading: boolean;
  canView: boolean;
}): ReactNode {
  if (!canView) {
    return <p className="text-sm text-subtle">Live state is not visible to your role.</p>;
  }
  if (loading && version === "") {
    return <p className="text-sm text-muted">Reading the deployment…</p>;
  }
  if (version === "") {
    // Only when the overlay WAS read and promotes nothing. When it could not be
    // read at all the band is not rendered, because "nothing is pinned" would
    // be a claim about a file this node never opened.
    return <p className="text-sm text-subtle">Nothing is pinned in the overlay.</p>;
  }
  return (
    <p className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
      <span className="text-lg font-semibold tracking-tight"><DataText kind="id">{version}</DataText></span>
      {engineVersion && engineVersion !== version ? (
        <span className="text-muted">engine {engineVersion}</span>
      ) : null}
      {/* Drift is said in words, at full contrast, rather than in a hue:
          --memql-warn is a ~2.3:1 amber in light mode and would be the least
          legible thing on the page while carrying the most weight. */}
      {sync ? (
        <span className={outOfSync ? "font-medium text-fg" : "text-muted"}>
          {sync}
          {outOfSync ? " — the cluster does not match the overlay" : ""}
        </span>
      ) : null}
      {health ? <span className="text-muted">{health}</span> : null}
    </p>
  );
}

// isRollbackTarget answers "may the cluster be rolled back to this record".
//
// A repair is a deployment record too -- same concept, same history -- marked
// by the engine with a "repair:" note prefix. That prefix is the ONLY thing
// distinguishing the two; it is defined once in
// component/deploycontrol/repair.go and mirrored in
// src/clusterops/useDeploymentTimeline.ts, which is where this constant comes
// from.
const REPAIR_NOTE_PREFIX = "repair:";

function isRollbackTarget(row: Row | undefined, runningVersion: string): boolean {
  if (row === undefined) return false;
  const field = (key: string): string => {
    const v = (row as Record<string, unknown>)[key];
    return typeof v === "string" ? v : "";
  };
  if (field("status") !== "succeeded") return false;
  if (field("notes").trimStart().startsWith(REPAIR_NOTE_PREFIX)) return false;
  // Rolling back to the version already running is a no-op that reads as a
  // deploy. The old timeline expressed this as `!isCurrent` against its own
  // newest entry; comparing against what the cluster REPORTS it is running is
  // the same rule with a better source.
  const version = field("engineVersion") || field("version");
  if (version !== "" && runningVersion !== "" && version === runningVersion) return false;
  const id = typeof row.id === "string" ? row.id : "";
  return id !== "";
}
