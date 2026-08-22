import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { ErrorMessage } from "../components/StatusMessage";
import {
  Badge,
  Band,
  Button,
  ConfirmDialog,
  Container,
  DataText,
  Field,
  PageHeader,
  Skeleton,
  TextInput,
  type StatusTone,
} from "../ui";
import { useDeployConsole } from "../deploy/useDeployConsole";
import { useDeploymentTimeline, type DeploymentEntry } from "./useDeploymentTimeline";

// Cluster operations (memql#4193): update, repair and restore/rollback,
// riding the existing DeployControlService path (identity node; the bff
// forwards with the caller as a verified ForwardedAuthority, so the
// owner-only rollback and repair gates run against the human --
// component/grpc/deploy_control_forward.go). NOT install: a cluster must
// exist before its portal does.
//
// Every action states exactly what will happen in a ConfirmDialog, and
// progress afterwards is the deployment RECORD's status -- the timeline
// below is graph state re-read live, not a client-side guess.
//
// REPAIR (memql#4209) is the cluster-side half of the extension's repair:
// the identity node asks ArgoCD to hard-refresh and re-sync this
// installation's Application from the committed overlay (prune included)
// and watches it until it is synced and healthy. Nothing changes version.
// It is owner-only, type-to-confirm, and its progress is a REPAIR RECORD on
// the same timeline (a deployment row with a "repair:" note), resolved to
// succeeded or failed from what the node observes. The host-side half --
// tools, hosts file, local CA, checkout, the k3d cluster itself -- is not
// reachable from inside the cluster and stays with the VS Code extension.
// This page does not shell around its own wire contract.

// The phrase an owner types to arm the repair verb. Lower-case, the verb
// itself: the point is a deliberate keystroke, not a password.
const REPAIR_CONFIRM_PHRASE = "repair";

