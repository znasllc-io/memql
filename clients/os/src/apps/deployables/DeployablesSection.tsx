import { useMemo, useState } from "react";
import { Archive, ArrowUpCircle, ChevronRight, GitBranch, Globe, Plus } from "lucide-react";

import { usePackageActions } from "./packages/actions";
import { useDeployablesSettings } from "./settingsContext";
import {
  Button,
  Caption,
  Chip,
  Head,
  LiveList,
  Notice,
  Refine,
  Row as ListRow,
  Select,
  useNow,
  useThreeFeedView,
  type RefineChip,
} from "../../kit";
import { formatFreshness } from "../../kit/format";
import type { LiveView } from "../../live/liveView";
import type { ArrivalKind } from "../../live/arrival";
import { AccountChip, NO_ACCOUNT_LABEL } from "../accounts/AccountPicker";
import { accountIsArchived, accountName, accountNameFrom, type AccountRow } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
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
  standingInputFor,
  type DeployableListGroup,
  type DeployableListRow,
  type ListFilter,
} from "./list";
import { sourceLabel, type DeploymentRow, type PackageRow } from "./packages/rows";
import { ComposePage } from "./page/ComposePage";
import { DeployablePage } from "./page/DeployablePage";
import { HistoryView } from "./page/HistoryView";
import { Rail } from "./page/Rail";
import { SourceView } from "./page/SourceView";
import { SITE_STATUSES, type SiteRow } from "./rows";
import type { ListDensity } from "./settings";
import { LIST_TRAFFIC_WINDOW, type TrafficSummary } from "./traffic";
import { useSiteTraffic } from "./useSiteTraffic";
import type { CredentialRow } from "./sources/rows";
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
  | { kind: "deployable"; siteId: string }
  | { kind: "source"; packageId: string }
  | { kind: "history"; packageId: string }
  | { kind: "compose"; parkedPackageId?: string; only?: string };

