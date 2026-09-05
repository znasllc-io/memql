import { ArrowLeft, GitBranch, History, Sparkles } from "lucide-react";

import { Button, Caption, Chip, Fact, Facts, Head, Panel } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { ActionBar, type Act } from "../../../kit/ActionBar";
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
  const label = sourceLabel(pkg);
  const live = apps.filter((a) => a.status === "live").length;
  // Declared by the manifest and never deployed -- the difference between what
  // the source SAYS it contains and what it has actually put on the internet.
  const deployedNames = new Set(apps.map((a) => a.packageDeployableName));
  const undeployed = pkg.declares.filter((d) => !deployedNames.has(d.name));
  const total = apps.length + undeployed.length;

  // ===========================================================================
  // A SOURCE HAS NO DEPLOY, AND THAT IS THE POINT
  // ===========================================================================
  // This bar carried one: `deploy(pkg.id, false)`, an analyze run for the WHOLE
  // package with NO placements. Two things followed. The run parked and sat on
  // the list saying a deploy was waiting for somebody, which they had not
  // asked for; and confirming it deployed EVERY app in the manifest, because a
  // placement set is where "leave this one out" is expressed and there was
  // none -- so an app deliberately skipped came back.
  //
  // A source is the thing apps came FROM. Its page is where its credential,
  // its auto-deploy switch, its history and its archive live, and every one of
  // those is a fact about the source itself. Deploying is an act on ONE
  // deployable, done on that deployable's page, reached by opening the group
  // in the list -- which is also the only place a person can say which apps
  // they mean.
  //
  // The bar stays, with no acts: it still reads what this source IS and how
  // many of its apps are serving, which is what somebody who opened it came to
  // find out.
  const acts: Act[] = [];

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

          {/* WHAT IT DECLARES, not only what it deployed.
              A site row is written only for an app that actually deployed, so
              an app skipped at the confirm gate had no row and was missing
              from this list -- on the very page whose subject is the source
              that declares it. The two are shown together and told apart by
              their state: a deployed app says published or paused, a declared
              one says "not deployed yet". */}
          <section className="os-report-part">
            <h4 className="os-report-heading">
              <GitBranch size={12} aria-hidden /> Apps it produces
            </h4>
            {apps.length === 0 && undeployed.length === 0 ? (
              <Caption>
                Nothing yet. This source has not been analyzed, so there is no reading of what it contains.
              </Caption>
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
                {undeployed.map((d) => (
                  <li key={`declared:${d.name}`}>
                    {/* NOT A BUTTON. Everything else in this list opens a
                        deployable's page, and this app has none to open -- it
                        has never been deployed. Making it look pressable would
                        promise a page that does not exist; deploying it is
                        done from its row in the list, where the act belongs. */}
                    <div className="os-source-app" data-undeployed="true">
                      <span className="os-source-app-name">{d.name}</span>
                      <span className="os-source-app-host">no address yet</span>
                      <Chip tone="muted">not deployed</Chip>
                    </div>
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
        // ARCHIVED IS NOT TRACKED. The word was hard-coded, so an archived
        // source said "Tracked" with its own Restore control directly above.
        state={pkg.status === "archived" ? "Archived" : "Tracked"}
        // COUNTED THE WAY THE LIST COUNTS, which is everything this source
        // declares -- deployed or not. The two surfaces sat side by side
        // saying "2 apps" and "1 app" about the same source, because this one
        // counted site rows and the list counts the catalogue.
        detail={`${total} app${total === 1 ? "" : "s"}${live > 0 ? `, ${live} serving` : ""}${
          undeployed.length > 0 ? `, ${undeployed.length} not deployed` : ""
        }${
          pkg.autoDeploy ? " -- deploys itself when the plan is unchanged" : ""
        }`}
        tone={pkg.status === "archived" ? "none" : live > 0 ? "live" : "none"}
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
