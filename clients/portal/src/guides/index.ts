import { GUIDE_ENTRIES } from "./entries";
import type { GuideEntry } from "./types";

export type { GuideEntry, GuideTechnical } from "./types";
export { GUIDE_ENTRIES } from "./entries";

// The registry, keyed for lookup. Built once from the list, because the LIST
// is the authorable thing and a hand-maintained map keyed by a string that
// also appears inside each entry is two places to get one id wrong.
const BY_ID = new Map<string, GuideEntry>(GUIDE_ENTRIES.map((entry) => [entry.id, entry]));

// guideFor answers undefined for a page with no guide, and PageHeader renders
// no Eye button in that case. An Eye that opens an empty dialog is worse than
// no Eye: it teaches a person the control does nothing.
export function guideFor(id: string | undefined): GuideEntry | undefined {
  return id === undefined ? undefined : BY_ID.get(id);
}

export function guideIds(): readonly string[] {
  return GUIDE_ENTRIES.map((entry) => entry.id);
}