export function DeployablesSection({
  sites,
  packages,
  parked,
  feedError,
  density,
  selectedSiteId,
  onSelectSite,
  viewerUserId,
  canWrite,
  isClusterOwner,
  clusterDomain,
  credentials,
  onAsk,
  onReseed,
}: {
  sites: LiveView<SiteRow> | null;
  packages: LiveView<PackageRow> | null;
  parked: LiveView<DeploymentRow> | null;
  feedError: string;
  density: ListDensity;
  selectedSiteId: string;
  onSelectSite: (siteId: string) => void;
  viewerUserId: string;
  canWrite: boolean;
  isClusterOwner: boolean;
  clusterDomain: string;
  credentials: readonly CredentialRow[];
  onAsk?: (tag: string) => void;
  onReseed: () => void;
}) {
  const [filter, setFilter] = useState<ListFilter>(DEFAULT_LIST_FILTER);
  const [showArchived, setShowArchived] = useState(false);
  // THE VIEW INITIALISES FROM THE SELECTION, so arriving from the Map -- which
  // navigates here with a site chosen -- lands on that deployable rather than
  // on the list with an invisible selection.
  const [view, setView] = useState<DeployablesView>(() =>
    selectedSiteId === "" ? { kind: "list" } : { kind: "deployable", siteId: selectedSiteId },
  );
  // WHAT WAS JUST DELETED, so the list can say what happened to it. The name
  // is free the instant the row is stamped; the domains come down on the
  // reconciliation sweep's own schedule, and this says so rather than implying
  // the whole thing is finished.
  const [justDeleted, setJustDeleted] = useState("");
  const accounts = useAccountOptions();

  const siteRows = sites?.snapshot.rows ?? [];
  const packageRows = packages?.snapshot.rows ?? [];
  const parkedRows = parked?.snapshot.rows ?? [];

  const packageActions = usePackageActions();
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
    setJustDeleted("");
    setView({ kind: "deployable", siteId });
  }

  function backToList() {
    setView({ kind: "list" });
  }

  // ---- the views -----------------------------------------------------------

  /**
   * Turn a declared app back on.
   *
   * ONLY that -- it does not deploy. Somebody clicking an app they had
   * declined is asking to reconsider it, not to ship it; the row becomes an
   * ordinary "not deployed" one and deploying it is the NEXT click. Making
   * this deploy would be the same mistake the off-list exists to fix.
   */
  async function enableDeclared(packageId: string, app: string) {
    const pkg = packageRows.find((p) => p.id === packageId);
    if (pkg === undefined) return;
    await packageActions.setDeployableEnabled(packageId, pkg.disabledDeployables, app, true);
    onReseed();
  }

  // DEPLOYING AN APP THE SOURCE ONLY DECLARES.
  //
  // It has no site and no run of its own, so there is nothing to open: the
  // analysis that reads the tree is what produces the confirm gate where an
  // address is asked for. That is why this is an ACT and not navigation -- and
  // why the row that calls it says "not deployed" rather than looking like a
  // page.
  //
  // `only` is what keeps it to ONE app. Without it the gate would send no
  // placement for the apps that already have sites, the engine would default
  // them to not-skipped, and deploying one app would rebuild and republish
  // every one of them -- which is exactly the complaint that took the Deploy
  // button off the source page.
  async function openDeclared(packageId: string, app: string) {
    setView({ kind: "compose", parkedPackageId: packageId, only: app });
    await packageActions.deploy(packageId, false);
    onReseed();
  }

  if (view.kind === "compose") {
    const parkedFor =
      view.parkedPackageId === undefined ? null : newestParked(packageRows, parkedRows, view.parkedPackageId);
    return (
      <ComposePage
        clusterDomain={clusterDomain}
        canWrite={canWrite}
        isClusterOwner={isClusterOwner}
        viewerUserId={viewerUserId}
        credentials={credentials}
        onBack={backToList}
        onAsk={onAsk}
        parked={parkedFor ?? undefined}
        only={view.only}
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
      return <HistoryView pkg={pkg} canWrite={canWrite} onBack={() => setView({ kind: "source", packageId: pkg.id })} />;
    }
    return (
      <SourceView
        pkg={pkg}
        apps={apps}
        credentials={credentials}
        canWrite={canWrite}
        onBack={backToList}
        onOpenHistory={() => setView({ kind: "history", packageId: pkg.id })}
        onOpenApp={openSite}
        onAsk={onAsk}
        attempts={parkedRows.filter((d) => d.packageId === pkg.id).length}
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
        canWrite={canWrite}
        isClusterOwner={isClusterOwner}
        clusterDomain={clusterDomain}
        onAsk={onAsk}
        onBack={backToList}
        onOpenSource={(packageId) => setView({ kind: "source", packageId })}
        onOpenHistory={(packageId) => setView({ kind: "history", packageId })}
        onDeleted={(deletedId) => {
          const gone = siteRows.find((s) => s.id === deletedId);
          setJustDeleted(gone?.hostname ?? "");
          onSelectSite("");
          setView({ kind: "list" });
        }}
      />
    );
  }

  return renderList();

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
      ? "Nothing archived. Archived deployables stay here, so they can always be found again."
      : filterIsNarrowing(filter)
        ? "Nothing matches. Clear the search or a facet in Refine to see your deployables."
        : canWrite
          ? "No deployables yet. New deployable is where one starts."
          : "No deployables yet. The engine decides which reach you: your own, or every one of them if you are a cluster owner.";

    return (
      <div className="os-app-stack os-deployables-list" data-density={density}>
        <Head title="Deployables" meta={listedCount}>
          <Refine
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
          {canWrite ? (
            <Button tone="primary" onClick={() => setView({ kind: "compose" })}>
              <Plus size={13} aria-hidden /> New deployable
            </Button>
          ) : null}
        </Head>

        {/* WHAT HAPPENED TO THE THING THAT IS NO LONGER HERE. The name is free
            the instant the row is stamped; the certificate and route come down
            on the reconciliation sweep's own schedule, and saying both is the
            difference between "it worked" and "it worked, and here is the part
            that is still happening". */}
        {justDeleted === "" ? null : (
          <Notice
            tone="info"
            sentence={`${justDeleted} is deleted, and its name is free to use again.`}
            next="Any domains it served come down on the next reconciliation sweep, within about two minutes. Its record stays in this cluster's history."
          >
            <Button tone="quiet" onClick={() => setJustDeleted("")}>
              Got it
            </Button>
          </Notice>
        )}

        {feedError ? (
          <Notice
            tone="error"
            sentence="This cluster did not return its deployables."
            next="The engine decides which reach you -- your own, or every one of them if you are a cluster owner."
          >
            <Button onClick={onReseed}>Try again</Button>
          </Notice>
        ) : null}

        <LiveList<DeployableListGroup>
          source={list}
          rowId={(g) => g.id}
          fingerprint={groupFingerprint}
          label="Deployables in this cluster"
          emptyText={emptyText}
          renderRow={(group, tick) => (
            <GroupLine
              group={group}
              figures={figures}
              tick={tick}
              accounts={accounts}
              selectedSiteId={selectedSiteId}
              onOpenSite={openSite}
              onOpenSource={(packageId) => setView({ kind: "source", packageId })}
              onOpenParked={(packageId) => setView({ kind: "compose", parkedPackageId: packageId })}
              onDeployDeclared={(packageId, app) => void openDeclared(packageId, app)}
              onEnableDeclared={(packageId, app) => void enableDeclared(packageId, app)}
            />
          )}
        />

        {archivedCount > 0 || showArchived ? (
          <div className="os-archive-toggle">
            <button type="button" className="os-sort" aria-expanded={showArchived} onClick={flipArchived}>
              <Archive size={12} aria-hidden />{" "}
              {showArchived ? "Show active deployables" : `Show archived (${archivedCount})`}
            </button>
            <Caption>
              {showArchived
                ? "Archived deployables are kept, not deleted -- restoring one puts it back unpublished. Deleting one from here releases its name."
                : "An archive is a place, not a void."}
            </Caption>
          </div>
        ) : null}
      </div>
    );
  }
}

