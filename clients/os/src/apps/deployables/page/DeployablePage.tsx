import { useEffect, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";
import { ChartLine, Lock, MessageSquareText, Receipt } from "lucide-react";

import { Button, Caption, Chip, Chips, Head, Input, Panel, useLiveView } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { OpenLogsButton } from "../../../logs/OpenLogs";
import { useAccountOptions } from "../../accounts/tie";
import { AccessCodesPanel } from "../access/AccessCodesPanel";
import { usePackageActions, useSiteLifecycle } from "../packages/actions";
import { ProblemNotice } from "../packages/ReportView";
import { everyOtherAppSkipped, type DeployTarget, type Placement, type SiteVersion } from "../packages/calls";
import { deploymentFromRow, recordedAsCandidate, type DeploymentRow, type PackageRow } from "../packages/rows";
import type { PartsHeld } from "../parts";
import { deployedByLabel, deployerOf, type NameOf } from "../people";
import { usePackageDeployments } from "../packages/usePackages";
import { boundStoreId, ownerLabel, siteName, type SiteRow } from "../rows";
import { ReviewsSection } from "../reviews/ReviewsSection";
import type { CredentialRow } from "../sources/rows";
import { confirmationWordFor } from "../words";
import { actsFor, candidateBlockedReason, candidateTargetOffered, runForApp, siblingRunInFlight, type ActName } from "./acts";
import { usePreviewReadiness, usePreviewWrites } from "../preview/usePreview";
import { DeployableWorkspace, type WorkspaceDetail } from "./DeployableWorkspace";
import { RuntimeSettingsPanel } from "./stops/RuntimeSettings";
import { LIST_TITLES, type Dimension } from "../statistics/read";
import { SiteStatisticsView, StatisticsPeriodControl, statisticsMeasured } from "../statistics/SiteStatisticsView";
import { StatisticsListPage } from "../statistics/StatisticsListPage";
import { destinationsOf } from "../storeStatistics/read";
import { StoreStatisticsSection } from "../storeStatistics/StoreStatisticsSection";
import "../composition.css";
import { railFor, refusalStopFor, type RailStage, type StandingInput } from "./rail";
import { BuildStop } from "./stops/Build";
import { IconButton } from "../../../kit/IconButton";
import { SourceStop } from "./stops/Source";
import { WhatItIsStop } from "./stops/WhatItIs";
import { WhereItLivesStop } from "./stops/WhereItLives";
import { DomainWizard } from "./stops/Domains";
import { ShopifyStorePanel } from "../store/ShopifyStorePanel";
import { RefusalNotice } from "../preview/PreviewSection";
import { useBundleFlip } from "./useBundleFlip";
import { VersionPage } from "./VersionPage";
import { bundledWithMemQL, rollBackLegal, versionLabel, versionState } from "./Versions";
import { VisitPage } from "./VisitPage";
import { VisitSiteButton } from "./VisitSiteButton";

// The deployable page (epic memql#4937, design sections C and D): ONE head,
// ONE rail, ONE bar.
//
// ===========================================================================
// WHAT THIS PAGE USED TO BE
// ===========================================================================
// Measured on a live cluster: 5,069px over 5.9 viewports, TWO stacked heads
// (the list's and this one's), THIRTEEN rails at three different meanings, 36
// controls -- with Pause at y=2412, Archive at 2499, and "archive this source
// AND EVERY APP IT PRODUCED" at 885, three sections higher than either. Six
// controls read "Retry", carrying two different promises.
//
// Three moves fix all of it, and each is a rule rather than a tidy-up:
//
//   1. THE PAGE REPLACES THE LIST (DESIGN.md rule 11). One head.
//   2. A SETTLED STOP IS ONE LINE, and exactly one stop is open -- chosen by
//      what is actually a question (`openStopFor`). One rail.
//   3. ACTS FOLLOW THE STATE, IN ONE PLACE (rule 12). The bar is pinned to
//      the bottom edge, so nothing is ever scrolled to.
//
// The history is one line at the foot of the rail and belongs to the SOURCE,
// which is also what stops it being drawn twice for a two-app package.
//
// ===========================================================================
// THE TIMELINE IS RETAINED HERE, NEVER AT THE ROOT
// ===========================================================================
// The site, package and credential feeds are the app root's -- one per
// concept, for the life of the window. A package's deployment TIMELINE is
// retained by this page (clients/os/README.md): keeping every package's
// timeline live would subscribe the window to every deploy in the cluster to
// render one.

export interface DeployablePageProps {
  site: SiteRow;
  /** Its source when package-produced, else null: hand-made. */
  pkg: PackageRow | null;
  /** The caller's credential cards, from the root feed. */
  credentials: readonly CredentialRow[];
  viewerUserId: string;
  /** Resolves a user id to a name the roster gave, "" when it did not (epic memql#5289, task memql#5306). */
  nameOf: NameOf;
  /** The parts this session holds (epic memql#5289); the app reads them once. */
  can: PartsHeld;
  clusterDomain: string;
  onAsk?: (tag: string) => void;
  /** The quiet Back to the list. */
  onBack: () => void;
  initialDetail?: WorkspaceDetail;
  shopifyResult?: string;
  openRevision?: number;
  backLabel?: string;
  /** Opens the source's own view. */
  onOpenSource?: (packageId: string, repositoryView?: "settings") => void;
  /** True while this deployable's delete is still tearing its domains down. */
  deleting?: boolean;
  /**
   * Told when a delete or a deactivation was accepted, so the section can say
   * what happened to the row that is no longer here. `what` is the verb the
   * person used, because the two free a name for different reasons.
   */
  onDeleted?: (siteId: string, what: "deleted" | "deactivated") => void;
}

export function DeployablePage({
  site,
  pkg,
  credentials,
  viewerUserId,
  nameOf,
  can,
  clusterDomain,
  onBack,
  backLabel = "Deployables",
  onOpenSource,
  deleting = false,
  onDeleted,
  initialDetail, shopifyResult, openRevision,
}: DeployablePageProps) {
  const { source: timeline, reseed } = usePackageDeployments(pkg?.id ?? "");
  const deployments = useLiveView(timeline, `deployments:${pkg?.id ?? ""}`, (rows) =>
    newestFirst(rows.map(deploymentFromRow).filter((d) => d.id !== "")),
  );
  // THIS APP'S RUN, not the source's newest (memql#4953). `rows[0]` was the
  // newest run of the whole package, whatever deployable it was about, and
  // everything on this page derived from it: while a run for `web` analyzed,
  // `storefront`'s page read "Building", hid Unpublish and Redeploy, and
  // offered a Cancel that killed `web`'s deploy from a page about
  // `storefront`. A run records its scope now, so this can ask.
  const app = site.packageDeployableName;
  const timelineRows = deployments?.snapshot.rows ?? [];
  const run = runForApp(timelineRows, app);
  // ...and a run of the same source that is NOT this app's, while it is in
  // flight. The bar keeps this app's own state and withholds only the acts
  // that would start a second run of one source. See ActsInput.siblingRun.
  const siblingRun = siblingRunInFlight(timelineRows, app);

  const accounts = useAccountOptions();
  const flipped = useBundleFlip(site);
  const headActions = usePackageActions();

  // WHERE THE GATE'S VERSION GOES (memql#5601): the live version -- the
  // default, and what every deploy did before -- or the candidate, which keeps
  // the live site serving what it serves while the new one is previewed.
  //
  // A GATE OPENS ON WHAT ITS RUN RECORDED. A run opened as a candidate keeps
  // that at the confirm unless the confirm says otherwise, so a gate showing
  // "Live version" over such a run would be showing the opposite of what an
  // unchanged click used to do. Held per GATE: a new parked run, or another
  // deployable, starts from its own record again, so an answer given to one
  // gate never confirms the next.
  const parkedRunId = run !== null && run.status === "awaiting_confirm" ? run.id : "";
  const recordedTarget: DeployTarget = parkedRunId !== "" && recordedAsCandidate(run, app) ? "candidate" : "serving";
  const [deployTarget, setDeployTarget] = useState<DeployTarget>(recordedTarget);
  // Which answer the last confirm from this page sent, so a refusal of a
  // candidate lands beside the choice while the gate is still the question.
  const [confirmedAs, setConfirmedAs] = useState<DeployTarget | null>(null);
  useEffect(() => {
    setDeployTarget(recordedTarget);
    setConfirmedAs(null);
  }, [site.id, parkedRunId, recordedTarget]);
  const offerCandidate = candidateTargetOffered({ site, pkg, run, can });
  const candidateBlocked = offerCandidate ? candidateBlockedReason(run) : "";
  const gateTarget: DeployTarget = offerCandidate && candidateBlocked === "" ? deployTarget : "serving";
  const lifecycle = useSiteLifecycle();
  const [zipOpen, setZipOpen] = useState(false);

  // THE OPEN STOP IS A READING, WITH AN OVERRIDE. `openStopFor` decides which
  // stop is the question; clicking another opens it instead, and clicking the
  // open one closes it. Cleared when the deployable changes, so a stop opened
  // on one is never carried onto another.
  const [detail, setDetail] = useState<WorkspaceDetail | null>(initialDetail ?? null);
  // WHICH VERSION THE VERSION PAGE IS ABOUT, and whether it had served when
  // its row was pressed. Its state is NOT kept: the page reads it from the
  // live row on every render (`versionOpen` below), so a roll back that lands
  // while the page is open turns it to Current and takes the act off the bar.
  const [chosenVersion, setChosenVersion] = useState<{ version: SiteVersion; served: boolean } | null>(null);
  // WHICH STATISTICS LIST IS OPEN WHOLE, held the way the version is: its
  // page needs the list it is about, and without one Statistics stands.
  const [listDimension, setListDimension] = useState<Dimension | null>(null);
  // WHETHER THE ACCESS CODES HOLD BACK: what they hold is the only copy.
  const [accessHeld, setAccessHeld] = useState(false);
  useEffect(() => {
    if (detail !== "access") setAccessHeld(false);
  }, [detail]);
  // WHICH PART OF "Addresses and client" IS OPEN. The domains list, one
  // binding's setup, or the add form -- each its OWN view with its own Head, so
  // the trail names the real depth and a list never shares a scroll column
  // with the detail it opened (DESIGN.md rule 11). The state lives HERE rather
  // than in the stop because the Head that owns the trail is drawn here.
  const [domainView, setDomainView] = useState<{ kind: "list" } | { kind: "domain"; id: string } | { kind: "add" }>({ kind: "list" });
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState("");
  useEffect(() => {
    setDetail(initialDetail ?? null);
    setChosenVersion(null);
    setListDimension(null);
    setDomainView({ kind: "list" });
    setConfirming(false);
    setTyped("");
  }, [site.id, initialDetail, openRevision]);

  // A VERSION'S ROW OPENS ITS PAGE. A typed confirmation standing in the bar
  // is about the deployable, and the version's bar carries none of the
  // deployable's acts, so it is dropped rather than carried under the page.
  function openVersion(version: SiteVersion, served: boolean) {
    setConfirming(false);
    setTyped("");
    setChosenVersion({ version, served });
    setDetail("version");
  }

  // A LIST'S "All pages" OPENS IT WHOLE, as a page one step under Statistics.
  function openList(dimension: Dimension) {
    setListDimension(dimension);
    setDetail("statisticsList");
  }

  const rail: StandingInput = { mode: "standing", pkg, app, run, site };
  const refusalStop = refusalStopFor(run);
  const name = pkg?.declares.find(d => d.name === app)?.displayName?.trim() || site.title || site.packageDeployableName || siteName(site);

  // WHAT THE ENGINE SAYS IS LEGAL about this deployable's candidate (epic
  // memql#5531). The bar offers a promotion only when the readiness has landed
  // AND says yes -- absent, never disabled, and the Preview section below
  // draws the reason when the answer is no.
  const previewRead = usePreviewReadiness(site);
  const previewReadiness = previewRead.readiness;
  // The promotion is the one preview write the BAR makes, so the hook lives
  // here rather than in the section: rule 12 puts every act that changes what
  // the public is served on the bar, and an act needs its writer beside it.
  const previewWrites = usePreviewWrites(previewRead.reread);
  const reading = actsFor({
    site,
    pkg,
    run,
    siblingRun,
    can,
    deleting,
    releasing: site.hostname,
    deployTarget: gateTarget,
    preview:
      previewReadiness === null
        ? null
        : {
            hasCandidate: previewReadiness.hasCandidate,
            canPromote: site.kind !== "shopify_storefront" && previewReadiness.canPromote && site.candidateRef !== site.bundleRef,
            canGoLive: previewReadiness.canGoLive,
            goLiveRefusal: previewReadiness.goLiveRefusal,
          },
  });
  // WHO DEPLOYED IT, off the rows already here: this app's newest run's
  // requester, else the site's owner (the deployer since PR #5284), else the
  // source's. Only the name is looked up, and only where the roster is
  // readable; a deploy nobody can name says nothing beyond the ownership chip.
  const deployedBy = deployedByLabel(deployerOf(run, site, pkg), viewerUserId, nameOf);

  function act(named: ActName) {
    // Whatever this act is, a refusal it draws is its own: only the candidate
    // confirm below marks one as belonging beside the choice.
    setConfirmedAs(null);
    switch (named) {
      case "Discard":
      case "Delete":
      case "Archive":
      case "Deactivate":
        setConfirming(true);
        return;
      case "Go live":
        void lifecycle.setStatus(site.id, "live");
        return;
      case "Take offline":
        void lifecycle.setStatus(site.id, "disabled");
        return;
      case "Restore":
        void lifecycle.restore(site.id);
        return;
      case "Promote the candidate":
        // THE VERSION IS NAMED, and the naming is the safety: the engine
        // refuses a promotion whose candidate is not the one stored, so a
        // candidate republished between this page reading it and the click
        // is refused rather than promoted by surprise.
        if (previewReadiness !== null) void previewWrites.promote(site.id, previewReadiness.candidateRef);
        return;
      case "Cancel":
        if (pkg !== null && run !== null) void headActions.cancel(pkg.id, run.id).then(reseed);
        return;
      case "Deploy":
      case "Deploy as candidate":
      case "Deploy the update":
      case "Redeploy":
      case "Retry the deploy":
        // A PARKED RUN'S DEPLOY IS THE CONFIRMATION, not a new run -- and it
        // says so on the wire now (memql#4954). The comment was here and the
        // id was not, so `packageDeploy` minted a fresh run for the
        // confirmation and left the parked one at the gate forever: the list
        // went on saying a deploy was waiting for an answer somebody had
        // already given, and a Retry's report described bytes the click did
        // not ship. Still scoped to this page's app, or confirming from here
        // would deploy every sibling the source declares.
        //
        // AND IT NAMES ITS TARGET, the live version included (memql#5601). The
        // engine records a target when a run opens and, at the confirm, an
        // explicit one wins while an omitted one reuses what was recorded --
        // so a Deploy that left it out could confirm, as a candidate, a run
        // somebody else opened as one, under a button promising the live site.
        if (pkg !== null && run !== null && run.status === "awaiting_confirm") {
          const target: DeployTarget = named === "Deploy as candidate" ? "candidate" : "serving";
          setConfirmedAs(target);
          void headActions
            .deploy(pkg.id, { confirm: true, placements: onlyThisApp(pkg, site, target), deploymentId: run.id })
            .then(reseed);
          return;
        }
        if (pkg === null) {
          // A hand-made deployable's next version IS a zip, so the act opens
          // the picker rather than starting a run there is no source for.
          setDetail("source");
          setZipOpen(true);
          return;
        }
        void headActions
          .deploy(pkg.id, { confirm: false, placements: onlyThisApp(pkg, site) })
          .then(reseed);
        return;
      default:
        return;
    }
  }

  // Which destructive act the confirmation is for. Delete and Discard are one
  // capability under two names; Archive is its own; Deactivate is the source
  // app's one destructive act (2026-09-05 design, D2).
  const confirmAct: ActName | null = confirming
    ? (reading.acts.find(
        (a) => a.name === "Delete" || a.name === "Discard" || a.name === "Archive" || a.name === "Deactivate",
      )?.name ?? null)
    : null;

  async function runConfirm() {
    if (confirmAct === "Archive") {
      const done = await lifecycle.archive(site.id, typed);
      if (done) setConfirming(false);
      return;
    }
    if (confirmAct === "Deactivate") {
      // ONE CAPABILITY, on the SOURCE the row names rather than on `pkg`: an
      // archived source is not in the active feed, so `pkg` is null for an
      // app the old cascade left archived -- and that is exactly the row
      // whose name still needs freeing.
      const done = await headActions.deactivate(site.packageId, site.packageDeployableName, typed);
      if (done) {
        setConfirming(false);
        onDeleted?.(site.id, "deactivated");
      }
      return;
    }
    const done = await lifecycle.remove(site.id, typed);
    if (done) {
      setConfirming(false);
      onDeleted?.(site.id, "deleted");
    }
  }

  const busy = lifecycle.busy || headActions.busy;
  // A refused candidate confirm, while the gate it answered is still open,
  // belongs beside the choice rather than at the top of the page.
  const refusalAtChoice = offerCandidate && confirmedAs === "candidate" && headActions.refusal !== null;
  const acts: Act[] = reading.acts.map((spec) => ({
    label: spec.name,
    tone: spec.tone,
    busy,
    onAct: () => act(spec.name),
    ariaLabel: `${spec.name} ${name}`,
  }));

  const stopBody = (stage: RailStage) => {
    const refusal = refusalStop === stage.id && run?.error ? run.error : null;
    switch (stage.id) {
      case "source":
        return (
          <SourceStop
            site={site}
            pkg={pkg}
            credentials={credentials}
            canDeploy={can.deploy}
            flipped={flipped}
            zipOpen={zipOpen}
            onZipOpenChange={setZipOpen}
            refusal={refusal}
            onOpenSource={pkg === null || !onOpenSource ? undefined : () => onOpenSource(pkg.id)}
          />
        );
      case "whatItIs":
        return <WhatItIsStop site={site} pkg={pkg} run={run} refusal={refusal} />;
      case "whereItLives":
        return (
          <WhereItLivesStop
            site={site}
            accounts={accounts}
            canBindDomain={can.domains}
            clusterDomain={clusterDomain}
            onOpenDomain={(id) => setDomainView({ kind: "domain", id })}
            onAddDomain={() => setDomainView({ kind: "add" })}
          />
        );
      case "build":
        return <BuildStop run={run} app={site.packageDeployableName} refusal={refusal} />;
      default:
        return null;
    }
  };

  if (detail === "whereItLives" && domainView.kind !== "list") {
    const toAddresses = () => setDomainView({ kind: "list" });
    // ONE SURFACE FOR A DOMAIN, whether it is being added or come back to: the
    // wizard opens on the name, or on the binding at whatever stage the
    // cluster has walked it to. It owns the whole pane -- its bar is the
    // window's floor, so it cannot sit inside a page that already has one.
    return <DomainWizard
      key={domainView.kind === "domain" ? domainView.id : "add"}
      site={site}
      name={name}
      domainId={domainView.kind === "domain" ? domainView.id : ""}
      trail={[{ label: backLabel, onSelect: onBack }, { label: name, onSelect: () => { setDomainView({ kind: "list" }); setDetail(null); } }, { label: "Addresses and client", onSelect: toAddresses }]}
      back={{ label: "Addresses and client", onSelect: toAddresses }}
      onLeave={toAddresses}
    />;
  }

  // THE STORE TAKES THE PANE, beside Addresses and for its reason (DESIGN.md
  // rules 9 and 11). A store's detail is credentials, a scope comparison, a
  // subscription record, a paired development store and a table per mirrored
  // concept -- which is a page of its own, with its own bar, rather than a
  // detail drawn under the deployable's. It is absent for anyone whose grants
  // do not reach `execute app:deployables/store`, so the branch cannot be
  // entered by a person the engine would then serve nothing to.
  if (detail === "store" && can.store) {
    const toOverview = () => setDetail(null);
    return <ShopifyStorePanel
      site={site} canBind={can.store} result={shopifyResult} revision={openRevision}
      onWritten={previewRead.reread} runs={timelineRows} can={can}
      trail={[{ label: backLabel, onSelect: onBack }, { label: name, onSelect: toOverview }, { label: "Store" }]}
      back={{ label: name, onSelect: toOverview }} />;
  }

  // REVIEWS TAKE THE PANE, for the Store's reason: a queue, one review with its
  // log and its acts, the store's decision log and its settings are pages, and
  // each draws its own Head under this one's trail. Absent without the reviews
  // part, and for a deployable that fronts no store.
  const reviewsStore = site.kind === "shopify_storefront" && !site.systemOwned ? boundStoreId(site) : "";
  if (detail === "reviews" && can.reviews && reviewsStore !== "") {
    const toOverview = () => setDetail(null);
    return <ReviewsSection
      site={site} storeId={reviewsStore} name={name} nameOf={nameOf} viewerUserId={viewerUserId}
      trail={[{ label: backLabel, onSelect: onBack }, { label: name, onSelect: toOverview }]}
      back={{ label: name, onSelect: toOverview }} />;
  }

  // STORE STATISTICS TAKE THE PANE, for Reviews' reason: the store's sales are
  // a page of their own under this one's trail. Drawn only for a holder of the
  // sales part (judged for the site's organization, as `can` is), on a
  // storefront that is not the cluster's own and has a store on either
  // destination. A DRAFT KEEPS IT (ruling R3): the figures are the store's
  // sales, which do not wait for this site to be published.
  const salesStores = destinationsOf(site);
  const storeStatistics = can.sales && site.kind === "shopify_storefront" && !site.systemOwned && (salesStores.production !== "" || salesStores.testing !== "");
  if (detail === "storeStatistics" && storeStatistics) {
    const toOverview = () => setDetail(null);
    return <StoreStatisticsSection key={site.id}
      site={site} name={name}
      trail={[{ label: backLabel, onSelect: onBack }, { label: name, onSelect: toOverview }]}
      back={{ label: name, onSelect: toOverview }} />;
  }

  // EVERY OTHER DETAIL CHANGES THE PAGE TOO (the owner's rule, 2026-11-03).
  // Traffic (now Statistics), Access codes and each stop's detail used to open
  // a dialog over the page; now each takes the page's place under its own
  // Head, so the trail names where the person is and both Backs -- the row's
  // and the browser's -- step out. The bar stays: the deployable's acts are
  // still the deployable's while one of its details is open. Visit and a
  // version's details followed on 2026-11-08 ("no pop-up at all"), and Logs
  // opens the Logs app. A version's page needs the version it is about, so
  // without one the overview stands; a whole statistics list needs its list,
  // so without one Statistics stands.
  const inPage = detail === null || detail === "whereItLives" || detail === "store" || detail === "reviews" || detail === "storeStatistics" || (detail === "version" && chosenVersion === null) ? null
    : detail === "statisticsList" && listDimension === null ? "statistics" : detail;
  // WHAT THE OPEN VERSION IS NOW, read from the live row rather than kept
  // from the click: one that has become the one serving has served.
  const versionOpen = inPage === "version" && chosenVersion !== null ? {
    version: chosenVersion.version,
    state: versionState(site, chosenVersion.version.bundleRef),
    served: chosenVersion.served || chosenVersion.version.bundleRef === site.bundleRef,
  } : null;
  const pageTitle = versionOpen !== null ? `Version ${versionLabel(versionOpen.version.bundleRef)}` : inPage === null ? "" : detailTitle(inPage, listDimension);
  // A NEW ACCESS CODE IS SHOWN ONCE, so Back waits for Done while one stands,
  // as the dialog's Close and Escape did: the row's Back is inert, and the
  // browser's Back lands on it.
  const holding = inPage === "access" && accessHeld;
  const toOverview = () => setDetail(null);
  // A WHOLE LIST IS ONE STEP UNDER STATISTICS: the trail names it and Back
  // returns there, not to the deployable.
  const underStatistics = inPage === "statisticsList";
  const toStatistics = () => setDetail("statistics");
  const detailView = inPage === null ? null : (
    <Panel label={`${pageTitle} for ${siteName(site)}`}>
      <Head navigationKey={`deployable:${site.id}:${inPage}${underStatistics ? `:${listDimension}` : ""}${versionOpen !== null ? `:${versionOpen.version.bundleRef}` : ""}`} title={pageTitle}
        breadcrumbs={[{ label: backLabel, onSelect: holding ? undefined : onBack }, { label: name, onSelect: holding ? undefined : toOverview }, ...(underStatistics ? [{ label: detailTitle("statistics", null), onSelect: toStatistics }] : []), { label: pageTitle }]}
        back={holding ? undefined : underStatistics ? { label: detailTitle("statistics", null), onSelect: toStatistics } : { label: name, onSelect: toOverview }}>
        {/* The period at the right of the Head (spec 8), where the page has statistics to read for it. */}
        {inPage === "statistics" && statisticsMeasured(site) ? <StatisticsPeriodControl /> : null}
      </Head>
      {inPage === "runtime" ? <RuntimeSettingsPanel site={site} canEdit={can.publish} onOpenStore={site.kind === "shopify_storefront" && can.store ? () => setDetail("store") : undefined} /> :
        inPage === "statistics" ? <SiteStatisticsView site={site} onShowAll={openList} /> :
        inPage === "statisticsList" && listDimension !== null ? <StatisticsListPage site={site} dimension={listDimension} /> :
        // KEYED, so a code held for one deployable is never drawn over another.
        inPage === "access" ? <AccessCodesPanel key={site.id} site={site} onBusyChange={setAccessHeld} /> :
        inPage === "visit" ? <VisitPage key={site.id} site={site} can={can} readiness={previewReadiness} readinessError={previewRead.error} onWritten={previewRead.reread} onOpenStore={() => setDetail("store")} /> :
        versionOpen !== null ? <VersionPage site={site} version={versionOpen.version} state={versionOpen.state} served={versionOpen.served} bundled={bundledWithMemQL(site)} /> :
        (() => { const stage = railFor(rail).stages.find(s => s.id === inPage); return stage ? <>{stage.reason ? <Caption>{stage.reason}</Caption> : null}{stopBody(stage)}</> : null; })()}
      {lifecycle.refusal ? <ProblemNotice problem={{ ...lifecycle.refusal, fatal: true }} tone="error" /> : null}
    </Panel>
  );

  // ON A VERSION'S PAGE THE BAR IS THE VERSION'S (design record 2026-11-08,
  // section 10): Roll back to this version when that is legal, and nothing
  // otherwise. Not the deployable's other acts: the page is about one version,
  // a bar holds three acts, and three of the deployable's could push this one
  // out. They come back with Back. A refusal stands on the page, above.
  const versionActs: Act[] = versionOpen !== null && rollBackLegal({ site, version: versionOpen.version, served: versionOpen.served, canPublish: can.publish }) ? [{
    label: "Roll back to this version",
    tone: "primary",
    busy: lifecycle.busy,
    onAct: () => void lifecycle.rollTo(site.id, versionOpen.version.bundleRef),
    ariaLabel: `Roll ${site.hostname} back to ${versionLabel(versionOpen.version.bundleRef)}`,
  }] : [];

  if (detail === "whereItLives") {
    const stage = railFor(rail).stages.find(stage => stage.id === "whereItLives");
    return <div className="os-deploy-pane deployable-workspace" data-os-page-context={JSON.stringify({ page: "Deployable", siteId: site.id, hostname: site.hostname, name, view: "Addresses and client" })}><div className="os-deploy-scroll">
      <Panel label={`Addresses for ${siteName(site)}`}>
        <Head title="Addresses and client" breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label: name, onSelect: () => setDetail(null) }, { label: "Addresses and client" }]}
          back={{ label: name, onSelect: () => setDetail(null) }} />
        {stage ? stopBody(stage) : null}
      </Panel>
    </div></div>;
  }

  return (
    <div className="os-deploy-pane deployable-workspace" data-os-page-context={JSON.stringify({ page: "Deployable", siteId: site.id, hostname: site.hostname, name, status: site.status, packageId: pkg?.id, view: inPage === null ? "Overview" : pageTitle })}>
      <div className="os-deploy-scroll">
        {detailView ?? <Panel label={`Deployable ${siteName(site)}`}>
          {/* Page navigation stays with the title; Ask belongs to the window. */}
          <Head navigationKey={`deployable:${site.id}`} title={name} breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label: name }]} back={{ label: backLabel, onSelect: onBack }}>
            <VisitSiteButton site={site} name={name} onOpenVisit={() => setDetail("visit")} />
            <OpenLogsButton iconOnly subject={site.id} subjectConcept={Concepts.PLATFORM_SITE} ariaLabel={`Logs for ${name}`} />
            {/* ABSENT ON A DRAFT, the way Access codes is absent where it would
                open nothing: a draft has never served. A system-owned site keeps
                it, for the note that says why its traffic is not recorded. */}
            {site.status !== "draft" ? <IconButton label="Statistics" onClick={() => setDetail("statistics")}><ChartLine size={16} aria-hidden /></IconButton> : null}
            {/* The store's sales, beside the website's Statistics, and in its
                place on a draft. Absent, never disabled, where it is not drawn. */}
            {storeStatistics ? <IconButton label="Store statistics" onClick={() => setDetail("storeStatistics")}><Receipt size={16} aria-hidden /></IconButton> : null}
            {/* ABSENT, NOT DISABLED (rule 12): a system-owned site is exempt from
                the whole axis, and a code opens a gate that only a package's own
                deployable can declare. */}
            {can.access && !site.systemOwned && site.packageDeployableName ? <IconButton label="Access codes" onClick={() => setDetail("access")}><Lock size={16} aria-hidden /></IconButton> : null}
            {/* A store's reviews, for whoever holds the reviews part. */}
            {can.reviews && reviewsStore !== "" ? <IconButton label="Reviews" onClick={() => setDetail("reviews")}><MessageSquareText size={16} aria-hidden /></IconButton> : null}
          </Head>

          <Chips label="Deployable facts">
            {deployedBy === "you" && ownerLabel(site, viewerUserId) === "yours" ? null : <Chip tone="muted">{ownerLabel(site, viewerUserId)}</Chip>}
            {site.apiProxy ? (
              <Chip title="This app can use the cluster API from its own address.">
                api proxy
              </Chip>
            ) : null}
            {/* PUBLIC WRITES ARE A FACT WORTH SEEING FROM THE LIST, not only
                from the settings stop. A deployable that accepts posts from
                anyone is the one an operator wants to notice while scanning,
                and the chip is how it says so without being opened. */}
            {site.shopperForms ? (
              <Chip title="Visitors can submit forms to this app from its own address.">
                public forms
              </Chip>
            ) : null}
            {site.systemOwned ? (
              <Chip title="Managed by the cluster. Cannot be deleted here.">
                system-owned
              </Chip>
            ) : null}
            {deployedBy === "" ? null : (
              <span className="os-deploy-by" data-os-deployed-by>
                deployed by {deployedBy}
              </span>
            )}
          </Chips>

          {headActions.refusal && !refusalAtChoice ? (
            <ProblemNotice problem={{ ...headActions.refusal, fatal: true }} tone="error" />
          ) : null}

          {/* GO LIVE IS ABSENT FROM THE BAR WHEN THE ENGINE WOULD REFUSE IT
              (Connect Shopify, D5), and the reason is drawn here instead,
              with the way to the store when that is where it is cleared. The
              Store panel is absent without the store part, so is the way. */}
          {reading.withheld && site.kind !== "shopify_storefront" ? (
            <RefusalNotice refusal={reading.withheld} storefront={can.store} onOpenStore={() => setDetail("store")} />
          ) : null}

          <DeployableWorkspace key={site.id}
            onUpdate={reading.acts.some(a => a.name === "Deploy the update") && !headActions.busy ? () => act("Deploy the update") : undefined}
            site={site} pkg={pkg} run={run} runs={timelineRows} can={can} accounts={accounts} canDomains={can.domains} canStore={can.store}
            timelineState={deployments?.snapshot.state ?? "disconnected"}
            timelineError={deployments?.snapshot.error ?? ""} onRetryRead={reseed}
            onInspect={setDetail} onOpenSource={() => pkg && onOpenSource ? onOpenSource(pkg.id) : setDetail("source")}
            onOpenSourceSettings={pkg && onOpenSource ? () => onOpenSource(pkg.id, "settings") : undefined}
            onOpenVersion={(version, _shown, served) => openVersion(version, served)}
            readiness={previewReadiness}
            deployChoice={offerCandidate ? {
              target: gateTarget,
              onChoose: (next) => {
                setDeployTarget(next);
                // A refusal of the candidate answered a question that changing
                // the choice stops asking.
                if (refusalAtChoice) {
                  headActions.clear();
                  setConfirmedAs(null);
                }
              },
              hostname: site.hostname,
              blocked: candidateBlocked,
              refusal: refusalAtChoice ? headActions.refusal : null,
            } : undefined}
          />

          {/* A refusal renders IN SURFACE, beside the rail -- never a toast,
              and never inside a dialog that then closes, because a refusal
              nobody can re-read is a refusal nobody can act on. */}
          {lifecycle.refusal ? (
            <ProblemNotice problem={{ ...lifecycle.refusal, fatal: true }} tone="error" />
          ) : null}
        </Panel>}
      </div>

      <ActionBar state={reading.state} detail={reading.detail} tone={reading.tone} acts={versionOpen !== null ? versionActs : confirming ? [] : acts}>
        {confirming && confirmAct ? (
          <ConfirmRow
            site={site}
            act={confirmAct}
            typed={typed}
            onTyped={setTyped}
            busy={lifecycle.busy || headActions.busy}
            onCancel={() => {
              setConfirming(false);
              setTyped("");
            }}
            onConfirm={() => void runConfirm()}
          />
        ) : null}
      </ActionBar>
    </div>
  );
}

