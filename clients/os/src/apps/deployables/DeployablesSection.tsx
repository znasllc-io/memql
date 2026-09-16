import type { ConnectReturn } from "./sources/connectReturn";
import { healthExplanation } from "./health";
import { AddButton } from "../../kit/AddButton";
import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { ActivePane } from "./paneActivity";
import { Archive, ArrowUpCircle, ChevronRight, GitBranch, Globe, ShoppingBag } from "lucide-react";

import type { OsAppProps } from "../../system/registry";
import { useDeployablesSettings } from "./settingsContext";
import { siteStateWord, statusFacetLabel } from "./words";
import {
  Button,
  EmptyState,
  RefreshButton,
  Caption,
  Head,
  LiveList,
  Notice,
  Refine,
  Select,
  useNow,
  useThreeFeedView,
  type RefineChip,
} from "../../kit";
import { formatFreshness } from "../../kit/format";
import type { LiveView } from "../../live/liveView";
import type { ArrivalKind } from "../../live/arrival";
import { NO_ACCOUNT_LABEL } from "../accounts/AccountPicker";
import { accountIsArchived, accountName, accountNameFrom, type AccountRow } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import { deployedByLabel, deployerOf, usePeopleNames } from "./people";
import {
  ACCOUNT_ANY,
  ACCOUNT_NONE,
  DEFAULT_LIST_FILTER,
  SOURCE_FACETS,
  filterIsNarrowing,
  foldDeployables,
  groupFingerprint,
  listViewKey,
  newestParkedRun,
  SECTION_LABELS,
  type DeployableListGroup,
  type DeployableListRow,
  type ListFilter,
} from "./list";
import { runIsScopedToApp, sourceLabel, type DeploymentRow, type PackageRow } from "./packages/rows";
import { ComposePage } from "./page/ComposePage";
import type { PartsHeld } from "./parts";
import { DeployablePage } from "./page/DeployablePage";
import { HistoryView } from "./page/HistoryView";
import { SourceView } from "./page/SourceView";
import { SITE_STATUSES, type SiteRow } from "./rows";
import type { ListDensity } from "./settings";
import { LIST_TRAFFIC_WINDOW, type TrafficSummary } from "./traffic";
import { useSiteTraffic } from "./useSiteTraffic";
import type { CredentialFeedStatus, CredentialRow } from "./sources/rows";
import { DEPLOYABLE_KINDS, kindLabel } from "./targets";

// The Deployables section (epic memql#4937, design section A): FOUR SIBLING
// VIEWS, one at a time, one Head each.
//
// ===========================================================================
// A LIST AND ITS DETAIL NEVER SHARE A SCROLL COLUMN (DESIGN.md rule 11)
// ===========================================================================
// This section used to hold `selectedSiteId` and render <DeployablePage> as a
// SIBLING of <LiveList> -- so selecting a row appended a whole page beneath
// the list it was selected from. That is where the second `os-head` and most
// of the measured 5,069px came from.
//
// Every other app in this shell already did better: Bin puts its detail beside
// the list in its own scroller (`.os-bin-list { overflow-y: auto }`), and
// Campaigns, Users, Accounts and Training do variants of the same. Deployables
// was the one app that did not adopt the pattern the shell already had.
//
//     List --select an app--> Deployable  --history--> History
//       |                          |
//       |   --select a source--> Source
//       |
//       '-- New deployable -----> Compose
//
// Compose already worked this way; everything else joins it.
//
// ===========================================================================
// ONE LIST LANGUAGE
// ===========================================================================
// Every line is a ROW. A source is a real row that opens its own view, and the
// apps it produced indent beneath it -- where the source used to be a `div` of
// caption text with two chips, wedged between clickable rows, reachable by no
// keyboard and announced as nothing. That is the "list inside a list" the
// owner reported.
//
// The five-dot rail moves into a FIXED TRAILING COLUMN so the marks land at
// the same x on every row and can be scanned down the list; it used to follow
// a variable-length hostname and land somewhere different on each.

/** Which view the section is showing. One at a time, one Head each. */
type DeployablesView =
  | { kind: "list" }
  | { kind: "deployable"; siteId: string; from?: string }
  | { kind: "source"; packageId: string; fromSite?: string }
  | { kind: "history"; packageId: string; siteId?: string; returnTo?: DeployablesView }
  | { kind: "compose"; parkedPackageId?: string; only?: string; fromSource?: string; connectResult?: ConnectReturn };

