import { Head } from "../../kit";
import { AddRepository } from "../pipelines/AddRepository";
import { LocalTabs } from "../../kit/LocalTabs";
import { PENDING_DEPLOYMENT_STATUSES } from "./packages/rows";
import { useEffect, useMemo, useState } from "react";
import { Concepts, type LiveSnapshot, type Row } from "@znasllc-io/memql-sdk-core/client";

import { Notice, roleAdmits, useAppReach } from "../../kit";
import { useDeployableParts } from "./parts";
import { useSession } from "../../chrome/access";
import { useLiveView } from "../../live/liveView";
import { useArrivals } from "../../live/useArrivals";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { DeployablesSection } from "./DeployablesSection";
import { ActivePane } from "./paneActivity";
import { MapSection, NO_SELECTION, type MapSelection } from "./map/MapSection";
import type { MapNode } from "./map/layout";
import { deploymentFromRow, packageFromRow, type DeploymentRow, type PackageRow } from "./packages/rows";
import { usePendingDeployments } from "./packages/usePendingDeployments";
import { usePackages } from "./packages/usePackages";
import { siteFingerprint, siteFromRow, type SiteRow } from "./rows";
import { SourceConnectionsProvider, useSourceConnections } from "../../modules/connections/connections";
import { ConnectionsPanel } from "../../modules/connections/ConnectionsPanel";
import { accountSetupState, githubAccountsFor } from "./sources/accountSetup";
import { ConnectReturnNotice } from "./sources/ConnectReturnNotice";
import type { ConnectReturn } from "./sources/connectReturn";
import { credentialFromRow, type CredentialRow } from "./sources/rows";
import { useSourceCredentials } from "../../modules/connections/useSourceCredentials";
import {
  LocalDeployablesSettingsStore,
  type DeployablesSettings,
  type DeployablesSettingsStore,
} from "./settings";
import { DeployablesSettingsProvider } from "./settingsContext";
import { useSiteHealth } from "./useSiteHealth";
import { siteStateWord } from "./words";
import { shopifyMessage } from "../../modules/connections/shopifyReply";
import { useSites } from "./useSites";
import { usePipelineRuns, usePipelines } from "./pipelines/feeds";
import { pipelineFromRow, runFromRow, type PipelineRow, type RunRow } from "./pipelines/rows";
import { PipelineChannelsSettings } from "./pipelines/ChannelsSettings";
import { RunsSection } from "./pipelines/RunsSection";
import { useConnectFlow } from "./pipelines/connect/useConnectFlow";
import { DeliveryOverview, ChangesSection } from "../pipelines/DeliveryOverview";
import { PipelinesAISettings } from "../pipelines/PipelinesAISettings";
import { ReleasesSection } from "../cluster/releases/ReleasesSection";
import { PipelinesSection } from "../settings/PipelinesSection";
import { holds } from "../../system/roles";

// Deployables: the things this cluster serves, the map of what serves where,
// and the one flow that makes a new one (epic memql#4725, rebuilt by
// memql#4885 around one list and one page).
//
// ===========================================================================
// ONE FEED PER CONCEPT, ONE SELECTION, THREE SURFACES
// ===========================================================================
// The list, the map and the page are readings of collections retained HERE
// rather than inside each section. Two subscriptions over one concept would
// be free to disagree about what the cluster currently holds, and "the list
// and the map disagree" is the one failure this app must not have -- it is a
// picture and a table of the same thing, side by side.
//
// Holding the feeds here also means switching sections costs nothing: each
// collection stays retained for the life of the window rather than re-seeding
// every time somebody looks at the map and comes back.
//
// Sections are the app's own navigation. It never opens a window.

// The disconnected snapshot, generic over the row type so the feeds share one
// rather than casting. A cast here would be the kind of thing that reads as
// harmless and hides a real type change later.
const EMPTY_SNAPSHOT = <T,>(): LiveSnapshot<T> => ({
  rows: [],
  state: "disconnected",
  error: "",
  version: 0,
});

