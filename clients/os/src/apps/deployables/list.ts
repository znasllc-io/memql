import { PENDING_DEPLOYMENT_STATUSES } from "./packages/rows";
import { packageFingerprint, runCoversApp, shortRepo, sourceLabel, type DeploymentRow, type PackageRow } from "./packages/rows";
import { isPlaceholderBundle, type StandingInput } from "./page/rail";
import { bundleForm, siteFingerprint, siteName, type SiteRow } from "./rows";
import { deployerOf } from "./people";

// The Deployables list's fold (epic memql#4885, design D2): ONE ROW PER THING
// THAT SERVES OR WILL, grouped under the source it came from.
//
// ===========================================================================
// PURE, AND THE ASSERTION
// ===========================================================================
// Everything here is a function of the three feeds the app root retains --
// the sites, the packages and the parked runs -- and the question being asked
// of them. What the list SAYS is asserted against this module; the section
// only draws its answer. That is the same split the map keeps between
// `layout.ts` and `DeployMap.tsx`, and for the same reason: a fold asserted
// through render() is asserted through three layers that can each fail for
// unrelated reasons.
//
// ===========================================================================
// THE SEED IS THE POPULATION, THE FACETS ARE FOLDS
// ===========================================================================
// `sitesAll` and `packagesAll` carry no caller term -- the concept's tier
// decides (memql#5303) -- so one seed of each holds the complete truth the
// caller may read,
// and search, kind, status, client, source and the archived flip are all
// client-side folds over it (the rule Files states for its browse). A filter
// change therefore costs no round trip -- and, because it re-baselines
// through the view KEY rather than a `key` prop on the list, it announces
// nothing: revealing rows the browser already held is not the cluster sending
// them (clients/os/README.md).

/** The client facet's two answers that are not an account id. */
export const ACCOUNT_ANY = "all";
export const ACCOUNT_NONE = "none";

/**
 * The three ways a source arrives, as a person chose them (design D6). A
 * bundle baked into the edge image -- MemQL OS, a baked site -- is
 * none of the three, and answers "" rather than being described as something
 * it is not: it matches no source facet and shows under "Any source".
 */
export const SOURCE_FACETS = [
  { value: "repository", label: "A repository" },
  { value: "zip", label: "A zip in Files" },
  { value: "ci", label: "Pushed by CI" },
] as const;

export type SourceFacet = (typeof SOURCE_FACETS)[number]["value"];

export interface ListFilter {
  search: string;
  /** A kind from the target registry, or "" for any. */
  kind: string;
  /** draft | live | disabled, or "" for any. Archived is a PLACE (the flip), not a facet. */
  status: string;
  /** ACCOUNT_ANY, ACCOUNT_NONE, or an account id. */
  accountId: string;
  /** A SourceFacet, or "" for any. */
  source: string;
}

export const DEFAULT_LIST_FILTER: ListFilter = {
  search: "",
  kind: "",
  status: "",
  accountId: ACCOUNT_ANY,
  source: "",
};

/** Whether the filter hides anything -- empty and filtered-to-empty are different answers. */
export function filterIsNarrowing(filter: ListFilter): boolean {
  return (
    filter.search.trim() !== "" ||
    filter.kind !== "" ||
    filter.status !== "" ||
    filter.accountId !== ACCOUNT_ANY ||
    filter.source !== ""
  );
}

/**
 * The transform's inputs written down. `useThreeFeedView` rebuilds on it, and
 * a rebuild is how a filter re-baselines the arrival cue: the view's version
 * does not move, so `useArrivals` folds nothing and the rows the filter just
 * revealed ring for nobody.
 */
export function listViewKey(filter: ListFilter, showArchived: boolean): string {
  return [
    "deployables",
    showArchived ? "archived" : "active",
    filter.search.trim().toLowerCase(),
    filter.kind,
    filter.status,
    filter.accountId,
    filter.source,
  ].join(":");
}

// ---------------------------------------------------------------------------
// The rows
// ---------------------------------------------------------------------------

