import type { ConnectReturn } from "../sources/connectReturn";
import { ConnectReturnNotice } from "../sources/ConnectReturnNotice";
import { useEffect, useMemo, useRef, useState } from "react";

import { Rocket } from "lucide-react";
import { useSession } from "../../../chrome/access";
import { bare } from "../people";

import { Caption, Field, Notice, RefreshButton, useLiveView, type Stop } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { Wizard } from "../../../kit/Wizard";
import { AccountPicker } from "../../accounts/AccountPicker";
import { organizationChosen, useDefaultOrganization } from "../../accounts/organization";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { useCreateSite, usePublish } from "../actions";
import { useAddDomain } from "../domainActions";
import { hostnameFor } from "../hostname";
import { useNewPackage, usePackageActions, useSiteLifecycle } from "../packages/actions";
import { ProblemNotice, ReportView } from "../packages/ReportView";
import {
  deploymentFromRow,
  shortRepo,
  sourceLabel,
  type DeployableOutcome,
  type DeploymentRow,
  type PackageRow,
} from "../packages/rows";
import { usePackageDeployments } from "../packages/usePackages";
import { probeNote, probeParks, zipVerdict } from "../sources/probe";
import { type CredentialFeedStatus, type CredentialRow } from "../sources/rows";
import { useAddressChecks, useArtifactProbe, useSourceProbe } from "../sources/useProbes";
import { kindLabel, type StopId } from "../targets";
import { siteStateDetail } from "../words";
import {
  EMPTY_ADDRESS,
  EMPTY_DRAFT,
  addressKeyFor,
  appsToPlace,
  duplicateSource,
  movingStopFor,
  pathOf,
  phaseOf,
  placementsComplete,
  placementsFrom,
  runForScopedFlow,
  seedAddress,
  sourceReady,
  type AddressDraft,
  type AddressVerdicts,
  type ComposeDraft,
  type ComposePath,
  type ComposePhase,
} from "./compose";
import { everyOtherAppSkipped } from "../packages/calls";
import { BuildStop } from "./stops/Build";
import { CiHandoff } from "./stops/compose/CiHandoff";
import { Rail } from "./RailView";
import "../composition.css";
import { headActionFor, railFor, type ComposeInput, type HeadAction, type RailProblem, type RailStage } from "./rail";
import { partsForOrganization, type PartsHeld } from "../parts";
import { ManifestPreview } from "./stops/compose/ManifestPreview";
import { type ConnectionNeed } from "./stops/compose/RepositorySource";
import { SourceConnections } from "../sources/SourceConnections";
import { useSourceConnections, type SourceConnectionRow } from "../sources/connections";
import { ComposeSourceDetailStep, ComposeSourceKindStep, SOURCE_DETAIL_NAME, SOURCE_KIND_LABEL } from "./stops/compose/Source";
import { ComposeWhereItLivesStop } from "./stops/compose/WhereItLives";

// The compose reading (epic memql#4885, design D4): THE RAIL IS THE FORM.
//
// ===========================================================================
// THE SAME FIVE STOPS, ANSWERED INSTEAD OF READ
// ===========================================================================
// Add a deployable opens this in place of the list: the Head's title becomes
// "Add a deployable", a quiet Back returns to the list, and beneath the Head
// the five stops the page reads as facts are rendered as INPUTS -- Source
// open first, the rest not reachable yet, the bar's ONE action following
// the state. There is no Next, no Back between stops and no step number: the
// order is a law the pipeline enforces, and the next unanswered stop is the
// open one.
//
// ===========================================================================
// TWO PATHS THROUGH ONE RAIL
// ===========================================================================
// A repository, and a zip whose root holds a manifest, are the PACKAGE path:
// Analyze creates the source and parks a run with its report, the report
// names the apps, and Deploy places them. A CI push, and a zip that is a
// built site, are the HAND-MADE path: there is nothing to analyze, so
// What-it-is is answered by the probe the moment the source is chosen,
// Analyze creates the draft site, and Deploy publishes the zip -- or, for a
// CI push, there is nothing left to do here and the Live stop waits.
//
// ===========================================================================
// ONE FLOW FOR AN APP A SOURCE ALREADY DECLARES (2026-09-05, D5 and D6)
// ===========================================================================
// Opened from a declared app's row -- inactive, or never deployed -- the same
// rail runs with its Source stop already answered by the source that exists.
// An INACTIVE app opens with a notice at the top saying so and Activate on
// the bar; nothing is written until it is pressed, and pressing it takes the
// app off the source's off-list and reads the source, which is the ordinary
// first step. Where it lives then asks about this app ALONE: the scope used
// to reach the wire and never the form.
//
// ===========================================================================
// THE FLOW ENDS ON BUILT, WITH GO LIVE BESIDE DONE (2026-09-05, D10)
// ===========================================================================
// A first deploy leaves the site with its files in place and not live, on
// purpose. The bar used to read "Deployed", then "Published 1 app", then "not
// serving yet" -- three words for one state, two of which sounded final. It
// reads Built now, in the one vocabulary every other surface uses, and offers
// Go live right there, so the two-step is one screen.
//
// ===========================================================================
// WHERE THE FLOW IS COMES FROM THE ROWS, NEVER FROM A STEP COUNTER
// ===========================================================================
// A run advances by writing its own status on a node nobody in this browser
// is talking to, so the phase is a READING (`compose.ts`'s `phaseOf`) over
// the newest run and what this flow has created. That is also what makes
// closing the window and reopening from the list land in the same place: the
// parked run IS the state.

export interface ComposePageProps {
  connectResult?: ConnectReturn;
  clusterDomain: string;
  /** The parts this session holds (epic memql#5289); the app reads them once. */
  can: PartsHeld;
  /** A CI-pushed source is a cluster-owner act, and not a part. */
  isClusterOwner: boolean;
  viewerUserId: string;
  /** The caller's credential cards, from the root feed, for the Source stop's picker. */
  credentials: readonly CredentialRow[];
  credentialFeed?: CredentialFeedStatus;
  /** The quiet Back: the list is what this replaced. */
  onBack: () => void;
  backLabel?: string;
  onAsk?: (tag: string) => void;
  /** A parked run and its source, when a "will serve" row reopened the reading. */
  parked?: { pkg: PackageRow; run: DeploymentRow };
  /**
   * The SOURCE this flow is about, independent of whether a run has parked.
   *
   * `parked` is null until the analysis parks its gate, and stays null when
   * the analysis FAILS -- so a flow entered for a declared app derived no
   * package id at all in either window, which made its own Retry a no-op on
   * the one screen that offers it. The id, the manifest's app names and the
   * off-list all come from here; `parked` remains the RUN.
   */
  source?: PackageRow;
  /**
   * Deploy ONLY this app, leaving every other one the source declares alone.
   *
   * Set when the flow was entered from an app the source declares. Every
   * other app is sent explicitly skipped; without that the engine defaults
   * them to not-skipped and one app's first deploy rebuilds and republishes
   * all of them. It also narrows Where it lives to this app (D6).
   */
  only?: string;
  /**
   * The manifest apps of that source that ALREADY have a site, from the app
   * root's site feed. Their addresses were chosen on their first deploy and
   * the pipeline never re-reads them, so this stop does not re-ask.
   */
  placed?: readonly string[];
  /**
   * Every source the caller can see, for the one-source-once check (D8): a
   * repository this cluster already tracks at that ref parks the Source stop
   * and names the source, before Analyze would be refused for it.
   */
  packages?: readonly PackageRow[];
  packageFeed?: { state: string; error: string; retry: () => void };
  siteFeed?: { state: string; error: string };
  placedSources?: readonly { packageId: string; name: string }[];
}

