import { useSession } from "../../../chrome/access";
import { bare } from "../people";
import { sourceName } from "../list";
import { useSourceConnections } from "../sources/connections";
import { sourceRecord } from "../sources/sourceRecord";
import { SourceAccess } from "../sources/SourceAccess";
import { RemoveSource } from "../sources/RemoveSource";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { useAccountOptions } from "../../accounts/tie";
import { accountNameFrom } from "../../accounts/rows";
import { AvailableVersion } from "./AvailableVersion";
import { GitBranch, History } from "lucide-react";

import { Caption, Fact, Facts, Head, Panel } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { shortVersion, type PackageRow } from "../packages/rows";
import type { PartsHeld } from "../parts";
import { siteName, type SiteRow } from "../rows";
import type { CredentialRow } from "../sources/rows";
import { deploymentStateWord, siteStateWord, stateChip } from "../words";
import { AutoDeploySwitch, CredentialChip, PackageLifecycle } from "./stops/Source";

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
//
// ===========================================================================
// EVERY APP IT DECLARES IS A ROW, AND EVERY ROW OPENS (2026-09-05, D5/D6)
// ===========================================================================
// A deployed app opens its page. A declared one -- inactive, or never
// deployed -- opens the compose flow for THAT app: the owner asked for a way
// to activate an app from its source, and "click the app, read that it is
// inactive, press Activate" is that way. The rows read the same state words
// the list and the bar do.