export interface DeployableListRow {
  /** The site id, or `<packageId>/<app>` for a row that will serve. */
  key: string;
  /** The site row, or null for an app a parked run names that has no site yet. */
  site: SiteRow | null;
  /** The source, or null for a hand-made deployable. */
  pkg: PackageRow | null;
  /** The manifest app name; "" for a hand-made site, or for a source whose report names no app. */
  app: string;
  /** What a person calls it: the app's manifest name, a hand-made site's label, else its address. */
  name: string;
  /** The address it answers at; "" before its first publish. */
  hostname: string;
  /** spa | static | shopify_storefront, from the site or from the report. */
  kind: string;
  /** The newest parked run of its source, or null: the waiting mark's fact. */
  parked: DeploymentRow | null;
  /**
   * Who deployed it (epic memql#5289, task memql#5306): the parked run's
   * requester, else the site's owner -- the deployer since PR #5284 -- else
   * the source's owner. A bare user id, "" when no row says; the LIST turns
   * it into a name or "you", because only the list holds the roster.
   */
  deployedBy: string;
  /**
   * The owner turned this one off.
   *
   * TRUE only for a DECLARED app with no site: the site is the fact and the
   * list is a preference about an app that has none, so a name lingering after
   * the app was deployed again marks nothing. A disabled row is listed and
   * inert -- findable, never offered for deploy, built by no run.
   */
  disabled?: boolean;
}

/**
 * Which half of the list a group belongs to.
 *
 * The distinction a person actually makes is "did this come from somewhere I
 * can redeploy from" -- a repository or an upload -- or is it a thing that
 * stands on its own. Sorting the two together by address produced a list where
 * a two-app source sat between two hand-made sites, which is the shape that
 * read as a jumble.
 */
export type ListSection = "source" | "standalone";

const SECTION_ORDER: Readonly<Record<ListSection, number>> = { source: 0, standalone: 1 };

/** What each section is called, where a person reads it. */
export const SECTION_LABELS: Readonly<Record<ListSection, string>> = {
  source: "From a source",
  standalone: "Standalone",
};

export interface DeployableListGroup {
  /** `pkg:<id>` for a source and its rows; `site:<id>` for a hand-made site on its own. */
  id: string;
  pkg: PackageRow | null;
  rows: DeployableListRow[];
  section: ListSection;
  /**
   * Whether this group renders its section's heading.
   *
   * Computed here rather than in the view because it is a fact about the
   * ORDER, and the view renders one group at a time with no sight of its
   * neighbours. Always true for the first group of a section, including when
   * that section is the only one -- a list of hand-made sites still says what
   * it is.
   */
  startsSection: boolean;
}

/** Which of the three sources a row came through, or "" for none of them. */
export function sourceOf(row: Pick<DeployableListRow, "site" | "pkg">): SourceFacet | "" {
  if (row.pkg !== null) {
    if (row.pkg.sourceKind === "repo") return "repository";
    if (row.pkg.sourceKind === "artifact") return "zip";
    return "";
  }
  if (row.site === null) return "";
  // A hand-made site IS its bundle: published from a Library zip when the row
  // records the artifact, otherwise pushed by CI through the bundle route --
  // the placeholder a CI-fed site starts with included, since its bytes are
  // on their way from exactly there.
  if (row.site.artifactId !== "") return "zip";
  const form = bundleForm(row.site.bundleRef);
  if (form === "uploaded" || isPlaceholderBundle(row.site.bundleRef)) return "ci";
  return "";
}

/** The standing rail's input for a row: the source, the app, the parked run, the site. */
export function standingInputFor(row: DeployableListRow): StandingInput {
  return { mode: "standing", pkg: row.pkg, app: row.app, run: row.parked, site: row.site };
}

// ---------------------------------------------------------------------------
// The fold
// ---------------------------------------------------------------------------