export function ComposePage(props: ComposePageProps) {
  const { clusterDomain, can: globalCan, isClusterOwner, credentials, onBack, backLabel = "Deployables", parked, only, source: fixedSource, packages } = props;
  const [selectedConnectionId, setSelectedConnectionId] = useState("");
  const connections = useSourceConnections();
  const [selectedRunId, setSelectedRunId] = useState("");
  const [sourceNotice, setSourceNotice] = useState("");
  const [draft, setDraft] = useState<ComposeDraft>(() => props.connectResult ? { ...EMPTY_DRAFT, choice: "repo" } : EMPTY_DRAFT);
  const [addresses, setAddresses] = useState<Record<string, AddressDraft>>({});
  // What this flow has CREATED. Held rather than derived, because it is the
  // one thing about the flow that is not a reading of a row this browser
  // already holds: the package and site feeds are the app root's, and the new
  // rows reach them on their own broadcasts rather than from here.
  const [created, setCreated] = useState({ packageId: "", siteId: "" });
  const [publishedZip, setPublishedZip] = useState(false);
  // WHAT THIS FLOW SWITCHED ON. Two acts whose consequence lands on a feed
  // this page does not read back: activating an inactive app (the off-list is
  // the package row's) and going live at the end (the site row's). Both are
  // held so the bar answers the click immediately rather than a broadcast
  // later; neither inserts a row locally.
  const [activated, setActivated] = useState(false);
  const [wentLive, setWentLive] = useState(false);
  const [liveIds, setLiveIds] = useState<string[]>([]);
  const [journeyChoice, setJourneyChoice] = useState<{ key: string; stop: WizardStep } | null>(null);
  const { access } = useSession();
  const githubViewer = bare(access?.userId ?? "");
  const selectedConnection = connections.rows.find(c => c.id === selectedConnectionId && c.status === "active" && bare(c.ownerUserId) === githubViewer);
  const githubGrant = credentials.find(c => c.id === selectedConnection?.credentialId && bare(c.ownerUserId) === githubViewer && !connections.revokedCredentialIds.includes(c.id));
  const [connectionNeed, setConnectionNeed] = useState<ConnectionNeed>("");

  const probe = useSourceProbe();
  const zipProbe = useArtifactProbe();
  const checks = useAddressChecks();
  const { accounts, state: accountState, error: accountError } = useAccountOptionsFeed();
  const defaultAccountId = useDefaultOrganization(accounts);
  const [pickedAccountId, setAccountId] = useState("");
  const [accountChosenManually, setAccountChosenManually] = useState(false);
  // Capture a verified default once. A feed refresh must not switch a draft.
  useEffect(() => {
    if (defaultAccountId && !accountChosenManually) setAccountId(held => held || defaultAccountId);
  }, [defaultAccountId, accountChosenManually]);
  const sourceAccountId = parked?.pkg.accountId || fixedSource?.accountId || pickedAccountId || defaultAccountId;
  // A registered repository may already exist. Reuse it only in this account;
  // the GitHub Source choice is a different object and never a package picker.
  const matchingPackage = draft.choice === "repo" && draft.repoUrl
    ? duplicateSource(packages ?? [], draft.repoUrl, draft.repoRef, probe.reply?.defaultBranch ?? "") : null;
  const selectedSource = matchingPackage?.accountId === sourceAccountId ? matchingPackage : undefined;
  const source = fixedSource ?? selectedSource;
  const placed = props.placed ?? props.placedSources?.filter(site => site.packageId === source?.id).map(site => site.name) ?? [];
  const placementsKnown = !props.siteFeed || (props.siteFeed.state === "live" && !props.siteFeed.error);
  const saveBoundary = useRef("");
  saveBoundary.current = JSON.stringify([githubViewer, selectedConnection?.id, sourceAccountId, githubGrant?.id, githubGrant?.status, access?.everyAccount, access?.accountIds]);
  const saveMounted = useRef(true);
  const analyzePending = useRef(false);
  useEffect(() => { saveMounted.current = true; return () => { saveMounted.current = false; }; }, []);
  const previousViewer = useRef(githubViewer);
  useEffect(() => {
    if (previousViewer.current === githubViewer) return;
    previousViewer.current = githubViewer;
    setSelectedConnectionId(""); setSelectedRunId(""); setAddresses({});
    setDraft({ ...EMPTY_DRAFT }); setCreated({packageId: "", siteId: ""});
    setAccountId(""); setAccountChosenManually(false); setJourneyChoice(null); probe.clear();
  }, [githubViewer]);
  useEffect(() => {
    if (!selectedConnectionId || fixedSource || parked || created.packageId) return;
    if (selectedConnection && githubGrant?.status === "active") return;
    setSelectedConnectionId(""); setSelectedRunId(""); setAddresses({});
    setDraft(held => ({...held, repoUrl: "", repoRef: "", credentialId: "", sourceConnectionId: ""}));
    setSourceNotice("The selected source is no longer available. Choose or reconnect a source.");
    setJourneyChoice(null); probe.clear();
  }, [selectedConnectionId, selectedConnection, githubGrant?.status, fixedSource, parked, created.packageId, probe.clear]);
  const can = partsForOrganization(sourceAccountId, globalCan, source || parked || created.packageId || created.siteId ? "update" : "create");

  const newPackage = useNewPackage();
  const pkgActions = usePackageActions();
  const createSite = useCreateSite();
  const publish = usePublish();
  const addDomain = useAddDomain();
  const lifecycle = useSiteLifecycle();

  const packageId = created.packageId || parked?.pkg.id || source?.id || "";
  const { source: timeline, reseed } = usePackageDeployments(packageId);
  const deployments = useLiveView(timeline, `compose-deployments:${packageId}`, (rows) =>
    newestFirst(rows.map(deploymentFromRow).filter((d) => d.id !== "")),
  );
  // THE RUN THIS COMPOSE IS ABOUT (memql#4953). `rows[0]` is the source's
  // newest run whatever app it names, and this flow is entered FOR one when
  // `only` is set -- so composing `web` while `storefront` deploys used to
  // read the sibling's run as the gate it was waiting on. A flow opened for
  // one app reads narrower still (`runForScopedFlow`): a whole-source run
  // that succeeded without ever placing this app is not this app's. Unscoped
  // compose (a brand-new source) still takes the newest, which is right:
  // there is one app and one run.
  const timelineRows = deployments?.snapshot.rows ?? [];
  const currentTimeline = selectedSource && !fixedSource && !parked ? timelineRows.filter(row => row.id === selectedRunId) : timelineRows;
  const run =
    (only !== undefined && only !== ""
      ? runForScopedFlow(currentTimeline, only)
      : (currentTimeline[0] ?? null)) ??
    parked?.run ??
    null;
  const report = run?.report ?? null;

  const zip = zipProbe.reply === null ? null : zipVerdict(zipProbe.reply);
  // A SOURCE THAT EXISTS IS THE PACKAGE PATH, whether or not a run has parked
  // yet: the flow opened for a declared app has its Source answered by the
  // row it was opened from, and reading it as "unknown" rendered the source
  // form under a title that named the source.
  const path: ComposePath = parked !== undefined || source !== undefined ? "package" : pathOf(draft, zip);
  const probeParked = probeParks(probe.reply?.reason ?? "");
  // ONE SOURCE, ONCE (D8): the check runs only while a NEW repository is being
  // chosen; a flow opened for an existing source is not registering one.
  const duplicate = matchingPackage && !selectedSource && !fixedSource && !parked ? matchingPackage : null;
  const githubSelectionReady = draft.choice !== "repo" || (selectedConnection !== undefined && githubGrant?.status === "active" &&
    draft.credentialId === githubGrant.id && draft.sourceConnectionId === selectedConnection.id && connectionNeed === "");
  const sourceDone = parked !== undefined || fixedSource !== undefined || (githubSelectionReady && organizationChosen(accounts, sourceAccountId) && sourceReady(draft, zip, probeParked, duplicate));

  // THE OFF-LIST, read off the source row (D5). `activated` answers the click
  // before the row's own broadcast does.
  const scoped = only !== undefined && only !== "";
  const inactive = scoped && source !== undefined && source.disabledDeployables.includes(only) && !activated;
  const archivedSource = source?.status === "archived";

  const phase = phaseOf({
    path,
    runStatus: run?.status ?? (selectedRunId ? "analyzing" : ""),
    siteId: created.siteId,
    // A CI-PUSHED SOURCE HAS NOTHING TO PUBLISH FROM HERE. Its bytes are on
    // somebody's CI, so the moment the draft site exists this flow is done
    // and the Live stop is what waits.
    published: draft.choice === "ci" ? created.siteId !== "" : publishedZip,
  });
  const journeyKey = `${phase}:${run?.id ?? ""}:${inactive}`;


  const placedKey = (placed ?? []).join("|");
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const apps = useMemo(() => appsToPlace(path, report, placed ?? [], only ?? ""), [path, report, placedKey, only]);
  const appsKey = apps.join("|");
  const sourceName = parked ? parked.pkg.name : (source?.name ?? draft.name);

  // EVERY APP STARTS WITH A GENERATED NAME, and only a MISSING one:
  // re-seeding one they have edited would undo their typing every time the
  // report re-arrived on an event.
  useEffect(() => {
    if (apps.length === 0) return;
    setAddresses((held) => {
      let changed = false;
      const next = { ...held };
      for (const app of apps) {
        if (next[app] !== undefined && (next[app]!.accountId !== "" || sourceAccountId === "")) continue;
        if (next[app] !== undefined) {
          next[app] = { ...next[app]!, accountId: sourceAccountId };
          changed = true;
          continue;
        }
        next[app] = { ...seedAddress(), accountId: sourceAccountId };
        changed = true;
      }
      return changed ? next : held;
    });
    // `appsKey` rather than `apps`: the array is rebuilt on every report
    // event, and keying on its identity would re-run this for a report
    // naming exactly the same apps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [appsKey, apps.length, sourceAccountId]);

  // DEPLOYING ONE DECLARED APP, and nothing else.
  //
  // `apps` is already just this app (D6). The problem is the others: an app
  // with a site gets no placement, the engine defaults it to not-skipped, and
  // deploying one app rebuilds and republishes every one of them -- the
  // complaint that took the Deploy button off the source page, in a new
  // place. So when the flow is scoped to one app, every OTHER app the report
  // names is sent explicitly skipped. That is the same `skip` a person ticks
  // by hand, and it is why this could not be built until skip actually
  // reached the engine.
  const everyApp = useMemo(() => (report?.deployables ?? []).map((d) => d.name).filter((n) => n !== ""), [report]);
  const placementApps = scoped ? everyApp : selectedSource ? [...new Set([...apps, ...placed])] : apps;
  const placementAddresses = useMemo(() => {
    if (!scoped) {
      if (!selectedSource) return addresses;
      const next = { ...addresses };
      for (const app of placed) next[app] = { ...EMPTY_ADDRESS, accountId: sourceAccountId, skip: true };
      return next;
    }
    const out: Record<string, AddressDraft> = { ...addresses };
    for (const app of everyApp) {
      if (app === only) continue;
      out[app] = { ...(out[app] ?? EMPTY_ADDRESS), skip: true };
    }
    return out;
  }, [scoped, addresses, everyApp, only, selectedSource, placedKey, sourceAccountId]);

  // THE CLUSTER'S VERDICTS, per app (D7): folded from the check hook's keys
  // into the shape `placementsComplete` reads, so Deploy stays out of reach
  // until every address going out has checked out.
  const verdicts = useMemo(() => {
    const out: Record<string, AddressVerdicts> = {};
    for (const app of apps) {
      const key = addressKeyFor(app);
      const slug = checks.verdicts[`${key}:slug`];
      const ownDomain = checks.verdicts[`${key}:domain`];
      out[app] = { ...(slug ? { slug } : {}), ...(ownDomain ? { ownDomain } : {}) };
    }
    return out;
  }, [apps, checks.verdicts]);

  const placementsDone = placementsComplete(apps, addresses, clusterDomain, verdicts);
  // Analyzing is `deploy`; registering a NEW source on the way is `sources`
  // (createPackage), so a flow opened with no source behind it needs both.
  const canStart = can.deploy && (source !== undefined || parked !== undefined || can.sources);
  const readyToAnalyze =
    canStart && !archivedSource && path !== "unknown" && sourceDone && (path !== "handmade" || placementsDone) && (!selectedSource || placementsKnown) && (!selectedSource || selectedSource.declares.length === 0 || selectedSource.declares.some(app => !placed.includes(app.name)));
  const readyToDeploy = path === "handmade" ? draft.artifactId !== "" : placementsDone && (!selectedSource || placementsKnown);

  const action = actionFor(phase, readyToAnalyze, readyToDeploy);
  const busy =
    newPackage.busy || pkgActions.busy || createSite.busy || publish.busy || addDomain.busy || lifecycle.busy;

  const outcomes: readonly DeployableOutcome[] = run?.deployables ?? [];
  const placementsLocked =
    phase === "deploying" || phase === "published" || (path === "handmade" && created.siteId !== "");
  const siteHostname = created.siteId === "" ? "" : hostnameFor(addresses[""]?.slug ?? "", clusterDomain);

  // -------------------------------------------------------------------------
  // The acts
  // -------------------------------------------------------------------------

  async function analyze(): Promise<void> {
    if (busy || analyzePending.current || (!fixedSource && !parked && draft.choice === "repo" && !githubSelectionReady)) return;
    analyzePending.current = true;
    try {
    const boundary = saveBoundary.current;
    if (source !== undefined || created.packageId !== "") {
      // AN APP OF A SOURCE THAT EXISTS: no package to create. The analysis
      // is SCOPED on the wire (memql#4953): the engine derives a run's
      // `scopedTo` from its placements' skips, so an unscoped call would park
      // a gate about the whole source that every sibling reads as its own.
      const outcome = await pkgActions.deploy(source?.id ?? created.packageId, {
        confirm: false,
        ...(scoped ? { placements: everyOtherAppSkipped(source?.declares ?? [], only) } : selectedSource ? { placements: Object.fromEntries(placed.map(name => [name, { hostname: "", accountId: sourceAccountId, ownDomain: "", skip: true }])) } : {}),
      });
      if (!saveMounted.current || boundary !== saveBoundary.current) return;
      if (selectedSource && outcome) setSelectedRunId(outcome.deploymentId);
      reseed();
      return;
    }
    if (path === "package") {
      const id = await newPackage.create({
        name: draft.name.trim(),
        sourceKind: draft.choice === "zip" ? "artifact" : "repo",
        repoUrl: draft.choice === "repo" ? draft.repoUrl.trim() : "",
        repoRef: draft.choice === "repo" ? draft.repoRef.trim() : "",
        autoDeploy: draft.choice === "repo" && draft.autoDeploy === true,
        credentialId: draft.choice === "repo" ? draft.credentialId.trim() : "",
        sourceConnectionId: draft.choice === "repo" ? selectedConnection?.id : undefined,
        artifactId: draft.choice === "zip" ? draft.artifactId : "",
        accountId: sourceAccountId,
      });
      if (id === "" || !saveMounted.current || boundary !== saveBoundary.current) return;
      setCreated((held) => ({ ...held, packageId: id }));
      // WITHOUT confirm: the run parks with its report and nothing is built.
      // The gate is always present (design D12), and this is it.
      await pkgActions.deploy(id, { confirm: false });
      if (!saveMounted.current || boundary !== saveBoundary.current) return;
      reseed();
      return;
    }

    const address = addresses[""] ?? EMPTY_ADDRESS;
    const siteId = await createSite.create(
      { slug: address.slug, kind: draft.kind, title: draft.name.trim(), storeId: draft.storeId ?? "", accountId: address.accountId },
      clusterDomain,
    );
    if (siteId === "") return;
    setCreated((held) => ({ ...held, siteId }));
    if (address.ownDomain.trim() !== "") await addDomain.add(siteId, address.ownDomain.trim());
    } finally { analyzePending.current = false; }
  }

  /**
   * Activate an inactive app (D5): off the off-list, then the ordinary first
   * step. One click rather than two, because "activate" without reading the
   * source would leave a person on a screen whose next act is the one they
   * just pressed.
   */
  async function activate(): Promise<void> {
    if (source === undefined || !scoped) return;
    if (!await pkgActions.enableDeployables(source.id, [only])) return;
    setActivated(true);
    await analyze();
  }

  async function deploy(): Promise<void> {
    if (path === "handmade") {
      const ok = await publish.publish(created.siteId, draft.artifactId);
      if (ok) setPublishedZip(true);
      return;
    }
    // SKIPPING IS DEACTIVATING (D5), a standing choice rather than a fact
    // about this run: the name lands on the source's off-list, the row reads
    // inactive, and activating it from its row is what deploys it later.
    //
    // ONLY WHAT THE PERSON TICKED. `placementAddresses` also carries the apps
    // that `only` skipped to scope a single-app deploy, and those are a
    // MECHANISM rather than a choice -- disabling them would turn "deploy just
    // this one" into "turn the others off".
    const declined = Object.entries(addresses)
      .filter(([, a]) => a.skip === true)
      .map(([app]) => app);
    // The names, not the resulting list (memql#4951): a membership change has
    // nothing to read, and `parked` is absent on the first-deploy path.
    if (!await pkgActions.disableDeployables(packageId, declined)) return;
    // THE PARKED RUN IS WHAT THIS CONFIRMS (memql#4954). Compose is where a
    // person reads the report and answers it, so the answer has to land on the
    // run that asked -- otherwise the gate they just passed stays open and the
    // bytes their report described are fetched again.
    await pkgActions.deploy(packageId, {
      confirm: true,
      placements: placementsFrom(placementApps, placementAddresses, clusterDomain),
      ...(run !== null && run.status === "awaiting_confirm" ? { deploymentId: run.id } : {}),
    });
    reseed();
  }

  async function retry(): Promise<void> {
    if (packageId === "" || busy) return;
    const boundary = saveBoundary.current;
    // A RETRY KEEPS THE SCOPE. Retrying a gate opened for one app used to
    // park an unscoped run -- the flow stayed titled and narrowed for that
    // app while the run underneath it was about the whole source, which is
    // the same fan-out the scoped analysis exists to prevent.
    const outcome = await pkgActions.deploy(packageId, {
      confirm: false,
      ...(scoped ? { placements: everyOtherAppSkipped(source?.declares ?? parked?.pkg.declares ?? [], only) } : selectedSource ? { placements: Object.fromEntries(placed.map(name => [name, { hostname: "", accountId: sourceAccountId, ownDomain: "", skip: true }])) } : {}),
    });
    if (!saveMounted.current || boundary !== saveBoundary.current) return;
    if (selectedSource && outcome) setSelectedRunId(outcome.deploymentId);
    reseed();
  }

  /**
   * Go live, from the end of the flow (D10): every app this run put in place
   * flips to live. The same status write the deployable's own bar makes, so
   * the same guard decides; a refusal renders beneath the rail.
   */
  async function goLive(): Promise<void> {
    const ids = path === "handmade" ? [created.siteId] : outcomes.map((o) => o.siteId ?? "").filter((id) => id !== "");
    if (ids.length === 0) return;
    for (const id of ids) {
      if (liveIds.includes(id)) continue;
      if (!await lifecycle.setStatus(id, "live")) return;
      setLiveIds(held => [...held, id]);
    }
    setWentLive(true);
  }

  function act() {
    switch (phase) {
      case "composing":
        void analyze();
        return;
      case "awaiting_confirm":
        void deploy();
        return;
      case "stopped":
        void retry();
        return;
      default:
        return;
    }
  }

  // -------------------------------------------------------------------------
  // The rail
  // -------------------------------------------------------------------------

  const input: ComposeInput = {
    mode: "compose",
    only,
    ...stopsFor(phase, path, sourceDone, report !== null, inactive || archivedSource),
    probeReason: probeParked && probe.reply !== null ? probeNote(probe.reply) : "",
    report,
    problem: phase === "stopped" ? problemOf(run) : null,
    moving: movingStopFor(phase, run?.status ?? ""),
    prebuilt: path === "handmade",
    answers: {
      source: sourceAnswer(parked?.pkg ?? source ?? null, draft),
      ...(path === "handmade" ? { whatItIs: handMadeVerdict(draft) } : {}),
      ...(siteHostname === "" ? {} : { whereItLives: siteHostname }),
      ...(phase === "published" && publishedHosts(outcomes).length > 0
        ? { whereItLives: publishedHosts(outcomes).join(", ") }
        : {}),
      ...liveAnswer(phase, path, draft, siteHostname, outcomes, wentLive),
    },
  };

  // A NEW DEPLOYABLE OPENS ON THE CHOICE, AND THE CHOICE OPENS THE STEP IT
  // NAMES. Reading it off the draft rather than off a click means a flow that
  // comes back with its choice already made -- from GitHub, after connecting --
  // lands on the repository step rather than on a question already answered.
  const defaultStop: WizardStep = phase === "composing" ? fixedSource || parked ? "whatItIs" : draft.choice === "" ? "source" : draft.choice === "repo" && !selectedConnection ? "githubSource" : "sourceDetail"
    : phase === "analyzing" || phase === "awaiting_confirm" && path === "package" ? "whatItIs"
    : phase === "stopped" ? (railFor(input).stages.find(s => s.state === "stopped")?.id as StopId ?? "whatItIs")
    : phase === "deploying" || phase === "awaiting_confirm" && path === "handmade" ? "build" : "live";
  const journeyStop = journeyChoice?.key === journeyKey ? journeyChoice.stop : defaultStop;
  const chooseStop = (stop: WizardStep) => setJourneyChoice({ key: journeyKey, stop });

  const sourceLocked = parked !== undefined || fixedSource !== undefined || phase !== "composing";
  const accountField = () => <Field label="Accounts">
    <div className="os-compose-account-control">
      <AccountPicker id="compose-source-organization" label="Accounts" required requiredLabel="Choose an account"
        value={sourceAccountId} accounts={accounts} disabled={sourceLocked || busy} onChange={setAccountId}
        onCommit={() => setAccountChosenManually(true)} />
      {!sourceLocked && !fixedSource && !parked && !accountChosenManually && organizationChosen(accounts, sourceAccountId) ? <Caption>Selected by default. Review account.</Caption> : null}
      {accountError ? <Caption>Accounts could not be refreshed. {String(accountError)}</Caption> : accounts.length === 0 ? <Caption>{accountState === "seeding" ? "Loading accounts…" : "No account is available for this deployable."}</Caption> : null}
    </div>
  </Field>;
  const stopBody = (stage: RailStage) => {
    switch (stage.id) {
      case "source":
        return (
          <>
          <div className="os-compose-source-fields">
          {draft.choice !== "repo" ? accountField() : null}
          {sourceLocked && source ? <Caption>{source.name} · {sourceLabel(source)}</Caption> : <ComposeSourceDetailStep
            draft={draft} onDraft={(patch) => setDraft(held => ({ ...held, ...patch }))}
            connection={selectedConnection} probe={probe} zipProbe={zipProbe} zip={zip}
            siteId={created.siteId} clusterDomain={clusterDomain} locked={sourceLocked}
            onConnectionNeed={setConnectionNeed} duplicateOf={duplicate} />}
          {draft.choice === "repo" && (draft.repoUrl || sourceLocked) ? accountField() : null}
          {selectedSource && !placementsKnown ? <Caption>{props.siteFeed?.error ? "Existing deployables could not be read. Refresh Deployables before continuing." : "Checking existing deployables…"}</Caption> : null}
          {selectedSource && props.siteFeed?.error && props.packageFeed?.retry ? <RefreshButton label="Refresh deployables" onClick={props.packageFeed.retry} /> : null}
          {selectedSource && placementsKnown && selectedSource.declares.length > 0 && selectedSource.declares.every(app => placed.includes(app.name)) ? <Caption>All apps from this repository already have deployables. Open them from Deployables.</Caption> : null}
          </div>
          </>
        );
      case "whatItIs":
        // THE PREVIEW STANDS IN UNTIL THE REPORT EXISTS, and never beside it.
        // A probe under a grant answers what the manifest CLAIMS, which is
        // enough to recognise the package you just picked; the run answers
        // what this cluster FOUND, and the moment it has, the claim is the
        // weaker of two sentences about one thing.
        if (report === null) {
          // ...and a stop nobody can reach yet says nothing, which is the
          // rule the address stop below states: a preview inside a pending
          // stop would contradict the mark beside it.
          if (stage.state === "pending" || stage.state === "ahead") return null;
          // An app a source already declares: what the last analysis said it
          // was, until this one says more.
          if (source !== undefined && scoped) {
            const declared = source.declares.find((d) => d.name === only);
            return (
              <div className="os-stop-body">
                <Caption>
                  {only} is declared as {declared ? kindLabel(declared.kind) : "an app"} by this source. Analyze reads
                  the source again for its build plan.
                </Caption>
              </div>
            );
          }
          if (probe.reply === null) return null;
          return (
            <div className="os-stop-body">
              <ManifestPreview manifest={probe.reply.manifest} />
            </div>
          );
        }
        return (
          <div className="os-stop-body">
            <ReportView report={report} only={only} />
          </div>
        );
      case "whereItLives":
        // Not reachable yet is not a form: a stop nobody can answer draws its
        // blurb and nothing else.
        if (stage.state === "pending" || stage.state === "ahead") return null;
        return (
          <ComposeWhereItLivesStop
            apps={apps}
            sourceName={sourceName}
            addresses={addresses}
            onAddress={(app, patch) =>
              setAddresses((held) => ({ ...held, [app]: { ...(held[app] ?? EMPTY_ADDRESS), ...patch } }))
            }
            accounts={accounts}
            canBindDomain={can.domains}
            clusterDomain={clusterDomain}
            outcomes={outcomes}
            /* CHOSEN ONCE. The moment the flow has written something at these
               addresses -- a package run past its gate, or the hand-made
               draft site -- the fields become facts: editing a slug that
               `createSite` already claimed changes nothing, and a field that
               accepts a value and does nothing is worse than no field. */
            locked={placementsLocked}
            checks={checks}
            verdicts={verdicts}
          />
        );
      case "build":
        return <>
          <BuildStop run={run} app={only ?? ""} refusal={run?.error ?? null} />
          {run ? <Rail input={{ mode: "deploy", deployment: run }} /> : null}
        </>;
      case "live":
        return <>
          {draft.choice === "ci" && created.siteId ? <CiHandoff siteId={created.siteId} name={draft.name} clusterDomain={clusterDomain} /> : null}
          {outcomes.length || created.siteId ? <ComposeWhereItLivesStop apps={apps} sourceName={sourceName} addresses={addresses}
            onAddress={() => {}} accounts={accounts} canBindDomain={can.domains} clusterDomain={clusterDomain}
            outcomes={outcomes} locked checks={checks} verdicts={verdicts} /> : null}
          {run?.error ? <ProblemNotice problem={run.error} tone="error" /> : null}
        </>;
      default:
        return null;
    }
  };

  // THE FORWARD ACT LIVES ON THE BAR, NOT IN THE HEAD (rule 12, epic
  // memql#4937). Cancel sits beside it, so leaving is as reachable as
  // continuing. An inactive app's bar offers Activate instead, and an app of
  // an archived source offers nothing but the way back: a deploy of an
  // archived source is refused server-side, and the source's own page is
  // where Restore is.
  const barActs: Act[] = archivedSource
    ? []
    : inactive
      ? can.retire
        ? [{ label: "Activate", tone: "primary", busy, onAct: () => void activate() }]
        : []
      : action === null || action.disabled
        ? []
        : [{ label: action.label, tone: action.tone, busy, onAct: act }];

  const title = composeTitle(parked?.pkg ?? fixedSource, only);
  // AN INACTIVE APP, OR ONE OF AN ARCHIVED SOURCE, IS HELD: whatever the
  // timeline says, the only act is Activate (or the way back), and the bar
  // must not read a finished flow off a run that never placed this app.
  const held = inactive || archivedSource;
  const finished = phase === "published" && !held;
  const canGoLive = finished && !wentLive && can.publish && (path !== "handmade" || draft.choice !== "ci");
  const journeyActs: Act[] = !held && !finished && (
    journeyStop === "source" || journeyStop === "githubSource" ? []
    : journeyStop === "sourceDetail" && path === "handmade" && sourceDone && phase === "composing"
      ? [{ label: "Continue", tone: "primary", onAct: () => chooseStop("whatItIs") }]
      : journeyStop === "whatItIs" && (phase === "awaiting_confirm" && path === "package" || phase === "composing" && path === "handmade")
        ? [{ label: "Choose addresses", tone: "primary", onAct: () => chooseStop("whereItLives") }]
        : false
  ) || barActs;

  // THE STEPS, off the pipeline's own reading. Completion, failures and
  // availability come from `railFor`, never from a local step counter; which
  // step is OPEN is the only thing a click changes.
  const awaitingLive = finished && !wentLive;
  // WHICH WAY IN, as the rail says it. A flow opened for a source that already
  // exists never asked, so it reads the answer off the source.
  const kind = draft.choice !== "" ? draft.choice : (parked?.pkg ?? source) === undefined ? "" : (parked?.pkg ?? source)!.sourceKind === "artifact" ? "zip" : "repo";
  const steps: Stop[] = railFor(input).stages.flatMap((stage): Stop[] => {
    const id = stage.id as StopId;
    const state = id === "live" && awaitingLive ? ("open" as const) : stage.state;
    if (id === "source") {
      // THE SOURCE STAGE IS TWO STEPS: the choice, and the step the choice
      // names. One question each -- which is the whole reason the old single
      // step read as crowded.
      const settled = state === "complete" || state === "done";
      const chosen = kind !== "";
      return [
        {
          id: "source",
          name: "Method",
          state: settled || (chosen && journeyStop !== "source") ? "complete" : "open",
          answer: chosen ? SOURCE_KIND_LABEL[kind] : "",
          // Chosen once something has been read from it: the kind is a fact.
          openable: !sourceLocked,
          body: sourceLocked ? undefined : (
            <ActivityTarget target="deployables:compose:source" className="deployable-journey-current">
              <ComposeSourceKindStep
                draft={draft}
                isClusterOwner={isClusterOwner}
                onChoose={(choice) => {
                  setSelectedConnectionId(""); setSelectedRunId(""); probe.clear();
                  setDraft((held) => ({ ...held, choice, name: "", kind: "", artifactId: "", repoUrl: "", repoRef: "", credentialId: "", sourceConnectionId: "", storeId: "" }));
                  // CHOOSING ANSWERS THE STEP, so the wizard moves on to the
                  // one the answer names.
                  chooseStop(choice === "repo" ? "githubSource" : "sourceDetail");
                }}
              />
            </ActivityTarget>
          ),
        },
        ...(kind === "repo" && !fixedSource && !parked ? [{
          id: "githubSource", name: "Source", state: selectedConnection || sourceLocked ? "complete" as const : "open" as const,
          answer: selectedConnection?.accountLogin ?? "", openable: !sourceLocked && !busy,
          body: <SourceConnections disabled={busy} mode="choose" selectedId={selectedConnectionId} credentials={credentials}
            credentialFeed={props.credentialFeed} connectResult={props.connectResult} returnSection="deployables"
            onChoose={(connection: SourceConnectionRow) => {
              setSelectedConnectionId(connection.id); setSelectedRunId(""); setSourceNotice("");
              setCreated({packageId: "", siteId: ""}); setAddresses({}); probe.clear();
              setDraft(held => ({...held, repoUrl: "", repoRef: "", name: "", credentialId: connection.credentialId, sourceConnectionId: connection.id}));
              chooseStop("sourceDetail");
            }} />,
        }] : []),
        {
          id: "sourceDetail",
          // Named by the answer above it, and by nothing until there is one.
          name: chosen ? SOURCE_DETAIL_NAME[kind]! : "Details",
          state: !chosen || kind === "repo" && !selectedConnection && !sourceLocked ? "ahead" : settled ? "complete" : journeyStop === "source" ? "waiting" : state,
          sentence: chosen ? DETAIL_SENTENCES[kind] : undefined,
          // The pipeline's own word on this stage -- what it settled as, or
          // why it stopped ("private, or not there") -- belongs to the step
          // that holds the fields it is about.
          answer: chosen ? stage.reason : "",
          openable: chosen && (kind !== "repo" || selectedConnection !== undefined || sourceLocked),
          body: chosen && (kind !== "repo" || selectedConnection !== undefined || sourceLocked) ? (
            <ActivityTarget target="deployables:compose:sourceDetail" className="deployable-journey-current">
              {stopBody({ ...stage, state })}
            </ActivityTarget>
          ) : undefined,
        },
      ];
    }
    const reachable = (state !== "pending" && state !== "ahead") || id === journeyStop;
    return [{
      id,
      name: STEP_NAMES[id],
      state,
      sentence: STEP_SENTENCES[id],
      answer: stage.reason,
      openable: reachable,
      body: reachable ? (
        <ActivityTarget target={`deployables:compose:${id}`} className="deployable-journey-current">
          {stopBody({ ...stage, state })}
        </ActivityTarget>
      ) : undefined,
    }];
  });

  // WHAT THE STEP THE CHOICE NAMES STILL NEEDS, in its own words. "Describe the
  // source -- the open step has more to answer" was true of every one of these
  // and said nothing about any of them.
  const detailNeeds: { word: string; detail: string } | null =
    phase !== "composing" || source !== undefined || parked !== undefined || sourceDone || draft.choice === "" || held
      ? null
      : draft.choice === "zip"
        ? { word: "Choose a zip", detail: "one you have put in Files" }
        : draft.choice === "ci"
          ? { word: "Name it", detail: "and say what kind of app your CI will push" }
          : !selectedConnection
            ? { word: "Choose a source", detail: "a GitHub account and personal or organization access" }
            : connectionNeed === "reconnect"
              ? { word: "Source needs attention", detail: "return to Source to reconnect or choose another" }
              : { word: "Choose a repository", detail: "then configure its branch, name and owning account" };
  const word = !sourceLocked && (draft.choice !== "repo" || draft.repoUrl !== "") && !organizationChosen(accounts, sourceAccountId) ? "Choose an account" : detailNeeds !== null ? detailNeeds.word : finished && draft.choice === "ci" ? "Waiting for CI" : composePhaseWord(phase, {
    inactive, archivedSource, declared: source !== undefined, wentLive, choice: draft.choice, moreToAnswer: action !== null && action.disabled,
  });
  const detail = detailNeeds !== null ? detailNeeds.detail : finished
    ? finishedSentence(path, draft, outcomes, siteHostname, wentLive)
    : inactive
      ? siteStateDetail("Inactive", "")
      : archivedSource
        ? "restore the source to deploy its apps again"
        : composePhaseDetail(phase, action, apps, addresses, source !== undefined, draft.choice);

  // THE FLOOR HAS TWO VERBS AND ONE BUTTON, the same as every add wizard.
  //
  //   CANCEL   while nothing has been written -- which is everything before
  //            Analyze -- so leaving costs nothing and the word is true.
  //   LEAVE    once something exists: a source that was read, a run that is
  //            parked or building. Going keeps it, the list reopens it, and a
  //            build the cluster has started carries on without anybody
  //            watching. There is no act here that undoes a run, so the floor
  //            does not offer one by calling it Cancel.
  //
  // The forward act is the button and the way out beside it is text. When it
  // is the cluster's turn there is no forward act, and Leave is the button --
  // the same picture the machine and the domain draw while they wait.
  const written = phase !== "composing" || created.packageId !== "";
  const acts: Act[] = finished
    ? canGoLive
      ? [
          { label: "Done", text: true, onAct: onBack },
          { label: "Go live", tone: "primary", busy, onAct: () => void goLive() },
        ]
      : [{ label: "Done", tone: "primary", onAct: onBack }]
    : journeyActs.length > 0
      ? [{ label: written ? "Leave" : "Cancel", text: true, onAct: onBack }, ...journeyActs]
      : written
        ? [{ label: "Leave", onAct: onBack }]
        : [{ label: "Cancel", text: true, onAct: onBack }];

  return (
    <Wizard
      className="deployable-journey"
      icon={<Rocket aria-hidden />}
      /* THE TITLE SAYS WHAT THIS IS. "Add a deployable" is true only when
         there is no source yet: opened for one app of a source added days
         ago, it read as though the whole source were being added again --
         which is exactly what the report beside it appeared to confirm. */
      title={title}
      lead={source === undefined && parked === undefined && only === undefined ? "Put a site or an app on this cluster." : undefined}
      breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label: title }]}
      back={{ label: backLabel, onSelect: onBack }}
      label="Deployable setup progress"
      steps={steps}
      open={journeyStop}
      onOpen={(id) => chooseStop(id as WizardStep)}
      status={{ word, detail, tone: phase === "analyzing" || phase === "deploying" ? "busy" : wentLive ? "live" : "none" }}
      acts={acts}
      context={{ page: title, packageId: (parked?.pkg ?? source)?.id, app: only, mode: "compose", step: journeyStop }}
      notices={
        <>
          {/* THE NOTICE AT THE TOP (D5): an inactive app says so before
              anything else on the page, and says what Activate does. */}
          {inactive ? (
            <Notice
              tone="info"
              sentence={`${only} is inactive.`}
              next="It was skipped when this source was deployed, so nothing was built for it and it has no address. Activate it to read the source again and choose where it lives; nothing is built until you deploy."
            />
          ) : null}
          {archivedSource ? (
            <Notice
              tone="warn"
              sentence="This source is archived."
              next="An archived source does not deploy. Restore it from its own page first; its apps come back inactive, ready to be activated."
            />
          ) : null}

          {/* A refusal the engine gave BEFORE any row exists has no stop to
              belong to yet, so it renders beneath the action that asked for
              it. Every other refusal in this flow lands at its stop. */}
          {sourceNotice ? <Caption>{sourceNotice}</Caption> : null}
          {newPackage.refusal ? <ProblemNotice problem={{ ...newPackage.refusal, fatal: true }} tone="error" /> : null}
          {pkgActions.refusal ? <ProblemNotice problem={{ ...pkgActions.refusal, fatal: true }} tone="error" /> : null}
          {createSite.error === "" ? null : (
            <Notice
              tone="error"
              sentence="This deployable was not created."
              next="Nothing was written. The name may already be taken -- this cluster's own answer is below."
              detail={createSite.error}
            />
          )}
          {addDomain.error === "" ? null : (
            <Notice
              tone="warn"
              sentence="It was created, but the domain was not bound."
              next="Add the domain on the deployable's own page, where the two DNS records are."
              detail={addDomain.error}
            />
          )}
          {publish.error === "" ? null : (
            <Notice
              tone="error"
              sentence="That zip was not deployed."
              next="The deployable exists and nothing is in place yet. Deploy again, or pick another zip."
              detail={publish.error}
            />
          )}
          {lifecycle.refusal ? <ProblemNotice problem={{ ...lifecycle.refusal, fatal: true }} tone="error" /> : null}
          {liveIds.length > 0 && !wentLive ? <Notice tone="warn" sentence={`${liveIds.length} app${liveIds.length === 1 ? " is" : "s are"} live.`} next="Go live again to finish the remaining apps." /> : null}
          <ConnectReturnNotice result={props.connectResult} />
        </>
      }
    >
      {can.deploy ? null : <Caption>You do not have permission to create deployables.</Caption>}
    </Wizard>
  );
}