export function SourceView({
  pkg,
  viewerUserId,
  onRemoved,
  apps,
  appsSettled = false,
  credentials,
  can,
  onBack,
  backLabel = "Deployables",
  onOpenHistory,
  onOpenApp,
  onOpenDeclared,
  onReview,
  attempts,
  pendingStatus,
  deployedBy,
}: {
  pkg: PackageRow;
  viewerUserId?: string;
  onRemoved?: () => void;
  /** The apps this source produced, from the root's site feed. */
  apps: readonly SiteRow[];
  appsSettled?: boolean;
  credentials: readonly CredentialRow[];
  /** The parts this session holds: the credential and the switch are `sources`, the cascade is `retire`. */
  can: PartsHeld;
  onBack: () => void;
  backLabel?: string;
  onOpenHistory: () => void;
  onOpenApp: (siteId: string) => void;
  /** Opens the compose flow for an app the source declares and has not deployed. */
  onOpenDeclared: (app: string) => void;
  /**
   * Reopen this source's pending run, either working or waiting for review.
   *
   * A parked run used to be reached from an app's row inside the old combined
   * list. Sources have a list of their own now, where a waiting one reads
   * "Review needed" -- and the act that answers that state belongs on this
   * page's bar, with the state, rather than hidden in a row (rule 12).
   */
  onReview?: () => void;
  onAsk?: (tag: string) => void;
  /** The newest pending run may still be working; only a parked run needs review. */
  pendingStatus?: string;
  /** How many runs actually await confirmation, for the history line. */
  attempts: number;
  /**
   * Who deployed this source last, in words: "you", a name the roster gave,
   * or "" when nothing honest can be said (epic memql#5289, task memql#5306).
   * Read off the newest parked run's requester, else the source's owner.
   */
  deployedBy: string;
}) {
  const accounts = useAccountOptions();
  const label = sourceName(pkg);
  const connections = useSourceConnections();
  const { access } = useSession();
  const mine = bare(pkg.ownerUserId) === bare(viewerUserId ?? access?.userId ?? "");
  const provenance = sourceRecord(pkg, credentials, connections.rows);
  const live = apps.filter((a) => siteStateWord(a) === "Live").length;
  // Declared by the manifest and never deployed -- the difference between what
  // the source SAYS it contains and what it has actually put on the internet.
  const deployedNames = new Set(apps.map((a) => a.packageDeployableName));
  const undeployed = pkg.declares.filter((d) => !deployedNames.has(d.name));
  const inactive = undeployed.filter((d) => pkg.disabledDeployables.includes(d.name)).length;
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
  // deployable, done on that deployable's page -- or, for one the source only
  // declares, on the compose flow its row opens.
  //
  // The bar stays, with no acts: it still reads what this source IS and how
  // many of its apps are live, which is what somebody who opened it came to
  // find out.
  const acts: Act[] = onReview === undefined ? [] : [{ label: pendingStatus === "awaiting_confirm" ? "Review" : "Resume setup", tone: "primary", onAct: onReview }];

  return (
    <div className="os-deploy-pane deployable-source-view" data-os-page-context={JSON.stringify({ page: "Source", packageId: pkg.id, source: label })}>
      <div className="os-deploy-scroll">
        <Panel label={`Source ${label}`}>
          <Head title={label} breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label }]} back={{ label: backLabel, onSelect: onBack }}>{can.sources && mine && pkg.sourceKind === "repo" && !pkg.sourceRemoved ? <RemoveSource pkg={pkg} onRemoved={onRemoved} /> : null}</Head>

          <Caption>
            {pkg.sourceKind === "repo" ? provenance.provenance : "Shared ZIP package"}
          </Caption>

          {pkg.sourceRemoved ? <Caption>Removed from Sources. Its deployables and repository history are retained.</Caption> : null}
          <div className="deployable-source-access"><CredentialChip pkg={pkg} credentials={credentials} /></div>
          {pkg.updateAvailable ? <Caption>A newer version is available: {shortVersion(pkg.latestKnownVersion)}. Open an app to review and deploy it.</Caption> : null}
          <Facts>
            <Fact label="MemQL account" value={accountNameFrom(accounts, pkg.accountId)} />
            {pkg.sourceKind === "repo" ? <Fact label="GitHub account" value={provenance.identity} /> : null}
            {pkg.sourceKind === "repo" ? <Fact label="GitHub target" value={provenance.binding ? `${provenance.target} (${provenance.binding.accountType === "Organization" ? "organization" : "personal account"})` : `${provenance.target} (from repository URL)`} /> : null}
            {pkg.sourceKind === "repo" ? <Fact label="Repository" value={pkg.repoUrl} /> : <Fact label="ZIP in Files" value={pkg.artifactId} />}
            {pkg.sourceKind === "repo" ? <Fact label="Tracking" value={pkg.repoRef === "" ? "default branch" : pkg.repoRef} /> : null}
            <Fact label="Deployed" value={pkg.deployedVersion === "" ? "" : shortVersion(pkg.deployedVersion)} mono />
            <Fact
              label="Latest upstream"
              value={pkg.latestKnownVersion === "" ? "" : shortVersion(pkg.latestKnownVersion)}
              mono
            />
            <Fact label="Added" value={formatMoment(pkg.createdAt)} />
            {deployedBy === "" ? null : <Fact label="Deployed by" value={deployedBy} />}
          </Facts>

          <AvailableVersion key={pkg.id} pkg={pkg} />

          {/* WHAT IT DECLARES, not only what it deployed.
              A site row is written only for an app that actually deployed, so
              an app skipped at the confirm gate had no row and was missing
              from this list -- on the very page whose subject is the source
              that declares it. The two are shown together and told apart by
              their state word. */}
          <section className="os-report-part">
            <h4 className="os-report-heading">
              <GitBranch size={12} aria-hidden /> Apps it produces
              {appsSettled ? <span className="os-head-meta">{apps.length + undeployed.length}</span> : null}
            </h4>
            {apps.length === 0 && undeployed.length === 0 ? (
              <Caption>
                Nothing yet. This repository has not been analyzed, so there is no reading of what it contains.
              </Caption>
            ) : (
              <RecordList><ul className="os-source-apps">
                {apps.map((app) => {
                  const word = siteStateWord(app);
                  return (
                    <li key={app.id}>
                      <RecordRow name={app.packageDeployableName || siteName(app)} secondary={app.hostname} state={word === "Live" ? "live" : stateChip(word)} tone={word === "Live" ? "accent" : "muted"} onOpen={() => onOpenApp(app.id)} />
                    </li>
                  );
                })}
                {undeployed.map((d) => {
                  const off = pkg.disabledDeployables.includes(d.name);
                  return (
                    <li key={`declared:${d.name}`}>
                      {/* A BUTTON, which opens the compose flow for this app.
                          It used to be inert, on the reasoning that the app
                          had no page to open -- but the flow that gives it one
                          is the page, and the owner asked to reach it from
                          here. */}
                      <RecordRow name={d.name} secondary="no address yet" state={off ? "inactive" : "not deployed"} onOpen={() => onOpenDeclared(d.name)} />
                    </li>
                  );
                })}
              </ul></RecordList>
            )}
            {inactive > 0 ? (
              <Caption>
                Open an inactive app to review its setup and activate it.
              </Caption>
            ) : null}
          </section>

          {pkg.sourceKind === "repo" && can.sources && mine ? <SourceAccess key={`${pkg.id}:${pkg.credentialId}:${pkg.sourceConnectionId}`} pkg={pkg} credentials={credentials} /> : null}
          {can.sources && pkg.status !== "archived" ? <AutoDeploySwitch pkg={pkg} /> : null}

          <button type="button" className="os-deploy-history-line" onClick={onOpenHistory}>
            <History size={12} aria-hidden />
            <span>
              History {attempts > 0 ? `· ${attempts} waiting for review` : ""}
            </span>
            <span aria-hidden>&#9656;</span>
          </button>

          {/* THE CASCADE LIVES HERE AND NOWHERE ELSE. It deactivates every app
              the source produced, so the only honest place for it is the page
              whose subject is the source. */}
          {can.retire ? <PackageLifecycle pkg={pkg} apps={apps} /> : null}
        </Panel>
      </div>

      <ActionBar
        // ARCHIVED IS NOT TRACKED. The word was hard-coded, so an archived
        // source said "Tracked" with its own Restore control directly above.
        state={pkg.status === "archived" ? "Archived" : pendingStatus !== undefined ? deploymentStateWord(pendingStatus) : "Tracked"}
        // COUNTED THE WAY THE LIST COUNTS, which is everything this source
        // declares -- deployed or not.
        detail={`${total} app${total === 1 ? "" : "s"}${live > 0 ? `, ${live} live` : ""}${
          inactive > 0 ? `, ${inactive} inactive` : ""
        }${undeployed.length - inactive > 0 ? `, ${undeployed.length - inactive} not deployed` : ""}${
          pkg.autoDeploy ? " -- deploys itself when the plan is unchanged" : ""
        }`}
        tone={pkg.status === "archived" ? "none" : live > 0 ? "live" : "none"}
        acts={acts}
      />
    </div>
  );
}