function siteRowFor(site: SiteRow, pkg: PackageRow | null, parked: DeploymentRow | null): DeployableListRow {
  const app = pkg === null ? "" : site.packageDeployableName;
  // WHAT A PERSON CALLS IT: the app's name in the source's manifest, since
  // that is the name they wrote and the name the report shows; a hand-made
  // site's label when it has one; else its address, which is what a
  // deployable IS. Never blank -- a nameless row is indistinguishable from a
  // row that failed to render.
  const name = pkg?.declares.find(d => d.name === app)?.displayName?.trim() || app || site.title.trim() || siteName(site);
  // NEVER disabled: a site row means the app was deployed, and the owner's
  // off-list is a preference about apps that have none.
  return {
    key: site.id,
    site,
    pkg,
    app,
    name,
    hostname: site.hostname,
    kind: site.kind,
    parked,
    deployedBy: deployerOf(parked, site, pkg),
    disabled: false,
  };
}

function isArchived(row: DeployableListRow): boolean {
  return row.site?.status === "archived" || row.pkg?.status === "archived";
}

function matches(row: DeployableListRow, filter: ListFilter): boolean {
  if (filter.kind !== "" && row.kind !== filter.kind) return false;
  // A will-serve row has no status yet, so a status facet leaves it out: it
  // is none of draft, live or disabled, and claiming one would be a guess.
  if (filter.status !== "" && (row.site?.status ?? "") !== filter.status) return false;
  const accountId = row.site?.accountId ?? row.pkg?.accountId ?? "";
  if (filter.accountId === ACCOUNT_NONE && accountId !== "") return false;
  if (filter.accountId !== ACCOUNT_ANY && filter.accountId !== ACCOUNT_NONE && accountId !== filter.accountId) return false;
  if (filter.source !== "" && sourceOf(row) !== filter.source) return false;
  const needle = filter.search.trim().toLowerCase();
  if (needle !== "") {
    const hay = [row.name, row.hostname, row.app, row.pkg?.name ?? "", row.pkg === null ? "" : sourceLabel(row.pkg)]
      .join(" ")
      .toLowerCase();
    if (!hay.includes(needle)) return false;
  }
  return true;
}

/**
 * Addressed rows first, by address; rows that will serve after them, by name.
 * A TUPLE rather than a prefixed string, because locale collation orders
 * punctuation before letters and a "~" prefix put the unaddressed rows first.
 */
function compareRows(a: DeployableListRow, b: DeployableListRow): number {
  const unaddressed = (a.hostname === "" ? 1 : 0) - (b.hostname === "" ? 1 : 0);
  if (unaddressed !== 0) return unaddressed;
  const key = (row: DeployableListRow) => (row.hostname === "" ? row.name : row.hostname).toLowerCase();
  return key(a).localeCompare(key(b)) || a.key.localeCompare(b.key);
}

/**
 * foldDeployables answers the list: which groups, holding which rows, given
 * the three feeds and the question asked of them.
 *
 * A site whose `packageId` names a package the feed does not hold stands
 * alone rather than vanishing -- the page reads it the same way, as its own
 * source. A parked run whose package the feed does not hold adds nothing: a
 * run the caller cannot see the source of is a run they cannot act on.
 */
export function foldDeployables(
  sites: readonly SiteRow[],
  packages: readonly PackageRow[],
  parkedRuns: readonly DeploymentRow[],
  filter: ListFilter,
  showArchived: boolean,
): DeployableListGroup[] {
  const { rowsByGroup, groupPackage } = collectRows(sites, packages, parkedRuns);

  const groups: DeployableListGroup[] = [];
  for (const [id, rows] of rowsByGroup) {
    const kept = rows
      .filter((row) => isArchived(row) === showArchived)
      .filter((row) => matches(row, filter))
      .sort(compareRows);
    if (kept.length === 0) continue;
    const pkg = groupPackage.get(id) ?? null;
    // `startsSection` is filled in after the sort, which is the only place the
    // neighbours are known.
    groups.push({ id, pkg, rows: kept, section: pkg === null ? "standalone" : "source", startsSection: false });
  }
  // BY SECTION, THEN BY FIRST ADDRESS. The address order is the one the list
  // always had, and it is kept WITHIN a section: the feeds fold events in
  // arrival order, and a list that depended on it would reshuffle on an update
  // -- exactly when somebody is watching. The section term is what stops a
  // source group and a hand-made site interleaving.
  const ordered = groups.sort(
    (a, b) =>
      SECTION_ORDER[a.section] - SECTION_ORDER[b.section] ||
      compareRows(a.rows[0]!, b.rows[0]!) ||
      a.id.localeCompare(b.id),
  );
  return ordered.map((group, i) => ({
    ...group,
    startsSection: i === 0 || ordered[i - 1]!.section !== group.section,
  }));
}