/** A step of this wizard: the pipeline's own stops, plus the one the Source
 *  stage splits into. */
type WizardStep = StopId | "githubSource" | "sourceDetail";

/** What the step a choice names is for, beside its name. */
const DETAIL_SENTENCES: Readonly<Record<string, string>> = {
  repo: "Which repository, and how this cluster reaches it.",
  zip: "Which zip, and what to call it.",
  ci: "What to call it, and what kind of app it is.",
};

/** What each step is called on the rail. */
const STEP_NAMES: Record<StopId, string> = { source: "Source", whatItIs: "Review", whereItLives: "Address", build: "Build", live: "Live" };

/**
 * What an open step is for, beside its name.
 *
 * THE WIZARD'S OWN WORDS rather than the stops' shared blurbs, for two
 * reasons the blurbs could not have known about. They were written to stand
 * under a heading, so "Review" read "Review  Review the apps, build steps..."
 * on one line. And Source has none at all: its body IS three named options,
 * and a sentence listing the same three above them was the fourth place on
 * the page the list appeared (rule 7).
 */
const STEP_SENTENCES: Record<StopId, string | undefined> = {
  source: undefined,
  whatItIs: "What the source holds, and what deploying it would do.",
  whereItLives: "Where each app answers, and whose it is.",
  build: "Turn the source into files and put them in place.",
  live: "Make it available to visitors.",
};

