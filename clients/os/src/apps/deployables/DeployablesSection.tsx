import { useMemo, useState } from "react";
import { Archive, ArrowUpCircle, GitBranch, Globe, Plus } from "lucide-react";

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
  useThreeFeedView,
  type RefineChip,
} from "../../kit";
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
  standingInputFor,
  type DeployableListGroup,
  type DeployableListRow,
  type ListFilter,
} from "./list";
import { sourceLabel, type DeploymentRow, type PackageRow } from "./packages/rows";
import { ComposePage } from "./page/ComposePage";
import { DeployablePage } from "./page/DeployablePage";
import { Rail } from "./page/Rail";
import { SITE_STATUSES, type SiteRow } from "./rows";
import type { ListDensity } from "./settings";
import type { CredentialRow } from "./sources/rows";
import { DEPLOYABLE_KINDS, kindLabel } from "./targets";

// The Deployables section (epic memql#4885, design D2): the list, and the
// page or the compose reading beneath or in place of it.
//
// ===========================================================================
// ONE ROW PER THING THAT SERVES OR WILL
// ===========================================================================
// One LiveList over the SITE feed, joined client-side to the PACKAGE feed by
// `packageId` and to the PARKED RUNS by the same key (`list.ts` is the fold).
// A package with several apps renders as a group -- the source line once,
// the rows beneath it -- and a hand-made site renders as its own row. Each
// row carries what a person reads a deployable by: its name, its address,
// the standing five-dot rail (the same vocabulary the page draws in full, so
// a row reads as the shape they will see), the client it is for, the arrival
// cue, and the waiting mark when a run of its source is parked at the
// confirm gate.
//
// ===========================================================================
// THE FACETS ARE FOLDS, AND A FILTER RE-BASELINES THROUGH THE VIEW KEY
// ===========================================================================
// Search, kind, status, client and source live behind the one Refine
// affordance on the Head line (DESIGN.md rule 2) and fold the seeded
// snapshot client-side -- the seed is the population. A filter change
// rebuilds the view through `listViewKey` rather than a `key` prop on the
// list, so revealing rows the browser already held announces nothing
// (clients/os/README.md: a resync is not an arrival). "Show archived" stays
// the quiet flip below the list, the archive being a PLACE (rule 10).
//
// ===========================================================================
// ONE SELECTION, AND THE PAGE BENEATH THE LIST
// ===========================================================================
// The selection is the Map's (`MapSelection`, held by the app root), so
// walking a cluster on the map and switching to the list lands on the same
// deployable. Selecting a row opens its page beneath the list; New
// deployable, and a row that will serve, replace the list with the compose
// reading in place -- the Head's title becomes "New deployable" and a quiet
// Back returns here.

