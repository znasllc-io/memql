import { CONTENT_KINDS, SOURCE_VALUES } from "./concepts";
import { artifactName, isContentKind, type ArtifactRow } from "./rows";

// The list derivation (design D1, reshaped by epic memql#4842): place / kind /
// source / search / folder scope / sort, as ONE pure fold over the artifacts
// snapshot.
//
// CLIENT-SIDE ON PURPOSE. The owner-scoped set is small, and `folderId == ""`
// cannot match pre-field rows server-side -- a row promoted before folders
// exist has no member at all, and only a client-side fold reads absence and
// the empty string as the same answer: the root. (The `archived != true`
// lesson, applied to the next field.)
//
// ===========================================================================
// PLACES, NOT A CHECKBOX (epic memql#4842, #4846)
// ===========================================================================
// The rail offers three places and the fold answers for the one being looked
// at. Library is what you have; Desktop is the subset sitting on your desks
// (an id-set handed in by the caller, because desks are shell state and this
// module stays pure); the Bin is what you archived. The old `showArchived`
// toggle is gone -- archived rows have a place now, not a filter.
//
// THE PLACE IS `bin`, NOT `archive` (epic memql#4981). One destination had
// two names: the row menu's verb has always been "Move to Bin", the dock
// fixture it lands in is the Bin app, and the rail alone called it Archive.
// A person who archives a file from the menu and then goes looking for
// "Bin" in the rail found "Archive" instead and had to work out that they
// are the same place. ARCHIVE STAYS AS THE VERB -- archiving is what you do,
// the Bin is where it goes -- so `confirmBeforeArchive`, `planArchive` and
// the confirm's own wording are deliberately untouched.

/** No client constraint. */
export const ACCOUNT_ANY = "all";
/** Only rows tied to no client at all. */
export const ACCOUNT_NONE = "none";

export type KindFilter = "all" | (typeof CONTENT_KINDS)[number];
export type SourceFilter = "all" | (typeof SOURCE_VALUES)[number];

export const FILES_PLACES = ["library", "desktop", "bin"] as const;
export type FilesPlace = (typeof FILES_PLACES)[number];

export function isFilesPlace(value: unknown): value is FilesPlace {
  return (FILES_PLACES as readonly unknown[]).includes(value);
}

/** What the desk holds, folded from shell state by the caller. */
export interface DeskMembership {
  /** Artifact ids of loose desk file icons (uploads in flight excluded). */
  fileArtifactIds: ReadonlySet<string>;
  /** Library folder ids with a shortcut on any desk. */
  folderIds: ReadonlySet<string>;
}

export const EMPTY_DESK: DeskMembership = {
  fileArtifactIds: new Set(),
  folderIds: new Set(),
};

export interface FilesFilter {
  /** Which of the rail's three places is being looked at. */
  place: FilesPlace;
  /**
   * The folder scope: "" = the place's own root, an id = that folder, null =
   * everywhere. A non-empty search widens to the whole PLACE on its own --
   * someone searching is asking about the place, not about the folder they
   * happen to be in. In the Bin "" already means everything archived:
   * archived rows are a flat population with folders as an optional
   * narrowing, not a tree with a root.
   */
  folderId: string | null;
  kind: KindFilter;
  source: SourceFilter;
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
  place: "library",
  folderId: "",
  kind: "all",
  source: "all",
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

/** Whether a row belongs to the Desktop place's population at all: a loose
 *  desk file, or a file inside a folder that has a desk shortcut. */
function inDesktop(row: ArtifactRow, desk: DeskMembership): boolean {
  return desk.fileArtifactIds.has(row.id) || desk.folderIds.has(row.folderId);
}

export function applyFilters(
  rows: readonly ArtifactRow[],
  filter: FilesFilter,
  desk: DeskMembership = EMPTY_DESK,
): ArtifactRow[] {
  const searching = filter.search.trim() !== "";
  const kept = rows.filter((row) => {
    // The records lens never renders here, under ANY combination (design D2).
    if (!isContentKind(row.kind)) return false;

    // The place decides the population before any facet narrows it.
    if (filter.place === "bin") {
      if (!row.archived) return false;
      if (!searching && filter.folderId !== null && filter.folderId !== "" && row.folderId !== filter.folderId) {
        return false;
      }
    } else if (filter.place === "desktop") {
      if (row.archived) return false;
      if (searching) {
        if (!inDesktop(row, desk)) return false;
      } else if (filter.folderId === "" || filter.folderId === null) {
        // The Desktop root mirrors the desk: loose file icons. A desk
        // folder's contents live one click away, exactly as on the desk.
        if (!desk.fileArtifactIds.has(row.id)) return false;
      } else if (row.folderId !== filter.folderId) {
        return false;
      }
    } else {
      if (row.archived) return false;
      if (!searching && filter.folderId !== null && row.folderId !== filter.folderId) return false;
    }

    if (filter.kind !== "all" && row.kind !== filter.kind) return false;
    if (filter.source !== "all" && row.source !== filter.source) return false;
    if (!matchesSearch(row, filter.search)) return false;
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