/** The concepts this app owns, for its Logs section: what serves, where it
 *  came from, each attempt to deploy it, and a client's own domain on it --
 *  and each pipeline run over a source, whose steps' output is about the run
 *  and carries no app tag (epic memql#5478). */
const DEPLOYABLES_LOG_CONCEPTS = [
  Concepts.PLATFORM_SITE,
  Concepts.PLATFORM_PACKAGE,
  Concepts.PLATFORM_PACKAGE_DEPLOYMENT,
  Concepts.PLATFORM_CUSTOM_DOMAIN,
  Concepts.PIPELINES_RUN,
] as const;

/** Pipelines > Settings' sibling views, one `LocalTabs` row under its Head. */
const PIPELINE_SETTINGS_TABS = [["infrastructure", "Infrastructure"], ["connections", "Connections"], ["ai", "AI"]] as const;
type PipelineSettingsTab = (typeof PIPELINE_SETTINGS_TABS)[number][0];

export function DeployablesApp(props: Parameters<typeof DeployablesAppContent>[0]) {
  return <SourceConnectionsProvider><DeployablesAppContent {...props} /></SourceConnectionsProvider>;
}

function DeployablesAppContent({
  sectionId: requestedSection,
  surface = "deployables",
  navigation,
  windowVisible = true,
  navigate: requestedNavigate,
  askContext,
  intent,
  consumeIntent,
  reportSetupState,
  store,
}: OsAppProps & { store?: DeployablesSettingsStore; surface?: "deployables" | "pipelines" }) {
  const isPipelines = surface === "pipelines";
  const sectionId = requestedSection === "repositories" ? "sources" : requestedSection;
  const pipelineReach = useAppReach("pipelines");
  const deployablesReach = useAppReach("deployables");
  const [composeOpenRequest, setComposeOpenRequest] = useState<{ packageId: string; app: string; revision: number }>();
  const navigate = (section: string, options?: Parameters<OsAppProps["navigate"]>[1]) => {
    const destination = isPipelines && section === "sources" ? "repositories" : section;
    if (options === undefined) requestedNavigate(destination); else requestedNavigate(destination, options);
  };
  const [addingRepository, setAddingRepository] = useState(false);
  const [settingsTab, setSettingsTab] = useState<PipelineSettingsTab>("infrastructure");
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalDeployablesSettingsStore(), [store]);
  const [settings, setSettings] = useState<DeployablesSettings>(() => settingsStore.load());
  const { access, config } = useSession();
  // An UNRESOLVED session is not an admin. `roleAdmits` refuses an unrankable
  // role, so "" admits only ungated surfaces -- the right answer while access
  // is still resolving, and the safe one if it never resolves.
  const actorRole = access?.role ?? "";
  const viewerUserId = access?.userId ?? "";
  // THE PARTS THIS SESSION HOLDS (epic memql#5289, task memql#5305), read
  // from the effective capability set: `execute` on each of the eleven
  // `app:deployables/<part>` resources. This replaced `canWrite` (rank >=
  // 200, one answer for every act): a person granted `deploy` and not
  // `publish` pushes a build and cannot take a site live, and every control
  // below names the part it needs. Presentation; the engine's capability gate
  // on each construct is the authority, and refuses `capability_not_held`.
  const deployableParts = useDeployableParts();
  const can = isPipelines ? { ...deployableParts,
    connect: holds("execute", "app:pipelines/connect"),
    rerun: holds("execute", "app:pipelines/rerun"),
    cancel: holds("execute", "app:pipelines/cancel"),
    channels: holds("execute", "app:pipelines/channels"),
    sources: holds("execute", "app:pipelines/repositories"),
  } : { ...deployableParts, connect: false, rerun: false, cancel: false, channels: false };
  // The OWNER rung, under the one ladder: a CI-pushed source is a cluster
  // owner's act (memql#4805, D1) and is NOT a part -- the concept's
  // clusterOwner tier is its gate. A client's own domain WAS this rung's and
  // is the `domains` part now.
  const isClusterOwner = roleAdmits(actorRole, { min: "owner" });

  const { source: collection, reseed } = useSites();
  // A SECOND FEED, over a second concept, and deliberately not folded into the
  // one above. `useSites` and `usePackages` read different concepts with
  // different shapes -- one feed cannot carry both -- and each is retained
  // once here for the life of the window so switching sections costs nothing.
  // The rule the app root exists for is one feed PER CONCEPT, not one feed
  // total: what must never happen is two subscriptions over the SAME concept
  // free to disagree about what the cluster holds.
  const { source: packageCollection, reseed: reseedPackages } = usePackages();
  // A THIRD FEED, over a third concept (epic memql#4885): the caller's own
  // source credentials, read once here as CARDS and passed down so the
  // wizard and the Sources list are two
  // readings of one feed rather than two subscriptions free to disagree.
  const { source: credentialCollection, snapshot: credentialSnapshot, reseed: reseedCredentials } = useSourceCredentials();
  // A FOURTH FEED, and the ONE recorded exception to clients/os/README.md's
  // rule that a package's deployment timeline is retained by the page and
  // never by the root. This exception keeps only pending work and review
  // gates, so leaving analysis does not hide it. Terminal runs leave this
  // feed; their full history remains on the package's own page.
  const { source: awaitingCollection, reseed: reseedAwaiting } = usePendingDeployments();
  // THE PIPELINES FEEDS, a fifth and sixth concept (epic memql#5479): the
  // caller's pipelines -- one per source they connected -- and their runs.
  // Retained here for the rule this file exists for: the Runs tab, a source
  // page's Checks and Pipeline fact, and the Overview map's Checks node are
  // readings of ONE collection per concept. Both are read owner-scoped, so
  // they answer the viewer's own pipelines and nobody else's.
  const runsFeed = usePipelineRuns();
  const { source: pipelineCollection, reseed: reseedPipelines } = usePipelines();
  // The connect rail's answers live above the sections (issue memql#5502), so
  // leaving the rail -- for another tab, for the source page -- keeps them.
  const connectFlow = useConnectFlow();

  // PROJECT, then narrow, in one pass. The collection holds RAW wire rows --
  // the fold upserts an event payload as the row type with no projection hook
  // -- so every predicate below has to run on a `siteFromRow` result.
  //
  // `deleted` is dropped HERE as well as by `sitesAll`'s own `isNotDeleted`
  // conjunct, because a soft delete arrives as an UPDATE: the read excludes it
  // and the subscription does not, so without this the row would vanish on a
  // reseed and reappear on the next event.
  const sites = useLiveView<Row, SiteRow>(collection, "sites", (rows) =>
    rows.map(siteFromRow).filter((s) => s.id !== "" && !s.deleted),
  );
  const baseSnapshot = sites?.snapshot ?? EMPTY_SNAPSHOT<SiteRow>();
  const measuredSites = useSiteHealth(baseSnapshot.rows, windowVisible);
  const healthById = new Map(measuredSites.map(site => [site.id, site.health]));
  // The live list and its nested pages consume a source, while the map consumes
  // rows. Project observations into that source too. The key also expires an
  // observation without requiring a site mutation or a new monitor response.
  const healthKey = JSON.stringify(measuredSites.map(site => [site.id, site.health, siteStateWord(site)]));
  const measuredSource = useLiveView(sites, healthKey, rows => rows.map(site => ({...site, health:healthById.get(site.id)})));
  const snapshot = measuredSource?.snapshot ?? EMPTY_SNAPSHOT<SiteRow>();

  // The arrival cue, ONCE, for the whole app. The list renders it through
  // LiveList and the map draws it on its nodes; both read the same reducer over
  // the same snapshot, so a publish announces itself identically in both.
  const ticks = useArrivals(snapshot, (s) => s.id, siteFingerprint);

  const packages = useLiveView<Row, PackageRow>(packageCollection, "packages", (rows) =>
    rows.map(packageFromRow).filter((p) => p.id !== ""),
  );
  const packageSnapshot = packages?.snapshot ?? EMPTY_SNAPSHOT<PackageRow>();

  const credentials = useLiveView<Row, CredentialRow>(credentialCollection, "credentials", (rows) =>
    rows.map(credentialFromRow).filter((c) => c.id !== ""),
  );
  const credentialRows = credentials?.snapshot.rows ?? [];
  const connections = useSourceConnections();
  const githubAccounts = githubAccountsFor(credentialRows, viewerUserId, connections.revokedCredentialIds);
  const setupState = accountSetupState(githubAccounts, credentialSnapshot);
  useEffect(() => { reportSetupState?.(setupState); }, [reportSetupState, setupState]);
  useEffect(() => () => reportSetupState?.("unknown"), [reportSetupState]);
  useEffect(() => { connections.observeCredentials(credentialRows); }, [connections.observeCredentials, credentialRows]);

  // Pending status is held HERE as well as by the feed's `inScope`: the
  // seed and the events both narrow to it, and the projection says so once
  // more so a row this view renders can never be a run that has moved on.
  const parked = useLiveView<Row, DeploymentRow>(awaitingCollection, "awaitingConfirm", (rows) =>
    rows.map(deploymentFromRow).filter((d) => d.id !== "" && PENDING_DEPLOYMENT_STATUSES.has(d.status)),
  );
  const parkedSnapshot = parked?.snapshot ?? EMPTY_SNAPSHOT<DeploymentRow>();

  const runs = useLiveView<Row, RunRow>(runsFeed.source, "pipelineRuns", (rows) =>
    rows.map(runFromRow).filter((r) => r.id !== ""),
  );
  const pipelines = useLiveView<Row, PipelineRow>(pipelineCollection, "pipelines", (rows) =>
    rows.map(pipelineFromRow).filter((p) => p.id !== ""),
  );
  const pipelineRows = pipelines?.snapshot.rows ?? [];
  // Whether both pipelines feeds have answered: before that a source's Checks
  // say nothing at all, because "no pipeline" read before the read landed is a
  // claim, not a fact. A read that FAILED is live with an error and no rows,
  // which is not an answer either: taken as "no pipeline" it would offer
  // Connect pipeline to a source that has one. It is said as the failure.
  const pipelinesError = pipelines?.snapshot.error || runs?.snapshot.error || "";
  const pipelinesSettled = pipelines?.snapshot.state === "live" && runs?.snapshot.state === "live" && pipelinesError === "";
  // "All runs" goes to the Runs tab, so it is offered only to somebody the tab
  // is drawn for (`read app:deployables/runs`).
  const runsReachable = pipelineReach.sections.includes("runs");

  const [openRequest, setOpenRequest] = useState<{ siteId: string; revision: number; detail?: "store"; result?: string } | undefined>();
  // A run to open on the Runs tab -- a check run's details link, a link from
  // Ask -- and a source to open on the Sources tab -- the map's Checks node, a
  // run page's trail. Each is consumed once by revision.
  const [runOpenRequest, setRunOpenRequest] = useState<{ runId: string; revision: number } | undefined>();
  const [releaseOpenRequest, setReleaseOpenRequest] = useState<{ intentId: string; revision: number } | undefined>();
  const [runsFilterRequest, setRunsFilterRequest] = useState<{ pipelineId: string; revision: number } | undefined>();
  const [sourceOpenRequest, setSourceOpenRequest] = useState<{ packageId: string; revision: number; repositoryView?: "settings" } | undefined>();
  function openSource(packageId: string, repositoryView?: "settings") {
    if (!isPipelines) { pipelineReach.open("repositories", { packageId, ...(repositoryView ? { repositoryView } : {}) }); return; }
    setSourceOpenRequest((held) => ({ packageId, repositoryView, revision: (held?.revision ?? 0) + 1 }));
    navigate("sources", { fromContent: true });
  }
  function openRunsFor(pipelineId: string) {
    if (!isPipelines) { pipelineReach.open("runs", { pipelineId }); return; }
    setRunsFilterRequest((held) => ({ pipelineId, revision: (held?.revision ?? 0) + 1 }));
    navigate("runs", { fromContent: true });
  }
  const [selection, setSelection] = useState<MapSelection>(NO_SELECTION);
  const selectedSiteId = selection.siteIds.length === 1 ? (selection.siteIds[0] ?? "") : "";

  function selectSite(siteId: string) {
    setSelection(siteId === "" ? NO_SELECTION : { nodeId: `site:${siteId}`, siteIds: [siteId] });
  }

  function selectNode(node: MapNode) {
    setSelection((held) =>
      held.nodeId === node.id ? NO_SELECTION : { nodeId: node.id, siteIds: [...node.siteIds] },
    );
  }

  function reseedAll() {
    reseed();
    reseedPackages();
    reseedAwaiting();
    runsFeed.reseed();
    reseedPipelines();
  }

  function update(patch: Partial<DeployablesSettings>) {
    // THE FUNCTIONAL FORM, because a patch is applied to the LATEST document
    // rather than to the one this render closed over. With one preference per
    // screen that distinction never showed; the per-group open/shut set made
    // it visible immediately -- two toggles in a tick and the second wrote its
    // own id over the first's, so opening two sources left one open.
    //
    // The save sits inside the updater so it cannot be handed a stale value
    // either. It is idempotent -- the same document written twice is the same
    // document -- which is what makes it safe under StrictMode's double
    // invocation.
    setSettings((held) => {
      const next = { ...held, ...patch, version: 1 as const };
      settingsStore.save(next);
      return next;
    });
  }

  /** Open or shut one source group, as a function of the LATEST open set. */
  function toggleSource(id: string) {
    setSettings((held) => {
      const open = held.expandedSources.includes(id)
        ? held.expandedSources.filter((s) => s !== id)
        : [...held.expandedSources, id];
      const collapsed = held.collapsedSources ?? [];
      const next = { ...held, expandedSources: open, collapsedSources: collapsed.includes(id) ? collapsed.filter(s => s !== id) : [...collapsed, id], version: 1 as const };
      settingsStore.save(next);
      return next;
    });
  }

  // THE ANSWER FROM A GITHUB CONNECT, delivered as a window intent (epic
  // memql#4915). It is held here and RENDERED BY THE SOURCES GROUP rather
  // than by this component, because the result belongs on the surface that
  // asked for the connection -- a page of its own would be a toast with more
  // pixels. Consumed by id, so acting on a stale render can never eat a
  // newer instruction, and an unrecognised payload is consumed and ignored
  // rather than left standing to re-fire on every render.
  const [connectionProvider, setConnectionProvider] = useState<"github" | "shopify">("github");
  const [connectionsIntent, setConnectionsIntent] = useState<OsAppProps["intent"]>();
  const [shopifyError, setShopifyError] = useState("");
  const [connectResult, setConnectResult] = useState<ConnectReturn | null>(null);
  useEffect(() => {
    if (!intent) return;
    const requestedSite = intent.payload.siteId;
    if (typeof requestedSite === "string" && requestedSite) {
      setOpenRequest(held => ({ siteId: requestedSite, revision: (held?.revision ?? 0) + 1 }));
      navigate("deployables");
    }
    const composeSource = intent.payload.composeSourceId, composeApp = intent.payload.composeApp;
    if (typeof composeSource === "string" && composeSource && typeof composeApp === "string" && composeApp) {
      setComposeOpenRequest(held => ({ packageId: composeSource, app: composeApp, revision: (held?.revision ?? 0) + 1 }));
      navigate("deployables");
    }
    const requestedPipeline = intent.payload.pipelineId;
    if (typeof requestedPipeline === "string" && requestedPipeline) openRunsFor(requestedPipeline);
    const requestedRun = intent.payload.runId;
    if (typeof requestedRun === "string" && requestedRun) {
      if (!isPipelines) pipelineReach.open("runs", { runId: requestedRun });
      else { setRunOpenRequest(held => ({ runId: requestedRun, revision: (held?.revision ?? 0) + 1 })); navigate("runs"); }
    }
    const requestedRelease = intent.payload.releaseIntentId;
    if (typeof requestedRelease === "string" && requestedRelease) {
      if (!isPipelines) pipelineReach.open("releases", { releaseIntentId: requestedRelease });
      else { setReleaseOpenRequest(held => ({ intentId: requestedRelease, revision: (held?.revision ?? 0) + 1 })); navigate("releases"); }
    }
    // A source named by a link: its page, on the Sources tab -- the same
    // request the map's Checks node and a run page's trail make.
    const requestedSource = intent.payload.packageId;
    if (typeof requestedSource === "string" && requestedSource && typeof requestedRun !== "string") {
      openSource(requestedSource, intent.payload.repositoryView === "settings" ? "settings" : undefined);
    }
    if (intent.payload.provider === "shopify") { setConnectionProvider("shopify"); setConnectionsIntent(intent); }
    const shopify = intent.payload["shopify"];
    if (shopify && typeof shopify === "object" && "siteId" in shopify && typeof shopify.siteId === "string" && shopify.siteId) {
      setShopifyError("");
      setOpenRequest(held => ({ siteId: shopify.siteId as string, revision: (held?.revision ?? 0) + 1, detail: "store", result: "reason" in shopify && typeof shopify.reason === "string" ? shopify.reason : "" }));
      reseedAll();
    }
    if (sectionId !== "settings" && shopify && typeof shopify === "object" && (!("siteId" in shopify) || !shopify.siteId)) {
      setShopifyError(shopifyMessage("reason" in shopify && typeof shopify.reason === "string" ? shopify.reason : "connect_state_invalid"));
    }
    const carried = intent.payload["connect"];
    if (carried !== null && typeof carried === "object") {
      const answer = carried as Partial<ConnectReturn>;
      setConnectResult({
        reason: typeof answer.reason === "string" ? answer.reason : "",
        section: typeof answer.section === "string" ? answer.section : sectionId,
        ...(typeof answer.credentialId === "string" ? { credentialId: answer.credentialId } : {}),
        ...(typeof answer.flowId === "string" ? { flowId: answer.flowId } : {}),
      });
    }
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent, sectionId]);

  const settingsContent = (
    <ConnectionsPanel heading={!isPipelines} appId={isPipelines ? "pipelines" : "deployables"} initialProvider={connectionProvider} intent={connectionsIntent} consumeIntent={() => setConnectionsIntent(undefined)}
      connectResult={connectResult?.section === "settings" ? connectResult : null}
      additionalSections={can.channels ? [{ id: "channels", name: "Channels", render: (active, header) => <PipelineChannelsSettings header={header} active={active} pipelines={pipelineRows} pipelinesReady={pipelines?.snapshot.state === "live" && !pipelines.snapshot.error} refreshPipelines={reseedPipelines} /> }] : []} />
  );
  // The app's slice of the cluster's logs (epic memql#4895). It survived the
  // compose restructure while Sites, Packages and Actions did not, and the
  // difference is whose section it is: those three were this app's own reading
  // of its subject and became one, and this is a shell convention every app
  // carries at the log store's own admin floor.
  const logsContent = (
      <AppLogsSection
        app={surface}
        subjectConcepts={DEPLOYABLES_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  const deployablesContent = (
      // THE ONE BRANCH THAT NEEDS IT. The traffic window is read four levels
      // down (Live stop -> traffic panel) and the open-source set is read by
      // the list itself; both are written here. The settings section takes
      // `settings` and `update` directly because it is one level away, and the
      // map has neither.
      <DeployablesSettingsProvider value={{ settings, update, toggleSource }}>
        <DeployablesSection
          active={sectionId === "deployables"}
          navigation={navigation}
          openRequest={openRequest}
          composeOpenRequest={composeOpenRequest}
          connectResult={connectResult?.section === "deployables" ? connectResult : null}
          pipelines={pipelines}
          runs={runs}
          pipelinesSettled={pipelinesSettled}
          pipelinesError={pipelinesError}
          connectFlow={isPipelines ? connectFlow : undefined}
          onOpenRepository={isPipelines ? undefined : pipelineReach.sections.includes("repositories") ? openSource : null}
          onOpenRuns={runsReachable ? openRunsFor : undefined}
          sites={measuredSource}
          packages={packages}
          parked={parked}
          feedError={snapshot.error || packageSnapshot.error || parkedSnapshot.error}
          selectedSiteId={selectedSiteId}
          onSelectSite={selectSite}
          viewerUserId={viewerUserId}
          can={can}
          isClusterOwner={isClusterOwner}
          clusterDomain={config.domain}
          credentials={credentialRows}
          credentialFeed={{ state: credentialSnapshot.state, error: credentialSnapshot.error, retry: reseedCredentials }}
          onAsk={askContext}
          onReseed={reseedAll}
          onSettings={() => navigate("settings", { fromContent: true })}
        />
      </DeployablesSettingsProvider>
    );
  // THE SAME SECTION ON ITS OTHER ROOT. Every page either list opens is the
  // same page, so this is a second INSTANCE rather than a second component --
  // with its own view state, so drilling into a source here does not move the
  // Deployables tab off whatever it was showing. It takes no open request and
  // no connect return: those are addressed to Deployables, which is where the
  // map sends people and where GitHub sends them back.
  const sourcesContent = (
      <DeployablesSettingsProvider value={{ settings, update, toggleSource }}>
        <DeployablesSection
          root="sources"
          sourceLabel={isPipelines ? "Repositories" : "Sources"}
          onAddRepository={isPipelines && can.sources ? () => setAddingRepository(true) : undefined}
          active={sectionId === "sources"}
          navigation={navigation}
          connectResult={null}
          sourceOpenRequest={sourceOpenRequest}
          onOpenDeployable={isPipelines ? id => deployablesReach.open("deployables", { siteId: id }) : undefined}
          onComposeDeployable={isPipelines ? (packageId, app) => deployablesReach.open("deployables", { composeSourceId: packageId, composeApp: app }) : undefined}
          pipelines={pipelines}
          runs={runs}
          pipelinesSettled={pipelinesSettled}
          pipelinesError={pipelinesError}
          connectFlow={connectFlow}
          onOpenRuns={runsReachable ? openRunsFor : undefined}
          sites={measuredSource}
          packages={packages}
          parked={parked}
          feedError={snapshot.error || packageSnapshot.error || parkedSnapshot.error}
          selectedSiteId=""
          onSelectSite={() => {}}
          viewerUserId={viewerUserId}
          can={can}
          isClusterOwner={isClusterOwner}
          clusterDomain={config.domain}
          credentials={credentialRows}
          credentialFeed={{ state: credentialSnapshot.state, error: credentialSnapshot.error, retry: reseedCredentials }}
          onAsk={askContext}
          onReseed={reseedAll}
          onSettings={() => navigate("settings", { fromContent: true })}
        />
      </DeployablesSettingsProvider>
    );
  const mapContent = (
    <MapSection
      sites={snapshot.rows}
      snapshot={snapshot}
      ticks={ticks}
      selection={selection}
      onSelectNode={selectNode}
      // THE MAP POINTS, THE SECTION OWNS THE PAGE (rule 11). Choosing a
      // deployable on the map selects it -- the two surfaces share ONE
      // selection -- and navigates to the section that owns its page, which
      // opens on that selection.
      onOpenDeployable={(siteId) => {
        selectSite(siteId);
        setOpenRequest(held => ({ siteId, revision: (held?.revision ?? 0) + 1 }));
        navigate("deployables", { fromContent: true });
      }}
      onBrowse={() => navigate("deployables", { fromContent: true })}
      pipelines={pipelineRows}
      runs={runs?.snapshot.rows ?? []}
      onOpenSource={openSource}
    />
  );
  // RUNS (epic memql#5479): every run of every source this person connected.
  // Its own section and its own view state, so opening a run here does not
  // move Sources off whatever it was showing.
  const runsContent = (
    <RunsSection
      active={sectionId === "runs"}
      navigation={navigation}
      runs={runs}
      feed={runsFeed}
      pipelines={pipelineRows}
      packages={packageSnapshot.rows}
      openRequest={runOpenRequest}
      filterRequest={runsFilterRequest}
      can={can}
      onOpenSource={openSource}
      onOpenSources={() => navigate("sources", { fromContent: true })}
    />
  );
  const openRun = (runId: string) => { setRunOpenRequest(held => ({ runId, revision: (held?.revision ?? 0) + 1 })); navigate("runs", { fromContent: true }); };
  if (isPipelines) return <ActivePane active={windowVisible}>
    <RetainedSection active={sectionId === "overview"}><DeliveryOverview pipelines={pipelineRows} runs={runs?.snapshot.rows ?? []} ready={pipelinesSettled} error={pipelinesError} onOpenRun={openRun} onRepositories={pipelineReach.sections.includes("repositories") ? () => navigate("sources", { fromContent: true }) : undefined} onRuns={runsReachable ? () => navigate("runs", { fromContent: true }) : undefined} onReleases={pipelineReach.sections.includes("releases") ? () => navigate("releases", { fromContent: true }) : undefined} /></RetainedSection>
    <RetainedSection active={sectionId === "sources"}><ConnectReturnNotice result={connectResult?.section === "sources" || connectResult?.section === "repositories" ? connectResult : null} />{addingRepository ? <AddRepository back={() => setAddingRepository(false)} onAdded={id => { setAddingRepository(false); reseedPackages(); openSource(id); }} onSettings={() => { setSettingsTab("connections"); navigate("settings", { fromContent: true }); }} /> : sourcesContent}</RetainedSection>
    <RetainedSection active={sectionId === "changes"}><ChangesSection runs={runs?.snapshot.rows ?? []} ready={pipelinesSettled} error={pipelinesError} onOpenRun={openRun} /></RetainedSection>
    <RetainedSection active={sectionId === "runs"}>{runsContent}</RetainedSection>
    <RetainedSection active={sectionId === "releases"}><ReleasesSection visible={windowVisible && sectionId === "releases"} openRequest={releaseOpenRequest} runs={runs?.snapshot.rows ?? []} runsFresh={runs?.snapshot.state === "live" && !runs.snapshot.error} onOpenRun={runsReachable ? openRun : undefined} onOpenRepository={pipelineReach.sections.includes("repositories") ? openSource : undefined} /></RetainedSection>
    <RetainedSection active={sectionId === "logs"}>{logsContent}</RetainedSection>
    <RetainedSection active={sectionId === "settings"}><div className="os-app-stack"><Head title="Settings" /><LocalTabs label="Pipeline settings" value={settingsTab} onChange={setSettingsTab} options={PIPELINE_SETTINGS_TABS} />{settingsTab === "infrastructure" ? <PipelinesSection heading={false} intent={intent} consumeIntent={consumeIntent} /> : settingsTab === "ai" ? <PipelinesAISettings /> : settingsContent}</div></RetainedSection>
  </ActivePane>;
  return <ActivePane active={windowVisible}>
    <RetainedSection active={sectionId === "settings"}>{settingsContent}</RetainedSection>
    <RetainedSection active={sectionId === "logs"}>{logsContent}</RetainedSection>
    <RetainedSection active={sectionId === "deployables"}>{shopifyError ? <Notice tone="warn" sentence={shopifyError} /> : null}{deployablesContent}</RetainedSection>
    <RetainedSection active={sectionId === "sources"}>
      <ConnectReturnNotice result={connectResult?.section === "sources" ? connectResult : null} />
      {sourcesContent}
    </RetainedSection>
    <RetainedSection active={sectionId === "runs"}>{runsContent}</RetainedSection>
    <RetainedSection active={!["settings", "logs", "deployables", "sources", "runs"].includes(sectionId)}>{mapContent}</RetainedSection>
  </ActivePane>;
}

/** Keep visited panes alive across tabs; hidden panes have no keyboard targets. */
function RetainedSection({ active, children }: { active: boolean; children: React.ReactNode }) {
  const [visited, setVisited] = useState(active);
  useEffect(() => { if (active) setVisited(true); }, [active]);
  return active || visited ? <ActivePane active={active}><div hidden={!active} inert={!active} style={{ display: active ? "contents" : "none" }}>{children}</div></ActivePane> : null;
}
