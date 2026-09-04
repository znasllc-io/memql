import { ArrowLeft, GitBranch, History, Sparkles } from "lucide-react";

import { Button, Caption, Chip, Fact, Facts, Head, Panel } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { usePackageActions } from "../packages/actions";
import { shortVersion, sourceLabel, type PackageRow } from "../packages/rows";
import { siteName, type SiteRow } from "../rows";
import type { CredentialRow } from "../sources/rows";
import { AutoDeploySwitch, PackageLifecycle, SwitchCredential } from "./stops/Source";

// SourceView -- a source is a THING, with its own page (epic memql#4937, D4).
//
// ===========================================================================
// WHY THIS EXISTS
// ===========================================================================
// Everything on this page used to render inside the Source stop of EVERY app
// the source produced. Two measured consequences:
//
//   * `storefront` and `web` drew an IDENTICAL 2,600px "Every attempt" --
//     `usePackageDeployments` reads the PACKAGE's timeline, so one history was
//     rendered twice, on two pages, for one source.
//
//   * "Archive this source and every app it produced" sat at y=885 on `web`'s
//     page. `packageArchive` cascades, so that control on web's page also
//     archives `storefront` -- and it rendered 1,614px ABOVE that page's own
//     archive, because the Source stop comes first.
//
// A control that destroys a SIBLING, easier to reach than the one that
// destroys the thing you are looking at, is not a layout problem. The fix is
// to put source-level acts where the source is.
//
// The app's Source stop keeps one line and a link here.

export function SourceView({
  pkg,
  apps,
  credentials,
  canWrite,
  onBack,
  onOpenHistory,
  onOpenApp,
  onAsk,
  attempts,
}: {
  pkg: PackageRow;
  /** The apps this source produced, from the root's site feed. */
  apps: readonly SiteRow[];
  credentials: readonly CredentialRow[];
  canWrite: boolean;
  onBack: () => void;
  onOpenHistory: () => void;
  onOpenApp: (siteId: string) => void;
  onAsk?: (tag: string) => void;
  /** How many runs this source has, for the history line. */
  attempts: number;
}) {
  const actions = usePackageActions();
  const label = sourceLabel(pkg);
  const live = apps.filter((a) => a.status === "live").length;

  const acts: Act[] = canWrite
    ? [
        {
          label: "Deploy",
          tone: "primary",
          busy: actions.busy,
          ariaLabel: `Deploy ${label}`,
          onAct: () => void actions.deploy(pkg.id, false),
        },
      ]
    : [];

  return (
    <div className="os-deploy-pane">
      <div className="os-deploy-scroll">
        <Panel label={`Source ${label}`}>
          <Head title={label}>
            <Button tone="quiet" onClick={onBack}>
              <ArrowLeft size={13} aria-hidden /> Deployables
            </Button>
            {onAsk ? (
              <Button tone="quiet" onClick={() => onAsk(`app:deployables package:${pkg.name || pkg.id}`)}>
                <Sparkles size={13} aria-hidden /> Ask
              </Button>
            ) : null}
          </Head>

          <Caption>
            What this source is, what it fetches under, and what happens when it moves. These are facts about the
            source rather than about any one app it produced, which is why they are here and not on each of them.
          </Caption>

          <Facts>
            <Fact label="Tracking" value={pkg.repoRef === "" ? "default branch" : pkg.repoRef} />
            <Fact label="Deployed" value={pkg.deployedVersion === "" ? "" : shortVersion(pkg.deployedVersion)} mono />
            <Fact
              label="Latest upstream"
              value={pkg.latestKnownVersion === "" ? "" : shortVersion(pkg.latestKnownVersion)}
              mono
            />
            <Fact label="Added" value={formatMoment(pkg.createdAt)} />
          </Facts>

          <section className="os-report-part">
            <h4 className="os-report-heading">
              <GitBranch size={12} aria-hidden /> Apps it produces
            </h4>
            {apps.length === 0 ? (
              <Caption>Nothing has been deployed from this source yet.</Caption>
            ) : (
              <ul className="os-source-apps">
                {apps.map((app) => (
                  <li key={app.id}>
                    <button type="button" className="os-source-app" onClick={() => onOpenApp(app.id)}>
                      <span className="os-source-app-name">{app.packageDeployableName || siteName(app)}</span>
                      <span className="os-mono os-source-app-host">{app.hostname}</span>
                      <Chip tone={app.status === "live" ? "accent" : "muted"}>{statusWord(app.status)}</Chip>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </section>

          {pkg.sourceKind === "repo" && canWrite ? <SwitchCredential pkg={pkg} credentials={credentials} /> : null}
          {canWrite && pkg.status !== "archived" ? <AutoDeploySwitch pkg={pkg} /> : null}

          <button type="button" className="os-deploy-history-line" onClick={onOpenHistory}>
            <History size={12} aria-hidden />
            <span>
              History &middot; {attempts} attempt{attempts === 1 ? "" : "s"}
            </span>
            <span aria-hidden>&#9656;</span>
          </button>

          {/* THE CASCADE LIVES HERE AND NOWHERE ELSE. It archives this source
              and every app it produced, so the only honest place for it is the
              page whose subject is the source. */}
          {canWrite ? <PackageLifecycle pkg={pkg} apps={apps} /> : null}
        </Panel>
      </div>

      <ActionBar
        state="Tracked"
        detail={`${apps.length} app${apps.length === 1 ? "" : "s"}${live > 0 ? `, ${live} serving` : ""}${
          pkg.autoDeploy ? " -- deploys itself when the plan is unchanged" : ""
        }`}
        tone={live > 0 ? "live" : "none"}
        acts={acts}
      />
    </div>
  );
}

function statusWord(status: string): string {
  switch (status) {
    case "live":
      return "published";
    case "disabled":
      return "unpublished";
    case "draft":
      return "draft";
    case "archived":
      return "archived";
    default:
      return status;
  }
}