/** What the list says about the row that just left it. */
type Gone = { name: string; what: "deleted" | "deactivated" } | null;

export function DeployablesSection({
  active = true, navigation, openRequest, connectResult,
  sites,
  packages,
  parked,
  feedError,
  density,
  selectedSiteId,
  onSelectSite,
  viewerUserId,
  can,
  isClusterOwner,
  clusterDomain,
  credentials,
  credentialFeed,
  onAsk,
  onReseed,
}: {
  active?: boolean;
  navigation?: OsAppProps["navigation"];
  openRequest?: { siteId: string; revision: number };
  connectResult?: ConnectReturn | null;
  sites: LiveView<SiteRow> | null;
  packages: LiveView<PackageRow> | null;
  parked: LiveView<DeploymentRow> | null;
  feedError: string;
  density: ListDensity;
  selectedSiteId: string;
  onSelectSite: (siteId: string) => void;
  viewerUserId: string;
  /** The parts this session holds (epic memql#5289). */
  can: PartsHeld;
  isClusterOwner: boolean;
  clusterDomain: string;
  credentials: readonly CredentialRow[];
  credentialFeed?: CredentialFeedStatus;
  onAsk?: (tag: string) => void;
  onReseed: () => void;
}) {
  const [filter, setFilter] = useState<ListFilter>(DEFAULT_LIST_FILTER);
  const [showArchived, setShowArchived] = useState(false);
  // THE VIEW INITIALISES FROM THE SELECTION, so arriving from the Map -- which
  // navigates here with a site chosen -- lands on that deployable rather than
  // on the list with an invisible selection.
  const [view, holdView] = useState<DeployablesView>(() =>
    selectedSiteId === "" ? { kind: "list" } : { kind: "deployable", siteId: selectedSiteId },
  );
  // A peer tab shows the landing without destroying an unfinished nested pane.
  // Reopening New resumes its retained draft; contextual Back restores the pane.
  const [landing, setLanding] = useState(false);
  const lastNavigation = useRef<number | undefined>(undefined);
  const lastOpenRequest = useRef<number | undefined>(undefined);
  function setView(next: DeployablesView) { setLanding(false); holdView(next); }
  useLayoutEffect(() => {
    if (!active) return;
    if (openRequest && openRequest.revision !== lastOpenRequest.current) {
      lastOpenRequest.current = openRequest.revision;
      setView({ kind: "deployable", siteId: openRequest.siteId });
      lastNavigation.current = navigation?.revision;
      return;
    }
    if (navigation && navigation.revision !== lastNavigation.current) {
      lastNavigation.current = navigation.revision;
      setLanding(navigation.origin === "peer");
    }
  }, [active, navigation?.revision, navigation?.origin, openRequest?.revision, openRequest?.siteId]);
  // OAuth is a full-page return. Resume the repository step once; the
  // live credential feed decides whether this account actually connected.
  useLayoutEffect(() => {
    if (connectResult && can.deploy) setView({ kind: "compose", connectResult });
  }, [connectResult, can.deploy]);
  // WHAT WAS JUST DELETED, so the list can say what happened to it. The name
  // is free the instant the row is stamped; the domains come down on the
  // reconciliation sweep's own schedule, and this says so rather than implying
  // the whole thing is finished.
  const [justGone, setJustGone] = useState<Gone>(null);
  const accounts = useAccountOptions();
  // THE ROSTER, once, for "deployed by" (epic memql#5289, task memql#5306):
  // the rows already carry the deployer's id, and this is the one lookup that
  // turns it into a name -- refusal-tolerant, so a reader's rows simply keep
  // their ownership chip.
  const nameOf = usePeopleNames();

  const siteRows = sites?.snapshot.rows ?? [];
  const packageRows = packages?.snapshot.rows ?? [];
  const parkedRows = parked?.snapshot.rows ?? [];

  const viewKey = listViewKey(filter, showArchived);
  const list = useThreeFeedView<SiteRow, PackageRow, DeploymentRow, DeployableListGroup>(
    sites,
    packages,
    parked,
    viewKey,
    (s, p, r) => foldDeployables(s, p, r, filter, showArchived),
  );
  const groups = list?.snapshot.rows ?? [];
  const listedCount = groups.reduce((n, g) => n + g.rows.length, 0);

  const { figures } = useSiteTraffic(
    groups.flatMap((g) =>
      g.rows.map((r) => r.site).filter((s): s is SiteRow => s !== null && !s.systemOwned).map((s) => s.id),
    ),
    LIST_TRAFFIC_WINDOW,
  );

  const archivedCount = useMemo(
    () =>
      foldDeployables(siteRows, packageRows, parkedRows, DEFAULT_LIST_FILTER, true).reduce(
        (n, g) => n + g.rows.length,
        0,
      ),
    [siteRows, packageRows, parkedRows],
  );

  function patch(next: Partial<ListFilter>) {
    setFilter((held) => ({ ...held, ...next }));
  }

  function flipArchived() {
    setShowArchived((v) => !v);
    patch({ status: "" });
  }

  function openSite(siteId: string) {
    onSelectSite(siteId);
    setJustGone(null);
    setView({ kind: "deployable", siteId, ...(!landing && view.kind === "source" ? { from: view.packageId } : {}) });
  }

  function backToList() {
    setView({ kind: "list" });
  }

  // ---- the views -----------------------------------------------------------

  // OPENING AN APP THE SOURCE ONLY DECLARES (2026-09-05 design, D5 and D6).
  //
  // A CLICK NEVER ACTS. It used to: a row the owner had turned off was turned
  // back ON by the click that looked like opening it, and a row nobody had
  // turned off started an analysis run from the list. Both now open the
  // compose flow scoped to that app -- an inactive one with a notice saying
  // so and Activate on the bar, a merely-declared one with Analyze -- and
  // nothing is written until the bar's act is pressed. The flow reads the
  // source and the off-list off the root feeds, so it knows which it is.
  function openDeclared(packageId: string, app: string) {
    setView({ kind: "compose", parkedPackageId: packageId, only: app, ...(!landing && view.kind === "source" ? { fromSource: packageId } : {}) });
  }

  return <>
    <ActivePane active={!landing || view.kind === "list"}>
      <div data-deployable-view hidden={landing && view.kind !== "list"} inert={landing && view.kind !== "list"} style={{ display: landing && view.kind !== "list" ? "none" : "contents" }}>{renderCurrentView()}</div>
    </ActivePane>
    {landing && view.kind !== "list" ? renderList() : null}
  </>;

  function renderCurrentView() {
    if (view.kind === "compose") {
      const parkedFor =
        view.parkedPackageId === undefined ? null : newestParked(packageRows, parkedRows, view.parkedPackageId);
      return (
        <ComposePage
          clusterDomain={clusterDomain}
          connectResult={view.connectResult}
          can={can}
          isClusterOwner={isClusterOwner}
          viewerUserId={viewerUserId}
          credentials={credentials}
          credentialFeed={credentialFeed}
          backLabel={view.fromSource ? "Source" : "Deployables"}
          onBack={() => view.fromSource ? setView({ kind: "source", packageId: view.fromSource }) : backToList()}
          onAsk={onAsk}
          parked={parkedFor ?? undefined}
          source={
            view.parkedPackageId === undefined
              ? undefined
              : (packageRows.find((p) => p.id === view.parkedPackageId) ?? undefined)
          }
          only={view.only}
          packages={packageRows}
          placed={
            parkedFor === null
              ? []
              : siteRows.filter((s) => s.packageId === parkedFor.pkg.id).map((s) => s.packageDeployableName)
          }
        />
      );
    }

    if (view.kind === "source" || view.kind === "history") {
      const pkg = packageRows.find((p) => p.id === view.packageId) ?? null;
      // The source left the feed while this was open -- archived from another
      // window, say. The list is what the person sees, rather than a page about
      // a thing that is gone.
      if (pkg === null) return renderList();
      const apps = siteRows.filter((s) => s.packageId === pkg.id);
      if (view.kind === "history") {
        return <HistoryView pkg={pkg} can={can} app={siteRows.find(s => s.id === view.siteId)}
          backLabel={view.returnTo?.kind === "history" ? "App history" : undefined}
          onOpenSourceHistory={() => setView({ kind: "history", packageId: pkg.id, returnTo: view })}
          onBack={() => setView(view.returnTo ?? (view.siteId ? { kind: "deployable", siteId: view.siteId } : { kind: "source", packageId: pkg.id }))} />;
      }
      return (
        <SourceView
          pkg={pkg}
          apps={apps}
          credentials={credentials}
          can={can}
          backLabel={view.fromSite ? (siteRows.find(s => s.id === view.fromSite)?.title || "Deployable") : "Deployables"}
          onBack={() => view.fromSite ? setView({ kind: "deployable", siteId: view.fromSite }) : backToList()}
          onOpenHistory={() => setView({ kind: "history", packageId: pkg.id, returnTo: view })}
          onOpenApp={openSite}
          onOpenDeclared={(app) => openDeclared(pkg.id, app)}
          onAsk={onAsk}
          attempts={parkedRows.filter((d) => d.packageId === pkg.id).length}
          deployedBy={deployedByLabel(
            deployerOf(parkedRows.find((d) => d.packageId === pkg.id) ?? null, null, pkg),
            viewerUserId,
            nameOf,
          )}
        />
      );
    }

    if (view.kind === "deployable") {
      const site = siteRows.find((s) => s.id === view.siteId) ?? null;
      if (site === null) return renderList();
      const pkg = site.packageId === "" ? null : (packageRows.find((p) => p.id === site.packageId) ?? null);
      return (
        <DeployablePage
          key={site.id}
          site={site}
          pkg={pkg}
          credentials={credentials}
          viewerUserId={viewerUserId}
          nameOf={nameOf}
          can={can}
          clusterDomain={clusterDomain}
          onAsk={onAsk}
          onBack={() => view.from ? setView({ kind: "source", packageId: view.from }) : backToList()}
          backLabel={view.from ? "Source" : "Deployables"}
          onOpenSource={(packageId) => setView({ kind: "source", packageId, fromSite: site.id })}
          onOpenHistory={(packageId) => setView({ kind: "history", packageId, siteId: site.id, returnTo: view })}
          onDeleted={(goneId, what) => {
            const gone = siteRows.find((s) => s.id === goneId);
            setJustGone(gone === undefined ? null : { name: gone.hostname, what });
            onSelectSite("");
            setView({ kind: "list" });
          }}
        />
      );
    }

    return renderList();
  }

  // ---- the list ------------------------------------------------------------

  function renderList() {
    const chips: RefineChip[] = [];
    if (filter.kind !== "")
      chips.push({ id: "kind", label: kindLabel(filter.kind), onRemove: () => patch({ kind: "" }) });
    if (filter.status !== "") chips.push({ id: "status", label: filter.status, onRemove: () => patch({ status: "" }) });
    if (filter.accountId !== ACCOUNT_ANY) {
      chips.push({
        id: "client",
        label: filter.accountId === ACCOUNT_NONE ? NO_ACCOUNT_LABEL : accountNameFrom(accounts, filter.accountId),
        onRemove: () => patch({ accountId: ACCOUNT_ANY }),
      });
    }
    if (filter.source !== "") {
      chips.push({
        id: "source",
        label: SOURCE_FACETS.find((s) => s.value === filter.source)?.label ?? filter.source,
        onRemove: () => patch({ source: "" }),
      });
    }

    const emptyText = showArchived
      ? "No archived deployables."
      : filterIsNarrowing(filter)
        ? "No matching deployables. Clear the filters to see everything."
        : can.deploy
          ? "No deployables yet. Add a repository, upload built files or connect your CI."
          : "No deployables are available to this account.";

    const filtered = filterIsNarrowing(filter);
    return (
      <div className="os-app-stack os-deployables-list deployable-overview" data-density={density}>
        <Head title="Deployables" meta={!feedError && list?.snapshot.state === "live" ? listedCount : undefined}>
          <Refine iconOnly
            search={filter.search}
            onSearch={(search) => patch({ search })}
            chips={chips}
            label="Refine deployables"
          >
            <Select id="deployables-kind" label="Kind" value={filter.kind} onChange={(kind) => patch({ kind })}>
              <option value="">Any kind</option>
              {DEPLOYABLE_KINDS.map((kind) => (
                <option key={kind.value} value={kind.value}>
                  {kind.label}
                </option>
              ))}
            </Select>
            {showArchived ? null : (
              <Select
                id="deployables-status"
                label="Status"
                value={filter.status}
                onChange={(status) => patch({ status })}
              >
                <option value="">Any status</option>
                {SITE_STATUSES.filter((s) => s !== "archived").map((status) => (
                  <option key={status} value={status}>
                    {statusFacetLabel(status)}
                  </option>
                ))}
              </Select>
            )}
            <Select
              id="deployables-client"
              label="Client"
              value={filter.accountId}
              onChange={(accountId) => patch({ accountId })}
            >
              <option value={ACCOUNT_ANY}>Any client</option>
              <option value={ACCOUNT_NONE}>{NO_ACCOUNT_LABEL}</option>
              {accounts.map((account) => (
                <option key={account.id} value={account.id}>
                  {accountIsArchived(account) ? `${accountName(account)} (archived)` : accountName(account)}
                </option>
              ))}
            </Select>
            <Select id="deployables-source" label="Source" value={filter.source} onChange={(source) => patch({ source })}>
              <option value="">Any source</option>
              {SOURCE_FACETS.map((source) => (
                <option key={source.value} value={source.value}>
                  {source.label}
                </option>
              ))}
            </Select>
          </Refine>
          {/* "New", not "New deployable": the Head directly beside it reads
              Deployables, so the noun was said twice on one line. The
              ACCESSIBLE name keeps the full phrase, because a screen reader
              reaching this button out of context has no Head to read it
              against. */}
          {/* `deploy`, because composing ENDS in a deploy: a person holding
              only `sources` has nothing to reach here that is theirs. */}
          {can.deploy ? (
            <AddButton label="New deployable" className="deployable-new" onClick={() => setView({ kind: "compose" })} />
          ) : null}
        </Head>

        {/* WHAT HAPPENED TO THE THING THAT IS NO LONGER HERE. The name is free
            the instant the row is stamped; the certificate and route come down
            on the reconciliation sweep's own schedule, and saying both is the
            difference between "it worked" and "it worked, and here is the part
            that is still happening". */}
        {justGone === null ? null : (
          <Notice
            tone="info"
            sentence={
              justGone.what === "deactivated"
                ? `${justGone.name} is released, and its app is inactive under its source.`
                : `${justGone.name} is deleted, and its name is free to use again.`
            }
            next={
              justGone.what === "deactivated"
                ? "Any client domain it served comes down on the next reconciliation sweep, within about two minutes. Activate the app from its row to deploy it again at a fresh address."
                : "Any domains it served come down on the next reconciliation sweep, within about two minutes. Its record stays in this cluster's history."
            }
          >
            <Button tone="quiet" onClick={() => setJustGone(null)}>
              Got it
            </Button>
          </Notice>
        )}

        {feedError ? (
          <Notice
            tone="error"
            sentence="Deployables could not be loaded."
            detail={feedError}
            next="Check your connection and try again."
          >
            <RefreshButton label="Reload deployables" onClick={onReseed} />
          </Notice>
        ) : null}

        {groups.length === 0 && (feedError || list?.snapshot.state !== "live") ? (feedError ? null : <div data-os-livelist data-state={list?.snapshot.state ?? "disconnected"}>
          <EmptyState icon={Globe} title={list?.snapshot.state === "seeding" ? "Loading from the cluster" : "Not connected to the cluster"}>
            {list?.snapshot.state === "seeding" ? "Your apps and sources will appear here." : "Your apps will appear when the connection returns."}
          </EmptyState>
        </div>) : <LiveList<DeployableListGroup>
          source={list}
          rowId={(g) => g.id}
          fingerprint={groupFingerprint}
          label="Deployables in this cluster"
          emptyText={emptyText}
          emptyContent={<EmptyState icon={Globe} title={showArchived ? "No archived deployables" : filtered ? "No matching deployables" : "No deployables yet"}
            action={filtered ? <Button onClick={() => setFilter(DEFAULT_LIST_FILTER)}>Clear filters</Button> : undefined}>
            {showArchived ? "Archived apps will appear here. Restore one to bring it back offline." : filtered ? "Try a different search or clear the filters." : can.deploy ? "Use New deployable to add a repository, built files or a CI pipeline." : "Apps shared with your account will appear here."}
          </EmptyState>}
          renderRow={(group, tick) => (
            <GroupLine
              group={group}
              figures={figures}
              tick={tick}
              accounts={accounts}
          deployedByOf={(row) => deployedByLabel(row.deployedBy, viewerUserId, nameOf)}
              selectedSiteId={selectedSiteId}
              onOpenSite={openSite}
              onOpenSource={(packageId) => setView({ kind: "source", packageId })}
              onOpenParked={(packageId, only) =>
                setView({ kind: "compose", parkedPackageId: packageId, ...(only === "" ? {} : { only }) })
              }
              onOpenDeclared={openDeclared}
            />
          )}
        />}

        {archivedCount > 0 || showArchived ? (
          <div className="os-archive-toggle">
            <button type="button" className="os-sort" aria-expanded={showArchived} onClick={flipArchived}>
              <Archive size={12} aria-hidden />{" "}
              {showArchived ? "Show active deployables" : `Show archived (${archivedCount})`}
            </button>
            <Caption>
              {showArchived
                ? "Restored apps return offline. Open an app to restore it."
                : "Restore archived apps here."}
            </Caption>
          </div>
        ) : null}
      </div>
    );
  }
}

