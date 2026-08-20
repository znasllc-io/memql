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
  PageHeader,
  Skeleton,
  type StatusTone,
} from "../ui";
import { useDeployConsole } from "../deploy/useDeployConsole";
import { useDeploymentTimeline, type DeploymentEntry } from "./useDeploymentTimeline";

// Cluster operations (memql#4193): update and restore/rollback, riding the
// existing DeployControlService path (identity node; the bff forwards with
// the caller as a verified ForwardedAuthority, so the owner-only rollback
// gate runs against the human -- component/grpc/deploy_control_forward.go).
// NOT install: a cluster must exist before its portal does.
//
// Every action states exactly what will happen in a ConfirmDialog, and
// progress afterwards is the deployment RECORD's status -- the timeline
// below is graph state re-read live, not a client-side guess.
//
// REPAIR IS ABSENT ON PURPOSE. The vision names update / repair / restore,
// and the deploy-control surface exposes no repair RPC today (its verbs:
// getDeploymentStatus, suggestNextVersion, cutVersion, deploy, rollback,
// rollbackDeployment, rolloutAction). Repair of a LOCAL cluster lives in
// the VS Code extension's capability scripts; a cluster-side repair RPC is
// an engine gap filed upstream, and this page says so rather than shelling
// around it.
export function ClusterOpsPage(): ReactNode {
  const console_ = useDeployConsole();
  const timeline = useDeploymentTimeline(console_.permissions.canView);
  const [confirming, setConfirming] = useState<
    | { kind: "update" }
    | { kind: "ship"; deploymentId: string; version: string }
    | { kind: "rollback"; deploymentId: string; version: string }
    | null
  >(null);

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
          </p>
        ) : null}

        <Band title="Repair">
          <p className="text-sm text-muted">
            Not exposed from the cluster yet: the deploy-control surface carries no repair verb,
            and this console does not shell around its own wire contract. Repairing a LOCAL
            cluster is the VS Code extension's guided flow; a cluster-side repair action is filed
            as an engine gap.
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
        {canRollBack && !isCurrent && entry.status === "succeeded" ? (
          <Button size="xs" tone="danger" onClick={onRollBack} disabled={busy}>
            Roll back to this
          </Button>
        ) : null}
      </span>
    </li>
  );
}