/**
 * EVERY ROW THE THREE FEEDS DESCRIBE, grouped and not yet asked anything.
 *
 * Split out of `foldDeployables` so that a second reading can start from the
 * same rows: the Sources list summarises a source by ALL its rows, and must
 * not inherit the deployables list's questions (its search, its facets, its
 * archived flip) by reading that list's answer.
 */
function collectRows(
  sites: readonly SiteRow[],
  packages: readonly PackageRow[],
  parkedRuns: readonly DeploymentRow[],
): { rowsByGroup: Map<string, DeployableListRow[]>; groupPackage: Map<string, PackageRow | null> } {
  const packageById = new Map(packages.map((p) => [p.id, p]));
  const parkedByPackage = new Map(packages.map((p) => [p.id, newestParkedRun(parkedRuns, p.id)]));

  const rowsByGroup = new Map<string, DeployableListRow[]>();
  const groupPackage = new Map<string, PackageRow | null>();
  const push = (groupId: string, pkg: PackageRow | null, row: DeployableListRow) => {
    const list = rowsByGroup.get(groupId) ?? [];
    list.push(row);
    rowsByGroup.set(groupId, list);
    groupPackage.set(groupId, pkg);
  };

  const served = new Set<string>();
  for (const site of sites) {
    const pkg = site.packageId === "" ? null : (packageById.get(site.packageId) ?? null);
    // A PARKED RUN BELONGS TO THE APPS IT NAMES (memql#4953). Every row of a
    // source used to carry the same run, so one app's gate marked every
    // sibling "a deploy is waiting for you" and repainted their compact rails
    // as unreached -- a live app drawn as not-yet-there because a DIFFERENT
    // app was waiting for an answer.
    const sourceParked = pkg === null ? null : (parkedByPackage.get(pkg.id) ?? null);
    const parked = runCoversApp(sourceParked, site.packageDeployableName) ? sourceParked : null;
    if (pkg !== null) served.add(`${pkg.id}/${site.packageDeployableName}`);
    push(pkg === null ? `site:${site.id}` : `pkg:${pkg.id}`, pkg, siteRowFor(site, pkg, parked));
  }

  // THE ROWS THAT WILL SERVE: every app a parked run's report names that has
  // no site yet, and -- when the report names none -- the source itself, so
  // a parked run is never invisible. Somebody who closed the window
  // mid-compose finds their run on its row (design section A).
  for (const [packageId, run] of parkedByPackage) {
    const pkg = packageById.get(packageId);
    if (pkg === undefined || run === null) continue;
    // The apps the run is FOR, out of the ones its report names. A scoped run
    // must not raise a row for a sibling it was told to skip.
    const apps = (run.report?.deployables ?? []).filter((a) => runCoversApp(run, a.name));
    const pending = apps.length === 0 ? [{ name: "", kind: "" }] : apps.filter((a) => !served.has(`${packageId}/${a.name}`));
    for (const app of pending) {
      // CLAIM THE KEY, or the declared pass below adds the same app a second
      // time -- it reads the same catalogue and has no other way to know this
      // row already exists.
      served.add(`${packageId}/${app.name}`);
      push(`pkg:${packageId}`, pkg, {
        key: `${packageId}/${app.name}`,
        site: null,
        pkg,
        app: app.name,
        name: app.name === "" ? pkg.name || packageId : ((app as {displayName?: string}).displayName?.trim() || app.name),
        hostname: "",
        kind: app.kind,
        parked: run,
        deployedBy: deployerOf(run, null, pkg),
        // A run is IN FLIGHT for this app; whatever the off-list says, this is
        // happening and the row must not read as inert.
        disabled: false,
      });
    }
  }

  // WHAT THE SOURCE DECLARES BUT HAS NOT DEPLOYED.
  //
  // A site row is written only for an app that actually deployed, so an app
  // SKIPPED at the confirm gate had no row and was invisible -- absent from
  // this list and absent from its own source's page, which is how somebody
  // could decline one app of a source and then be unable to find it again.
  // `declares` is the source's own catalogue, rewritten by every analysis, and
  // the difference between it and the sites is exactly the set a person can
  // still deploy.
  //
  // AFTER the parked pass above, so that when a parked run names the same app
  // THE RUN'S ROW WINS: a run in flight is the more specific fact, and `served`
  // is what keeps the two from both landing.
  //
  // THIS BLOCK USED TO RUN FIRST, and the comment claimed this outcome while
  // the code produced its opposite -- the declared pass claimed the key, so no
  // row ever carried a run. The reopen path was dead code, and clicking the
  // row started ANOTHER analyze run every time: somebody who closed the window
  // mid-compose could not get back to their own gate and silently accumulated
  // runs, each of which parked.
  for (const pkg of packages) {
    for (const declared of pkg.declares) {
      const key = `${pkg.id}/${declared.name}`;
      if (served.has(key)) continue;
      served.add(key);
      push(`pkg:${pkg.id}`, pkg, {
        key,
        site: null,
        pkg,
        app: declared.name,
        name: declared.displayName?.trim() || declared.name,
        // NO ADDRESS, because it has none: nobody was asked where this should
        // live. Deploying it is what asks.
        hostname: "",
        kind: declared.kind,
        parked: null,
        deployedBy: deployerOf(null, null, pkg),
        // The owner's standing choice, and it only means anything here --
        // among the apps that have no site. One that HAS a site was deployed,
        // and the site is the fact.
        disabled: pkg.disabledDeployables.includes(declared.name),
      });
    }
  }

  // A registered source with no report still has a resumable setup, including
  // failures that occur after the person leaves. Its timeline owns the result.
  for (const pkg of packages) {
    if (rowsByGroup.has(`pkg:${pkg.id}`) || pkg.sourceRemoved || pkg.status === "archived") continue;
    push(`pkg:${pkg.id}`, pkg, {
      key: `${pkg.id}/`, site: null, pkg, app: "", name: pkg.name || pkg.id,
      hostname: "", kind: "", parked: null, deployedBy: deployerOf(null, null, pkg), disabled: false,
    });
  }

  return { rowsByGroup, groupPackage };
}