function newestParked(
  packages: readonly PackageRow[],
  parked: readonly DeploymentRow[],
  packageId: string,
): { pkg: PackageRow; run: DeploymentRow } | null {
  const pkg = packages.find((p) => p.id === packageId);
  if (pkg === undefined) return null;
  const run = newestParkedRun(parked, packageId);
  return run === null ? null : { pkg, run };
}

// ---------------------------------------------------------------------------
// One entry: a source ROW with its apps, or a hand-made site on its own
// ---------------------------------------------------------------------------

function GroupLine({
  group,
  tick,
  accounts,
  deployedByOf,
  selectedSiteId,
  onOpenSite,
  onOpenSource,
  onOpenParked,
  onOpenDeclared,
  figures,
}: {
  group: DeployableListGroup;
  tick: ArrivalKind | null;
  accounts: AccountRow[];
  /** "you", a name, or "" for a row's deployer (epic memql#5289, task memql#5306). */
  deployedByOf: (row: DeployableListRow) => string;
  selectedSiteId: string;
  onOpenSite: (siteId: string) => void;
  onOpenSource: (packageId: string) => void;
  /** A parked run's row: reopen its gate, scoped to the app when the run is. */
  onOpenParked: (packageId: string, only: string) => void;
  /** A declared app with no site, inactive or not: open the compose flow for it. */
  onOpenDeclared: (packageId: string, app: string) => void;
  figures: Map<string, TrafficSummary>;
}) {
  // Show source relationships until the person explicitly collapses a group.
  const { settings, toggleSource } = useDeployablesSettings();
  const expanded = !settings.collapsedSources?.includes(group.id);

  const line = (row: DeployableListRow, rowTick: ArrivalKind | null, waiting: boolean) => (
    <DeployableLine
      key={row.key}
      row={row}
      tick={rowTick}
      waiting={waiting}
      accounts={accounts}
      deployedBy={deployedByOf(row)}
      open={row.site !== null && row.site.id === selectedSiteId}
      onOpen={() =>
        row.site !== null
          ? onOpenSite(row.site.id)
          : // NO SITE, and two reasons. A row a PARKED run put here has a gate
            // to reopen -- scoped to this app when the run is, so the flow
            // asks about this app alone. Anything else the source declares,
            // inactive or not, opens the compose flow for that app, which is
            // where Activate and Analyze live. A click never acts (D6).
            row.parked !== null
            ? onOpenParked(row.pkg?.id ?? "", runIsScopedToApp(row.parked, row.app) ? row.app : "")
            : onOpenDeclared(row.pkg?.id ?? "", row.app)
      }
      traffic={row.site === null ? null : (figures.get(row.site.id) ?? null)}
    />
  );

  // THE SECTION'S NAME, carried by the first group in it. The list renders one
  // group at a time with no sight of its neighbours, so which group starts a
  // section is decided in the fold (`startsSection`) where the order is known.
  const heading = group.startsSection ? (
    <h3 className="os-deploy-sectionhead">{SECTION_LABELS[group.section]}</h3>
  ) : null;

  if (group.pkg === null) {
    return (
      <>
        {heading}
        {line(group.rows[0]!, tick, group.rows[0]!.parked !== null)}
      </>
    );
  }

  const pkg = group.pkg;
  const waiting = group.rows.some((row) => row.parked !== null);
  const appsId = `os-deploy-apps-${group.id.replace(/[^A-Za-z0-9_-]/g, "-")}`;

  return <>
    {heading}
    <div className="os-deploy-group" data-archived={pkg.status === "archived" || undefined}>
      <div className="os-deploy-grouphead">
        <button type="button" className="deployable-source-heading" onClick={() => onOpenSource(pkg.id)} aria-label={`Open ${sourceLabel(pkg)}`}>
          <GitBranch size={14} aria-hidden />
          <span className="os-row-name">{sourceLabel(pkg)}</span>
          {pkg.updateAvailable ? <span className="deployable-status" data-tone="accent"><ArrowUpCircle size={12} aria-hidden /> Update available</span> : null}
          {pkg.status === "archived" ? <span className="deployable-status">Archived</span> : null}
        </button>
        <button type="button" className="os-deploy-disclose" aria-expanded={expanded} aria-controls={appsId}
          aria-label={`${expanded ? "Collapse" : "Expand"} ${sourceLabel(pkg)}`} onClick={() => toggleSource(group.id)}>
          <span>{group.rows.length} app{group.rows.length === 1 ? "" : "s"}</span><ChevronRight size={13} aria-hidden />
        </button>
      </div>
      {waiting && !expanded ? <span className="os-deploy-waiting">Review needed</span> : null}
      {expanded ? <div id={appsId} className="os-deploy-group-apps">{group.rows.map(row => line(row, null, false))}</div> : null}
    </div>
  </>;
}

