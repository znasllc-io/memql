import { ArrowLeft } from "lucide-react";

import { Caption, Head, Panel, useLiveView } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { deploymentFromRow, sourceLabel, type DeploymentRow, type PackageRow } from "../packages/rows";
import { usePackageDeployments } from "../packages/usePackages";
import { EveryAttempt } from "./EveryAttempt";

// HistoryView -- the source's own record, on its own page (epic memql#4937).
//
// ===========================================================================
// WHY IT MOVED
// ===========================================================================
// Six attempts, each a full six-stop rail with its own refusal block, is
// 2,600px -- over half the old deployable page, below everything else. And it
// is the SOURCE's timeline (`usePackageDeployments` reads by packageId), so a
// two-app source rendered the identical wall on both of its apps' pages.
//
// ===========================================================================
// AND WHAT MOVING IT FIXED
// ===========================================================================
// The word "Retry" appeared SIX times on one page, carrying TWO promises: the
// Head's retried the SOURCE, an attempt's retried the bytes that lost run had
// already fetched. clients/os/README.md names that as the thing being avoided.
//
// They are now never on screen together: the forward act lives on the
// deployable's bar, and `Retry from these bytes` lives here, on the attempt
// whose run it names. One page, one meaning of the word.

export function HistoryView({
  pkg,
  canWrite,
  onBack,
}: {
  pkg: PackageRow;
  canWrite: boolean;
  onBack: () => void;
}) {
  const { source: timeline, reseed } = usePackageDeployments(pkg.id);
  const deployments = useLiveView(timeline, `history:${pkg.id}`, (rows) =>
    newestFirst(rows.map(deploymentFromRow).filter((d) => d.id !== "")),
  );
  const count = deployments?.snapshot.rows.length ?? 0;

  return (
    <div className="os-deploy-pane">
      <div className="os-deploy-scroll">
        <Panel label={`History of ${sourceLabel(pkg)}`}>
          <Head title="History" meta={count}>
            <button type="button" className="os-button" data-tone="quiet" onClick={onBack}>
              <ArrowLeft size={13} aria-hidden /> {sourceLabel(pkg)}
            </button>
          </Head>
          <Caption>
            Every attempt against this source, newest first. A deployment row is append-only past a terminal status, so
            this is the literal record of what was tried -- it cannot be rewritten by the next attempt to look like it
            always went well.
          </Caption>
          <EveryAttempt pkg={pkg} deployments={deployments} canWrite={canWrite} reseed={reseed} />
        </Panel>
      </div>

      {/* THE BAR STATES WHAT THIS PAGE IS. A record offers no lifecycle act,
          and saying so beats an empty band of chrome. */}
      <ActionBar
        state=""
        detail="A record. The only Retry here deploys the bytes a lost or cancelled run had already fetched -- a different promise from Deploy."
        tone="none"
      />
    </div>
  );
}

function newestFirst(rows: DeploymentRow[]): DeploymentRow[] {
  const at = (d: DeploymentRow): string => d.startedAt || d.createdAt;
  return [...rows].sort((a, b) => {
    const byTime = at(b).localeCompare(at(a));
    return byTime !== 0 ? byTime : b.id.localeCompare(a.id);
  });
}