export function ClusterOpsPage(): ReactNode {
  const console_ = useDeployConsole();
  const timeline = useDeploymentTimeline(console_.permissions.canView);
  const [confirming, setConfirming] = useState<
    | { kind: "update" }
    | { kind: "ship"; deploymentId: string; version: string }
    | { kind: "rollback"; deploymentId: string; version: string }
    | { kind: "repair" }
    | null
  >(null);
  const [repairPhrase, setRepairPhrase] = useState("");

  if (!console_.permissions.canView) {
    return (
      <div className="rounded-lg border border-line bg-surface p-6">
        <h2 className="text-sm font-semibold">This is an operator surface</h2>
        <p className="mt-2 max-w-2xl text-sm text-muted">
          Deployment state and actions are refused server-side below the operator roles. Ask a
          cluster owner to change your role.
        </p>
      </div>
    );
  }

  const status = console_.status;
  const canAct = console_.permissions.canShip;
  const canRollBack = console_.permissions.canRollBack;
  const canRepair = console_.permissions.canRepair;
  const closeRepair = () => {
    setConfirming(null);
    setRepairPhrase("");
  };

  return (
    <Container variant="data">
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          title="Cluster operations"
          blurb={
            <>
              Update and roll back this installation. Deeper deployment detail lives on the{" "}
              <Link to="/views/deployments" className="text-accent hover:underline">
                Deployments view
              </Link>
              ; this page is the operations end of it.
            </>
          }
          actions={
            <>
              <Button size="xs" onClick={() => { console_.refresh(); timeline.reload(); }}>
                Refresh
              </Button>
              {canAct ? (
                <Button size="xs" tone="primary" onClick={() => setConfirming({ kind: "update" })} disabled={console_.busy}>
                  Cut a patch version
                </Button>
              ) : null}
            </>
          }
          meta={
            status ? (
              <span className="text-xs text-subtle">
                running <DataText kind="id">{status.version || "unknown"}</DataText>
                {status.engineVersion ? (
                  <>
                    {" "}
                    engine <DataText kind="id">{status.engineVersion}</DataText>
                  </>
                ) : null}
              </span>
            ) : undefined
          }
        />

        {console_.error !== "" ? <ErrorMessage>{console_.error}</ErrorMessage> : null}
        {console_.actionError !== "" ? (
          <ErrorMessage>
            {console_.actionError}
            {console_.actionAuditEventId !== "" ? (
              <>
                {" "}
                (audit <DataText kind="id">{console_.actionAuditEventId}</DataText>)
              </>
            ) : null}
          </ErrorMessage>
        ) : null}
        {console_.actionMessage !== "" ? (
          <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
            {console_.actionMessage}
            {console_.actionAuditEventId !== "" ? (
              <>
                {" "}
                (audit <DataText kind="id">{console_.actionAuditEventId}</DataText>)
              </>
            ) : null}
          </p>
        ) : null}

        <Band
          title="Repair"
          meta={
            canRepair ? (
              <Button
                size="xs"
                tone="danger"
                onClick={() => setConfirming({ kind: "repair" })}
                disabled={console_.busy}
              >
                Repair this installation
              </Button>
            ) : undefined
          }
        >
          <p className="text-sm text-muted">
            Re-converges this installation onto its committed overlay: the identity node asks
            ArgoCD to re-fetch the manifests and sync the application (pruning tracked resources
            Git no longer describes), then watches it until it is synced and healthy. Nothing
            changes version. Progress lands on the timeline below as a repair record.
            {canRepair
              ? " Owner-only and type-to-confirm."
              : " Owner-only: the control is offered to the cluster owner, and the cluster refuses it below that role."}
          </p>
        </Band>

        <Band
          title="Deployment timeline"
          meta={timeline.entries.length === 0 ? undefined : `${timeline.entries.length} deployments, newest first — live`}
          panel
        >
          {timeline.error !== "" ? (
            <ErrorMessage>Could not read the timeline: {timeline.error}</ErrorMessage>
          ) : timeline.loading && timeline.entries.length === 0 ? (
            <Skeleton variant="rows" rows={5} />
          ) : timeline.entries.length === 0 ? (
            <p className="p-3 text-sm text-muted">
              No deployment records yet. The first update lands here as an append-only timeline.
            </p>
          ) : (
            <ul className="divide-y divide-line">
              {timeline.entries.map((entry, index) => (
                <TimelineRow
                  key={entry.deploymentId}
                  entry={entry}
                  isCurrent={index === 0}
                  canShip={canAct}
                  canRollBack={canRollBack}
                  busy={console_.busy}
                  onShip={() =>
                    setConfirming({ kind: "ship", deploymentId: entry.deploymentId, version: entry.engineVersion })
                  }
                  onRollBack={() =>
                    setConfirming({ kind: "rollback", deploymentId: entry.deploymentId, version: entry.engineVersion })
                  }
                />
              ))}
            </ul>
          )}
        </Band>

        <ConfirmDialog
          open={confirming?.kind === "update"}
          title="Cut a patch version?"
          confirmLabel="Cut the version"
          busy={console_.busy}
          onConfirm={() => {
            setConfirming(null);
            console_.cut("patch");
          }}
          onCancel={() => setConfirming(null)}
        >
          Records a new deployment at the next patch version. Nothing rolls until that deployment
          is shipped from the timeline below; the record's status is the progress you will see.
        </ConfirmDialog>
        <ConfirmDialog
          open={confirming?.kind === "ship"}
          title="Deploy this version?"
          confirmLabel="Deploy"
          busy={console_.busy}
          onConfirm={() => {
            if (confirming?.kind === "ship") console_.ship(confirming.deploymentId);
            setConfirming(null);
          }}
          onCancel={() => setConfirming(null)}
        >
          Rolls every node type to{" "}
          <DataText kind="id">{confirming?.kind === "ship" ? confirming.version || confirming.deploymentId : ""}</DataText>{" "}
          through the GitOps path. Progress and failure land on the deployment record below as
          its status changes.
        </ConfirmDialog>
        <ConfirmDialog
          open={confirming?.kind === "rollback"}
          title="Roll back to this deployment?"
          confirmLabel="Roll back"
          tone="danger"
          busy={console_.busy}
          onConfirm={() => {
            if (confirming?.kind === "rollback") console_.rollBack(confirming.deploymentId);
            setConfirming(null);
          }}
          onCancel={() => setConfirming(null)}
        >
          Owner-only, enforced against you personally on the identity node. Re-pins the cluster
          to{" "}
          <DataText kind="id">
            {confirming?.kind === "rollback" ? confirming.version || confirming.deploymentId : ""}
          </DataText>{" "}
          and supersedes the current deployment; the timeline records both sides of the move.
        </ConfirmDialog>
        <ConfirmDialog
          open={confirming?.kind === "repair"}
          title="Repair this installation?"
          confirmLabel="Repair"
          tone="danger"
          busy={console_.busy}
          confirmDisabled={repairPhrase.trim() !== REPAIR_CONFIRM_PHRASE}
          onConfirm={() => {
            if (confirming?.kind === "repair") console_.repair();
            closeRepair();
          }}
          onCancel={closeRepair}
        >
          <p>
            Owner-only, enforced against you personally on the identity node. ArgoCD re-fetches
            the committed manifests and syncs this installation's application, pruning tracked
            resources Git no longer describes; drifted or deleted workloads are re-applied.
            Nothing changes version. A repair record lands on the timeline at in_progress and is
            resolved to succeeded or failed from what the node observes -- not from this button.
          </p>
          <div className="mt-3">
            <Field label={`Type "${REPAIR_CONFIRM_PHRASE}" to confirm`}>
              <TextInput value={repairPhrase} onChange={setRepairPhrase} placeholder={REPAIR_CONFIRM_PHRASE} />
            </Field>
          </div>
        </ConfirmDialog>
      </section>
    </Container>
  );
}