/**
 * What this flow is about, for the Head and the Panel's label.
 *
 * "Add a deployable" is the ONLY-when-nothing-is-known answer, and both other
 * facts arrive independently: `only` is known from the click, while `parked`
 * lands when the analysis parks its gate seconds later. Reading the title off
 * `parked` alone meant an app-scoped flow was called "Add a deployable" for as
 * long as the analysis ran -- which is the frame the report was read in.
 */
function composeTitle(pkg: PackageRow | undefined, only: string | undefined): string {
  const app = only !== undefined && only !== "" ? only : "";
  const source = pkg === undefined ? "" : pkg.name || pkg.id;
  if (app !== "" && source !== "") return `Deploy ${app} from ${source}`;
  if (app !== "") return `Deploy ${app}`;
  if (source !== "") return `Deploy ${source}`;
  return "Add a deployable";
}

/** Where the flow is, in words -- the bar's left half, in the one vocabulary. */
function composePhaseWord(
  phase: ComposePhase,
  at: { inactive: boolean; archivedSource: boolean; declared: boolean; wentLive: boolean; choice?: string; moreToAnswer?: boolean },
): string {
  if (at.archivedSource) return "Archived";
  if (at.inactive) return "Inactive";
  switch (phase) {
    case "analyzing":
      return "Reading the source";
    case "awaiting_confirm":
      return "Ready to deploy";
    case "deploying":
      return "Deploying";
    case "published":
      return at.wentLive ? "Live" : "Built";
    case "stopped":
      return "Stopped";
    default:
      // An app a source declares and has not deployed is exactly that. A
      // source nobody has added yet used to read the page's title here, which
      // is the page's own title said a second time (rule 7) in the one place
      // that is meant to say what is NEEDED. So it says that.
      if (at.declared) return "Not deployed";
      if ((at.choice ?? "") === "") return "Choose a source";
      return at.moreToAnswer ? "Describe the source" : "Ready";
  }
}

