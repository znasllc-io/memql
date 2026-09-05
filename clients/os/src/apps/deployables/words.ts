import { bundleForm, type SiteRow } from "./rows";

// words.ts -- THE ONE VOCABULARY for what a deployable is (2026-09-05 design,
// D1).
//
// ===========================================================================
// ONE WORD PER STATE, READ FROM ONE PLACE
// ===========================================================================
// The owner walked the flow on a live cluster and met three words for one
// state: after Deploy the bar read "Deployed", then "Published 1 app", then
// "It is not serving yet". Deploy sounded final, Published sounded live, and
// neither was. Every surface that names a state -- the bar, the list's chip,
// the source page's app list, the rail's Live stop, the compose flow's end --
// now reads it from here, so the word a person learns on one screen is the
// word they meet on the next, and changing one is one edit.
//
// The engine's enum does not move. `live`, `disabled`, `draft` and `archived`
// stay what they are; what changed is what a PERSON is told they mean:
//
//   Inactive      declared by the source, on its off-list, no site. Skipped.
//   Not deployed  nothing has landed here yet: a placeholder bundle, or an
//                 app the source declares that nobody has deployed.
//   Built         its files are in place at the address. Not live until you
//                 say -- a first deploy leaves it here on purpose.
//   Live          serving at the address.
//   Offline       taken offline on purpose. Visitors get "temporarily
//                 unavailable" rather than "no such site".
//   Archived      filed away. Answers nothing; the name is still held.
//   Deleting      the delete landed and the domains are still coming down.
//
// The retired words -- Published, Unpublished, Publish, Unpublish, Make it
// live, Paused, off -- appear nowhere a person reads. "Publish" survives only
// as the Library's `sitePublishFromArtifact` capability name, which is the
// engine's and not a word on a surface.

export type SiteStateWord = "Not deployed" | "Built" | "Live" | "Offline" | "Archived" | "Deleting" | "Inactive";

/**
 * The placeholder a new deployable starts with -- `blob://sites/<id>/pending/`
 * (deployables.md). It is written so the row can exist before any bytes do,
 * and reading it as a build would tell a person their CI had pushed when
 * nothing has.
 */
export function isPlaceholderBundle(bundleRef: string): boolean {
  return /\/pending\/?$/.test(bundleRef.trim());
}

/** Whether a site holds real files -- the difference between Built and Not deployed. */
export function siteIsBuilt(site: Pick<SiteRow, "bundleRef">): boolean {
  return bundleForm(site.bundleRef) !== "none" && !isPlaceholderBundle(site.bundleRef);
}

/** What a deployable that has a site row IS, in one word. */
export function siteStateWord(site: Pick<SiteRow, "status" | "bundleRef">, deleting = false): SiteStateWord {
  if (deleting) return "Deleting";
  switch (site.status) {
    case "live":
      return "Live";
    case "disabled":
      return "Offline";
    case "archived":
      return "Archived";
    default:
      return siteIsBuilt(site) ? "Built" : "Not deployed";
  }
}

/** The same word as a list chip: lowercase, and absent for Live (the row's accent already says it). */
export function stateChip(word: SiteStateWord): string {
  return word === "Live" ? "" : word.toLowerCase();
}

/**
 * The one clause beneath the state word. `host` is the address; the Live and
 * Built clauses name it because the address is the thing being talked about.
 */
export function siteStateDetail(word: SiteStateWord, host: string): string {
  const at = host.trim() === "" ? "its address" : host;
  switch (word) {
    case "Live":
      return `serving at ${at}`;
    case "Built":
      return `in place at ${at}, not live yet`;
    case "Offline":
      return 'taken offline on purpose -- visitors get "temporarily unavailable", not "no such site"';
    case "Archived":
      return "filed away -- it answers nothing, and the name is still held";
    case "Deleting":
      return `releasing ${at}`;
    case "Inactive":
      return "skipped when its source was deployed -- nothing built, no address";
    default:
      return "nothing has been deployed here yet";
  }
}

/** The Refine status facet's label for an engine value. */
export function statusFacetLabel(status: string): string {
  switch (status) {
    case "live":
      return "Live";
    case "disabled":
      return "Offline";
    case "draft":
      return "Not live yet";
    default:
      return status;
  }
}

/** The Live stop's note, in the same words. */
export function liveStopNote(site: Pick<SiteRow, "status" | "bundleRef" | "hostname">): string {
  switch (siteStateWord(site)) {
    case "Live":
      return `Live at ${site.hostname}.`;
    case "Built":
      return `Built. In place at ${site.hostname}, not live yet.`;
    case "Offline":
      return 'Offline. Visitors get "temporarily unavailable" rather than "no such site", so a deployable taken offline on purpose stays distinguishable from a typo.';
    case "Archived":
      return "Archived. It answers nothing, like a site that never existed.";
    default:
      return "Nothing deployed here yet.";
  }
}

/**
 * The name a person types to confirm archiving or deleting a standalone
 * deployable (2026-09-05 design, D9): the address label under the cluster's
 * domain, or the whole hostname when it has no label of its own. Mirrors the
 * server's `confirmationMatches`, which accepts either.
 */
export function confirmationWordFor(hostname: string): string {
  const host = hostname.trim().toLowerCase();
  const dot = host.indexOf(".");
  if (dot < 0) return host;
  const rest = host.slice(dot + 1);
  return rest.includes(".") ? host.slice(0, dot) : host;
}
