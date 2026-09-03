import { Undo2 } from "lucide-react";

import { Button, Caption, Chip, LiveList } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import type { LiveView } from "../../../live/liveView";
import { usePackageActions } from "../packages/actions";
import { BuildLog, ProblemNotice } from "../packages/ReportView";
import { deploymentFingerprint, shortVersion, type DeploymentRow, type PackageRow } from "../packages/rows";
import { Rail } from "./Rail";

// Every attempt: the append-only runs of this deployable's source, each with
// its own rail (design section A).
//
// ===========================================================================
// THE TIMELINE IS A RECORD, NOT A LOG
// ===========================================================================
// A deployment row is append-only past a terminal status, so this is the
// literal history of what was attempted -- it cannot be rewritten by the next
// attempt to look like it always went well. That is also what makes rollback
// possible: a prior row records the exact tuple to restore, and "Roll back to
// this" on a succeeded run that is not the latest is the whole-package
// rollback, restoring that row's tuple for every app it published.
//
// So each entry carries its own stage rail rather than a status word. "It
// failed" is a much smaller statement than "it failed at build, so every site
// is still serving what it was", and the second one is what tells somebody
// whether to worry. The parked run is listed as "waiting for you"; its report
// is what the What-it-is stop shows.

export function EveryAttempt({
  pkg,
  deployments,
  canWrite,
  reseed,
}: {
  pkg: PackageRow | null;
  /** The page's timeline view, newest first; null before the connection exists. */
  deployments: LiveView<DeploymentRow> | null;
  canWrite: boolean;
  reseed: () => void;
}) {
  // Its own write hook: a rollback refused here renders here.
  const actions = usePackageActions();
  const latest = deployments?.snapshot.rows[0] ?? null;

  return (
    <section className="os-report-part">
      <h4 className="os-report-heading">Every attempt</h4>
      {pkg === null ? (
        <Caption>
          A hand-made deployable has no attempts: what it served, and when, is the version list on the Live stop.
        </Caption>
      ) : (
        <>
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
          {actions.refusal ? <ProblemNotice problem={{ ...actions.refusal, fatal: true }} tone="error" /> : null}
        </>
      )}
    </section>
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