/**
 * What the bar says beside the phase.
 *
 * On the placement step it counts what is actually going out, because that is
 * the number the forward act is about -- and because "0 of 2" is the honest
 * reading of a state where the act is absent (memql#4930). It also says what
 * Deploy DOES and does not do, so "Built" at the end is what was promised.
 */
function composePhaseDetail(
  phase: ComposePhase,
  action: HeadAction | null,
  apps: readonly string[],
  addresses: Readonly<Record<string, AddressDraft>>,
  declared: boolean,
  choice = "-",
): string {
  // NOTHING IS CHOSEN YET, so the bar's word already says what is needed
  // ("Choose a source") and the three choices are on the page. What is worth
  // saying beside it is the thing somebody about to start wants to know.
  if (phase === "composing" && !declared && choice === "") return "nothing is deployed until you confirm";
  if (phase === "awaiting_confirm") {
    if (apps.length > 1) {
      const going = apps.filter((app) => addresses[app]?.skip !== true).length;
      if (going === 0) return "pick at least one app to deploy";
      return `${going} of ${apps.length} apps, each at its own address -- deploying puts their files in place; going live is the step after`;
    }
    if (action !== null && action.disabled) return "the open step has more to answer";
    return "deploying puts its files in place; going live is the step after";
  }
  if (action !== null && action.disabled) return "the open step has more to answer";
  switch (phase) {
    case "analyzing":
      return "reading the tree and running the gates a node runs at boot";
    case "deploying":
      return "building, then putting the files in place -- nothing goes live until you say";
    case "stopped":
      return "the last attempt did not finish; retrying starts a fresh one";
    default:
      return declared ? "reading the source is the first step; nothing is deployed until you confirm" : "nothing is deployed until you confirm";
  }
}

