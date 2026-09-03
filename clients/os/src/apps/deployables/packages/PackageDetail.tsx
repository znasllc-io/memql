import { useMemo, useState } from "react";
import { Archive, ArrowUpCircle, ExternalLink, RotateCcw, Rocket, Sparkles, Undo2 } from "lucide-react";

import {
  Button,
  Caption,
  Chip,
  Chips,
  Fact,
  Facts,
  Input,
  LiveList,
  Notice,
  Panel,
  ProvenanceDot,
  Subhead,
  useLiveView,
} from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { usePackageActions } from "./actions";
import { ConfirmGate } from "./ConfirmGate";
import { BuildLog, ProblemNotice, ReportView } from "./ReportView";
import { Rail } from "../page/Rail";
import { usePackageDeployments } from "./usePackages";
import {
  deploymentFingerprint,
  deploymentFromRow,
  shortVersion,
  sourceLabel,
  type DeploymentRow,
  type PackageRow,
} from "./rows";

// One package, in full: where it came from, what deploying it would do, and
// everything that has been attempted.
//
// ===========================================================================
// THE TIMELINE IS A RECORD, NOT A LOG
// ===========================================================================
// A deployment row is append-only past a terminal status, so this timeline is
// the literal history of what was attempted -- it cannot be rewritten by the
// next attempt to look like it always went well. That is also what makes
// rollback possible: a prior row records the exact tuple to restore.
//
// So each entry carries its own stage rail rather than a status word. "It
// failed" is a much smaller statement than "it failed at build, so every site
// is still serving what it was", and the second one is what tells somebody
// whether to worry.

const TERMINAL = new Set(["succeeded", "refused", "failed"]);

