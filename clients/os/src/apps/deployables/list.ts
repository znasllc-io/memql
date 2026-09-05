import { packageFingerprint, sourceLabel, type DeploymentRow, type PackageRow } from "./packages/rows";
import { isPlaceholderBundle, type StandingInput } from "./page/rail";
import { bundleForm, siteFingerprint, siteName, type SiteRow } from "./rows";

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
// `sitesAll` and `packagesAll` carry the composite tier's own predicate and
// no other, so one seed of each holds the complete truth the caller may read,
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
 * bundle baked into the edge image -- the seeded portal, a baked site -- is
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
  const name = app || site.title.trim() || siteName(site);
  return { key: site.id, site, pkg, app, name, hostname: site.hostname, kind: site.kind, parked };
}

function isArchived(row: DeployableListRow): boolean {
  return row.site?.status === "archived" || row.pkg?.status === "archived";
}

function matches(row: DeployableListRow, filter: ListFilter): boolean {
  if (filter.kind !== "" && row.kind !== filter.kind) return false;
  // A will-serve row has no status yet, so a status facet leaves it out: it
  // is none of draft, live or disabled, and claiming one would be a guess.
  if (filter.status !== "" && (row.site?.status ?? "") !== filter.status) return false;
  const accountId = row.site?.accountId ?? "";
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
    const parked = pkg === null ? null : (parkedByPackage.get(pkg.id) ?? null);
    if (pkg !== null) served.add(`${pkg.id}/${site.packageDeployableName}`);
    push(pkg === null ? `site:${site.id}` : `pkg:${pkg.id}`, pkg, siteRowFor(site, pkg, parked));
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
  // BEFORE the parked pass below, so that when a parked run names the same app
  // the run's row wins: a run in flight is the more specific fact, and `served`
  // is what keeps the two from both landing.
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
        name: declared.name,
        // NO ADDRESS, because it has none: nobody was asked where this should
        // live. Deploying it is what asks.
        hostname: "",
        kind: declared.kind,
        parked: null,
      });
    }
  }

  // THE ROWS THAT WILL SERVE: every app a parked run's report names that has
  // no site yet, and -- when the report names none -- the source itself, so
  // a parked run is never invisible. Somebody who closed the window
  // mid-compose finds their run on its row (design section A).
  for (const [packageId, run] of parkedByPackage) {
    const pkg = packageById.get(packageId);
    if (pkg === undefined || run === null) continue;
    const apps = run.report?.deployables ?? [];
    const pending = apps.length === 0 ? [{ name: "", kind: "" }] : apps.filter((a) => !served.has(`${packageId}/${a.name}`));
    for (const app of pending) {
      push(`pkg:${packageId}`, pkg, {
        key: `${packageId}/${app.name}`,
        site: null,
        pkg,
        app: app.name,
        name: app.name === "" ? pkg.name || packageId : app.name,
        hostname: "",
        kind: app.kind,
        parked: run,
      });
    }
  }

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
    if (run.packageId !== packageId || run.status !== "awaiting_confirm") continue;
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
      [row.key, row.site === null ? "will-serve" : siteFingerprint(row.site), row.parked === null ? "" : `waiting:${row.parked.id}`].join(":"),
    ),
  ].join("|");
}