/** What replaced the list: a fresh compose, or a parked run's reading. */
type ComposeTarget = { kind: "new" } | { kind: "parked"; packageId: string };

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
  /** The parked runs -- the fourth feed the app root retains (design section A). */
  parked: LiveView<DeploymentRow> | null;
  /** The first error any of the three feeds reported, verbatim. */
  feedError: string;
  density: ListDensity;
  selectedSiteId: string;
  onSelectSite: (siteId: string) => void;
  viewerUserId: string;
  /** Rank >= 200. Presentation over a server-side law: the guard is the gate. */
  canWrite: boolean;
  /** The client's own domain renders only for one. */
  isClusterOwner: boolean;
  /** The domain this cluster serves, threaded to the page's Domains content. */
  clusterDomain: string;
  /** The credential feed's cards, for the Source stop. */
  credentials: readonly CredentialRow[];
  onAsk?: (tag: string) => void;
  onReseed: () => void;
}) {
  const [filter, setFilter] = useState<ListFilter>(DEFAULT_LIST_FILTER);
  const [showArchived, setShowArchived] = useState(false);
  const [compose, setCompose] = useState<ComposeTarget | null>(null);
  const accounts = useAccountOptions();

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

  // How many rows the flip would reveal: the whole archived population, before
  // any facet -- the number is about the place, not about the question.
  const archivedCount = useMemo(
    () => foldDeployables(siteRows, packageRows, parkedRows, DEFAULT_LIST_FILTER, true).reduce((n, g) => n + g.rows.length, 0),
    [siteRows, packageRows, parkedRows],
  );

  function patch(next: Partial<ListFilter>) {
    setFilter((held) => ({ ...held, ...next }));
  }

  function flipArchived() {
    // The status facet asks a question of the ACTIVE list -- draft, live,
    // disabled -- and archived is a place rather than one of those answers,
    // so the facet is cleared on the way in and not offered there.
    setShowArchived((v) => !v);
    patch({ status: "" });
  }

  const open = siteRows.find((s) => s.id === selectedSiteId) ?? null;
  const pkg = open === null || open.packageId === "" ? null : (packageRows.find((p) => p.id === open.packageId) ?? null);
  const siblings =
    open === null || open.packageId === "" ? [] : siteRows.filter((s) => s.packageId === open.packageId && s.id !== open.id);

  // The parked reading a "will serve" row opened. The run may have moved on
  // while this was open -- confirmed from another window, say -- in which
  // case it left the feed, there is nothing to reopen, and the list is what
  // the person sees.
  const parkedFor =
    compose?.kind === "parked" ? newestParked(packageRows, parkedRows, compose.packageId) : null;

  if (compose?.kind === "new" || parkedFor !== null) {
    return (
      <ComposePage
        clusterDomain={clusterDomain}
        canWrite={canWrite}
        isClusterOwner={isClusterOwner}
        viewerUserId={viewerUserId}
        credentials={credentials}
        onBack={() => setCompose(null)}
        onAsk={onAsk}
        parked={parkedFor ?? undefined}
        placed={
          parkedFor === null
            ? []
            : siteRows.filter((s) => s.packageId === parkedFor.pkg.id).map((s) => s.packageDeployableName)
        }
      />
    );
  }

  // The Refine chips: every active facet, removable in place (rule 2).
  const chips: RefineChip[] = [];
  if (filter.kind !== "") chips.push({ id: "kind", label: kindLabel(filter.kind), onRemove: () => patch({ kind: "" }) });
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

  // Empty and filtered-to-empty are DIFFERENT answers: one is about the
  // cluster, the other about the question just asked of it.
  const emptyText = showArchived
    ? "Nothing archived. Archived deployables stay here, so they can always be found again."
    : filterIsNarrowing(filter)
      ? "Nothing matches. Clear the search or a facet in Refine to see your deployables."
      : canWrite
        ? "No deployables yet. New deployable is where one starts."
        : "No deployables yet. The engine decides which reach you: your own, or every one of them if you are a cluster owner.";

  return (
    <div className="os-app-stack" data-density={density}>
      {/* THE HEAD IS THE WHOLE TOP (DESIGN.md rules 1-3): the name, the count
          of what the list is showing, one Refine affordance, and ONE primary
          action. New deployable is that action, and it renders for the deploy
          tier only -- a reader gets no disabled button to read past. */}
      <Head title="Deployables" meta={listedCount}>
        <Refine search={filter.search} onSearch={(search) => patch({ search })} chips={chips} label="Refine deployables">
          <Select id="deployables-kind" label="Kind" value={filter.kind} onChange={(kind) => patch({ kind })}>
            <option value="">Any kind</option>
            {DEPLOYABLE_KINDS.map((kind) => (
              <option key={kind.value} value={kind.value}>
                {kind.label}
              </option>
            ))}
          </Select>
          {showArchived ? null : (
            <Select id="deployables-status" label="Status" value={filter.status} onChange={(status) => patch({ status })}>
              <option value="">Any status</option>
              {SITE_STATUSES.filter((s) => s !== "archived").map((status) => (
                <option key={status} value={status}>
                  {status}
                </option>
              ))}
            </Select>
          )}
          {/* THE CLIENT FACET (epic memql#4800, D5), the Files pattern: "No
              client" is a first-class answer because "what still needs
              filing" is the one question the other two cannot express. */}
          <Select id="deployables-client" label="Client" value={filter.accountId} onChange={(accountId) => patch({ accountId })}>
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
          <Button tone="primary" onClick={() => setCompose({ kind: "new" })}>
            <Plus size={13} aria-hidden /> New deployable
          </Button>
        ) : null}
      </Head>

      {/* A read this surface is not allowed to make comes back as a refusal on
          the feed, not as an empty list. NO `detail`: LiveList already prints
          the feed's error verbatim directly beneath the list it belongs to, and
          repeating it a few lines up reads as two different failures. */}
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
            tick={tick}
            accounts={accounts}
            selectedSiteId={selectedSiteId}
            onOpenSite={(siteId) => onSelectSite(selectedSiteId === siteId ? "" : siteId)}
            onOpenParked={(packageId) => setCompose({ kind: "parked", packageId })}
          />
        )}
      />

      {archivedCount > 0 || showArchived ? (
        <div className="os-archive-toggle">
          {/* Quiet text, not button furniture (DESIGN.md rules 3/10): a view
              flip below the list, weighted like the sort control. */}
          <button type="button" className="os-sort" aria-expanded={showArchived} onClick={flipArchived}>
            <Archive size={12} aria-hidden />{" "}
            {showArchived ? "Show active deployables" : `Show archived (${archivedCount})`}
          </button>
          <Caption>
            {showArchived
              ? "Archived deployables are kept, not deleted. Restoring one puts it back on the active list."
              : "An archive is a place, not a void."}
          </Caption>
        </div>
      ) : null}

      {open === null ? null : (
        <DeployablePage
          key={open.id}
          site={open}
          pkg={pkg}
          siblings={siblings}
          credentials={credentials}
          viewerUserId={viewerUserId}
          canWrite={canWrite}
          isClusterOwner={isClusterOwner}
          clusterDomain={clusterDomain}
          onAsk={onAsk}
        />
      )}
    </div>
  );
}

