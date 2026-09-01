import { CONTENT_KINDS, SOURCE_VALUES } from "./concepts";
import { artifactName, isContentKind, type ArtifactRow } from "./rows";

// The list derivation (design D1): kind / source / archived / search / folder
// scope / sort, as ONE pure fold over the artifacts snapshot.
//
// CLIENT-SIDE ON PURPOSE. The owner-scoped set is small, and `folderId == ""`
// cannot match pre-field rows server-side -- a row promoted before folders
// exist has no member at all, and only a client-side fold reads absence and
// the empty string as the same answer: the root. (The `archived != true`
// lesson, applied to the next field.)

/** No client constraint. */
export const ACCOUNT_ANY = "all";
/** Only rows tied to no client at all. */
export const ACCOUNT_NONE = "none";

export type KindFilter = "all" | (typeof CONTENT_KINDS)[number];
export type SourceFilter = "all" | (typeof SOURCE_VALUES)[number];

export interface FilesFilter {
  /**
   * The folder scope: "" = the root, an id = that folder, null = everywhere.
   * A non-empty search widens to everywhere on its own -- someone searching
   * is asking about their Library, not about the folder they happen to be in.
   */
  folderId: string | null;
  kind: KindFilter;
  source: SourceFilter;
  /** Archived rows are EXCLUDED by default and visibly marked when shown. */
  showArchived: boolean;
  /**
   * The client scope (epic memql#4800): "all" = no account constraint,
   * "none" = only rows with NO client, an id = rows labelled with it.
   *
   * "none" is a real value rather than an absence because "show me what is
   * not filed to anybody" is the question somebody asks when they are trying
   * to file things -- and it is the only one the other two cannot express.
   */
  accountId: string;
  search: string;
  /** false = newest first, the default. */
  sortAscending: boolean;
}

export const DEFAULT_FILTER: FilesFilter = {
  folderId: "",
  kind: "all",
  source: "all",
  showArchived: false,
  accountId: ACCOUNT_ANY,
  search: "",
  sortAscending: false,
};

function matchesSearch(row: ArtifactRow, needle: string): boolean {
  const q = needle.trim().toLowerCase();
  if (q === "") return true;
  if (artifactName(row).toLowerCase().includes(q)) return true;
  if (row.summary.toLowerCase().includes(q)) return true;
  return row.labels.some((label) => label.toLowerCase().includes(q));
}

export function applyFilters(rows: readonly ArtifactRow[], filter: FilesFilter): ArtifactRow[] {
  const searching = filter.search.trim() !== "";
  const kept = rows.filter((row) => {
    // The records lens never renders here, under ANY combination (design D2).
    if (!isContentKind(row.kind)) return false;
    if (row.archived && !filter.showArchived) return false;
    if (filter.kind !== "all" && row.kind !== filter.kind) return false;
    if (filter.source !== "all" && row.source !== filter.source) return false;
    if (!matchesSearch(row, filter.search)) return false;
    if (!searching && filter.folderId !== null && row.folderId !== filter.folderId) return false;
    // THE CLIENT SCOPE READS ABSENCE AND THE EMPTY LIST AS ONE ANSWER, and it
    // has to: every row promoted before `accountIds` existed carries no key at
    // all, so a filter that distinguished them would hide the entire
    // pre-existing Library from the "no client" view -- which is exactly the
    // view somebody uses to find what still needs filing.
    if (filter.accountId === ACCOUNT_NONE && row.accountIds.length > 0) return false;
    if (
      filter.accountId !== ACCOUNT_ANY &&
      filter.accountId !== ACCOUNT_NONE &&
      !row.accountIds.includes(filter.accountId)
    ) {
      return false;
    }
    return true;
  });
  const direction = filter.sortAscending ? 1 : -1;
  return kept.sort(
    (a, b) => direction * (a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id)),
  );
}