/** The status facet's words, matching the bar's (D6). */
function statusFacetLabel(status: string): string {
  switch (status) {
    case "live":
      return "Published";
    case "disabled":
      return "Unpublished";
    case "draft":
      return "Draft";
    default:
      return status;
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
  selectedSiteId,
  onOpenSite,
  onOpenSource,
  onOpenParked,
  onDeployDeclared,
  onEnableDeclared,
  figures,
}: {
  group: DeployableListGroup;
  tick: ArrivalKind | null;
  accounts: AccountRow[];
  selectedSiteId: string;
  onOpenSite: (siteId: string) => void;
  onOpenSource: (packageId: string) => void;
  onOpenParked: (packageId: string) => void;
  /** A declared app with no site: deploying it is what asks for its address. */
  onDeployDeclared: (packageId: string, app: string) => void;
  /** A declared app the owner turned off: the only act it offers is back on. */
  onEnableDeclared: (packageId: string, app: string) => void;
  figures: Map<string, TrafficSummary>;
}) {
  // COLLAPSED IS THE DEFAULT, and which groups are open is remembered. The
  // OPEN set is stored rather than the closed one: closed is the default, so a
  // list of what is shut would have to name every source that ever existed and
  // a source added tomorrow would arrive open.
  const { settings, toggleSource } = useDeployablesSettings();
  const expanded = settings.expandedSources.includes(group.id);

  const line = (row: DeployableListRow, rowTick: ArrivalKind | null, waiting: boolean) => (
    <DeployableLine
      key={row.key}
      row={row}
      tick={rowTick}
      waiting={waiting}
      accounts={accounts}
      open={row.site !== null && row.site.id === selectedSiteId}
      onOpen={() =>
        row.site !== null
          ? onOpenSite(row.site.id)
          : // NO SITE, and the three reasons are different. A row a PARKED run
            // put here has a gate to reopen. One the owner TURNED OFF is
            // inert: clicking it offers to turn it back on and nothing else,
            // because clicking a thing somebody declined must not deploy it.
            // One the source merely DECLARES has nothing yet, and deploying it
            // is what makes the gate.
            row.parked !== null
            ? onOpenParked(row.pkg?.id ?? "")
            : row.disabled === true
              ? onEnableDeclared(row.pkg?.id ?? "", row.app)
              : onDeployDeclared(row.pkg?.id ?? "", row.app)
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

  // THE SOURCE IS A REAL ROW. It was a `div` of caption text with two chips,
  // wedged between clickable rows -- not focusable, not announced, and not
  // openable, while looking like a heading for the list beneath it. It opens
  // the source's own view now, which is where its credential, its auto-deploy
  // switch, its history and its archive live.
  return (
    <>
      {heading}
      <div className="os-deploy-group" data-archived={pkg.status === "archived" || undefined}>
        <div className="os-deploy-grouphead">
          {/* THE COUNT IS THE CONTROL, and a bare chevron was not: a lone arrow
              said nothing about what it would do. The count was already on this
              row and is exactly what opening the group reveals, so the two are
              one control -- "2 apps" with a chevron that turns.

              IT SITS OUTSIDE THE ROW, not in its trailing cluster, because an
              openable ListRow IS a button and a button cannot contain another
              one: nested, this rendered but never fired.

              TWO CONTROLS, TWO JOBS: this opens the group, the row opens the
              SOURCE's own view. Folding them together would cost one. */}
          <button
            type="button"
            className="os-deploy-disclose"
            aria-expanded={expanded}
            aria-controls={appsId}
            aria-label={`${expanded ? "Collapse" : "Expand"} ${sourceLabel(pkg)}`}
            onClick={() => toggleSource(group.id)}
          >
            <ChevronRight size={12} aria-hidden />
            <span>
              {group.rows.length} app{group.rows.length === 1 ? "" : "s"}
            </span>
          </button>
          <ListRow
            icon={<GitBranch size={15} aria-hidden />}
            name={sourceLabel(pkg)}
            current
            onOpen={() => onOpenSource(pkg.id)}
            state={
              <>
                {pkg.updateAvailable ? (
                  <Chip tone="accent" title={`Newer upstream: ${pkg.latestKnownVersion}`}>
                    <ArrowUpCircle size={11} aria-hidden /> update
                  </Chip>
                ) : null}
                {pkg.status === "archived" ? (
                  <Chip tone="muted">
                    <Archive size={11} aria-hidden /> archived
                  </Chip>
                ) : null}
                {waiting ? <span className="os-deploy-waiting">a deploy is waiting for you</span> : null}

                {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
                <span className="os-deploy-railcol" />
              </>
            }
          />
        </div>
        {/* A SHUT GROUP RENDERS NO APPS AT ALL rather than hiding them with
            CSS: they are not read, not focusable, and not found by a search of
            the page -- which is what "collapsed" has to mean for the count on
            the row to be the honest summary of what is inside. */}
        {expanded ? (
          <div id={appsId} className="os-deploy-group-apps">
            {group.rows.map((row) => line(row, null, false))}
          </div>
        ) : null}
      </div>
    </>
  );
}

function DeployableLine({
  row,
  tick,
  waiting,
  accounts,
  open,
  onOpen,
  traffic,
}: {
  row: DeployableListRow;
  tick: ArrivalKind | null;
  waiting: boolean;
  accounts: AccountRow[];
  open: boolean;
  onOpen: () => void;
  traffic: TrafficSummary | null;
}) {
  const now = useNow();
  const site = row.site;
  const archived = site?.status === "archived" || row.pkg?.status === "archived";
  return (
    <ListRow
      icon={<Globe size={16} aria-hidden />}
      name={row.name}
      current={site?.status === "live"}
      dim={site?.status === "disabled" || archived || row.disabled === true}
      open={open}
      onOpen={onOpen}
      state={
        <>
          {waiting ? <span className="os-deploy-waiting">a deploy is waiting for you</span> : null}
          {/* THE OWNER TURNED IT OFF, said on the row rather than only by
              dimming it: a greyed row with no word reads as broken. */}
          {row.disabled === true ? <Chip tone="muted">off</Chip> : null}
          <AccountChip name={accountNameFrom(accounts, site?.accountId ?? "")} />
          {traffic === null || traffic.lastServedAt === "" ? null : (
            <Chip title={`${traffic.requests.toLocaleString()} requests over the last week`}>
              served {formatFreshness(traffic.lastServedAt, now)}
            </Chip>
          )}
          {/* A FIXED TRAILING COLUMN, so the five marks land at the same x on
              every row and can be scanned DOWN the list. They used to follow a
              variable-length hostname and land somewhere different on each. */}
          <span className="os-deploy-railcol">
            <Rail compact input={standingInputFor(row)} label={`${row.name} stops`} />
          </span>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {row.hostname === "" ? (
        <span className="os-deploy-address">no address yet</span>
      ) : row.hostname === row.name ? null : (
        <span className="os-deploy-address os-mono">{row.hostname}</span>
      )}
    </ListRow>
  );
}