// ---------------------------------------------------------------------------
// The readings
// ---------------------------------------------------------------------------

/** Which stops are answered and which one is open, by phase. */
function stopsFor(
  phase: ComposePhase,
  path: ComposePath,
  sourceDone: boolean,
  hasReport: boolean,
  held: boolean,
): { answered: StopId[]; open: StopId | null } {
  // An inactive app, or one of an archived source, is a source answered and
  // nothing else asked: the bar's Activate (or the source's Restore) is the
  // question, and an open stop beside a notice saying "not yet" would be two
  // questions at once.
  if (held) return { answered: ["source"], open: null };
  switch (phase) {
    case "composing":
      if (!sourceDone) return { answered: [], open: "source" };
      return path === "handmade"
        ? { answered: ["source", "whatItIs"], open: "whereItLives" }
        : { answered: ["source"], open: "whatItIs" };
    case "analyzing":
      return { answered: ["source"], open: null };
    case "awaiting_confirm":
      return path === "handmade"
        ? { answered: ["source", "whatItIs", "whereItLives"], open: "live" }
        : { answered: ["source", "whatItIs"], open: "whereItLives" };
    case "deploying":
      return { answered: ["source", "whatItIs", "whereItLives"], open: null };
    case "published":
      return { answered: ["source", "whatItIs", "whereItLives", "build", "live"], open: null };
    case "stopped":
      return { answered: hasReport ? ["source", "whatItIs"] : ["source"], open: null };
  }
}