/**
 * THE NEWEST PARKED RUN OF ONE SOURCE, or null.
 *
 * The pipeline parks one at a time, and the feed folds events in the order
 * the cluster sent them -- so a caller reading `rows[0]` would read a STALE
 * run exactly when a new one arrived, which is the same reason the page sorts
 * its timeline rather than trusting arrival order.
 */
export function newestParkedRun(runs: readonly DeploymentRow[], packageId: string): DeploymentRow | null {
  let newest: DeploymentRow | null = null;
  for (const run of runs) {
    if (run.packageId !== packageId || !PENDING_DEPLOYMENT_STATUSES.has(run.status)) continue;
    if (newest === null || at(run) > at(newest) || (at(run) === at(newest) && run.id > newest.id)) {
      newest = run;
    }
  }
  return newest;
}

function at(run: DeploymentRow): string {
  return run.startedAt || run.createdAt;
}

/**
 * What counts as a CHANGE to a group, for the arrival cue: the source's own
 * fingerprint (a rename, a newer upstream version, an archive), each row's
 * site fingerprint (a publish, a status flip), a row arriving or leaving, and
 * a run parking or clearing. Nothing that moves on a timer -- the three
 * fingerprints this composes already keep liveness fields out, and a parked
 * run is named by id rather than by anything the pipeline touches while it
 * waits.
 */