/** The newest parked run of a source, with the source, or null when either is gone. */
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
// One entry: a source with its rows, or a hand-made site on its own
// ---------------------------------------------------------------------------

function GroupLine({
  group,
  tick,
  accounts,
  selectedSiteId,
  onOpenSite,
  onOpenParked,
}: {
  group: DeployableListGroup;
  tick: ArrivalKind | null;
  accounts: AccountRow[];
  selectedSiteId: string;
  onOpenSite: (siteId: string) => void;
  onOpenParked: (packageId: string) => void;
}) {
  const line = (row: DeployableListRow, rowTick: ArrivalKind | null, waiting: boolean) => (
    <DeployableLine
      key={row.key}
      row={row}
      tick={rowTick}
      waiting={waiting}
      accounts={accounts}
      open={row.site !== null && row.site.id === selectedSiteId}
      onOpen={() => (row.site === null ? onOpenParked(row.pkg?.id ?? "") : onOpenSite(row.site.id))}
    />
  );

  // A HAND-MADE DEPLOYABLE IS ITS OWN SCOPE. There is no source line above it
  // to carry the waiting mark, so the row is where the fact belongs -- which
  // is still exactly one place.
  if (group.pkg === null) return line(group.rows[0]!, tick, group.rows[0]!.parked !== null);

  const pkg = group.pkg;
  // ONE PARKED RUN BELONGS TO ONE SOURCE. Every row of a group carries the
  // same run, so saying it per row said one fact three times, stacked
  // (DESIGN.md rule 7). It is said here, once, beside the update chip.
  const waiting = group.rows.some((row) => row.parked !== null);
  return (
    <div className="os-deploy-group" data-archived={pkg.status === "archived" || undefined}>
      {/* THE SOURCE LINE, ONCE (DESIGN.md rule 7). The update chip lives here
          because a newer upstream version is a fact about the source, not
          about any one app it produced: it is a STANDING mark, true until
          somebody deploys, beside the arrival ring that says it just landed
          (clients/os/README.md: the update needs both). The waiting mark is
          the same class of fact and sits beside it. */}
      <div className="os-deploy-group-source">
        <span className="os-pkg-source">
          {pkg.sourceKind === "repo" ? <GitBranch size={11} aria-hidden /> : null}
          {sourceLabel(pkg)}
        </span>
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
      </div>
      {group.rows.map((row) => line(row, null, false))}
    </div>
  );
}

function DeployableLine({
  row,
  tick,
  waiting,
  accounts,
  open,
  onOpen,
}: {
  row: DeployableListRow;
  tick: ArrivalKind | null;
  /** The waiting mark belongs on the row only when the row IS the scope. */
  waiting: boolean;
  accounts: AccountRow[];
  open: boolean;
  onOpen: () => void;
}) {
  const site = row.site;
  const archived = site?.status === "archived" || row.pkg?.status === "archived";
  return (
    <ListRow
      icon={<Globe size={16} aria-hidden />}
      name={row.name}
      current={site?.status === "live"}
      dim={site?.status === "disabled" || archived}
      open={open}
      onOpen={onOpen}
      state={
        <>
          {waiting ? <span className="os-deploy-waiting">a deploy is waiting for you</span> : null}
          <AccountChip name={accountNameFrom(accounts, site?.accountId ?? "")} />
          {/* THE RAIL IS NOT PART OF THE ADDRESS. Rendered after the hostname
              with the same spacing, the five marks read as punctuation on the
              end of it -- `store.example.com` and the dots ran on as one
              string. It belongs on the trailing edge, and LAST there rather
              than first: the rail is what a person scans DOWN a list, so it
              wants the one position that is the same on every row. */}
          <Rail compact input={standingInputFor(row)} label={`${row.name} stops`} />
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {/* The address, said once: a hand-made site with no label IS its
          address, and the name already says it. A row that will serve has no
          address yet, and says so in prose rather than in the data voice. */}
      {row.hostname === "" ? (
        <span className="os-deploy-address">no address yet</span>
      ) : row.hostname === row.name ? null : (
        <span className="os-deploy-address os-mono">{row.hostname}</span>
      )}
    </ListRow>
  );
}