/**
 * The Head's action for a phase.
 *
 * It goes THROUGH `headActionFor` rather than around it: that function is the
 * design's table, and a second answer beside it would be a second table. All
 * this decides is which row of it a phase is on.
 */
function actionFor(phase: ComposePhase, readyToAnalyze: boolean, readyToDeploy: boolean): HeadAction | null {
  switch (phase) {
    case "composing":
      return headActionFor({ at: "composing", sourceComplete: readyToAnalyze });
    case "awaiting_confirm":
      return headActionFor({ at: "awaiting_confirm", placementsComplete: readyToDeploy });
    case "analyzing":
    case "deploying":
      return headActionFor({ at: "running" });
    case "stopped":
      return headActionFor({ at: "refused_or_failed" });
    case "published":
      // Nothing from the table: Go live is offered beside Done by the page
      // itself, because it is the one act a finished flow has left.
      return null;
  }
}

/** What the Source stop's one-line answer says, from the source itself. */
function sourceAnswer(pkg: PackageRow | null, draft: ComposeDraft): string {
  if (pkg !== null) return sourceLabel(pkg);
  switch (draft.choice) {
    case "repo":
      return draft.repoUrl.trim() === ""
        ? ""
        : `${shortRepo(draft.repoUrl)} at ${draft.repoRef.trim() === "" ? "default branch" : draft.repoRef.trim()}`;
    case "zip":
      return draft.artifactId === "" ? "" : "a zip in Files";
    case "ci":
      return draft.name.trim() === "" ? "" : "pushed by your CI";
    default:
      return "";
  }
}