export function groupFingerprint(group: DeployableListGroup): string {
  return [
    group.pkg === null ? "" : packageFingerprint(group.pkg),
    ...group.rows.map((row) =>
      [row.key, row.site === null ? "will-serve" : siteFingerprint(row.site), row.parked === null ? "" : `pending:${row.parked.id}:${row.parked.status}`].join(":"),
    ),
  ].join("|");
}

// ---------------------------------------------------------------------------
// Two lists, because there are two kinds of thing (DESIGN.md: "a tab is a
// different kind of thing; a subset is a filter")
// ---------------------------------------------------------------------------
//
// The list used to be ONE list of both: a source was a row with its apps
// indented beneath it, then a "Standalone" heading over the rest -- a tree
// inside a list, with two row types and an order that followed origin rather
// than anything a person was looking for. The owner's word for it was "I
// really hate that combined list".
//
// A SOURCE is a repository or a zip that produces deployables, and has a life
// of its own (a newer version upstream, a run parked at its gate, archived). A
// DEPLOYABLE is a thing that answers at an address, wherever it came from. They
// are two nouns, so they are two tabs -- and where a deployable came from
// stops being a heading and an indent, and becomes a fact on its row and a
// facet in Refine, which already carried that facet.

/** Serving deployables and setup in progress share this list. A persisted
 * source without a report retains a setup row so failed analysis is resumable. */
export function flatDeployables(groups: readonly DeployableListGroup[]): DeployableListRow[] {
  return groups
    .flatMap((group) => group.rows)
    .filter((row) => row.site !== null || row.parked !== null || row.app === "")
    .sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" }) || a.hostname.localeCompare(b.hostname));
}

/** What a zip that was never given a name is called, on either tab. */
const UNNAMED_ZIP = "Uploaded zip";

/** What a source is called on a row: its own name, else where it lives. */
export function sourceName(pkg: Pick<PackageRow, "name" | "sourceKind" | "repoUrl" | "repoRef">): string {
  if (pkg.name.trim() !== "") return pkg.name.trim();
  // A NAME, so it takes a name's capital: the same two words a deployable's
  // row uses for where it came from (`originLabel`), because the two are read
  // against each other across the tabs.
  return pkg.sourceKind === "artifact" ? UNNAMED_ZIP : sourceLabel(pkg);
}

/**
 * foldSources answers the Sources list: EVERY source this cluster tracks, each
 * with every app it produced.
 *
 * It is its own fold rather than a filter over the deployables list's answer,
 * which is what it was first written as -- and three things were wrong with
 * that, none of which a cluster with no sources could show:
 *
 *  - A SOURCE WITH NO APPS WAS NOT LISTED. A group exists in that fold only
 *    when it has a row, so a source whose analysis was refused -- no manifest,
 *    a private repository -- had nothing to hang on and was nowhere: not on
 *    this tab, where somebody would go to try it again or archive it.
 *  - A SEARCH TRIMMED THE SUMMARY. The deployables list narrows ROWS, so
 *    searching for one app left its source reading "1 app" out of three. The
 *    search here is asked of the SOURCE -- its name, where it lives, and what
 *    it made, so "which source made admin" still finds it -- and never removes
 *    a row from a source it kept.
 *  - ARCHIVED WAS THE ROW'S, NOT THE SOURCE'S. One archived app moved its
 *    (active) source into the archived view as a source with one app. A source
 *    is archived when ITS status says so; an archived app of an active source
 *    is one of its apps, counted and not deployed.
 *
 * BY NAME, because a source is a named thing and that is what somebody scans
 * a list of them for. (The deployables list inside it keeps address order.)
 */