export function PackageDetail({
  pkg,
  viewerUserId,
  domain,
  canWrite,
  onAsk,
}: {
  pkg: PackageRow;
  viewerUserId: string;
  domain: string;
  canWrite: boolean;
  onAsk?: (tag: string) => void;
}) {
  const { source: collection, reseed } = usePackageDeployments(pkg.id);
  const deployments = useLiveView(collection, `deployments:${pkg.id}`, (rows) =>
    rows.map(deploymentFromRow).filter((d) => d.id !== ""),
  );
  const rows = deployments?.snapshot.rows ?? [];
  const actions = usePackageActions();

  const [dismissedGate, setDismissedGate] = useState("");
  const [confirmName, setConfirmName] = useState("");
  const [archiving, setArchiving] = useState(false);

  // A PARKED RUN IS NOT FINISHED, so it is picked up from the row rather than
  // held in component state. Somebody who closed the window and came back
  // finds their deploy exactly where they left it -- which is the whole reason
  // the confirm stage is a status on an append-only row and not a modal.
  const parked = useMemo(() => rows.find((d) => d.status === "awaiting_confirm") ?? null, [rows]);
  const running = useMemo(() => rows.find((d) => !TERMINAL.has(d.status) && d.status !== "awaiting_confirm") ?? null, [rows]);
  const latest = rows[0] ?? null;
  const deployedNames = useMemo(
    () => rows.flatMap((d) => d.deployables.filter((o) => o.siteId !== "").map((o) => o.name)),
    [rows],
  );

  const gate = parked !== null && parked.id !== dismissedGate ? parked : null;

  return (
    <Panel label={`Package ${pkg.name}`}>
      <div className="os-head">
        <Subhead>{pkg.name === "" ? "unnamed package" : pkg.name}</Subhead>
        <div className="os-head-actions">
          {pkg.repoUrl === "" ? null : (
            <a className="os-button" data-tone="quiet" href={pkg.repoUrl} target="_blank" rel="noreferrer noopener">
              <ExternalLink size={13} aria-hidden /> Source
            </a>
          )}
          {onAsk ? (
            <Button onClick={() => onAsk(`app:deployables package:${pkg.name || pkg.id}`)} ariaLabel={`Ask about ${pkg.name}`}>
              <Sparkles size={13} aria-hidden /> Ask
            </Button>
          ) : null}
          {canWrite && pkg.status !== "archived" ? (
            <Button
              tone="primary"
              onClick={() => {
                setDismissedGate("");
                void actions.deploy(pkg.id, false).then(reseed);
              }}
              busy={actions.busy && gate === null}
              busyLabel="Checking"
              ariaLabel={`Check what deploying ${pkg.name} would do`}
            >
              <Rocket size={13} aria-hidden /> {rows.length === 0 ? "Deploy" : "Redeploy"}
            </Button>
          ) : null}
        </div>
      </div>

      <Chips label="Package state">
        <span className="os-deploy-status" data-tone={pkg.status === "archived" ? "muted" : "ok"}>
          {/* The kit's dot has three tones and one of them is SILENCE. An
              archived package is not "unreachable" -- nothing is wrong with it
              -- so it gets no dot, which is what `unknown` renders. */}
          <ProvenanceDot tone={pkg.status === "archived" ? "unknown" : "reachable"} />
          {pkg.status === "archived" ? "archived" : "active"}
        </span>
        <Chip>{sourceLabel(pkg)}</Chip>
        {pkg.ownerUserId === viewerUserId && pkg.ownerUserId !== "" ? <Chip tone="accent">yours</Chip> : null}
        {pkg.credentialId === "" ? null : (
          <Chip title="The id of one of your source credentials. The token itself is never on this row, and this cluster reads it only at the moment of a fetch.">
            private, via {pkg.credentialId}
          </Chip>
        )}
      </Chips>

      {pkg.updateAvailable ? (
        <Notice
          tone="info"
          sentence={`There is a newer version upstream: ${shortVersion(pkg.latestKnownVersion)}.`}
          next="Deploying it starts a fresh run, and shows you what it would do first."
        >
          {canWrite ? (
            <Button
              tone="primary"
              onClick={() => {
                setDismissedGate("");
                void actions.deploy(pkg.id, false).then(reseed);
              }}
              busy={actions.busy}
            >
              <ArrowUpCircle size={13} aria-hidden /> Deploy the update
            </Button>
          ) : null}
        </Notice>
      ) : null}

      <Facts>
        <Fact label="Deployed" value={pkg.deployedVersion === "" ? "" : shortVersion(pkg.deployedVersion)} mono />
        <Fact label="Latest upstream" value={pkg.latestKnownVersion === "" ? "" : shortVersion(pkg.latestKnownVersion)} mono />
        <Fact
          label="Tracking"
          value={pkg.repoRef === "" ? (pkg.sourceKind === "repo" ? "default branch" : "") : pkg.repoRef}
        />
        <Fact label="Added" value={formatMoment(pkg.createdAt)} />
      </Facts>

      {actions.refusal ? <ProblemNotice problem={{ ...actions.refusal, fatal: true }} tone="error" /> : null}

      {gate ? (
        <ConfirmGate
          pkg={pkg}
          deployment={gate}
          deployedNames={deployedNames}
          domain={domain}
          busy={actions.busy}
          onConfirm={(hostnames) => void actions.deploy(pkg.id, true, hostnames).then(reseed)}
          onCancel={() => setDismissedGate(gate.id)}
        />
      ) : null}

      {running ? (
        <section className="os-report-part">
          <h4 className="os-report-heading">Deploying now</h4>
          <Rail input={{ mode: "deploy", deployment: running }} />
        </section>
      ) : null}

      {latest?.report && gate === null && running === null ? (
        <section className="os-report-part">
          <h4 className="os-report-heading">What this package holds</h4>
          <ReportView report={latest.report} />
        </section>
      ) : null}

      <section className="os-report-part">
        <h4 className="os-report-heading">Every attempt</h4>
        <LiveList<DeploymentRow>
          source={deployments}
          rowId={(d) => d.id}
          fingerprint={deploymentFingerprint}
          label={`Deployments of ${pkg.name}`}
          emptyText="Nothing has been deployed yet. The first deploy shows you what it would do before it does it."
          renderRow={(d) => (
            <article className="os-attempt" data-status={d.status}>
              <header className="os-attempt-head">
                <span className="os-attempt-version os-mono">
                  {d.sourceVersion === "" ? "no version" : shortVersion(d.sourceVersion)}
                </span>
                <span className="os-caption">{formatMoment(d.startedAt || d.createdAt)}</span>
                <span className="os-attempt-status" data-status={d.status}>
                  {statusWord(d.status)}
                </span>
                {canWrite && d.status === "succeeded" && d.id !== latest?.id ? (
                  <Button
                    onClick={() => void actions.rollback(pkg.id, d.id).then(reseed)}
                    busy={actions.busy}
                    ariaLabel={`Roll back to ${shortVersion(d.sourceVersion)}`}
                  >
                    <Undo2 size={12} aria-hidden /> Roll back to this
                  </Button>
                ) : null}
              </header>
              <Rail input={{ mode: "deploy", deployment: d }} />
              {d.error ? <ProblemNotice problem={d.error} tone="error" /> : null}
              <BuildLog tail={d.buildLogTail} />
              {d.deployables.length > 0 ? (
                <ul className="os-attempt-sites">
                  {d.deployables.map((o) => (
                    <li key={o.name}>
                      <span className="os-report-name">{o.name}</span>
                      {o.hostname === "" ? null : <span className="os-mono">{o.hostname}</span>}
                      {o.created ? <Chip tone="accent">created</Chip> : null}
                      {o.refusal ? <Chip tone="muted">{o.refusal.code}</Chip> : null}
                    </li>
                  ))}
                </ul>
              ) : null}
            </article>
          )}
        />
      </section>

      {canWrite ? (
        <section className="os-report-part os-danger-part">
          <h4 className="os-report-heading">
            <Archive size={12} aria-hidden /> {pkg.status === "archived" ? "Restore" : "Archive"}
          </h4>
          {pkg.status === "archived" ? (
            <>
              <Caption>Puts this package back on the active list. Its sites keep whatever state they have.</Caption>
              <Button onClick={() => void actions.restore(pkg.id)} busy={actions.busy}>
                <RotateCcw size={12} aria-hidden /> Restore this package
              </Button>
            </>
          ) : archiving ? (
            <>
              <Caption>
                Archiving keeps the package and everything it recorded. Every one of its sites has to be archived first.
                Type <strong>{pkg.name}</strong> to confirm.
              </Caption>
              <div className="os-confirm-row">
                <Input
                  id={`os-archive-${pkg.id}`}
                  label={`Type ${pkg.name} to confirm`}
                  value={confirmName}
                  onChange={setConfirmName}
                  placeholder={pkg.name}
                />
                <Button tone="quiet" onClick={() => setArchiving(false)}>
                  Cancel
                </Button>
                <Button
                  tone="danger"
                  disabled={confirmName !== pkg.name}
                  busy={actions.busy}
                  onClick={() => void actions.archive(pkg.id, confirmName)}
                >
                  Archive
                </Button>
              </div>
            </>
          ) : (
            <>
              <Caption>An archived package stays listed and can be restored. Nothing is deleted.</Caption>
              <Button onClick={() => setArchiving(true)}>
                <Archive size={12} aria-hidden /> Archive this package
              </Button>
            </>
          )}
        </section>
      ) : null}
    </Panel>
  );
}

/** What a deployment's status means, in the reader's terms rather than the
 *  state machine's. `succeeded` is the one worth translating: what a person
 *  cares about is that it is live. */
function statusWord(status: string): string {
  switch (status) {
    case "succeeded":
      return "live";
    case "refused":
      return "refused";
    case "failed":
      return "failed";
    case "awaiting_confirm":
      return "waiting for you";
    default:
      return status.replace(/_/g, " ");
  }
}