function DeployableLine({
  row,
  tick,
  waiting,
  accounts,
  deployedBy,
  open,
  onOpen,
  traffic,
}: {
  row: DeployableListRow;
  tick: ArrivalKind | null;
  waiting: boolean;
  accounts: AccountRow[];
  /** "you", a name, or "" when nothing honest can be said. */
  deployedBy: string;
  open: boolean;
  onOpen: () => void;
  traffic: TrafficSummary | null;
}) {
  const now = useNow();
  const site = row.site;
  const archived = site?.status === "archived" || row.pkg?.status === "archived";
  const state = row.disabled ? "Inactive" : site ? siteStateWord(site) : row.parked ? "Review needed" : "Not deployed";
  const name = row.name;
  const client = accountNameFrom(accounts, site?.accountId ?? "");
  return <button type="button" className="os-row deployable-list-row" data-current={state === "Live" || undefined}
    data-dim={site?.status === "disabled" || archived || row.disabled || undefined} data-open={open || undefined} onClick={onOpen}>
    <span className="deployable-list-icon">{site?.kind === "shopify_storefront" ? <ShoppingBag size={18} aria-hidden /> : <Globe size={18} aria-hidden />}</span>
    <span className="deployable-list-identity"><span className="os-row-name">{name}</span>
      <span className="os-deploy-address">{row.hostname === name ? kindLabel(row.kind) : row.hostname || "No address yet"}</span>
    </span>
    <span className="deployable-list-summary">
      {client ? <span>{client}</span> : null}
      {deployedBy ? <span className="os-deploy-by" data-os-deployed-by>{site || row.parked ? "deployed" : "added"} by {deployedBy}</span> : null}
      {traffic?.lastServedAt ? <span title={`${traffic.requests.toLocaleString()} requests over the last week`}>served {formatFreshness(traffic.lastServedAt, now)}</span> : null}
    </span>
    <span className="deployable-list-state"><span className="deployable-status" data-tone={state === "Live" ? "accent" : state === "Unavailable" ? "warn" : "muted"} title={site?.status === "live" ? healthExplanation(site, now.getTime()) : undefined}>{state}</span>
      {waiting && state !== "Review needed" ? <span className="os-deploy-waiting">Review needed</span> : null}
      {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
    </span>
    <ChevronRight size={14} aria-hidden className="deployable-list-chevron" />
  </button>;
}