/**
 * The typed confirmation, in the bar.
 *
 * IT SAYS WHAT IT COSTS BEFORE IT ASKS. A delete releases the address and
 * takes the deployable's domains down, and it is terminal -- archive is the
 * reversible step, one rung up. A deactivation releases the address too, and
 * is NOT terminal: the source still declares the app. Saying that where the
 * person is about to type is the difference between a confirmation and a
 * speed bump.
 *
 * THE WORD TO TYPE IS THE THING'S NAME (2026-09-05 design, D9): the address
 * label for a standalone, the app's manifest name for a source's app. Typing
 * `storefront.memql.example.com` for every archive and delete was the owner's
 * complaint, and the server accepts the label.
 */
function ConfirmRow({
  site,
  act,
  typed,
  onTyped,
  busy,
  onCancel,
  onConfirm,
}: {
  site: SiteRow;
  act: ActName;
  typed: string;
  onTyped: (v: string) => void;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const removing = act === "Delete" || act === "Discard";
  const deactivating = act === "Deactivate";
  const word = deactivating ? site.packageDeployableName : confirmationWordFor(site.hostname);
  return (
    <>
      <span className="os-actbar-confirm">
        {deactivating ? (
          <>
            Deactivating takes <strong>{site.packageDeployableName}</strong> off the internet and releases{" "}
            <strong className="os-mono">{site.hostname}</strong> and any client domain bound to it. Its source still
            declares it, so it can be activated again and deployed at a fresh address.
          </>
        ) : removing ? (
          <>
            Deleting releases <strong className="os-mono">{site.hostname}</strong> and takes its domains down. The
            record stays in this cluster&rsquo;s history, but the deployable cannot be brought back &mdash; restore it
            from Archived instead if you are not sure.
          </>
        ) : (
          <>
            Archiving keeps this deployable and its whole history, and it can be restored. The name stays yours;
            Delete is what releases it.
          </>
        )}
      </span>
      <Input
        id={`os-confirm-${site.id}`}
        label={`Type ${word} to confirm`}
        value={typed}
        onChange={onTyped}
        placeholder={word}
      />
      <Button tone="quiet" onClick={onCancel}>
        Keep it
      </Button>
      <Button tone="danger" disabled={typed.trim() !== word} busy={busy} onClick={onConfirm}>
        {act}
      </Button>
    </>
  );
}

/**
 * Placements that redeploy THIS deployable and leave its siblings alone.
 *
 * THE SUBJECT OF THIS PAGE IS ONE APP. A deploy call with no placements is a
 * whole-PACKAGE run: the engine defaults every deployable to not-skipped, so
 * Redeploy on one app rebuilt and republished all of them. Worse, it FAILED
 * outright once a source declared an app that had never been deployed -- the
 * run refused with `deployable "web" has never been deployed and no hostname
 * was chosen for it`, on a page about a different app entirely, while the
 * site this page is about was serving perfectly.
 *
 * This used to be justified with "it needs no placements -- every app of an
 * existing deployable already has its address", which stopped being true the
 * moment a source recorded what it DECLARES as well as what it deployed.
 *
 * THIS app gets no entry: absent means not-skipped, and the engine republishes
 * through the (packageId, deployable name) key it recorded on the first
 * deploy, so its address is already remembered. Every OTHER declared app is
 * skipped BY NAME -- including ones the owner turned off, which the engine has
 * no other way to know about.
 */
function onlyThisApp(pkg: PackageRow, site: SiteRow, target?: DeployTarget): Record<string, Placement> {
  const placements = everyOtherAppSkipped(pkg.declares, site.packageDeployableName);
  // THE CONFIRM NAMES THIS APP'S TARGET (memql#5601); the analysis that opens
  // the gate names none, because the answer is given at the gate.
  if (target !== undefined && site.packageDeployableName !== "") {
    placements[site.packageDeployableName] = { hostname: "", accountId: "", ownDomain: "", target };
  }
  return placements;
}

/**
 * The timeline, newest first.
 *
 * The collection folds events in the order the cluster sent them, so a page
 * that read `rows[0]` as the newest run would read a STALE one the moment a
 * new run arrived by event -- exactly when somebody is watching.
 */
function newestFirst(rows: DeploymentRow[]): DeploymentRow[] {
  const at = (d: DeploymentRow): string => d.startedAt || d.createdAt;
  return [...rows].sort((a, b) => {
    const byTime = at(b).localeCompare(at(a));
    return byTime !== 0 ? byTime : b.id.localeCompare(a.id);
  });
}

// Each detail's title, as its page's Head and the trail name it. A whole
// statistics list is named by its list ("Pages"), Statistics without one.
function detailTitle(detail: WorkspaceDetail, dimension: Dimension | null): string {
  return { source: "Source", whatItIs: "App and deployment plan", whereItLives: "Addresses and client", build: "Build", runtime: "App values", statistics: "Statistics", statisticsList: dimension === null ? "Statistics" : LIST_TITLES[dimension], access: "Access codes", store: "Store", reviews: "Reviews", storeStatistics: "Store statistics", visit: "Visit", version: "Version" }[detail];
}