/** A hand-made deployable's verdict: nobody analyzed it, so it is what was chosen. */
function handMadeVerdict(draft: ComposeDraft): string {
  const kind = draft.kind === "" ? "" : kindLabel(draft.kind);
  if (kind === "") return "";
  return draft.choice === "ci" ? `${kind}, pushed by your CI` : `${kind}, already built`;
}

function liveAnswer(
  phase: ComposePhase,
  path: ComposePath,
  draft: ComposeDraft,
  siteHostname: string,
  outcomes: readonly DeployableOutcome[],
  wentLive: boolean,
): Partial<Record<StopId, string>> {
  if (phase !== "published") return {};
  if (path === "handmade" && draft.choice === "ci") {
    return {
      live:
        siteHostname === ""
          ? "Waiting for the first push from your CI."
          : `Waiting for the first push from your CI. ${siteHostname} starts serving when it lands.`,
    };
  }
  const published = publishedHosts(outcomes);
  const hosts = published.length > 0 ? published.join(", ") : siteHostname;
  if (hosts === "") return {};
  return { live: wentLive ? `Live at ${hosts}.` : `Built. In place at ${hosts}, not live yet.` };
}

/**
 * The hostnames a run actually put in place, from its outcomes.
 *
 * `?? ""` IS THE WHOLE FIX. The engine writes a skipped outcome as
 * `{name, refusal}` with NO hostname key, and `listOf<DeployableOutcome>`
 * casts the raw array without normalising -- so `hostname` is `undefined` at
 * runtime while the type says `string`, and the obvious `o.hostname !== ""`
 * is TRUE for it. Skipping one app of two and deploying reported two apps,
 * and the hostname list it joined carried an empty slot.
 *
 * A REFUSAL IS NOT THE TEST. `deployable_account_refused` and
 * `deployable_domain_refused` place the app and then fail the tie, so they
 * carry both a hostname and a refusal -- and they did land. The hostname is
 * the fact; the refusal is about something else.
 */
function publishedHosts(outcomes: readonly DeployableOutcome[]): string[] {
  return outcomes.map((o) => (o.hostname ?? "").trim()).filter((h) => h !== "");
}

/** The bar's line once the flow has finished, in the one vocabulary (D10). */
function finishedSentence(
  path: ComposePath,
  draft: ComposeDraft,
  outcomes: readonly DeployableOutcome[],
  siteHostname: string,
  wentLive: boolean,
): string {
  if (path === "handmade" && draft.choice === "ci") {
    return "The deployable is ready and its door is open. Nothing serves until your CI pushes. It is on the Deployables list now.";
  }
  // NO OUTCOMES AT ALL is the hand-made path: a zip is placed through
  // `sitePublishFromArtifact` rather than through a run, so there is no
  // per-deployable list to count and one site was placed. "Nothing was put
  // in place" is reserved for a run that REPORTED outcomes and placed none
  // -- which is a real state, and a different one.
  const hosts = outcomes.length === 0 ? (siteHostname === "" ? [] : [siteHostname]) : publishedHosts(outcomes);
  if (hosts.length === 0) return "Nothing was put in place. It is on the Deployables list now.";
  if (wentLive) return `${hosts.length === 1 ? "serving at" : "serving at"} ${hosts.join(", ")}. It is on the Deployables list now.`;
  return `${hosts.length === 1 ? "in place at" : `${hosts.length} apps in place at`} ${hosts.join(", ")}, not live yet -- Go live starts serving`;
}

/** The refusal that stopped the flow: the run's error, or the report's first fatal problem. */
function problemOf(run: DeploymentRow | null): RailProblem | null {
  if (run === null) return null;
  if (run.error !== null && run.error.message.trim() !== "") return run.error;
  const fatal = (run.report?.problems ?? []).find((p) => p.fatal);
  return fatal ? { code: fatal.code, message: fatal.message, scope: fatal.scope } : null;
}

/**
 * The timeline, newest first -- the page's own rule for its own reason: the
 * collection folds events in the order the cluster sent them, so `rows[0]`
 * would read a stale run exactly when a new one arrived.
 */
function newestFirst(rows: DeploymentRow[]): DeploymentRow[] {
  const at = (d: DeploymentRow): string => d.startedAt || d.createdAt;
  return [...rows].sort((a, b) => {
    const byTime = at(b).localeCompare(at(a));
    return byTime !== 0 ? byTime : b.id.localeCompare(a.id);
  });
}
