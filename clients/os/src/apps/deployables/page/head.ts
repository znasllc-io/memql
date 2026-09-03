import type { DeploymentRow, PackageRow } from "../packages/rows";
import type { SiteRow } from "../rows";
import { isPublished, type HeadState } from "./rail";

// The Head's state, derived from what the page holds (design section C).
//
// `headActionFor` (rail.ts) answers the design's table for a STATE; this is
// where the state comes from. It is pure and separate from the page so the
// whole table can be walked against fixtures -- a running run, a parked one,
// a refused one, a draft with a bundle, a live site with and without a newer
// version, a hand-made site, a system-owned row, an archived one, a reader --
// without rendering anything.

export interface HeadInput {
  site: SiteRow;
  /** The site's source, or null for a hand-made deployable. */
  pkg: PackageRow | null;
  /** The NEWEST run of the source, whatever its status; null with no source. */
  run: DeploymentRow | null;
  canWrite: boolean;
}

/** The stages a run is AT while it moves. Mirrors the pipeline's non-terminal set. */
const IN_FLIGHT = new Set(["analyzing", "building", "staging_dsl", "rolling", "publishing"]);

/**
 * headStateFor names the row of the table this page is on, or null when the
 * Head offers no action at all -- and null is a statement, not a gap: a
 * system-owned row, an archived deployable, an archived source and a reader
 * each get NO lifecycle control rather than a disabled one, because a button
 * that can only fail is a button somebody has to read past to learn it is
 * not for them.
 *
 * The run outranks the site: a refused redeploy of a live site reads Retry,
 * not Redeploy, because the thing that just happened is the thing a person
 * came to see. A parked run answers Deploy with the placements complete by
 * construction -- every app of an EXISTING deployable already has its
 * address, so there is nothing left to choose.
 */
export function headStateFor({ site, pkg, run, canWrite }: HeadInput): HeadState | null {
  if (!canWrite) return null;
  if (site.systemOwned) return null;
  if (site.status === "archived") return null;
  // A deploy of an archived source is refused server-side; Restore lives on
  // the Source stop, and the Head says nothing until it is used.
  if (pkg !== null && pkg.status === "archived") return null;

  if (run !== null) {
    if (run.status === "awaiting_confirm") return { at: "awaiting_confirm", placementsComplete: true };
    if (IN_FLIGHT.has(run.status)) return { at: "running" };
    if (run.status === "refused" || run.status === "failed") return { at: "refused_or_failed" };
  }

  switch (site.status) {
    case "draft":
      // The placeholder a new deployable starts with is not a bundle; a
      // draft holding one is waiting on exactly one thing, and it is the
      // person's.
      return isPublished(site) ? { at: "draft_with_bundle" } : null;
    case "live":
    case "disabled":
      // A paused site reads as live for the action's purpose -- the next
      // deploy is the same deploy -- and Resume is the Live stop's.
      return { at: "live", updateAvailable: pkg?.updateAvailable ?? false };
    default:
      return null;
  }
}