function statusTone(status: string): StatusTone {
  switch (status) {
    case "succeeded":
      return "ok";
    case "failed":
    case "rolled_back":
      return "danger";
    case "pending":
    case "in_progress":
      return "warn";
    default:
      return "neutral";
  }
}

function TimelineRow({
  entry,
  isCurrent,
  canShip,
  canRollBack,
  busy,
  onShip,
  onRollBack,
}: {
  entry: DeploymentEntry;
  isCurrent: boolean;
  canShip: boolean;
  canRollBack: boolean;
  busy: boolean;
  onShip: () => void;
  onRollBack: () => void;
}): ReactNode {
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2">
      <DataText kind="id" title={entry.deploymentId}>
        {entry.engineVersion || entry.deploymentId}
      </DataText>
      {entry.kind === "repair" ? (
        <span title={entry.notes}>
          <Badge tone="neutral">repair</Badge>
        </span>
      ) : null}
      <Badge tone={statusTone(entry.status)}>{entry.status || "unknown"}</Badge>
      {isCurrent ? <span className="text-xs text-subtle">newest</span> : null}
      <DataText kind="time">{entry.createdAt}</DataText>
      <span className="min-w-0 flex-1 truncate text-xs text-muted">
        {entry.nodeSpecs.length === 0
          ? ""
          : entry.nodeSpecs
              .map((spec) => `${spec.nodeType}×${spec.replicas}${spec.version ? `@${spec.version}` : ""}`)
              .join("  ")}
      </span>
      <span className="flex gap-2">
        {canShip && entry.status === "pending" ? (
          <Button size="xs" onClick={onShip} disabled={busy}>
            Deploy
          </Button>
        ) : null}
        {canRollBack && !isCurrent && entry.status === "succeeded" && entry.kind !== "repair" ? (
          <Button size="xs" tone="danger" onClick={onRollBack} disabled={busy}>
            Roll back to this
          </Button>
        ) : null}
      </span>
    </li>
  );
}