export function foldSources(
  sites: readonly SiteRow[],
  packages: readonly PackageRow[],
  parkedRuns: readonly DeploymentRow[],
  search: string,
  showArchived: boolean,
  provenance?: (pkg: PackageRow) => string,
): DeployableListGroup[] {
  const { rowsByGroup } = collectRows(sites, packages, parkedRuns);
  const needle = search.trim().toLowerCase();
  const groups: DeployableListGroup[] = [];
  for (const pkg of packages) {
    if (pkg.sourceRemoved || (pkg.status === "archived") !== showArchived) continue;
    const rows = [...(rowsByGroup.get(`pkg:${pkg.id}`) ?? [])].sort(compareRows);
    if (needle !== "") {
      const hay = [pkg.name, sourceLabel(pkg), sourceName(pkg), provenance?.(pkg) ?? "", ...rows.flatMap((row) => [row.name, row.hostname])]
        .join(" ")
        .toLowerCase();
      if (!hay.includes(needle)) continue;
    }
    groups.push({ id: `pkg:${pkg.id}`, pkg, rows, section: "source", startsSection: false });
  }
  return groups.sort(
    (a, b) =>
      sourceName(a.pkg!).localeCompare(sourceName(b.pkg!), undefined, { sensitivity: "base" }) || a.id.localeCompare(b.id),
  );
}

/**
 * Where a deployable came from, as its row says it.
 *
 * The source's own short name when it has one, because that is what a person
 * would look for on the Sources tab. Otherwise the way in it took -- and
 * "Built in" for a bundle baked into the edge image, which is none of the
 * three ways in and must not be described as one of them.
 */
export function originLabel(row: Pick<DeployableListRow, "site" | "pkg">): string {
  if (row.pkg !== null) {
    if (row.pkg.name.trim() !== "") return row.pkg.name.trim();
    return row.pkg.sourceKind === "repo" ? shortRepo(row.pkg.repoUrl) : UNNAMED_ZIP;
  }
  const via = sourceOf(row);
  if (via === "zip") return "Zip";
  if (via === "ci") return "CI";
  if (via === "repository") return "Repository";
  return row.site !== null && row.site.systemOwned ? "Built in" : "";
}

/** What counts as NEWS on one deployable's row, for the arrival cue. */
export function rowFingerprint(row: DeployableListRow): string {
  return [
    row.key,
    row.site === null ? "will-serve" : siteFingerprint(row.site),
    row.pkg === null ? "" : packageFingerprint(row.pkg),
    row.parked === null ? "" : `pending:${row.parked.id}:${row.parked.status}`,
  ].join(":");
}

export interface SourceSummary {
  /** How many apps the source declares (or has produced). */
  apps: number;
  /** How many of them answer at an address. */
  deployed: number;
  /** A run is parked at its gate, waiting on a person. */
  waiting: boolean;
}

export function sourceSummary(group: DeployableListGroup): SourceSummary {
  return {
    apps: group.rows.filter(row => row.app !== "" || row.site !== null).length,
    deployed: group.rows.filter((row) => row.site !== null && row.site.status !== "archived").length,
    waiting: group.rows.some((row) => row.parked?.status === "awaiting_confirm"),
  };
}

/**
 * A source's state, in one word, with the tone it carries.
 *
 * IN ORDER OF WHAT A PERSON HAS TO DO ABOUT IT: a run waiting on them first,
 * then a newer version they could deploy, then the quiet states.
 */
export function sourceStateWord(group: DeployableListGroup): { word: string; tone: "accent" | "warn" | "muted" } {
  const pkg = group.pkg;
  if (pkg === null) return { word: "", tone: "muted" };
  if (pkg.status === "archived") return { word: "Archived", tone: "muted" };
  if (group.rows.some(row => row.parked?.status === "analyzing")) return { word: "Analyzing", tone: "muted" };
  if (sourceSummary(group).waiting) return { word: "Review needed", tone: "warn" };
  if (pkg.updateAvailable) return { word: "Update available", tone: "accent" };
  return { word: sourceSummary(group).deployed === 0 ? "Nothing deployed" : "Current", tone: "muted" };
}

