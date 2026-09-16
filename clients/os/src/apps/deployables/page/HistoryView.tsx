
import { Button, Caption, Head, Panel, useLiveView } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { deploymentFromRow, runCoversApp, sourceLabel, type DeploymentRow, type PackageRow } from "../packages/rows";
import { siteName, type SiteRow } from "../rows";
import type { PartsHeld } from "../parts";
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
  can,
  onBack,
  backLabel,
  app,
  onOpenSourceHistory,
}: {
  pkg: PackageRow;
  /** The parts this session holds (epic memql#5289). */
  can: PartsHeld;
  onBack: () => void;
  backLabel?: string;
  app?: SiteRow;
  onOpenSourceHistory?: () => void;
}) {
  const { source: timeline, reseed } = usePackageDeployments(pkg.id);
  const deployments = useLiveView(timeline, `history:${pkg.id}:${app?.id ?? "source"}`, (rows) =>
    newestFirst(rows.map(deploymentFromRow).filter((d) => d.id !== "" && (!app || runCoversApp(d, app.packageDeployableName)))),
  );
  const count = deployments?.snapshot.state === "live" ? deployments.snapshot.rows.length : undefined;

  return (
    <div className="os-deploy-pane deployable-source-view" data-os-page-context={JSON.stringify({ page: "History", packageId: pkg.id, siteId: app?.id, source: sourceLabel(pkg) })}>
      <div className="os-deploy-scroll">
        <Panel label={`History of ${sourceLabel(pkg)}`}>
          <Head title="History" meta={count} breadcrumbs={[{ label: backLabel || sourceLabel(pkg), onSelect: onBack }, { label: "History" }]} back={{ label: backLabel || (app ? app.title || app.packageDeployableName || siteName(app) : sourceLabel(pkg)), onSelect: onBack }} />
          <Caption>
            {app ? "Attempts that include this app, newest first." : "Every attempt for this source, newest first."}
          </Caption>
          {app && onOpenSourceHistory ? <Button tone="quiet" onClick={onOpenSourceHistory}>All source attempts</Button> : null}
          <EveryAttempt pkg={pkg} deployments={deployments} can={can} reseed={reseed} scopedApp={app?.packageDeployableName} />
        </Panel>
      </div>

      {/* THE BAR STATES WHAT THIS PAGE IS. A record offers no lifecycle act,
          and saying so beats an empty band of chrome. */}
      <ActionBar
        state=""
        detail="Deployment history"
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
