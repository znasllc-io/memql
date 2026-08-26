import { useCallback, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";
import { CHECKLIST_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { useRowDetail } from "../cluster/useConceptRows";
import { RowDetailDialog } from "../components/RowDetailDialog";
import { ErrorMessage } from "../components/StatusMessage";
import { DeploymentOps } from "../deploy/DeploymentOps";
import { overlayAbsenceOf, overlayAbsenceStatement } from "../deploy/noOverlay";
import { ReleasesCard } from "../deploy/releases/ReleasesCard";
import { useReleases } from "../deploy/releases/useReleases";
import {
  COMPONENT_CONCEPT,
  GATE_LEG_CONCEPT,
  ROLLOUT_CONCEPT,
  componentRows,
  gateLegRows,
  rolloutRows,
} from "../deploy/rows";
import { useDeployConsole } from "../deploy/useDeployConsole";
import { Band, DataText } from "../ui";
import { ElementView } from "../viewkit/ElementView";

// The whole live-deploy panel, as a widget (epic memql#4661, task memql#4674).
//
// ===========================================================================
// WHY THIS ONE IS BIG, AND WHY THAT IS THE RIGHT ANSWER
// ===========================================================================
// The Deployments page was the hardest of the five to converge. Beside its
// population it carried a control panel, a release list, a precondition
// notice, a row dialog, and three row sets that ARE NOT CONCEPT ROWS -- gate
// legs, resolved images, in-flight rollouts, all projected from a live status
// read rather than walked from the graph.
//
// An arrangement cannot express those: a section is a concept and a walk, and
// these have neither. The alternative to putting them here would have been to
// keep a bespoke page for Deployments, which is exactly the outcome the epic's
// D6 rules out. So the page above this is an ordinary arrangement over
// v1:cluster:deployment -- a reading, a shape, a history -- and everything
// that is about the LIVE CLUSTER rather than the population lives in one
// widget, unchanged in behaviour.
//
// The projected row sets still render through view-kit elements over synthetic
// ConceptLikes, which is what they always did (deploy/rows.ts): the rule the
// composition guard enforces is that nothing hand-renders a ROW, and this
// does not.
export function DeployControlsWidget({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: {
  concept: Concept;
  rows: readonly Row[];
  selectedRowId: string;
  onSelect: (rowId: string) => void;
}): ReactNode {
  const console_ = useDeployConsole();
  const releases = useReleases();
  const { permissions, status, loading, error, actionMessage, actionError, actionAuditEventId } =
    console_;
  const overlayAbsence = overlayAbsenceOf(error);

  const legs = gateLegRows(status);
  const images = componentRows(status);
  const rollouts = rolloutRows(status);

  // Deployments spends row-click on deploy/rollback selection, so reading a
  // row goes through a local dialog opened from the projected tables.
  const [viewRowId, setViewRowId] = useState("");
  const onLocalSelect = useCallback((id: string) => setViewRowId(id), []);
  const onCloseView = useCallback(() => setViewRowId(""), []);
  const detail = useRowDetail(concept.id, viewRowId);
  const localRow = (images.find((r) => r.id === viewRowId) ?? rollouts.find((r) => r.id === viewRowId)) as
    | Row
    | undefined;
  const historyHit = viewRowId !== "" && rows.some((row) => row.id === viewRowId);
  void onSelect;

  const selectedRow = rows.find((row) => row.id === selectedRowId);
  const canRollBackToSelection = isRollbackTarget(selectedRow, status?.version ?? "");

  return (
    <div className="flex flex-col gap-6">
      <ReleaseReading
        version={status?.version ?? ""}
        engineVersion={status?.engineVersion ?? ""}
        sync={status?.argocd.syncStatus ?? ""}
        health={status?.argocd.healthStatus ?? ""}
        outOfSync={status?.argocd.outOfSync === true}
        loading={loading}
        canView={permissions.canView}
      />

      {/* A PRECONDITION IS NOT A FAULT: a local cluster has nothing to deploy,
          and a node with no checkout cannot read what is pinned. Both are true
          statements about how this installation runs, so they are stated
          plainly rather than rendered as a failed read (memql#4265). */}
      {overlayAbsence !== "" ? (
        <p className="rounded border border-line bg-raised px-3 py-2 text-sm text-muted">
          {overlayAbsenceStatement(overlayAbsence)}
        </p>
      ) : error ? (
        <ErrorMessage>Could not read the deployment: {error}</ErrorMessage>
      ) : null}
      {actionError ? (
        <ErrorMessage>
          {actionError}
          <RefusalAuditLink id={actionAuditEventId} />
        </ErrorMessage>
      ) : null}
      {actionMessage ? (
        // role="status" is load-bearing rather than decorative: the outcome of
        // a deploy arrives asynchronously and a person may not be looking at
        // this corner of the page when it does.
        <p
          role="status"
          className="rounded border border-ok bg-ok-subtle px-3 py-2 text-sm text-fg"
        >
          {actionMessage}
          {/* The audit id on SUCCESS too, not only on a refusal (memql#4264).
              An operator reconciling "what did I just do to this cluster"
              against the audit trail needs the id of the thing that HAPPENED
              at least as much as the id of the thing that did not. */}
          {actionAuditEventId === "" ? null : (
            <>
              {" "}
              (audit <DataText kind="id">{actionAuditEventId}</DataText>)
            </>
          )}
        </p>
      ) : null}

      {overlayAbsence === "" &&
      (permissions.canShip || permissions.canRollBack || permissions.canRepair) ? (
        <Band
          title="Ship"
          headingLevel="h3"
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
          headingLevel="h3"
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
            <ElementView element={CHECKLIST_ELEMENT} rows={legs} concept={GATE_LEG_CONCEPT} />
          )}
        </Band>
      ) : (
        <Band title="Live state" headingLevel="h3">
          <p className="text-sm text-subtle">
            Reading a deployment&apos;s live state needs the admin or owner role. Your
            role is {permissions.role || "not established on this connection"}. The
            deployment history below is what the cluster returns for you.
          </p>
        </Band>
      )}

      {permissions.canView && images.length > 0 ? (
        <Band title="Images in force" headingLevel="h3" meta="the cloud overlay, resolved digests" panel>
          <ElementView
            element={TABLE_ELEMENT}
            rows={images}
            concept={COMPONENT_CONCEPT}
            onSelect={onLocalSelect}
          />
        </Band>
      ) : null}

      {permissions.canView && rollouts.length > 0 ? (
        <Band title="Rollouts in flight" headingLevel="h3" panel>
          <ElementView
            element={TABLE_ELEMENT}
            rows={rollouts}
            concept={ROLLOUT_CONCEPT}
            onSelect={onLocalSelect}
          />
        </Band>
      ) : null}

      {/* OWNER ONLY, and gated on the access read having LANDED. Rendering it
          while the connection's identity is still unknown would flash a
          release-cutting form at everyone for one paint -- which is a
          different bug from showing it to the wrong person and looks the
          same in a screenshot. */}
      {releases.accessResolved && releases.isOwner ? (
        <Band title="Releases" headingLevel="h3" meta="Cut a version of MemQL itself">
          <ReleasesCard state={releases} />
        </Band>
      ) : null}

      <RowDetailDialog
        open={viewRowId !== ""}
        onClose={onCloseView}
        rowId={viewRowId}
        row={historyHit ? detail.row : (localRow ?? null)}
        loading={historyHit ? detail.loading : false}
        error={historyHit ? detail.error : ""}
        missing={historyHit ? detail.missing : viewRowId !== "" && localRow === undefined}
      />
    </div>
  );
}

// The audit id beside a REFUSAL, with the way to go and read it. A refusal
// that names its own audit row is a refusal an operator can take somewhere.
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
    // read at all the notice above is rendered instead, because "nothing is
    // pinned" would be a claim about a file this node never opened.
    return <p className="text-sm text-subtle">Nothing is pinned in the overlay.</p>;
  }
  return (
    <p className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
      <span className="text-lg font-semibold tracking-tight">
        <DataText kind="id">{version}</DataText>
      </span>
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
